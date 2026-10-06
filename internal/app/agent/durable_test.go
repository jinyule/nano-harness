package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// deliveredNotices lists the notice IDs user messages delivered, by turn.
func deliveredNotices(events []session.Event) []string {
	var delivered []string
	for _, event := range events {
		if record := event.Record; record.Type == session.RecordUserMessage && record.Message.Source.NoticeID != "" {
			delivered = append(delivered, strconv.FormatUint(record.Turn, 10)+":"+record.Message.Source.NoticeID)
		}
	}
	return delivered
}

func committedNotices(events []session.Event) []string {
	var queued []string
	for _, event := range events {
		if event.Record.Type == session.RecordNoticeQueued {
			queued = append(queued, event.Record.Message.Source.NoticeID)
		}
	}
	return queued
}

func durableHarness(t *testing.T, maxSteps int, actions ...modelAction) (*engineHarness, *Registry, *memoryRepository) {
	t.Helper()
	harness := startEngineHarness(t, maxSteps, actions...)
	repository := newMemoryRepository()
	registry, _ := startRegistry(t, harness, repository, newMemoryPolicy())
	return harness, registry, repository
}

func TestAgent_QueueNoticeCommitsBeforeDelivering(t *testing.T) {
	_, registry, repository := durableHarness(t, 2, modelAction{completion: assistantCompletion("noticed")})
	root, err := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	identified := noticeMessage("x")
	identified.Source.NoticeID = "notice-9"
	for name, call := range map[string]func() error{
		"invalid":          func() error { return root.QueueNotice(context.Background(), session.Message{}) },
		"preset ID":        func() error { return root.QueueNotice(context.Background(), identified) },
		"notify preset ID": func() error { return root.Notify(identified) },
	} {
		if err := call(); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s = %v", name, err)
		}
	}
	if err := registry.QueueNotice(context.Background(), "missing", noticeMessage("x")); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("missing = %v", err)
	}
	// The notice is committed while nothing is queued yet.
	log := repository.logs["root"]
	queuedWhileCommitting := -1
	log.appendHook = func(record session.Record) error {
		if record.Type == session.RecordNoticeQueued {
			root.mu.Lock()
			queuedWhileCommitting = len(root.notices)
			root.mu.Unlock()
		}
		return nil
	}
	if err := registry.QueueNotice(context.Background(), "root", noticeMessage("job finished")); err != nil {
		t.Fatal(err)
	}
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := root.Events(context.Background())
	if queuedWhileCommitting != 0 || !slices.Equal(committedNotices(events), []string{"notice-1"}) || !slices.Equal(deliveredNotices(events), []string{"1:notice-1"}) {
		t.Fatalf("queued while committing = %d, events = %q / %q", queuedWhileCommitting, committedNotices(events), deliveredNotices(events))
	}
	if pending := session.PendingNotices(events); len(pending) != 0 || root.Status().Last.Text != "noticed" {
		t.Fatalf("pending = %#v, status = %+v", pending, root.Status())
	}
}

func TestAgent_QueueNoticeFailuresCommitNothing(t *testing.T) {
	_, registry, repository := durableHarness(t, 2, modelAction{completion: assistantCompletion("noticed")})
	root, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	oneShot, _ := registry.Create(context.Background(), CreateRequest{SessionID: "once", ParentID: "root", Depth: 1, Mode: "one-shot", Create: true})
	if err := oneShot.QueueNotice(context.Background(), noticeMessage("late")); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("idle one-shot = %v", err)
	}
	if err := (&Agent{}).QueueNotice(context.Background(), noticeMessage("x")); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive = %v", err)
	}
	log := repository.logs["root"]
	failure := errors.New("disk full")
	log.eventsErr = failure
	if err := root.QueueNotice(context.Background(), noticeMessage("x")); !errors.Is(err, failure) {
		t.Fatalf("events failure = %v", err)
	}
	log.eventsErr = nil
	log.appendHook = appendFailure(session.RecordNoticeQueued, 1, failure)
	if err := root.QueueNotice(context.Background(), noticeMessage("x")); !errors.Is(err, failure) {
		t.Fatalf("append failure = %v", err)
	}
	root.mu.Lock()
	queued := len(root.notices)
	root.mu.Unlock()
	if queued != 0 {
		t.Fatalf("failed commit queued %d notices", queued)
	}
	// The next notice takes the first ID: the failed one left no record.
	if err := root.QueueNotice(context.Background(), noticeMessage("x")); err != nil {
		t.Fatal(err)
	}
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := root.Events(context.Background())
	once, _ := oneShot.Events(context.Background())
	if !slices.Equal(committedNotices(events), []string{"notice-1"}) || len(committedNotices(once)) != 0 {
		t.Fatalf("queued = %q / %q", committedNotices(events), committedNotices(once))
	}
}

