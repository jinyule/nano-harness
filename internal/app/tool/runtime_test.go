package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type fakeApprover struct {
	outcome session.ApprovalOutcome
	err     error
	seen    []ApprovalRequest
}

func (approver *fakeApprover) Decide(_ context.Context, request ApprovalRequest) (session.ApprovalOutcome, error) {
	approver.seen = append(approver.seen, request)
	return approver.outcome, approver.err
}

type fakeTool struct {
	definition  session.ToolDefinition
	concurrency Concurrency
	reason      string
	execute     func(context.Context, Execution) (string, error)
}

func (tool *fakeTool) Definition() session.ToolDefinition    { return tool.definition }
func (tool *fakeTool) Concurrency() Concurrency              { return tool.concurrency }
func (tool *fakeTool) ApprovalReason(json.RawMessage) string { return tool.reason }
func (tool *fakeTool) Execute(ctx context.Context, execution Execution) (string, error) {
	return tool.execute(ctx, execution)
}

type fakeJournal struct{}

func (fakeJournal) Append(context.Context, session.Record) (session.Event, error) {
	return session.Event{}, nil
}

func startRuntime(t *testing.T, approver Approver) (*Runtime, *plugin.Scope) {
	t.Helper()
	runtime, err := New(approver)
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if runtime.ID() != "tools" || runtime.Start(context.Background(), scope) != nil {
		t.Fatal("start")
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return runtime, scope
}

func definitionFor(name string) session.ToolDefinition {
	return session.ToolDefinition{Name: name, Description: "test tool", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func TestRuntimeRegistrationSchedulingAndApproval(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("nil approver accepted")
	}
	approver := &fakeApprover{outcome: session.ApprovalAllowedOnce}
	closedRuntimeScope := &plugin.Scope{}
	_ = closedRuntimeScope.Close(context.Background())
	inactiveForStart, _ := New(approver)
	if err := inactiveForStart.Start(context.Background(), closedRuntimeScope); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed start scope=%v", err)
	}
	inactive, _ := New(approver)
	if err := inactive.Register(&fakeTool{definition: definitionFor("inactive"), concurrency: ConcurrencyParallel, execute: func(context.Context, Execution) (string, error) { return "", nil }}, &plugin.Scope{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive registration=%v", err)
	}
	runtime, runtimeScope := startRuntime(t, approver)
	if runtime.Start(context.Background(), &plugin.Scope{}) == nil {
		t.Fatal("double start accepted")
	}
	var mu sync.Mutex
	order := []string{}
	parallel := &fakeTool{definition: definitionFor("parallel"), concurrency: ConcurrencyParallel, execute: func(context.Context, Execution) (string, error) {
		mu.Lock()
		order = append(order, "parallel")
		mu.Unlock()
		return "parallel", nil
	}}
	exclusive := &fakeTool{definition: definitionFor("exclusive"), concurrency: ConcurrencyExclusive, reason: "write", execute: func(_ context.Context, execution Execution) (string, error) {
		if !execution.Elevated {
			t.Fatal("approved tool was not elevated")
		}
		mu.Lock()
		order = append(order, "exclusive")
		mu.Unlock()
		return "exclusive", nil
	}}
	providerScope := &plugin.Scope{}
	for _, candidate := range []Tool{parallel, exclusive} {
		if err := runtime.Register(candidate, providerScope); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.Register(parallel, &plugin.Scope{}); err == nil {
		t.Fatal("duplicate accepted")
	}
	invalid := &fakeTool{definition: session.ToolDefinition{}, concurrency: "bad", execute: parallel.execute}
	if err := runtime.Register(invalid, &plugin.Scope{}); err == nil {
		t.Fatal("invalid accepted")
	}
	if err := runtime.Register(nil, &plugin.Scope{}); err == nil {
		t.Fatal("nil accepted")
	}
	closedProviderScope := &plugin.Scope{}
	_ = closedProviderScope.Close(context.Background())
	rollbackTool := &fakeTool{definition: definitionFor("rollback"), concurrency: ConcurrencyParallel, execute: parallel.execute}
	if err := runtime.Register(rollbackTool, closedProviderScope); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed provider scope=%v", err)
	}
	if definitions, _ := runtime.Definitions(nil); len(definitions) != 2 {
		t.Fatalf("failed registration leaked: %#v", definitions)
	}
	definitions, err := runtime.Definitions([]string{"parallel"})
	if err != nil || len(definitions) != 1 || definitions[0].Name != "parallel" {
		t.Fatalf("definitions=%#v err=%v", definitions, err)
	}
	all, _ := runtime.Definitions(nil)
	if len(all) != 2 || all[0].Name != "exclusive" {
		t.Fatalf("all=%#v", all)
	}
	calls := []session.ToolCall{
		{ID: "1", Name: "parallel", Arguments: json.RawMessage(`{}`)},
		{ID: "2", Name: "parallel", Arguments: json.RawMessage(`{}`)},
		{ID: "3", Name: "exclusive", Arguments: json.RawMessage(`{}`)},
		{ID: "4", Name: "missing", Arguments: json.RawMessage(`{}`)},
	}
	results := runtime.ExecuteBatch(context.Background(), BatchRequest{SessionID: "s", Cwd: ".", Turn: 1, Step: 1, Calls: calls, Journal: fakeJournal{}})
	if len(results) != 4 || results[2].Output != "exclusive" || !results[3].IsError || len(approver.seen) != 1 {
		t.Fatalf("results=%#v approvals=%d", results, len(approver.seen))
	}
	if err := providerScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if definitions, _ := runtime.Definitions(nil); len(definitions) != 0 {
		t.Fatal("cleanup retained tools")
	}
	if err := runtimeScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Definitions(nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("after close=%v", err)
	}
	closedResult := runtime.ExecuteBatch(context.Background(), BatchRequest{Calls: []session.ToolCall{{ID: "c", Name: "parallel", Arguments: json.RawMessage(`{}`)}}})
	if len(closedResult) != 1 || !closedResult[0].IsError {
		t.Fatalf("closed execution=%#v", closedResult)
	}
}

func TestRuntimeToolFailureContainment(t *testing.T) {
	approver := &fakeApprover{outcome: session.ApprovalRejected}
	runtime, _ := startRuntime(t, approver)
	scope := &plugin.Scope{}
	tools := []*fakeTool{
		{definition: definitionFor("rejected"), concurrency: ConcurrencyExclusive, reason: "risk", execute: func(context.Context, Execution) (string, error) { t.Fatal("executed rejected"); return "", nil }},
		{definition: definitionFor("failure"), concurrency: ConcurrencyExclusive, execute: func(context.Context, Execution) (string, error) { return "", errors.New("failed") }},
		{definition: definitionFor("panic"), concurrency: ConcurrencyExclusive, execute: func(context.Context, Execution) (string, error) { panic("boom") }},
		{definition: definitionFor("large"), concurrency: ConcurrencyExclusive, execute: func(context.Context, Execution) (string, error) {
			return strings.Repeat("x", session.MaxTextBytes+10), nil
		}},
	}
	for _, candidate := range tools {
		if err := runtime.Register(candidate, scope); err != nil {
			t.Fatal(err)
		}
	}
	calls := make([]session.ToolCall, len(tools))
	for index, candidate := range tools {
		calls[index] = session.ToolCall{ID: candidate.definition.Name, Name: candidate.definition.Name, Arguments: json.RawMessage(`{}`)}
	}
	results := runtime.ExecuteBatch(context.Background(), BatchRequest{SessionID: "s", Turn: 1, Step: 1, Calls: calls, Journal: fakeJournal{}})
	for index, result := range results[:3] {
		if !result.IsError {
			t.Errorf("result %d not error: %#v", index, result)
		}
	}
	if results[3].IsError || !strings.HasSuffix(results[3].Output, "[output truncated]") {
		t.Fatalf("large=%#v", results[3])
	}
	approver.outcome, approver.err = session.ApprovalAllowedOnce, errors.New("approval")
	result := runtime.ExecuteBatch(context.Background(), BatchRequest{SessionID: "s", Turn: 1, Step: 1, Calls: calls[:1], Journal: fakeJournal{}})[0]
	if !result.IsError {
		t.Fatal("approval error not contained")
	}
}
