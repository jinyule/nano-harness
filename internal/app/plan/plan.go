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

// state is the process-local part of one session's plan mode.
type state struct {
	// active is the recorded mode as of the latest boundary or commit.
	active bool
	// pending holds a selection awaiting the next step boundary.
	pending bool
	target  bool
	// notify asks the next boundary to tell the model about a user switch.
	notify bool
}

// Service owns pending selections and the boundary commit.
type Service struct {
	mu       sync.Mutex
	started  bool
	running  bool
	sessions map[string]*state
}

// New constructs an inactive service.
func New() *Service { return &Service{sessions: map[string]*state{}} }

// ID returns the stable plugin identity.
func (*Service) ID() string { return "plan-mode" }

// Start activates selections until scope cleanup discards every pending one.
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
		service.mu.Unlock()
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
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.running {
		return "", ErrNotRunning
	}
	events, err := journal.Events(ctx)
	if err != nil {
		return "", err
	}
	id := journal.Header().SessionID
	current := service.sessions[id]
	if current == nil {
		current = &state{}
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
	service.keep(id, current)
	return change, nil
}

// Step runs at each step boundary of turn, before step/start. It commits a
// pending selection, appends a user-switch notice when the model's last
// request described the other mode, and returns the plan policy section in
// force for the step's request, or "" outside plan mode. A failed append
// returns its error and keeps the selection pending for a later boundary.
func (service *Service) Step(ctx context.Context, journal Journal, turn uint64) (string, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.running {
		return "", ErrNotRunning
	}
	events, err := journal.Events(ctx)
	if err != nil {
		return "", err
	}
	view := session.ProjectPlan(events)
	id := journal.Header().SessionID
	current := service.sessions[id]
	if current == nil {
		current = &state{}
	}
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
			service.keep(id, current)
			return "", err
		}
	}
	current.notify = false
	service.keep(id, current)
	if current.active {
		return Section, nil
	}
	return "", nil
}

// Active reports whether the session's latest step boundary or commit left
// it in plan mode. Tools call it while their step runs.
func (service *Service) Active(sessionID string) bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	current := service.sessions[sessionID]
	return service.running && current != nil && current.active
}

// Exit records an approved plan review: the session leaves plan mode at the
// next step boundary, and no user-switch notice is added because the tool
// result already tells the model.
func (service *Service) Exit(sessionID string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.running {
		return ErrNotRunning
	}
	current := service.sessions[sessionID]
	if current == nil || !current.active {
		return ErrInactive
	}
	current.pending, current.target, current.notify = true, false, false
	return nil
}

// keep retains only sessions with process-local state worth remembering.
func (service *Service) keep(id string, current *state) {
	if current.active || current.pending || current.notify {
		service.sessions[id] = current
		return
	}
	delete(service.sessions, id)
}
