package plan

import (
	"context"
	"encoding/json"
	"testing"

	appPlan "github.com/jinyule/nano-harness/internal/app/plan"
	appQuestion "github.com/jinyule/nano-harness/internal/app/question"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type denyApprover struct{}

func (denyApprover) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalRejected, nil
}

type brokerFunc func(context.Context, appQuestion.Request) ([]appQuestion.Answer, error)

func (ask brokerFunc) Ask(ctx context.Context, request appQuestion.Request) ([]appQuestion.Answer, error) {
	return ask(ctx, request)
}

type memoryJournal struct{ events []session.Event }

func (*memoryJournal) Header() session.Header { return session.Header{SessionID: "root"} }
func (journal *memoryJournal) Events(context.Context) ([]session.Event, error) {
	return append([]session.Event(nil), journal.events...), nil
}
func (journal *memoryJournal) Append(_ context.Context, record session.Record) (session.Event, error) {
	event := session.Event{Sequence: uint64(len(journal.events) + 1), Record: record}
	journal.events = append(journal.events, event)
	return event, nil
}

type harness struct {
	runtime   *appTool.Runtime
	mode      *appPlan.Service
	modeScope *plugin.Scope
	journal   *memoryJournal
	seen      []appQuestion.Request
	answer    func(appQuestion.Request) ([]appQuestion.Answer, error)
}

// start composes the real tool runtime, plan mode, question service, and provider.
func start(t *testing.T) *harness {
	t.Helper()
	current := &harness{journal: &memoryJournal{}, mode: appPlan.New()}
	runtime, _ := appTool.New(denyApprover{})
	questions := appQuestion.New()
	scopes := []*plugin.Scope{{}, {}, {}, {}, {}}
	t.Cleanup(func() {
		for _, scope := range scopes {
			_ = scope.Close(context.Background())
		}
	})
	if err := runtime.Start(t.Context(), scopes[4]); err != nil {
		t.Fatal(err)
	}
	if err := current.mode.Start(t.Context(), scopes[3]); err != nil {
		t.Fatal(err)
	}
	if err := questions.Start(t.Context(), scopes[2]); err != nil {
		t.Fatal(err)
	}
	if err := questions.RegisterBroker(brokerFunc(func(_ context.Context, request appQuestion.Request) ([]appQuestion.Answer, error) {
		current.seen = append(current.seen, request)
		return current.answer(request)
	}), scopes[1]); err != nil {
		t.Fatal(err)
	}
	provider, err := New(runtime, current.mode, questions)
	if err != nil {
		t.Fatal(err)
	}
	if provider.ID() != "plan-tools" {
		t.Fatalf("ID = %q", provider.ID())
	}
	if err := provider.Start(t.Context(), scopes[0]); err != nil {
		t.Fatal(err)
	}
	current.runtime, current.modeScope = runtime, scopes[3]
	return current
}

// enter puts the root session in plan mode as a step boundary would.
func (current *harness) enter(t *testing.T) {
	t.Helper()
	if _, err := current.mode.Select(t.Context(), current.journal, true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := current.mode.Step(t.Context(), current.journal, 1); err != nil {
		t.Fatal(err)
	}
}

func (current *harness) call(t *testing.T, plan string) session.ToolResult {
	t.Helper()
	arguments, _ := json.Marshal(map[string]string{"plan": plan})
	return current.runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{
		SessionID: "root", Turn: 1, Step: 1,
		Calls: []session.ToolCall{{ID: "call-exit", Name: "exit_plan_mode", Arguments: arguments}},
	})[0]
}

func review(selected []string, custom string) func(appQuestion.Request) ([]appQuestion.Answer, error) {
	return func(appQuestion.Request) ([]appQuestion.Answer, error) {
		return []appQuestion.Answer{{ID: "plan-review", Selected: selected, Custom: custom}}, nil
	}
}

