// Package subagent coordinates full in-process delegated agents: one-shot
// runs in the foreground or as background jobs, background continuable
// children that settle and cold-resume, messages between direct parents and
// children, interruption of descendants, and discovery from parent-owned
// catalog records.
package subagent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/app/transcript"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	// maxDelegationDepth bounds the absolute depth of any child.
	maxDelegationDepth = 4
	// maxActiveChildren bounds live continuable children in one pool: the
	// children of a root or one-shot agent plus all continuable children
	// below them through continuable parents.
	maxActiveChildren = 8
	// JobKind is the background job kind of one-shot children.
	JobKind = "subagent"
)

var (
	// ErrInvalidConfig identifies an unusable service dependency.
	ErrInvalidConfig = errors.New("invalid subagent configuration")
	// ErrNotRunning indicates the subagent service has not started or has stopped.
	ErrNotRunning = errors.New("subagent service is not running")

	errTeardownFailed = errors.New("subagent activation teardown failed")

	closeAgent    = func(registry *agent.Registry, ctx context.Context, id string) error { return registry.Close(ctx, id) }
	beforePublish = func(*agent.Agent) {}
	beforeResume  = func() {}
	// parked observes a settlement watcher that keeps its child resident.
	parked = func(string) {}
	// released observes, under Service.mu, a child whose release waiters
	// were just woken.
	released = func(string) {}
)

// Code classifies a model-facing delegation failure.
type Code string

const (
	// CodeInvalidRequest identifies arguments the service cannot accept.
	CodeInvalidRequest Code = "INVALID_REQUEST"
	// CodeDepthLimit identifies a delegation that would exceed the depth cap.
	CodeDepthLimit Code = "DEPTH_LIMIT"
	// CodeLimitReached identifies a full continuable pool.
	CodeLimitReached Code = "ACTIVATION_LIMIT_REACHED"
	// CodeUnauthorized identifies an operation outside the caller's lineage.
	CodeUnauthorized Code = "UNAUTHORIZED"
	// CodeNotResumable identifies a target that cannot accept messages.
	CodeNotResumable Code = "NOT_RESUMABLE"
	// CodeParentUnavailable identifies a child message whose parent is not live.
	CodeParentUnavailable Code = "PARENT_UNAVAILABLE"
	// CodeAborted identifies cancellation at inbox acceptance.
	CodeAborted Code = "ABORTED"
	// CodeAbortedBeforeDispatch identifies cancellation before scheduling.
	CodeAbortedBeforeDispatch Code = "ABORTED_BEFORE_DISPATCH"
	// CodeTeardownFailed identifies failure to release a child and its resources.
	CodeTeardownFailed Code = "ACTIVATION_TEARDOWN_FAILED"
)

// Error is a delegation failure whose message is shown to the model.
type Error struct {
	Code    Code
	Message string
	Err     error
}

func (failure *Error) Error() string { return failure.Message }

// Unwrap exposes an underlying cause, such as a transcript failure.
func (failure *Error) Unwrap() error { return failure.Err }

func fail(code Code, format string, values ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, values...)}
}

// Journal is the caller's durable log; a catalog record cites the caller's
// active tool step.
type Journal interface {
	Append(context.Context, session.Record) (session.Event, error)
}

// StartRequest delegates one task from the calling agent. Turn and Step
// locate the caller's tool call so the catalog record lands in its step.
type StartRequest struct {
	ParentID    string
	Journal     Journal
	Turn        uint64
	Step        uint64
	Description string
	Prompt      string
	// Fork seeds the child with the parent's completed turns instead of
	// starting a fresh conversation.
	Fork bool
}

// Report is the terminal state of a one-shot child.
type Report struct {
	Outcome session.TurnOutcome
	// Text is the child's closing answer, or its partial streamed answer
	// when the run did not finish.
	Text string
}

// Entry is one row of a catalog listing. Depth counts from the caller, so
// direct children have depth 1. Unavailable reports a child whose own
// catalog could not be read; its children are skipped.
type Entry struct {
	ID          string
	Parent      string
	Label       string
	Mode        string
	Depth       int
	Running     bool
	Unavailable bool
}

// Info is a secret-free snapshot of a live child for presentation.
type Info struct {
	SessionID string
	ParentID  string
	Label     string
	Mode      string
	Depth     int
	Busy      bool
	Pending   int
	Last      agent.TurnResult
}

