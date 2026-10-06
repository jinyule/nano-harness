package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/jinyule/nano-harness/internal/app/compaction"
	"github.com/jinyule/nano-harness/internal/app/plan"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

var beforeAgentDrain = func() {}

type turnRequest struct {
	message session.Message
	result  chan TurnResult
}

// Agent is one sequential turn worker with asynchronous control channels.
type Agent struct {
	engine    *Engine
	journal   *journal
	parentID  string
	label     string
	mode      string
	persona   string
	tools     []string
	depth     int
	delegated bool

	turns  chan turnRequest
	steers chan session.Message
	done   chan struct{}
	// wake unblocks an idle worker after Notify queued a notice.
	wake chan struct{}

	mu            sync.Mutex
	active        bool
	busy          bool
	pending       int
	currentCancel context.CancelFunc
	currentDone   <-chan struct{}
	idleWaiters   []chan struct{}
	last          TurnResult
	// notices wait for the next step boundary; woken asks the worker to
	// open a turn for them because no turn would otherwise deliver them.
	notices []session.Message
	woken   bool
}

func (agent *Agent) start(ctx context.Context, scope *plugin.Scope) error {
	workerContext, cancel := context.WithCancel(ctx)
	if err := scope.Defer(func(closeContext context.Context) error {
		cancel()
		<-agent.done
		agent.journal.closeSubscribers()
		return agent.journal.Close(closeContext)
	}); err != nil {
		cancel()
		return err
	}
	agent.mu.Lock()
	agent.active = true
	agent.mu.Unlock()
	go agent.run(workerContext)
	return nil
}

func (agent *Agent) run(ctx context.Context) {
	defer close(agent.done)
	defer func() {
		agent.mu.Lock()
		agent.active, agent.busy, agent.pending = false, false, 0
		agent.notices, agent.woken = nil, false
		agent.notifyIdleLocked()
		agent.mu.Unlock()
		beforeAgentDrain()
		for {
			select {
			case request := <-agent.turns:
				request.result <- TurnResult{SessionID: agent.journal.Header().SessionID, Outcome: session.OutcomeCanceled, Err: ErrNotRunning}
				close(request.result)
			default:
				return
			}
		}
	}()
	for {
		if notice, ok := agent.claimWake(); ok {
			agent.finishTurn(agent.turn(ctx, notice), false)
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-agent.wake:
		case request := <-agent.turns:
			result := agent.turn(ctx, request.message)
			agent.finishTurn(result, true)
			request.result <- result
			close(request.result)
		}
	}
}

// turn runs one interruptible turn.
func (agent *Agent) turn(ctx context.Context, message session.Message) TurnResult {
	turnContext, cancel := context.WithCancel(ctx)
	agent.mu.Lock()
	agent.busy, agent.currentCancel, agent.currentDone = true, cancel, turnContext.Done()
	agent.mu.Unlock()
	result := agent.engine.runTurn(turnContext, runInput{
		journal: agent.journal, message: message, persona: agent.persona,
		tools: agent.tools, delegated: agent.delegated, drain: agent.drainSteers, notices: agent.drainNotices,
	})
	cancel()
	return result
}

// finishTurn records a settled turn. Notices that arrived too late for it
// open another turn unless a queued turn will deliver them first. A
// cancelled turn only replays a wake requested after cancellation.
func (agent *Agent) finishTurn(result TurnResult, submitted bool) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.currentCancel, agent.currentDone = nil, nil
	agent.busy = false
	if submitted {
		agent.pending--
	}
	// A turn its admission dropped committed nothing and is not the last turn.
	if !errors.Is(result.Err, ErrNotAdmitted) {
		agent.last = result
	}
	agent.woken = len(agent.notices) > 0 && agent.pending == 0 && (result.Outcome != session.OutcomeCanceled || agent.woken) && agent.mode != "one-shot"
	if agent.pending == 0 && !agent.woken {
		agent.notifyIdleLocked()
	}
}

