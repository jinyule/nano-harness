package question

import (
	"context"
	"encoding/json"
	"testing"

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

type harness struct {
	runtime       *appTool.Runtime
	providerScope *plugin.Scope
	seen          []appQuestion.Request
	answer        func(appQuestion.Request) ([]appQuestion.Answer, error)
}

// start composes the real tool runtime, question service, and provider.
func start(t *testing.T) *harness {
	t.Helper()
	current := &harness{}
	runtime, _ := appTool.New(denyApprover{})
	service := appQuestion.New()
	scopes := []*plugin.Scope{{}, {}, {}, {}}
	t.Cleanup(func() {
		for _, scope := range scopes {
			_ = scope.Close(context.Background())
		}
	})
	if err := runtime.Start(t.Context(), scopes[3]); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(t.Context(), scopes[2]); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterBroker(brokerFunc(func(_ context.Context, request appQuestion.Request) ([]appQuestion.Answer, error) {
		current.seen = append(current.seen, request)
		return current.answer(request)
	}), scopes[1]); err != nil {
		t.Fatal(err)
	}
	provider, err := New(runtime, service)
	if err != nil {
		t.Fatal(err)
	}
	if provider.ID() != "question-tools" {
		t.Fatalf("ID = %q", provider.ID())
	}
	if err := provider.Start(t.Context(), scopes[0]); err != nil {
		t.Fatal(err)
	}
	current.runtime, current.providerScope = runtime, scopes[0]
	return current
}

func (current *harness) call(t *testing.T, arguments string, delegated bool) session.ToolResult {
	t.Helper()
	results := current.runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{
		SessionID: "root", Turn: 1, Step: 1, Delegated: delegated,
		Calls: []session.ToolCall{{ID: "call-1", Name: "ask_user_question", Arguments: json.RawMessage(arguments)}},
	})
	return results[0]
}

func TestNew_RequiresDependencies(t *testing.T) {
	runtime, _ := appTool.New(denyApprover{})
	if _, err := New(nil, appQuestion.New()); err == nil {
		t.Fatal("nil runtime accepted")
	}
	if _, err := New(runtime, nil); err == nil {
		t.Fatal("nil asker accepted")
	}
}

func TestAskUserQuestion_DefinitionMatchesUpstreamBlockingSchema(t *testing.T) {
	current := start(t)
	catalog, err := current.runtime.Catalog(nil)
	if err != nil || len(catalog.Definitions) != 1 || len(catalog.Guidance) != 0 {
		t.Fatalf("catalog = %+v, %v", catalog, err)
	}
	const want = `{"type":"object","properties":{"questions":{"type":"array","description":"Questions to ask the user before continuing.","items":{"type":"object","additionalProperties":true,"properties":{"id":{"type":"string","description":"Stable id for this question; echoed in the answer."},"question":{"type":"string","description":"The specific question to ask the user."},"header":{"type":"string","description":"Optional short heading for the question, such as \"Confirm\" or \"Choose Mode\"."},"options":{"type":"array","description":"Optional choices to show the user. If you recommend one, put it first and append \"(Recommended)\" to that label.","items":{"type":"object","additionalProperties":true,"properties":{"label":{"type":"string","description":"Short user-facing option label."},"description":{"type":"string","description":"One sentence explaining the tradeoff or impact."}},"required":["label"]}},"multi_select":{"type":"boolean","description":"Whether the user may select more than one option. Defaults to false."}},"required":["id","question"]}}},"required":["questions"]}`
	if got := string(catalog.Definitions[0].Parameters); got != want {
		t.Fatalf("parameters\n got: %s\nwant: %s", got, want)
	}
	if err := current.providerScope.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if catalog, _ := current.runtime.Catalog(nil); len(catalog.Definitions) != 0 {
		t.Fatal("registration survived scope cleanup")
	}
}

func TestAskUserQuestion_ReturnsTheAnswerBatch(t *testing.T) {
	current := start(t)
	current.answer = func(appQuestion.Request) ([]appQuestion.Answer, error) {
		return []appQuestion.Answer{
			{ID: "areas", Selected: []string{"API <v2>", "UI & docs"}, Custom: "and tests"},
			{ID: "why"},
		}, nil
	}
	result := current.call(t, `{"questions":[
		{"id":"why","question":"Why?","extra":"kept open"},
		{"id":"areas","question":"Which areas?","header":"Scope","multi_select":true,"options":[{"label":"API <v2>","description":"Server","note":"open"},{"label":"UI & docs"}]}
	]}`, false)
	if result.IsError || result.Output != `{"answers":[{"id":"why","selected":[]},{"id":"areas","selected":["API <v2>","UI & docs"],"custom":"and tests"}]}` {
		t.Fatalf("result = %+v", result)
	}
	request := current.seen[0]
	areas := request.Questions[1]
	if request.SessionID != "root" || request.CallID != "call-1" || request.Delegated || areas.Header != "Scope" || !areas.MultiSelect || areas.Options[0].Description != "Server" || areas.Text != "Which areas?" {
		t.Fatalf("request = %+v", request)
	}
}

func TestAskUserQuestion_FailuresBecomeErrorResults(t *testing.T) {
	current := start(t)
	current.answer = func(appQuestion.Request) ([]appQuestion.Answer, error) { return nil, appQuestion.ErrCancelled }
	for _, test := range []struct {
		name      string
		arguments string
		delegated bool
		want      string
	}{
		{"cancelled", `{"questions":[{"id":"a","question":"?"}]}`, false, "Error: the user cancelled ask_user_question"},
		{"delegated", `{"questions":[{"id":"a","question":"?"}]}`, true, "Error: " + appQuestion.ErrDelegated.Error()},
		{"empty", `{"questions":[]}`, false, "Error: ask_user_question requires at least one question"},
		{"duplicate", `{"questions":[{"id":"a","question":"?"},{"id":"a","question":"?"}]}`, false, `Error: question id "a" must be unique within this call`},
		{"schema", `{"questions":[{"id":"a"}]}`, false, `Error: invalid arguments: missing required property "questions[0].question"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if result := current.call(t, test.arguments, test.delegated); !result.IsError || result.Output != test.want {
				t.Fatalf("result = %+v", result)
			}
		})
	}
	if len(current.seen) != 1 {
		t.Fatalf("broker saw %d requests; only the cancelled one is valid", len(current.seen))
	}
}

func TestRender_EncodesLikeJSONStringify(t *testing.T) {
	got := render([]appQuestion.Answer{{ID: "a&b", Selected: []string{"<x>"}}})
	if got != `{"answers":[{"id":"a&b","selected":["<x>"]}]}` {
		t.Fatalf("render = %s", got)
	}
}