// child is the service's handle on one live delegated agent, guarded by
// Service.mu. One-shot children live until their run is collected;
// continuable children stay resident until they settle.
type child struct {
	id, parent, label, mode string
	depth                   int
	// pool is the continuable capacity pool; empty for one-shot children.
	pool  string
	agent *agent.Agent
	// boundary counts the events before this residency, so settlement
	// reports only what this residency produced.
	boundary int
	// announced records that a delivery was accepted, so settlement owes
	// the parent a notice.
	announced bool
	// delivered counts the messages and settlement notices this service
	// handed to the agent during this residency. A child whose interrupted
	// turn left some of them uncommitted stays resident until the next
	// delivery opens a turn that commits them.
	delivered int
	// watched records that the settlement watcher runs; it starts with the
	// first accepted delivery.
	watched bool
	closing bool
	done    chan struct{}
	// closeErr is published before done closes and shared by release waiters.
	closeErr error
	// generation counts accepted deliveries and child removals; wake is
	// closed and replaced on each. The settlement watcher compares
	// generations across an idle wait so a message accepted meanwhile keeps
	// the child resident.
	generation uint64
	wake       chan struct{}
}

// Service owns delegated handles while the registry owns their worker
// scopes and the job service owns background one-shot runs.
type Service struct {
	registry   *agent.Registry
	jobs       *job.Service
	repository transcript.Repository

	mu      sync.Mutex
	started bool
	active  bool
	ctx     context.Context
	cancel  context.CancelFunc
	// children holds live handles and, while a child is being cold-resumed,
	// a closing placeholder without an agent that deliveries wait on.
	children map[string]*child
	// reserved counts pool slots held by creations in flight.
	reserved map[string]int
	watchers sync.WaitGroup
}

// New constructs an inactive delegation service. Background one-shot runs
// are jobs owned by the parent; repository reads the catalogs of children
// that are not live.
func New(registry *agent.Registry, jobs *job.Service, repository transcript.Repository) (*Service, error) {
	if registry == nil || jobs == nil || repository == nil {
		return nil, ErrInvalidConfig
	}
	return &Service{registry: registry, jobs: jobs, repository: repository, children: map[string]*child{}, reserved: map[string]int{}}, nil
}

// ID returns the stable plugin identity.
func (*Service) ID() string { return "subagents" }

// Start owns settlement watchers and every child created through this service.
func (service *Service) Start(ctx context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.started {
		return ErrInvalidConfig
	}
	service.ctx, service.cancel = context.WithCancel(ctx)
	if err := scope.Defer(service.stop); err != nil {
		service.cancel()
		return err
	}
	service.started, service.active = true, true
	return nil
}

// stop refuses new work, stops the settlement watchers, and closes every
// live child, deepest first, without settlement notices.
func (service *Service) stop(ctx context.Context) error {
	service.mu.Lock()
	service.active = false
	service.cancel()
	children := make([]*child, 0, len(service.children))
	for _, current := range service.children {
		children = append(children, current)
	}
	service.mu.Unlock()
	service.watchers.Wait()
	slices.SortFunc(children, func(left, right *child) int { return right.depth - left.depth })
	var failures []error
	for _, current := range children {
		failures = append(failures, service.close(ctx, current, nil))
	}
	return errors.Join(failures...)
}

