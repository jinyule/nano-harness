// Package goal owns long-running session goals: one durable completion
// objective per session that people and models create and update, and that
// the round driver continues through automatic goal rounds.
//
// The session log is the only durable authority. Every mutation appends a
// goal/change record with the full post-mutation snapshot, and each admitted
// round is a goal-sourced user/message; session.GoalState folds both. Whether
// this process may continue an active goal automatically (armed) is
// process-local: creation and resume arm a goal, pause, completion,
// blocking, and clear disarm it, and every goal starts disarmed after the
// session is opened again.
package goal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	// DefaultMaxRounds is the round cap of a goal created without one.
	DefaultMaxRounds = 256
	// HumanSource is the message source kind frontends give a person's typed
	// input. Every other producer uses its own kind, so it alone proves that
	// a person spoke in a turn.
	HumanSource = "user"
)

// ErrNotRunning indicates the service has not started or has stopped.
var ErrNotRunning = errors.New("goal service is not running")

// Code is the stable classification of a rejected goal operation.
type Code string

// Rejection codes mirror the upstream goal domain.
const (
	CodeAgentNotLive       Code = "GOAL_AGENT_NOT_LIVE"
	CodeNotFound           Code = "GOAL_NOT_FOUND"
	CodeAlreadyExists      Code = "GOAL_ALREADY_EXISTS"
	CodeStaleRevision      Code = "GOAL_STALE_REVISION"
	CodeInvalidObjective   Code = "GOAL_INVALID_OBJECTIVE"
	CodeInvalidMaxRounds   Code = "GOAL_INVALID_MAX_ROUNDS"
	CodeInvalidBlockReason Code = "GOAL_INVALID_BLOCK_REASON"
	CodeInvalidEdit        Code = "GOAL_INVALID_EDIT"
	CodeInvalidTransition  Code = "GOAL_INVALID_TRANSITION"
)

// Error is an expected rejection. Message is the upstream model-visible text.
type Error struct {
	Code    Code
	Message string
}

func (err *Error) Error() string { return err.Message }

func reject(code Code, format string, values ...any) error {
	return &Error{Code: code, Message: fmt.Sprintf(format, values...)}
}

// Actor names who requested a mutation. A host pause also interrupts the
// running turn; a model or driver mutation never does.
type Actor string

const (
	// ActorHost is a person acting through a frontend command.
	ActorHost Actor = "host"
	// ActorModel is a model acting through a goal tool inside its own turn.
	ActorModel Actor = "model"
	// ActorDriver is the round driver.
	ActorDriver Actor = "driver"
)

// View is a detached current goal with its process-local activation.
type View struct {
	Goal            session.GoalSnapshot
	RoundsStarted   uint64
	CreatedAtUnixMS int64
	UpdatedAtUnixMS int64
	Armed           bool
}

// Change notifies a watcher after one mutation committed. View is nil
// after a clear.
type Change struct {
	SessionID string
	Operation session.GoalOperation
	Actor     Actor
	View      *View
}

// Journals resolves the durable log of a live agent.
type Journals interface {
	Journal(sessionID string) (agent.Journal, error)
}

// Admissions publishes the round admission that opens goal-sourced turns.
type Admissions interface {
	RegisterAdmission(kind string, admission agent.Admission, scope *plugin.Scope) error
}

// Config holds deployment defaults and deterministic test seams.
type Config struct {
	// DefaultMaxRounds caps goals created without a cap; zero selects DefaultMaxRounds.
	DefaultMaxRounds uint64
	// Now and Random default to the wall clock and crypto/rand.
	Now    func() time.Time
	Random io.Reader
}

type watcher struct {
	sessionID string
	notify    func(Change)
}

// Service serializes goal mutations and round admission per process.
type Service struct {
	journals   Journals
	admissions Admissions
	maxRounds  uint64
	now        func() time.Time
	random     io.Reader

	// mu serializes every read-validate-append of goal facts, including the
	// commit of an admitted round, so no mutation interleaves with one.
	mu      sync.Mutex
	started bool
	running bool
	armed   map[string]bool

	// watchMu is held for reading while watchers run, so a withdrawn
	// watcher is never called after its cleanup returns.
	watchMu  sync.RWMutex
	watchers map[*watcher]struct{}
}

