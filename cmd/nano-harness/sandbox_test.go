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
	"time"

	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func allowSandboxCalls(t *testing.T, assembled *assembledApp, broker approval.Broker) {
	t.Helper()
	scope := &plugin.Scope{}
	if err := assembled.app.approval.RegisterBroker(broker, scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
}

func TestComposition_SandboxModesAndSwitch(t *testing.T) {
	for _, mode := range []session.SandboxMode{session.SandboxReadOnly, session.SandboxWorkspaceWrite, session.SandboxDangerFullAccess} {
		t.Run(string(mode), func(t *testing.T) {
			outside, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			target := "file.txt"
			if mode == session.SandboxDangerFullAccess {
				target = filepath.Join(outside, "file.txt")
			}
			args := func(values map[string]string) string { encoded, _ := json.Marshal(values); return string(encoded) }
			assembled, seen := startAssembled(t, []modelStep{
				{tool: "read", arguments: args(map[string]string{"file_path": target})},
				{tool: "write", arguments: args(map[string]string{"file_path": target, "content": "written"})},
				{tool: "edit", arguments: args(map[string]string{"file_path": target, "old_string": "written", "new_string": "edited"})},
				{tool: "bash", arguments: `{"description":"Create command proof", "command":"printf shell > shell.txt"}`},
				{text: "first"},
				{tool: "write", arguments: args(map[string]string{"file_path": target, "content": "changed"})},
				{text: "second"},
			})
			allowSandboxCalls(t, assembled, &allowFrontend{app: assembled.app})
			rootStatus := assembled.root.Status()
			workspace := assembledWorkspace(t, assembled)
			path := target
			if !filepath.IsAbs(path) {
				path = filepath.Join(workspace, path)
			}
			if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := assembled.app.registry.SetSandboxMode(t.Context(), rootStatus.SessionID, mode); err != nil {
				t.Fatal(err)
			}
			if result := assembled.turn(t, "first"); result.Err != nil || result.Text != "first" {
				t.Fatalf("turn=%+v", result)
			}
			contents, err := os.ReadFile(path) //nolint:gosec // fixed target in this test workspace or its private host directory
			if err != nil {
				t.Fatal(err)
			}
			want := "edited"
			if mode == session.SandboxReadOnly {
				want = "old"
			}
			if string(contents) != want {
				t.Fatalf("mode=%s file=%s", mode, contents)
			}
			_, shellErr := os.Stat(filepath.Join(workspace, "shell.txt"))
			if (shellErr == nil) != (mode != session.SandboxReadOnly) {
				t.Fatalf("shell effect mode=%s: %v", mode, shellErr)
			}
			requests := seen()
			if len(requests) != 5 || !strings.Contains(string(requests[0].Input[len(requests[0].Input)-1]), "Current DSH file policy: "+string(mode)) {
				t.Fatalf("policy location: %+v", requests)
			}
			if strings.Contains(requests[0].Instructions, "Current DSH file policy:") {
				t.Fatal("dynamic policy leaked into stable system prompt")
			}
			if err := assembled.app.registry.SetSandboxMode(t.Context(), rootStatus.SessionID, session.SandboxReadOnly); err != nil {
				t.Fatal(err)
			}
			// Close and reopen the actual JSONL journal; the event alone restores policy.
			if err := assembled.app.registry.Close(t.Context(), rootStatus.SessionID); err != nil {
				t.Fatal(err)
			}
			resumed, err := assembled.app.registry.Create(t.Context(), agent.CreateRequest{SessionID: rootStatus.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			assembled.root = resumed
			if result := assembled.turn(t, "second"); result.Err != nil {
				t.Fatal(result.Err)
			}
			contents, _ = os.ReadFile(path) //nolint:gosec // same private target after resume
			if string(contents) != want {
				t.Fatalf("resume lost read-only: %s", contents)
			}
			records := assembled.records(t)
			var asks int
			for _, record := range records {
				if record.Type == session.RecordApprovalAsked {
					asks++
				}
			}
			wantAsks := 3
			if mode == session.SandboxReadOnly {
				wantAsks = 1
			}
			if asks != wantAsks {
				t.Fatalf("approvals=%d want=%d", asks, wantAsks)
			}
			results := orderedToolResults(records)
			if !results[len(results)-1].IsError || !strings.Contains(results[len(results)-1].Output, "read-only mode") {
				t.Fatalf("resumed mutation=%+v", results)
			}
		})
	}
}

type switchBroker struct {
	entered chan struct{}
	release chan struct{}
}

func (broker *switchBroker) Ask(ctx context.Context, _ approval.Question) session.ApprovalOutcome {
	close(broker.entered)
	select {
	case <-broker.release:
		return session.ApprovalAllowedOnce
	case <-ctx.Done():
		return session.ApprovalCancelled
	}
}

func TestComposition_SandboxSwitchWhileApprovalIsPending(t *testing.T) {
	assembled, seen := startAssembled(t, []modelStep{{tool: "write", arguments: `{"file_path":"blocked","content":"x"}`}, {text: "denied"}})
	broker := &switchBroker{entered: make(chan struct{}), release: make(chan struct{})}
	allowSandboxCalls(t, assembled, broker)
	turn, err := assembled.root.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "work"}}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-broker.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("approval was not reached")
	}
	if err := assembled.app.registry.SetSandboxMode(t.Context(), "session-plan", session.SandboxReadOnly); err != nil {
		t.Fatal(err)
	}
	close(broker.release)
	select {
	case result := <-turn:
		if result.Err != nil {
			t.Fatal(result.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("switched turn did not finish")
	}
	if _, err := os.Stat(filepath.Join(assembledWorkspace(t, assembled), "blocked")); !os.IsNotExist(err) {
		t.Fatalf("switched write changed filesystem: %v", err)
	}
	requests := seen()
	if len(requests) != 2 || !strings.Contains(string(requests[1].Input[len(requests[1].Input)-1]), "Current DSH file policy: read-only") {
		t.Fatalf("changed context absent: %+v", requests)
	}
	records := assembled.records(t)
	var trace []string
	for _, record := range records {
		if record.Type == session.RecordApprovalAsked || record.Type == session.RecordSandboxMode || record.Type == session.RecordApprovalDecided || record.Type == session.RecordToolResult {
			trace = append(trace, string(record.Type))
		}
	}
	if strings.Join(trace, ",") != "approval/asked,sandbox/mode,approval/decided,tool/result" {
		t.Fatalf("switch timing=%v", trace)
	}
	result := orderedToolResults(records)[0]
	if !result.IsError || !strings.Contains(result.Output, "read-only") {
		t.Fatalf("execution bypassed mode switch: %+v", result)
	}
}

func TestComposition_SandboxRejectsOldSessionsWithoutChangingThem(t *testing.T) {
	config := applicationConfig{workspaceRoot: "/workspace"}
	previous := sha256.Sum256([]byte("nano-harness-v2\x00/workspace\x00tool-runtime-v2\x00fs-tools-v3\x00search-tools-v3\x00shell-tools-v3\x00job-tools-v1\x00subagent-tools-v3\x00todo-tools-v1\x00web-tools-v2\x00question-tools-v1\x00plan-tools-v1\x00skill-tools-v1\x00goal-tools-v2\x00spill-v1\x00attachments-v1\x00session-v2"))
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
	if _, _, err := current.Inspect(t.Context(), "old"); !errors.Is(err, sessionjsonl.ErrCorruptSession) {
		t.Fatalf("Inspect old composition: %v", err)
	}
	if _, err := current.Open(t.Context(), sessionjsonl.OpenOptions{SessionID: "old", Cwd: config.workspaceRoot}); !errors.Is(err, sessionjsonl.ErrCorruptSession) {
		t.Fatalf("Open old composition: %v", err)
	}
	after, err := os.ReadFile(log.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejection changed old session: %v", err)
	}
}
