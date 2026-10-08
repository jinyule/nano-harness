package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// scriptedAdmission records each call and either opens the turn or returns err.
type scriptedAdmission struct {
	admit    bool
	err      error
	messages []session.Message
	journals []Journal
}

func (admission *scriptedAdmission) Admit(ctx context.Context, journal Journal, message session.Message, open func(context.Context) error) error {
	admission.messages = append(admission.messages, message)
	admission.journals = append(admission.journals, journal)
	if admission.admit {
		if err := open(ctx); err != nil {
			return err
		}
	}
	return admission.err
}

func roundMessage(text string) session.Message {
	return session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "round"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}
}

func TestEngine_RegisterAdmissionValidatesAndWithdraws(t *testing.T) {
	harness := startEngineHarness(t, 1)
	scope := &plugin.Scope{}
	admission := &scriptedAdmission{}
	for name, test := range map[string]struct {
		kind      string
		admission Admission
		scope     *plugin.Scope
	}{
		"empty kind":    {"", admission, scope},
		"nil admission": {"round", nil, scope},
		"nil scope":     {"round", admission, nil},
	} {
		if err := harness.engine.RegisterAdmission(test.kind, test.admission, test.scope); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := harness.engine.RegisterAdmission("round", admission, scope); err != nil {
		t.Fatal(err)
	}
	if err := harness.engine.RegisterAdmission("round", &scriptedAdmission{}, &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("duplicate kind = %v", err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	replacement, replacementScope := &scriptedAdmission{}, &plugin.Scope{}
	if err := harness.engine.RegisterAdmission("round", replacement, replacementScope); err != nil {
		t.Fatalf("withdrawn kind not reusable: %v", err)
	}
	// A stale withdrawal never removes the newer registration.
	var stale Admission = admission
	harness.engine.removeAdmission("round", &stale)
	if harness.engine.admissions["round"] == nil {
		t.Fatal("stale withdrawal removed a newer admission")
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := harness.engine.RegisterAdmission("other", admission, closed); !errors.Is(err, plugin.ErrScopeClosed) || harness.engine.admissions["other"] != nil {
		t.Fatalf("closed scope = %v, left %v", err, harness.engine.admissions["other"])
	}
	_ = replacementScope.Close(context.Background())
	for _, scope := range harness.scopes {
		_ = scope.Close(context.Background())
	}
	if err := harness.engine.RegisterAdmission("round", admission, &plugin.Scope{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stopped engine = %v", err)
	}
}

func TestEngine_AdmissionGatesOnlyItsKind(t *testing.T) {
	harness := startEngineHarness(t, 1,
		modelAction{completion: assistantCompletion("admitted")},
		modelAction{completion: assistantCompletion("ordinary")},
	)
	admission := &scriptedAdmission{admit: true}
	scope := &plugin.Scope{}
	if err := harness.engine.RegisterAdmission("round", admission, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	journal, _ := turnJournal()
	result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: roundMessage("go"), notices: noMessages, drain: noMessages})
	if result.Err != nil || result.Outcome != session.OutcomeCompleted || result.Turn != 1 || result.Text != "admitted" {
		t.Fatalf("admitted turn = %+v", result)
	}
	if len(admission.messages) != 1 || session.Text(admission.messages[0]) != "go" || admission.journals[0] != Journal(journal) {
		t.Fatalf("admission saw %+v", admission.messages)
	}
	result = harness.engine.runTurn(context.Background(), runInput{journal: journal, message: agentMessage(session.RoleUser, "plain"), notices: noMessages, drain: noMessages})
	if result.Err != nil || result.Turn != 2 || len(admission.messages) != 1 {
		t.Fatalf("ordinary turn = %+v, admission calls %d", result, len(admission.messages))
	}
}

func TestEngine_RejectedAdmissionCommitsNothing(t *testing.T) {
	harness := startEngineHarness(t, 1)
	failure := errors.New("gate failed")
	for name, test := range map[string]struct {
		admission *scriptedAdmission
		turn      uint64
		outcome   session.TurnOutcome
		err       error
		events    int
	}{
		"stale":        {&scriptedAdmission{err: ErrNotAdmitted}, 0, "", ErrNotAdmitted, 0},
		"gate failure": {&scriptedAdmission{err: failure}, 1, session.OutcomeError, failure, 0},
		// An admission that opens and then reports staleness breaks its
		// contract; the turn it opened is still closed.
		"stale after open": {&scriptedAdmission{admit: true, err: ErrNotAdmitted}, 1, session.OutcomeError, ErrNotAdmitted, 3},
	} {
		scope := &plugin.Scope{}
		if err := harness.engine.RegisterAdmission("round", test.admission, scope); err != nil {
			t.Fatal(err)
		}
		journal, log := turnJournal()
		result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: roundMessage("go"), notices: noMessages, drain: noMessages})
		if !errors.Is(result.Err, test.err) || result.Turn != test.turn || result.Outcome != test.outcome || len(log.events) != test.events {
			t.Errorf("%s: result=%+v events=%d", name, result, len(log.events))
		}
		_ = scope.Close(context.Background())
	}
	journal, log := turnJournal()
	log.appendErr = errors.New("disk full")
	scope := &plugin.Scope{}
	if err := harness.engine.RegisterAdmission("round", &scriptedAdmission{admit: true}, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	if result := harness.engine.runTurn(context.Background(), runInput{journal: journal, message: roundMessage("go"), notices: noMessages, drain: noMessages}); result.Err == nil || result.Outcome != session.OutcomeError {
		t.Fatalf("turn/start failure = %+v", result)
	}
}

func TestAgent_DroppedTurnKeepsTheLastResult(t *testing.T) {
	harness := startEngineHarness(t, 1, modelAction{completion: assistantCompletion("done")})
	registry, _ := startRegistry(t, harness, newMemoryRepository(), newMemoryPolicy())
	root, err := registry.Create(context.Background(), CreateRequest{SessionID: "root", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	results, err := root.Submit(context.Background(), agentMessage(session.RoleUser, "work"))
	if err != nil {
		t.Fatal(err)
	}
	first := <-results
	scope := &plugin.Scope{}
	if err := harness.engine.RegisterAdmission("round", &scriptedAdmission{err: ErrNotAdmitted}, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	results, err = root.Followup(context.Background(), roundMessage("stale"))
	if err != nil {
		t.Fatal(err)
	}
	if dropped := <-results; !errors.Is(dropped.Err, ErrNotAdmitted) || dropped.Turn != 0 {
		t.Fatalf("dropped = %+v", dropped)
	}
	if err := root.WhenIdle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last := root.Status().Last; last.Turn != first.Turn || last.Outcome != session.OutcomeCompleted {
		t.Fatalf("last = %+v", last)
	}
	journal, err := registry.Journal("root")
	if err != nil || journal.Header().SessionID != "root" {
		t.Fatalf("journal = %v, %v", journal, err)
	}
	if _, err := registry.Journal("missing"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("missing journal = %v", err)
	}
}
