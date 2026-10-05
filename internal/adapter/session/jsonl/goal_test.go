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

func goalSnapshotRecord(operation coresession.GoalOperation, revision uint64, objective string, phase coresession.GoalPhase, rounds uint64, updated int64) coresession.Record {
	snapshot := &coresession.GoalSnapshot{ID: "goal-fixture", Revision: revision, Objective: objective, Phase: phase, MaxRounds: 3}
	if phase == coresession.GoalBlocked {
		snapshot.BlockedReason = &coresession.GoalBlockReason{Code: "model-reported", Message: "the docs host is unreachable"}
	}
	return coresession.Record{Type: coresession.RecordGoalChange, Goal: &coresession.GoalChange{Operation: operation, Snapshot: snapshot, RoundsStarted: rounds, CreatedAtUnixMS: 1000, UpdatedAtUnixMS: updated}}
}

func goalRoundMessage(text string, revision, round uint64) *coresession.Message {
	return &coresession.Message{Role: coresession.RoleUser, Source: coresession.MessageSource{Kind: coresession.GoalSource, GoalID: "goal-fixture", GoalRevision: revision, GoalRound: round}, Content: []coresession.ContentBlock{{Type: coresession.ContentText, Text: text}}}
}

func TestSessionV2Goal_FrozenContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-goal.jsonl")
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
	if len(events) != 37 {
		t.Fatalf("events=%d", len(events))
	}
	for _, check := range []struct {
		prefix   int
		revision uint64
		phase    coresession.GoalPhase
		rounds   uint64
	}{{10, 1, coresession.GoalActive, 0}, {17, 1, coresession.GoalActive, 1}, {20, 4, coresession.GoalActive, 1}, {35, 5, coresession.GoalBlocked, 2}, {36, 6, coresession.GoalComplete, 2}} {
		state, err := coresession.ProjectGoal(events[:check.prefix])
		if err != nil || state.Goal == nil || state.Goal.Revision != check.revision || state.Goal.Phase != check.phase || state.RoundsStarted != check.rounds {
			t.Fatalf("prefix %d: state=%+v err=%v", check.prefix, state, err)
		}
	}
	if state, err := coresession.ProjectGoal(events); err != nil || state.Goal != nil {
		t.Fatalf("cleared state=%+v err=%v", state, err)
	}
	surface, err := coresession.Surface(events)
	if err != nil || len(surface) != 12 || surface[4].Message.Source.GoalRound != 1 || surface[10].Message.Source.Kind != "tool-goal" {
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
		t.Fatal("read and closed resume changed a committed goal fixture")
	}

	// The writer uses independently constructed records, never decoded fixture values.
	output := temporaryFile(t)
	header := coresession.Header{SessionID: "fixture", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/synthetic/workspace"}
	if _, err := writeHeader(output, header, nil); err != nil {
		t.Fatal(err)
	}
	writer := &Log{file: output, header: header, active: true, size: int64(bytes.IndexByte(fixture, '\n') + 1)}
	human := &coresession.Message{Role: coresession.RoleUser, Source: coresession.MessageSource{Kind: "user"}, Content: []coresession.ContentBlock{{Type: coresession.ContentText, Text: "keep the docs green"}}}
	wrapup := &coresession.Message{Role: coresession.RoleUser, Source: coresession.MessageSource{Kind: "tool-goal"}, Content: []coresession.ContentBlock{{Type: coresession.ContentText, Text: "goal blocked: write the closing message"}}}
	header1 := &coresession.RequestHeader{Provider: "openai", Model: "model"}
	const edited = "keep every docs gate green"
	for _, record := range []coresession.Record{
		{Type: coresession.RecordTurnStart, Turn: 1},
		{Type: coresession.RecordUserMessage, Turn: 1, Message: human},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 1},
		{Type: coresession.RecordRequestHeader, Turn: 1, Step: 1, Header: header1},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("creating a goal")},
		{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: "call-create", Name: "create_goal", Arguments: json.RawMessage(`{"objective":"keep the docs green","max_goal_rounds":3}`)}},
		goalSnapshotRecord(coresession.GoalOpCreate, 1, "keep the docs green", coresession.GoalActive, 0, 1000),
		{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &coresession.ToolResult{CallID: "call-create", Output: "created"}},
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 1},
		{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted},
		{Type: coresession.RecordTurnStart, Turn: 2},
		{Type: coresession.RecordUserMessage, Turn: 2, Message: goalRoundMessage("goal round 1", 1, 1)},
		{Type: coresession.RecordStepStart, Turn: 2, Step: 1},
		{Type: coresession.RecordRequestHeader, Turn: 2, Step: 1, Header: header1},
		{Type: coresession.RecordAssistantMessage, Turn: 2, Step: 1, Message: assistantMessage("fixed one gate")},
		{Type: coresession.RecordStepEnd, Turn: 2, Step: 1},
		{Type: coresession.RecordTurnEnd, Turn: 2, Outcome: coresession.OutcomeCompleted},
		goalSnapshotRecord(coresession.GoalOpEdit, 2, edited, coresession.GoalActive, 1, 2000),
		goalSnapshotRecord(coresession.GoalOpPause, 3, edited, coresession.GoalPaused, 1, 3000),
		goalSnapshotRecord(coresession.GoalOpResume, 4, edited, coresession.GoalActive, 1, 4000),
		{Type: coresession.RecordTurnStart, Turn: 3},
		{Type: coresession.RecordUserMessage, Turn: 3, Message: goalRoundMessage("goal round 2", 4, 2)},
		{Type: coresession.RecordStepStart, Turn: 3, Step: 1},
		{Type: coresession.RecordRequestHeader, Turn: 3, Step: 1, Header: header1},
		{Type: coresession.RecordAssistantMessage, Turn: 3, Step: 1, Message: assistantMessage("still blocked")},
		{Type: coresession.RecordToolCall, Turn: 3, Step: 1, Call: &coresession.ToolCall{ID: "call-block", Name: "update_goal", Arguments: json.RawMessage(`{"goal_id":"goal-fixture","revision":4,"action":"blocked","blocked_reason":"the docs host is unreachable"}`)}},
		goalSnapshotRecord(coresession.GoalOpBlock, 5, edited, coresession.GoalBlocked, 2, 5000),
		{Type: coresession.RecordToolResult, Turn: 3, Step: 1, Result: &coresession.ToolResult{CallID: "call-block", Output: "blocked"}},
		{Type: coresession.RecordStepEnd, Turn: 3, Step: 1},
		{Type: coresession.RecordUserMessage, Turn: 3, Message: wrapup},
		{Type: coresession.RecordStepStart, Turn: 3, Step: 2},
		{Type: coresession.RecordRequestHeader, Turn: 3, Step: 2, Header: header1},
		{Type: coresession.RecordAssistantMessage, Turn: 3, Step: 2, Message: assistantMessage("blocked on the docs host")},
		{Type: coresession.RecordStepEnd, Turn: 3, Step: 2},
		{Type: coresession.RecordTurnEnd, Turn: 3, Outcome: coresession.OutcomeCompleted},
		goalSnapshotRecord(coresession.GoalOpComplete, 6, edited, coresession.GoalComplete, 2, 6000),
		{Type: coresession.RecordGoalChange, Goal: &coresession.GoalChange{Operation: coresession.GoalOpClear, Cleared: &coresession.GoalRef{ID: "goal-fixture", Revision: 7}, ClearedAtUnixMS: 7000}},
	} {
		appendRecord(t, writer, record)
	}
	actual, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, fixture) {
		t.Fatalf("writer differs from frozen v2 goal fixture:\n%s", actual)
	}
}