// StartContinuable creates a background continuable child, submits its task,
// and returns its id without waiting. The child stays resident while it
// works, receives messages, or has continuable children of its own; when it
// settles, the service releases it and notifies the parent.
func (service *Service) StartContinuable(ctx context.Context, request StartRequest) (string, error) {
	current, err := service.create(ctx, request, session.SubagentContinuable)
	if err != nil {
		return "", err
	}
	if _, err := current.agent.Submit(ctx, taskMessage(request.Prompt, request.ParentID, true)); err != nil {
		return "", errors.Join(err, service.close(context.WithoutCancel(ctx), current, nil))
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	acceptedLocked(service, current)
	return current.id, nil
}

// Run creates a one-shot child, waits for its single turn, releases it, and
// returns its report. Cancelling ctx releases the child and returns ctx's
// error.
func (service *Service) Run(ctx context.Context, request StartRequest) (Report, error) {
	current, err := service.create(ctx, request, session.SubagentOneShot)
	if err != nil {
		return Report{}, err
	}
	return service.finish(ctx, current, request.Prompt)
}

// StartBackground admits a parent-owned job before creating its one-shot
// child. Startup uses the job's cancellation signal and failures become job
// output. It waits for startup to commit the catalog in the caller's active
// step, then returns the job id without waiting for the child's turn.
func (service *Service) StartBackground(ctx context.Context, request StartRequest) (string, error) {
	if _, err := checkStart(request); err != nil {
		return "", err
	}
	if !service.running() {
		return "", ErrNotRunning
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	started := make(chan struct{})
	id, err := service.jobs.Launch(job.Spec{Kind: JobKind, Label: request.Description, Owner: request.ParentID, Run: func(jobContext context.Context, _ *job.Output) job.Outcome {
		current, err := func() (*child, error) {
			defer close(started)
			return service.create(jobContext, request, session.SubagentOneShot)
		}()
		if err != nil {
			return jobOutcome(Report{}, err)
		}
		report, err := service.finish(jobContext, current, request.Prompt)
		return jobOutcome(report, err)
	}})
	if err != nil {
		return "", err
	}
	// The parent step cannot close before its catalog append finishes.
	<-started
	return id, nil
}

// jobOutcome maps a background run to its job settlement. Teardown failure
// takes priority over cancellation and withholds the child's output; other
// cancelled runs are killed and unfinished runs fail with their stop reason.
func jobOutcome(report Report, err error) job.Outcome {
	switch {
	case errors.Is(err, errTeardownFailed):
		return job.Outcome{Status: job.StatusFailed, Detail: err.Error()}
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return job.Outcome{Status: job.StatusKilled}
	case err != nil:
		return job.Outcome{Status: job.StatusFailed, Detail: err.Error()}
	case report.Outcome == session.OutcomeCompleted:
		return job.Outcome{Status: job.StatusCompleted, Result: report.Text}
	case report.Outcome == session.OutcomeCanceled || report.Outcome == session.OutcomeInterrupted:
		return job.Outcome{Status: job.StatusKilled}
	default:
		return job.Outcome{Status: job.StatusFailed, Detail: string(report.Outcome)}
	}
}

// finish drives a one-shot child's single turn, reads its closing answer,
// and releases it.
func (service *Service) finish(ctx context.Context, current *child, prompt string) (Report, error) {
	release := context.WithoutCancel(ctx)
	results, err := current.agent.Submit(ctx, taskMessage(prompt, current.parent, false))
	if err != nil {
		return Report{}, errors.Join(err, service.close(release, current, nil))
	}
	var result agent.TurnResult
	select {
	case result = <-results:
	case <-ctx.Done():
		return Report{}, errors.Join(ctx.Err(), service.close(release, current, nil))
	}
	report := Report{Outcome: result.Outcome}
	if events, err := current.agent.Events(release); err == nil {
		report.Text = session.FinalAssistantText(events[current.boundary:])
	}
	return report, service.close(release, current, nil)
}

// create validates a delegation, creates the child agent, publishes its
// handle, and records it in the parent's catalog. Any failure after the
// agent exists releases it.
func (service *Service) create(ctx context.Context, request StartRequest, mode string) (*child, error) {
	label, err := checkStart(request)
	if err != nil {
		return nil, err
	}
	if !service.running() {
		return nil, ErrNotRunning
	}
	parent, err := service.registry.Find(request.ParentID)
	if err != nil {
		return nil, err
	}
	depth := parent.Status().Depth + 1
	if depth > maxDelegationDepth {
		return nil, fail(CodeDepthLimit, "subagent depth %d exceeds maxDepth %d", depth, maxDelegationDepth)
	}
	provider, seed := session.SubagentSpawn, []session.Event(nil)
	if request.Fork {
		events, err := parent.Events(ctx)
		if err != nil {
			return nil, err
		}
		provider, seed = session.SubagentFork, completedTurns(events)
	}
	pool := ""
	reserved := false
	if mode == session.SubagentContinuable {
		if pool, err = service.reserve(request.ParentID); err != nil {
			return nil, err
		}
		reserved = true
		defer func() {
			if reserved {
				service.unreserve(pool)
			}
		}()
	}
	created, err := service.registry.Create(ctx, agent.CreateRequest{
		ParentID: request.ParentID, Label: label, Mode: mode, Provider: provider, Seed: seed, Depth: depth, Create: true,
	})
	if err != nil {
		return nil, err
	}
	// The child's own events begin after the inherited seed.
	current := &child{id: created.Status().SessionID, parent: request.ParentID, label: label, mode: mode, depth: depth, pool: pool, agent: created, boundary: len(seed), done: make(chan struct{}), wake: make(chan struct{})}
	beforePublish(created)
	if err := service.publish(current); err != nil {
		return nil, errors.Join(err, teardownFailure(current.id, closeAgent(service.registry, context.WithoutCancel(ctx), current.id)))
	}
	reserved = false
	catalog := session.Record{Type: session.RecordSubagentCatalog, Turn: request.Turn, Step: request.Step, Catalog: &session.SubagentCatalog{SessionID: current.id, Mode: mode, Label: label}}
	if _, err := request.Journal.Append(ctx, catalog); err != nil {
		return nil, errors.Join(err, service.close(context.WithoutCancel(ctx), current, nil))
	}
	return current, nil
}

// checkStart preserves model-supplied strings and rejects explicit bounds
// before creating a transcript. Empty descriptions and prompts are valid;
// the job service separately requires a non-empty background job label.
func checkStart(request StartRequest) (string, error) {
	if request.Journal == nil {
		return "", fail(CodeInvalidRequest, "subagent delegation requires a calling session journal")
	}
	if len(request.Description) > session.MaxSubagentLabelBytes {
		return "", fail(CodeInvalidRequest, "invalid description: at most %d bytes", session.MaxSubagentLabelBytes)
	}
	if len(request.Prompt) > session.MaxTextBytes {
		return "", fail(CodeInvalidRequest, "invalid prompt: at most %d bytes", session.MaxTextBytes)
	}
	return request.Description, nil
}

// completedTurns is the balanced prefix a fork inherits: every event up to
// and including the last turn/end. The caller's in-flight turn is excluded;
// before any completed turn the fork starts fresh.
func completedTurns(events []session.Event) []session.Event {
	for index, event := range slices.Backward(events) {
		if event.Record.Type == session.RecordTurnEnd {
			return events[:index+1]
		}
	}
	return nil
}

func (service *Service) running() bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.active
}

// poolLocked returns the continuable pool a new child of parentID joins:
// its continuable parent's pool, or a pool rooted at any other parent.
func (service *Service) poolLocked(parentID string) string {
	if parent := service.children[parentID]; parent != nil && parent.mode == session.SubagentContinuable {
		return parent.pool
	}
	return parentID
}

// reserve holds one slot in parentID's pool for a creation in flight.
func (service *Service) reserve(parentID string) (string, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	pool, err := service.admitLocked(parentID)
	if err != nil {
		return "", err
	}
	service.reserved[pool]++
	return pool, nil
}

// admitLocked returns the pool a new continuable child of parentID joins if
// it has a free slot. Admission never queues: a parent waiting on its own
// descendants must not wait for its own slot.
func (service *Service) admitLocked(parentID string) (string, error) {
	pool := service.poolLocked(parentID)
	used := service.reserved[pool]
	for _, current := range service.children {
		if current.pool == pool {
			used++
		}
	}
	if used >= maxActiveChildren {
		return "", fail(CodeLimitReached, "subagent limit reached (active child limit: %d); wait for an existing child to finish or complete this work with the current agents", maxActiveChildren)
	}
	return pool, nil
}

func (service *Service) unreserve(pool string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.unreserveLocked(pool)
}

func (service *Service) unreserveLocked(pool string) {
	service.reserved[pool]--
	if service.reserved[pool] == 0 {
		delete(service.reserved, pool)
	}
}

// publish makes a handle visible unless the service stopped or the parent
// handle began closing, which would otherwise miss the new child.
func (service *Service) publish(current *child) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.active {
		return ErrNotRunning
	}
	if parent := service.children[current.parent]; parent != nil && parent.closing {
		return fail(CodeUnauthorized, "subagent parent %q is being released; the child was not established", current.parent)
	}
	// Publishing transfers the reservation into the resident handle atomically.
	if current.pool != "" {
		service.unreserveLocked(current.pool)
	}
	service.children[current.id] = current
	return nil
}

