package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// toolResultLines returns the raw tool/result lines of a transcript.
func toolResultLines(t *testing.T, path string) [][]byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	var lines [][]byte
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		if bytes.Contains(line, []byte(`"type":"tool/result"`)) {
			lines = append(lines, line)
		}
	}
	return lines
}

// One assembled session collects results from the file, search, and
// runtime batches; neither the next chat request nor the compaction summary
// request carries their classifications or metadata, compaction leaves the
// raw results byte-for-byte, and a resumed log replays them unchanged.
func TestComposition_StructuredResultsSurviveCompactionAndResume(t *testing.T) {
	assembled, seen := startAssembled(t, []modelStep{
		{tool: "glob", arguments: `{"pattern":"*.txt"}`},
		{tool: "grep", arguments: `{"pattern":"alpha"}`},
		{tool: "read", arguments: `{"file_path":"notes.txt"}`},
		{tool: "edit", arguments: `{"file_path":"notes.txt","old_string":"alpha","new_string":"beta"}`},
		{tool: "read", arguments: `{"file_path":"missing.txt"}`},
		{tool: "grep", arguments: `{"pattern":"("}`},
		{tool: "nope", arguments: `{}`},
		{text: "done"},
		{text: "Summary: notes.txt now says beta."},
	})
	allowAssembledWrites(t, assembled)
	workspace := assembledWorkspace(t, assembled)
	if err := os.WriteFile(filepath.Join(workspace, "notes.txt"), []byte("alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := assembled.turn(t, "inspect and change notes"); result.Err != nil || result.Text != "done" {
		t.Fatalf("turn = %+v", result)
	}
	before := toolResultLines(t, assembled.transcript)
	if compacted, err := assembled.root.Compact(t.Context()); err != nil || !compacted {
		t.Fatalf("compaction = %t, %v", compacted, err)
	}
	records := assembled.records(t)
	if after := toolResultLines(t, assembled.transcript); len(after) != len(before) || !bytes.Equal(bytes.Join(after, nil), bytes.Join(before, nil)) {
		t.Fatal("compaction changed committed tool results")
	}
	summarized := false
	for _, record := range records {
		summarized = summarized || record.Type == session.RecordCompactionSummary
	}
	results := orderedToolResults(records)
	if !summarized || len(results) != 7 {
		t.Fatalf("summarized=%t results=%d", summarized, len(results))
	}
	for index, tool := range []string{"glob", "grep", "read", "edit"} {
		if result := results[index]; result.IsError || result.Meta == nil || result.Meta.Tool() != tool || result.Error != nil {
			t.Errorf("%s result = %+v", tool, result)
		}
	}
	if glob := results[0].Meta.Glob; glob == nil || glob.Total != 1 || len(glob.Paths) != 1 {
		t.Errorf("glob metadata = %+v", glob)
	}
	for offset, want := range []session.ToolError{{Name: "FsError", Code: "FS_NOT_FOUND"}, {Name: "SearchError", Code: "SEARCH_INVALID_PATTERN"}, {Name: "ToolNotFoundError", Code: "UNKNOWN_TOOL"}} {
		if result := results[4+offset]; !result.IsError || result.Error == nil || *result.Error != want || result.Meta != nil {
			t.Errorf("failure %d = %+v, want %+v", offset, result, want)
		}
	}

	requests := seen()
	if len(requests) != 9 || !strings.HasPrefix(requests[8].Instructions, "Summarize the supplied conversation prefix") {
		t.Fatalf("requests = %d; the last is not the compaction summary", len(requests))
	}
	for name, request := range map[string]seenRequest{"next chat": requests[7], "compaction summary": requests[8]} {
		input, _ := json.Marshal(request.Input)
		if !strings.Contains(string(input), "notes.txt") {
			t.Errorf("%s request lacks the result text: %s", name, input)
		}
		for _, private := range []string{"FsError", "FS_NOT_FOUND", "SearchError", "ToolNotFoundError", "UNKNOWN_TOOL", "total_lines", "old_text", "line_number", `\"meta\"`} {
			if strings.Contains(string(input), private) {
				t.Errorf("%s request contains %s", name, private)
			}
		}
	}

	transcript, err := os.ReadFile(assembled.transcript)
	if err != nil {
		t.Fatal(err)
	}
	var header struct {
		Header session.Header `json:"header"`
	}
	if err := json.Unmarshal(transcript[:bytes.IndexByte(transcript, '\n')], &header); err != nil {
		t.Fatal(err)
	}
	manager, err := sessionjsonl.New(sessionjsonl.Config{Root: filepath.Dir(assembled.transcript), CompositionID: header.Header.CompositionID})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := manager.Start(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	resumed, err := manager.Open(t.Context(), sessionjsonl.OpenOptions{SessionID: header.Header.SessionID, Cwd: header.Header.Cwd})
	if err != nil {
		t.Fatal(err)
	}
	events, err := resumed.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	var replayed []session.ToolResult
	for _, event := range events {
		if event.Record.Type == session.RecordToolResult {
			replayed = append(replayed, *event.Record.Result)
		}
	}
	got, _ := json.Marshal(replayed)
	want, _ := json.Marshal(results)
	if string(got) != string(want) {
		t.Fatalf("resumed results differ:\n%s\n%s", got, want)
	}
	if after, err := os.ReadFile(assembled.transcript); err != nil || !bytes.Equal(after, transcript) {
		t.Fatalf("resume changed the transcript: %v", err)
	}
}