func TestExitPlanMode_ECMAScriptHeadingWhitespace(t *testing.T) {
	for _, test := range []struct {
		plan  string
		valid bool
	}{
		{"\ufeff#\ufeffPlan\ufeff", true}, {"#\u2003Plan", true}, {"#\u0085Plan", false},
		{"\u0085# Plan", false}, {"# \u0085", true}, {"#\ufeff", false},
	} {
		t.Run(test.plan, func(t *testing.T) {
			current := start(t)
			current.enter(t)
			current.answer = review([]string{"Approve"}, "")
			result := current.call(t, test.plan)
			if result.IsError == test.valid || (len(current.seen) == 1) != test.valid {
				t.Fatalf("plan=%q result=%+v reviews=%d", test.plan, result, len(current.seen))
			}
			if test.valid && current.seen[0].Questions[0].Detail != test.plan {
				t.Fatal("review changed plan text")
			}
		})
	}
}

func TestNew_RequiresDependencies(t *testing.T) {
	runtime, _ := appTool.New(denyApprover{})
	for _, test := range []struct {
		runtime *appTool.Runtime
		mode    Mode
		asker   Asker
	}{{nil, appPlan.New(), appQuestion.New()}, {runtime, nil, appQuestion.New()}, {runtime, appPlan.New(), nil}} {
		if _, err := New(test.runtime, test.mode, test.asker); err == nil {
			t.Fatalf("New(%+v) accepted", test)
		}
	}
}

func TestExitPlanMode_DefinitionMatchesUpstream(t *testing.T) {
	current := start(t)
	catalog, err := current.runtime.Catalog(nil)
	if err != nil || len(catalog.Definitions) != 1 || len(catalog.Guidance) != 0 {
		t.Fatalf("catalog = %+v, %v", catalog, err)
	}
	definition := catalog.Definitions[0]
	if definition.Description != "Use only in plan mode. Present your plan for the user's review and, on approval, leave plan mode. The user may approve (carry out the plan from your next step) or keep planning — their feedback comes back in the tool result; revise and present again." {
		t.Fatalf("description = %q", definition.Description)
	}
	if got := string(definition.Parameters); got != `{"type":"object","properties":{"plan":{"type":"string","description":"The complete plan, as markdown, starting with a # heading that names it."}},"required":["plan"]}` {
		t.Fatalf("parameters = %s", got)
	}
}

func TestExitPlanMode_RejectsOutsidePlanModeAndBadPlans(t *testing.T) {
	current := start(t)
	current.answer = review([]string{"Approve"}, "")
	if result := current.call(t, "# Plan"); !result.IsError || result.Output != "Error: exit_plan_mode is only available in plan mode" {
		t.Fatalf("outside plan mode = %+v", result)
	}
	current.enter(t)
	for _, plan := range []string{"", "   ", "#", "#Title", "## Title", "Title\n# Later", "# \n"} {
		if result := current.call(t, plan); !result.IsError || result.Output != "Error: exit_plan_mode requires a non-empty markdown plan starting with a # heading" {
			t.Fatalf("plan %q = %+v", plan, result)
		}
	}
	if len(current.seen) != 0 {
		t.Fatal("a rejected plan reached the review")
	}
	if !startsWithTitle("  # Title") || !startsWithTitle("#\nTitle") || !startsWithTitle("#　標題") {
		t.Fatal("valid titles rejected")
	}
}

func TestExitPlanMode_ApprovalLeavesAtTheNextBoundary(t *testing.T) {
	current := start(t)
	current.enter(t)
	current.answer = review([]string{"Approve"}, "")
	result := current.call(t, "# Ship it\n\n- step")
	if result.IsError || result.Output != "Plan approved — plan mode exited; carry out the plan starting with your next step." {
		t.Fatalf("approval = %+v", result)
	}
	request := current.seen[0]
	question := request.Questions[0]
	if request.SessionID != "root" || request.CallID != "call-exit" || question.ID != "plan-review" || question.Header != "Plan review" ||
		question.Text != "Approve this plan and leave plan mode?" || question.Detail != "# Ship it\n\n- step" || question.MultiSelect ||
		len(question.Options) != 2 || question.Options[0].Label != "Approve" || question.Options[1].Label != "Keep planning" ||
		question.Intent.Kind != appQuestion.IntentPlanReview || question.Intent.Approve != "Approve" {
		t.Fatalf("review request = %+v", request)
	}
	if !current.mode.Active("root") {
		t.Fatal("approval left plan mode before the boundary")
	}
	if section, err := current.mode.Step(t.Context(), current.journal, 1); err != nil || section != "" {
		t.Fatalf("boundary = %q, %v", section, err)
	}
}