// New constructs an inactive service.
func New(journals Journals, admissions Admissions, config Config) (*Service, error) {
	if journals == nil || admissions == nil || config.DefaultMaxRounds > session.MaxGoalRounds {
		return nil, errors.New("invalid goal service configuration")
	}
	if config.DefaultMaxRounds == 0 {
		config.DefaultMaxRounds = DefaultMaxRounds
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	return &Service{journals: journals, admissions: admissions, maxRounds: config.DefaultMaxRounds, now: config.Now, random: config.Random, armed: map[string]bool{}, watchers: map[*watcher]struct{}{}}, nil
}

// ID returns the stable plugin identity.
func (*Service) ID() string { return "goals" }

// Start activates operations and publishes round admission until scope
// cleanup, which rejects later operations and forgets every activation.
func (service *Service) Start(_ context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	if service.started {
		service.mu.Unlock()
		return errors.New("goal service already started")
	}
	service.started = true
	service.mu.Unlock()
	if err := scope.Defer(func(context.Context) error {
		service.mu.Lock()
		service.running = false
		service.armed = map[string]bool{}
		service.mu.Unlock()
		return nil
	}); err != nil {
		return err
	}
	service.mu.Lock()
	service.running = true
	service.mu.Unlock()
	return service.admissions.RegisterAdmission(session.GoalSource, service, scope)
}

// Watch calls notify after every committed mutation of sessionID until
// scope cleanup. notify runs on the mutating goroutine after the commit and
// must not block or call Watch.
func (service *Service) Watch(sessionID string, notify func(Change), scope *plugin.Scope) error {
	if sessionID == "" || notify == nil || scope == nil {
		return errors.New("invalid goal watcher")
	}
	entry := &watcher{sessionID: sessionID, notify: notify}
	service.watchMu.Lock()
	service.watchers[entry] = struct{}{}
	service.watchMu.Unlock()
	unwatch := func(context.Context) error {
		service.watchMu.Lock()
		delete(service.watchers, entry)
		service.watchMu.Unlock()
		return nil
	}
	if err := scope.Defer(unwatch); err != nil {
		_ = unwatch(context.Background())
		return err
	}
	return nil
}

// Get returns the current goal of a live agent's session, or nil.
func (service *Service) Get(ctx context.Context, sessionID string) (*View, error) {
	journal, err := service.journal(sessionID)
	if err != nil {
		return nil, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.running {
		return nil, ErrNotRunning
	}
	state, err := project(ctx, journal)
	if err != nil {
		return nil, err
	}
	return service.view(sessionID, state), nil
}

// Authority is what the committed messages of one turn prove about who
// asked for a goal operation.
type Authority struct {
	// Human reports a person's message in a root agent's turn.
	Human bool
	// Round is the current goal when the turn is its exact current round.
	Round *View
}

// Authority inspects the user messages of turn in a live agent's session.
// A delegated agent never has human authority, and only the current
// round of the current goal revision counts as a goal round.
func (service *Service) Authority(ctx context.Context, sessionID string, turn uint64, delegated bool) (Authority, error) {
	journal, err := service.journal(sessionID)
	if err != nil {
		return Authority{}, err
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.running {
		return Authority{}, ErrNotRunning
	}
	events, err := ownEvents(ctx, journal)
	if err != nil {
		return Authority{}, err
	}
	state, err := session.ProjectGoal(events)
	if err != nil {
		return Authority{}, err
	}
	var authority Authority
	for _, event := range events {
		record := event.Record
		if record.Type != session.RecordUserMessage || record.Turn != turn {
			continue
		}
		source := record.Message.Source
		authority.Human = authority.Human || !delegated && source.Kind == HumanSource
		if current := state.Goal; source.Kind == session.GoalSource && current != nil && source.GoalID == current.ID && source.GoalRevision == current.Revision && source.GoalRound == state.RoundsStarted {
			authority.Round = service.view(sessionID, state)
		}
	}
	return authority, nil
}

// Disarm removes automatic continuation without a durable change. The
// round driver calls it when it takes over a session and when it stops.
func (service *Service) Disarm(sessionID string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	delete(service.armed, sessionID)
}

// Create starts and arms a goal. A completed goal may be replaced; any
// other current goal must be edited, resumed, or cleared instead.
// maxRounds is the caller's JSON number, nil for the deployment default.
func (service *Service) Create(ctx context.Context, sessionID, objective string, maxRounds *float64, actor Actor) (*View, error) {
	objective, err := checkObjective(objective)
	if err != nil {
		return nil, err
	}
	rounds := service.maxRounds
	if maxRounds != nil {
		if rounds, err = RoundCap(*maxRounds); err != nil {
			return nil, err
		}
	}
	var random [16]byte
	if _, err := io.ReadFull(service.random, random[:]); err != nil {
		return nil, fmt.Errorf("generate goal ID: %w", err)
	}
	return service.mutate(ctx, sessionID, actor, func(state session.GoalState, _ bool) (session.GoalChange, bool, error) {
		if current := state.Goal; current != nil && current.Phase != session.GoalComplete {
			return session.GoalChange{}, false, reject(CodeAlreadyExists, "goal %q already exists with phase %q", current.ID, current.Phase)
		}
		now := max(service.now().UnixMilli(), 1)
		snapshot := &session.GoalSnapshot{ID: "goal-" + hex.EncodeToString(random[:]), Revision: 1, Objective: objective, Phase: session.GoalActive, MaxRounds: rounds}
		return session.GoalChange{Operation: session.GoalOpCreate, Snapshot: snapshot, CreatedAtUnixMS: now, UpdatedAtUnixMS: now}, true, nil
	})
}

// Edit replaces the objective and/or round cap of the current revision
// without changing its phase or activation.
func (service *Service) Edit(ctx context.Context, sessionID string, ref session.GoalRef, objective *string, maxRounds *float64, actor Actor) (*View, error) {
	return service.mutate(ctx, sessionID, actor, func(state session.GoalState, armed bool) (session.GoalChange, bool, error) {
		current, err := expectCurrent(state, ref)
		if err != nil {
			return session.GoalChange{}, false, err
		}
		if objective == nil && maxRounds == nil {
			return session.GoalChange{}, false, reject(CodeInvalidEdit, "goal edit requires objective and/or maxGoalRounds")
		}
		next := advance(current)
		next.Phase, next.BlockedReason = current.Phase, current.BlockedReason
		if objective != nil {
			if next.Objective, err = checkObjective(*objective); err != nil {
				return session.GoalChange{}, false, err
			}
		}
		if maxRounds != nil {
			if next.MaxRounds, err = RoundCap(*maxRounds); err != nil {
				return session.GoalChange{}, false, err
			}
		}
		return service.successor(state, session.GoalOpEdit, next), armed, nil
	})
}

// Pause stops an active goal and disarms it.
func (service *Service) Pause(ctx context.Context, sessionID string, ref session.GoalRef, actor Actor) (*View, error) {
	return service.transition(ctx, sessionID, ref, actor, session.GoalOpPause, session.GoalPaused, session.GoalActive)
}

// Complete marks an unfinished goal complete and disarms it.
func (service *Service) Complete(ctx context.Context, sessionID string, ref session.GoalRef, actor Actor) (*View, error) {
	return service.transition(ctx, sessionID, ref, actor, session.GoalOpComplete, session.GoalComplete, session.GoalActive, session.GoalPaused, session.GoalBlocked)
}

// Resume arms a paused or blocked goal, or rearms an active goal that is
// disarmed, while its round cap has capacity. It clears a blocker.
func (service *Service) Resume(ctx context.Context, sessionID string, ref session.GoalRef, actor Actor) (*View, error) {
	return service.mutate(ctx, sessionID, actor, func(state session.GoalState, armed bool) (session.GoalChange, bool, error) {
		current, err := expectCurrent(state, ref)
		if err != nil {
			return session.GoalChange{}, false, err
		}
		if current.Phase == session.GoalComplete {
			return session.GoalChange{}, false, transitionError(*current, session.GoalOpResume, session.GoalActive, session.GoalPaused, session.GoalBlocked)
		}
		if current.Phase == session.GoalActive && armed {
			return session.GoalChange{}, false, reject(CodeInvalidTransition, "goal %q is already active and armed", current.ID)
		}
		if state.RoundsStarted >= current.MaxRounds {
			return session.GoalChange{}, false, reject(CodeInvalidTransition, "goal %q exhausted %d goal rounds; increase maxGoalRounds before resuming", current.ID, current.MaxRounds)
		}
		next := advance(current)
		next.Phase = session.GoalActive
		return service.successor(state, session.GoalOpResume, next), true, nil
	})
}

// Block stops an active goal with a policy code and explanation and disarms it.
func (service *Service) Block(ctx context.Context, sessionID string, ref session.GoalRef, reason session.GoalBlockReason, actor Actor) (*View, error) {
	reason.Message = strings.TrimSpace(reason.Message)
	return service.mutate(ctx, sessionID, actor, func(state session.GoalState, _ bool) (session.GoalChange, bool, error) {
		current, err := expectCurrent(state, ref)
		if err != nil {
			return session.GoalChange{}, false, err
		}
		if current.Phase != session.GoalActive {
			return session.GoalChange{}, false, transitionError(*current, session.GoalOpBlock, session.GoalActive)
		}
		if !session.ValidGoalCode(reason.Code) || reason.Message == "" || len(reason.Message) > session.MaxGoalTextBytes {
			return session.GoalChange{}, false, reject(CodeInvalidBlockReason, "goal block reason requires a lower-kebab-case code and a non-empty message")
		}
		next := advance(current)
		next.Phase, next.BlockedReason = session.GoalBlocked, &reason
		return service.successor(state, session.GoalOpBlock, next), false, nil
	})
}

// Clear removes the current goal, leaving a tombstone and its history.
func (service *Service) Clear(ctx context.Context, sessionID string, ref session.GoalRef, actor Actor) error {
	_, err := service.mutate(ctx, sessionID, actor, func(state session.GoalState, _ bool) (session.GoalChange, bool, error) {
		current, err := expectCurrent(state, ref)
		if err != nil {
			return session.GoalChange{}, false, err
		}
		cleared := &session.GoalRef{ID: current.ID, Revision: current.Revision + 1}
		return session.GoalChange{Operation: session.GoalOpClear, Cleared: cleared, ClearedAtUnixMS: service.timestamp(state)}, false, nil
	})
	return err
}

func (service *Service) transition(ctx context.Context, sessionID string, ref session.GoalRef, actor Actor, operation session.GoalOperation, phase session.GoalPhase, allowed ...session.GoalPhase) (*View, error) {
	return service.mutate(ctx, sessionID, actor, func(state session.GoalState, _ bool) (session.GoalChange, bool, error) {
		current, err := expectCurrent(state, ref)
		if err != nil {
			return session.GoalChange{}, false, err
		}
		permitted := false
		for _, candidate := range allowed {
			permitted = permitted || current.Phase == candidate
		}
		if !permitted {
			return session.GoalChange{}, false, transitionError(*current, operation, allowed...)
		}
		next := advance(current)
		next.Phase = phase
		return service.successor(state, operation, next), false, nil
	})
}

// mutate commits one change built from the current state under the
// service lock, records the resulting activation, and notifies watchers.
func (service *Service) mutate(ctx context.Context, sessionID string, actor Actor, build func(session.GoalState, bool) (session.GoalChange, bool, error)) (*View, error) {
	journal, err := service.journal(sessionID)
	if err != nil {
		return nil, err
	}
	service.mu.Lock()
	if !service.running {
		service.mu.Unlock()
		return nil, ErrNotRunning
	}
	state, err := project(ctx, journal)
	if err != nil {
		service.mu.Unlock()
		return nil, err
	}
	change, armed, err := build(state, service.armed[sessionID])
	if err == nil {
		record := session.Record{Type: session.RecordGoalChange, Goal: &change}
		if _, err = journal.Append(ctx, record); err == nil {
			err = state.Apply(record)
		}
	}
	if err != nil {
		service.mu.Unlock()
		return nil, err
	}
	if armed {
		service.armed[sessionID] = true
	} else {
		delete(service.armed, sessionID)
	}
	view := service.view(sessionID, state)
	service.mu.Unlock()
	service.notify(Change{SessionID: sessionID, Operation: change.Operation, Actor: actor, View: view})
	return view, nil
}

func (service *Service) notify(change Change) {
	service.watchMu.RLock()
	defer service.watchMu.RUnlock()
	for entry := range service.watchers {
		if entry.sessionID == change.SessionID {
			entry.notify(change)
		}
	}
}

// Admit opens a goal round only while it is still the next round of the
// current active, armed revision and no later stop revoked continuation.
// A stopped service admits nothing.
func (service *Service) Admit(ctx context.Context, journal agent.Journal, message session.Message, open func(context.Context) error) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.running {
		return agent.ErrNotAdmitted
	}
	events, err := ownEvents(ctx, journal)
	if err != nil {
		return err
	}
	state, err := session.ProjectGoal(events)
	if err != nil {
		return err
	}
	source, current := message.Source, state.Goal
	if current == nil || current.Phase != session.GoalActive || !service.armed[journal.Header().SessionID] || source.GoalID != current.ID || source.GoalRevision != current.Revision || source.GoalRound != state.RoundsStarted+1 || source.GoalRound > current.MaxRounds || settle(events, 0).revoked() {
		return agent.ErrNotAdmitted
	}
	return open(ctx)
}

// Settle applies the turns committed after sequence after: a cancelled
// goal round pauses its own revision if it is still current, active, and
// armed, and any other cancelled turn or any failed turn disarms the goal,
// unless a later create or resume armed it again. It returns the last
// sequence it examined.
func (service *Service) Settle(ctx context.Context, sessionID string, after uint64) (uint64, error) {
	journal, err := service.journal(sessionID)
	if err != nil {
		return after, err
	}
	service.mu.Lock()
	if !service.running {
		service.mu.Unlock()
		return after, ErrNotRunning
	}
	events, err := ownEvents(ctx, journal)
	if err != nil {
		service.mu.Unlock()
		return after, err
	}
	last := after
	if len(events) > 0 {
		last = events[len(events)-1].Sequence
	}
	outcome := settle(events, after)
	state, err := session.ProjectGoal(events)
	service.mu.Unlock()
	if err != nil {
		return after, err
	}
	if current := state.Goal; outcome.pause != nil && current != nil && current.Ref() == *outcome.pause && current.Phase == session.GoalActive && service.isArmed(sessionID) {
		if _, err := service.Pause(ctx, sessionID, *outcome.pause, ActorDriver); err != nil {
			service.Disarm(sessionID)
			return last, err
		}
	}
	if outcome.disarm {
		service.Disarm(sessionID)
	}
	return last, nil
}

func (service *Service) isArmed(sessionID string) bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.armed[sessionID]
}

// settlement is what the turns after a sequence imply for continuation.
type settlement struct {
	// pause names the revision of a cancelled goal round.
	pause *session.GoalRef
	// disarm reports a cancelled non-round turn or a failed turn.
	disarm bool
}

func (outcome settlement) revoked() bool { return outcome.pause != nil || outcome.disarm }

// settle scans events after sequence after. A create or resume clears
// everything an earlier stop implied.
func settle(events []session.Event, after uint64) settlement {
	var outcome settlement
	var round *session.GoalRef
	opening := false
	for _, event := range events {
		if event.Sequence <= after {
			continue
		}
		record := event.Record
		switch {
		case record.Type == session.RecordTurnStart:
			round, opening = nil, true
		case record.Type == session.RecordUserMessage && opening:
			opening = false
			if source := record.Message.Source; source.Kind == session.GoalSource {
				round = &session.GoalRef{ID: source.GoalID, Revision: source.GoalRevision}
			}
		case record.Type == session.RecordGoalChange && (record.Goal.Operation == session.GoalOpCreate || record.Goal.Operation == session.GoalOpResume):
			outcome = settlement{}
		case record.Type == session.RecordTurnEnd && record.Outcome == session.OutcomeCanceled && round != nil:
			outcome.pause = round
		case record.Type == session.RecordTurnEnd && (record.Outcome == session.OutcomeCanceled || record.Outcome == session.OutcomeError):
			outcome.disarm = true
		}
	}
	return outcome
}

// RoundCap validates a caller's JSON number as a positive safe-integer round cap.
func RoundCap(value float64) (uint64, error) {
	if value < 1 || value > session.MaxGoalRounds || value != math.Trunc(value) {
		return 0, reject(CodeInvalidMaxRounds, "maxGoalRounds must be a positive safe integer")
	}
	return uint64(value), nil
}

func checkObjective(objective string) (string, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return "", reject(CodeInvalidObjective, "goal objective must be a non-empty string")
	}
	if len(objective) > session.MaxGoalTextBytes {
		return "", reject(CodeInvalidObjective, "goal objective exceeds %d bytes", session.MaxGoalTextBytes)
	}
	return objective, nil
}

