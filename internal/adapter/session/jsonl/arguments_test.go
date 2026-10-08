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

func TestSessionV2Arguments_FrozenContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-arguments.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	manager, scope := startManager(t)
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	path := filepath.Join(manager.config.Root, "fixture.jsonl")
	if err := os.WriteFile(path, fixture, 0o600); err != nil { //nolint:gosec // fixed fixture name under the test-owned private manager root
		t.Fatal(err)
	}
	header, events, err := manager.Inspect(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	surface, err := coresession.Surface(events)
	if err != nil || len(surface) != 4 || !surface[2].Call.ArgumentsOmitted || string(surface[2].Call.Arguments) != "{}" || !surface[3].Result.IsError {
		t.Fatalf("omitted call did not replay: surface=%+v, err=%v", surface, err)
	}
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "fixture", Cwd: header.Cwd})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // fixed fixture name under the test-owned private manager root
	if err != nil || !bytes.Equal(after, fixture) {
		t.Fatalf("resume changed the fixture: %v", err)
	}
	output := temporaryFile(t)
	if _, err := writeHeader(output, coresession.Header{SessionID: "fixture", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/synthetic/workspace"}, nil); err != nil {
		t.Fatal(err)
	}
	writer := &Log{file: output, header: header, active: true, size: int64(bytes.IndexByte(fixture, '\n') + 1)}
	for _, record := range []coresession.Record{
		{Type: coresession.RecordTurnStart, Turn: 1},
		{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("hello")},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 1},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("answer")},
		{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: "call", Name: "todo_write", Arguments: json.RawMessage(`{}`), ArgumentsOmitted: true}},
		{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &coresession.ToolResult{CallID: "call", Output: "Error: tool arguments exceed 786432 bytes; submit a smaller call", IsError: true}},
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 1},
		{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted},
	} {
		appendRecord(t, writer, record)
	}
	actual, err := os.ReadFile(output.Name())
	if err != nil || !bytes.Equal(actual, fixture) {
		t.Fatalf("writer differs from frozen argument fixture: %v\n%s", err, actual)
	}
}

func TestSessionV2Arguments_RejectsChangedContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-arguments.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, from, to string }{
		{"marker-type", `"arguments_omitted":true`, `"arguments_omitted":"true"`},
		{"retained-arguments", `"arguments":{}`, `"arguments":{"v":1}`},
		{"successful-result", `"is_error":true`, `"is_error":false`},
		{"approval", `{"seq":6,"record":{"type":"tool/result","turn":1,"step":1,"result":{"call_id":"call","output":"Error: tool arguments exceed 786432 bytes; submit a smaller call","is_error":true}}}`, `{"seq":6,"record":{"type":"approval/asked","turn":1,"step":1,"approval":{"id":"a","call_id":"call","tool_name":"todo_write","reason":"write"}}}`},
		{"todo-side-effect", `{"seq":6,"record":{"type":"tool/result","turn":1,"step":1,"result":{"call_id":"call","output":"Error: tool arguments exceed 786432 bytes; submit a smaller call","is_error":true}}}`, `{"seq":6,"record":{"type":"todo/write","turn":1,"step":1,"todo":{"call_id":"call","items":[]}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := temporaryFile(t)
			changed := bytes.Replace(fixture, []byte(test.from), []byte(test.to), 1)
			if bytes.Equal(changed, fixture) {
				t.Fatal("fixture did not change")
			}
			// Keep a valid interrupted tail so unfinished work cannot mask
			// acceptance of an illegal result, approval, or side effect.
			end := bytes.Index(changed, []byte(`{"seq":7`))
			if end < 0 {
				t.Fatal("fixture lacks the step closure")
			}
			changed = changed[:end]
			if _, err := file.Write(changed); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSession(file, testCompositionID); !errors.Is(err, ErrCorruptSession) {
				t.Fatalf("illegal omitted call accepted: %v", err)
			}
		})
	}
}

func TestLog_ArgumentLimitFitsEscapedRecords(t *testing.T) {
	manager, scope := startManager(t)
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "escaped", Cwd: "/synthetic/workspace", Create: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close(t.Context()) }()
	chunkArguments := `{"v":"` + strings.Repeat("<", coresession.MaxArgumentsBytes-8) + `"}`
	call := (coresession.ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{"v":"` + strings.Repeat("<", (coresession.MaxArgumentsBytes-8)/6) + `"}`)}).LimitArguments()
	for _, record := range []coresession.Record{
		{Type: coresession.RecordTurnStart, Turn: 1},
		{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("hello")},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 1},
		{Type: coresession.RecordAssistantChunk, Turn: 1, Step: 1, Chunk: &coresession.AssistantChunk{Kind: coresession.ChunkTool, Arguments: chunkArguments}},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("answer")},
		{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &call},
		{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &coresession.ToolResult{CallID: "call", Output: "ok"}},
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 1},
		{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted},
	} {
		appendRecord(t, log, record)
	}
	if _, _, err := manager.Inspect(t.Context(), "escaped"); err != nil {
		t.Fatal(err)
	}
}