// acceptedLocked records the first accepted delivery: the child now owes
// its parent a settlement notice and is watched until it settles. After
// stop began, stop owns the child and no watcher starts.
func acceptedLocked(service *Service, current *child) {
	current.announced = true
	if !current.watched && service.active {
		current.watched = true
		service.watchers.Add(1)
		go service.watch(current)
	}
}

// wakeLocked records a state change the settlement watcher must observe.
func wakeLocked(current *child) {
	current.generation++
	close(current.wake)
	current.wake = make(chan struct{})
}

func (service *Service) hasContinuableChildrenLocked(id string) bool {
	for _, current := range service.children {
		if current.parent == id && current.mode == session.SubagentContinuable {
			return true
		}
	}
	return false
}

// watch settles one continuable child: once it is idle, no delivery arrived
// during the idle wait, its log holds every message delivered to it, and it
// has no continuable children, the watcher releases it and notifies its
// parent with how its last turn ended.
func (service *Service) watch(current *child) {
	defer service.watchers.Done()
	for {
		service.mu.Lock()
		if current.closing {
			// Another path is releasing the child.
			service.mu.Unlock()
			return
		}
		generation, wake := current.generation, current.wake
		service.mu.Unlock()
		if err := current.agent.WhenIdle(service.ctx); err != nil {
			return
		}
		// A log that can no longer be read belongs to an agent that stopped;
		// it settles with no pending messages and a default outcome.
		var own []session.Event
		events, err := current.agent.Events(service.ctx)
		if err == nil {
			own = events[current.boundary:]
		}
		outcome, ended := session.LastOutcome(own)
		if !ended {
			outcome = session.OutcomeCompleted
		}
		committed := relayed(own)
		service.mu.Lock()
		// An agent leaves accepted messages queued without opening a turn
		// for them only after a cancelled turn; after any other ending a
		// shortfall means nothing is left to wait for.
		queued := committed < current.delivered && outcome == session.OutcomeCanceled
		switch {
		case current.generation != generation:
			// A delivery or closing advanced the generation; re-check.
			service.mu.Unlock()
			continue
		case service.hasContinuableChildrenLocked(current.id) || queued:
			// A working child, or a message an interrupted turn left
			// queued, keeps the child resident until the next delivery or
			// child release wakes it.
			service.mu.Unlock()
			parked(current.id)
			select {
			case <-wake:
				continue
			case <-service.ctx.Done():
				return
			}
		}
		current.closing = true
		announced := current.announced
		service.mu.Unlock()
		var notice *session.Message
		if announced {
			message := settlementMessage(current.id, outcome, session.FinalAssistantText(own))
			notice = &message
		}
		// release reflects cleanup failure in the parent notice before removing the handle.
		_ = service.release(context.WithoutCancel(service.ctx), current, notice)
		return
	}
}

