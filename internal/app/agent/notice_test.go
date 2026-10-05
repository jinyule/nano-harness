package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func noticeMessage(text string) session.Message {
	return session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "tool-jobs"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}
}

// userTexts lists committed user messages with the turn that owns them.
func userTexts(events []session.Event) []string {
	var texts []string
	for _, event := range events {
		if event.Record.Type == session.RecordUserMessage {
			texts = append(texts, strconv.FormatUint(event.Record.Turn, 10)+":"+session.Text(*event.Record.Message))
		}
	}
	return texts
}

func queuedNotices(batches ...[]session.Message) (func() []session.Message, *int) {
	calls := 0
	return func() []session.Message {
		calls++
		if calls > len(batches) {
			return nil
		}
		return batches[calls-1]
	}, &calls
}

func TestEngine_DeliversNoticesAtTurnStartBoundariesAndCompletion(t *testing.T) {
	call := session.ToolCall{ID: "call-1", Name: "inspect", Arguments: json.RawMessage(`{}`)}
	harness := startEngineHarness(t, 3,
		modelAction{completion: assistantCompletion("using tool", call)},
		modelAction{completion: assistantCompletion("first answer")},
		modelAction{completion: assistantCompletion("after notice")},
	)
	scope := &plugin.Scope{}
	if err := harness.tools.Register((&engineTool{name: "inspect", output: "ok"}).define(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	notices, calls := queuedNotices(
		[]session.Message{noticeMessage("at start")},
		[]session.Message{noticeMessage("at boundary")},
		[]session.Message{noticeMessage("at completion")},
	)
	journal, log := turnJournal()
	result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), notices: notices, drain: noMessages})
	if result.Outcome != session.OutcomeCompleted || result.Text != "after notice" || result.Err != nil || *calls != 3 {
		t.Fatalf("result = %+v, notice calls = %d", result, *calls)
	}
	want := []session.RecordType{
		session.RecordTurnStart, session.RecordUserMessage, session.RecordUserMessage,
		session.RecordStepStart, session.RecordRequestHeader, session.RecordAssistantMessage, session.RecordToolCall, session.RecordToolResult, session.RecordStepEnd,
		session.RecordUserMessage,
		session.RecordStepStart, session.RecordRequestHeader, session.RecordAssistantMessage, session.RecordStepEnd,
		session.RecordUserMessage,
		session.RecordStepStart, session.RecordRequestHeader, session.RecordAssistantMessage, session.RecordStepEnd,
		session.RecordTurnEnd,
	}
	if got := recordTypes(log.events); !slices.Equal(got, want) {
		t.Fatalf("record order = %#v", got)
	}
	if texts := userTexts(log.events); !slices.Equal(texts, []string{"1:go", "1:at start", "1:at boundary", "1:at completion"}) {
		t.Fatalf("user messages = %q", texts)
	}
	// The model saw the notice before its last answer.
	surface := harness.model.seen[2].Surface
	if last := surface[len(surface)-1].Message; last == nil || session.Text(*last) != "at completion" {
		t.Fatalf("final request surface ends with %#v", surface[len(surface)-1])
	}
}

func TestEngine_LastStepLeavesNoticesQueued(t *testing.T) {
	harness := startEngineHarness(t, 1, modelAction{completion: assistantCompletion("done")})
	notices, calls := queuedNotices(nil, []session.Message{noticeMessage("late")})
	journal, log := turnJournal()
	result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), notices: notices, drain: noMessages})
	if result.Outcome != session.OutcomeCompleted || *calls != 1 || countType(recordTypes(log.events), session.RecordUserMessage) != 1 {
		t.Fatalf("result = %+v, calls = %d, records = %#v", result, *calls, recordTypes(log.events))
	}
}

func TestEngine_NoticeAppendFailuresCloseTheTurn(t *testing.T) {
	failure := errors.New("injected")
	for _, test := range []struct {
		name    string
		batches [][]session.Message
		failAt  int
	}{
		{name: "turn start", batches: [][]session.Message{{noticeMessage("start")}}, failAt: 2},
		{name: "completion", batches: [][]session.Message{nil, {noticeMessage("completion")}}, failAt: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := startEngineHarness(t, 2, modelAction{completion: assistantCompletion("done")})
			notices, _ := queuedNotices(test.batches...)
			journal, log := turnJournal()
			log.appendHook = appendFailure(session.RecordUserMessage, test.failAt, failure)
			result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), notices: notices, drain: noMessages})
			if !errors.Is(result.Err, failure) || result.Outcome != session.OutcomeError {
				t.Fatalf("result = %+v", result)
			}
			if types := recordTypes(log.events); types[len(types)-1] != session.RecordTurnEnd {
				t.Fatalf("turn left open: %#v", types)
			}
		})
	}
}

