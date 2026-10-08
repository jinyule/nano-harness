package agent

import (
	"context"
	"errors"
	"reflect"
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
	seen     []ContextRequest
	sections []ContextSection
}

func (probe *contextProbe) StepContext(_ context.Context, request ContextRequest) (ContextContribution, error) {
	probe.seen = append(probe.seen, request)
	if probe.err != nil {
		return ContextContribution{}, probe.err
	}
	messages := make([]session.Message, probe.messages)
	for index := range messages {
		messages[index] = session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: probe.kind}, Content: []session.ContentBlock{{Type: session.ContentText, Text: probe.kind}}}
	}
	return ContextContribution{Sections: probe.sections, Messages: messages}, nil
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
	if !reflect.DeepEqual(observed, first.seen[0].Events) || observed[len(observed)-1].Record.Message.Source.Kind != "user" {
		t.Fatalf("providers did not observe the same committed input: %+v", observed)
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

func TestEngine_CompleteRuntimeSnapshotLifecycle(t *testing.T) {
	const scopeSection = "Delegation permission scope."
	start := func(steps int) *engineHarness {
		t.Helper()
		actions := make([]modelAction, steps)
		for i := range actions {
			actions[i] = modelAction{completion: assistantCompletion("done")}
		}
		h := startEngineHarness(t, 1, actions...)
		scope := &plugin.Scope{}
		t.Cleanup(func() { _ = scope.Close(context.Background()) })
		if err := NewSandboxContext(h.engine, "/workspace").Start(t.Context(), scope); err != nil {
			t.Fatal(err)
		}
		if err := h.engine.RegisterContext(&contextProbe{sections: []ContextSection{{Order: OrderSubagentDelegation, Text: scopeSection}}, kind: "skill-catalog", messages: 1}, scope); err != nil {
			t.Fatal(err)
		}
		return h
	}
	journal, log := turnJournal()
	run := func(h *engineHarness, mode session.SandboxMode, snapshots int) {
		t.Helper()
		result := h.engine.runTurn(t.Context(), runInput{notices: noMessages, journal: journal, message: agentMessage(session.RoleUser, "go")})
		if result.Err != nil || result.Outcome != session.OutcomeCompleted {
			t.Fatalf("result=%+v", result)
		}
		var count int
		for _, event := range log.events {
			if event.Record.Message != nil && event.Record.Message.Source.Kind == "runtime-context" {
				count++
			}
		}
		if count != snapshots {
			t.Fatalf("committed snapshots=%d, want %d", count, snapshots)
		}
		request := h.model.seen[len(h.model.seen)-1]
		var latest string
		for _, node := range request.Surface {
			if node.Message != nil && node.Message.Source.Kind == "runtime-context" {
				if node.Message.Source.Plugin != "agent-engine" || node.Message.Role != session.RoleUser {
					t.Fatalf("snapshot provenance=%+v", node.Message)
				}
				latest = session.Text(*node.Message)
			}
		}
		want := "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\n" + session.SandboxPolicyText(mode, "/workspace") + "\n\n" + scopeSection
		if latest != want {
			t.Fatalf("incomplete model context: %q", latest)
		}
		// Independent context remains after the complete snapshot and before
		// the step, even on steps where the snapshot was already retained.
		last := request.Surface[len(request.Surface)-1]
		if last.Message == nil || last.Message.Source.Kind != "skill-catalog" {
			t.Fatalf("independent contribution order: %+v", request.Surface)
		}
		var boundary []string
		for _, event := range log.events {
			if event.Record.Turn != result.Turn {
				continue
			}
			if event.Record.Type == session.RecordUserMessage {
				boundary = append(boundary, event.Record.Message.Source.Kind)
			}
			if event.Record.Type == session.RecordStepStart {
				boundary = append(boundary, "step")
				break
			}
		}
		if !slices.Equal(boundary, []string{"user", "skill-catalog", "step"}) && !slices.Equal(boundary, []string{"user", "runtime-context", "skill-catalog", "step"}) {
			t.Fatalf("snapshot placement=%q", boundary)
		}
	}
	h := start(2)
	run(h, session.SandboxWorkspaceWrite, 1)
	run(h, session.SandboxWorkspaceWrite, 1)
	// Reconstruct both engine and journal from committed events, without caches.
	events, _ := journal.Events(t.Context())
	log = &memoryLog{header: log.header, events: events}
	journal = newJournal(log)
	h = start(4)
	run(h, session.SandboxWorkspaceWrite, 1)
	change := func(mode session.SandboxMode) {
		t.Helper()
		if _, err := journal.Append(t.Context(), session.Record{Type: session.RecordSandboxMode, Sandbox: &session.SandboxModeChange{Mode: mode}}); err != nil {
			t.Fatal(err)
		}
	}
	change(session.SandboxReadOnly)
	run(h, session.SandboxReadOnly, 2)
	// Returning to an older mode must compare with the latest snapshot,
	// rather than suppressing a replacement because an older match exists.
	change(session.SandboxWorkspaceWrite)
	run(h, session.SandboxWorkspaceWrite, 3)
	var shadowed []uint64
	for _, event := range log.events {
		if event.Record.Message != nil && event.Record.Message.Source.Kind == "runtime-context" {
			shadowed = append(shadowed, event.Sequence)
		}
	}
	if _, err := journal.Append(t.Context(), session.Record{Type: session.RecordCompactionSummary, Compaction: &session.CompactionData{ID: "c", ShadowedSeqs: shadowed, Summary: []session.ContentBlock{{Type: session.ContentText, Text: "summary"}}}}); err != nil {
		t.Fatal(err)
	}
	run(h, session.SandboxWorkspaceWrite, 4)
}

func TestEngine_RejectsIncompleteRuntimeSnapshot(t *testing.T) {
	for _, cause := range []string{"provider", "events", "projection", "append"} {
		t.Run(cause, func(t *testing.T) {
			h := startEngineHarness(t, 1)
			scope := &plugin.Scope{}
			t.Cleanup(func() { _ = scope.Close(context.Background()) })
			if err := NewSandboxContext(h.engine, "/workspace").Start(t.Context(), scope); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("injected")
			second := &contextProbe{sections: []ContextSection{{Order: OrderSubagentDelegation, Text: "scope"}}}
			journal, log := turnJournal()
			want := failure
			switch cause {
			case "provider":
				second.err = failure
			case "events":
				log.eventsErr = failure
			case "projection":
				log.events = []session.Event{{Sequence: 1, Record: session.Record{Type: session.RecordCompactionSummary, Compaction: &session.CompactionData{ID: "broken", ShadowedSeqs: []uint64{999}}}}}
				want = session.ErrInvalidRecord
			case "append":
				log.appendErr = failure
			}
			if err := h.engine.RegisterContext(second, scope); err != nil {
				t.Fatal(err)
			}
			if err := h.engine.stepContext(t.Context(), runInput{journal: journal}, 1); !errors.Is(err, want) {
				t.Fatalf("step context=%v", err)
			}
			if slices.Contains(recordTypes(log.events), session.RecordUserMessage) {
				t.Fatal("failed complete snapshot published partial context")
			}
		})
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

// Snapshot sections follow their declared order, as upstream sorts runtime
// contexts; registration order only breaks ties.
func TestEngine_SnapshotSectionsFollowOrderNotRegistration(t *testing.T) {
	h := startEngineHarness(t, 1)
	scope := &plugin.Scope{}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	for _, probe := range []*contextProbe{
		{sections: []ContextSection{{Order: OrderSubagentDelegation, Text: "late"}}},
		{sections: []ContextSection{{Order: OrderSandboxPolicy, Text: "early"}}},
		{sections: []ContextSection{{Order: OrderSandboxPolicy, Text: "tie after early"}}},
	} {
		if err := h.engine.RegisterContext(probe, scope); err != nil {
			t.Fatal(err)
		}
	}
	journal, log := turnJournal()
	if err := h.engine.stepContext(t.Context(), runInput{journal: journal}, 1); err != nil {
		t.Fatal(err)
	}
	want := "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\nearly\n\ntie after early\n\nlate"
	if len(log.events) != 1 || session.Text(*log.events[0].Record.Message) != want {
		t.Fatalf("snapshot = %+v", log.events)
	}
}

// Go sorts slices of at most 12 elements by insertion, which is stable by
// accident; 48 sections, 16 per order and interleaved against their order,
// make an unstable sort reorder ties.
func TestEngine_SnapshotKeepsRegistrationOrderAmongManyTiedSections(t *testing.T) {
	h := startEngineHarness(t, 1)
	scope := &plugin.Scope{}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	orders := []int{OrderSubagentDelegation, OrderSandboxPolicy, OrderSubagentDelegation + 80}
	byOrder := map[int][]string{}
	for index := range 48 {
		order := orders[index%len(orders)]
		text := "section " + strings.Repeat("·", index)
		byOrder[order] = append(byOrder[order], text)
		if err := h.engine.RegisterContext(&contextProbe{sections: []ContextSection{{Order: order, Text: text}}}, scope); err != nil {
			t.Fatal(err)
		}
	}
	journal, log := turnJournal()
	if err := h.engine.stepContext(t.Context(), runInput{journal: journal}, 1); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, order := range []int{OrderSandboxPolicy, OrderSubagentDelegation, OrderSubagentDelegation + 80} {
		if len(byOrder[order]) != 16 {
			t.Fatalf("order %d has %d sections", order, len(byOrder[order]))
		}
		want = append(want, byOrder[order]...)
	}
	text := "Current runtime context. This snapshot supersedes earlier runtime-context snapshots.\n\n" + strings.Join(want, "\n\n")
	if len(log.events) != 1 || session.Text(*log.events[0].Record.Message) != text {
		t.Fatalf("tied sections lost their registration order:\n%s", session.Text(*log.events[0].Record.Message))
	}
}
