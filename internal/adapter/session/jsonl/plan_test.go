package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

func planMode(turn uint64, active bool) coresession.Record {
	return coresession.Record{Type: coresession.RecordPlanMode, Turn: turn, Plan: &coresession.PlanMode{Active: active}}
}

func TestSessionV2Plan_FrozenContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-plan.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	manager, scope := startManager(t)
	t.Cleanup(func() {
		if err := scope.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(manager.config.Root, "fixture.jsonl")
	if err := os.WriteFile(path, fixture, 0o600); err != nil { //nolint:gosec // fixed fixture name under the test-owned private manager root
		t.Fatal(err)
	}
	_, events, err := manager.Inspect(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 23 || coresession.ProjectPlan(events[:12]) != (coresession.PlanView{Active: true, Requested: true, Told: false}) {
		t.Fatalf("events=%d view=%+v", len(events), coresession.ProjectPlan(events[:12]))
	}
	if view := coresession.ProjectPlan(events); view != (coresession.PlanView{Active: false, Requested: true, Told: false}) {
		t.Fatalf("final view=%+v", view)
	}
	surface, err := coresession.Surface(events)
	if err != nil || len(surface) != 8 || surface[3].Message.Source.Kind != "plan-mode" || surface[5].Call.Name != "exit_plan_mode" {
		t.Fatalf("surface=%+v err=%v", surface, err)
	}
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "fixture", Cwd: "/synthetic/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // fixed fixture name under the test-owned private manager root
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, fixture) {
		t.Fatal("read and closed resume changed a committed plan fixture")
	}

	// The writer uses independently constructed records, never decoded fixture values.
	output := temporaryFile(t)
	header := coresession.Header{SessionID: "fixture", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/synthetic/workspace"}
	if _, err := writeHeader(output, header); err != nil {
		t.Fatal(err)
	}
	writer := &Log{file: output, header: header, active: true, size: int64(bytes.IndexByte(fixture, '\n') + 1)}
	notice := &coresession.Message{Role: coresession.RoleUser, Source: coresession.MessageSource{Kind: "plan-mode"}, Content: []coresession.ContentBlock{{Type: coresession.ContentText, Text: "The user switched this session to plan mode."}}}
	for _, record := range []coresession.Record{
		{Type: coresession.RecordTurnStart, Turn: 1},
		{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("hello")},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 1},
		{Type: coresession.RecordRequestHeader, Turn: 1, Step: 1, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("hi")},
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 1},
		{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted},
		planMode(0, true),
		{Type: coresession.RecordTurnStart, Turn: 2},
		{Type: coresession.RecordUserMessage, Turn: 2, Message: userMessage("design the change")},
		{Type: coresession.RecordUserMessage, Turn: 2, Message: notice},
		{Type: coresession.RecordStepStart, Turn: 2, Step: 1},
		{Type: coresession.RecordRequestHeader, Turn: 2, Step: 1, Header: &coresession.RequestHeader{Provider: "openai", Model: "model", System: "plan policy"}},
		{Type: coresession.RecordAssistantMessage, Turn: 2, Step: 1, Message: assistantMessage("plan ready")},
		{Type: coresession.RecordToolCall, Turn: 2, Step: 1, Call: &coresession.ToolCall{ID: "call-exit", Name: "exit_plan_mode", Arguments: json.RawMessage(`{"plan":"# Plan\n\n- step"}`)}},
		{Type: coresession.RecordToolResult, Turn: 2, Step: 1, Result: &coresession.ToolResult{CallID: "call-exit", Output: "Plan approved — plan mode exited; carry out the plan starting with your next step."}},
		{Type: coresession.RecordStepEnd, Turn: 2, Step: 1},
		planMode(2, false),
		{Type: coresession.RecordStepStart, Turn: 2, Step: 2},
		{Type: coresession.RecordRequestHeader, Turn: 2, Step: 2, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}},
		{Type: coresession.RecordAssistantMessage, Turn: 2, Step: 2, Message: assistantMessage("done")},
		{Type: coresession.RecordStepEnd, Turn: 2, Step: 2},
		{Type: coresession.RecordTurnEnd, Turn: 2, Outcome: coresession.OutcomeCompleted},
	} {
		appendRecord(t, writer, record)
	}
	actual, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, fixture) {
		t.Fatalf("writer differs from frozen v2 plan fixture:\n%s", actual)
	}
}

