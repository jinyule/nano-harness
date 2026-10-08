package goal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// followup is one round the driver queued; the test plays the agent and
// sends the turn's result.
type followup struct {
	message session.Message
	result  chan agent.TurnResult
}

// fakeRoot is an always-idle root agent whose turns the test settles.
type fakeRoot struct {
	agent.Controller
	journal     *memoryJournal
	calls       chan followup
	followupErr error
	// blocked, when set, receives each Followup, which then waits for cancellation.
	blocked   chan struct{}
	idleErr   error
	eventsErr error
	idle      chan chan struct{}

	mu         sync.Mutex
	interrupts int
	pending    chan agent.TurnResult
	// interruptCancels makes Interrupt settle the pending round as cancelled.
	interruptCancels bool
}

func (root *fakeRoot) Status() agent.Status { return agent.Status{SessionID: root.journal.id} }
func (root *fakeRoot) Events(ctx context.Context) ([]session.Event, error) {
	if root.eventsErr != nil {
		return nil, root.eventsErr
	}
	return root.journal.Events(ctx)
}
func (root *fakeRoot) WhenIdle(ctx context.Context) error {
	if root.idle != nil {
		release := make(chan struct{})
		select {
		case root.idle <- release:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	if root.idleErr != nil {
		return root.idleErr
	}
	return ctx.Err()
}

func TestDriver_FailedOpeningDoesNotRequeueTheRound(t *testing.T) {
	test := startDriver(t, func(test *driverFixture) { test.root.idle = make(chan chan struct{}, 1) })
	view := test.create(t, "ship", nil)
	close(<-test.root.idle)
	call := test.next(t)
	call.result <- agent.TurnResult{Outcome: session.OutcomeError, Err: errors.New("disk full")}
	release := <-test.root.idle // The result has been processed; let the next scheduling pass run.
	close(release)
	select {
	case repeated := <-test.root.calls:
		t.Fatalf("failed opening requeued round %+v as %+v", call.message.Source, repeated.message.Source)
	case <-test.root.idle:
		// The create wake was consumed without queueing another round.
	case <-time.After(5 * time.Second):
		t.Fatal("driver never finished the scheduling pass")
	}
	current, err := test.service.Get(t.Context(), "root")
	if err != nil || current.Armed || current.Goal.Ref() != view.Goal.Ref() || current.RoundsStarted != 0 {
		t.Fatalf("failed opening retains continuation: %+v, %v", current, err)
	}
	test.stop(t)
	if len(test.root.calls) != 0 {
		t.Fatal("failed opening requeued the same round")
	}
}

func TestDriver_FailedOldRoundPreservesNewAuthorization(t *testing.T) {
	test := startDriver(t, func(test *driverFixture) {
		test.root.idle = make(chan chan struct{}, 1)
		test.root.cancelOnInterrupt(false)
	})
	view := test.create(t, "old", nil)
	close(<-test.root.idle)
	call := test.next(t)
	if err := test.service.Clear(t.Context(), "root", view.Goal.Ref(), ActorHost); err != nil {
		t.Fatal(err)
	}
	authorized := test.create(t, "new", nil)
	call.result <- agent.TurnResult{Outcome: session.OutcomeError, Err: errors.New("disk full")}
	release := <-test.root.idle
	current, err := test.service.Get(t.Context(), "root")
	if err != nil || !current.Armed || current.Goal.Ref() != authorized.Goal.Ref() {
		t.Fatalf("failed old round revoked authorization: %+v, %v", current, err)
	}
	close(release)
	next := test.next(t)
	if next.message.Source.GoalID != authorized.Goal.ID || next.message.Source.GoalRound != 1 {
		t.Fatalf("new round = %+v", next.message.Source)
	}
	test.play(t, next, session.OutcomeCompleted, func() {
		if _, err := test.service.Complete(t.Context(), "root", authorized.Goal.Ref(), ActorModel); err != nil {
			t.Error(err)
		}
	})
}

func (root *fakeRoot) Followup(ctx context.Context, message session.Message) (<-chan agent.TurnResult, error) {
	if root.blocked != nil {
		root.blocked <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if root.followupErr != nil {
		return nil, root.followupErr
	}
	result := make(chan agent.TurnResult, 1)
	root.mu.Lock()
	root.pending = result
	root.mu.Unlock()
	root.calls <- followup{message: message, result: result}
	return result, nil
}

func (root *fakeRoot) Interrupt() {
	root.mu.Lock()
	defer root.mu.Unlock()
	root.interrupts++
	if root.interruptCancels && root.pending != nil {
		select {
		case root.pending <- agent.TurnResult{Outcome: session.OutcomeCanceled}:
		default:
		}
	}
}

func (root *fakeRoot) cancelOnInterrupt(cancels bool) {
	root.mu.Lock()
	defer root.mu.Unlock()
	root.interruptCancels = cancels
}

func (root *fakeRoot) interrupted() int {
	root.mu.Lock()
	defer root.mu.Unlock()
	return root.interrupts
}

type rootSource struct {
	root agent.Controller
	err  error
}

func (source rootSource) Agent() (agent.Controller, error) { return source.root, source.err }

type driverFixture struct {
	*fixture
	root    *fakeRoot
	scope   *plugin.Scope
	changes chan Change
	turn    uint64
}

func startDriver(t *testing.T, prepare func(*driverFixture)) *driverFixture {
	t.Helper()
	service := startService(t)
	root := &fakeRoot{journal: service.journal, calls: make(chan followup, 8), interruptCancels: true}
	test := &driverFixture{fixture: service, root: root, scope: &plugin.Scope{}, changes: make(chan Change, 16)}
	watchScope := &plugin.Scope{}
	if err := service.service.Watch("root", func(change Change) { test.changes <- change }, watchScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watchScope.Close(context.Background()) })
	if prepare != nil {
		prepare(test)
	}
	driver, err := NewDriver(service.service, rootSource{root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := driver.Start(context.Background(), test.scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { test.stop(t) })
	return test
}

func (test *driverFixture) stop(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := test.scope.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func (test *driverFixture) next(t *testing.T) followup {
	t.Helper()
	select {
	case call := <-test.root.calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("driver queued no round")
		return followup{}
	}
}

func (test *driverFixture) change(t *testing.T, operation session.GoalOperation) Change {
	t.Helper()
	for {
		select {
		case change := <-test.changes:
			if change.Operation == operation {
				return change
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no %s change", operation)
		}
	}
}

// play opens the round through the real admission, then ends it.
func (test *driverFixture) play(t *testing.T, call followup, outcome session.TurnOutcome, during func()) {
	t.Helper()
	test.turn++
	turn := test.turn
	err := test.service.Admit(context.Background(), test.journal, call.message, func(context.Context) error {
		test.journal.raw(session.Record{Type: session.RecordTurnStart, Turn: turn}, session.Record{Type: session.RecordUserMessage, Turn: turn, Message: &call.message})
		return nil
	})
	if err != nil {
		call.result <- agent.TurnResult{Err: err}
		return
	}
	if during != nil {
		during()
	}
	test.journal.raw(session.Record{Type: session.RecordTurnEnd, Turn: turn, Outcome: outcome})
	call.result <- agent.TurnResult{Turn: turn, Outcome: outcome}
}

func TestNewDriver_Validates(t *testing.T) {
	service := startService(t)
	if _, err := NewDriver(nil, rootSource{}); err == nil {
		t.Fatal("nil goals accepted")
	}
	if _, err := NewDriver(service.service, nil); err == nil {
		t.Fatal("nil root accepted")
	}
	driver, _ := NewDriver(service.service, rootSource{})
	if driver.ID() != "goal-driver" {
		t.Fatalf("ID = %q", driver.ID())
	}
}

type failingWatch struct{ *Service }

func (failingWatch) Watch(string, func(Change), *plugin.Scope) error {
	return errors.New("watch failed")
}

func TestDriver_StartContainsFailures(t *testing.T) {
	service := startService(t)
	failure := errors.New("boom")
	root := &fakeRoot{journal: service.journal}
	for name, test := range map[string]struct {
		goals Goals
		root  rootSource
		scope func() *plugin.Scope
	}{
		"root":   {service.service, rootSource{err: failure}, func() *plugin.Scope { return &plugin.Scope{} }},
		"events": {service.service, rootSource{root: &fakeRoot{journal: service.journal, eventsErr: failure}}, func() *plugin.Scope { return &plugin.Scope{} }},
		"scope": {service.service, rootSource{root: root}, func() *plugin.Scope {
			scope := &plugin.Scope{}
			_ = scope.Close(context.Background())
			return scope
		}},
		"watch": {failingWatch{service.service}, rootSource{root: root}, func() *plugin.Scope { return &plugin.Scope{} }},
	} {
		driver, _ := NewDriver(test.goals, test.root)
		scope := test.scope()
		if err := driver.Start(context.Background(), scope); err == nil {
			t.Errorf("%s: start succeeded", name)
		}
		if err := scope.Close(context.Background()); err != nil {
			t.Errorf("%s: rollback = %v", name, err)
		}
	}
}

func TestDriver_RunsRoundsUntilTheModelCompletes(t *testing.T) {
	test := startDriver(t, nil)
	view := test.create(t, "ship", new(float64(3)))
	first := test.next(t)
	if first.message.Source != (session.MessageSource{Kind: session.GoalSource, GoalID: view.Goal.ID, GoalRevision: 1, GoalRound: 1}) {
		t.Fatalf("first round = %+v", first.message.Source)
	}
	test.play(t, first, session.OutcomeCompleted, nil)
	second := test.next(t)
	if second.message.Source.GoalRound != 2 {
		t.Fatalf("second round = %+v", second.message.Source)
	}
	test.play(t, second, session.OutcomeStepLimit, func() {
		if _, err := test.service.Complete(context.Background(), "root", view.Goal.Ref(), ActorModel); err != nil {
			t.Error(err)
		}
	})
	if test.root.interrupted() != 0 {
		t.Fatal("a model completion interrupted the turn")
	}
	test.stop(t)
	if len(test.root.calls) != 0 {
		t.Fatal("driver continued a complete goal")
	}
}

func TestDriver_BlocksAtTheRoundLimit(t *testing.T) {
	test := startDriver(t, nil)
	test.create(t, "ship", new(float64(1)))
	test.play(t, test.next(t), session.OutcomeCompleted, nil)
	blocked := test.change(t, session.GoalOpBlock)
	if blocked.Actor != ActorDriver || *blocked.View.Goal.BlockedReason != (session.GoalBlockReason{Code: "round-limit", Message: "Goal reached its configured limit of 1 rounds."}) {
		t.Fatalf("blocked = %+v", blocked.View)
	}
}

func TestDriver_CancelledRoundPausesAndHostPauseInterrupts(t *testing.T) {
	test := startDriver(t, nil)
	view := test.create(t, "ship", nil)
	test.play(t, test.next(t), session.OutcomeCanceled, nil)
	paused := test.change(t, session.GoalOpPause)
	if paused.Actor != ActorDriver || paused.View.Goal.Phase != session.GoalPaused {
		t.Fatalf("paused = %+v", paused)
	}
	resumed, err := test.service.Resume(context.Background(), "root", paused.View.Goal.Ref(), ActorHost)
	if err != nil {
		t.Fatal(err)
	}
	call := test.next(t)
	if call.message.Source.GoalRevision != resumed.Goal.Revision || call.message.Source.GoalRound != 2 {
		t.Fatalf("resumed round = %+v", call.message.Source)
	}
	// The round is open; a person pauses it and the driver interrupts the turn.
	test.turn++
	turn := test.turn
	if err := test.service.Admit(context.Background(), test.journal, call.message, func(context.Context) error {
		test.journal.raw(session.Record{Type: session.RecordTurnStart, Turn: turn}, session.Record{Type: session.RecordUserMessage, Turn: turn, Message: &call.message})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	test.root.cancelOnInterrupt(false)
	if _, err := test.service.Pause(context.Background(), "root", resumed.Goal.Ref(), ActorHost); err != nil {
		t.Fatal(err)
	}
	if test.root.interrupted() != 1 {
		t.Fatalf("host pause interrupts = %d", test.root.interrupted())
	}
	test.journal.raw(session.Record{Type: session.RecordTurnEnd, Turn: turn, Outcome: session.OutcomeCanceled})
	call.result <- agent.TurnResult{Turn: turn, Outcome: session.OutcomeCanceled}
	test.stop(t)
	current, _ := test.service.Get(context.Background(), "root")
	if current.Goal.Phase != session.GoalPaused || current.Goal.Revision != resumed.Goal.Revision+1 || view.Goal.ID != current.Goal.ID {
		t.Fatalf("host-paused goal was paused again or resumed: %+v", current)
	}
}

func TestDriver_ModelPauseDoesNotInterrupt(t *testing.T) {
	test := startDriver(t, nil)
	view := test.create(t, "ship", nil)
	call := test.next(t)
	test.play(t, call, session.OutcomeCompleted, func() {
		if _, err := test.service.Pause(context.Background(), "root", view.Goal.Ref(), ActorModel); err != nil {
			t.Error(err)
		}
	})
	test.stop(t)
	if test.root.interrupted() != 0 || len(test.root.calls) != 0 {
		t.Fatalf("interrupts=%d calls=%d", test.root.interrupted(), len(test.root.calls))
	}
}

func TestDriver_BlocksPersistentRefusalsAndQueueFailures(t *testing.T) {
	test := startDriver(t, nil)
	test.create(t, "ship", nil)
	// A refusal that neither a new revision nor a revoking stop explains is persistent.
	test.next(t).result <- agent.TurnResult{Err: agent.ErrNotAdmitted}
	blocked := test.change(t, session.GoalOpBlock)
	if blocked.View.Goal.BlockedReason.Code != "prompt-rejected" || blocked.View.Goal.BlockedReason.Message != "Goal round was rejected before entering its step." {
		t.Fatalf("blocked = %+v", blocked.View.Goal)
	}

	failing := startDriver(t, func(test *driverFixture) { test.root.followupErr = errors.New("queue full") })
	failing.create(t, "ship", nil)
	blocked = failing.change(t, session.GoalOpBlock)
	if *blocked.View.Goal.BlockedReason != (session.GoalBlockReason{Code: "queue-failed", Message: "Could not queue goal round 1: queue full"}) {
		t.Fatalf("queue failure = %+v", blocked.View.Goal)
	}
}

func TestDriver_StaleRefusalRetriesTheNewRevision(t *testing.T) {
	test := startDriver(t, nil)
	view := test.create(t, "ship", nil)
	call := test.next(t)
	objective := "ship more"
	if _, err := test.service.Edit(context.Background(), "root", view.Goal.Ref(), &objective, nil, ActorHost); err != nil {
		t.Fatal(err)
	}
	test.play(t, call, session.OutcomeCompleted, nil)
	retry := test.next(t)
	if retry.message.Source.GoalRevision != 2 || retry.message.Source.GoalRound != 1 {
		t.Fatalf("retry = %+v", retry.message.Source)
	}
	test.play(t, retry, session.OutcomeCompleted, nil)
	test.next(t).result <- agent.TurnResult{Err: agent.ErrNotAdmitted}
}

func TestDriver_TakesOverDisarmedAndStopsCleanly(t *testing.T) {
	test := startDriver(t, func(test *driverFixture) { test.create(t, "restored", nil) })
	current, _ := test.service.Get(context.Background(), "root")
	if current.Armed {
		t.Fatal("driver inherited an armed goal")
	}
	if _, err := test.service.Resume(context.Background(), "root", current.Goal.Ref(), ActorHost); err != nil {
		t.Fatal(err)
	}
	test.next(t) // in flight; shutdown interrupts it and waits for its result
	test.stop(t)
	if current, _ := test.service.Get(context.Background(), "root"); current.Armed || test.root.interrupted() != 1 {
		t.Fatalf("after stop armed=%t interrupts=%d", current.Armed, test.root.interrupted())
	}
}

func TestDriver_StopsWhileQueueingARound(t *testing.T) {
	test := startDriver(t, func(test *driverFixture) { test.root.blocked = make(chan struct{}, 1) })
	test.create(t, "ship", nil)
	<-test.root.blocked
	test.stop(t)
	if current, _ := test.service.Get(context.Background(), "root"); current.Goal.Phase != session.GoalActive || current.Armed {
		t.Fatalf("cancelled queueing changed the goal: %+v", current)
	}
}

func TestDriver_StopsWhenTheRootStopsAndBoundsCleanup(t *testing.T) {
	stopped := startDriver(t, func(test *driverFixture) { test.root.idleErr = agent.ErrNotRunning })
	stopped.stop(t)

	stuck := startDriver(t, nil)
	stuck.root.cancelOnInterrupt(false)
	stuck.create(t, "ship", nil)
	call := stuck.next(t)
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stuck.scope.Close(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("bounded cleanup = %v", err)
	}
	call.result <- agent.TurnResult{Outcome: session.OutcomeCanceled}
}
