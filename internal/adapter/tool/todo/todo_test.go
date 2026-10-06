package todo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type allowApprover struct{}

func (allowApprover) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalAllowedOnce, nil
}

// recordingJournal validates every record like a durable log and keeps the committed order.
type recordingJournal struct {
	mu      sync.Mutex
	records []session.Record
	err     error
}

func (journal *recordingJournal) Append(ctx context.Context, record session.Record) (session.Event, error) {
	if err := ctx.Err(); err != nil {
		return session.Event{}, err
	}
	if journal.err != nil {
		return session.Event{}, journal.err
	}
	if err := record.Validate(); err != nil {
		return session.Event{}, err
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.records = append(journal.records, record)
	return session.Event{Sequence: uint64(len(journal.records)), Record: record}, nil
}

func (journal *recordingJournal) written() []session.Record {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return slices.Clone(journal.records)
}

func startTools(t *testing.T) *appTool.Runtime {
	t.Helper()
	runtime, err := appTool.New(allowApprover{})
	if err != nil {
		t.Fatal(err)
	}
	runtimeScope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	provider, err := New(runtime)
	if err != nil {
		t.Fatal(err)
	}
	providerScope := &plugin.Scope{}
	if err := provider.Start(context.Background(), providerScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(providerScope.Close(context.Background()), runtimeScope.Close(context.Background())); err != nil {
			t.Error(err)
		}
	})
	return runtime
}

func execute(t *testing.T, runtime *appTool.Runtime, journal appTool.Journal, arguments ...string) []session.ToolResult {
	t.Helper()
	calls := make([]session.ToolCall, len(arguments))
	for index, raw := range arguments {
		calls[index] = session.ToolCall{ID: "call-" + string(rune('a'+index)), Name: toolName, Arguments: json.RawMessage(raw)}
	}
	return runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{SessionID: "session", Cwd: t.TempDir(), Turn: 3, Step: 2, Calls: calls, Journal: journal})
}

func TestNew_RequiresToolRuntime(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil runtime = %v", err)
	}
}