func expectCurrent(state session.GoalState, ref session.GoalRef) (*session.GoalSnapshot, error) {
	current := state.Goal
	if current == nil {
		return nil, reject(CodeNotFound, "no current goal")
	}
	if ref != current.Ref() {
		return nil, reject(CodeStaleRevision, "stale goal ref %q revision %d; current is %q revision %d", ref.ID, ref.Revision, current.ID, current.Revision)
	}
	return current, nil
}

func transitionError(current session.GoalSnapshot, operation session.GoalOperation, allowed ...session.GoalPhase) error {
	names := make([]string, len(allowed))
	for index, phase := range allowed {
		names[index] = string(phase)
	}
	return reject(CodeInvalidTransition, "cannot %s goal %q from phase %q; expected %s", operation, current.ID, current.Phase, strings.Join(names, " or "))
}

// advance copies the definition of current into its next revision with no
// phase-specific fields.
func advance(current *session.GoalSnapshot) *session.GoalSnapshot {
	return &session.GoalSnapshot{ID: current.ID, Revision: current.Revision + 1, Objective: current.Objective, MaxRounds: current.MaxRounds}
}

// successor wraps a next revision with the current counters and a clamped timestamp.
func (service *Service) successor(state session.GoalState, operation session.GoalOperation, snapshot *session.GoalSnapshot) session.GoalChange {
	return session.GoalChange{Operation: operation, Snapshot: snapshot, RoundsStarted: state.RoundsStarted, CreatedAtUnixMS: state.CreatedAtUnixMS, UpdatedAtUnixMS: service.timestamp(state)}
}

