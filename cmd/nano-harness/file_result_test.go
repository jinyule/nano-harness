package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestComposition_FileResultsPersistIndependentMetadata(t *testing.T) {
	assembled, seen := startAssembled(t, []modelStep{
		{tool: "write", arguments: `{"file_path":"f","content":"a\r\nb\r\n"}`},
		{tool: "read", arguments: `{"file_path":"f","offset":2,"limit":1}`},
		{tool: "edit", arguments: `{"file_path":"f","old_string":"b","new_string":"B"}`},
		{tool: "write", arguments: `{"file_path":"f","content":"a\nC\n"}`},
		{tool: "read", arguments: `{"file_path":"absent"}`},
		{text: "done"},
	})
	allowAssembledWrites(t, assembled)
	if result := assembled.turn(t, "change the file"); result.Err != nil || result.Text != "done" {
		t.Fatalf("turn = %+v", result)
	}
	path := filepath.Join(assembledWorkspace(t, assembled), "f")
	if data, err := os.ReadFile(path); err != nil || string(data) != "a\nC\n" { //nolint:gosec // fixed filename under the workspace from this test's private session header
		t.Fatalf("published = %q, %v", data, err)
	}
	results := orderedToolResults(assembled.records(t))
	oldEdit, oldWrite := "a\nb", "a\nB"
	want := []*session.ToolMeta{
		{Write: &session.WriteMeta{Operation: "create", Diffs: []session.FileDiff{}}},
		{Read: &session.ReadMeta{Path: path, Offset: 2, Lines: []session.ReadLine{{Number: 2, Text: "b"}}, TotalLines: 2}},
		{Edit: &session.EditMeta{Diffs: []session.FileDiff{{Path: path, OldText: &oldEdit, NewText: "a\nB"}}}},
		{Write: &session.WriteMeta{Operation: "update", Diffs: []session.FileDiff{{Path: path, OldText: &oldWrite, NewText: "a\nC"}}}},
	}
	if len(results) != 5 {
		t.Fatalf("results = %+v", results)
	}
	for i, meta := range want {
		if results[i].IsError || !reflect.DeepEqual(results[i].Meta, meta) {
			t.Errorf("disk result %d = %+v, want %+v", i, results[i], meta)
		}
	}
	if result := results[4]; !result.IsError || result.Error == nil || *result.Error != (session.ToolError{Name: "FsError", Code: "FS_NOT_FOUND"}) || result.Meta != nil {
		t.Errorf("disk missing = %+v", result)
	}
	requests := seen()
	if len(requests) != 6 {
		t.Fatalf("requests = %d", len(requests))
	}
	input, _ := json.Marshal(requests[5].Input)
	for _, private := range []string{"FsError", "FS_NOT_FOUND", "old_text", "total_lines", "\"meta\""} {
		if strings.Contains(string(input), private) {
			t.Errorf("model input contains %s", private)
		}
	}
}
