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
	settings   *settings.Service
	maxSteps   int

	mu      sync.RWMutex
	started bool
	active  bool
}

// NewEngine validates the full core-loop dependency graph.
func NewEngine(runtime *llm.Runtime, tools *appTool.Runtime, retries *retry.Service, compactor *compaction.Service, assembler *prompt.Assembler, configuration *settings.Service, config EngineConfig) (*Engine, error) {
	if runtime == nil || tools == nil || retries == nil || compactor == nil || assembler == nil || configuration == nil || config.MaxSteps < 0 || config.MaxSteps > 256 {
		return nil, ErrInvalidConfig
	}
	if config.MaxSteps == 0 {
		config.MaxSteps = 32
	}
	return &Engine{llm: runtime, tools: tools, retry: retries, compaction: compactor, prompt: assembler, settings: configuration, maxSteps: config.MaxSteps}, nil
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
	drain     func() []session.Message
}

func (engine *Engine) runTurn(ctx context.Context, input runInput) (result TurnResult) {
	result.SessionID = input.journal.Header().SessionID
	engine.mu.RLock()
	active := engine.active
	engine.mu.RUnlock()
	if !active {
		result.Err, result.Outcome = ErrNotRunning, session.OutcomeError
		return result
	}
	events, err := input.journal.Events(ctx)
	if err != nil {
		result.Err, result.Outcome = err, session.OutcomeError
		return result
	}
	turn := nextTurn(events)
	result.Turn = turn
	if _, err = input.journal.Append(ctx, session.Record{Type: session.RecordTurnStart, Turn: turn}); err != nil {
		result.Err, result.Outcome = err, session.OutcomeError
		return result
	}
	if _, err = input.journal.Append(ctx, session.Record{Type: session.RecordUserMessage, Turn: turn, Message: &input.message}); err != nil {
		result.Err, result.Outcome = err, session.OutcomeError
		return result
	}
	turnOpen := true
	stepOpen := false
	var openStep uint64
	defer func() {
		if recovered := recover(); recovered != nil {
			result.Err = errors.New("agent loop panicked")
			result.Outcome = session.OutcomeError
		}
		if stepOpen {
			_, closeErr := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordStepEnd, Turn: turn, Step: openStep})
			result.Err = errors.Join(result.Err, closeErr)
		}
		if turnOpen {
			_, closeErr := input.journal.Append(context.WithoutCancel(ctx), session.Record{Type: session.RecordTurnEnd, Turn: turn, Outcome: result.Outcome})
			result.Err = errors.Join(result.Err, closeErr)
		}
	}()

	maxSteps := uint64(engine.maxSteps) //nolint:gosec // construction restricts maxSteps to the positive range 1-256
	for step := uint64(1); step <= maxSteps; step++ {
		if err := ctx.Err(); err != nil {
			result.Err, result.Outcome = err, session.OutcomeCanceled
			return result
		}
		if _, err := engine.compaction.Maybe(ctx, compaction.Request{Journal: input.journal, Turn: turn}); err != nil {
			result.Err, result.Outcome = fmt.Errorf("proactive compaction: %w", err), session.OutcomeError
			return result
		}
		if _, err := input.journal.Append(ctx, session.Record{Type: session.RecordStepStart, Turn: turn, Step: step}); err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		stepOpen = true
		openStep = step
		document, _, err := engine.settings.Snapshot()
		if err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		definitions, err := engine.tools.Definitions(input.tools)
		if err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		system, err := engine.prompt.Build(prompt.Input{
			Workspace: input.journal.Header().Cwd, Provider: document.Route.Provider, Model: document.Route.Model,
			Persona: input.persona, Delegated: input.delegated, Tools: definitions,
		})
		if err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		model := findModel(document, document.Route.Provider, document.Route.Model)
		header := &session.RequestHeader{Provider: document.Route.Provider, Model: document.Route.Model, System: system, Tools: definitions, ContextWindow: model.ContextWindow}
		if _, err := input.journal.Append(ctx, session.Record{Type: session.RecordRequestHeader, Turn: turn, Step: step, Header: header}); err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		call, err := engine.llm.PrepareCall(ctx, document.Route.Provider, document.Route.Model)
		if err != nil {
			result.Err, result.Outcome = err, outcomeFor(err)
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
		completion, err := engine.retry.Do(ctx, input.journal, turn, step, document.Route.Provider, document.Route.Provider+"/"+document.Route.Model, func() (llm.Completion, bool, error) {
			emitted := false
			completion, streamErr := call.Stream(ctx, llm.Request{
				SessionID: result.SessionID, Purpose: "agent", System: system,
				Surface: surface, Tools: definitions,
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
				compacted, compactErr := engine.compaction.Maybe(ctx, compaction.Request{Journal: input.journal, Turn: turn, Force: true})
				if compactErr != nil || !compacted {
					result.Err, result.Outcome = errors.Join(err, compactErr), session.OutcomeError
					return result
				}
				continue
			}
			result.Err, result.Outcome = err, outcomeFor(err)
			return result
		}
		message := completion.Message
		message.Role = session.RoleAssistant
		if message.Source.Kind == "" {
			message.Source = session.MessageSource{Kind: "provider", Plugin: document.Route.Provider}
		}
		if _, err := input.journal.Append(ctx, session.Record{Type: session.RecordAssistantMessage, Turn: turn, Step: step, Message: &message}); err != nil {
			result.Err, result.Outcome = err, session.OutcomeError
			return result
		}
		for index := range completion.Calls {
			call := completion.Calls[index]
			if _, err := input.journal.Append(ctx, session.Record{Type: session.RecordToolCall, Turn: turn, Step: step, Call: &call}); err != nil {
				result.Err, result.Outcome = err, session.OutcomeError
				return result
			}
		}
		result.Text = session.Text(message)
		if len(completion.Calls) == 0 {
			if _, err := input.journal.Append(ctx, session.Record{Type: session.RecordStepEnd, Turn: turn, Step: step, Usage: completion.Usage}); err != nil {
				result.Err, result.Outcome = err, session.OutcomeError
				return result
			}
			stepOpen = false
			openStep = 0
			if _, err := input.journal.Append(ctx, session.Record{Type: session.RecordTurnEnd, Turn: turn, Outcome: session.OutcomeCompleted}); err != nil {
				result.Err, result.Outcome = err, session.OutcomeError
				return result
			}
			turnOpen = false
			result.Outcome = session.OutcomeCompleted
			return result
		}
		toolResults := engine.tools.ExecuteBatch(ctx, appTool.BatchRequest{
			SessionID: result.SessionID, Cwd: input.journal.Header().Cwd, Turn: turn, Step: step,
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
		for _, steer := range input.drain() {
			steerCopy := steer
			if _, err := input.journal.Append(ctx, session.Record{Type: session.RecordUserMessage, Turn: turn, Message: &steerCopy}); err != nil {
				result.Err, result.Outcome = err, session.OutcomeError
				return result
			}
		}
	}
	result.Outcome = session.OutcomeStepLimit
	return result
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

func findModel(document settings.Document, provider, model string) settings.Model {
	for _, candidate := range document.Providers[provider].Models {
		if candidate.ID == model {
			return candidate
		}
	}
	return settings.Model{ID: model}
}

func outcomeFor(err error) session.TurnOutcome {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
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
	if message.Role != session.RoleUser || message.Source.Kind == "" || strings.TrimSpace(session.Text(message)) == "" && !slices.ContainsFunc(message.Content, func(block session.ContentBlock) bool { return block.Type == session.ContentImage }) {
		return false
	}
	return (session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message}).Validate() == nil
}