func TestExitPlanMode_KeepPlanningDismissalAndFailures(t *testing.T) {
	current := start(t)
	current.enter(t)
	for _, test := range []struct {
		name   string
		answer func(appQuestion.Request) ([]appQuestion.Answer, error)
		want   string
		class  *session.ToolError
	}{
		{"keep planning", review([]string{"Keep planning"}, ""), "Error: The user chose to keep planning; revise the plan and present it again.", nil},
		{"skipped", review([]string{}, ""), "Error: The user chose to keep planning; revise the plan and present it again.", nil},
		{"feedback", review([]string{}, "split the migration"), "Error: The user chose to keep planning; their feedback: split the migration", nil},
		{"dismissed", func(appQuestion.Request) ([]appQuestion.Answer, error) { return nil, appQuestion.ErrCancelled }, "Error: The user dismissed the plan review to speak instead; stay in plan mode, stop here, and wait for their message.", nil},
		{"unavailable", func(appQuestion.Request) ([]appQuestion.Answer, error) { return nil, context.DeadlineExceeded }, "Error: no user-questions answerer accepted the request", nil},
		{"invalid answer", func(appQuestion.Request) ([]appQuestion.Answer, error) { return nil, nil }, "Error: the user-questions answerer returned an invalid answer batch", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			current.answer = test.answer
			result := current.call(t, "# Plan")
			if !result.IsError || result.Output != test.want || (result.Error == nil) != (test.class == nil) || test.class != nil && *result.Error != *test.class {
				t.Fatalf("result = %+v error=%+v", result, result.Error)
			}
			if !current.mode.Active("root") {
				t.Fatal("a declined review left plan mode")
			}
		})
	}
	current.answer = func(request appQuestion.Request) ([]appQuestion.Answer, error) {
		_ = current.modeScope.Close(context.Background())
		return review([]string{"Approve"}, "")(request)
	}
	if result := current.call(t, "# Plan"); !result.IsError || result.Output != "Error: plan mode service is not running" {
		t.Fatalf("stopped mode = %+v", result)
	}
}

// TestExitPlanMode_DelegatedCallerNeverReachesTheUser proves the tool hands
// the caller's delegation to the question service: a delegated plan review
// fails as DELEGATED_CALLER without asking, and plan mode stays active.
func TestExitPlanMode_DelegatedCallerNeverReachesTheUser(t *testing.T) {
	current := start(t)
	current.enter(t)
	current.answer = review([]string{"Approve"}, "")
	arguments, _ := json.Marshal(map[string]string{"plan": "# Plan"})
	result := current.runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{
		SessionID: "root", Turn: 1, Step: 1, Delegated: true,
		Calls: []session.ToolCall{{ID: "call-exit", Name: "exit_plan_mode", Arguments: arguments}},
	})[0]
	want := "Error: human interaction is unavailable while the calling agent is owned by another live agent; include the unresolved question or decision in the child agent's final result"
	if !result.IsError || result.Output != want || result.Error == nil || *result.Error != (session.ToolError{Name: "UserQuestionError", Code: "DELEGATED_CALLER"}) {
		t.Fatalf("delegated review = %+v error=%+v", result, result.Error)
	}
	if len(current.seen) != 0 || !current.mode.Active("root") {
		t.Fatalf("delegated review asked %d times; plan mode active=%v", len(current.seen), current.mode.Active("root"))
	}
}
