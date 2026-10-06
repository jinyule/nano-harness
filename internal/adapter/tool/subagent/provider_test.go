package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	appSubagent "github.com/jinyule/nano-harness/internal/app/subagent"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type allowAll struct{}

func (allowAll) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalAllowedOnce, nil
}

type nullJournal struct{}

func (nullJournal) Append(_ context.Context, record session.Record) (session.Event, error) {
	return session.Event{Sequence: 1, Record: record}, nil
}

// fakeService records the last call and returns configured values.
type fakeService struct {
	mu        sync.Mutex
	calls     []string
	requests  []appSubagent.StartRequest
	report    appSubagent.Report
	entries   []appSubagent.Entry
	err       error
	arguments []string
}

func (service *fakeService) record(call string, request appSubagent.StartRequest) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.calls = append(service.calls, call)
	service.requests = append(service.requests, request)
}

func (service *fakeService) StartContinuable(_ context.Context, request appSubagent.StartRequest) (string, error) {
	service.record("continuable", request)
	return "child-1", service.err
}

func (service *fakeService) StartBackground(_ context.Context, request appSubagent.StartRequest) (string, error) {
	service.record("background", request)
	return "subagent-1", service.err
}

func (service *fakeService) Run(_ context.Context, request appSubagent.StartRequest) (appSubagent.Report, error) {
	service.record("run", request)
	return service.report, service.err
}

func (service *fakeService) SendMessage(_ context.Context, senderID, targetID, text string) error {
	service.arguments = []string{"send", senderID, targetID, text}
	return service.err
}

func (service *fakeService) Interrupt(callerID, targetID string) error {
	service.arguments = []string{"interrupt", callerID, targetID}
	return service.err
}

func (service *fakeService) ListChildren(_ context.Context, parentID string) ([]appSubagent.Entry, error) {
	service.arguments = []string{"children", parentID}
	return service.entries, service.err
}

func (service *fakeService) ListDescendants(_ context.Context, rootID string) ([]appSubagent.Entry, error) {
	service.arguments = []string{"descendants", rootID}
	return service.entries, service.err
}

func startProvider(t *testing.T, service Service) *appTool.Runtime {
	t.Helper()
	runtime, _ := appTool.New(allowAll{})
	runtimeScope, providerScope := &plugin.Scope{}, &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	provider, err := New(runtime, service)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background(), providerScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = providerScope.Close(context.Background())
		_ = runtimeScope.Close(context.Background())
	})
	return runtime
}

func call(t *testing.T, runtime *appTool.Runtime, name string, arguments map[string]any) session.ToolResult {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	return runtime.ExecuteBatch(context.Background(), appTool.BatchRequest{
		SessionID: "root", Turn: 3, Step: 2, Journal: nullJournal{},
		Calls: []session.ToolCall{{ID: "call", Name: name, Arguments: encoded}},
	})[0]
}

