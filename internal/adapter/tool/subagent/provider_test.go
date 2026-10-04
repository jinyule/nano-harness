package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/agent"
	appSubagent "github.com/jinyule/nano-harness/internal/app/subagent"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type allowApprover struct{}

func (allowApprover) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalAllowedOnce, nil
}

type fakeService struct {
	spawnRequest appSubagent.SpawnRequest
	spawnInfo    appSubagent.Info
	spawnErr     error
	waitID       string
	waitInfo     appSubagent.Info
	waitErr      error
	followCaller string
	followID     string
	followTask   string
	followInfo   appSubagent.Info
	followErr    error
	interruptIDs [2]string
	interruptErr error
	reportID     string
	reportInfo   appSubagent.Info
	reportErr    error
	listParent   string
	listInfos    []appSubagent.Info
	listErr      error
}

func (service *fakeService) Spawn(_ context.Context, request appSubagent.SpawnRequest) (appSubagent.Info, error) {
	service.spawnRequest = request
	return service.spawnInfo, service.spawnErr
}
func (service *fakeService) Wait(_ context.Context, id string) (appSubagent.Info, error) {
	service.waitID = id
	return service.waitInfo, service.waitErr
}
func (service *fakeService) Followup(_ context.Context, caller, id, task string) (appSubagent.Info, error) {
	service.followCaller, service.followID, service.followTask = caller, id, task
	return service.followInfo, service.followErr
}
func (service *fakeService) Interrupt(caller, id string) error {
	service.interruptIDs = [2]string{caller, id}
	return service.interruptErr
}
func (service *fakeService) Report(id string) (appSubagent.Info, error) {
	service.reportID = id
	return service.reportInfo, service.reportErr
}
func (service *fakeService) List(parent string) ([]appSubagent.Info, error) {
	service.listParent = parent
	return service.listInfos, service.listErr
}

// startTools registers the provider on a live runtime and returns a call helper.
func startTools(t *testing.T, service Service) func(name, arguments string) session.ToolResult {
	t.Helper()
	runtime, _ := appTool.New(allowApprover{})
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
	return func(name, arguments string) session.ToolResult {
		return runtime.ExecuteBatch(context.Background(), appTool.BatchRequest{
			SessionID: "parent", Turn: 1, Step: 1, Journal: nopJournal{},
			Calls: []session.ToolCall{{ID: "call", Name: name, Arguments: json.RawMessage(arguments)}},
		})[0]
	}
}

type nopJournal struct{}

func (nopJournal) Append(context.Context, session.Record) (session.Event, error) {
	return session.Event{}, nil
}

func TestProvider_ValidatesRegistersAndCleansTools(t *testing.T) {
	runtime, _ := appTool.New(allowApprover{})
	service := &fakeService{}
	if _, err := New(nil, service); err == nil {
		t.Fatal("nil runtime accepted")
	}
	if _, err := New(runtime, nil); err == nil {
		t.Fatal("nil service accepted")
	}
	provider, err := New(runtime, service)
	if err != nil || provider.ID() != "subagent-tools" {
		t.Fatalf("provider = %+v, %v", provider, err)
	}
	if err := provider.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, appTool.ErrNotRunning) {
		t.Fatalf("inactive runtime error = %v", err)
	}
	runtimeScope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := provider.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed provider scope error = %v", err)
	}
	providerScope := &plugin.Scope{}
	if err := provider.Start(context.Background(), providerScope); err != nil {
		t.Fatal(err)
	}
	catalog, err := runtime.Catalog(nil)
	definitions := catalog.Definitions
	want := []string{"list_subagents", "spawn_subagent", "subagent_followup", "subagent_interrupt", "subagent_report"}
	if err != nil || len(definitions) != len(want) {
		t.Fatalf("definitions = %#v, %v", definitions, err)
	}
	for index := range want {
		if definitions[index].Name != want[index] {
			t.Fatalf("definitions = %#v", definitions)
		}
	}
	if err := providerScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	catalog, _ = runtime.Catalog(nil)
	if len(catalog.Definitions) != 0 {
		t.Fatalf("tools retained: %#v", definitions)
	}
}

func TestTools_PublishSchemasAndRejectInvalidArguments(t *testing.T) {
	runtime, _ := appTool.New(allowApprover{})
	scope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	provider, _ := New(runtime, &fakeService{})
	if err := provider.Start(context.Background(), &plugin.Scope{}); err != nil {
		t.Fatal(err)
	}
	catalog, _ := runtime.Catalog([]string{"spawn_subagent"})
	want := `{"type":"object","properties":{"label":{"type":"string"},"task":{"type":"string"},"mode":{"type":"string","enum":["one-shot","continuable"]},"persona":{"type":"string"},"tools":{"type":"array","items":{"type":"string"}},"fork":{"type":"boolean"}},"required":["label","task","mode"]}`
	if len(catalog.Definitions) != 1 || string(catalog.Definitions[0].Parameters) != want || len(catalog.Guidance) != 0 {
		t.Fatalf("spawn schema = %#v", catalog)
	}
	call := startTools(t, &fakeService{})
	for name, arguments := range map[string]string{
		"spawn_subagent":     `{"label":"x","task":"t","mode":"forever"}`,
		"subagent_followup":  `{"session_id":"child"}`,
		"subagent_interrupt": `{}`,
		"subagent_report":    `{"session_id":1}`,
		"list_subagents":     `{"extra":true}`,
	} {
		if result := call(name, arguments); !result.IsError || !strings.Contains(result.Output, "invalid arguments") {
			t.Errorf("%s accepted %s: %#v", name, arguments, result)
		}
	}
}

