package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jinyule/nano-harness/internal/app/compaction"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/plan"
	"github.com/jinyule/nano-harness/internal/app/prompt"
	"github.com/jinyule/nano-harness/internal/app/retry"
	"github.com/jinyule/nano-harness/internal/app/settings"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// EngineConfig keeps local safety limits independent from provider quotas.
type EngineConfig struct {
	MaxSteps int
}

// Engine runs durable turns over provider-neutral components.
type Engine struct {
	llm        *llm.Runtime
	tools      *appTool.Runtime
	retry      *retry.Service
	compaction *compaction.Service
	prompt     *prompt.Assembler
	plan       *plan.Service
	settings   *settings.Service
	maxSteps   int

	mu         sync.RWMutex
	started    bool
	active     bool
	contexts   []*contextEntry
	admissions map[string]*Admission
}

// NewEngine validates the full core-loop dependency graph.
func NewEngine(runtime *llm.Runtime, tools *appTool.Runtime, retries *retry.Service, compactor *compaction.Service, assembler *prompt.Assembler, planMode *plan.Service, configuration *settings.Service, config EngineConfig) (*Engine, error) {
	if runtime == nil || tools == nil || retries == nil || compactor == nil || assembler == nil || planMode == nil || configuration == nil || config.MaxSteps < 0 || config.MaxSteps > 256 {
		return nil, ErrInvalidConfig
	}
	if config.MaxSteps == 0 {
		config.MaxSteps = 32
	}
	return &Engine{llm: runtime, tools: tools, retry: retries, compaction: compactor, prompt: assembler, plan: planMode, settings: configuration, maxSteps: config.MaxSteps}, nil
}

// ID returns the stable plugin identity.
func (*Engine) ID() string { return "agent-engine" }

// Start activates new turns until scope cleanup.
func (engine *Engine) Start(_ context.Context, scope *plugin.Scope) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.started {
		return ErrInvalidConfig
	}
	if err := scope.Defer(func(context.Context) error {
		engine.mu.Lock()
		engine.active = false
		engine.mu.Unlock()
		return nil
	}); err != nil {
		return err
	}
	engine.started, engine.active = true, true
	return nil
}

type runInput struct {
	journal   *journal
	message   session.Message
	persona   string
	tools     []string
	delegated bool
	// route pins a delegated agent's requests to the route it inherited;
	// the zero value follows the hot settings route.
	route session.SubagentRoute
	// drain takes steers at tool-step boundaries.
	drain func() []session.Message
	// notices takes queued notices at turn start, at tool-step boundaries,
	// and before a turn would complete, which then continues to answer them.
	// The last allowed step takes none, so they wait for the next turn.
	notices func() []session.Message
}