// relayed counts the committed messages in events that this service
// delivered: agent messages and settlement notices.
func relayed(events []session.Event) int {
	count := 0
	for _, event := range events {
		if record := event.Record; record.Type == session.RecordUserMessage && (record.Message.Source.Kind == SourceAgentMessage || record.Message.Source.Kind == SourceSettled) {
			count++
		}
	}
	return count
}

// deliveredLocked records one message the agent of a resident child
// accepted from this service.
func deliveredLocked(current *child) {
	current.delivered++
	wakeLocked(current)
}

// close releases a child and everything below it. Concurrent callers wait
// for the first release.
func (service *Service) close(ctx context.Context, current *child, notice *session.Message) error {
	service.mu.Lock()
	if current.closing {
		done := current.done
		service.mu.Unlock()
		if err := waitFor(ctx, done); err != nil {
			return err
		}
		service.mu.Lock()
		defer service.mu.Unlock()
		return current.closeErr
	}
	current.closing = true
	// A watcher parked on this child's children re-checks and exits.
	wakeLocked(current)
	service.mu.Unlock()
	return service.release(ctx, current, notice)
}

// release finishes closing a child its caller marked closing: it interrupts
// the child, releases its live children first, closes its agent and
// transcript, and ends the jobs it owned. It then removes the handle and,
// in the same critical section, delivers notice to the parent, so a
// continuable parent cannot settle between the two. Release waiters wake
// last: once they observe the child gone, any notice reached the parent.
func (service *Service) release(ctx context.Context, current *child, notice *session.Message) error {
	current.agent.Interrupt()
	service.mu.Lock()
	var descendants []*child
	for _, candidate := range service.children {
		if candidate.parent == current.id {
			descendants = append(descendants, candidate)
		}
	}
	service.mu.Unlock()
	var failures []error
	for _, descendant := range descendants {
		failures = append(failures, service.close(ctx, descendant, nil))
	}
	if err := closeAgent(service.registry, ctx, current.id); err != nil && !errors.Is(err, agent.ErrAgentNotFound) {
		failures = append(failures, err)
	}
	failures = append(failures, service.jobs.Release(ctx, current.id))
	failure := teardownFailure(current.id, errors.Join(failures...))
	if failure != nil && notice != nil {
		message := settlementMessage(current.id, session.OutcomeError, "")
		notice = &message
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	delete(service.children, current.id)
	current.closeErr = failure
	parent := service.children[current.parent]
	delivered := false
	if notice != nil && service.active && (parent == nil || !parent.closing) {
		// The only failure is a parent that is no longer live; the notice
		// then has no reader, as during teardown.
		delivered = service.registry.Notify(current.parent, *notice) == nil
	}
	if parent != nil {
		if delivered {
			parent.delivered++
		}
		wakeLocked(parent)
	}
	close(current.done)
	released(current.id)
	return failure
}

// teardownFailure preserves a cleanup marker across joined startup and
// cancellation errors; their order cannot change the terminal classification.
func teardownFailure(id string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Code: CodeTeardownFailed, Message: fmt.Sprintf("subagent %q activation teardown failed: %v", id, err), Err: fmt.Errorf("%w: %w", errTeardownFailed, err)}
}