func TestProvider_RegistersTodoWriteForScopeLifetime(t *testing.T) {
	runtime, err := appTool.New(allowApprover{})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := New(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if provider.ID() != "todo-tools" {
		t.Fatalf("ID = %q", provider.ID())
	}
	if err := provider.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, appTool.ErrNotRunning) {
		t.Fatalf("start before runtime = %v", err)
	}
	runtimeScope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeScope.Close(context.Background()) })
	closed := &plugin.Scope{}
	if err := closed.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("start in closed scope = %v", err)
	}
	if catalog, _ := runtime.Catalog(nil); len(catalog.Definitions) != 0 {
		t.Fatalf("failed start leaked %v", catalog.Definitions)
	}
	scope := &plugin.Scope{}
	if err := provider.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	catalog, err := runtime.Catalog(nil)
	if err != nil || len(catalog.Definitions) != 1 || catalog.Definitions[0].Name != toolName || len(catalog.Guidance) != 0 {
		t.Fatalf("catalog = %+v err=%v", catalog, err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if catalog, _ := runtime.Catalog(nil); len(catalog.Definitions) != 0 {
		t.Fatalf("closed scope left %v", catalog.Definitions)
	}
	if results := execute(t, runtime, &recordingJournal{}, `{"todos":[]}`); !results[0].IsError || !strings.Contains(results[0].Output, "unknown tool") {
		t.Fatalf("closed tool executed: %+v", results)
	}
}

func TestWriteTool_AppendsWholeListSnapshotsInCallOrder(t *testing.T) {
	runtime := startTools(t)
	journal := &recordingJournal{}
	results := execute(t, runtime, journal,
		`{"todos":[{"content":"  plan the work  ","status":"completed"},{"content":"run subagent a","status":"in_progress"},{"content":"run subagent b","status":"in_progress"},{"content":"merge","status":"pending"}]}`,
		`{"todos":[]}`,
	)
	want := []string{"Updated todo list: 1 pending, 2 in progress, 1 completed.", "Updated todo list: 0 pending, 0 in progress, 0 completed."}
	for index, result := range results {
		if result.IsError || result.Output != want[index] || result.CallID != "call-"+string(rune('a'+index)) {
			t.Fatalf("result %d = %+v", index, result)
		}
	}
	written := journal.written()
	if len(written) != 2 {
		t.Fatalf("written = %+v", written)
	}
	first := written[0]
	expected := []session.TodoItem{
		{Content: "plan the work", Status: session.TodoCompleted},
		{Content: "run subagent a", Status: session.TodoInProgress},
		{Content: "run subagent b", Status: session.TodoInProgress},
		{Content: "merge", Status: session.TodoPending},
	}
	if first.Type != session.RecordTodoWrite || first.Turn != 3 || first.Step != 2 || first.Todo.CallID != "call-a" || !slices.Equal(first.Todo.Items, expected) {
		t.Fatalf("first snapshot = %+v", first)
	}
	if second := written[1]; second.Todo.CallID != "call-b" || second.Todo.Items == nil || len(second.Todo.Items) != 0 {
		t.Fatalf("clearing snapshot = %+v", second)
	}
}

func TestWriteTool_RejectsInvalidListsWithoutWriting(t *testing.T) {
	runtime := startTools(t)
	journal := &recordingJournal{}
	for name, test := range map[string]struct{ arguments, message string }{
		"missing todos":     {`{}`, `invalid arguments: missing required property "todos"`},
		"todos not array":   {`{"todos":"nope"}`, `invalid arguments: "todos" must be an array`},
		"unknown root key":  {`{"todos":[],"extra":true}`, `invalid arguments: "extra" is not a declared property`},
		"unknown item key":  {`{"todos":[{"content":"a","status":"pending","activeForm":"x"}]}`, `invalid arguments: "todos[0].activeForm" is not a declared property`},
		"unknown status":    {`{"todos":[{"content":"a","status":"pending"},{"content":"b","status":"doing"}]}`, `invalid arguments: "todos[1].status" must be one of ["pending","in_progress","completed"]`},
		"blank content":     {`{"todos":[{"content":"   ","status":"pending"}]}`, "invalid todo: `content` must be a non-empty string"},
		"duplicate content": {`{"todos":[{"content":"dup","status":"pending"},{"content":" dup ","status":"completed"}]}`, `invalid todos: duplicate content "dup"`},
		"oversize content":  {`{"todos":[{"content":"` + strings.Repeat("x", session.MaxTodoContentBytes+1) + `","status":"pending"}]}`, "invalid todo: `content` exceeds 2048 bytes"},
		"too many todos":    {tooManyTodos(), "invalid todos: at most 256 todos are allowed (got 257)"},
	} {
		results := execute(t, runtime, journal, test.arguments)
		if !results[0].IsError || results[0].Output != "Error: "+test.message {
			t.Errorf("%s: output = %q, want %q", name, results[0].Output, test.message)
		}
	}
	if written := journal.written(); len(written) != 0 {
		t.Fatalf("rejected lists reached the log: %+v", written)
	}
}

func tooManyTodos() string {
	items := make([]string, session.MaxTodoItems+1)
	for index := range items {
		items[index] = fmt.Sprintf(`{"content":"task %d","status":"pending"}`, index)
	}
	return `{"todos":[` + strings.Join(items, ",") + `]}`
}

func TestWriteTool_RequiresOwningSessionAndReportsCommitFailures(t *testing.T) {
	runtime := startTools(t)
	valid := `{"todos":[{"content":"a","status":"pending"}]}`
	if results := execute(t, runtime, nil, valid); results[0].Output != "Error: todo_write requires an owning agent session" {
		t.Fatalf("non-agent caller = %+v", results)
	}
	if results := execute(t, runtime, nil, `{"todos":[{"content":"","status":"pending"}]}`); !strings.Contains(results[0].Output, "non-empty string") {
		t.Fatalf("list validation must precede the owner check: %+v", results)
	}
	failure := errors.New("disk full")
	if results := execute(t, runtime, &recordingJournal{err: failure}, valid); results[0].Output != "Error: record todo list: disk full" {
		t.Fatalf("append failure = %+v", results)
	}
	journal := &recordingJournal{}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	results := runtime.ExecuteBatch(canceled, appTool.BatchRequest{SessionID: "session", Turn: 1, Step: 1, Calls: []session.ToolCall{{ID: "call", Name: toolName, Arguments: json.RawMessage(valid)}}, Journal: journal})
	if !results[0].IsError || results[0].Output != "Error: tool call aborted before dispatch" || len(journal.written()) != 0 {
		t.Fatalf("canceled write = %+v records=%v", results, journal.written())
	}
}