func (engine *Engine) runTurn(ctx context.Context, input runInput) (result TurnResult) {
	result.SessionID = input.journal.Header().SessionID
	engine.mu.RLock()
	active := engine.active
	engine.mu.RUnlock()
	if !active {
		result.Err, result.Outcome = ErrNotRunning, outcomeFor(ctx, ErrNotRunning)
		return result
	}
	events, err := input.journal.Events(ctx)
	if err != nil {
		result.Err, result.Outcome = err, outcomeFor(ctx, err)
		return result
	}
	turn := nextTurn(events)
	result.Turn = turn
	turnOpen := false
	stepOpen := false
	var openStep uint64
	// Like upstream's finally block, this closes every turn whose
	// turn/start committed, whatever ends it, including its opening.
	defer func() {
		panicked := false
		if recovered := recover(); recovered != nil {
			result.Err = errors.New("agent loop panicked")
			result.Outcome = session.OutcomeError
			panicked = true
		}
		if stepOpen {
			// A step cannot close over an open question or a committed call
			// without a result. The batch has returned, so no decision is in
			// flight; like resume repair, each question the log leaves open
			// is cancelled, then each call that may have run gets a result.
			events, eventsErr := input.journal.Events(context.WithoutCancel(ctx))
			result.Err = errors.Join(result.Err, eventsErr)
			approvals, calls := unresolved(events)
			for _, approvalID := range approvals {
				_, decideErr := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordApprovalDecided, Turn: turn, Step: openStep, Approval: &session.ApprovalData{ID: approvalID, Outcome: session.ApprovalCancelled}})
				result.Err = errors.Join(result.Err, decideErr)
			}
			for _, callID := range calls {
				interrupted := session.InterruptedToolResult(callID)
				_, resultErr := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordToolResult, Turn: turn, Step: openStep, Result: &interrupted})
				result.Err = errors.Join(result.Err, resultErr)
			}
			_, closeErr := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordStepEnd, Turn: turn, Step: openStep})
			result.Err = errors.Join(result.Err, closeErr)
		}
		if !panicked && result.Outcome == session.OutcomeError {
			// A failure is settled once, after the step closeout (whose
			// appends ignore cancellation and may block on fsync) and just
			// before turn/end commits: whichever boundary returned it, a
			// turn cancelled by now records a cancellation. The failure
			// itself is kept, and a committed or stopped outcome is never
			// rewritten. An interrupt during the turn/end append itself is
			// left to the agent, which then does not wake for notices.
			result.Outcome = outcomeFor(ctx, result.Err)
		}
		if turnOpen {
			_, closeErr := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordTurnEnd, Turn: turn, Outcome: result.Outcome})
			result.Err = errors.Join(result.Err, closeErr)
		}
	}()
	err = engine.openTurn(ctx, input, func(ctx context.Context) error {
		// The opening is decided once. A cancellation before this point
		// commits nothing; a later one lets turn/start and the opening
		// message commit together, and the next boundary observes it.
		if err := ctx.Err(); err != nil {
			return err
		}
		commit := context.WithoutCancel(ctx)
		if _, err := input.journal.Append(commit, session.Record{Type: session.RecordTurnStart, Turn: turn}); err != nil {
			return err
		}
		turnOpen, result.opened = true, true
		_, err := input.journal.Append(commit, session.Record{Type: session.RecordUserMessage, Turn: turn, Message: &input.message})
		return err
	})
	if errors.Is(err, ErrNotAdmitted) && !turnOpen {
		result.Turn, result.Err = 0, err
		return result
	}
	if err != nil {
		result.Err, result.Outcome = err, outcomeFor(ctx, err)
		return result
	}
	// Every boundary that takes queued input checks cancellation first, so
	// a cancelled turn leaves notices queued and steers pending.
	if err := ctx.Err(); err != nil {
		result.Err, result.Outcome = err, session.OutcomeCanceled
		return result
	}
	if err := appendUserMessages(ctx, input.journal, turn, input.notices()); err != nil {
		result.Err, result.Outcome = err, session.OutcomeError
		return result
	}

	maxSteps := uint64(engine.maxSteps) //nolint:gosec // construction restricts maxSteps to the positive range 1-256
	for step := uint64(1); step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			result.Err, result.Outcome = err, session.OutcomeCanceled
			return result
		}
		if _, err := engine.compaction.Maybe(ctx, compaction.Request{Journal: input.journal, Turn: turn, Route: input.route}); err != nil {
			result.Err, result.Outcome = fmt.Errorf("proactive compaction: %w", err), session.OutcomeError
			return result
		}
		// Plan mode changes take effect here, before the step opens, so the
		// request header below is the first to reflect them.
		planPolicy, err := engine.plan.Step(ctx, input.journal, turn)
		if err != nil {
			result.Err, result.Outcome = fmt.Errorf("plan mode boundary: %w", err), outcomeFor(ctx, err)
			return result
		}
		if err := engine.stepContext(ctx, input, turn); err != nil {
			result.Err, result.Outcome = fmt.Errorf("step context: %w", err), outcomeFor(ctx, err)
			return result
		}
		// Step context commits regardless of cancellation; a cancellation
		// that arrived meanwhile ends the turn before the step opens.
		if err := ctx.Err(); err != nil {
			result.Err, result.Outcome = err, session.OutcomeCanceled
			return result
		}
		if _, err := input.journal.Append(ctx, session.Record{Type: session.RecordStepStart, Turn: turn, Step: step}); err != nil {
			result.Err, result.Outcome = err, outcomeFor(ctx, err)
			return result
		}
		stepOpen = true
		openStep = step
		document, _, err := engine.settings.Snapshot()
		if err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		catalog, err := engine.tools.Catalog(input.tools)
		if err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		definitions := catalog.Definitions
		route := requestRoute(document, input.route)
		system, err := engine.prompt.Build(prompt.Input{
			Workspace: input.journal.Header().Cwd, Provider: route.Provider, Model: route.Model,
			Persona: input.persona, PlanPolicy: planPolicy, Tools: definitions, Guidance: catalog.Guidance,
		})
		if err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		model := findModel(document, route.Provider, route.Model)
		header := &session.RequestHeader{Provider: route.Provider, Model: route.Model, Effort: route.Effort, System: system, Tools: definitions, ContextWindow: model.ContextWindow}
		if _, err := input.journal.Append(ctx, session.Record{Type: session.RecordRequestHeader, Turn: turn, Step: step, Header: header}); err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		call, err := engine.llm.PrepareCall(ctx, route.Provider, route.Model)
		if err != nil {
			result.Err, result.Outcome = err, outcomeFor(ctx, err)
			return result
		}
		events, err = input.journal.Events(ctx)
		if err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		surface, err := session.Surface(events)
		if err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		completion, err := engine.retry.Do(ctx, input.journal, turn, step, route.Provider, route.Provider+"/"+route.Model, func() (llm.Completion, bool, error) {
			emitted := false
			completion, streamErr := call.Stream(ctx, llm.Request{
				SessionID: result.SessionID, Purpose: "agent", System: system,
				Surface: surface, Tools: definitions, Effort: &header.Effort,
			}, func(chunk session.AssistantChunk) error {
				emitted = true
				_, appendErr := input.journal.Append(ctx, session.Record{Type: session.RecordAssistantChunk, Turn: turn, Step: step, Chunk: &chunk})
				return appendErr
			})
			return completion, emitted, streamErr
		})
		if err != nil {
			var failure *llm.Error
			if errors.As(err, &failure) && failure.Code == llm.ErrorContextWindow {
				if _, closeErr := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordStepEnd, Turn: turn, Step: step}); closeErr != nil {
					result.Err, result.Outcome = errors.Join(err, closeErr), session.OutcomeError
					return result
				}
				stepOpen = false
				openStep = 0
				compacted, compactErr := engine.compaction.Maybe(ctx, compaction.Request{Journal: input.journal, Turn: turn, Force: true, Route: input.route})
				if compactErr != nil || !compacted {
					result.Err, result.Outcome = errors.Join(err, compactErr), session.OutcomeError
					return result
				}
				continue
			}
			result.Err, result.Outcome = err, outcomeFor(ctx, err)
			return result
		}
		message := completion.Message
		message.Role = session.RoleAssistant
		if message.Source.Kind == "" {
			message.Source = session.MessageSource{Kind: "provider", Plugin: route.Provider}
		}
		if _, err := input.journal.Append(ctx, session.Record{Type: session.RecordAssistantMessage, Turn: turn, Step: step, Message: &message}); err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		if completion.Stop == llm.StopMaxTokens {
			result.Text, result.Outcome = session.Text(message), session.OutcomeMaxTokens
			if _, err := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordStepEnd, Turn: turn, Step: step, Usage: completion.Usage}); err != nil {
				result.Err, result.Outcome = err, session.OutcomeError
				return result
			}
			stepOpen, openStep = false, 0
			// Like every turn that stops before answering, a truncated one
			// takes no notices; a cancellation that raced it wins.
			if err := ctx.Err(); err != nil {
				result.Err, result.Outcome = err, session.OutcomeCanceled
			}
			return result
		}
		// A completion's calls commit as one unit, unaffected by
		// cancellation: the batch below observes it and answers every
		// recorded call as aborted before dispatch, as upstream records
		// results for the calls an abort skipped.
		for index := range completion.Calls {
			completion.Calls[index] = completion.Calls[index].LimitArguments()
			call := completion.Calls[index]
			if _, err := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordToolCall, Turn: turn, Step: step, Call: &call}); err != nil {
				result.Err, result.Outcome = err, session.OutcomeError
				return result
			}
		}
		result.Text = session.Text(message)
		if len(completion.Calls) == 0 {
			if _, err := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordStepEnd, Turn: turn, Step: step, Usage: completion.Usage}); err != nil {
				result.Err, result.Outcome = err, session.OutcomeError
				return result
			}
			stepOpen = false
			openStep = 0
			if err := ctx.Err(); err != nil {
				result.Err, result.Outcome = err, session.OutcomeCanceled
				return result
			}
			if step < maxSteps {
				notices := input.notices()
				if err := appendUserMessages(ctx, input.journal, turn, notices); err != nil {
					result.Err, result.Outcome = err, session.OutcomeError
					return result
				}
				if len(notices) > 0 {
					continue
				}
			}
			if _, err := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordTurnEnd, Turn: turn, Outcome: session.OutcomeCompleted}); err != nil {
				result.Err, result.Outcome = err, session.OutcomeError
				return result
			}
			turnOpen = false
			result.Outcome = session.OutcomeCompleted
			return result
		}
		toolRoute := appTool.Route{Provider: route.Provider, Model: route.Model, ImageInput: call.Info().Vision}
		toolResults := engine.tools.ExecuteBatch(ctx, appTool.BatchRequest{
			SessionID: result.SessionID, Cwd: input.journal.Header().Cwd, Route: toolRoute, Turn: turn, Step: step,
			Calls: completion.Calls, Delegated: input.delegated, Journal: input.journal,
		})
		for index := range toolResults {
			if _, err := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordToolResult, Turn: turn, Step: step, Result: &toolResults[index]}); err != nil {
				result.Err, result.Outcome = err, session.OutcomeError
				return result
			}
		}
		if _, err := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordStepEnd, Turn: turn, Step: step, Usage: completion.Usage}); err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		stepOpen = false
		openStep = 0
		if err := ctx.Err(); err != nil {
			result.Err, result.Outcome = err, session.OutcomeCanceled
			return result
		}
		arrived := input.drain()
		if step < maxSteps {
			arrived = append(arrived, input.notices()...)
		}
		if err := appendUserMessages(ctx, input.journal, turn, arrived); err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
	}
	result.Outcome = session.OutcomeStepLimit
	return result
}

