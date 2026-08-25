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

func toolExecution(sessionID, arguments string) appTool.Execution {
	return appTool.Execution{SessionID: sessionID, Arguments: json.RawMessage(arguments)}
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
	definitions, err := runtime.Definitions(nil)
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
	definitions, _ = runtime.Definitions(nil)
	if len(definitions) != 0 {
		t.Fatalf("tools retained: %#v", definitions)
	}
}

func TestDecodeAndDefinition_RejectUnknownMalformedAndTrailingValues(t *testing.T) {
	var target struct {
		Value string `json:"value"`
	}
	if err := decode(json.RawMessage(`{"value":"ok"}`), &target); err != nil || target.Value != "ok" {
		t.Fatalf("decode = %+v, %v", target, err)
	}
	for _, raw := range []string{`{`, `{"unknown":true}`, `{"value":"ok"} {}`} {
		if err := decode(json.RawMessage(raw), &target); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	value := definition("name", "description", `{"type":"object"}`)
	if value.Name != "name" || value.Description != "description" || string(value.Parameters) != `{"type":"object"}` {
		t.Fatalf("definition = %#v", value)
	}
}

func TestSpawnTool_DelegatesWaitsAndPropagatesFailures(t *testing.T) {
	service := &fakeService{}
	tool := spawnTool{service: service}
	if tool.Definition().Name != "spawn_subagent" || tool.Concurrency() != appTool.ConcurrencyParallel || tool.ApprovalReason(nil) != "" {
		t.Fatal("spawn metadata is invalid")
	}
	if _, err := tool.Execute(context.Background(), toolExecution("parent", `{`)); err == nil {
		t.Fatal("invalid arguments accepted")
	}
	service.spawnErr = errors.New("spawn")
	arguments := `{"label":"worker","task":"task","mode":"continuable","persona":"focus","tools":["read_file"],"fork":true}`
	if _, err := tool.Execute(context.Background(), toolExecution("parent", arguments)); !errors.Is(err, service.spawnErr) {
		t.Fatalf("spawn error = %v", err)
	}
	service.spawnErr = nil
	service.spawnInfo = appSubagent.Info{SessionID: "child"}
	service.waitErr = errors.New("wait")
	if _, err := tool.Execute(context.Background(), toolExecution("parent", arguments)); !errors.Is(err, service.waitErr) {
		t.Fatalf("wait error = %v", err)
	}
	service.waitErr = nil
	service.waitInfo = sampleInfo("child", "report")
	output, err := tool.Execute(context.Background(), toolExecution("parent", arguments))
	if err != nil || !strings.Contains(output, "session=child") || service.waitID != "child" {
		t.Fatalf("output = %q, request = %+v, wait=%q, error=%v", output, service.spawnRequest, service.waitID, err)
	}
	if service.spawnRequest.ParentSessionID != "parent" || !service.spawnRequest.Fork || service.spawnRequest.Tools[0] != "read_file" {
		t.Fatalf("spawn request = %+v", service.spawnRequest)
	}
}

func TestFollowupInterruptReportAndListTools(t *testing.T) {
	service := &fakeService{}
	followup := followupTool{service: service}
	interrupt := interruptTool{service: service}
	report := reportTool{service: service}
	list := listTool{service: service}
	for name, metadata := range map[string]struct {
		definition session.ToolDefinition
		parallel   appTool.Concurrency
		reason     string
	}{
		"followup":  {followup.Definition(), followup.Concurrency(), followup.ApprovalReason(nil)},
		"interrupt": {interrupt.Definition(), interrupt.Concurrency(), interrupt.ApprovalReason(nil)},
		"report":    {report.Definition(), report.Concurrency(), report.ApprovalReason(nil)},
		"list":      {list.Definition(), list.Concurrency(), list.ApprovalReason(nil)},
	} {
		if metadata.definition.Name == "" || metadata.parallel != appTool.ConcurrencyParallel || metadata.reason != "" {
			t.Fatalf("%s metadata = %+v", name, metadata)
		}
	}
	for _, candidate := range []interface {
		Execute(context.Context, appTool.Execution) (string, error)
	}{followup, interrupt, report} {
		if _, err := candidate.Execute(context.Background(), toolExecution("parent", `{`)); err == nil {
			t.Fatal("invalid arguments accepted")
		}
	}

	service.followErr = errors.New("follow")
	if _, err := followup.Execute(context.Background(), toolExecution("parent", `{"session_id":"child","task":"next"}`)); !errors.Is(err, service.followErr) {
		t.Fatalf("followup error = %v", err)
	}
	service.followErr = nil
	service.followInfo = sampleInfo("child", "next report")
	output, err := followup.Execute(context.Background(), toolExecution("parent", `{"session_id":"child","task":"next"}`))
	if err != nil || !strings.Contains(output, "next report") || service.followCaller != "parent" || service.followID != "child" || service.followTask != "next" {
		t.Fatalf("followup output = %q service=%+v error=%v", output, service, err)
	}

	service.interruptErr = errors.New("interrupt")
	if _, err := interrupt.Execute(context.Background(), toolExecution("parent", `{"session_id":"child"}`)); !errors.Is(err, service.interruptErr) {
		t.Fatalf("interrupt error = %v", err)
	}
	service.interruptErr = nil
	if output, err := interrupt.Execute(context.Background(), toolExecution("parent", `{"session_id":"child"}`)); err != nil || output != "subagent interrupted" || service.interruptIDs != [2]string{"parent", "child"} {
		t.Fatalf("interrupt output = %q ids=%#v error=%v", output, service.interruptIDs, err)
	}

	service.reportErr = errors.New("report")
	if _, err := report.Execute(context.Background(), toolExecution("parent", `{"session_id":"child"}`)); !errors.Is(err, service.reportErr) {
		t.Fatalf("report error = %v", err)
	}
	service.reportErr = nil
	service.reportInfo = sampleInfo("child", "current")
	if output, err := report.Execute(context.Background(), toolExecution("parent", `{"session_id":"child"}`)); err != nil || !strings.Contains(output, "current") || service.reportID != "child" {
		t.Fatalf("report output = %q id=%q error=%v", output, service.reportID, err)
	}

	service.listErr = errors.New("list")
	if _, err := list.Execute(context.Background(), toolExecution("parent", `{}`)); !errors.Is(err, service.listErr) {
		t.Fatalf("list error = %v", err)
	}
	service.listErr = nil
	if output, err := list.Execute(context.Background(), toolExecution("parent", `{}`)); err != nil || output != "no subagents" || service.listParent != "parent" {
		t.Fatalf("empty list = %q parent=%q error=%v", output, service.listParent, err)
	}
	service.listInfos = []appSubagent.Info{sampleInfo("one", "first"), sampleInfo("two", "second")}
	if output, err := list.Execute(context.Background(), toolExecution("parent", `{}`)); err != nil || !strings.Contains(output, "session=one") || !strings.Contains(output, "\nsession=two") {
		t.Fatalf("list output = %q, error = %v", output, err)
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
