package question

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
)

type brokerFunc func(context.Context, Request) ([]Answer, error)

func (ask brokerFunc) Ask(ctx context.Context, request Request) ([]Answer, error) {
	return ask(ctx, request)
}

func startService(t *testing.T) (*Service, *plugin.Scope) {
	t.Helper()
	service := New()
	scope := &plugin.Scope{}
	if service.ID() != "user-questions" {
		t.Fatalf("ID = %q", service.ID())
	}
	if err := service.Start(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return service, scope
}

func closedScope(t *testing.T) *plugin.Scope {
	t.Helper()
	scope := &plugin.Scope{}
	if err := scope.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	return scope
}

func choice() Question {
	return Question{ID: "mode", Text: "Which mode?", Header: "Choose Mode", Options: []Option{{Label: "Fast (Recommended)", Description: "Less work"}, {Label: "Thorough"}}}
}

func answering(answers ...Answer) Broker {
	return brokerFunc(func(context.Context, Request) ([]Answer, error) { return answers, nil })
}

func TestService_LifecycleAndBrokerRegistration(t *testing.T) {
	inactive := New()
	if err := inactive.RegisterBroker(answering(), &plugin.Scope{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("register before start = %v", err)
	}
	if err := inactive.Start(t.Context(), closedScope(t)); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("start with closed scope = %v", err)
	}
	service, scope := startService(t)
	if err := service.Start(t.Context(), &plugin.Scope{}); !errors.Is(err, ErrInvalidBroker) {
		t.Fatalf("double start = %v", err)
	}
	if err := service.RegisterBroker(nil, &plugin.Scope{}); !errors.Is(err, ErrInvalidBroker) {
		t.Fatalf("nil broker = %v", err)
	}
	if err := service.RegisterBroker(answering(), nil); !errors.Is(err, ErrInvalidBroker) {
		t.Fatalf("nil scope = %v", err)
	}
	if err := service.RegisterBroker(answering(), closedScope(t)); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed broker scope = %v", err)
	}
	request := Request{SessionID: "s", Questions: []Question{choice()}}
	if _, err := service.Ask(t.Context(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("failed registration left a broker: %v", err)
	}
	brokerScope := &plugin.Scope{}
	if err := service.RegisterBroker(answering(Answer{ID: "mode", Selected: []string{"Thorough"}}), brokerScope); err != nil {
		t.Fatal(err)
	}
	if err := service.RegisterBroker(answering(), &plugin.Scope{}); !errors.Is(err, ErrInvalidBroker) {
		t.Fatalf("duplicate broker = %v", err)
	}
	if answers, err := service.Ask(t.Context(), request); err != nil || answers[0].Selected[0] != "Thorough" {
		t.Fatalf("answers = %+v, %v", answers, err)
	}
	if err := brokerScope.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Ask(t.Context(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("withdrawn broker still answers: %v", err)
	}
	replacement := &plugin.Scope{}
	if err := service.RegisterBroker(answering(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Ask(t.Context(), request); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped service = %v", err)
	}
	if err := service.RegisterBroker(answering(), &plugin.Scope{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("register after stop = %v", err)
	}
	// The broker's own late cleanup must not clear a different registration.
	if err := replacement.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestService_AskRejectsInvalidRequestsBeforeTheBroker(t *testing.T) {
	service, _ := startService(t)
	asked := false
	if err := service.RegisterBroker(brokerFunc(func(context.Context, Request) ([]Answer, error) {
		asked = true
		return nil, nil
	}), &plugin.Scope{}); err != nil {
		t.Fatal(err)
	}
	many := make([]Question, MaxQuestions+1)
	for index := range many {
		many[index] = Question{ID: strings.Repeat("q", index+1), Text: "?"}
	}
	options := make([]Option, MaxOptions+1)
	for index := range options {
		options[index] = Option{Label: strings.Repeat("o", index+1)}
	}
	review := func(intent Intent, detail string) Question {
		question := choice()
		question.Detail, question.Intent = detail, &intent
		return question
	}
	for _, test := range []struct {
		name      string
		questions []Question
		want      string
	}{
		{"empty", nil, "ask_user_question requires at least one question"},
		{"too many", many, "ask_user_question accepts at most 16 questions"},
		{"blank id", []Question{{ID: "", Text: "?"}}, "question ids must be trimmed single-line text of 1-128 bytes"},
		{"untrimmed id", []Question{{ID: " id", Text: "?"}}, "question ids must be trimmed single-line text of 1-128 bytes"},
		{"multiline id", []Question{{ID: "a\nb", Text: "?"}}, "question ids must be trimmed single-line text of 1-128 bytes"},
		{"long id", []Question{{ID: strings.Repeat("x", MaxIDBytes+1), Text: "?"}}, "question ids must be trimmed single-line text of 1-128 bytes"},
		{"duplicate id", []Question{{ID: "a", Text: "?"}, {ID: "a", Text: "?"}}, `question id "a" must be unique within this call`},
		{"blank text", []Question{{ID: "a", Text: " "}}, "question a needs non-empty question text"},
		{"too many options", []Question{{ID: "a", Text: "?", Options: options}}, "question a offers more than 32 options"},
		{"blank label", []Question{{ID: "a", Text: "?", Options: []Option{{Label: " "}}}}, "question a has an option without a label"},
		{"duplicate label", []Question{{ID: "a", Text: "?", Options: []Option{{Label: "x"}, {Label: "x"}}}}, `question a repeats option label "x"`},
		{"unknown intent", []Question{review(Intent{Kind: "other", Approve: "Thorough"}, "plan")}, `question mode declares unknown intent "other"`},
		{"approve not offered", []Question{review(Intent{Kind: IntentPlanReview, Approve: "Yes"}, "plan")}, `question mode declares intent plan-review whose approve label "Yes" names none of its options`},
		{"review without detail", []Question{review(Intent{Kind: IntentPlanReview, Approve: "Thorough"}, "")}, "question mode declares intent plan-review without the detail it reviews"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.Ask(t.Context(), Request{SessionID: "s", Questions: test.questions})
			var requestErr *RequestError
			if !errors.As(err, &requestErr) || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	if asked {
		t.Fatal("an invalid request reached the broker")
	}
}

func TestService_AskFailsClosed(t *testing.T) {
	service, _ := startService(t)
	var answer func(context.Context, Request) ([]Answer, error)
	if err := service.RegisterBroker(brokerFunc(func(ctx context.Context, request Request) ([]Answer, error) {
		return answer(ctx, request)
	}), &plugin.Scope{}); err != nil {
		t.Fatal(err)
	}
	request := Request{SessionID: "s", CallID: "call", Questions: []Question{choice()}}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.Ask(canceled, request); !errors.Is(err, ErrAborted) {
		t.Fatalf("canceled before asking = %v", err)
	}
	delegated := request
	delegated.Delegated = true
	if _, err := service.Ask(t.Context(), delegated); !errors.Is(err, ErrDelegated) {
		t.Fatalf("delegated = %v", err)
	}
	answer = func(context.Context, Request) ([]Answer, error) { return nil, ErrCancelled }
	if _, err := service.Ask(t.Context(), request); !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancelled = %v", err)
	}
	answer = func(context.Context, Request) ([]Answer, error) { return nil, errors.New("terminal gone") }
	if _, err := service.Ask(t.Context(), request); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("broker failure = %v", err)
	}
	interrupted, stop := context.WithCancel(t.Context())
	answer = func(ctx context.Context, _ Request) ([]Answer, error) {
		stop()
		<-ctx.Done()
		return nil, ErrCancelled
	}
	if _, err := service.Ask(interrupted, request); !errors.Is(err, ErrAborted) {
		t.Fatalf("interrupted wait = %v", err)
	}
	multi := choice()
	multi.MultiSelect = true
	free := Question{ID: "why", Text: "Why?"}
	for name, test := range map[string]struct {
		questions []Question
		answers   []Answer
	}{
		"missing":             {[]Question{choice()}, nil},
		"extra":               {[]Question{choice()}, []Answer{{ID: "mode"}, {ID: "other"}}},
		"duplicate":           {[]Question{choice(), free}, []Answer{{ID: "mode"}, {ID: "mode"}}},
		"unknown id":          {[]Question{choice()}, []Answer{{ID: "other"}}},
		"label not offered":   {[]Question{choice()}, []Answer{{ID: "mode", Selected: []string{"Other"}}}},
		"two single choices":  {[]Question{choice()}, []Answer{{ID: "mode", Selected: []string{"Thorough", "Fast (Recommended)"}}}},
		"single with custom":  {[]Question{choice()}, []Answer{{ID: "mode", Selected: []string{"Thorough"}, Custom: "x"}}},
		"repeated selection":  {[]Question{multi}, []Answer{{ID: "mode", Selected: []string{"Thorough", "Thorough"}}}},
		"selection on free":   {[]Question{free}, []Answer{{ID: "why", Selected: []string{"x"}}}},
		"oversized custom":    {[]Question{free}, []Answer{{ID: "why", Custom: strings.Repeat("x", MaxCustomBytes+1)}}},
		"invalid UTF-8 input": {[]Question{free}, []Answer{{ID: "why", Custom: "\xff"}}},
	} {
		t.Run(name, func(t *testing.T) {
			answer = func(context.Context, Request) ([]Answer, error) { return test.answers, nil }
			if _, err := service.Ask(t.Context(), Request{SessionID: "s", Questions: test.questions}); !errors.Is(err, ErrInvalidAnswer) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestService_AskReturnsDetachedAnswersInRequestOrder(t *testing.T) {
	service, _ := startService(t)
	multi := Question{ID: "areas", Text: "Which areas?", MultiSelect: true, Options: []Option{{Label: "API"}, {Label: "UI"}}}
	review := Question{ID: "plan-review", Text: "Approve?", Detail: "# Plan", Options: []Option{{Label: "Approve"}, {Label: "Keep planning"}}, Intent: &Intent{Kind: IntentPlanReview, Approve: "Approve"}}
	free := Question{ID: "why", Text: "Why?"}
	var seen Request
	selected := []string{"UI", "API"}
	if err := service.RegisterBroker(brokerFunc(func(_ context.Context, request Request) ([]Answer, error) {
		seen = request
		request.Questions[0].Options[0].Label = "mutated"
		request.Questions[1].Intent.Approve = "mutated"
		return []Answer{
			{ID: "why"},
			{ID: "plan-review", Custom: "add tests"},
			{ID: "areas", Selected: selected, Custom: "and docs"},
		}, nil
	}), &plugin.Scope{}); err != nil {
		t.Fatal(err)
	}
	request := Request{SessionID: "s", CallID: "call", Questions: []Question{multi, review, free}}
	answers, err := service.Ask(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if seen.SessionID != "s" || seen.CallID != "call" || request.Questions[0].Options[0].Label != "API" || request.Questions[1].Intent.Approve != "Approve" {
		t.Fatalf("broker saw %+v and could mutate the caller's request %+v", seen, request)
	}
	want := []Answer{{ID: "areas", Selected: []string{"UI", "API"}, Custom: "and docs"}, {ID: "plan-review", Selected: []string{}, Custom: "add tests"}, {ID: "why", Selected: []string{}}}
	for index := range want {
		if answers[index].ID != want[index].ID || !slices.Equal(answers[index].Selected, want[index].Selected) || answers[index].Selected == nil || answers[index].Custom != want[index].Custom {
			t.Fatalf("answers = %+v", answers)
		}
	}
	selected[0] = "mutated"
	if answers[0].Selected[0] != "UI" {
		t.Fatal("answers alias the broker's slices")
	}
}
