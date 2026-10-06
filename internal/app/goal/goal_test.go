package goal

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// memoryJournal enforces record shapes and the goal fold like the JSONL store.
type memoryJournal struct {
	mu        sync.Mutex
	id        string
	events    []session.Event
	appendErr error
	eventsErr error
}

func (journal *memoryJournal) Header() session.Header { return session.Header{SessionID: journal.id} }

func (journal *memoryJournal) Events(context.Context) ([]session.Event, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.eventsErr != nil {
		return nil, journal.eventsErr
	}
	events := make([]session.Event, len(journal.events))
	for index, event := range journal.events {
		events[index] = session.CloneEvent(event)
	}
	return events, nil
}

func (journal *memoryJournal) Append(_ context.Context, record session.Record) (session.Event, error) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	if journal.appendErr != nil {
		return session.Event{}, journal.appendErr
	}
	if err := record.Validate(); err != nil {
		return session.Event{}, err
	}
	event := session.CloneEvent(session.Event{Sequence: uint64(len(journal.events) + 1), Record: record})
	if _, err := session.ProjectGoal(append(journal.events[:len(journal.events):len(journal.events)], event)); err != nil {
		return session.Event{}, err
	}
	journal.events = append(journal.events, event)
	return event, nil
}

// raw appends without validation, to stage corrupt or lifecycle records.
func (journal *memoryJournal) raw(records ...session.Record) {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	for _, record := range records {
		journal.events = append(journal.events, session.Event{Sequence: uint64(len(journal.events) + 1), Record: record})
	}
}

type memoryJournals struct{ journals map[string]*memoryJournal }

// settlementJournals fences the second lookup: Settle has read the old
// ending and is about to apply its Pause through the normal mutation path.
type settlementJournals struct {
	Journals
	mu      sync.Mutex
	lookups int
	read    chan struct{}
	apply   chan struct{}
}

func (journals *settlementJournals) Journal(id string) (agent.Journal, error) {
	journals.mu.Lock()
	journals.lookups++
	fence := journals.lookups == 2
	journals.mu.Unlock()
	if fence {
		close(journals.read)
		<-journals.apply
	}
	return journals.Journals.Journal(id)
}

func TestService_SettlePreservesLaterHumanAuthorization(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "pause-resume", true: "clear-create"}[replacement], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			fixture := startService(t)
			view := fixture.create(t, "old goal", nil)
			message := RoundMessage(*view)
			fixture.journal.raw(session.Record{Type: session.RecordTurnStart, Turn: 1}, session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message}, session.Record{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeCanceled})
			barrier := &settlementJournals{Journals: fixture.service.journals, read: make(chan struct{}), apply: make(chan struct{})}
			fixture.service.journals = barrier
			done := make(chan struct{})
			var settleErr error
			var release sync.Once
			go func() { _, settleErr = fixture.service.Settle(ctx, "root", 0); close(done) }()
			t.Cleanup(func() { release.Do(func() { close(barrier.apply) }); <-done })
			select {
			case <-barrier.read:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var authorized *View
			var err error
			if replacement {
				if err := fixture.service.Clear(t.Context(), "root", view.Goal.Ref(), ActorHost); err != nil {
					t.Fatal(err)
				}
				authorized, err = fixture.service.Create(t.Context(), "root", "new goal", nil, ActorHost)
			} else {
				paused, pauseErr := fixture.service.Pause(t.Context(), "root", view.Goal.Ref(), ActorHost)
				if pauseErr != nil {
					t.Fatal(pauseErr)
				}
				authorized, err = fixture.service.Resume(t.Context(), "root", paused.Goal.Ref(), ActorHost)
			}
			if err != nil {
				t.Fatal(err)
			}
			release.Do(func() { close(barrier.apply) })
			<-done
			err = settleErr
			if err != nil && codeOf(err) != CodeStaleRevision {
				t.Fatal(err)
			}
			current, err := fixture.service.Get(t.Context(), "root")
			if err != nil || current.Goal.Ref() != authorized.Goal.Ref() || !current.Armed || current.Goal.Phase != session.GoalActive {
				t.Fatalf("old settlement revoked human authorization: %+v, %v", current, err)
			}
		})
	}
}

func (journals memoryJournals) Journal(sessionID string) (agent.Journal, error) {
	journal, ok := journals.journals[sessionID]
	if !ok {
		return nil, agent.ErrAgentNotFound
	}
	return journal, nil
}

type recordingAdmissions struct {
	err        error
	kind       string
	admission  agent.Admission
	registered int
}

func (admissions *recordingAdmissions) RegisterAdmission(kind string, admission agent.Admission, scope *plugin.Scope) error {
	if admissions.err != nil {
		return admissions.err
	}
	admissions.kind, admissions.admission = kind, admission
	admissions.registered++
	return scope.Defer(func(context.Context) error {
		admissions.registered--
		return nil
	})
}

type clock struct{ now int64 }

func (clock *clock) time() time.Time { return time.UnixMilli(clock.now) }

type fixture struct {
	service    *Service
	journal    *memoryJournal
	admissions *recordingAdmissions
	clock      *clock
	scope      *plugin.Scope
}

