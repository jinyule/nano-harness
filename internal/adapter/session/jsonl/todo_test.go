package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

var fixtureTodos = []coresession.TodoItem{
	{Content: "write tests", Status: coresession.TodoCompleted},
	{Content: "implement", Status: coresession.TodoInProgress},
	{Content: "document", Status: coresession.TodoPending},
}

func standingTodos(events []coresession.Event) []coresession.TodoItem {
	var plan []coresession.TodoItem
	for _, event := range events {
		plan = coresession.StandingTodos(plan, event.Record)
	}
	return plan
}

func todoWrite(turn, step uint64, callID string, items []coresession.TodoItem) coresession.Record {
	return coresession.Record{Type: coresession.RecordTodoWrite, Turn: turn, Step: step, Todo: &coresession.TodoWrite{CallID: callID, Items: items}}
}

func todoCall(step uint64, id string) coresession.Record {
	return coresession.Record{Type: coresession.RecordToolCall, Turn: 1, Step: step, Call: &coresession.ToolCall{ID: id, Name: "todo_write", Arguments: json.RawMessage(`{"todos":[]}`)}}
}

func TestSessionV2Todo_FrozenContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-todo.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	manager, scope := startManager(t)
	t.Cleanup(func() {
		if err := scope.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(manager.config.Root, "fixture.jsonl")
	if err := os.WriteFile(path, fixture, 0o600); err != nil { //nolint:gosec // fixed fixture name under the test-owned private manager root
		t.Fatal(err)
	}
	_, events, err := manager.Inspect(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 14 || events[6].Record.Type != coresession.RecordTodoWrite || !slices.Equal(standingTodos(events), fixtureTodos) {
		t.Fatalf("events=%v plan=%v", events, standingTodos(events))
	}
	surface, err := coresession.Surface(events)
	if err != nil || len(surface) != 5 || surface[2].Call.Name != "todo_write" || surface[3].Result.CallID != "call-todo" {
		t.Fatalf("surface=%+v err=%v", surface, err)
	}
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "fixture", Cwd: "/synthetic/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // fixed fixture name under the test-owned private manager root
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, fixture) {
		t.Fatal("read and closed resume changed a committed todo fixture")
	}

	// The writer uses independently constructed records, never decoded fixture values.
	output := temporaryFile(t)
	header := coresession.Header{SessionID: "fixture", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/synthetic/workspace"}
	if _, err := writeHeader(output, header); err != nil {
		t.Fatal(err)
	}
	writer := &Log{file: output, header: header, active: true, size: int64(bytes.IndexByte(fixture, '\n') + 1)}
	arguments := json.RawMessage(`{"todos":[{"content":"write tests","status":"completed"},{"content":"implement","status":"in_progress"},{"content":"document","status":"pending"}]}`)
	items := []coresession.TodoItem{
		{Content: "write tests", Status: coresession.TodoCompleted},
		{Content: "implement", Status: coresession.TodoInProgress},
		{Content: "document", Status: coresession.TodoPending},
	}
	for _, record := range []coresession.Record{
		{Type: coresession.RecordTurnStart, Turn: 1},
		{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("ship it")},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 1},
		{Type: coresession.RecordRequestHeader, Turn: 1, Step: 1, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("planning")},
		{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: "call-todo", Name: "todo_write", Arguments: arguments}},
		todoWrite(1, 1, "call-todo", items),
		{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &coresession.ToolResult{CallID: "call-todo", Output: "Updated todo list: 1 pending, 1 in progress, 1 completed."}},
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 1},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 2},
		{Type: coresession.RecordRequestHeader, Turn: 1, Step: 2, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 2, Message: assistantMessage("done")},
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 2},
		{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted},
	} {
		appendRecord(t, writer, record)
	}
	actual, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, fixture) {
		t.Fatalf("writer differs from frozen v2 todo fixture:\n%s", actual)
	}
}

func TestSessionV2Todo_RejectsChangedContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-todo.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	const items = `"items":[{"content":"write tests","status":"completed"},{"content":"implement","status":"in_progress"},{"content":"document","status":"pending"}]`
	for _, test := range []struct{ name, from, to string }{
		{"unknown-todo-field", `"call_id":"call-todo","items"`, `"call_id":"call-todo","priority":1,"items"`},
		{"unknown-item-field", `"items":[{"content":"write tests"`, `"items":[{"id":1,"content":"write tests"`},
		{"unknown-status", `"items":[{"content":"write tests","status":"completed"}`, `"items":[{"content":"write tests","status":"done"}`},
		{"untrimmed-content", `"items":[{"content":"write tests"`, `"items":[{"content":" write tests"`},
		{"duplicate-content", `"items":[{"content":"write tests","status":"completed"},{"content":"implement"`, `"items":[{"content":"write tests","status":"completed"},{"content":"write tests"`},
		{"null-items", items, `"items":null`},
		{"missing-items", `,` + items, ``},
		{"no-pending-call", `"todo":{"call_id":"call-todo"`, `"todo":{"call_id":"call-other"`},
		{"outside-step", `"type":"todo/write","turn":1,"step":1`, `"type":"todo/write","turn":1,"step":2`},
		{"outside-turn", `"type":"todo/write","turn":1,"step":1`, `"type":"todo/write","turn":2,"step":1`},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := bytes.Replace(fixture, []byte(test.from), []byte(test.to), 1)
			if bytes.Equal(changed, fixture) {
				t.Fatalf("replacement %q did not change the fixture", test.from)
			}
			file := temporaryFile(t)
			if _, err := file.Write(changed); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSession(file, testCompositionID); !errors.Is(err, ErrCorruptSession) {
				t.Fatalf("got %v, want %v", err, ErrCorruptSession)
			}
		})
	}
}