func TestSessionV2Plan_RejectsChangedContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-plan.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, from, to string }{
		{"unknown-plan-field", `"plan":{"active":true}`, `"plan":{"active":true,"reason":"x"}`},
		{"missing-payload", `{"type":"plan/mode","plan":{"active":true}}`, `{"type":"plan/mode"}`},
		{"inside-step", `"type":"plan/mode","turn":2,`, `"type":"plan/mode","turn":2,"step":1,`},
		{"idle-record-names-a-turn", `"type":"plan/mode","plan"`, `"type":"plan/mode","turn":1,"plan"`},
		{"turn-record-names-another-turn", `"type":"plan/mode","turn":2,`, `"type":"plan/mode","turn":3,`},
		{"repeats-current-mode", `"turn":2,"plan":{"active":false}`, `"turn":2,"plan":{"active":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := bytes.Replace(fixture, []byte(test.from), []byte(test.to), 1)
			if bytes.Equal(changed, fixture) {
				t.Fatalf("replacement %q did not change the fixture", test.from)
			}
			file := temporaryFile(t)
			if _, err := file.Write(changed); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSession(file, testCompositionID); !errors.Is(err, ErrCorruptSession) {
				t.Fatalf("got %v, want %v", err, ErrCorruptSession)
			}
		})
	}
}

func TestValidateOrder_PlanModeOnlyAtStepBoundaries(t *testing.T) {
	inStep := orderPrefix()
	if _, err := validateOrder(addOrder(inStep, planMode(1, true)), false); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("plan/mode inside an open step accepted: %v", err)
	}
	beforeStep := []coresession.Event{
		orderedEvent(coresession.Record{Type: coresession.RecordTurnStart, Turn: 1}),
		orderedEvent(coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("hello")}),
		orderedEvent(planMode(1, true)),
		orderedEvent(coresession.Record{Type: coresession.RecordCompactionStart, Turn: 1, Compaction: &coresession.CompactionData{ID: "compact"}}),
		orderedEvent(planMode(1, false)),
		orderedEvent(coresession.Record{Type: coresession.RecordCompactionEnd, Turn: 1, Compaction: &coresession.CompactionData{ID: "compact"}}),
	}
	if _, err := validateOrder(beforeStep, false); err != nil {
		t.Fatalf("boundary changes rejected: %v", err)
	}
	if _, err := validateOrder([]coresession.Event{orderedEvent(planMode(0, false))}, false); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("first record leaving an inactive mode accepted: %v", err)
	}
}

func TestInterruptedTailRepair_KeepsCommittedPlanMode(t *testing.T) {
	manager, scope := startManager(t)
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	root := t.TempDir()
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "plan", Create: true, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	appendRecord(t, log, planMode(0, true))
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("plan it")})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordStepStart, Turn: 1, Step: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("review")})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: "call", Name: "exit_plan_mode", Arguments: json.RawMessage(`{"plan":"# P"}`)}})
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	resumed, err := manager.Open(t.Context(), OpenOptions{SessionID: "plan", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close(context.Background()) })
	events, err := resumed.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 9 || events[6].Record.Result.Output != "Error: interrupted before a result was committed" || events[8].Record.Outcome != coresession.OutcomeInterrupted {
		t.Fatalf("repair events=%+v", events)
	}
	if view := coresession.ProjectPlan(events); !view.Active {
		t.Fatalf("repair changed the committed plan mode: %+v", view)
	}
	appendRecord(t, resumed, planMode(0, false))
}
