package tool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type fakeApprover struct {
	mu      sync.Mutex
	outcome session.ApprovalOutcome
	err     error
	seen    []ApprovalRequest
}

func (approver *fakeApprover) Decide(_ context.Context, request ApprovalRequest) (session.ApprovalOutcome, error) {
	approver.mu.Lock()
	defer approver.mu.Unlock()
	approver.seen = append(approver.seen, request)
	return approver.outcome, approver.err
}

type fakeJournal struct{}

func (fakeJournal) Append(context.Context, session.Record) (session.Event, error) {
	return session.Event{}, nil
}

type noArguments struct{}

type valueArguments struct {
	Value *string `json:"value"`
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

// simpleTool defines a no-argument tool with the given behavior.
func simpleTool(name string, concurrent bool, reason string, execute func(context.Context, Invocation) (Result, error)) *Tool {
	return Define(Spec[noArguments]{
		Name: name, Description: "test tool",
		Concurrent: func(noArguments) bool { return concurrent },
		Approval:   func(noArguments) string { return reason },
		Execute: func(ctx context.Context, invocation Invocation, _ noArguments) (Result, error) {
			return execute(ctx, invocation)
		},
	})
}

func TestRuntime_RegistrationLifecycleAndCatalog(t *testing.T) {
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
	ok := func(context.Context, Invocation) (Result, error) { return Text("ok"), nil }
	inactive, _ := New(approver)
	if err := inactive.Register(simpleTool("inactive", true, "", ok), &plugin.Scope{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive registration=%v", err)
	}
	runtime, runtimeScope := startRuntime(t, approver)
	if runtime.Start(context.Background(), &plugin.Scope{}) == nil {
		t.Fatal("double start accepted")
	}
	providerScope := &plugin.Scope{}
	grep := Define(Spec[noArguments]{
		Name: "grep", Description: "search",
		Guidance: Guidance{Order: OrderGrep, Text: func(visible func(string) bool) string {
			if visible("read") {
				return "grep with read"
			}
			return "grep alone"
		}},
		Execute: func(context.Context, Invocation, noArguments) (Result, error) { return Text("grep"), nil },
	})
	read := Define(Spec[noArguments]{
		Name: "read", Description: "read", Guidance: StaticGuidance(OrderRead, "read guidance"),
		Execute: func(context.Context, Invocation, noArguments) (Result, error) { return Text("read"), nil },
	})
	silent := Define(Spec[noArguments]{
		Name: "silent", Description: "no paragraph", Guidance: Guidance{Order: 1, Text: func(func(string) bool) string { return "" }},
		Execute: func(context.Context, Invocation, noArguments) (Result, error) { return Text("silent"), nil },
	})
	plain := simpleTool("plain", false, "", ok)
	for _, candidate := range []*Tool{grep, read, silent, plain} {
		if err := runtime.Register(candidate, providerScope); err != nil {
			t.Fatal(err)
		}
	}
	if err := runtime.Register(plain, &plugin.Scope{}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate accepted: %v", err)
	}
	if err := runtime.Register(nil, &plugin.Scope{}); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("nil tool = %v", err)
	}
	if err := runtime.Register(plain, nil); !errors.Is(err, ErrInvalidTool) {
		t.Fatalf("nil scope = %v", err)
	}
	invalid := Define(Spec[noArguments]{Name: "bad name", Description: "x", Execute: func(context.Context, Invocation, noArguments) (Result, error) { return Result{}, nil }})
	if err := runtime.Register(invalid, &plugin.Scope{}); !errors.Is(err, ErrInvalidTool) || !strings.Contains(err.Error(), `"bad name"`) {
		t.Fatalf("invalid definition = %v", err)
	}
	closedProviderScope := &plugin.Scope{}
	_ = closedProviderScope.Close(context.Background())
	if err := runtime.Register(simpleTool("rollback", true, "", ok), closedProviderScope); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed provider scope=%v", err)
	}
	all, err := runtime.Catalog(nil)
	if err != nil || len(all.Definitions) != 4 || all.Definitions[0].Name != "grep" || all.Definitions[3].Name != "silent" {
		t.Fatalf("catalog=%#v err=%v", all, err)
	}
	if strings.Join(all.Guidance, "|") != "read guidance|grep with read" {
		t.Fatalf("guidance=%q", all.Guidance)
	}
	filtered, err := runtime.Catalog([]string{"grep", "missing"})
	if err != nil || len(filtered.Definitions) != 1 || strings.Join(filtered.Guidance, "|") != "grep alone" {
		t.Fatalf("filtered=%#v err=%v", filtered, err)
	}
	// The returned schema is a copy: mutating it cannot change later snapshots.
	all.Definitions[0].Parameters[0] = 'x'
	if again, _ := runtime.Catalog(nil); again.Definitions[0].Parameters[0] != '{' {
		t.Fatal("catalog exposed registry storage")
	}
	if err := providerScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if catalog, _ := runtime.Catalog(nil); len(catalog.Definitions) != 0 {
		t.Fatal("cleanup retained tools")
	}
	stale := simpleTool("stale", true, "", ok)
	staleScope := &plugin.Scope{}
	if err := runtime.Register(stale, staleScope); err != nil {
		t.Fatal(err)
	}
	if err := runtimeScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := staleScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Catalog(nil); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("after close=%v", err)
	}
	closedResult := runtime.ExecuteBatch(context.Background(), BatchRequest{Calls: []session.ToolCall{{ID: "c", Name: "plain", Arguments: json.RawMessage(`{}`)}}})
	if len(closedResult) != 1 || !closedResult[0].IsError || closedResult[0].CallID != "c" {
		t.Fatalf("closed execution=%#v", closedResult)
	}
}

