package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func assembledWorkspace(t *testing.T, assembled *assembledApp) string {
	t.Helper()
	data, err := os.ReadFile(assembled.transcript)
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Header session.Header `json:"header"`
	}
	if err := json.Unmarshal(bytes.SplitN(data, []byte("\n"), 2)[0], &entry); err != nil {
		t.Fatal(err)
	}
	return entry.Header.Cwd
}

func allowAssembledWrites(t *testing.T, assembled *assembledApp) {
	t.Helper()
	scope := &plugin.Scope{}
	if err := assembled.app.approval.RegisterBroker(&allowFrontend{app: assembled.app}, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
}

func TestComposition_LargeWriteAndEditArgumentsEndToEnd(t *testing.T) {
	content := strings.Repeat("x", 131_072)
	for _, tool := range []string{"write", "edit"} {
		t.Run(tool, func(t *testing.T) {
			arguments := map[string]string{"file_path": "large.txt"}
			var steps []modelStep
			if tool == "write" {
				arguments["content"] = content
			} else {
				steps = append(steps, modelStep{tool: "read", arguments: `{"file_path":"large.txt"}`})
				arguments["old_string"], arguments["new_string"] = "before", content
			}
			encoded, _ := json.Marshal(arguments)
			steps = append(steps, modelStep{tool: tool, arguments: string(encoded)}, modelStep{text: "done"})
			assembled, seen := startAssembled(t, steps)
			allowAssembledWrites(t, assembled)
			path := filepath.Join(assembledWorkspace(t, assembled), "large.txt")
			if tool == "edit" {
				if err := os.WriteFile(path, []byte("before"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if result := assembled.turn(t, "write the file"); result.Err != nil || result.Outcome != session.OutcomeCompleted {
				t.Fatalf("large %s ended the turn: %+v", tool, result)
			}
			if actual, err := os.ReadFile(path); err != nil || string(actual) != content { //nolint:gosec // fixed file name in the workspace from this test's private session header
				t.Fatalf("large %s file: size=%d, err=%v", tool, len(actual), err)
			}
			results := orderedToolResults(assembled.records(t))
			if len(results) == 0 || results[len(results)-1].IsError || len(seen()) != len(steps) {
				t.Fatalf("large %s results=%+v, requests=%d", tool, results, len(seen()))
			}
		})
	}
}

func TestComposition_OversizedArgumentsRecoverEndToEnd(t *testing.T) {
	arguments, _ := json.Marshal(map[string]string{"file_path": "refused.txt", "content": strings.Repeat("x", session.MaxArgumentsBytes)})
	assembled, seen := startAssembled(t, []modelStep{
		{tool: "write", arguments: string(arguments)},
		{tool: "write", arguments: `{"file_path":"recovered.txt","content":"recovered"}`},
		{text: "done"},
	})
	allowAssembledWrites(t, assembled)
	if result := assembled.turn(t, "write then recover"); result.Err != nil || result.Outcome != session.OutcomeCompleted {
		t.Fatalf("oversized call ended the turn: %+v", result)
	}
	root := assembledWorkspace(t, assembled)
	if _, err := os.Stat(filepath.Join(root, "refused.txt")); !os.IsNotExist(err) {
		t.Fatalf("oversized call changed the filesystem: %v", err)
	}
	if actual, err := os.ReadFile(filepath.Join(root, "recovered.txt")); err != nil || string(actual) != "recovered" { //nolint:gosec // fixed file name in the workspace from this test's private session header
		t.Fatalf("recovery write=%q, err=%v", actual, err)
	}
	records := assembled.records(t)
	results := orderedToolResults(records)
	if len(results) != 2 || !results[0].IsError || !strings.Contains(results[0].Output, "tool arguments exceed") || results[1].IsError {
		t.Fatalf("results=%+v", results)
	}
	requests := seen()
	if len(requests) != 3 {
		t.Fatalf("requests=%d, want 3", len(requests))
	}
	input, _ := json.Marshal(requests[1].Input)
	if !strings.Contains(string(input), "tool arguments exceed") {
		t.Fatal("next model request lacks the recoverable error")
	}
	var approvals, omissions int
	for _, record := range records {
		if record.Call != nil && record.Call.ArgumentsOmitted {
			omissions++
			if string(record.Call.Arguments) != "{}" {
				t.Fatal("oversized arguments persisted")
			}
		}
		if record.Type == session.RecordApprovalAsked {
			approvals++
			if record.Approval.CallID != "call-1" {
				t.Fatal("oversized write requested approval")
			}
		}
	}
	if omissions != 1 || approvals != 1 {
		t.Fatalf("omissions=%d, approvals=%d; want 1 each", omissions, approvals)
	}
}

func TestComposition_ArgumentRuntimeRejectsOldSessionsWithoutChangingThem(t *testing.T) {
	config := applicationConfig{workspaceRoot: "/workspace", attachmentRoot: filepath.Join(t.TempDir(), "attachments")}
	previous := sha256.Sum256([]byte("nano-harness-v2\x00/workspace\x00fs-tools-v3\x00search-tools-v3\x00shell-tools-v3\x00job-tools-v1\x00subagent-tools-v3\x00todo-tools-v1\x00web-tools-v1\x00question-tools-v1\x00plan-tools-v1\x00skill-tools-v1\x00goal-tools-v2\x00spill-v1\x00attachments-v1\x00session-v2"))
	root := filepath.Join(t.TempDir(), "sessions")
	old, err := sessionjsonl.New(sessionjsonl.Config{Root: root, CompositionID: hex.EncodeToString(previous[:])})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := old.Start(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	log, err := old.Open(t.Context(), sessionjsonl.OpenOptions{SessionID: "old", Cwd: config.workspaceRoot, Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	current, err := sessionjsonl.New(sessionjsonl.Config{Root: root, CompositionID: compositionID(config)})
	if err != nil {
		t.Fatal(err)
	}
	currentScope := &plugin.Scope{}
	if err := current.Start(t.Context(), currentScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = currentScope.Close(context.Background()) })
	if _, err := current.Open(t.Context(), sessionjsonl.OpenOptions{SessionID: "old", Cwd: config.workspaceRoot}); !errors.Is(err, sessionjsonl.ErrCorruptSession) {
		t.Fatalf("old session accepted: %v", err)
	}
	after, err := os.ReadFile(log.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("old session changed: %v", err)
	}
}