func TestValidateOrder_BindsTodoWriteToOnePendingCall(t *testing.T) {
	call := addOrder(withAssistant(orderPrefix()), todoCall(1, "call"))
	written := addOrder(call, todoWrite(1, 1, "call", fixtureTodos))
	result := addOrder(written, coresession.Record{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &coresession.ToolResult{CallID: "call"}})
	for name, events := range map[string][]coresession.Event{
		"before call":    addOrder(withAssistant(orderPrefix()), todoWrite(1, 1, "call", fixtureTodos)),
		"other step":     addOrder(call, todoWrite(1, 2, "call", fixtureTodos)),
		"other turn":     addOrder(call, todoWrite(2, 1, "call", fixtureTodos)),
		"unknown call":   addOrder(call, todoWrite(1, 1, "other", fixtureTodos)),
		"duplicate":      addOrder(written, todoWrite(1, 1, "call", nil)),
		"after result":   addOrder(result, todoWrite(1, 1, "call", fixtureTodos)),
		"outside a step": addOrder(orderPrefix()[:2], todoWrite(1, 0, "call", fixtureTodos)),
	} {
		if _, err := validateOrder(events, false); !errors.Is(err, ErrCorruptSession) {
			t.Errorf("%s: got %v", name, err)
		}
	}

	// A provider may reuse a call ID after its result; the new pending call may write once more.
	reused := addOrder(result, coresession.Record{Type: coresession.RecordStepEnd, Turn: 1, Step: 1})
	reused = addOrder(reused, coresession.Record{Type: coresession.RecordStepStart, Turn: 1, Step: 2})
	reused = addOrder(reused, coresession.Record{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 2, Message: assistantMessage("again")})
	reused = addOrder(reused, todoCall(2, "call"))
	reused = addOrder(reused, todoWrite(1, 2, "call", []coresession.TodoItem{}))
	reused = addOrder(reused, coresession.Record{Type: coresession.RecordToolResult, Turn: 1, Step: 2, Result: &coresession.ToolResult{CallID: "call"}})
	reused = addOrder(reused, coresession.Record{Type: coresession.RecordStepEnd, Turn: 1, Step: 2})
	reused = addOrder(reused, coresession.Record{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted})
	if _, err := validateOrder(reused, true); err != nil {
		t.Fatal(err)
	}
	if plan := standingTodos(reused); plan == nil || len(plan) != 0 {
		t.Fatalf("empty replacement plan = %#v", plan)
	}
}

func TestLog_TodoWriteSurvivesAppendResumeAndInterruptedRepair(t *testing.T) {
	manager, scope := startManager(t)
	t.Cleanup(func() {
		if err := scope.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	root := t.TempDir()
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "todo", Create: true, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("plan")})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordStepStart, Turn: 1, Step: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("writing")})
	appendRecord(t, log, todoCall(1, "call"))
	if _, err := log.Append(t.Context(), todoWrite(1, 1, "missing", fixtureTodos)); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("append for a missing call = %v", err)
	}
	if _, err := log.Append(t.Context(), todoWrite(1, 1, "call", []coresession.TodoItem{{Content: "dup", Status: coresession.TodoPending}, {Content: "dup", Status: coresession.TodoPending}})); !errors.Is(err, coresession.ErrInvalidRecord) {
		t.Fatalf("append for an invalid list = %v", err)
	}
	committed := appendRecord(t, log, todoWrite(1, 1, "call", fixtureTodos))
	committed.Record.Todo.Items[0].Content = "changed"
	if events, err := log.Events(t.Context()); err != nil || !slices.Equal(standingTodos(events), fixtureTodos) {
		t.Fatalf("committed event aliases the log: %v", err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}

	// The crash happened after todo/write but before tool/result: repair keeps the committed list.
	resumed, err := manager.Open(t.Context(), OpenOptions{SessionID: "todo", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	events, err := resumed.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1].Record
	if len(events) != 9 || events[6].Record.Type != coresession.RecordToolResult || !events[6].Record.Result.IsError || last.Outcome != coresession.OutcomeInterrupted {
		t.Fatalf("repaired events = %#v", events)
	}
	if plan := standingTodos(events); !slices.Equal(plan, fixtureTodos) {
		t.Fatalf("resumed plan = %#v", plan)
	}
	appendRecord(t, resumed, coresession.Record{Type: coresession.RecordTurnStart, Turn: 2})
	events, err = resumed.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if plan := standingTodos(events); plan != nil {
		t.Fatalf("plan survived the next turn: %#v", plan)
	}
	if err := resumed.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}