// claimWake takes the oldest notice as the opening message of a woken
// turn; later notices are delivered at that turn's first boundary. Only
// the worker drains notices, and finishTurn recomputes woken after every
// turn, so a woken agent always holds at least one notice here.
func (agent *Agent) claimWake() (session.Message, bool) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if !agent.woken {
		return session.Message{}, false
	}
	agent.woken = false
	notice := agent.notices[0]
	agent.notices = agent.notices[1:]
	agent.busy = true
	return notice, true
}

// Notify delivers a model-facing notice, such as a background job
// completion. A busy agent appends it as a user message at the next step
// boundary of its active turn, which then cannot close before answering
// it; the last allowed step takes none, so a new turn answers it. An idle
// agent opens a new turn for it. A cancelled turn takes no notices; those
// queued before cancellation wait for the next turn. A notice accepted
// after cancellation wakes that turn as soon as the cancelled turn exits.
// Pending notices are in memory and are lost when the agent stops. A
// one-shot agent runs exactly one turn, so it accepts notices only while
// that turn runs and never opens another for them; notices that arrive too
// late for it are refused or discarded.
func (agent *Agent) Notify(message session.Message) error {
	return agent.NotifyContext(context.Background(), message)
}

// NotifyContext is Notify with a cancellation cutoff under the inbox lock.
// If ctx is cancelled before acceptance, the message is not queued.
func (agent *Agent) NotifyContext(ctx context.Context, message session.Message) error {
	if !validUserMessage(message) {
		return ErrInvalidConfig
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if !agent.active {
		return ErrNotRunning
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if agent.mode == "one-shot" && !agent.busy {
		return ErrInvalidConfig
	}
	agent.notices = append(agent.notices, cloneMessage(message))
	if agent.busy && agent.turnAbortedLocked() {
		agent.woken = true
	}
	if !agent.busy && agent.pending == 0 && !agent.woken {
		agent.woken = true
		select {
		case agent.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

func (agent *Agent) turnAbortedLocked() bool {
	select {
	case <-agent.currentDone:
		return true
	default:
		return false
	}
}

func (agent *Agent) drainNotices() []session.Message {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	// Inputs arriving after cancellation belong to the next turn.
	if agent.turnAbortedLocked() {
		return nil
	}
	notices := agent.notices
	agent.notices = nil
	return notices
}

// Submit queues one complete user message.
func (agent *Agent) Submit(ctx context.Context, message session.Message) (<-chan TurnResult, error) {
	// Delegation preserves even an empty task, represented by a text block.
	delegation := agent.delegated && message.Role == session.RoleUser && message.Source.Kind == "delegation" && (session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message}).Validate() == nil
	if !delegation && !validUserMessage(message) {
		return nil, ErrInvalidConfig
	}
	request := turnRequest{message: cloneMessage(message), result: make(chan TurnResult, 1)}
	agent.mu.Lock()
	if !agent.active {
		agent.mu.Unlock()
		return nil, ErrNotRunning
	}
	if agent.mode == "one-shot" && (agent.pending > 0 || agent.last.Turn > 0) {
		agent.mu.Unlock()
		return nil, ErrInvalidConfig
	}
	agent.pending++
	agent.mu.Unlock()
	select {
	case agent.turns <- request:
		return request.result, nil
	case <-ctx.Done():
		agent.mu.Lock()
		agent.pending--
		if agent.pending == 0 && !agent.busy && !agent.woken {
			agent.notifyIdleLocked()
		}
		agent.mu.Unlock()
		return nil, ctx.Err()
	case <-agent.done:
		agent.mu.Lock()
		agent.pending--
		agent.mu.Unlock()
		return nil, ErrNotRunning
	}
}

// Followup queues a later turn for a continuable agent.
func (agent *Agent) Followup(ctx context.Context, message session.Message) (<-chan TurnResult, error) {
	if agent.mode == "one-shot" {
		return nil, ErrInvalidConfig
	}
	return agent.Submit(ctx, message)
}

// Steer injects a user message at the next tool-step boundary.
func (agent *Agent) Steer(ctx context.Context, message session.Message) error {
	if !validUserMessage(message) {
		return ErrInvalidConfig
	}
	agent.mu.Lock()
	busy := agent.active && agent.busy
	agent.mu.Unlock()
	if !busy {
		return ErrAgentIdle
	}
	select {
	case agent.steers <- cloneMessage(message):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-agent.done:
		return ErrNotRunning
	}
}

func (agent *Agent) drainSteers() []session.Message {
	messages := make([]session.Message, 0)
	for {
		select {
		case message := <-agent.steers:
			messages = append(messages, message)
		default:
			return messages
		}
	}
}

// Interrupt cancels only the active turn; queued followups remain ordered.
func (agent *Agent) Interrupt() {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if agent.currentCancel != nil {
		agent.currentCancel()
	}
}

// WhenIdle waits until no turn is active, queued, or woken by a notice.
func (agent *Agent) WhenIdle(ctx context.Context) error {
	agent.mu.Lock()
	if !agent.active {
		agent.mu.Unlock()
		return ErrNotRunning
	}
	if agent.pending == 0 && !agent.busy && !agent.woken {
		agent.mu.Unlock()
		return nil
	}
	waiter := make(chan struct{})
	agent.idleWaiters = append(agent.idleWaiters, waiter)
	agent.mu.Unlock()
	select {
	case <-waiter:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (agent *Agent) notifyIdleLocked() {
	for _, waiter := range agent.idleWaiters {
		close(waiter)
	}
	agent.idleWaiters = nil
}

// Subscribe publishes future durable events; dropped updates remain replayable from disk.
func (agent *Agent) Subscribe(buffer int) (<-chan session.Event, func(), error) {
	agent.mu.Lock()
	active := agent.active
	agent.mu.Unlock()
	if !active {
		return nil, nil, ErrNotRunning
	}
	return agent.journal.subscribe(buffer)
}

// Status returns a detached live snapshot.
func (agent *Agent) Status() Status {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return Status{SessionID: agent.journal.Header().SessionID, ParentID: agent.parentID, Label: agent.label, Mode: agent.mode, Depth: agent.depth, Busy: agent.busy, Pending: agent.pending, Last: agent.last}
}

// Surface returns a detached current model-visible transcript for explicit forks.
func (agent *Agent) Surface(ctx context.Context) ([]session.SurfaceNode, error) {
	events, err := agent.journal.Events(ctx)
	if err != nil {
		return nil, err
	}
	return session.Surface(events)
}

// Events returns a detached durable snapshot for local presentation.
func (agent *Agent) Events(ctx context.Context) ([]session.Event, error) {
	return agent.journal.Events(ctx)
}

// Compact runs an explicit idle-only model-surface compaction.
func (agent *Agent) Compact(ctx context.Context) (bool, error) {
	agent.mu.Lock()
	if !agent.active {
		agent.mu.Unlock()
		return false, ErrNotRunning
	}
	if agent.pending != 0 || agent.busy {
		agent.mu.Unlock()
		return false, errors.New("agent must be idle before manual compaction")
	}
	agent.mu.Unlock()
	return agent.engine.compaction.Maybe(ctx, compaction.Request{Journal: agent.journal, Force: true})
}

// selectPlan applies a plan-mode selection while holding the worker's state
// lock. The worker marks itself busy under the same lock before it appends
// turn/start, so an immediate commit between turns can never interleave
// with a turn starting.
func (agent *Agent) selectPlan(ctx context.Context, active bool) (plan.Change, error) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if !agent.active {
		return "", ErrNotRunning
	}
	return agent.engine.plan.Select(ctx, agent.journal, active, agent.busy)
}

func (agent *Agent) close(ctx context.Context, scope *plugin.Scope) error {
	agent.Interrupt()
	return scope.Close(ctx)
}

var _ Controller = (*Agent)(nil)
