package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// contextProbe contributes fixed messages and records what it observed.
type contextProbe struct {
	kind     string
	messages int
	err      error
	after    func()
	seen     []ContextRequest
}

func (probe *contextProbe) StepContext(_ context.Context, request ContextRequest) ([]session.Message, error) {
	probe.seen = append(probe.seen, request)
	if probe.after != nil {
		defer probe.after()
	}
	if probe.err != nil {
		return nil, probe.err
	}
	messages := make([]session.Message, probe.messages)
	for index := range messages {
		messages[index] = session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: probe.kind}, Content: []session.ContentBlock{{Type: session.ContentText, Text: probe.kind}}}
	}
	return messages, nil
}

func TestEngine_RegisterContextValidatesAndFollowsScope(t *testing.T) {
	harness := startEngineHarness(t, 1)
	probe := &contextProbe{kind: "probe"}
	if err := harness.engine.RegisterContext(nil, &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil provider error = %v", err)
	}
	if err := harness.engine.RegisterContext(probe, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil scope error = %v", err)
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := harness.engine.RegisterContext(probe, closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope error = %v", err)
	}
	if len(harness.engine.contexts) != 0 {
		t.Fatal("failed registration left a contribution")
	}
	scope := &plugin.Scope{}
	if err := harness.engine.RegisterContext(probe, scope); err != nil {
		t.Fatal(err)
	}
	if err := harness.engine.RegisterContext(probe, scope); err != nil {
		t.Fatal(err)
	}
	if len(harness.engine.contexts) != 2 {
		t.Fatalf("contributions = %d", len(harness.engine.contexts))
	}
	if err := scope.Close(context.Background()); err != nil || len(harness.engine.contexts) != 0 {
		t.Fatalf("scope close = %v, remaining %d", err, len(harness.engine.contexts))
	}
	inactive, err := NewEngine(harness.llm, harness.tools, harness.retry, harness.compaction, harness.prompt, harness.plan, harness.settings, EngineConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := inactive.RegisterContext(probe, &plugin.Scope{}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive engine error = %v", err)
	}
}

func TestEngine_CommitsStepContextBeforeEveryStep(t *testing.T) {
	call := session.ToolCall{ID: "call-1", Name: "inspect", Arguments: []byte(`{}`)}
	harness := startEngineHarness(t, 3,
		modelAction{completion: assistantCompletion("using tool", call)},
		modelAction{completion: assistantCompletion("done")},
	)
	if err := harness.tools.Register((&engineTool{name: "inspect", output: "ok"}).define(), harness.toolScope); err != nil {
		t.Fatal(err)
	}
	first, second := &contextProbe{kind: "first", messages: 2}, &contextProbe{kind: "second", messages: 1}
	scope := &plugin.Scope{}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	for _, probe := range []*contextProbe{first, second} {
		if err := harness.engine.RegisterContext(probe, scope); err != nil {
			t.Fatal(err)
		}
	}
	journal, log := turnJournal()
	result := harness.engine.runTurn(context.Background(), runInput{notices: noMessages, journal: journal, message: agentMessage(session.RoleUser, "go"), drain: func() []session.Message { return nil }})
	if result.Err != nil || result.Outcome != session.OutcomeCompleted {
		t.Fatalf("result = %+v", result)
	}
	var kinds []string
	for _, event := range log.events {
		if event.Record.Type == session.RecordStepStart {
			kinds = append(kinds, "step")
		}
		if event.Record.Type != session.RecordUserMessage {
			continue
		}
		if event.Record.Turn != 1 || event.Record.Step != 0 {
			t.Fatalf("contribution placement = %+v", event.Record)
		}
		kinds = append(kinds, event.Record.Message.Source.Kind)
	}
	want := []string{"user", "first", "first", "second", "step", "first", "first", "second", "step"}
	if !slices.Equal(kinds, want) {
		t.Fatalf("contribution order = %v", kinds)
	}
	if len(second.seen) != 2 || !slices.Equal(second.seen[0].Tools, []string{"inspect"}) {
		t.Fatalf("second observations = %+v", second.seen)
	}
	observed := second.seen[0].Events
	if last := observed[len(observed)-1].Record; last.Message == nil || last.Message.Source.Kind != "first" {
		t.Fatalf("second provider did not observe the first contribution: %+v", last)
	}
	surface, err := session.Surface(log.events)
	if err != nil || len(surface) != 11 {
		t.Fatalf("surface = %d nodes, %v", len(surface), err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	harness.model.actions = append(harness.model.actions, modelAction{completion: assistantCompletion("later")})
	before := len(first.seen)
	if result := harness.engine.runTurn(context.Background(), runInput{notices: noMessages, journal: journal, message: agentMessage(session.RoleUser, "again")}); result.Err != nil || len(first.seen) != before {
		t.Fatalf("closed contribution still ran: %+v, observations %d", result, len(first.seen))
	}
}

func TestEngine_ContainsStepContextFailures(t *testing.T) {
	failure := errors.New("injected")
	for _, test := range []struct {
		name      string
		configure func(*engineHarness, *memoryLog, *contextProbe)
		outcome   session.TurnOutcome
		match     string
	}{
		{name: "provider", outcome: session.OutcomeError, match: "step context: injected", configure: func(_ *engineHarness, _ *memoryLog, probe *contextProbe) {
			probe.err = failure
		}},
		{name: "canceled", outcome: session.OutcomeCanceled, match: "context canceled", configure: func(_ *engineHarness, _ *memoryLog, probe *contextProbe) {
			probe.err = context.Canceled
		}},
		{name: "append", outcome: session.OutcomeError, match: "step context: injected", configure: func(_ *engineHarness, log *memoryLog, _ *contextProbe) {
			log.appendHook = appendFailure(session.RecordUserMessage, 2, failure)
		}},
		{name: "events", outcome: session.OutcomeError, match: "step context: injected", configure: func(harness *engineHarness, log *memoryLog, probe *contextProbe) {
			// The second provider's log read fails after the first contributed.
			probe.after = func() { log.eventsErr = failure }
			// A failed registration leaves the turn completing, which the
			// assertion below rejects.
			_ = harness.engine.RegisterContext(&contextProbe{kind: "second"}, &plugin.Scope{})
		}},
		{name: "tools", outcome: session.OutcomeError, match: "tool runtime", configure: func(harness *engineHarness, _ *memoryLog, _ *contextProbe) {
			_ = harness.toolScope.Close(context.Background())
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := startEngineHarness(t, 1, modelAction{completion: assistantCompletion("done")})
			probe := &contextProbe{kind: "probe", messages: 1}
			if err := harness.engine.RegisterContext(probe, &plugin.Scope{}); err != nil {
				t.Fatal(err)
			}
			journal, log := turnJournal()
			test.configure(harness, log, probe)
			result := harness.engine.runTurn(context.Background(), runInput{notices: noMessages, journal: journal, message: agentMessage(session.RoleUser, "go")})
			if result.Err == nil || !strings.Contains(result.Err.Error(), test.match) || result.Outcome != test.outcome {
				t.Fatalf("result = %+v", result)
			}
			if slices.Contains(recordTypes(log.events), session.RecordStepStart) {
				t.Fatal("a step opened after its context failed")
			}
		})
	}
}