// timestamp is the wall clock, clamped so a mutation never precedes the current goal's last update.
func (service *Service) timestamp(state session.GoalState) int64 {
	return max(service.now().UnixMilli(), state.UpdatedAtUnixMS)
}

func (service *Service) journal(sessionID string) (agent.Journal, error) {
	journal, err := service.journals.Journal(sessionID)
	if err != nil {
		return nil, &Error{Code: CodeAgentNotLive, Message: fmt.Sprintf("agent %q is not live in this registry", sessionID)}
	}
	return journal, nil
}

// ownEvents returns the events a session committed itself. A forked child
// inherits its parent's closed prefix, whose goal facts belong to the parent:
// the store validates them in place, but they never become the child's goal.
func ownEvents(ctx context.Context, journal agent.Journal) ([]session.Event, error) {
	events, err := journal.Events(ctx)
	if err != nil {
		return nil, err
	}
	return session.OwnEvents(events), nil
}

func project(ctx context.Context, journal agent.Journal) (session.GoalState, error) {
	events, err := ownEvents(ctx, journal)
	if err != nil {
		return session.GoalState{}, err
	}
	return session.ProjectGoal(events)
}

func (service *Service) view(sessionID string, state session.GoalState) *View {
	if state.Goal == nil {
		return nil
	}
	snapshot := *state.Goal
	if snapshot.BlockedReason != nil {
		reason := *snapshot.BlockedReason
		snapshot.BlockedReason = &reason
	}
	return &View{Goal: snapshot, RoundsStarted: state.RoundsStarted, CreatedAtUnixMS: state.CreatedAtUnixMS, UpdatedAtUnixMS: state.UpdatedAtUnixMS, Armed: service.armed[sessionID]}
}
