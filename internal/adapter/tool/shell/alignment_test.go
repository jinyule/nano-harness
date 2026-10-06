package shell

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	appJob "github.com/jinyule/nano-harness/internal/app/job"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

func TestBash_RunnerFailureHasInfrastructureExplanation(t *testing.T) {
	name := "sandbox-exec"
	if runtime.GOOS == "linux" {
		name = "bwrap"
	} else if runtime.GOOS != "darwin" {
		t.Skip("requires a supported sandbox backend")
	}
	dir := t.TempDir()
	diagnostic := name + ": initialization failed: Operation not permitted; Read-only file system"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nprintf '%s\\n' '"+diagnostic+"' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // test-owned executable fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	h := newHarness(t, platformProcess.New())
	result := h.call(t, map[string]any{"description": "Run command", "command": "touch should-not-exist"})
	if !result.IsError || !strings.Contains(result.Output, "SANDBOX_UNAVAILABLE") || !strings.Contains(result.Output, diagnostic) {
		t.Errorf("foreground = %#v", result)
	}
	result = h.call(t, map[string]any{"description": "Run command", "command": "touch should-not-exist", "run_in_background": true})
	id := strings.TrimPrefix(result.Output, "started background job ")
	view := h.settled(t, id)
	if view.Status != "failed" || !strings.Contains(view.Detail, "command did not run") || strings.Contains(view.Detail, "escalation available") {
		t.Errorf("background = %+v", view)
	}
	if _, err := os.Stat(filepath.Join(h.root.Path(), "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("target command ran: %v", err)
	}
}

func TestBash_ShutdownGraceIsConcurrentAndBounded(t *testing.T) {
	ready := make(chan chan struct{}, 2)
	runner := runnerFunc(func(ctx context.Context, request platformProcess.Request) (platformProcess.Result, error) {
		writer := &readyWriter{ready: make(chan struct{})}
		ready <- writer.ready
		request.Stdout = io.MultiWriter(request.Stdout, writer)
		return platformProcess.New().Run(ctx, request)
	})
	h := newHarness(t, runner)
	for range 2 {
		result := h.call(t, map[string]any{"description": "Ignore termination", "command": "trap '' TERM; printf ready; while :; do :; done", "run_in_background": true, "sandbox_permissions": "danger-full-access", "justification": "test bounded shutdown"})
		if result.IsError {
			t.Fatal(result.Output)
		}
		var signal chan struct{}
		select {
		case signal = <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("runner never started")
		}
		select {
		case <-signal:
		case <-time.After(5 * time.Second):
			t.Fatal("command never reached readiness")
		}
	}
	started := time.Now()
	if err := h.jobScope.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 3*time.Second || elapsed > 5*time.Second {
		t.Fatalf("parallel graceful shutdown took %v; expected one grace plus pipe drain", elapsed)
	}
	if len(h.jobs.List("session-1")) != 0 || len(h.notifier.texts()) != 0 {
		t.Fatal("shutdown retained jobs or published a late notice")
	}
}

type readyWriter struct {
	ready chan struct{}
	once  sync.Once
}

func (writer *readyWriter) Write(data []byte) (int, error) {
	writer.once.Do(func() { close(writer.ready) })
	return len(data), nil
}

func TestBash_CancellationAndShutdownRunTERMTrap(t *testing.T) {
	for _, action := range []string{"foreground", "fallback", "kill", "shutdown"} {
		t.Run(action, func(t *testing.T) {
			ready := &readyWriter{ready: make(chan struct{})}
			runner := runnerFunc(func(ctx context.Context, request platformProcess.Request) (platformProcess.Result, error) {
				if request.Stdout == nil {
					request.Stdout = ready
				} else {
					request.Stdout = io.MultiWriter(request.Stdout, ready)
				}
				return platformProcess.New().Run(ctx, request)
			})
			h := newHarness(t, runner)
			if action == "fallback" {
				for range 10 {
					_, err := h.jobs.Launch(appJob.Spec{Kind: "held", Label: "hold admission", Owner: "session-1", Run: func(ctx context.Context, _ *appJob.Output) appJob.Outcome {
						<-ctx.Done()
						return appJob.Outcome{Status: appJob.StatusKilled}
					}})
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if h.provider.bashPath == "" {
				t.Skip("bash is not installed")
			}
			command := `trap 'printf cleaned > term-cleanup; exit 0' TERM; printf ready; while :; do :; done`
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			callDone := make(chan error, 1)
			go func() {
				background, mode, justification := action == "kill" || action == "shutdown", "danger-full-access", "test"
				_, err := h.provider.bash(ctx, appTool.Invocation{SessionID: "session-1", Approved: true}, bashArgs{Description: "Hold with trap", Command: command, RunInBackground: &background, SandboxPermissions: &mode, Justification: &justification})
				callDone <- err
			}()
			select {
			case <-ready.ready:
			case <-time.After(5 * time.Second):
				t.Fatal("command never reached TERM readiness")
			}
			started := time.Now()
			switch action {
			case "foreground", "fallback":
				cancel()
			case "kill":
				_, _, _ = h.jobs.Kill("session-1", "bash-1", new("done"))
				h.settled(t, "bash-1")
			case "shutdown":
				if err := h.jobScope.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if err := <-callDone; (action == "foreground" || action == "fallback") && (err == nil || err.Error() != "tool call aborted") || (action == "kill" || action == "shutdown") && err != nil {
				t.Fatalf("call = %v", err)
			}
			if data, err := os.ReadFile(filepath.Join(h.root.Path(), "term-cleanup")); err != nil || string(data) != "cleaned" {
				t.Fatalf("TERM trap did not run: %q, %v", data, err)
			}
			if time.Since(started) > 5*time.Second {
				t.Fatal("termination exceeded grace plus drain bound")
			}
		})
	}
}
