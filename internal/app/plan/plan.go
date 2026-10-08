// Package plan owns plan mode: per-session collaboration state that adds
// the Base plan policy to every model request until the user leaves plan
// mode or approves a plan submitted through exit_plan_mode.
//
// The durable state is the last plan/mode record in the session log. A
// selection made while no turn is open is committed at once; one made during
// a turn stays pending in this process until the engine's next step
// boundary commits it, so the request that follows is the first to see the
// change. Plan mode is guidance only: it never filters tools or grants or
// withholds approval.
package plan

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// Section is the upstream Base composition's plan-mode guidance, rendered as
// the plan policy section of every request while plan mode is in force. It
// is the parsed YAML block value, including its final newline.
const Section = "You are in plan mode. Stay in plan mode until exit_plan_mode succeeds or the user switches the session mode. Imperative language to implement changes means plan the implementation, not execute it. A user's conversational agreement — including an answer confirming something you asked — approves nothing and does not end plan mode; fold the confirmed decision into the plan and submit it through exit_plan_mode.\n" +
	"\n" +
	"Explore first. Use non-mutating reads, searches, static analysis, and checks to ground the plan in the actual repository. Do not edit or write files, change configuration, run formatters or code generation that rewrites tracked files, commit, or otherwise carry out the plan. Prefer existing functions and patterns over new machinery.\n" +
	"\n" +
	"The tool catalog stays the same across modes for request-cache stability. These plan-mode rules override any later tool description or guidance that suggests using mutation tools; those tools remain listed only to keep the request shape stable. Do not use todo_write to track this planning phase: it tracks implementation after an approved plan, while the plan itself belongs in exit_plan_mode.\n" +
	"\n" +
	"Resolve discoverable facts by inspection. Use ask_user_question only for user-owned choices or material ambiguity that inspection cannot answer. Do not ask the user where code lives or how current behavior works when you can find out.\n" +
	"\n" +
	"Make the plan decision-complete: state the goal and success criteria; group implementation changes by subsystem; identify public API, schema, and data-flow changes; cover edge cases, failure modes, tests, acceptance criteria, and explicit assumptions. Keep it concise enough to review but detailed enough that another engineer can implement it without making design decisions.\n" +
	"\n" +
	"When ready, call exit_plan_mode with the complete plan markdown, starting with a # title. Make exit_plan_mode the only and final tool call in that assistant response: it presents the plan for approval, and implementation begins only in a later step after approval. Do not paste the final plan as a plain reply or ask \"should I proceed?\" through prose or ask_user_question. If review rejects it, incorporate the feedback and present again. If the review channel is unavailable or aborted, stay in plan mode and ask the user to switch modes manually; do not proceed with implementation.\n"

const (
	// NoticeSource is the message source kind of a user-switch notice.
	NoticeSource = "plan-mode"
	// enteredNotice and leftNotice tell the model about a user switch when
	// its last request described the other mode.
	enteredNotice = "The user switched this session to plan mode."
	leftNotice    = "The user switched this session back to the default mode."
)

var (
	// ErrNotRunning indicates the service has not started or has stopped.
	ErrNotRunning = errors.New("plan mode service is not running")
	// ErrInvalidRequest identifies a lifecycle or call that cannot be honored.
	ErrInvalidRequest = errors.New("invalid plan mode request")
	// ErrInactive reports an exit requested for a session not in plan mode.
	ErrInactive = errors.New("exit_plan_mode is only available in plan mode")
)

// Change reports what one user selection did.
type Change string

const (
	// Committed means the selection was recorded at once because no turn was open.
	Committed Change = "committed"
	// Queued means the selection applies from the next step boundary of the open turn.
	Queued Change = "queued"
	// Cancelled means an opposite pending selection was dropped; the recorded mode already matches.
	Cancelled Change = "cancelled"
	// Unchanged means the session already is, or is about to be, in the selected mode.
	Unchanged Change = "unchanged"
)

// Journal is the session log plan mode reads and commits to.
type Journal interface {
	Header() session.Header
	Events(context.Context) ([]session.Event, error)
	Append(context.Context, session.Record) (session.Event, error)
}

// state is the process-local part of one session's plan mode. Its mutex
// serializes that session's selections, boundaries, and exits, including
// their journal reads and appends, so sessions never wait for each other.
type state struct {
	mu sync.Mutex
	// active is the recorded mode as of the latest boundary or commit.
	active bool
	// pending holds a selection awaiting the next step boundary.
	pending bool
	target  bool
	// notify asks the next boundary to tell the model about a user switch.
	notify bool
}

// Service owns pending selections and the boundary commit. Its mutex
// guards only the lifecycle, the session map, and the calls in flight; an
// entry, once created, lives until the service stops, so a session's lock
// is never replaced while a caller holds it.
type Service struct {
	mu       sync.Mutex
	started  bool
	running  bool
	sessions map[string]*state
	// calls cancels and joins each Select, Step, or Exit in flight.
	calls *plugin.Calls
}

// New constructs an inactive service.
func New() *Service {
	service := &Service{sessions: map[string]*state{}}
	service.calls = plugin.NewCalls(&service.mu)
	return service
}

// ID returns the stable plugin identity.
func (*Service) ID() string { return "plan-mode" }

