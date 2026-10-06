package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/llm"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// blockingTool signals its start and holds until its call is cancelled.
func blockingTool(started chan<- struct{}) *appTool.Tool {
	return appTool.Define(appTool.Spec[engineArguments]{
		Name: "block", Description: "blocks until cancelled", Parameters: appTool.Parameters{appTool.Optional("value", appTool.Number(""))},
		Execute: func(ctx context.Context, _ appTool.Invocation, _ engineArguments) (appTool.Result, error) {
			started <- struct{}{}
			<-ctx.Done()
			return appTool.Result{}, ctx.Err()
		},
	})
}

func turnOutcomes(events []session.Event) []session.TurnOutcome {
	var outcomes []session.TurnOutcome
	for _, event := range events {
		if event.Record.Type == session.RecordTurnEnd {
			outcomes = append(outcomes, event.Record.Outcome)
		}
	}
	return outcomes
}

// TestAgent_InterruptDuringToolKeepsNoticesAndSteers reproduces an
// interrupt that lands while a tool runs: the turn records canceled, and
// the notice and steer that arrived during the tool reach the next turn.
func TestAgent_InterruptDuringToolKeepsNoticesAndSteers(t *testing.T) {
	block := session.ToolCall{ID: "call-block", Name: "block", Arguments: json.RawMessage(`{}`)}
	inspect := session.ToolCall{ID: "call-inspect", Name: "inspect", Arguments: json.RawMessage(`{}`)}
	harness := startEngineHarness(t, 4,
		modelAction{completion: assistantCompletion("blocking", block)},
		modelAction{completion: assistantCompletion("inspecting", inspect)},
		modelAction{completion: assistantCompletion("resumed")},
	)
	started := make(chan struct{}, 1)
	scope := &plugin.Scope{}
	if err := harness.tools.Register(blockingTool(started), scope); err != nil {
		t.Fatal(err)
	}
	if err := harness.tools.Register((&engineTool{name: "inspect", output: "ok"}).define(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	registry, _ := startRegistry(t, harness, newMemoryRepository(), newMemoryPolicy())
	root, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	first, err := root.Submit(context.Background(), agentMessage(session.RoleUser, "start"))
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := root.Notify(noticeMessage("pending")); err != nil {
		t.Fatal(err)
	}
	if err := root.Steer(context.Background(), agentMessage(session.RoleUser, "steer")); err != nil {
		t.Fatal(err)
	}
	root.Interrupt()
	if result := <-first; result.Outcome != session.OutcomeCanceled || !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("interrupted = %+v", result)
	}
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status := root.Status(); status.Last.Turn != 1 {
		t.Fatalf("interrupt opened a notice turn: %+v", status)
	}
	next, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "next"))
	if result := <-next; result.Outcome != session.OutcomeCompleted || result.Text != "resumed" {
		t.Fatalf("next = %+v", result)
	}
	events, _ := root.Events(context.Background())
	if outcomes := turnOutcomes(events); !slices.Equal(outcomes, []session.TurnOutcome{session.OutcomeCanceled, session.OutcomeCompleted}) {
		t.Fatalf("turn outcomes = %q", outcomes)
	}
	// The notice opens the next turn; the steer waits for its first tool
	// boundary, exactly as for any turn that ends before draining steers.
	if texts := userTexts(events); !slices.Equal(texts, []string{"1:start", "2:next", "2:pending", "2:steer"}) {
		t.Fatalf("user messages = %q", texts)
	}
}

