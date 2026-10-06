package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/plan"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// planTrace renders the plan-relevant record order of a log.
func planTrace(events []session.Event) []string {
	var trace []string
	for _, event := range events {
		record := event.Record
		switch {
		case record.Type == session.RecordPlanMode:
			trace = append(trace, "plan="+map[bool]string{true: "on", false: "off"}[record.Plan.Active])
		case record.Type == session.RecordRequestHeader && strings.Contains(record.Header.System, plan.Section):
			trace = append(trace, "header+plan")
		case record.Type == session.RecordRequestHeader:
			trace = append(trace, "header")
		case record.Type == session.RecordUserMessage && record.Message.Source.Kind == plan.NoticeSource:
			trace = append(trace, "notice:"+session.Text(*record.Message))
		case record.Type == session.RecordStepStart, record.Type == session.RecordTurnStart:
			trace = append(trace, string(record.Type))
		}
	}
	return trace
}

func registerExitTool(t *testing.T, harness *engineHarness) {
	t.Helper()
	scope := &plugin.Scope{}
	approve := appTool.Define(appTool.Spec[engineArguments]{
		Name: "approve", Description: "approves the plan", Parameters: appTool.Parameters{appTool.Optional("value", appTool.Number(""))},
		Execute: func(ctx context.Context, invocation appTool.Invocation, _ engineArguments) (appTool.Result, error) {
			if !harness.plan.Active(invocation.SessionID) {
				return appTool.Result{}, errors.New("not in plan mode")
			}
			return appTool.Text("approved"), harness.plan.Exit(ctx, invocation.SessionID)
		},
	})
	if err := harness.tools.Register(approve, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
}

func TestEngine_PlanModeChangesAtStepBoundaries(t *testing.T) {
	call := session.ToolCall{ID: "call-approve", Name: "approve", Arguments: json.RawMessage(`{}`)}
	harness := startEngineHarness(t, 3,
		modelAction{completion: assistantCompletion("plan ready", call)},
		modelAction{completion: assistantCompletion("implementing")},
	)
	registerExitTool(t, harness)
	journal, log := turnJournal()
	if change, err := harness.plan.Select(context.Background(), journal, true, false); err != nil || change != plan.Committed {
		t.Fatalf("select = %s, %v", change, err)
	}
	result := harness.engine.runTurn(context.Background(), runInput{notices: noMessages, journal: journal, message: agentMessage(session.RoleUser, "design it"), drain: func() []session.Message { return nil }})
	if result.Err != nil || result.Outcome != session.OutcomeCompleted {
		t.Fatalf("turn = %+v", result)
	}
	want := []string{"plan=on", string(session.RecordTurnStart), string(session.RecordStepStart), "header+plan", "plan=off", string(session.RecordStepStart), "header"}
	if got := planTrace(log.events); !slices.Equal(got, want) {
		t.Fatalf("trace = %v", got)
	}
	exit := slices.IndexFunc(log.events, func(event session.Event) bool {
		return event.Record.Type == session.RecordPlanMode && !event.Record.Plan.Active
	})
	if log.events[exit].Record.Turn != 1 || log.events[exit-1].Record.Type != session.RecordStepEnd {
		t.Fatalf("exit record = %+v after %s", log.events[exit].Record, log.events[exit-1].Record.Type)
	}
	if !strings.Contains(harness.model.seen[0].System, plan.Section) || strings.Contains(harness.model.seen[1].System, plan.Section) {
		t.Fatal("provider requests do not match the recorded plan policy")
	}
}

func TestEngine_PlanModeNoticeFollowsTheLastRequest(t *testing.T) {
	harness := startEngineHarness(t, 2,
		modelAction{completion: assistantCompletion("default answer")},
		modelAction{completion: assistantCompletion("planning")},
	)
	journal, log := turnJournal()
	drain := func() []session.Message { return nil }
	if result := harness.engine.runTurn(context.Background(), runInput{notices: noMessages, journal: journal, message: agentMessage(session.RoleUser, "hello"), drain: drain}); result.Err != nil {
		t.Fatal(result.Err)
	}
	if _, err := harness.plan.Select(context.Background(), journal, true, false); err != nil {
		t.Fatal(err)
	}
	if result := harness.engine.runTurn(context.Background(), runInput{notices: noMessages, journal: journal, message: agentMessage(session.RoleUser, "now plan"), drain: drain}); result.Err != nil {
		t.Fatal(result.Err)
	}
	want := []string{
		string(session.RecordTurnStart), string(session.RecordStepStart), "header",
		"plan=on", string(session.RecordTurnStart), "notice:The user switched this session to plan mode.", string(session.RecordStepStart), "header+plan",
	}
	if got := planTrace(log.events); !slices.Equal(got, want) {
		t.Fatalf("trace = %v", got)
	}
	surface, err := session.Surface(log.events)
	if err != nil || session.Text(*surface[2].Message) != "now plan" || surface[3].Message.Source.Kind != plan.NoticeSource {
		t.Fatalf("model surface = %+v, %v", surface, err)
	}
}

func TestRegistry_SetPlanModeCommitsIdleAndQueuesDuringTurns(t *testing.T) {
	release, started := make(chan struct{}), make(chan struct{}, 1)
	call := session.ToolCall{ID: "call-1", Name: "inspect", Arguments: json.RawMessage(`{}`)}
	harness := startEngineHarness(t, 3,
		modelAction{completion: assistantCompletion("checking", call), wait: release, started: started},
		modelAction{completion: assistantCompletion("planned")},
	)
	candidate := &engineTool{name: "inspect", output: "ok"}
	toolScope := &plugin.Scope{}
	if err := harness.tools.Register(candidate.define(), toolScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = toolScope.Close(context.Background()) })
	repository := newMemoryRepository()
	registry, _ := startRegistry(t, harness, repository, newMemoryPolicy())
	if _, err := registry.SetPlanMode(context.Background(), "missing", true); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("missing agent = %v", err)
	}
	root, err := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	child, err := registry.Create(context.Background(), CreateRequest{SessionID: "child", ParentID: "root", Depth: 1, Label: "child", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.SetPlanMode(context.Background(), child.Status().SessionID, true); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("delegated selection = %v", err)
	}
	results, err := root.Submit(context.Background(), agentMessage(session.RoleUser, "work"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if change, err := registry.SetPlanMode(context.Background(), "root", true); err != nil || change != plan.Queued {
		t.Fatalf("busy selection = %s, %v", change, err)
	}
	close(release)
	if result := <-results; result.Err != nil || result.Outcome != session.OutcomeCompleted {
		t.Fatalf("turn = %+v", result)
	}
	events, err := root.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{string(session.RecordTurnStart), string(session.RecordStepStart), "header", "plan=on", "notice:The user switched this session to plan mode.", string(session.RecordStepStart), "header+plan"}
	if got := planTrace(events); !slices.Equal(got, want) {
		t.Fatalf("trace = %v", got)
	}
	if change, err := registry.SetPlanMode(context.Background(), "root", false); err != nil || change != plan.Committed {
		t.Fatalf("idle selection = %s, %v", change, err)
	}
	events, _ = root.Events(context.Background())
	if last := events[len(events)-1].Record; last.Type != session.RecordPlanMode || last.Plan.Active || last.Turn != 0 {
		t.Fatalf("idle commit = %+v", last)
	}
	if err := registry.Close(context.Background(), "root"); err != nil {
		t.Fatal(err)
	}
	if _, err := root.selectPlan(context.Background(), true); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("closed agent selection = %v", err)
	}
}
