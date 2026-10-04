package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func todoRecord(items []TodoItem) Record {
	return Record{Type: RecordTodoWrite, Turn: 1, Step: 1, Todo: &TodoWrite{CallID: "call", Items: items}}
}

func TestTodoWrite_ValidatesWholeListSnapshots(t *testing.T) {
	maximal := make([]TodoItem, MaxTodoItems)
	for index := range maximal {
		maximal[index] = TodoItem{Content: fmt.Sprintf("task %d", index), Status: TodoInProgress}
	}
	for name, items := range map[string][]TodoItem{
		"empty list clears": {},
		"every status":      {{Content: "plan", Status: TodoCompleted}, {Content: "build", Status: TodoInProgress}, {Content: "ship", Status: TodoPending}},
		"parallel active":   {{Content: "a", Status: TodoInProgress}, {Content: "b", Status: TodoInProgress}},
		"maximal list":      maximal,
		"maximal content":   {{Content: strings.Repeat("x", MaxTodoContentBytes), Status: TodoPending}},
	} {
		if err := todoRecord(items).Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTodoWrite_RejectsInvalidShapesAndItems(t *testing.T) {
	valid := []TodoItem{{Content: "plan", Status: TodoPending}}
	for name, test := range map[string]struct {
		record  Record
		message string
	}{
		"missing step":     {Record{Type: RecordTodoWrite, Turn: 1, Todo: &TodoWrite{CallID: "call", Items: valid}}, "shape"},
		"missing payload":  {Record{Type: RecordTodoWrite, Turn: 1, Step: 1}, "shape"},
		"null items":       {todoRecord(nil), "shape"},
		"unrelated field":  {Record{Type: RecordTodoWrite, Turn: 1, Step: 1, Todo: &TodoWrite{CallID: "call", Items: valid}, Result: &ToolResult{CallID: "call"}}, "shape"},
		"missing turn":     {Record{Type: RecordTodoWrite, Step: 1, Todo: &TodoWrite{CallID: "call", Items: valid}}, "turn"},
		"invalid call ID":  {Record{Type: RecordTodoWrite, Turn: 1, Step: 1, Todo: &TodoWrite{CallID: " call", Items: valid}}, "call ID"},
		"too many":         {todoRecord(make([]TodoItem, MaxTodoItems+1)), "at most 256 todos are allowed (got 257)"},
		"empty content":    {todoRecord([]TodoItem{{Status: TodoPending}}), "`content` must be a non-empty string"},
		"untrimmed":        {todoRecord([]TodoItem{{Content: " plan", Status: TodoPending}}), "`content` must be trimmed"},
		"oversize content": {todoRecord([]TodoItem{{Content: strings.Repeat("x", MaxTodoContentBytes+1), Status: TodoPending}}), "exceeds 2048 bytes"},
		"unknown status":   {todoRecord([]TodoItem{{Content: "plan", Status: "doing"}}), `unknown status "doing"`},
		"missing status":   {todoRecord([]TodoItem{{Content: "plan"}}), `unknown status ""`},
		"duplicate":        {todoRecord([]TodoItem{{Content: "dup", Status: TodoPending}, {Content: "dup", Status: TodoCompleted}}), `duplicate content "dup"`},
	} {
		err := test.record.Validate()
		if !errors.Is(err, ErrInvalidRecord) || !strings.Contains(err.Error(), test.message) {
			t.Errorf("%s: got %v, want %q", name, err, test.message)
		}
	}
	for _, record := range []Record{
		{Type: RecordTurnStart, Turn: 1, Todo: &TodoWrite{CallID: "call", Items: valid}},
		{Type: RecordToolResult, Turn: 1, Step: 1, Result: &ToolResult{CallID: "call"}, Todo: &TodoWrite{CallID: "call", Items: valid}},
	} {
		if err := record.Validate(); !errors.Is(err, ErrInvalidRecord) {
			t.Errorf("%s accepted a todo payload: %v", record.Type, err)
		}
	}
}

func TestValidateTodoItems_ReturnsModelVisibleMessages(t *testing.T) {
	err := ValidateTodoItems([]TodoItem{{Content: "a", Status: TodoPending}, {Content: "a", Status: TodoPending}})
	if err == nil || err.Error() != `invalid todos: duplicate content "a"` || errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("duplicate error = %v", err)
	}
	if err := ValidateTodoItems([]TodoItem{{Status: TodoPending}}); err == nil || err.Error() != "invalid todo: `content` must be a non-empty string" {
		t.Fatalf("empty error = %v", err)
	}
}

func TestStandingTodos_FoldsLatestWriteUntilNextTurn(t *testing.T) {
	first := []TodoItem{{Content: "plan", Status: TodoInProgress}}
	second := []TodoItem{{Content: "plan", Status: TodoCompleted}, {Content: "build", Status: TodoInProgress}}
	var plan []TodoItem
	steps := []struct {
		record Record
		want   []TodoItem
	}{
		{Record{Type: RecordTurnStart, Turn: 1}, nil},
		{todoRecord(first), first},
		{Record{Type: RecordToolResult, Turn: 1, Step: 1, Result: &ToolResult{CallID: "call"}}, first},
		{todoRecord(second), second},
		{Record{Type: RecordTurnEnd, Turn: 1, Outcome: OutcomeCompleted}, second},
		{Record{Type: RecordTurnStart, Turn: 2}, nil},
		{todoRecord([]TodoItem{}), []TodoItem{}},
	}
	for index, step := range steps {
		plan = StandingTodos(plan, step.record)
		if !slices.Equal(plan, step.want) || (plan == nil) != (step.want == nil) {
			t.Fatalf("step %d plan = %#v, want %#v", index, plan, step.want)
		}
	}
	source := todoRecord([]TodoItem{{Content: "owned", Status: TodoPending}})
	folded := StandingTodos(nil, source)
	folded[0].Content = "changed"
	if source.Todo.Items[0].Content != "owned" {
		t.Fatal("StandingTodos aliases the committed record")
	}
}

func TestTodoWrite_IsDurableUIStateOutsideTheSurface(t *testing.T) {
	call := &ToolCall{ID: "call", Name: "todo_write", Arguments: json.RawMessage(`{"todos":[]}`)}
	events := []Event{
		{Sequence: 1, Record: Record{Type: RecordAssistantMessage, Turn: 1, Step: 1, Message: textMessage(RoleAssistant, "planning")}},
		{Sequence: 2, Record: Record{Type: RecordToolCall, Turn: 1, Step: 1, Call: call}},
		{Sequence: 3, Record: todoRecord([]TodoItem{{Content: "plan", Status: TodoPending}})},
		{Sequence: 4, Record: Record{Type: RecordToolResult, Turn: 1, Step: 1, Result: &ToolResult{CallID: "call", Output: "Updated todo list: 1 pending, 0 in progress, 0 completed."}}},
	}
	surface, err := Surface(events)
	if err != nil || len(surface) != 3 || surface[1].Call == nil || surface[2].Result == nil {
		t.Fatalf("surface = %#v err=%v", surface, err)
	}
	cloned := CloneEvent(events[2])
	cloned.Record.Todo.Items[0].Content = "changed"
	cloned.Record.Todo.CallID = "changed"
	if events[2].Record.Todo.Items[0].Content != "plan" || events[2].Record.Todo.CallID != "call" {
		t.Fatal("CloneEvent aliases the todo payload")
	}
	encoded, err := json.Marshal(todoRecord([]TodoItem{}))
	if err != nil || string(encoded) != `{"type":"todo/write","turn":1,"step":1,"todo":{"call_id":"call","items":[]}}` {
		t.Fatalf("encoded = %s err=%v", encoded, err)
	}
}