// TestAgent_LastToolStepLeavesNoticesForNextTurn proves a notice that
// arrives during the last allowed step is answered by a new turn instead of
// being committed into a turn that ends at its step limit.
func TestAgent_LastToolStepLeavesNoticesForNextTurn(t *testing.T) {
	block := session.ToolCall{ID: "call-block", Name: "block", Arguments: json.RawMessage(`{}`)}
	harness := startEngineHarness(t, 1,
		modelAction{completion: assistantCompletion("last step", block)},
		modelAction{completion: assistantCompletion("answered")},
	)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	tool := appTool.Define(appTool.Spec[engineArguments]{
		Name: "block", Description: "waits for release", Parameters: appTool.Parameters{appTool.Optional("value", appTool.Number(""))},
		Execute: func(context.Context, appTool.Invocation, engineArguments) (appTool.Result, error) {
			started <- struct{}{}
			<-release
			return appTool.Text("done"), nil
		},
	})
	scope := &plugin.Scope{}
	if err := harness.tools.Register(tool, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	registry, _ := startRegistry(t, harness, newMemoryRepository(), newMemoryPolicy())
	root, _ := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	results, _ := root.Submit(context.Background(), agentMessage(session.RoleUser, "start"))
	<-started
	if err := root.Notify(noticeMessage("late")); err != nil {
		t.Fatal(err)
	}
	close(release)
	if result := <-results; result.Outcome != session.OutcomeStepLimit {
		t.Fatalf("first = %+v", result)
	}
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	events, _ := root.Events(context.Background())
	if texts := userTexts(events); !slices.Equal(texts, []string{"1:start", "2:late"}) || root.Status().Last.Text != "answered" {
		t.Fatalf("user messages = %q, status = %+v", texts, root.Status())
	}
}

func TestEngine_LastToolStepTakesNoNotices(t *testing.T) {
	call := session.ToolCall{ID: "call-1", Name: "inspect", Arguments: json.RawMessage(`{}`)}
	harness := startEngineHarness(t, 1, modelAction{completion: assistantCompletion("tool", call)})
	scope := &plugin.Scope{}
	if err := harness.tools.Register((&engineTool{name: "inspect", output: "ok"}).define(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	notices, calls := queuedNotices(nil, []session.Message{noticeMessage("late")})
	steered := false
	journal, log := turnJournal()
	result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), notices: notices, drain: func() []session.Message {
		steered = true
		return []session.Message{agentMessage(session.RoleUser, "steer")}
	}})
	if result.Outcome != session.OutcomeStepLimit || *calls != 1 || !steered {
		t.Fatalf("result = %+v, notice calls = %d, steered = %v", result, *calls, steered)
	}
	// Steers still land at the last boundary; notices wait for a new turn.
	if texts := userTexts(log.events); !slices.Equal(texts, []string{"1:go", "1:steer"}) {
		t.Fatalf("user messages = %q", texts)
	}
}

// TestEngine_CancellationAtBoundariesKeepsQueuedInput cancels the turn at
// each boundary that takes queued input and requires a canceled outcome
// with nothing taken, or, when cancellation races a commit, the taken input
// committed rather than dropped.
func TestEngine_CancellationAtBoundariesKeepsQueuedInput(t *testing.T) {
	call := session.ToolCall{ID: "call-1", Name: "inspect", Arguments: json.RawMessage(`{}`)}
	truncated := assistantCompletion("partial")
	truncated.Stop = llm.StopMaxTokens
	for _, test := range []struct {
		name    string
		actions []modelAction
		// cancelOn names the record whose append cancels the turn, and
		// occurrence which append of that type.
		cancelOn   session.RecordType
		occurrence int
		notices    int
		drained    bool
		committed  []string
	}{
		{name: "turn start", actions: []modelAction{{completion: assistantCompletion("unused")}}, cancelOn: session.RecordUserMessage, occurrence: 1, committed: []string{"1:go"}},
		{name: "tool boundary", actions: []modelAction{{completion: assistantCompletion("tool", call)}}, cancelOn: session.RecordStepEnd, occurrence: 1, notices: 1, committed: []string{"1:go"}},
		{name: "completion", actions: []modelAction{{completion: assistantCompletion("done")}}, cancelOn: session.RecordStepEnd, occurrence: 1, notices: 1, committed: []string{"1:go"}},
		{name: "truncated step", actions: []modelAction{{completion: truncated}}, cancelOn: session.RecordAssistantMessage, occurrence: 1, notices: 1, committed: []string{"1:go"}},
		{name: "racing commit", actions: []modelAction{{completion: assistantCompletion("done")}}, cancelOn: session.RecordUserMessage, occurrence: 2, notices: 2, committed: []string{"1:go", "1:notice"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := startEngineHarness(t, 3, test.actions...)
			scope := &plugin.Scope{}
			if err := harness.tools.Register((&engineTool{name: "inspect", output: "ok"}).define(), scope); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = scope.Close(context.Background()) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			journal, log := turnJournal()
			seen := 0
			log.appendHook = func(record session.Record) error {
				if record.Type == test.cancelOn {
					if seen++; seen == test.occurrence {
						cancel()
					}
				}
				return nil
			}
			// Nothing waits at turn start; the completion boundary offers one.
			notices, calls := queuedNotices(nil, []session.Message{noticeMessage("notice")})
			drained := false
			result := harness.engine.runTurn(ctx, runInput{journal: journal, message: agentMessage(session.RoleUser, "go"), notices: notices, drain: func() []session.Message {
				drained = true
				return nil
			}})
			if result.Outcome != session.OutcomeCanceled || !errors.Is(result.Err, context.Canceled) {
				t.Fatalf("result = %+v", result)
			}
			if *calls != test.notices || drained != test.drained {
				t.Fatalf("notice calls = %d, drained = %v", *calls, drained)
			}
			if texts := userTexts(log.events); !slices.Equal(texts, test.committed) {
				t.Fatalf("user messages = %q", texts)
			}
			if outcomes := turnOutcomes(log.events); !slices.Equal(outcomes, []session.TurnOutcome{session.OutcomeCanceled}) {
				t.Fatalf("turn outcomes = %q", outcomes)
			}
		})
	}
}