func TestRuntime_SchedulesConcurrentGroupsAndExclusiveBarriers(t *testing.T) {
	approver := &fakeApprover{outcome: session.ApprovalAllowedOnce}
	runtime, _ := startRuntime(t, approver)
	scope := &plugin.Scope{}
	entered := make(chan string, 4)
	release := make(chan struct{})
	var mu sync.Mutex
	order := []string{}
	record := func(name string) {
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}
	concurrent := Define(Spec[valueArguments]{
		Name: "concurrent", Description: "overlaps unless asked not to",
		Parameters: Parameters{Optional("value", String("label"))},
		Concurrent: func(arguments valueArguments) bool { return arguments.Value == nil || *arguments.Value != "solo" },
		Execute: func(_ context.Context, _ Invocation, arguments valueArguments) (Result, error) {
			label := "unset"
			if arguments.Value != nil {
				label = *arguments.Value
			}
			entered <- label
			<-release
			record(label)
			return Text(label), nil
		},
	})
	exclusive := simpleTool("exclusive", false, "write", func(_ context.Context, invocation Invocation) (Result, error) {
		if !invocation.Approved || invocation.SessionID != "s" || invocation.Cwd != "/work" || !invocation.Delegated {
			t.Errorf("invocation = %+v", invocation)
		}
		record("exclusive")
		return Text("exclusive"), nil
	})
	for _, candidate := range []*Tool{concurrent, exclusive} {
		if err := runtime.Register(candidate, scope); err != nil {
			t.Fatal(err)
		}
	}
	calls := []session.ToolCall{
		{ID: "1", Name: "concurrent", Arguments: json.RawMessage(`{"value":"a"}`)},
		{ID: "2", Name: "concurrent", Arguments: json.RawMessage(`{}`)},
		{ID: "3", Name: "exclusive", Arguments: json.RawMessage(`{}`)},
		{ID: "4", Name: "concurrent", Arguments: json.RawMessage(`{"value":"solo"}`)},
		{ID: "5", Name: "missing", Arguments: json.RawMessage(`{}`)},
		{ID: "6", Name: "concurrent", Arguments: json.RawMessage(`{"value":1}`)},
	}
	done := make(chan []session.ToolResult)
	go func() {
		done <- runtime.ExecuteBatch(context.Background(), BatchRequest{SessionID: "s", Cwd: "/work", Turn: 1, Step: 1, Calls: calls, Delegated: true, Journal: fakeJournal{}})
	}()
	// Both members of the first group must be running before either finishes.
	first, second := <-entered, <-entered
	if (first != "a" || second != "unset") && (first != "unset" || second != "a") {
		t.Fatalf("group entries = %q, %q", first, second)
	}
	close(release)
	results := <-done
	if len(results) != 6 {
		t.Fatalf("results=%#v", results)
	}
	for index, want := range []string{"a", "unset", "exclusive", "solo"} {
		if results[index].Output != want || results[index].IsError || results[index].CallID != calls[index].ID {
			t.Fatalf("result %d = %#v", index, results[index])
		}
	}
	if !results[4].IsError || results[4].Output != "tool error: unknown tool missing" {
		t.Fatalf("unknown = %#v", results[4])
	}
	if !results[5].IsError || results[5].Output != `tool error: invalid arguments: "value" must be a string` {
		t.Fatalf("invalid = %#v", results[5])
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(order[2:], ",") != "exclusive,solo" || len(approver.seen) != 1 || approver.seen[0].Reason != "write" {
		t.Fatalf("order=%v approvals=%#v", order, approver.seen)
	}
}

func TestRuntime_ContainsApprovalFailuresPanicsAndLargeOutput(t *testing.T) {
	approver := &fakeApprover{outcome: session.ApprovalRejected}
	runtime, _ := startRuntime(t, approver)
	scope := &plugin.Scope{}
	never := func(context.Context, Invocation) (Result, error) {
		t.Fatal("executed a rejected call")
		return Result{}, nil
	}
	longReason := strings.Repeat("é", maxReasonBytes)
	tools := []*Tool{
		simpleTool("rejected", false, longReason, never),
		simpleTool("failure", false, "", func(context.Context, Invocation) (Result, error) { return Result{}, errors.New("failed") }),
		simpleTool("panic", false, "", func(context.Context, Invocation) (Result, error) { panic("boom") }),
		simpleTool("large", false, "", func(context.Context, Invocation) (Result, error) {
			return Text(strings.Repeat("é", session.MaxTextBytes)), nil
		}),
		simpleTool("invalid_utf8", false, "", func(context.Context, Invocation) (Result, error) { return Text("a\xffb"), nil }),
		Define(Spec[noArguments]{Name: "check_panic", Description: "panics while classifying", Concurrent: func(noArguments) bool { panic("classifier") }, Execute: never2}),
		Define(Spec[noArguments]{Name: "checked", Description: "semantic error", Check: func(noArguments) error { return errors.New("semantic") }, Execute: never2}),
	}
	for _, candidate := range tools {
		if err := runtime.Register(candidate, scope); err != nil {
			t.Fatal(err)
		}
	}
	calls := make([]session.ToolCall, len(tools))
	for index, candidate := range tools {
		name := candidate.Definition().Name
		calls[index] = session.ToolCall{ID: name, Name: name, Arguments: json.RawMessage(`{}`)}
	}
	results := runtime.ExecuteBatch(context.Background(), BatchRequest{SessionID: "s", Turn: 1, Step: 1, Calls: calls, Journal: fakeJournal{}})
	want := []string{"tool error: approval rejected", "tool error: failed", "tool error: implementation panicked"}
	for index, output := range want {
		if !results[index].IsError || results[index].Output != output {
			t.Errorf("result %d = %#v", index, results[index])
		}
	}
	if len(approver.seen[0].Reason) > maxReasonBytes || !strings.HasSuffix(approver.seen[0].Reason, "…") || !utf8.ValidString(approver.seen[0].Reason) {
		t.Fatalf("reason was not clamped: %d bytes", len(approver.seen[0].Reason))
	}
	large := results[3]
	if large.IsError || len(large.Output) > session.MaxTextBytes || !strings.HasSuffix(large.Output, "\n[output truncated]") || !utf8.ValidString(large.Output) {
		t.Fatalf("large=%d bytes error=%v", len(large.Output), large.IsError)
	}
	if results[4].Output != "a�b" {
		t.Fatalf("invalid UTF-8 = %q", results[4].Output)
	}
	if !results[5].IsError || results[5].Output != "tool error: implementation panicked" {
		t.Fatalf("classifier panic = %#v", results[5])
	}
	if !results[6].IsError || results[6].Output != "tool error: semantic" {
		t.Fatalf("check = %#v", results[6])
	}
	approver.outcome, approver.err = session.ApprovalAllowedOnce, errors.New("approval")
	result := runtime.ExecuteBatch(context.Background(), BatchRequest{SessionID: "s", Turn: 1, Step: 1, Calls: calls[:1], Journal: fakeJournal{}})[0]
	if !result.IsError || result.Output != "tool error: approval could not be recorded" {
		t.Fatalf("approval error = %#v", result)
	}
}

func never2(context.Context, Invocation, noArguments) (Result, error) {
	return Result{}, errors.New("must not execute")
}

func TestClamp_RespectsRuneBoundaries(t *testing.T) {
	if got := clamp("short", 10, "…"); got != "short" {
		t.Fatalf("short = %q", got)
	}
	if got := clamp("ééé", 5, "!"); got != "éé!" {
		t.Fatalf("rune boundary = %q", got)
	}
	if got := clamp("ééé", 4, "!"); got != "é!" {
		t.Fatalf("continuation byte = %q", got)
	}
}

// TestRuntime_ChecksEachCallAfterEarlierCallsInTheBatch pins that semantic
// checks observe effects of earlier calls, e.g. a workdir created by the
// previous command in the same step.
func TestRuntime_ChecksEachCallAfterEarlierCallsInTheBatch(t *testing.T) {
	runtime, _ := startRuntime(t, &fakeApprover{outcome: session.ApprovalAllowedOnce})
	created := false
	create := simpleTool("create", false, "", func(context.Context, Invocation) (Result, error) {
		created = true
		return Text("created"), nil
	})
	use := Define(Spec[noArguments]{
		Name: "use", Description: "needs the created state",
		Check: func(noArguments) error {
			if !created {
				return errors.New("state is missing")
			}
			return nil
		},
		Approval: func(noArguments) string { return "use state" },
		Execute:  func(context.Context, Invocation, noArguments) (Result, error) { return Text("used"), nil },
	})
	scope := &plugin.Scope{}
	for _, candidate := range []*Tool{create, use} {
		if err := runtime.Register(candidate, scope); err != nil {
			t.Fatal(err)
		}
	}
	results := runtime.ExecuteBatch(context.Background(), BatchRequest{SessionID: "s", Turn: 1, Step: 1, Journal: fakeJournal{}, Calls: []session.ToolCall{
		{ID: "1", Name: "use", Arguments: json.RawMessage(`{}`)},
		{ID: "2", Name: "create", Arguments: json.RawMessage(`{}`)},
		{ID: "3", Name: "use", Arguments: json.RawMessage(`{}`)},
	}})
	if !results[0].IsError || results[0].Output != "tool error: state is missing" || results[1].Output != "created" || results[2].IsError || results[2].Output != "used" {
		t.Fatalf("results = %#v", results)
	}
}