// SendMessage delivers one model-authored message between a direct parent
// and child. A resident continuable child may write to its direct parent;
// any agent may write to its own direct continuable child, which is
// cold-resumed from its transcript when it is not resident. A working
// recipient receives the message at its next step boundary and an idle one
// starts a turn with it. The call returns once the message is accepted.
func (service *Service) SendMessage(ctx context.Context, senderID, targetID, text string) error {
	if err := ctx.Err(); err != nil {
		return &Error{Code: CodeAbortedBeforeDispatch, Message: "tool call aborted before dispatch", Err: err}
	}
	if len(text) > session.MaxTextBytes {
		return fail(CodeInvalidRequest, "invalid message: at most %d bytes", session.MaxTextBytes)
	}
	sender, err := service.registry.Find(senderID)
	if err != nil {
		return fail(CodeUnauthorized, "message delivery requires the exact live sender agent")
	}
	for {
		if retry, err := service.deliver(ctx, sender, senderID, targetID, text); err != nil || !retry {
			if err != nil && ctx.Err() != nil {
				return &Error{Code: CodeAborted, Message: "tool call aborted", Err: err}
			}
			return err
		}
	}
}

// deliver makes one delivery attempt; it reports retry when the target
// changed residency and the caller should try again.
func (service *Service) deliver(ctx context.Context, sender *agent.Agent, senderID, targetID, text string) (bool, error) {
	service.mu.Lock()
	if err := ctx.Err(); err != nil {
		service.mu.Unlock()
		return false, &Error{Code: CodeAborted, Message: "tool call aborted", Err: err}
	}
	if !service.active {
		service.mu.Unlock()
		return false, ErrNotRunning
	}
	if self := service.children[senderID]; self != nil && self.parent == targetID && self.mode == session.SubagentContinuable && !self.closing {
		defer service.mu.Unlock()
		parent, err := service.registry.Find(targetID)
		if err != nil {
			return false, &Error{Code: CodeParentUnavailable, Message: "direct parent is not live; the message was not delivered", Err: err}
		}
		if err := notifyMessage(ctx, parent, senderID, text); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return false, err
			}
			return false, &Error{Code: CodeParentUnavailable, Message: "direct parent is not live; the message was not delivered", Err: err}
		}
		if parent := service.children[targetID]; parent != nil {
			deliveredLocked(parent)
		}
		return false, nil
	}
	if sender.Status().ParentID == targetID {
		service.mu.Unlock()
		return false, fail(CodeUnauthorized, "agent %q is not a resident continuable child and cannot send to parent %q", senderID, targetID)
	}
	if target := service.children[targetID]; target != nil {
		if wait := target.done; target.closing {
			service.mu.Unlock()
			return true, waitFor(ctx, wait)
		}
		if err := authorizeChild(target.id, target.parent, target.mode, senderID); err != nil {
			service.mu.Unlock()
			return false, err
		}
		err := notifyMessage(ctx, target.agent, senderID, text)
		if err == nil {
			acceptedLocked(service, target)
			deliveredLocked(target)
		}
		service.mu.Unlock()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return false, err
			}
			// The agent stopped outside this service; drop its handle.
			return false, errors.Join(err, service.close(context.WithoutCancel(ctx), target, nil))
		}
		return false, nil
	}
	service.mu.Unlock()
	return service.resume(ctx, sender, senderID, targetID)
}