func TestSessionV2Goal_RejectsChangedContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-goal.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, from, to string }{
		{"unknown-change-field", `"operation":"create",`, `"operation":"create","version":1,`},
		{"unknown-snapshot-field", `"revision":1,"objective"`, `"revision":1,"owner":"x","objective"`},
		{"unknown-source-field", `"goal_round":1}`, `"goal_round":1,"goal":"x"}`},
		{"unknown-operation", `"operation":"pause"`, `"operation":"halt"`},
		{"unknown-phase", `"phase":"paused"`, `"phase":"stopped"`},
		{"missing-payload", `{"type":"goal/change","goal":{"operation":"clear","cleared":{"id":"goal-fixture","revision":7},"cleared_at_unix_ms":7000}}`, `{"type":"goal/change"}`},
		{"names-a-turn", `"type":"goal/change","goal":{"operation":"create"`, `"type":"goal/change","turn":1,"goal":{"operation":"create"`},
		{"inside-step", `"type":"goal/change","goal":{"operation":"block"`, `"type":"goal/change","step":1,"goal":{"operation":"block"`},
		{"blocked-without-reason", `"blocked_reason":{"code":"model-reported","message":"the docs host is unreachable"},`, ``},
		{"reason-outside-blocked", `"phase":"paused",`, `"phase":"paused","blocked_reason":{"code":"x","message":"y"},`},
		{"untrimmed-objective", `"objective":"keep the docs green","phase"`, `"objective":"keep the docs green ","phase"`},
		{"skipped-revision", `"revision":3,`, `"revision":4,`},
		{"time-moves-back", `"updated_at_unix_ms":3000`, `"updated_at_unix_ms":1500`},
		{"rounds-not-preserved", `"phase":"paused","max_goal_rounds":3},"rounds_started":1`, `"phase":"paused","max_goal_rounds":3},"rounds_started":2`},
		{"illegal-transition", `"operation":"pause"`, `"operation":"block"`},
		{"stale-round", `"goal_revision":4,"goal_round":2`, `"goal_revision":3,"goal_round":2`},
		{"skipped-round", `"goal_revision":4,"goal_round":2`, `"goal_revision":4,"goal_round":3`},
		{"round-without-attribution", `"source":{"kind":"goal","goal_id":"goal-fixture","goal_revision":1,"goal_round":1}`, `"source":{"kind":"goal"}`},
		{"attribution-on-other-source", `"source":{"kind":"tool-goal"}`, `"source":{"kind":"tool-goal","goal_round":2}`},
		{"stale-clear", `"cleared":{"id":"goal-fixture","revision":7}`, `"cleared":{"id":"goal-fixture","revision":6}`},
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

func TestLogAppend_RejectsAStaleGoalRoundWithoutWriting(t *testing.T) {
	manager, scope := startManager(t)
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "goal", Create: true, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close(context.Background()) })
	appendRecord(t, log, goalSnapshotRecord(coresession.GoalOpCreate, 1, "ship", coresession.GoalActive, 0, 1000))
	appendRecord(t, log, goalSnapshotRecord(coresession.GoalOpEdit, 2, "ship more", coresession.GoalActive, 0, 1000))
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})
	before, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(t.Context(), coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: goalRoundMessage("round", 1, 1)}); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("stale round appended: %v", err)
	}
	after, err := os.ReadFile(log.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected append changed the file: %v", err)
	}
	appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: goalRoundMessage("round", 2, 1)})
}

