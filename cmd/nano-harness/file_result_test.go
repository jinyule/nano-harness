package main

import (
	"encoding/json"
	"fmt"
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

func TestComposition_FileEditDiffUsesActualChangedLines(t *testing.T) {
	line := strings.Repeat("a", 199)
	before := strings.Repeat(line+"\n", 240)
	after := before[:24_000] + "b" + before[24_001:]
	arguments, err := json.Marshal(map[string]string{"file_path": "f", "old_string": before, "new_string": after})
	if err != nil {
		t.Fatal(err)
	}
	assembled, seen := startAssembled(t, []modelStep{
		{tool: "read", arguments: `{"file_path":"f","limit":1}`},
		{tool: "edit", arguments: string(arguments)},
		{text: "done"},
	})
	allowAssembledWrites(t, assembled)
	path := filepath.Join(assembledWorkspace(t, assembled), "f")
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := assembled.turn(t, "change one character"); result.Err != nil || result.Text != "done" {
		t.Fatalf("turn = %+v", result)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != after { //nolint:gosec // fixed filename in the private workspace from this test's session header
		t.Fatalf("published file = %q, %v", data, err)
	}
	results := orderedToolResults(assembled.records(t))
	oldHunk := strings.TrimSuffix(strings.Repeat(line+"\n", 7), "\n")
	newHunk := strings.Repeat(line+"\n", 3) + "b" + line[1:] + "\n" + strings.TrimSuffix(strings.Repeat(line+"\n", 3), "\n")
	want := &session.ToolMeta{Edit: &session.EditMeta{Diffs: []session.FileDiff{{Path: path, OldText: &oldHunk, NewText: newHunk}}}}
	if len(results) != 2 || results[1].IsError || !reflect.DeepEqual(results[1].Meta, want) {
		t.Fatalf("persisted actual-change diff = %+v, want %+v", results, want)
	}
	requests := seen()
	if len(requests) != 3 {
		t.Fatalf("requests = %d", len(requests))
	}
	input, _ := json.Marshal(requests[2].Input)
	for _, private := range []string{"old_text", "diffs", "\"meta\""} {
		if strings.Contains(string(input), private) {
			t.Fatalf("model input contains metadata field %s", private)
		}
	}
}

func TestComposition_FileEditDiffBudgetDoesNotEnterModelInput(t *testing.T) {
	before := strings.Repeat("a\n", 1024) + "keep\n" + strings.Repeat("a\n", 1024)
	after := strings.ReplaceAll(before, "a", "b")
	assembled, seen := startAssembled(t, []modelStep{
		{tool: "read", arguments: `{"file_path":"f","limit":1}`},
		{tool: "edit", arguments: `{"file_path":"f","old_string":"a","new_string":"b","replace_all":true}`},
		{text: "done"},
	})
	allowAssembledWrites(t, assembled)
	path := filepath.Join(assembledWorkspace(t, assembled), "f")
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := assembled.turn(t, "replace the repeated lines"); result.Err != nil || result.Text != "done" {
		t.Fatalf("turn = %+v", result)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != after { //nolint:gosec // fixed filename in the private workspace from this test's session header
		t.Fatalf("published file = %q, %v", data, err)
	}
	results := orderedToolResults(assembled.records(t))
	want := &session.ToolMeta{Edit: &session.EditMeta{Diffs: []session.FileDiff{}, Truncated: true}}
	wantText := fmt.Sprintf("The file %s has been updated. All occurrences were successfully replaced.", path)
	if len(results) != 2 || results[1].IsError || results[1].Output != wantText || !reflect.DeepEqual(results[1].Meta, want) {
		t.Fatalf("persisted bounded diff = %+v, want successful result with truncated metadata", results)
	}
	requests := seen()
	if len(requests) != 3 {
		t.Fatalf("requests = %d", len(requests))
	}
	input, _ := json.Marshal(requests[2].Input)
	if !strings.Contains(string(input), wantText) {
		t.Fatal("next model input lost the unchanged edit text")
	}
	for _, private := range []string{"old_text", "diffs", "\"meta\"", "truncated"} {
		if strings.Contains(string(input), private) {
			t.Fatalf("model input contains metadata field %s", private)
		}
	}
}