// notifyMessage checks cancellation at the recipient's inbox acceptance.
func notifyMessage(ctx context.Context, recipient *agent.Agent, senderID, text string) error {
	if err := recipient.NotifyContext(ctx, agentMessage(senderID, text)); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return &Error{Code: CodeAborted, Message: "tool call aborted", Err: err}
		}
		return err
	}
	return nil
}

func waitFor(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// authorizeChild accepts only the sender's own continuable child.
func authorizeChild(id, parentID, mode, senderID string) error {
	if parentID != senderID {
		return fail(CodeUnauthorized, "subagent %q belongs to another parent session", id)
	}
	if mode != session.SubagentContinuable {
		return fail(CodeNotResumable, "subagent %q has no supported continuation state and cannot be resumed; choose a different target", id)
	}
	return nil
}

// resume reopens a non-resident child named in the sender's own catalog and
// makes it resident; the caller's next attempt delivers the message. A
// closing placeholder holds the child's pool slot and makes concurrent
// deliveries wait while the transcript reopens.
func (service *Service) resume(ctx context.Context, sender *agent.Agent, senderID, targetID string) (bool, error) {
	events, err := sender.Events(ctx)
	if err != nil {
		return false, err
	}
	children := session.Children(events)
	index := slices.IndexFunc(children, func(entry session.SubagentCatalog) bool { return entry.SessionID == targetID })
	if index < 0 {
		return false, fail(CodeNotResumable, "subagent %q is unavailable", targetID)
	}
	if err := authorizeChild(targetID, senderID, children[index].Mode, senderID); err != nil {
		return false, err
	}
	beforeResume()
	service.mu.Lock()
	// The service may have stopped, or another delivery made the child
	// resident, while the catalog was read; the next attempt decides again.
	if !service.active || service.children[targetID] != nil {
		service.mu.Unlock()
		return true, nil
	}
	pool, err := service.admitLocked(senderID)
	if err != nil {
		service.mu.Unlock()
		return false, err
	}
	placeholder := &child{id: targetID, parent: senderID, mode: session.SubagentContinuable, pool: pool, closing: true, done: make(chan struct{}), wake: make(chan struct{})}
	service.children[targetID] = placeholder
	service.mu.Unlock()
	resumed, err := service.registry.Create(ctx, agent.CreateRequest{SessionID: targetID})
	if err != nil {
		service.dropPlaceholder(placeholder)
		return false, &Error{Code: CodeNotResumable, Message: fmt.Sprintf("subagent %q is unavailable", targetID), Err: err}
	}
	release := context.WithoutCancel(ctx)
	status := resumed.Status()
	beforePublish(resumed)
	// This residency reports only events after the resume; facts the reopen
	// repaired for the previous residency fall before the boundary.
	resumedEvents, err := resumed.Events(ctx)
	if err == nil {
		err = authorizeChild(targetID, status.ParentID, status.Mode, senderID)
	}
	if err == nil {
		err = service.replace(placeholder, &child{id: targetID, parent: senderID, label: status.Label, mode: status.Mode, depth: status.Depth, pool: pool, agent: resumed, boundary: len(resumedEvents), done: make(chan struct{}), wake: make(chan struct{})})
	} else {
		service.dropPlaceholder(placeholder)
	}
	if err != nil {
		return false, errors.Join(err, teardownFailure(targetID, closeAgent(service.registry, release, targetID)))
	}
	return true, nil
}

// dropPlaceholder removes a failed resume's placeholder and releases the
// deliveries waiting on it.
func (service *Service) dropPlaceholder(placeholder *child) {
	service.mu.Lock()
	defer service.mu.Unlock()
	delete(service.children, placeholder.id)
	close(placeholder.done)
}

// replace swaps a resume placeholder for the resumed handle and releases the
// deliveries waiting on it. The handle is refused when the service stopped
// or the parent began closing meanwhile, since either would otherwise miss
// it.
func (service *Service) replace(placeholder, current *child) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	delete(service.children, placeholder.id)
	close(placeholder.done)
	if !service.active {
		return ErrNotRunning
	}
	if parent := service.children[current.parent]; parent != nil && parent.closing {
		return fail(CodeUnauthorized, "subagent parent %q is being released; the child was not established", current.parent)
	}
	service.children[current.id] = current
	return nil
}

