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

const structuredFixture = "testdata/session-v2-structured-results.jsonl"

// structuredRecords constructs the fixture's records independently of the
// decoder: one call per metadata kind plus classified failures.
func structuredRecords() []coresession.Record {
	old := "old line"
	call := func(id, name, arguments string) coresession.Record {
		return coresession.Record{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(arguments)}}
	}
	result := func(data coresession.ToolResult) coresession.Record {
		return coresession.Record{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &data}
	}
	return []coresession.Record{
		{Type: coresession.RecordTurnStart, Turn: 1},
		{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("inspect")},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 1},
		{Type: coresession.RecordRequestHeader, Turn: 1, Step: 1, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("calling tools")},
		call("call-read", "read", `{"file_path":"a.go"}`),
		call("call-missing", "read", `{"file_path":"missing.go"}`),
		call("call-image", "read_image", `{"file_path":"red.png"}`),
		call("call-write", "write", `{"file_path":"a.go","content":"new line\n"}`),
		call("call-edit", "edit", `{"file_path":"a.go","old_string":"x","new_string":"y"}`),
		call("call-glob", "glob", `{"pattern":"*.go"}`),
		call("call-grep", "grep", `{"pattern":"line"}`),
		call("call-search", "web_search", `{"queries":["nano"]}`),
		call("call-fetch", "web_fetch", `{"url":"https://example.com"}`),
		call("call-unknown", "nope", `{}`),
		result(coresession.ToolResult{CallID: "call-read", Output: "<path>a.go</path>", Meta: &coresession.ToolMeta{Read: &coresession.ReadMeta{
			Path: "a.go", Offset: 2, Lines: []coresession.ReadLine{{Number: 2, Text: "second"}, {Number: 3, Text: "third"}}, TotalLines: 4,
		}}}),
		result(coresession.ToolResult{CallID: "call-missing", Output: `Error: cannot read "missing.go": not found`, IsError: true, Error: &coresession.ToolError{Name: "FsError", Code: "FS_NOT_FOUND"}}),
		result(coresession.ToolResult{CallID: "call-image", Output: fixtureImageOutput,
			Image: &coresession.Image{ID: fixtureImageID, Name: "red.png", MediaType: "image/jpeg", Bytes: 600, Width: 1, Height: 1},
			Meta:  &coresession.ToolMeta{ReadImage: &coresession.ReadImageMeta{Path: "red.png"}}}),
		result(coresession.ToolResult{CallID: "call-write", Output: "Updated file", Meta: &coresession.ToolMeta{Write: &coresession.WriteMeta{
			Operation: coresession.WriteUpdate, Diffs: []coresession.FileDiff{{Path: "a.go", OldText: &old, NewText: "new line"}},
		}}}),
		result(coresession.ToolResult{CallID: "call-edit", Output: "The file a.go has been updated successfully.", Meta: &coresession.ToolMeta{Edit: &coresession.EditMeta{
			Diffs: []coresession.FileDiff{{Path: "a.go", NewText: "inserted"}}, Truncated: true,
		}}}),
		result(coresession.ToolResult{CallID: "call-glob", Output: "a.go\nb.go", Meta: &coresession.ToolMeta{Glob: &coresession.GlobMeta{
			Paths: []string{"a.go", "b.go"}, Total: 3, Truncated: true,
		}}}),
		result(coresession.ToolResult{CallID: "call-grep", Output: "a.go:\n  Line 2: second line", Meta: &coresession.ToolMeta{Grep: &coresession.GrepMeta{
			Files: []coresession.GrepFile{{Path: "a.go", Matches: []coresession.GrepMatch{{LineNumber: 2, Line: "second line"}}}}, Total: 1,
		}}}),
		result(coresession.ToolResult{CallID: "call-search", Output: "nano answer", Meta: &coresession.ToolMeta{WebSearch: &coresession.WebSearchMeta{
			Sources: []coresession.WebSource{{URL: "https://example.com/nano", Title: "Nano", Snippet: "harness", PublishedAt: "2026-10-01"}}, Answer: "nano answer",
		}}}),
		result(coresession.ToolResult{CallID: "call-fetch", Output: "Fetched https://example.com/ (HTTP 404)", Meta: &coresession.ToolMeta{WebFetch: &coresession.WebFetchMeta{
			URL: "https://example.com/", StatusCode: 404,
		}}}),
		result(coresession.ToolResult{CallID: "call-unknown", Output: `Error: unknown tool "nope"`, IsError: true, Error: &coresession.ToolError{Name: "ToolNotFoundError", Code: "UNKNOWN_TOOL"}}),
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 1},
		{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted},
	}
}