func TestInterruptedTailRepair_KeepsCommittedGoal(t *testing.T) {
	manager, scope := startManager(t)
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	root := t.TempDir()
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "goal", Create: true, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	appendRecord(t, log, goalSnapshotRecord(coresession.GoalOpCreate, 1, "ship", coresession.GoalActive, 0, 1000))
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: goalRoundMessage("round", 1, 1)})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordStepStart, Turn: 1, Step: 1})
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	resumed, err := manager.Open(t.Context(), OpenOptions{SessionID: "goal", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close(context.Background()) })
	events, err := resumed.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state, err := coresession.ProjectGoal(events)
	if err != nil || events[len(events)-1].Record.Outcome != coresession.OutcomeInterrupted || state.Goal.Phase != coresession.GoalActive || state.RoundsStarted != 1 {
		t.Fatalf("repair state=%+v err=%v events=%d", state, err, len(events))
	}
}

// TestOpen_ForkSeedKeepsTheParentGoalInPlace proves that a fork seed carrying
// the parent's goal facts is still validated and accepted in place, while the
// child's own events hold no goal.
func TestOpen_ForkSeedKeepsTheParentGoalInPlace(t *testing.T) {
	manager, scope := startManager(t)
	defer func() { _ = scope.Close(context.Background()) }()
	parent, err := manager.Open(t.Context(), OpenOptions{SessionID: "parent", Create: true, Cwd: "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	appendRecord(t, parent, goalSnapshotRecord(coresession.GoalOpCreate, 1, "ship", coresession.GoalActive, 0, 1000))
	appendRecord(t, parent, coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})
	appendRecord(t, parent, coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: goalRoundMessage("round", 1, 1)})
	appendRecord(t, parent, coresession.Record{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted})
	seed, err := parent.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	child, err := manager.Open(t.Context(), OpenOptions{SessionID: "child", Create: true, Cwd: "/workspace", ParentSessionID: "parent", DelegationDepth: 1, Seed: seed})
	if err != nil {
		t.Fatal(err)
	}
	appendRecord(t, child, coresession.Record{Type: coresession.RecordSubagentDescriptor, Subagent: &coresession.SubagentDescriptor{Version: 2, Provider: coresession.SubagentFork, Mode: coresession.SubagentOneShot, Label: "fork", Inherited: uint64(len(seed))}})
	if err := child.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, events, err := manager.Inspect(t.Context(), "child")
	if err != nil {
		t.Fatal(err)
	}
	if full, err := coresession.ProjectGoal(events); err != nil || full.Goal == nil || full.RoundsStarted != 1 {
		t.Fatalf("seeded log = %+v, %v", full, err)
	}
	if own, err := coresession.ProjectGoal(coresession.OwnEvents(events)); err != nil || own.Goal != nil {
		t.Fatalf("child's own goal = %+v, %v", own, err)
	}
}