// TestAgent_OwedNoticeIsDeliveredOnceAfterRestart interrupts the turn a
// notice arrived during, closes the agent, and resumes the session: the
// owed notice opens no turn on its own and the next turn delivers it once.
func TestAgent_OwedNoticeIsDeliveredOnceAfterRestart(t *testing.T) {
	started := make(chan struct{}, 1)
	harness, registry, _ := durableHarness(t, 2,
		modelAction{started: started, wait: make(chan struct{})},
		modelAction{completion: assistantCompletion("resumed")},
		modelAction{completion: assistantCompletion("later")},
	)
	root, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	first, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "block"))
	<-started
	if err := root.QueueNotice(context.Background(), noticeMessage("job finished")); err != nil {
		t.Fatal(err)
	}
	root.Interrupt()
	if result := <-first; result.Outcome != session.OutcomeCanceled {
		t.Fatalf("interrupted = %+v", result)
	}
	if err := registry.Close(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}

	resumed, err := registry.Create(context.Background(), CreateRequest{SessionID: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	harness.model.mu.Lock()
	calls := len(harness.model.seen)
	harness.model.mu.Unlock()
	if status := resumed.Status(); status.Busy || calls != 1 {
		t.Fatalf("resume opened a turn: %+v, model calls = %d", status, calls)
	}
	next, _ := resumed.Submit(context.Background(), agentMessage(session.RoleUser, "next"))
	if result := <-next; result.Text != "resumed" {
		t.Fatalf("next = %+v", result)
	}
	events, _ := resumed.Events(context.Background())
	if texts := userTexts(events); !slices.Equal(texts, []string{"1:block", "2:next", "2:job finished"}) || !slices.Equal(deliveredNotices(events), []string{"2:notice-1"}) {
		t.Fatalf("user messages = %q, delivered = %q", texts, deliveredNotices(events))
	}

	// A second restart owes nothing and delivers nothing again.
	if err := registry.Close(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}
	again, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root"})
	later, _ := again.Submit(context.Background(), agentMessage(session.RoleUser, "again"))
	<-later
	events, _ = again.Events(context.Background())
	if !slices.Equal(deliveredNotices(events), []string{"2:notice-1"}) || len(session.PendingNotices(events)) != 0 {
		t.Fatalf("delivered = %q", deliveredNotices(events))
	}
}

func TestAgent_DurableNoticeAtLastStepWakesTheNextTurn(t *testing.T) {
	block := session.ToolCall{ID: "call-block", Name: "block", Arguments: json.RawMessage(`{}`)}
	harness, registry, _ := durableHarness(t, 1,
		modelAction{completion: assistantCompletion("last step", block)},
		modelAction{completion: assistantCompletion("answered")},
	)
	started, release := make(chan struct{}, 1), make(chan struct{})
	tool := appTool.Define(appTool.Spec[engineArguments]{
		Name: "block", Description: "waits for release", Parameters: appTool.Parameters{appTool.Optional("value", appTool.Number(""))},
		Execute: func(context.Context, appTool.Invocation, engineArguments) (appTool.Result, error) {
			started <- struct{}{}
			<-release
			return appTool.Text("done"), nil
		},
	})
	scope := &plugin.Scope{}
	if err := harness.tools.Register(tool, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	root, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	results, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "start"))
	<-started
	if err := root.QueueNotice(context.Background(), noticeMessage("late")); err != nil {
		t.Fatal(err)
	}
	close(release)
	if result := <-results; result.Outcome != session.OutcomeStepLimit {
		t.Fatalf("first = %+v", result)
	}
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := root.Events(context.Background())
	if !slices.Equal(deliveredNotices(events), []string{"2:notice-1"}) || root.Status().Last.Text != "answered" {
		t.Fatalf("delivered = %q, status = %+v", deliveredNotices(events), root.Status())
	}
}

// TestAgent_ForkOwesNoneOfItsParentsNotices forks a parent whose completed
// prefix holds an owed notice: the child queues nothing for it and numbers
// its own notices after the inherited ones.
func TestAgent_ForkOwesNoneOfItsParentsNotices(t *testing.T) {
	started := make(chan struct{}, 1)
	_, registry, _ := durableHarness(t, 2, modelAction{started: started, wait: make(chan struct{})})
	root, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	first, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "block"))
	<-started
	if err := root.QueueNotice(context.Background(), noticeMessage("parent job finished")); err != nil {
		t.Fatal(err)
	}
	root.Interrupt()
	<-first
	events, _ := root.Events(context.Background())
	seed := events
	for len(seed) > 0 && seed[len(seed)-1].Record.Type != session.RecordTurnEnd {
		seed = seed[:len(seed)-1]
	}
	if owed := session.PendingNotices(seed); len(owed) != 1 {
		t.Fatalf("seed owes %d notices", len(owed))
	}
	child, err := registry.Create(context.Background(), CreateRequest{SessionID: "fork", ParentID: "root", Label: "fork", Mode: "continuable", Provider: session.SubagentFork, Seed: seed, Depth: 1, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	child.mu.Lock()
	childOwed := len(child.notices)
	child.mu.Unlock()
	root.mu.Lock()
	rootOwed := len(root.notices)
	root.mu.Unlock()
	childEvents, _ := child.Events(context.Background())
	if childOwed != 0 || rootOwed != 1 || session.NoticeID(childEvents) != "notice-2" {
		t.Fatalf("child owes %d, root owes %d, next child ID %q", childOwed, rootOwed, session.NoticeID(childEvents))
	}
}
