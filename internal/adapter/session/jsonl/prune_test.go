package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

var fixtureLongOutput = strings.Repeat("L", 4096) + strings.Repeat("M", 5000) + strings.Repeat("T", 1024)

func TestSessionV2Prune_FrozenContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-prune.jsonl")
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
	pruned, _ := coresession.PruneToolOutput(fixtureLongOutput)
	if len(events) != 14 || events[6].Record.Result.Output != fixtureLongOutput || events[8].Record.Prune.Output != pruned {
		t.Fatalf("events=%d", len(events))
	}
	surface, err := coresession.Surface(events)
	if err != nil || len(surface) != 5 || surface[3].Sequence != 7 || surface[3].Result.Output != pruned {
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
		t.Fatal("read and closed resume changed a committed prune fixture")
	}

	// The writer uses independently constructed records, never decoded fixture values.
	output := temporaryFile(t)
	header := coresession.Header{SessionID: "fixture", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/synthetic/workspace"}
	if _, err := writeHeader(output, header, nil); err != nil {
		t.Fatal(err)
	}
	writer := &Log{file: output, header: header, active: true, size: int64(bytes.IndexByte(fixture, '\n') + 1)}
	replacement := strings.Repeat("L", 4096) + "\n\n[... tool result middle pruned ...]\n\n" + strings.Repeat("T", 1024)
	for _, record := range []coresession.Record{
		{Type: coresession.RecordTurnStart, Turn: 1},
		{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("inspect")},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 1},
		{Type: coresession.RecordRequestHeader, Turn: 1, Step: 1, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("reading")},
		{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: "call-read", Name: "read", Arguments: json.RawMessage(`{}`)}},
		{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &coresession.ToolResult{CallID: "call-read", Output: fixtureLongOutput}},
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 1},
		{Type: coresession.RecordCompactionPrune, Turn: 1, Prune: &coresession.ToolResultPrune{Seq: 7, Output: replacement}},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 2},
		{Type: coresession.RecordRequestHeader, Turn: 1, Step: 2, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 2, Message: assistantMessage("done")},
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 2},
		{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted},
	} {
		appendRecord(t, writer, record)
	}
	actual, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, fixture) {
		t.Fatal("writer differs from frozen v2 prune fixture")
	}
}

func TestSessionV2Prune_RejectsChangedContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-prune.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, from, to string }{
		{"unknown-prune-field", `"prune":{"seq":7,`, `"prune":{"seq":7,"chars":1,`},
		{"missing-payload", `,"prune":{"seq":7,`, `,"other":{"seq":7,`},
		{"inside-step", `"type":"compaction/prune","turn":1,`, `"type":"compaction/prune","turn":1,"step":1,`},
		{"wrong-turn", `"type":"compaction/prune","turn":1,`, `"type":"compaction/prune","turn":2,`},
		{"names-the-call", `"prune":{"seq":7,`, `"prune":{"seq":6,`},
		{"unknown-sequence", `"prune":{"seq":7,`, `"prune":{"seq":99,`},
		{"tampered-marker", `[... tool result middle pruned ...]`, `[... tool result middle trimmed ...]`},
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

func TestValidateOrder_PruneOnlyAtCompactionBoundaries(t *testing.T) {
	pruned, _ := coresession.PruneToolOutput(fixtureLongOutput)
	result := addOrder(withCall(withAssistant(orderPrefix())), coresession.Record{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &coresession.ToolResult{CallID: "call", Output: fixtureLongOutput}})
	for index := range result {
		result[index].Sequence = uint64(index + 1)
	}
	prune := func(turn uint64) coresession.Record {
		return coresession.Record{Type: coresession.RecordCompactionPrune, Turn: turn, Prune: &coresession.ToolResultPrune{Seq: uint64(len(result)), Output: pruned}}
	}
	if _, err := validateOrder(addOrder(result, prune(1)), false); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("prune inside an open step accepted: %v", err)
	}
	closed := addOrder(result, coresession.Record{Type: coresession.RecordStepEnd, Turn: 1, Step: 1})
	inTransaction := addOrder(closed, coresession.Record{Type: coresession.RecordCompactionStart, Turn: 1, Compaction: &coresession.CompactionData{ID: "compact"}})
	if _, err := validateOrder(addOrder(inTransaction, prune(1)), false); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("prune inside a compaction transaction accepted: %v", err)
	}
	if _, err := validateOrder(addOrder(closed, prune(1)), false); err != nil {
		t.Fatalf("prune at a step boundary rejected: %v", err)
	}
	ended := addOrder(closed, coresession.Record{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted})
	if _, err := validateOrder(addOrder(ended, prune(0)), true); err != nil {
		t.Fatalf("prune between turns rejected: %v", err)
	}
}
