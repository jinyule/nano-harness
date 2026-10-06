package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

func TestCompositionID_RejectsPreviousWebSearchAuditSemantics(t *testing.T) {
	config := applicationConfig{workspaceRoot: "/workspace"}
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
	path := filepath.Join(root, "old.jsonl")
	before, err := os.ReadFile(path) //nolint:gosec // fixed transcript name under the test-owned session root
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
		t.Fatalf("old search audit semantics accepted: %v", err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // fixed transcript name under the test-owned session root
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("rejected session changed: %v", err)
	}
}