// Interrupt cancels the current turn of a live descendant of the caller
// without waiting. A target that is not live is an accepted no-op.
// Interrupting does not stop the target's own children.
func (service *Service) Interrupt(callerID, targetID string) error {
	if _, err := service.registry.Find(callerID); err != nil {
		return fail(CodeUnauthorized, "interrupting %q requires the exact live ancestor agent", targetID)
	}
	if callerID == targetID {
		return fail(CodeUnauthorized, "agent %q cannot interrupt itself", callerID)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.active {
		return ErrNotRunning
	}
	target := service.children[targetID]
	if target == nil {
		return nil
	}
	for ancestor := target.parent; ancestor != callerID; {
		parent := service.children[ancestor]
		if parent == nil {
			return fail(CodeUnauthorized, "subagent %q is not a live descendant of agent %q", targetID, callerID)
		}
		ancestor = parent.parent
	}
	if !target.closing {
		target.agent.Interrupt()
	}
	return nil
}

// ListChildren returns the caller's direct children from its own catalog,
// in the order their catalog records committed, with whether each is
// working now. Delegations that run concurrently in one step commit in the
// order they finish creating their children.
func (service *Service) ListChildren(ctx context.Context, parentID string) ([]Entry, error) {
	if !service.running() {
		return nil, ErrNotRunning
	}
	children, err := service.catalog(ctx, parentID)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, len(children))
	for index, current := range children {
		entries[index] = service.entry(current, parentID, 1)
	}
	return entries, nil
}

// ListDescendants walks the catalogs below the caller depth-first in
// creation order, visiting each session once. A child whose catalog cannot
// be read is reported unavailable and its subtree is skipped.
func (service *Service) ListDescendants(ctx context.Context, rootID string) ([]Entry, error) {
	if !service.running() {
		return nil, ErrNotRunning
	}
	type position struct {
		entry  session.SubagentCatalog
		parent string
		depth  int
	}
	roots, err := service.catalog(ctx, rootID)
	if err != nil {
		return nil, err
	}
	stack := make([]position, 0, len(roots))
	for _, root := range slices.Backward(roots) {
		stack = append(stack, position{entry: root, parent: rootID, depth: 1})
	}
	visited := map[string]bool{rootID: true}
	var entries []Entry
	for len(stack) > 0 {
		next := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[next.entry.SessionID] {
			continue
		}
		visited[next.entry.SessionID] = true
		entry := service.entry(next.entry, next.parent, next.depth)
		children, err := service.catalog(ctx, next.entry.SessionID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			entry.Unavailable = true
		}
		entries = append(entries, entry)
		for _, grandchild := range slices.Backward(children) {
			stack = append(stack, position{entry: grandchild, parent: next.entry.SessionID, depth: next.depth + 1})
		}
	}
	return entries, nil
}

// catalog reads one session's own children from its live journal, or from
// its transcript when it is not live.
func (service *Service) catalog(ctx context.Context, sessionID string) ([]session.SubagentCatalog, error) {
	var events []session.Event
	if live, err := service.registry.Find(sessionID); err == nil {
		events, err = live.Events(ctx)
		if err != nil {
			return nil, err
		}
	} else if _, events, err = service.repository.Inspect(ctx, sessionID); err != nil {
		return nil, err
	}
	return session.Children(events), nil
}

func (service *Service) entry(current session.SubagentCatalog, parent string, depth int) Entry {
	entry := Entry{ID: current.SessionID, Parent: parent, Label: current.Label, Mode: current.Mode, Depth: depth}
	if live, err := service.registry.Find(current.SessionID); err == nil {
		status := live.Status()
		entry.Running = status.Busy || status.Pending > 0
	}
	return entry
}

// List returns lexical snapshots of live children, optionally restricted to
// one parent, for presentation.
func (service *Service) List(parentID string) ([]Info, error) {
	service.mu.Lock()
	if !service.active {
		service.mu.Unlock()
		return nil, ErrNotRunning
	}
	agents := make([]*agent.Agent, 0, len(service.children))
	for _, current := range service.children {
		if current.agent != nil && (parentID == "" || current.parent == parentID) {
			agents = append(agents, current.agent)
		}
	}
	service.mu.Unlock()
	infos := make([]Info, len(agents))
	for index, live := range agents {
		status := live.Status()
		infos[index] = Info{SessionID: status.SessionID, ParentID: status.ParentID, Label: status.Label, Mode: status.Mode, Depth: status.Depth, Busy: status.Busy, Pending: status.Pending, Last: status.Last}
	}
	slices.SortFunc(infos, func(left, right Info) int { return strings.Compare(left.SessionID, right.SessionID) })
	return infos, nil
}