func startService(t *testing.T) *fixture {
	t.Helper()
	journal := &memoryJournal{id: "root"}
	admissions := &recordingAdmissions{}
	now := &clock{now: 1000}
	service, err := New(memoryJournals{journals: map[string]*memoryJournal{"root": journal, "child": {id: "child"}}}, admissions, Config{Now: now.time, Random: bytes.NewReader(sequence(1024))})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := service.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return &fixture{service: service, journal: journal, admissions: admissions, clock: now, scope: scope}
}

func TestService_ECMAScriptObjectiveAndBlockReason(t *testing.T) {
	for _, test := range []struct {
		input, want string
		valid       bool
	}{
		{"\ufeff", "", false}, {"\ufeffship\ufeff", "ship", true}, {"\u0085", "\u0085", true}, {"\u0085ship\u0085", "\u0085ship\u0085", true},
	} {
		t.Run(test.input, func(t *testing.T) {
			fixture := startService(t)
			view, err := fixture.service.Create(t.Context(), "root", test.input, nil, ActorHost)
			if (err == nil) != test.valid {
				t.Errorf("create error=%v want valid=%t", err, test.valid)
			}
			if err == nil && view.Goal.Objective != test.want {
				t.Errorf("created objective=%q want=%q", view.Goal.Objective, test.want)
			}
			if view == nil {
				view = fixture.create(t, "base", nil)
			}
			edited, editErr := fixture.service.Edit(t.Context(), "root", view.Goal.Ref(), &test.input, nil, ActorHost)
			if (editErr == nil) != test.valid {
				t.Errorf("edit error=%v want valid=%t", editErr, test.valid)
			}
			if editErr == nil {
				view = edited
				if view.Goal.Objective != test.want {
					t.Errorf("edited objective=%q want=%q", view.Goal.Objective, test.want)
				}
			}
			blocked, blockErr := fixture.service.Block(t.Context(), "root", view.Goal.Ref(), session.GoalBlockReason{Code: "model-reported", Message: test.input}, ActorHost)
			if (blockErr == nil) != test.valid {
				t.Errorf("block error=%v want valid=%t", blockErr, test.valid)
			}
			if blockErr == nil && blocked.Goal.BlockedReason.Message != test.want {
				t.Errorf("reason=%q want=%q", blocked.Goal.BlockedReason.Message, test.want)
			}
		})
	}
}

func sequence(size int) []byte {
	data := make([]byte, size)
	for index := range data {
		data[index] = byte(index)
	}
	return data
}

func codeOf(err error) Code {
	var failure *Error
	if errors.As(err, &failure) {
		return failure.Code
	}
	return ""
}

func (fixture *fixture) create(t *testing.T, objective string, rounds *float64) *View {
	t.Helper()
	view, err := fixture.service.Create(context.Background(), "root", objective, rounds, ActorModel)
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func TestNew_ValidatesAndDefaults(t *testing.T) {
	journals, admissions := memoryJournals{}, &recordingAdmissions{}
	for _, test := range []struct {
		journals   Journals
		admissions Admissions
		config     Config
	}{
		{nil, admissions, Config{}},
		{journals, nil, Config{}},
		{journals, admissions, Config{DefaultMaxRounds: session.MaxGoalRounds + 1}},
	} {
		if _, err := New(test.journals, test.admissions, test.config); err == nil {
			t.Fatalf("New(%+v) accepted", test)
		}
	}
	service, err := New(journals, admissions, Config{})
	if err != nil || service.maxRounds != DefaultMaxRounds || service.now == nil || service.random == nil || service.ID() != "goals" {
		t.Fatalf("defaults = %+v, %v", service, err)
	}
}

func TestService_StartPublishesAdmissionAndCleanupStops(t *testing.T) {
	fixture := startService(t)
	if fixture.admissions.kind != session.GoalSource || fixture.admissions.admission != fixture.service || fixture.admissions.registered != 1 {
		t.Fatalf("admission = %+v", fixture.admissions)
	}
	if err := fixture.service.Start(context.Background(), &plugin.Scope{}); err == nil {
		t.Fatal("second start accepted")
	}
	fixture.create(t, "ship", nil)
	if err := fixture.scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fixture.admissions.registered != 0 || len(fixture.service.armed) != 0 {
		t.Fatalf("cleanup left admission=%d armed=%v", fixture.admissions.registered, fixture.service.armed)
	}
	if _, err := fixture.service.Get(context.Background(), "root"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped Get = %v", err)
	}
	if _, err := fixture.service.Pause(context.Background(), "root", session.GoalRef{}, ActorHost); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped Pause = %v", err)
	}
	if _, err := fixture.service.Settle(context.Background(), "root", 0); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped Settle = %v", err)
	}
	if err := fixture.service.Admit(context.Background(), fixture.journal, session.Message{}, nil); !errors.Is(err, agent.ErrNotAdmitted) {
		t.Fatalf("stopped Admit = %v", err)
	}

	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	inactive, _ := New(memoryJournals{}, &recordingAdmissions{}, Config{})
	if err := inactive.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) || inactive.running {
		t.Fatalf("closed scope = %v running=%t", err, inactive.running)
	}
	failing, _ := New(memoryJournals{}, &recordingAdmissions{err: errors.New("engine stopped")}, Config{})
	scope := &plugin.Scope{}
	if err := failing.Start(context.Background(), scope); err == nil {
		t.Fatal("admission failure ignored")
	}
	_ = scope.Close(context.Background())
	if failing.running {
		t.Fatal("rollback left the service running")
	}
}