// Start activates selections until scope cleanup. Cleanup rejects new
// calls, cancels every call in flight, and returns only after each has
// returned, so nothing is appended once it returns; it also discards every
// pending selection.
func (service *Service) Start(_ context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.started {
		return ErrInvalidRequest
	}
	if err := scope.Defer(func(context.Context) error {
		service.mu.Lock()
		service.running = false
		service.sessions = map[string]*state{}
		service.calls.Cancel(nil)
		service.mu.Unlock()
		// A call holding a session's lock may be inside a journal read or
		// append; the wait is bounded by the journal's cancellation latency.
		service.calls.Wait()
		return nil
	}); err != nil {
		return err
	}
	service.started, service.running = true, true
	return nil
}

// Select records a user's choice to enter or leave plan mode. inTurn
// reports whether the session has an open turn; the caller must hold it
// stable for the duration of the call, so an immediate commit can never
// interleave with a turn starting. The commit is the only effect: a failed
// append returns its error and leaves the session unchanged.
func (service *Service) Select(ctx context.Context, journal Journal, active, inTurn bool) (Change, error) {
	ctx, current, done, err := service.begin(ctx, journal.Header().SessionID, true)
	if err != nil {
		return "", err
	}
	defer done()
	current.mu.Lock()
	defer current.mu.Unlock()
	events, err := journal.Events(ctx)
	if err != nil {
		return "", err
	}
	current.active = session.ProjectPlan(events).Active
	target := current.active
	if current.pending {
		target = current.target
	}
	if active == target {
		return Unchanged, nil
	}
	change := Queued
	switch {
	case active == current.active:
		current.pending = false
		change = Cancelled
	case inTurn:
		current.pending, current.target, current.notify = true, active, true
	default:
		if _, err := journal.Append(ctx, session.Record{Type: session.RecordPlanMode, Plan: &session.PlanMode{Active: active}}); err != nil {
			return "", err
		}
		current.active, current.pending, current.notify = active, false, true
		change = Committed
	}
	return change, nil
}

// Step runs at each step boundary of turn, before step/start. It commits a
// pending selection, appends a user-switch notice when the model's last
// request described the other mode, and returns the plan policy section in
// force for the step's request, or "" outside plan mode. A failed append
// returns its error and keeps the selection pending for a later boundary.
func (service *Service) Step(ctx context.Context, journal Journal, turn uint64) (string, error) {
	ctx, current, done, err := service.begin(ctx, journal.Header().SessionID, true)
	if err != nil {
		return "", err
	}
	defer done()
	current.mu.Lock()
	defer current.mu.Unlock()
	events, err := journal.Events(ctx)
	if err != nil {
		return "", err
	}
	view := session.ProjectPlan(events)
	current.active = view.Active
	if current.pending && current.target != current.active {
		if _, err := journal.Append(ctx, session.Record{Type: session.RecordPlanMode, Turn: turn, Plan: &session.PlanMode{Active: current.target}}); err != nil {
			return "", err
		}
		current.active = current.target
	}
	current.pending = false
	if current.notify && view.Requested && view.Told != current.active {
		text := leftNotice
		if current.active {
			text = enteredNotice
		}
		notice := &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: NoticeSource}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}
		if _, err := journal.Append(ctx, session.Record{Type: session.RecordUserMessage, Turn: turn, Message: notice}); err != nil {
			return "", err
		}
	}
	current.notify = false
	if current.active {
		return Section, nil
	}
	return "", nil
}

// Active reports whether the session's latest step boundary or commit left
// it in plan mode. Tools call it while their step runs.
func (service *Service) Active(sessionID string) bool {
	service.mu.Lock()
	current := service.sessions[sessionID]
	service.mu.Unlock()
	if current == nil {
		return false
	}
	current.mu.Lock()
	defer current.mu.Unlock()
	return current.active
}

// Exit records an approved plan review: the session leaves plan mode at the
// next step boundary, and no user-switch notice is added because the tool
// result already tells the model. The context is checked under the
// session's lock, so cancellation before the selection rejects it without
// changing pending state; cancellation after it does not undo it.
func (service *Service) Exit(ctx context.Context, sessionID string) error {
	ctx, current, done, err := service.begin(ctx, sessionID, false)
	if err != nil {
		return err
	}
	defer done()
	current.mu.Lock()
	defer current.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("exit plan mode: %w", err)
	}
	if !current.active {
		return ErrInactive
	}
	current.pending, current.target, current.notify = true, false, false
	return nil
}

// begin admits one call for session id while the service runs, creating
// the session's state when create is set, and returns the call's context,
// which cleanup cancels. It reports ErrNotRunning once cleanup has begun
// and ErrInactive, without create, for a session that never reached a
// selection or boundary. done must run once the call stops touching the
// state and the journal.
func (service *Service) begin(ctx context.Context, id string, create bool) (context.Context, *state, func(), error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.running {
		return nil, nil, nil, ErrNotRunning
	}
	current := service.sessions[id]
	if current == nil {
		if !create {
			return nil, nil, nil, ErrInactive
		}
		current = &state{}
		service.sessions[id] = current
	}
	call, done := service.calls.Admit(ctx)
	return call, current, done, nil
}