func TestProvider_RegistersUpstreamDefinitionsForItsScope(t *testing.T) {
	runtime, _ := appTool.New(allowAll{})
	for _, test := range []struct {
		runtime *appTool.Runtime
		service Service
	}{{service: &fakeService{}}, {runtime: runtime}} {
		if _, err := New(test.runtime, test.service); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(%+v) = %v", test, err)
		}
	}
	provider, _ := New(runtime, &fakeService{})
	if provider.ID() != "subagent-tools" {
		t.Fatalf("ID = %q", provider.ID())
	}
	if err := provider.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, appTool.ErrNotRunning) {
		t.Fatalf("inactive runtime = %v", err)
	}
	runtimeScope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeScope.Close(context.Background()) })
	scope := &plugin.Scope{}
	if err := provider.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	catalog, _ := runtime.Catalog(nil)
	parameters := map[string]string{}
	for _, definition := range catalog.Definitions {
		parameters[definition.Name] = string(definition.Parameters)
	}
	if len(parameters) != 5 {
		t.Fatalf("definitions = %#v", catalog.Definitions)
	}
	for name, want := range map[string]string{
		"subagent":        `{"type":"object","properties":{"description":{"type":"string","description":"` + descriptionText + `"},"prompt":{"type":"string","description":"` + spawnPromptText + `"},"run_in_background":{"type":"boolean","description":"` + spawnBackgroundText + `"}},"required":["description","prompt"]}`,
		"subagent_fork":   `{"type":"object","properties":{"description":{"type":"string","description":"` + descriptionText + `"},"prompt":{"type":"string","description":"` + forkPromptText + `"},"run_in_background":{"type":"boolean","description":"` + forkBackgroundText + `"}},"required":["description","prompt"]}`,
		"list_agents":     `{"type":"object","properties":{"scope":{"type":"string","description":"` + scopeText + `","enum":["children","descendants"]}}}`,
		"interrupt_agent": `{"type":"object","properties":{"agent_id":{"type":"string","description":"The id of an agent created under you: your direct child or a deeper descendant."}},"required":["agent_id"]}`,
	} {
		if parameters[name] != want {
			t.Errorf("%s parameters = %s", name, parameters[name])
		}
	}
	if len(catalog.Guidance) != 1 || catalog.Guidance[0] != spawnGuidance {
		t.Fatalf("guidance = %q", catalog.Guidance)
	}
	if forkOnly, _ := runtime.Catalog([]string{"subagent_fork", "send_message"}); len(forkOnly.Guidance) != 0 {
		t.Fatalf("guidance without subagent = %q", forkOnly.Guidance)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if catalog, _ := runtime.Catalog(nil); len(catalog.Definitions) != 0 {
		t.Fatal("subagent tools survived cleanup")
	}
}

func TestDelegationTools_ChooseLifecycleByToolAndBackgroundFlag(t *testing.T) {
	service := &fakeService{report: appSubagent.Report{Outcome: session.OutcomeCompleted, Text: "final answer"}}
	runtime := startProvider(t, service)
	for _, test := range []struct {
		name      string
		arguments map[string]any
		call      string
		fork      bool
		want      string
	}{
		{"subagent", map[string]any{"description": "scan", "prompt": "do it"}, "continuable", false, "started subagent child-1"},
		{"subagent", map[string]any{"description": "scan", "prompt": "do it", "run_in_background": true}, "continuable", false, "started subagent child-1"},
		{"subagent", map[string]any{"description": "scan", "prompt": "do it", "run_in_background": false}, "run", false, "final answer"},
		{"subagent_fork", map[string]any{"description": "review", "prompt": "check"}, "run", true, "final answer"},
		{"subagent_fork", map[string]any{"description": "review", "prompt": "check", "run_in_background": false}, "run", true, "final answer"},
		{"subagent_fork", map[string]any{"description": "review", "prompt": "check", "run_in_background": true}, "background", true, "started background subagent job subagent-1"},
	} {
		result := call(t, runtime, test.name, test.arguments)
		last := len(service.calls) - 1
		request := service.requests[last]
		if result.IsError || result.Output != test.want || service.calls[last] != test.call || request.Fork != test.fork ||
			request.ParentID != "root" || request.Turn != 3 || request.Step != 2 || request.Journal == nil ||
			request.Description != test.arguments["description"] || request.Prompt != test.arguments["prompt"] {
			t.Errorf("%s(%v) = %#v via %s %#v", test.name, test.arguments, result, service.calls[last], request)
		}
	}
	// Independent delegations in one batch run together.
	encoded, _ := json.Marshal(map[string]any{"description": "a", "prompt": "b"})
	results := runtime.ExecuteBatch(context.Background(), appTool.BatchRequest{SessionID: "root", Turn: 1, Step: 1, Journal: nullJournal{}, Calls: []session.ToolCall{
		{ID: "one", Name: "subagent", Arguments: encoded}, {ID: "two", Name: "subagent_fork", Arguments: encoded},
	}})
	if len(results) != 2 || results[0].CallID != "one" || results[1].CallID != "two" {
		t.Fatalf("batch = %#v", results)
	}
}