func TestSpawnTool_DelegatesWaitsAndPropagatesFailures(t *testing.T) {
	service := &fakeService{}
	call := startTools(t, service)
	service.spawnErr = errors.New("spawn")
	arguments := `{"label":"worker","task":"task","mode":"continuable","persona":"focus","tools":["read"],"fork":true}`
	if result := call("spawn_subagent", arguments); !result.IsError || !strings.Contains(result.Output, "spawn") {
		t.Fatalf("spawn error = %#v", result)
	}
	service.spawnErr = nil
	service.spawnInfo = appSubagent.Info{SessionID: "child"}
	service.waitErr = errors.New("wait failed")
	if result := call("spawn_subagent", arguments); !result.IsError || !strings.Contains(result.Output, "wait failed") {
		t.Fatalf("wait error = %#v", result)
	}
	service.waitErr = nil
	service.waitInfo = sampleInfo("child", "report")
	result := call("spawn_subagent", arguments)
	if result.IsError || !strings.Contains(result.Output, "session=child") || service.waitID != "child" {
		t.Fatalf("result = %#v, request = %+v, wait=%q", result, service.spawnRequest, service.waitID)
	}
	request := service.spawnRequest
	if request.ParentSessionID != "parent" || !request.Fork || request.Tools[0] != "read" || request.Persona != "focus" || request.Mode != "continuable" {
		t.Fatalf("spawn request = %+v", request)
	}
	call("spawn_subagent", `{"label":"bare","task":"task","mode":"one-shot"}`)
	if bare := service.spawnRequest; bare.Fork || bare.Persona != "" || bare.Tools != nil {
		t.Fatalf("defaults = %+v", bare)
	}
}

func TestFollowupInterruptReportAndListTools(t *testing.T) {
	service := &fakeService{}
	call := startTools(t, service)

	service.followErr = errors.New("follow")
	if result := call("subagent_followup", `{"session_id":"child","task":"next"}`); !result.IsError || !strings.Contains(result.Output, "follow") {
		t.Fatalf("followup error = %#v", result)
	}
	service.followErr = nil
	service.followInfo = sampleInfo("child", "next report")
	result := call("subagent_followup", `{"session_id":"child","task":"next"}`)
	if result.IsError || !strings.Contains(result.Output, "next report") || service.followCaller != "parent" || service.followID != "child" || service.followTask != "next" {
		t.Fatalf("followup result = %#v service=%+v", result, service)
	}

	service.interruptErr = errors.New("interrupt")
	if result := call("subagent_interrupt", `{"session_id":"child"}`); !result.IsError || !strings.Contains(result.Output, "interrupt") {
		t.Fatalf("interrupt error = %#v", result)
	}
	service.interruptErr = nil
	if result := call("subagent_interrupt", `{"session_id":"child"}`); result.IsError || result.Output != "subagent interrupted" || service.interruptIDs != [2]string{"parent", "child"} {
		t.Fatalf("interrupt result = %#v ids=%#v", result, service.interruptIDs)
	}

	service.reportErr = errors.New("report")
	if result := call("subagent_report", `{"session_id":"child"}`); !result.IsError || !strings.Contains(result.Output, "report") {
		t.Fatalf("report error = %#v", result)
	}
	service.reportErr = nil
	service.reportInfo = sampleInfo("child", "current")
	if result := call("subagent_report", `{"session_id":"child"}`); result.IsError || !strings.Contains(result.Output, "current") || service.reportID != "child" {
		t.Fatalf("report result = %#v id=%q", result, service.reportID)
	}

	service.listErr = errors.New("list")
	if result := call("list_subagents", `{}`); !result.IsError || !strings.Contains(result.Output, "list") {
		t.Fatalf("list error = %#v", result)
	}
	service.listErr = nil
	if result := call("list_subagents", `{}`); result.IsError || result.Output != "no subagents" || service.listParent != "parent" {
		t.Fatalf("empty list = %#v parent=%q", result, service.listParent)
	}
	service.listInfos = []appSubagent.Info{sampleInfo("one", "first"), sampleInfo("two", "second")}
	if result := call("list_subagents", `{}`); result.IsError || !strings.Contains(result.Output, "session=one") || !strings.Contains(result.Output, "\nsession=two") {
		t.Fatalf("list result = %#v", result)
	}
}

func TestFormatInfo_DistinguishesIdleBusyAndPending(t *testing.T) {
	idle := sampleInfo("idle", "done")
	if output := formatInfo(idle); !strings.Contains(output, "status=idle") || !strings.Contains(output, "outcome=completed") {
		t.Fatalf("idle = %q", output)
	}
	busy := idle
	busy.Busy = true
	if output := formatInfo(busy); !strings.Contains(output, "status=running") {
		t.Fatalf("busy = %q", output)
	}
	pending := idle
	pending.Pending = 1
	if output := formatInfo(pending); !strings.Contains(output, "status=running") {
		t.Fatalf("pending = %q", output)
	}
}

func sampleInfo(id, text string) appSubagent.Info {
	return appSubagent.Info{SessionID: id, ParentID: "parent", Label: "worker", Mode: "continuable", Depth: 1, Last: agent.TurnResult{Outcome: session.OutcomeCompleted, Text: text}}
}
