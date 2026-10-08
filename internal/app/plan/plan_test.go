package plan

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type memoryJournal struct {
	id        string
	events    []session.Event
	eventsErr error
	// failAt fails the append whose 1-based position among appends matches.
	failAt  int
	appends int
	// entered and release, when set, hold every Events call open until
	// release or the call's context ends.
	entered chan struct{}
	release chan struct{}
}

func (journal *memoryJournal) Header() session.Header { return session.Header{SessionID: journal.id} }

func (journal *memoryJournal) Events(ctx context.Context) ([]session.Event, error) {
	if journal.entered != nil {
		journal.entered <- struct{}{}
		select {
		case <-journal.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return append([]session.Event(nil), journal.events...), journal.eventsErr
}

// stubbornJournal reports when a read observes cancellation, then finishes
// that read successfully once released, like a journal whose read was
// already past its last cancellation check.
type stubbornJournal struct {
	*memoryJournal
	entered, cancelled, release chan struct{}
}

func (journal *stubbornJournal) Events(ctx context.Context) ([]session.Event, error) {
	close(journal.entered)
	<-ctx.Done()
	close(journal.cancelled)
	<-journal.release
	return journal.memoryJournal.Events(context.Background())
}

func (journal *memoryJournal) Append(_ context.Context, record session.Record) (session.Event, error) {
	journal.appends++
	if journal.appends == journal.failAt {
		return session.Event{}, errors.New("disk full")
	}
	if err := record.Validate(); err != nil {
		return session.Event{}, err
	}
	event := session.Event{Sequence: uint64(len(journal.events) + 1), Record: record}
	journal.events = append(journal.events, event)
	return event, nil
}

func (journal *memoryJournal) record(record session.Record) {
	journal.events = append(journal.events, session.Event{Sequence: uint64(len(journal.events) + 1), Record: record})
}

func (journal *memoryJournal) header(turn, step uint64) {
	journal.record(session.Record{Type: session.RecordRequestHeader, Turn: turn, Step: step, Header: &session.RequestHeader{Provider: "p", Model: "m"}})
}

// tail returns the record types appended after index start.
func (journal *memoryJournal) tail(start int) []string {
	var kinds []string
	for _, event := range journal.events[start:] {
		kind := string(event.Record.Type)
		switch {
		case event.Record.Plan != nil && event.Record.Plan.Active:
			kind += "=on"
		case event.Record.Plan != nil:
			kind += "=off"
		case event.Record.Message != nil:
			kind += ":" + session.Text(*event.Record.Message)
		}
		kinds = append(kinds, kind)
	}
	return kinds
}

func startPlan(t *testing.T) (*Service, *plugin.Scope) {
	t.Helper()
	service := New()
	scope := &plugin.Scope{}
	if service.ID() != "plan-mode" {
		t.Fatalf("ID = %q", service.ID())
	}
	if err := service.Start(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return service, scope
}

func equal(left, right []string) bool { return strings.Join(left, "|") == strings.Join(right, "|") }

func TestService_Lifecycle(t *testing.T) {
	service := New()
	journal := &memoryJournal{id: "s"}
	closed := &plugin.Scope{}
	_ = closed.Close(t.Context())
	if err := service.Start(t.Context(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("start with closed scope = %v", err)
	}
	if _, err := service.Select(t.Context(), journal, true, false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("select before start = %v", err)
	}
	if _, err := service.Step(t.Context(), journal, 1); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("step before start = %v", err)
	}
	if err := service.Exit(t.Context(), "s"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("exit before start = %v", err)
	}
	service, scope := startPlan(t)
	if err := service.Start(t.Context(), &plugin.Scope{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("double start = %v", err)
	}
	if _, err := service.Select(t.Context(), journal, true, true); err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if service.Active("s") {
		t.Fatal("stopped service reports plan mode")
	}
	if _, err := service.Select(t.Context(), journal, true, false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("select after stop = %v", err)
	}
	if len(journal.events) != 0 {
		t.Fatalf("a pending selection survived shutdown: %+v", journal.events)
	}
}

func TestService_SelectBetweenTurnsCommitsAtOnce(t *testing.T) {
	service, _ := startPlan(t)
	journal := &memoryJournal{id: "s"}
	journal.header(1, 1)
	for _, step := range []struct {
		active bool
		want   Change
	}{{true, Committed}, {true, Unchanged}, {false, Committed}, {false, Unchanged}} {
		change, err := service.Select(t.Context(), journal, step.active, false)
		if err != nil || change != step.want {
			t.Fatalf("select(%t) = %s, %v; want %s", step.active, change, err, step.want)
		}
	}
	if got := journal.tail(1); !equal(got, []string{"plan/mode=on", "plan/mode=off"}) {
		t.Fatalf("records = %v", got)
	}
	if journal.events[1].Record.Turn != 0 {
		t.Fatalf("idle commit names turn %d", journal.events[1].Record.Turn)
	}
	// The flip-flop nets out: the last request already described the default mode.
	section, err := service.Step(t.Context(), journal, 2)
	if err != nil || section != "" || len(journal.events) != 3 {
		t.Fatalf("step = %q, %v, records %v", section, err, journal.tail(0))
	}
}

func TestService_SelectDuringTurnWaitsForTheBoundary(t *testing.T) {
	service, _ := startPlan(t)
	journal := &memoryJournal{id: "s"}
	journal.header(1, 1)
	if change, err := service.Select(t.Context(), journal, true, true); err != nil || change != Queued {
		t.Fatalf("queued = %s, %v", change, err)
	}
	if change, err := service.Select(t.Context(), journal, true, true); err != nil || change != Unchanged {
		t.Fatalf("repeat = %s, %v", change, err)
	}
	if len(journal.events) != 1 || service.Active("s") {
		t.Fatal("a mid-turn selection took effect before the boundary")
	}
	section, err := service.Step(t.Context(), journal, 1)
	if err != nil || section != Section {
		t.Fatalf("boundary = %q, %v", section, err)
	}
	if got := journal.tail(1); !equal(got, []string{"plan/mode=on", "user/message:" + enteredNotice}) {
		t.Fatalf("records = %v", got)
	}
	if journal.events[1].Record.Turn != 1 || journal.events[2].Record.Message.Source.Kind != NoticeSource || !service.Active("s") {
		t.Fatalf("boundary records = %+v", journal.events[1:])
	}
	journal.header(1, 2)
	if section, err := service.Step(t.Context(), journal, 1); err != nil || section != Section || len(journal.events) != 4 {
		t.Fatalf("steady boundary = %q, %v, %v", section, err, journal.tail(0))
	}
	// Leaving mid-turn, then changing one's mind, cancels the pending exit.
	if change, err := service.Select(t.Context(), journal, false, true); err != nil || change != Queued {
		t.Fatalf("queued exit = %s, %v", change, err)
	}
	if change, err := service.Select(t.Context(), journal, true, true); err != nil || change != Cancelled {
		t.Fatalf("cancel exit = %s, %v", change, err)
	}
	if section, err := service.Step(t.Context(), journal, 1); err != nil || section != Section || len(journal.events) != 4 {
		t.Fatalf("cancelled boundary = %q, %v, %v", section, err, journal.tail(0))
	}
	if change, err := service.Select(t.Context(), journal, false, true); err != nil || change != Queued {
		t.Fatalf("queued exit = %s, %v", change, err)
	}
	if section, err := service.Step(t.Context(), journal, 1); err != nil || section != "" {
		t.Fatalf("exit boundary = %q, %v", section, err)
	}
	if got := journal.tail(4); !equal(got, []string{"plan/mode=off", "user/message:" + leftNotice}) {
		t.Fatalf("exit records = %v", got)
	}
	if service.Active("s") {
		t.Fatal("the session is still in plan mode after the exit boundary")
	}
}

func TestService_SelectCancelsAPendingIdleOpposite(t *testing.T) {
	service, _ := startPlan(t)
	journal := &memoryJournal{id: "s"}
	if change, err := service.Select(t.Context(), journal, true, true); err != nil || change != Queued {
		t.Fatalf("queued = %s, %v", change, err)
	}
	// The turn ended before a boundary; leaving again between turns drops the selection.
	if change, err := service.Select(t.Context(), journal, false, false); err != nil || change != Cancelled {
		t.Fatalf("cancel = %s, %v", change, err)
	}
	if len(journal.events) != 0 {
		t.Fatalf("records = %v", journal.tail(0))
	}
	// A first request has no earlier header, so entering needs no notice.
	if _, err := service.Select(t.Context(), journal, true, false); err != nil {
		t.Fatal(err)
	}
	if section, err := service.Step(t.Context(), journal, 1); err != nil || section != Section || len(journal.events) != 1 {
		t.Fatalf("first boundary = %q, %v, %v", section, err, journal.tail(0))
	}
}

func TestService_ExitAppliesAtTheNextBoundaryWithoutNotice(t *testing.T) {
	service, _ := startPlan(t)
	journal := &memoryJournal{id: "s"}
	if err := service.Exit(t.Context(), "s"); !errors.Is(err, ErrInactive) {
		t.Fatalf("exit for an unseen session = %v", err)
	}
	if section, err := service.Step(t.Context(), journal, 1); err != nil || section != "" {
		t.Fatalf("default boundary = %q, %v", section, err)
	}
	if err := service.Exit(t.Context(), "s"); !errors.Is(err, ErrInactive) {
		t.Fatalf("exit outside plan mode = %v", err)
	}
	journal.record(session.Record{Type: session.RecordPlanMode, Plan: &session.PlanMode{Active: true}})
	journal.header(1, 1)
	if section, err := service.Step(t.Context(), journal, 2); err != nil || section != Section {
		t.Fatalf("resumed boundary = %q, %v", section, err)
	}
	journal.header(2, 1)
	if _, err := service.Select(t.Context(), journal, false, true); err != nil {
		t.Fatal(err)
	}
	if err := service.Exit(t.Context(), "s"); err != nil {
		t.Fatal(err)
	}
	if !service.Active("s") {
		t.Fatal("approval left plan mode before the boundary")
	}
	if section, err := service.Step(t.Context(), journal, 2); err != nil || section != "" {
		t.Fatalf("exit boundary = %q, %v", section, err)
	}
	if got := journal.tail(3); !equal(got, []string{"plan/mode=off"}) {
		t.Fatalf("approval records = %v", got)
	}
}

func TestService_CancelledExitKeepsPlanMode(t *testing.T) {
	service, _ := startPlan(t)
	journal := &memoryJournal{id: "s"}
	if _, err := service.Select(t.Context(), journal, true, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan struct{})
	done := make(chan error, 1)
	// Hold the session's lock so Exit waits where it checks the context.
	service.mu.Lock()
	locked := service.sessions["s"]
	service.mu.Unlock()
	locked.mu.Lock()
	go func() {
		close(entered)
		done <- service.Exit(ctx, "s")
	}()
	<-entered
	cancel()
	locked.mu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled exit = %v", err)
	}
	if section, err := service.Step(t.Context(), journal, 1); err != nil || section != Section {
		t.Fatalf("next boundary = %q, %v", section, err)
	}
	if len(journal.events) != 1 || !session.ProjectPlan(journal.events).Active {
		t.Fatalf("cancelled exit changed the journal: %+v", journal.events)
	}
}

func TestService_FailuresKeepTheSelection(t *testing.T) {
	service, _ := startPlan(t)
	failure := errors.New("read failure")
	broken := &memoryJournal{id: "s", eventsErr: failure}
	if _, err := service.Select(t.Context(), broken, true, false); !errors.Is(err, failure) {
		t.Fatalf("select read failure = %v", err)
	}
	if _, err := service.Step(t.Context(), broken, 1); !errors.Is(err, failure) {
		t.Fatalf("step read failure = %v", err)
	}
	journal := &memoryJournal{id: "s", failAt: 1}
	if _, err := service.Select(t.Context(), journal, true, false); err == nil || len(journal.events) != 0 {
		t.Fatalf("failed idle commit = %v, %v", err, journal.tail(0))
	}
	journal.header(1, 1)
	if change, err := service.Select(t.Context(), journal, true, true); err != nil || change != Queued {
		t.Fatalf("queued = %s, %v", change, err)
	}
	journal.failAt = 2
	if _, err := service.Step(t.Context(), journal, 2); err == nil {
		t.Fatal("failed boundary commit succeeded")
	}
	if service.Active("s") {
		t.Fatal("failed commit entered plan mode")
	}
	// The retry commits the mode; the notice fails and is retried at the next boundary.
	journal.failAt = 4
	if _, err := service.Step(t.Context(), journal, 2); err == nil {
		t.Fatal("failed notice succeeded")
	}
	if got := journal.tail(1); !equal(got, []string{"plan/mode=on"}) || !service.Active("s") {
		t.Fatalf("records after notice failure = %v", got)
	}
	if section, err := service.Step(t.Context(), journal, 2); err != nil || section != Section {
		t.Fatalf("notice retry = %q, %v", section, err)
	}
	if got := journal.tail(1); !equal(got, []string{"plan/mode=on", "user/message:" + enteredNotice}) {
		t.Fatalf("records after retry = %v", got)
	}
}

func TestService_SessionsDoNotWaitForEachOther(t *testing.T) {
	service, _ := startPlan(t)
	slow := &memoryJournal{id: "slow", entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := service.Step(context.Background(), slow, 1)
		done <- err
	}()
	<-slow.entered
	t.Cleanup(func() { close(slow.release) })
	fast := &memoryJournal{id: "fast"}
	finished := make(chan error, 1)
	go func() {
		_, err := service.Select(context.Background(), fast, true, false)
		if err == nil {
			_, err = service.Step(context.Background(), fast, 1)
		}
		finished <- err
	}()
	select {
	case err := <-finished:
		if err != nil || !service.Active("fast") {
			t.Fatalf("fast session = %v, active %t", err, service.Active("fast"))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a session waited for another session's journal read")
	}
	slow.release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestService_CleanupCancelsInFlightCalls(t *testing.T) {
	for _, test := range []struct {
		name string
		// queue first leaves a selection for the boundary to commit.
		queue bool
		call  func(*Service, *memoryJournal) error
	}{
		{"select", false, func(service *Service, journal *memoryJournal) error {
			_, err := service.Select(context.Background(), journal, true, false)
			return err
		}},
		{"step", true, func(service *Service, journal *memoryJournal) error {
			_, err := service.Step(context.Background(), journal, 1)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, scope := startPlan(t)
			journal := &memoryJournal{id: "s"}
			if test.queue {
				if change, err := service.Select(t.Context(), journal, true, true); err != nil || change != Queued {
					t.Fatalf("queue = %s, %v", change, err)
				}
			}
			journal.entered, journal.release = make(chan struct{}, 1), make(chan struct{})
			result := make(chan error, 1)
			go func() { result <- test.call(service, journal) }()
			<-journal.entered
			// Close runs while the call holds the session's lock inside its
			// journal read; nothing else will ever release that read.
			if err := scope.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			close(journal.release)
			err := <-result
			if journal.appends != 0 {
				t.Fatalf("an in-flight call committed after cleanup returned: %v", journal.tail(0))
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("in-flight call = %v", err)
			}
		})
	}
}

func TestService_CleanupWaitsForInFlightCalls(t *testing.T) {
	service, scope := startPlan(t)
	journal := &stubbornJournal{memoryJournal: &memoryJournal{id: "s"}, entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := service.Select(context.Background(), journal, true, false)
		result <- err
	}()
	<-journal.entered
	closed := make(chan error, 1)
	go func() { closed <- scope.Close(context.Background()) }()
	select {
	case <-journal.cancelled:
	case <-time.After(10 * time.Second):
		close(journal.release)
		t.Fatal("cleanup did not cancel the in-flight journal read")
	}
	if _, err := service.Select(t.Context(), &memoryJournal{id: "other"}, true, false); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("select during cleanup = %v", err)
	}
	select {
	case err := <-closed:
		t.Fatalf("cleanup returned with a call in flight: %v", err)
	default:
	}
	close(journal.release)
	if err := <-result; err != nil {
		t.Fatalf("in-flight select = %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	// The read that finished despite cancellation committed before cleanup returned.
	if got := journal.tail(0); !equal(got, []string{"plan/mode=on"}) {
		t.Fatalf("records = %v", got)
	}
}