func TestService_WatchNotifiesItsSessionUntilCleanup(t *testing.T) {
	fixture := startService(t)
	var changes []Change
	scope := &plugin.Scope{}
	for _, test := range []struct {
		id     string
		notify func(Change)
		scope  *plugin.Scope
	}{{"", func(Change) {}, scope}, {"root", nil, scope}, {"root", func(Change) {}, nil}} {
		if err := fixture.service.Watch(test.id, test.notify, test.scope); err == nil {
			t.Fatal("invalid watcher accepted")
		}
	}
	if err := fixture.service.Watch("root", func(change Change) { changes = append(changes, change) }, scope); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.Watch("child", func(Change) { t.Error("other session notified") }, scope); err != nil {
		t.Fatal(err)
	}
	view := fixture.create(t, "ship", nil)
	if _, err := fixture.service.Pause(context.Background(), "root", view.Goal.Ref(), ActorHost); err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Operation != session.GoalOpCreate || changes[0].Actor != ActorModel || !changes[0].View.Armed || changes[1].Operation != session.GoalOpPause || changes[1].Actor != ActorHost || changes[1].View.Armed {
		t.Fatalf("changes = %+v", changes)
	}
	_ = scope.Close(context.Background())
	if err := fixture.service.Clear(context.Background(), "root", session.GoalRef{ID: view.Goal.ID, Revision: 2}, ActorHost); err != nil || len(changes) != 2 {
		t.Fatalf("withdrawn watcher called: %v %d", err, len(changes))
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := fixture.service.Watch("root", func(Change) {}, closed); !errors.Is(err, plugin.ErrScopeClosed) || len(fixture.service.watchers) != 0 {
		t.Fatalf("closed scope watcher = %v, %d left", err, len(fixture.service.watchers))
	}
}

func TestService_CreateValidatesAndArms(t *testing.T) {
	fixture := startService(t)
	if view, err := fixture.service.Get(context.Background(), "root"); err != nil || view != nil {
		t.Fatalf("empty Get = %+v, %v", view, err)
	}
	for name, test := range map[string]struct {
		objective string
		rounds    *float64
		code      Code
		message   string
	}{
		"blank":      {" \n", nil, CodeInvalidObjective, "goal objective must be a non-empty string"},
		"too long":   {strings.Repeat("x", session.MaxGoalTextBytes+1), nil, CodeInvalidObjective, "goal objective exceeds 16384 bytes"},
		"zero cap":   {"ship", new(float64(0)), CodeInvalidMaxRounds, "maxGoalRounds must be a positive safe integer"},
		"fraction":   {"ship", new(float64(1.5)), CodeInvalidMaxRounds, "maxGoalRounds must be a positive safe integer"},
		"unsafe cap": {"ship", new(float64(session.MaxGoalRounds + 1)), CodeInvalidMaxRounds, "maxGoalRounds must be a positive safe integer"},
		"blank wins": {"", new(float64(0)), CodeInvalidObjective, "goal objective must be a non-empty string"},
	} {
		if _, err := fixture.service.Create(context.Background(), "root", test.objective, test.rounds, ActorModel); codeOf(err) != test.code || err.Error() != test.message {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := fixture.service.Create(context.Background(), "missing", "ship", nil, ActorModel); codeOf(err) != CodeAgentNotLive || err.Error() != `agent "missing" is not live in this registry` {
		t.Fatalf("missing agent = %v", err)
	}
	view := fixture.create(t, "  ship it  ", nil)
	want := session.GoalSnapshot{ID: "goal-101112131415161718191a1b1c1d1e1f", Revision: 1, Objective: "ship it", Phase: session.GoalActive, MaxRounds: DefaultMaxRounds}
	if view.Goal != want || !view.Armed || view.RoundsStarted != 0 || view.CreatedAtUnixMS != 1000 || view.UpdatedAtUnixMS != 1000 {
		t.Fatalf("created = %+v", view)
	}
	if _, err := fixture.service.Create(context.Background(), "root", "other", nil, ActorModel); codeOf(err) != CodeAlreadyExists || err.Error() != `goal "goal-101112131415161718191a1b1c1d1e1f" already exists with phase "active"` {
		t.Fatalf("duplicate = %v", err)
	}
	if _, err := fixture.service.Complete(context.Background(), "root", view.Goal.Ref(), ActorModel); err != nil {
		t.Fatal(err)
	}
	fixture.clock.now = 0
	replaced := fixture.create(t, "next", new(float64(session.MaxGoalRounds)))
	if replaced.Goal.MaxRounds != session.MaxGoalRounds || replaced.CreatedAtUnixMS != 1 || replaced.Goal.ID == view.Goal.ID {
		t.Fatalf("replacement = %+v", replaced)
	}
	exhausted, _ := New(memoryJournals{journals: map[string]*memoryJournal{"root": {id: "root"}}}, &recordingAdmissions{}, Config{Random: bytes.NewReader(nil)})
	scope := &plugin.Scope{}
	_ = exhausted.Start(context.Background(), scope)
	defer func() { _ = scope.Close(context.Background()) }()
	if _, err := exhausted.Create(context.Background(), "root", "ship", nil, ActorModel); err == nil || !strings.Contains(err.Error(), "generate goal ID") {
		t.Fatalf("entropy failure = %v", err)
	}
}

func TestService_LifecycleTransitions(t *testing.T) {
	fixture := startService(t)
	ctx := context.Background()
	view := fixture.create(t, "ship", new(float64(2)))
	ref := view.Goal.Ref()
	stale := session.GoalRef{ID: ref.ID, Revision: 9}
	if _, err := fixture.service.Pause(ctx, "root", stale, ActorHost); codeOf(err) != CodeStaleRevision || err.Error() != `stale goal ref "`+ref.ID+`" revision 9; current is "`+ref.ID+`" revision 1` {
		t.Fatalf("stale = %v", err)
	}
	if _, err := fixture.service.Resume(ctx, "root", ref, ActorHost); codeOf(err) != CodeInvalidTransition || err.Error() != `goal "`+ref.ID+`" is already active and armed` {
		t.Fatalf("armed resume = %v", err)
	}
	if _, err := fixture.service.Edit(ctx, "root", ref, nil, nil, ActorHost); codeOf(err) != CodeInvalidEdit {
		t.Fatalf("empty edit = %v", err)
	}
	blank := " "
	if _, err := fixture.service.Edit(ctx, "root", ref, &blank, nil, ActorHost); codeOf(err) != CodeInvalidObjective {
		t.Fatalf("blank edit = %v", err)
	}
	if _, err := fixture.service.Edit(ctx, "root", ref, nil, new(float64(-1)), ActorHost); codeOf(err) != CodeInvalidMaxRounds {
		t.Fatalf("negative cap edit = %v", err)
	}
	fixture.clock.now = 500
	objective := "ship more"
	edited, err := fixture.service.Edit(ctx, "root", ref, &objective, new(float64(3)), ActorHost)
	if err != nil || edited.Goal.Objective != "ship more" || edited.Goal.MaxRounds != 3 || edited.Goal.Revision != 2 || !edited.Armed || edited.UpdatedAtUnixMS != 1000 {
		t.Fatalf("edit = %+v, %v", edited, err)
	}
	fixture.clock.now = 2000
	paused, err := fixture.service.Pause(ctx, "root", edited.Goal.Ref(), ActorHost)
	if err != nil || paused.Goal.Phase != session.GoalPaused || paused.Armed || paused.UpdatedAtUnixMS != 2000 {
		t.Fatalf("pause = %+v, %v", paused, err)
	}
	if _, err := fixture.service.Pause(ctx, "root", paused.Goal.Ref(), ActorHost); codeOf(err) != CodeInvalidTransition || err.Error() != `cannot pause goal "`+ref.ID+`" from phase "paused"; expected active` {
		t.Fatalf("double pause = %v", err)
	}
	if _, err := fixture.service.Block(ctx, "root", paused.Goal.Ref(), session.GoalBlockReason{Code: "x", Message: "y"}, ActorHost); codeOf(err) != CodeInvalidTransition {
		t.Fatalf("block paused = %v", err)
	}
	resumed, err := fixture.service.Resume(ctx, "root", paused.Goal.Ref(), ActorHost)
	if err != nil || resumed.Goal.Phase != session.GoalActive || !resumed.Armed {
		t.Fatalf("resume = %+v, %v", resumed, err)
	}
	for name, reason := range map[string]session.GoalBlockReason{
		"bad code":      {Code: "Bad", Message: "y"},
		"blank message": {Code: "x", Message: "  "},
		"long message":  {Code: "x", Message: strings.Repeat("m", session.MaxGoalTextBytes+1)},
	} {
		if _, err := fixture.service.Block(ctx, "root", resumed.Goal.Ref(), reason, ActorModel); codeOf(err) != CodeInvalidBlockReason || err.Error() != "goal block reason requires a lower-kebab-case code and a non-empty message" {
			t.Errorf("%s: %v", name, err)
		}
	}
	blocked, err := fixture.service.Block(ctx, "root", resumed.Goal.Ref(), session.GoalBlockReason{Code: "model-reported", Message: " needs a key "}, ActorModel)
	if err != nil || blocked.Goal.Phase != session.GoalBlocked || blocked.Armed || *blocked.Goal.BlockedReason != (session.GoalBlockReason{Code: "model-reported", Message: "needs a key"}) {
		t.Fatalf("block = %+v, %v", blocked, err)
	}
	reworded := "ship most"
	editedBlocked, err := fixture.service.Edit(ctx, "root", blocked.Goal.Ref(), &reworded, nil, ActorHost)
	if err != nil || editedBlocked.Goal.Phase != session.GoalBlocked || editedBlocked.Goal.BlockedReason == nil || editedBlocked.Armed {
		t.Fatalf("edit blocked = %+v, %v", editedBlocked, err)
	}
	completed, err := fixture.service.Complete(ctx, "root", editedBlocked.Goal.Ref(), ActorHost)
	if err != nil || completed.Goal.Phase != session.GoalComplete || completed.Goal.BlockedReason != nil {
		t.Fatalf("complete = %+v, %v", completed, err)
	}
	if _, err := fixture.service.Resume(ctx, "root", completed.Goal.Ref(), ActorHost); codeOf(err) != CodeInvalidTransition || err.Error() != `cannot resume goal "`+ref.ID+`" from phase "complete"; expected active or paused or blocked` {
		t.Fatalf("resume complete = %v", err)
	}
	if _, err := fixture.service.Complete(ctx, "root", completed.Goal.Ref(), ActorHost); codeOf(err) != CodeInvalidTransition {
		t.Fatalf("complete twice = %v", err)
	}
	fixture.clock.now = 10
	if err := fixture.service.Clear(ctx, "root", completed.Goal.Ref(), ActorHost); err != nil {
		t.Fatal(err)
	}
	events, _ := fixture.journal.Events(ctx)
	if last := events[len(events)-1].Record.Goal; last.Operation != session.GoalOpClear || last.ClearedAtUnixMS != 2000 || last.Cleared.Revision != completed.Goal.Revision+1 {
		t.Fatalf("tombstone = %+v", last)
	}
	if err := fixture.service.Clear(ctx, "root", completed.Goal.Ref(), ActorHost); codeOf(err) != CodeNotFound || err.Error() != "no current goal" {
		t.Fatalf("clear none = %v", err)
	}
	for _, operation := range []func(session.GoalRef) error{
		func(ref session.GoalRef) error {
			_, err := fixture.service.Resume(ctx, "root", ref, ActorHost)
			return err
		},
		func(ref session.GoalRef) error {
			_, err := fixture.service.Block(ctx, "root", ref, session.GoalBlockReason{}, ActorHost)
			return err
		},
		func(ref session.GoalRef) error {
			_, err := fixture.service.Edit(ctx, "root", ref, &reworded, nil, ActorHost)
			return err
		},
	} {
		if err := operation(ref); codeOf(err) != CodeNotFound {
			t.Fatalf("operation without goal = %v", err)
		}
	}
}

func TestService_ResumeRequiresRemainingRoundsAndRearmsDisarmedGoals(t *testing.T) {
	fixture := startService(t)
	ctx := context.Background()
	view := fixture.create(t, "ship", new(float64(1)))
	fixture.service.Disarm("root")
	if current, _ := fixture.service.Get(ctx, "root"); current.Armed {
		t.Fatal("Disarm left the goal armed")
	}
	rearmed, err := fixture.service.Resume(ctx, "root", view.Goal.Ref(), ActorModel)
	if err != nil || !rearmed.Armed || rearmed.Goal.Revision != 2 {
		t.Fatalf("rearm = %+v, %v", rearmed, err)
	}
	fixture.journal.raw(session.Record{Type: session.RecordTurnStart, Turn: 1})
	if err := fixture.service.Admit(ctx, fixture.journal, RoundMessage(*rearmed), func(context.Context) error {
		fixture.journal.raw(session.Record{Type: session.RecordUserMessage, Turn: 1, Message: new(RoundMessage(*rearmed))}, session.Record{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeCompleted})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	paused, err := fixture.service.Pause(ctx, "root", rearmed.Goal.Ref(), ActorHost)
	if err != nil || paused.RoundsStarted != 1 {
		t.Fatalf("pause = %+v, %v", paused, err)
	}
	if _, err := fixture.service.Resume(ctx, "root", paused.Goal.Ref(), ActorHost); codeOf(err) != CodeInvalidTransition || err.Error() != `goal "`+view.Goal.ID+`" exhausted 1 goal rounds; increase maxGoalRounds before resuming` {
		t.Fatalf("exhausted resume = %v", err)
	}
}

func TestService_ContainsJournalFailures(t *testing.T) {
	fixture := startService(t)
	ctx := context.Background()
	failure := errors.New("disk full")
	fixture.journal.appendErr = failure
	if _, err := fixture.service.Create(ctx, "root", "ship", nil, ActorModel); !errors.Is(err, failure) || len(fixture.service.armed) != 0 {
		t.Fatalf("append failure = %v armed=%v", err, fixture.service.armed)
	}
	fixture.journal.appendErr = nil
	fixture.journal.eventsErr = failure
	if _, err := fixture.service.Create(ctx, "root", "ship", nil, ActorModel); !errors.Is(err, failure) {
		t.Fatalf("events failure = %v", err)
	}
	if _, err := fixture.service.Get(ctx, "root"); !errors.Is(err, failure) {
		t.Fatalf("Get events failure = %v", err)
	}
	if _, err := fixture.service.Get(ctx, "missing"); codeOf(err) != CodeAgentNotLive {
		t.Fatalf("Get missing = %v", err)
	}
	if _, err := fixture.service.Settle(ctx, "root", 3); !errors.Is(err, failure) {
		t.Fatalf("Settle events failure = %v", err)
	}
	if _, err := fixture.service.Settle(ctx, "missing", 3); codeOf(err) != CodeAgentNotLive {
		t.Fatalf("Settle missing = %v", err)
	}
	if err := fixture.service.Admit(ctx, fixture.journal, session.Message{}, nil); !errors.Is(err, failure) {
		t.Fatalf("Admit events failure = %v", err)
	}
	fixture.journal.eventsErr = nil
	fixture.journal.raw(session.Record{Type: session.RecordGoalChange, Goal: &session.GoalChange{Operation: session.GoalOpClear, Cleared: &session.GoalRef{ID: "goal", Revision: 2}, ClearedAtUnixMS: 1}})
	if _, err := fixture.service.Get(ctx, "root"); !errors.Is(err, session.ErrInvalidRecord) {
		t.Fatalf("corrupt Get = %v", err)
	}
	if err := fixture.service.Admit(ctx, fixture.journal, session.Message{}, nil); !errors.Is(err, session.ErrInvalidRecord) {
		t.Fatalf("corrupt Admit = %v", err)
	}
	if _, err := fixture.service.Settle(ctx, "root", 0); !errors.Is(err, session.ErrInvalidRecord) {
		t.Fatalf("corrupt Settle = %v", err)
	}
}

func TestService_AdmitsOnlyTheNextRoundOfTheArmedRevision(t *testing.T) {
	fixture := startService(t)
	ctx := context.Background()
	view := fixture.create(t, "ship", new(float64(1)))
	opened := 0
	open := func(context.Context) error { opened++; return nil }
	round := RoundMessage(*view)
	for name, message := range map[string]session.Message{
		"other goal":     withSource(round, func(source *session.MessageSource) { source.GoalID = "goal-other" }),
		"stale revision": withSource(round, func(source *session.MessageSource) { source.GoalRevision = 9 }),
		"skipped round":  withSource(round, func(source *session.MessageSource) { source.GoalRound = 2 }),
	} {
		if err := fixture.service.Admit(ctx, fixture.journal, message, open); !errors.Is(err, agent.ErrNotAdmitted) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := fixture.service.Admit(ctx, &memoryJournal{id: "child"}, round, open); !errors.Is(err, agent.ErrNotAdmitted) {
		t.Fatalf("goal-less session admitted: %v", err)
	}
	fixture.service.Disarm("root")
	if err := fixture.service.Admit(ctx, fixture.journal, round, open); !errors.Is(err, agent.ErrNotAdmitted) {
		t.Fatalf("disarmed goal admitted: %v", err)
	}
	rearmed, _ := fixture.service.Resume(ctx, "root", view.Goal.Ref(), ActorHost)
	round = RoundMessage(*rearmed)
	fixture.journal.raw(session.Record{Type: session.RecordTurnStart, Turn: 1}, session.Record{Type: session.RecordUserMessage, Turn: 1, Message: new(agentText("hi"))}, session.Record{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeCanceled})
	if err := fixture.service.Admit(ctx, fixture.journal, round, open); !errors.Is(err, agent.ErrNotAdmitted) || opened != 0 {
		t.Fatalf("round after a revoking stop admitted: %v", err)
	}
	fixture.service.Disarm("root")
	rearmed, _ = fixture.service.Resume(ctx, "root", rearmed.Goal.Ref(), ActorHost)
	round = RoundMessage(*rearmed)
	if err := fixture.service.Admit(ctx, fixture.journal, round, open); err != nil || opened != 1 {
		t.Fatalf("valid round = %v opened=%d", err, opened)
	}
	failure := errors.New("open failed")
	if err := fixture.service.Admit(ctx, fixture.journal, round, func(context.Context) error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("open failure = %v", err)
	}
	fixture.journal.raw(session.Record{Type: session.RecordTurnStart, Turn: 2}, session.Record{Type: session.RecordUserMessage, Turn: 2, Message: &round})
	if err := fixture.service.Admit(ctx, fixture.journal, RoundMessage(View{Goal: rearmed.Goal, RoundsStarted: 1}), open); !errors.Is(err, agent.ErrNotAdmitted) {
		t.Fatalf("round over the cap admitted: %v", err)
	}
	if _, err := fixture.service.Pause(ctx, "root", rearmed.Goal.Ref(), ActorHost); err != nil {
		t.Fatal(err)
	}
	if err := fixture.service.Admit(ctx, fixture.journal, round, open); !errors.Is(err, agent.ErrNotAdmitted) {
		t.Fatalf("paused goal admitted: %v", err)
	}
}

func agentText(text string) session.Message {
	return session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}
}

func withSource(message session.Message, mutate func(*session.MessageSource)) session.Message {
	mutate(&message.Source)
	return message
}

func TestService_SettlePausesCancelledRoundsAndDisarmsOtherStops(t *testing.T) {
	ctx := context.Background()
	turn := func(journal *memoryJournal, number uint64, message session.Message, outcome session.TurnOutcome) {
		journal.raw(session.Record{Type: session.RecordTurnStart, Turn: number}, session.Record{Type: session.RecordUserMessage, Turn: number, Message: &message}, session.Record{Type: session.RecordUserMessage, Turn: number, Message: new(agentText("steer"))}, session.Record{Type: session.RecordTurnEnd, Turn: number, Outcome: outcome})
	}
	t.Run("cancelled round pauses its revision", func(t *testing.T) {
		fixture := startService(t)
		view := fixture.create(t, "ship", nil)
		turn(fixture.journal, 1, RoundMessage(*view), session.OutcomeCanceled)
		last, err := fixture.service.Settle(ctx, "root", 0)
		current, _ := fixture.service.Get(ctx, "root")
		if err != nil || last != 5 || current.Goal.Phase != session.GoalPaused || current.Armed {
			t.Fatalf("settle = %d, %v, %+v", last, err, current)
		}
		if again, err := fixture.service.Settle(ctx, "root", last+1); err != nil || again != last+1 {
			t.Fatalf("nothing new = %d, %v", again, err)
		}
	})
	t.Run("cancelled human turn disarms", func(t *testing.T) {
		fixture := startService(t)
		fixture.create(t, "ship", nil)
		turn(fixture.journal, 1, agentText("work"), session.OutcomeCanceled)
		if _, err := fixture.service.Settle(ctx, "root", 0); err != nil {
			t.Fatal(err)
		}
		if current, _ := fixture.service.Get(ctx, "root"); current.Goal.Phase != session.GoalActive || current.Armed {
			t.Fatalf("current = %+v", current)
		}
	})
	t.Run("output limit disarms without pausing", func(t *testing.T) {
		fixture := startService(t)
		view := fixture.create(t, "ship", nil)
		turn(fixture.journal, 1, RoundMessage(*view), session.TurnOutcome("max_tokens"))
		if _, err := fixture.service.Settle(ctx, "root", 0); err != nil {
			t.Fatal(err)
		}
		if current, _ := fixture.service.Get(ctx, "root"); current.Goal.Phase != session.GoalActive || current.Armed {
			t.Fatalf("current = %+v", current)
		}
	})
	t.Run("failed round disarms without pausing", func(t *testing.T) {
		fixture := startService(t)
		view := fixture.create(t, "ship", nil)
		turn(fixture.journal, 1, RoundMessage(*view), session.OutcomeError)
		if _, err := fixture.service.Settle(ctx, "root", 0); err != nil {
			t.Fatal(err)
		}
		if current, _ := fixture.service.Get(ctx, "root"); current.Goal.Phase != session.GoalActive || current.Armed {
			t.Fatalf("current = %+v", current)
		}
	})
	t.Run("a later resume wins", func(t *testing.T) {
		fixture := startService(t)
		view := fixture.create(t, "ship", nil)
		turn(fixture.journal, 1, RoundMessage(*view), session.OutcomeCanceled)
		fixture.service.Disarm("root")
		if _, err := fixture.service.Resume(ctx, "root", view.Goal.Ref(), ActorHost); err != nil {
			t.Fatal(err)
		}
		turn(fixture.journal, 2, agentText("ok"), session.OutcomeCompleted)
		if _, err := fixture.service.Settle(ctx, "root", 0); err != nil {
			t.Fatal(err)
		}
		if current, _ := fixture.service.Get(ctx, "root"); current.Goal.Phase != session.GoalActive || !current.Armed {
			t.Fatalf("current = %+v", current)
		}
	})
	t.Run("an edited revision is not paused", func(t *testing.T) {
		fixture := startService(t)
		view := fixture.create(t, "ship", nil)
		turn(fixture.journal, 1, RoundMessage(*view), session.OutcomeCanceled)
		objective := "ship more"
		if _, err := fixture.service.Edit(ctx, "root", view.Goal.Ref(), &objective, nil, ActorHost); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.Settle(ctx, "root", 0); err != nil {
			t.Fatal(err)
		}
		if current, _ := fixture.service.Get(ctx, "root"); current.Goal.Phase != session.GoalActive || !current.Armed {
			t.Fatalf("current = %+v", current)
		}
	})
	t.Run("a failed pause disarms", func(t *testing.T) {
		fixture := startService(t)
		view := fixture.create(t, "ship", nil)
		turn(fixture.journal, 1, RoundMessage(*view), session.OutcomeCanceled)
		failure := errors.New("disk full")
		fixture.journal.appendErr = failure
		if _, err := fixture.service.Settle(ctx, "root", 0); !errors.Is(err, failure) {
			t.Fatalf("settle = %v", err)
		}
		fixture.journal.appendErr = nil
		if current, _ := fixture.service.Get(ctx, "root"); current.Goal.Phase != session.GoalActive || current.Armed {
			t.Fatalf("current = %+v", current)
		}
	})
	t.Run("empty log", func(t *testing.T) {
		fixture := startService(t)
		if last, err := fixture.service.Settle(ctx, "root", 0); err != nil || last != 0 {
			t.Fatalf("settle = %d, %v", last, err)
		}
	})
}

func TestRoundCap_AcceptsPositiveSafeIntegers(t *testing.T) {
	for value, want := range map[float64]uint64{1: 1, 256: 256, session.MaxGoalRounds: session.MaxGoalRounds} {
		if got, err := RoundCap(value); err != nil || got != want {
			t.Errorf("RoundCap(%v) = %d, %v", value, got, err)
		}
	}
	for _, value := range []float64{0, -1, 0.5, 2.5, session.MaxGoalRounds + 1} {
		if _, err := RoundCap(value); codeOf(err) != CodeInvalidMaxRounds {
			t.Errorf("RoundCap(%v) = %v", value, err)
		}
	}
}

func TestService_AuthorityReadsOnlyTheCallersTurn(t *testing.T) {
	fixture := startService(t)
	ctx := context.Background()
	view := fixture.create(t, "ship", nil)
	human := agentText("go")
	round := RoundMessage(*view)
	fixture.journal.raw(
		session.Record{Type: session.RecordTurnStart, Turn: 1}, session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &human}, session.Record{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeCompleted},
		session.Record{Type: session.RecordTurnStart, Turn: 2}, session.Record{Type: session.RecordUserMessage, Turn: 2, Message: &round},
	)
	for name, test := range map[string]struct {
		turn      uint64
		delegated bool
		human     bool
		round     bool
	}{
		"human turn":     {1, false, true, false},
		"delegated turn": {1, true, false, false},
		"goal round":     {2, false, false, true},
		"other turn":     {3, false, false, false},
	} {
		authority, err := fixture.service.Authority(ctx, "root", test.turn, test.delegated)
		if err != nil || authority.Human != test.human || (authority.Round != nil) != test.round {
			t.Errorf("%s: %+v, %v", name, authority, err)
		}
	}
	steer := agentText("also this")
	fixture.journal.raw(session.Record{Type: session.RecordUserMessage, Turn: 2, Message: &steer})
	if authority, _ := fixture.service.Authority(ctx, "root", 2, false); !authority.Human || authority.Round == nil || authority.Round.RoundsStarted != 1 {
		t.Fatalf("steered round = %+v", authority)
	}
	objective := "ship more"
	if _, err := fixture.service.Edit(ctx, "root", view.Goal.Ref(), &objective, nil, ActorHost); err != nil {
		t.Fatal(err)
	}
	if authority, _ := fixture.service.Authority(ctx, "root", 2, false); authority.Round != nil {
		t.Fatal("a round of an older revision kept authority")
	}
	if _, err := fixture.service.Authority(ctx, "missing", 1, false); codeOf(err) != CodeAgentNotLive {
		t.Fatalf("missing = %v", err)
	}
	failure := errors.New("unreadable")
	fixture.journal.eventsErr = failure
	if _, err := fixture.service.Authority(ctx, "root", 1, false); !errors.Is(err, failure) {
		t.Fatalf("events failure = %v", err)
	}
	fixture.journal.eventsErr = nil
	fixture.journal.raw(session.Record{Type: session.RecordGoalChange, Goal: &session.GoalChange{Operation: session.GoalOpCreate, Snapshot: &session.GoalSnapshot{ID: "goal-x", Revision: 1, Objective: "x", Phase: session.GoalActive, MaxRounds: 1}, CreatedAtUnixMS: 1, UpdatedAtUnixMS: 1}})
	if _, err := fixture.service.Authority(ctx, "root", 1, false); !errors.Is(err, session.ErrInvalidRecord) {
		t.Fatalf("corrupt = %v", err)
	}
	_ = fixture.scope.Close(ctx)
	if _, err := fixture.service.Authority(ctx, "root", 1, false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped = %v", err)
	}
}

// TestService_ForkedChildOwnsOnlyItsOwnGoal proves that the goal facts a
// forked child inherits from its parent's prefix stay the parent's: the child
// reads no goal, cannot admit a round of it, and settles nothing from it.
func TestService_ForkedChildOwnsOnlyItsOwnGoal(t *testing.T) {
	fixture := startService(t)
	ctx := context.Background()
	parent := fixture.create(t, "parent objective", nil)
	round := RoundMessage(*parent)
	fixture.journal.raw(
		session.Record{Type: session.RecordTurnStart, Turn: 1},
		session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &round},
		session.Record{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeCanceled},
	)
	inherited, _ := fixture.journal.Events(ctx)
	child := &memoryJournal{id: "child", events: inherited}
	child.raw(session.Record{Type: session.RecordSubagentDescriptor, Subagent: &session.SubagentDescriptor{Version: 2, Provider: "in-process", Mode: "one-shot", Label: "fork", Inherited: uint64(len(inherited))}})
	service, err := New(memoryJournals{journals: map[string]*memoryJournal{"root": fixture.journal, "child": child}}, &recordingAdmissions{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := service.Start(ctx, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(ctx) })
	if view, err := service.Get(ctx, "child"); err != nil || view != nil {
		t.Fatalf("child goal = %+v, %v", view, err)
	}
	if authority, err := service.Authority(ctx, "child", 1, true); err != nil || authority.Round != nil || authority.Human {
		t.Fatalf("child authority = %+v, %v", authority, err)
	}
	next := RoundMessage(View{Goal: parent.Goal, RoundsStarted: 1})
	if err := service.Admit(ctx, child, next, func(context.Context) error { return nil }); !errors.Is(err, agent.ErrNotAdmitted) {
		t.Fatalf("child admitted the parent's round: %v", err)
	}
	if _, err := service.Settle(ctx, "child", 0); err != nil {
		t.Fatal(err)
	}
	if view, _ := fixture.service.Get(ctx, "root"); view.Goal.Phase != session.GoalActive {
		t.Fatalf("settling the child changed the parent: %+v", view)
	}
	if _, err := service.Edit(ctx, "child", parent.Goal.Ref(), new("x"), nil, ActorModel); codeOf(err) != CodeNotFound {
		t.Fatalf("child edited the parent's goal: %v", err)
	}
}