func TestDelegationTools_ReportFailuresAndUnfinishedRuns(t *testing.T) {
	service := &fakeService{err: errors.New("subagent depth 5 exceeds maxDepth 4")}
	runtime := startProvider(t, service)
	for _, name := range []string{"subagent", "subagent_fork"} {
		for _, background := range []bool{true, false} {
			result := call(t, runtime, name, map[string]any{"description": "d", "prompt": "p", "run_in_background": background})
			if !result.IsError || result.Output != "Error: subagent depth 5 exceeds maxDepth 4" {
				t.Errorf("%s background=%t = %#v", name, background, result)
			}
		}
	}
	service.err = nil
	for _, test := range []struct {
		report appSubagent.Report
		want   string
	}{
		{appSubagent.Report{Outcome: session.OutcomeCanceled, Text: "half"}, "Error: subagent run was cancelled\nPartial output before the run ended:\nhalf"},
		{appSubagent.Report{Outcome: session.OutcomeInterrupted}, "Error: subagent run was cancelled"},
		{appSubagent.Report{Outcome: session.OutcomeError}, "Error: subagent run failed"},
		{appSubagent.Report{Outcome: session.OutcomeMaxTokens, Text: "partial"}, "Error: subagent run ended abnormally (max_tokens)\nPartial output before the run ended:\npartial"},
		{appSubagent.Report{Outcome: session.OutcomeStepLimit, Text: "draft"}, "Error: subagent run ended abnormally (step_limit)\nPartial output before the run ended:\ndraft"},
	} {
		service.report = test.report
		if result := call(t, runtime, "subagent_fork", map[string]any{"description": "d", "prompt": "p"}); !result.IsError || result.Output != test.want {
			t.Errorf("%s = %#v", test.report.Outcome, result)
		}
	}
}

func TestControlTools_DelegateToTheService(t *testing.T) {
	service := &fakeService{}
	runtime := startProvider(t, service)
	if result := call(t, runtime, "send_message", map[string]any{"agent_id": "child-1", "message": "hello"}); result.Output != "message delivered to agent child-1" || strings.Join(service.arguments, ",") != "send,root,child-1,hello" {
		t.Fatalf("send_message = %#v %q", result, service.arguments)
	}
	if result := call(t, runtime, "interrupt_agent", map[string]any{"agent_id": "grand"}); result.Output != "interrupt requested for agent grand" || strings.Join(service.arguments, ",") != "interrupt,root,grand" {
		t.Fatalf("interrupt_agent = %#v %q", result, service.arguments)
	}
	if result := call(t, runtime, "list_agents", map[string]any{}); result.Output != "(no subagents)" || service.arguments[0] != "children" {
		t.Fatalf("empty list_agents = %#v", result)
	}
	service.entries = []appSubagent.Entry{
		{ID: "a", Parent: "root", Label: "worker", Mode: session.SubagentContinuable, Depth: 1, Running: true},
		{ID: "b", Parent: "root", Label: "once", Mode: session.SubagentOneShot, Depth: 1},
		{ID: "c", Parent: "a", Label: "helper", Mode: session.SubagentContinuable, Depth: 2},
		{ID: "d", Parent: "b", Mode: session.SubagentOneShot, Depth: 2, Unavailable: true},
	}
	if result := call(t, runtime, "list_agents", map[string]any{"scope": "children"}); result.Output != "a [running] — worker\nc [inactive] — helper\nd [diagnostic: unavailable]" {
		t.Fatalf("children = %#v", result)
	}
	if result := call(t, runtime, "list_agents", map[string]any{"scope": "descendants"}); result.Output != "a [running] parent=root depth=1 — worker\nc [inactive] parent=a depth=2 — helper\nd [diagnostic: unavailable] parent=b depth=2" || service.arguments[0] != "descendants" {
		t.Fatalf("descendants = %#v", result)
	}
	if result := call(t, runtime, "list_agents", map[string]any{"scope": "everyone"}); !result.IsError || !strings.Contains(result.Output, "invalid arguments") {
		t.Fatalf("invalid scope = %#v", result)
	}
	service.err = errors.New(`subagent "x" belongs to another parent session`)
	for name, arguments := range map[string]map[string]any{
		"send_message":    {"agent_id": "x", "message": "m"},
		"interrupt_agent": {"agent_id": "x"},
		"list_agents":     {},
	} {
		if result := call(t, runtime, name, arguments); !result.IsError || result.Output != `Error: subagent "x" belongs to another parent session` {
			t.Errorf("%s = %#v", name, result)
		}
	}
}