func TestSessionV2StructuredResults_FrozenContract(t *testing.T) {
	fixture, err := os.ReadFile(structuredFixture)
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
	if err != nil || len(surface) != 22 {
		t.Fatalf("surface=%d err=%v", len(surface), err)
	}
	tools := map[string]string{}
	for _, node := range surface {
		if node.Result != nil && node.Result.Meta != nil {
			tools[node.Result.CallID] = node.Result.Meta.Tool()
		}
	}
	if len(tools) != 8 || tools["call-image"] != "read_image" || surface[13].Result.Error.Code != "FS_NOT_FOUND" || surface[14].Result.Image == nil || surface[21].Result.Error.Name != "ToolNotFoundError" {
		t.Fatalf("structured results did not replay: %v", tools)
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
	for _, record := range structuredRecords() {
		appendRecord(t, writer, record)
	}
	actual, err := os.ReadFile(output.Name())
	if err != nil || !bytes.Equal(actual, fixture) {
		t.Fatalf("writer differs from frozen structured fixture: %v\n%s", err, actual)
	}
}

func TestSessionV2StructuredResults_RejectsChangedContract(t *testing.T) {
	fixture, err := os.ReadFile(structuredFixture)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, from, to string }{
		{"unknown-tool-member", `"meta":{"read":{`, `"meta":{"reads":{`},
		{"unknown-meta-field", `"total_lines":4`, `"total_lines":4,"lang":"go"`},
		{"two-members", `"meta":{"web_fetch":`, `"meta":{"read_image":{"path":"x"},"web_fetch":`},
		{"null-list", `"paths":["a.go","b.go"]`, `"paths":null`},
		{"inconsistent-total", `"total":3,"truncated":true`, `"total":1,"truncated":true`},
		{"classified-success", `"call_id":"call-grep","output":`, `"error":{"name":"FsError","code":"FS_NOT_FOUND"},"call_id":"call-grep","output":`},
		{"failed-with-metadata", `"output":"Fetched https://example.com/ (HTTP 404)","is_error":false`, `"output":"Fetched https://example.com/ (HTTP 404)","is_error":true`},
		{"invalid-code", `"code":"UNKNOWN_TOOL"`, `"code":"UNKNOWN-TOOL"`},
		{"unknown-error-field", `"code":"UNKNOWN_TOOL"`, `"code":"UNKNOWN_TOOL","reason":"x"`},
		{"metadata-of-another-tool", `"name":"glob"`, `"name":"find"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := bytes.Replace(fixture, []byte(test.from), []byte(test.to), 1)
			if bytes.Equal(changed, fixture) {
				t.Fatal("fixture did not change")
			}
			file := temporaryFile(t)
			if _, err := file.Write(changed); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSession(file, testCompositionID); !errors.Is(err, ErrCorruptSession) {
				t.Fatalf("changed structured result accepted: %v", err)
			}
		})
	}
}

func TestLog_RepairClassifiesAnUnknownOutcome(t *testing.T) {
	manager, scope := startManager(t)
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	root := t.TempDir()
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "unknown", Create: true, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("write")})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordStepStart, Turn: 1, Step: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("writing")})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: "call", Name: "write", Arguments: json.RawMessage(`{"file_path":"a","content":"b"}`)}})
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	resumed, err := manager.Open(t.Context(), OpenOptions{SessionID: "unknown", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.Close(context.Background()) })
	events, err := resumed.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	repaired := events[5].Record.Result
	if repaired == nil || !repaired.IsError || repaired.Error == nil || *repaired.Error != coresession.ToolOutcomeUnknown || repaired.Meta != nil {
		t.Fatalf("repaired result = %+v", repaired)
	}
}