func TestAgent_NotifyOpensTurnWhenIdle(t *testing.T) {
	harness := startEngineHarness(t, 2, modelAction{completion: assistantCompletion("noticed")})
	registry, _ := startRegistry(t, harness, newMemoryRepository(), newMemoryPolicy())
	root, err := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := root.Notify(session.Message{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid Notify() = %v", err)
	}
	if err := registry.Notify("missing", noticeMessage("x")); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("missing Notify() = %v", err)
	}
	if err := registry.Notify("root", noticeMessage("job finished")); err != nil {
		t.Fatal(err)
	}
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	status := root.Status()
	if status.Busy || status.Last.Turn != 1 || status.Last.Text != "noticed" || status.Last.Outcome != session.OutcomeCompleted {
		t.Fatalf("status = %+v", status)
	}
	events, _ := root.Events(context.Background())
	if texts := userTexts(events); !slices.Equal(texts, []string{"1:job finished"}) {
		t.Fatalf("user messages = %q", texts)
	}
	inactive := &Agent{}
	if err := inactive.Notify(noticeMessage("x")); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive Notify() = %v", err)
	}
}

func TestAgent_NotifyWhileBusyExtendsTheActiveTurn(t *testing.T) {
	started, gate := make(chan struct{}, 1), make(chan struct{})
	harness := startEngineHarness(t, 3,
		modelAction{started: started, wait: gate, completion: assistantCompletion("first")},
		modelAction{completion: assistantCompletion("second")},
	)
	registry, _ := startRegistry(t, harness, newMemoryRepository(), newMemoryPolicy())
	root, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	results, err := root.Submit(context.Background(), agentMessage(session.RoleUser, "start"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := root.Notify(noticeMessage("done meanwhile")); err != nil {
		t.Fatal(err)
	}
	close(gate)
	if result := <-results; result.Turn != 1 || result.Text != "second" || result.Outcome != session.OutcomeCompleted {
		t.Fatalf("result = %+v", result)
	}
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := root.Events(context.Background())
	if texts := userTexts(events); !slices.Equal(texts, []string{"1:start", "1:done meanwhile"}) || root.Status().Last.Turn != 1 {
		t.Fatalf("user messages = %q, status = %+v", texts, root.Status())
	}
}

func TestAgent_NoticeAfterLastStepOpensNextTurnUnlessQueued(t *testing.T) {
	started, gate := make(chan struct{}, 1), make(chan struct{})
	harness := startEngineHarness(t, 1,
		modelAction{started: started, wait: gate, completion: assistantCompletion("first")},
		modelAction{completion: assistantCompletion("woken")},
	)
	registry, _ := startRegistry(t, harness, newMemoryRepository(), newMemoryPolicy())
	root, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	results, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "start"))
	<-started
	if err := root.Notify(noticeMessage("late")); err != nil {
		t.Fatal(err)
	}
	close(gate)
	<-results
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := root.Events(context.Background())
	if texts := userTexts(events); !slices.Equal(texts, []string{"1:start", "2:late"}) || root.Status().Last.Text != "woken" {
		t.Fatalf("user messages = %q, status = %+v", texts, root.Status())
	}

	// A queued turn delivers pending notices at its start instead.
	started, gate = make(chan struct{}, 1), make(chan struct{})
	harness.model.mu.Lock()
	harness.model.actions = []modelAction{
		{started: started, wait: gate, completion: assistantCompletion("third")},
		{completion: assistantCompletion("fourth")},
	}
	harness.model.mu.Unlock()
	first, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "three"))
	<-started
	second, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "four"))
	if err := root.Notify(noticeMessage("queued")); err != nil {
		t.Fatal(err)
	}
	close(gate)
	<-first
	if result := <-second; result.Text != "fourth" {
		t.Fatalf("second result = %+v", result)
	}
	events, _ = root.Events(context.Background())
	if texts := userTexts(events); !slices.Equal(texts[2:], []string{"3:three", "4:four", "4:queued"}) {
		t.Fatalf("user messages = %q", texts)
	}
}

func TestAgent_NoticesFromCanceledTurnWaitForNextTurn(t *testing.T) {
	started := make(chan struct{}, 1)
	harness := startEngineHarness(t, 1,
		modelAction{started: started, wait: make(chan struct{})},
		modelAction{completion: assistantCompletion("resumed")},
	)
	registry, _ := startRegistry(t, harness, newMemoryRepository(), newMemoryPolicy())
	root, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	results, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "block"))
	<-started
	if err := root.Notify(noticeMessage("pending")); err != nil {
		t.Fatal(err)
	}
	root.Interrupt()
	if result := <-results; result.Outcome != session.OutcomeCanceled {
		t.Fatalf("interrupted = %+v", result)
	}
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := root.Status(); status.Busy || status.Last.Turn != 1 {
		t.Fatalf("interrupt opened a notice turn: %+v", status)
	}
	next, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "next"))
	if result := <-next; result.Text != "resumed" {
		t.Fatalf("next = %+v", result)
	}
	events, _ := root.Events(context.Background())
	if texts := userTexts(events); !slices.Equal(texts, []string{"1:block", "2:next", "2:pending"}) {
		t.Fatalf("user messages = %q", texts)
	}
}