// appendUserMessages commits input that arrived during a turn, in order.
// The input has already left its queue, so a cancellation racing the
// commit must not drop it: the append ignores cancellation and a later
// boundary observes it.
func appendUserMessages(ctx context.Context, log *journal, turn uint64, messages []session.Message) error {
	for index := range messages {
		if _, err := log.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordUserMessage, Turn: turn, Message: &messages[index]}); err != nil {
			return err
		}
	}
	return nil
}

func nextTurn(events []session.Event) uint64 {
	var turn uint64
	for _, event := range events {
		if event.Record.Type == session.RecordTurnStart && event.Record.Turn > turn {
			turn = event.Record.Turn
		}
	}
	return turn + 1
}

// unresolved lists, in commit order, the approval questions without a
// decision and the calls without a result, as resume repair finds them. The
// log accepts step/end only once a step pairs both, so every one belongs to
// the open step.
func unresolved(events []session.Event) (approvals, calls []string) {
	for _, event := range events {
		record := event.Record
		if record.Type == session.RecordApprovalAsked {
			approvals = append(approvals, record.Approval.ID)
		}
		if record.Type == session.RecordApprovalDecided {
			approvals = slices.DeleteFunc(approvals, func(id string) bool { return id == record.Approval.ID })
		}
		if record.Type == session.RecordToolCall {
			calls = append(calls, record.Call.ID)
		}
		if record.Type == session.RecordToolResult {
			calls = slices.DeleteFunc(calls, func(id string) bool { return id == record.Result.CallID })
		}
	}
	return approvals, calls
}

