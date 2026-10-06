package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	shelltool "github.com/jinyule/nano-harness/internal/adapter/tool/shell"
	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appJob "github.com/jinyule/nano-harness/internal/app/job"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

func TestComposition_PersistsSearchAndShellStructuredResults(t *testing.T) {
	original := newShellTools
	newShellTools = func(runtime *appTool.Runtime, _ shelltool.Runner, root workspace.Root, jobs *appJob.Service) (*shelltool.Provider, error) {
		return shelltool.New(runtime, foregroundRunner{err: platformProcess.ErrSandboxUnavailable}, root, jobs)
	}
	t.Cleanup(func() { newShellTools = original })
	assembled, seen := startAssembled(t, []modelStep{
		{tool: "glob", arguments: `{"pattern":"*.txt"}`},
		{tool: "grep", arguments: `{"pattern":"needle","path":"a.txt"}`},
		{tool: "grep", arguments: `{"pattern":"needle","path":"missing"}`},
		{tool: "bash", arguments: `{"description":"Run command","command":"true"}`},
		{text: "done"},
	})
	allowSandboxCalls(t, assembled, &allowFrontend{app: assembled.app})
	if err := os.WriteFile(filepath.Join(assembledWorkspace(t, assembled), "a.txt"), []byte("needle\nnone\nneedle again\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := assembled.turn(t, "inspect"); result.Err != nil || result.Text != "done" {
		t.Fatalf("turn = %+v", result)
	}
	results := orderedToolResults(assembled.records(t))
	wantMeta := []*session.ToolMeta{
		{Glob: &session.GlobMeta{Paths: []string{"a.txt"}, Total: 1}},
		{Grep: &session.GrepMeta{Files: []session.GrepFile{{Path: "a.txt", Matches: []session.GrepMatch{{LineNumber: 1, Line: "needle"}, {LineNumber: 3, Line: "needle again"}}}}, Total: 2}},
		nil, nil,
	}
	wantErrors := []*session.ToolError{nil, nil, {Name: "SearchError", Code: "SEARCH_FAILED"}, {Name: "SandboxUnavailableError", Code: "SANDBOX_UNAVAILABLE"}}
	wantText := []string{"a.txt", "Found 2 matches\n\na.txt\nLine 1: needle\nLine 3: needle again", `Error: grep search failed: "missing" not found`, "Error: sandbox mode \"workspace-write\" is requested but no sandbox backend is usable on this host; refusing to run the command unconfined. Install bubblewrap or run a Landlock-enforcing kernel (Linux), ensure sandbox-exec is usable (macOS), or ensure the ACL restricted-token runner can start (Windows) — otherwise switch the consumer to danger-full-access."}
	if len(results) != 4 {
		t.Fatalf("disk results = %d", len(results))
	}
	requests := seen()
	if len(requests) != 5 {
		t.Fatalf("requests = %d", len(requests))
	}
	for index, result := range results {
		if result.Output != wantText[index] || result.IsError != (index >= 2) || !reflect.DeepEqual(result.Meta, wantMeta[index]) || !reflect.DeepEqual(result.Error, wantErrors[index]) {
			t.Errorf("disk result %d = %+v", index, result)
		}
		if output := callOutput(t, requests[4], result.CallID); output != wantText[index] {
			t.Errorf("model output %d = %q", index, output)
		}
	}
	input := requestText(t, requests[4])
	for _, private := range []string{"line_number", "truncated", "SearchError", "SEARCH_FAILED", "SandboxUnavailableError", "SANDBOX_UNAVAILABLE"} {
		if strings.Contains(input, private) {
			t.Errorf("structured field %s reached model", private)
		}
	}
}

// The assembled runtime records its own classifications on disk while the
// next model request carries only the unchanged result text.
func TestComposition_PersistsRuntimeClassificationsOutsideTheModelInput(t *testing.T) {
	assembled, seen := startAssembled(t, []modelStep{
		{tool: "nope", arguments: `{}`},
		{tool: "read", arguments: `{}`},
		{text: "done"},
	})
	if result := assembled.turn(t, "use tools"); result.Err != nil || result.Text != "done" {
		t.Fatalf("turn = %+v", result)
	}
	results := orderedToolResults(assembled.records(t))
	want := []struct{ output, name, code string }{
		{`Error: unknown tool "nope"`, "ToolNotFoundError", "UNKNOWN_TOOL"},
		{`Error: invalid arguments: missing required property "file_path"`, "ToolArgsError", "INVALID_ARGS"},
	}
	if len(results) != len(want) {
		t.Fatalf("results = %+v", results)
	}
	for index, expected := range want {
		result := results[index]
		if !result.IsError || result.Output != expected.output || result.Error == nil || result.Error.Name != expected.name || result.Error.Code != expected.code || result.Meta != nil {
			t.Errorf("result %d = %+v", index, result)
		}
	}
	requests := seen()
	if len(requests) != 3 {
		t.Fatalf("requests = %d", len(requests))
	}
	var last []byte
	for _, item := range requests[2].Input {
		last = append(append(last, item...), '\n')
	}
	if !bytes.Contains(last, []byte(`unknown tool \"nope\"`)) || bytes.Contains(last, []byte("ToolNotFoundError")) || bytes.Contains(last, []byte("UNKNOWN_TOOL")) || bytes.Contains(last, []byte("INVALID_ARGS")) {
		t.Fatalf("model input = %s", last)
	}
}

func TestComposition_StructuredResultsRejectOldRuntimeSessions(t *testing.T) {
	for _, token := range []struct{ current, previous string }{
		{"tool-runtime-v3", "tool-runtime-v2"},
		{"search-tools-v4", "search-tools-v3"},
		{"shell-tools-v5", "shell-tools-v4"},
	} {
		t.Run(token.current, func(t *testing.T) {
			config := applicationConfig{workspaceRoot: "/workspace", attachmentRoot: filepath.Join(t.TempDir(), "attachments")}
			identity := strings.Replace(compositionIdentity(t, config), "\x00"+token.current+"\x00", "\x00"+token.previous+"\x00", 1)
			previous := sha256.Sum256([]byte(identity))
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
			if _, err := log.Append(t.Context(), session.Record{Type: session.RecordTurnStart, Turn: 1}); err != nil {
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
				t.Fatalf("old runtime session accepted: %v", err)
			}
			if _, _, err := current.Inspect(t.Context(), "old"); !errors.Is(err, sessionjsonl.ErrCorruptSession) {
				t.Fatalf("old runtime session inspected: %v", err)
			}
			after, err := os.ReadFile(log.Path())
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("old session changed: %v", err)
			}
		})
	}
}

// compositionIdentity recovers the identity behind compositionID so the
// test can derive the previous runtime version from the current one.
func compositionIdentity(t *testing.T, config applicationConfig) string {
	t.Helper()
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	const prefix = `identity := "nano-harness-v2\x00" + config.workspaceRoot + "`
	_, rest, found := bytes.Cut(source, []byte(prefix))
	if !found {
		t.Fatal("composition identity not found")
	}
	tail, _, _ := bytes.Cut(rest, []byte(`"`))
	identity := "nano-harness-v2\x00" + config.workspaceRoot + strings.ReplaceAll(string(tail), `\x00`, "\x00")
	if sum := sha256.Sum256([]byte(identity)); hex.EncodeToString(sum[:]) != compositionID(config) {
		t.Fatal("recovered identity does not match compositionID")
	}
	return identity
}
