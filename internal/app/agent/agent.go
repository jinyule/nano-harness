package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/jinyule/nano-harness/internal/app/compaction"
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

	mu            sync.Mutex
	active        bool
	busy          bool
	pending       int
	currentCancel context.CancelFunc
	idleWaiters   []chan struct{}
	last          TurnResult
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
		select {
		case <-ctx.Done():
			return
		case request := <-agent.turns:
			agent.mu.Lock()
			agent.busy = true
			turnContext, cancel := context.WithCancel(ctx)
			agent.currentCancel = cancel
			agent.mu.Unlock()
			result := agent.engine.runTurn(turnContext, runInput{
				journal: agent.journal, message: request.message, persona: agent.persona,
				tools: agent.tools, delegated: agent.delegated, drain: agent.drainSteers,
			})
			cancel()
			agent.mu.Lock()
			agent.currentCancel = nil
			agent.busy = false
			agent.pending--
			agent.last = result
			if agent.pending == 0 {
				agent.notifyIdleLocked()
			}
			agent.mu.Unlock()
			request.result <- result
			close(request.result)
		}
	}
}

// Submit queues one complete user message.
func (agent *Agent) Submit(ctx context.Context, message session.Message) (<-chan TurnResult, error) {
	if !validUserMessage(message) {
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
		if agent.pending == 0 && !agent.busy {
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
	cancel := agent.currentCancel
	agent.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// WhenIdle waits until the active and queued turn count reaches zero.
func (agent *Agent) WhenIdle(ctx context.Context) error {
	agent.mu.Lock()
	if !agent.active {
		agent.mu.Unlock()
		return ErrNotRunning
	}
	if agent.pending == 0 && !agent.busy {
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

func (agent *Agent) close(ctx context.Context, scope *plugin.Scope) error {
	agent.Interrupt()
	return scope.Close(ctx)
}

var _ Controller = (*Agent)(nil)
