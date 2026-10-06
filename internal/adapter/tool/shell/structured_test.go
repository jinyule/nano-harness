package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	appJob "github.com/jinyule/nano-harness/internal/app/job"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

// diskCall executes against a real journal and independently decodes its file.
func diskCall(ctx context.Context, t *testing.T, h *harness, name string, arguments map[string]any, mode session.SandboxMode) session.ToolResult {
	t.Helper()
	manager, err := sessionjsonl.New(sessionjsonl.Config{Root: filepath.Join(t.TempDir(), "sessions"), CompositionID: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := manager.Start(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	log, err := manager.Open(t.Context(), sessionjsonl.OpenOptions{SessionID: "session-1", Cwd: h.root.Path(), Create: true})
	if err != nil {
		t.Fatal(err)
	}
	if mode != "" {
		if _, err := log.Append(t.Context(), session.Record{Type: session.RecordSandboxMode, Sandbox: &session.SandboxModeChange{Mode: mode}}); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	call := session.ToolCall{ID: "call", Name: name, Arguments: encoded}
	for _, record := range []session.Record{
		{Type: session.RecordTurnStart, Turn: 1},
		{Type: session.RecordStepStart, Turn: 1, Step: 1},
		{Type: session.RecordAssistantMessage, Turn: 1, Step: 1, Message: &session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "model"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "search"}}}},
		{Type: session.RecordToolCall, Turn: 1, Step: 1, Call: &call},
	} {
		if _, err := log.Append(t.Context(), record); err != nil {
			t.Fatal(err)
		}
	}
	result := h.runtime.ExecuteBatch(ctx, appTool.BatchRequest{SessionID: "session-1", Turn: 1, Step: 1, Journal: log, Calls: []session.ToolCall{call}})[0]
	if _, err := log.Append(t.Context(), session.Record{Type: session.RecordToolResult, Turn: 1, Step: 1, Result: &result}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	var event session.Event
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Record.Result != nil {
			return *event.Record.Result
		}
	}
	t.Fatal("no result on disk")
	return session.ToolResult{}
}

const unavailableText = "sandbox mode %q is requested but no sandbox backend is usable on this host; refusing to run the command unconfined. Install bubblewrap or run a Landlock-enforcing kernel (Linux), ensure sandbox-exec is usable (macOS), or ensure the ACL restricted-token runner can start (Windows) — otherwise switch the consumer to danger-full-access."

func TestBash_PersistsSandboxClassificationForActualModes(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("requires a supported sandbox backend")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []session.SandboxMode{session.SandboxReadOnly, session.SandboxWorkspaceWrite} {
		for _, backend := range []string{"missing", "failed"} {
			t.Run(string(mode)+"/"+backend, func(t *testing.T) {
				dir := t.TempDir()
				name := "bwrap"
				if runtime.GOOS == "darwin" {
					name = "sandbox-exec"
				}
				diagnostic := name + ": initialization failed"
				if backend == "failed" {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nprintf '%s\\n' '"+diagnostic+"' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // test-owned sandbox backend must be executable
						t.Fatal(err)
					}
				}
				t.Setenv("PATH", dir)
				h := newHarness(t, platformProcess.New())
				h.provider.bashPath = bash
				result := diskCall(t.Context(), t, h, "bash", map[string]any{"description": "Create proof", "command": "printf ran > proof"}, mode)
				want := "Error: " + fmt.Sprintf(unavailableText, mode)
				if backend == "failed" {
					want += " Runner failure: " + diagnostic
				}
				if !result.IsError || result.Output != want || !reflect.DeepEqual(result.Error, &session.ToolError{Name: "SandboxUnavailableError", Code: "SANDBOX_UNAVAILABLE"}) || result.Meta != nil {
					t.Fatalf("disk result = %+v", result)
				}
				if _, err := os.Stat(filepath.Join(h.root.Path(), "proof")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("command ran: %v", err)
				}
				result = diskCall(t.Context(), t, h, "bash", map[string]any{"description": "Create host proof", "command": "printf ran > proof"}, session.SandboxDangerFullAccess)
				data, err := os.ReadFile(filepath.Join(h.root.Path(), "proof"))
				if result.IsError || result.Error != nil || result.Meta != nil || err != nil || string(data) != "ran" {
					t.Fatalf("host = %+v, proof=%q, %v", result, data, err)
				}
			})
		}
	}
	wrapped := fmt.Errorf("runner: %w", platformProcess.ErrSandboxUnavailable)
	_, err = finish(t.Context(), processRun{err: wrapped}, 1)
	var failure appTool.Failure
	if !errors.Is(err, platformProcess.ErrSandboxUnavailable) || !errors.As(fmt.Errorf("outer: %w", err), &failure) || failure.ToolError() != (session.ToolError{Name: "SandboxUnavailableError", Code: "SANDBOX_UNAVAILABLE"}) {
		t.Fatalf("sandbox cause/classification lost: %v", err)
	}
}

func TestBash_PersistsAbortedWaitAndFallback(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", fallback), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h := newHarness(t, runnerFunc(func(context.Context, platformProcess.Request) (platformProcess.Result, error) {
				cancel()
				return platformProcess.Result{}, context.Canceled
			}))
			if fallback {
				for range 10 {
					if _, err := h.jobs.Launch(appJob.Spec{Owner: "session-1", Kind: "bash", Label: "hold", Run: func(ctx context.Context, _ *appJob.Output) appJob.Outcome {
						<-ctx.Done()
						return appJob.Outcome{Status: appJob.StatusKilled}
					}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			result := diskCall(ctx, t, h, "bash", map[string]any{"description": "Wait for command", "command": "true"}, "")
			if !result.IsError || result.Output != "Error: tool call aborted" || !reflect.DeepEqual(result.Error, &session.ToolError{Name: "AbortError", Code: "ABORTED"}) || result.Meta != nil {
				t.Fatalf("cancel result = %+v", result)
			}
		})
	}
	for _, withContext := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		if withContext {
			cancel()
		}
		_, err := finish(ctx, processRun{err: context.Canceled}, 1)
		cancel()
		var failure appTool.Failure
		if !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.ToolError() != (session.ToolError{Name: "AbortError", Code: "ABORTED"}) {
			t.Fatalf("cancel cause/classification lost: %v", err)
		}
	}
}

func TestBash_LeavesOrdinaryFailuresAndProcessOutcomesUnclassified(t *testing.T) {
	for _, test := range []struct {
		name    string
		result  platformProcess.Result
		err     error
		want    string
		isError bool
	}{
		{name: "start", err: errors.New("start failed"), want: "Error: start failed", isError: true},
		{name: "exit", result: platformProcess.Result{ExitCode: 7}, want: "(no output)\n[exit code: 7]"},
		{name: "signal", result: platformProcess.Result{Signal: "SIGTERM", ExitCode: -1}, want: "(no output)\n[killed by signal: SIGTERM]"},
		{name: "timeout", result: platformProcess.Result{TimedOut: true}, want: "(no output)\n[timed out after 60000ms]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, &fakeRunner{result: test.result, err: test.err})
			result := diskCall(t.Context(), t, h, "bash", map[string]any{"description": "Run command", "command": "true"}, "")
			if result.IsError != test.isError || result.Output != test.want || result.Error != nil || result.Meta != nil {
				t.Fatalf("process outcome = %+v", result)
			}
		})
	}
}