// requestRoute is the route one request uses: a delegated agent's
// inherited route, or else the hot settings route with its catalog effort.
func requestRoute(document settings.Document, inherited session.SubagentRoute) session.SubagentRoute {
	if inherited != (session.SubagentRoute{}) {
		return inherited
	}
	return session.SubagentRoute{Provider: document.Route.Provider, Model: document.Route.Model, Effort: findModel(document, document.Route.Provider, document.Route.Model).Effort}
}

func findModel(document settings.Document, provider, model string) settings.Model {
	for _, candidate := range document.Providers[provider].Models {
		if candidate.ID == model {
			return candidate
		}
	}
	return settings.Model{ID: model}
}

// outcomeFor settles a failed turn. The turn's own cancellation wins over
// whatever failure it caused or raced, including a provider timeout that
// fired first; a cancellation that reached the turn through a dependency,
// such as a service stopping at shutdown, counts too. Any other failure,
// including a deadline a dependency enforced internally, is an error, so
// pending notices still open the next turn.
func outcomeFor(ctx context.Context, err error) session.TurnOutcome {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return session.OutcomeCanceled
	}
	return session.OutcomeError
}

func cloneMessage(message session.Message) session.Message {
	copyMessage := message
	copyMessage.Content = slices.Clone(message.Content)
	for index := range copyMessage.Content {
		if copyMessage.Content[index].Image != nil {
			image := *copyMessage.Content[index].Image
			copyMessage.Content[index].Image = &image
		}
	}
	return copyMessage
}

func validUserMessage(message session.Message) bool {
	if message.Role != session.RoleUser || message.Source.Kind == "" || message.Source.NoticeID != "" || strings.TrimSpace(session.Text(message)) == "" && !slices.ContainsFunc(message.Content, func(block session.ContentBlock) bool { return block.Type == session.ContentImage }) {
		return false
	}
	return (session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message}).Validate() == nil
}
