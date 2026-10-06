package shell

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appJob "github.com/jinyule/nano-harness/internal/app/job"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

type runnerFunc func(context.Context, platformProcess.Request) (platformProcess.Result, error)

func (run runnerFunc) Run(ctx context.Context, request platformProcess.Request) (platformProcess.Result, error) {
	return run(ctx, request)
}

func TestBash_ForegroundHandoffUsesReadStatus(t *testing.T) {
	for _, test := range []struct {
		name   string
		result platformProcess.Result
		err    error
		want   string
	}{
		{name: "completed", result: platformProcess.Result{Stdout: platformProcess.Output{Text: "finished"}, ExitCode: 3}, want: "finished\n[exit code: 3]"},
		{name: "failed", err: platformProcess.ErrSandboxUnavailable},
		{name: "killed", result: platformProcess.Result{Signal: "SIGKILL", ExitCode: -1}, want: "(no output)\n[killed by signal: SIGKILL]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{block: make(chan struct{}), result: test.result, err: test.err}
			h := newHarness(t, runner)
			run := &processRun{}
			spec := h.provider.job(appTool.Invocation{SessionID: "session-1"}, "fast", platformProcess.Request{}, run)
			spec.Foreground = true
			id, err := h.jobs.Launch(spec)
			if err != nil {
				t.Fatal(err)
			}
			view, err := h.jobs.Wait(t.Context(), "session-1", id, time.Nanosecond)
			if err != nil || view.Status != appJob.StatusRunning {
				t.Fatalf("timeout = %+v, %v", view, err)
			}
			close(runner.block)
			// Wait joins settlement before the foreground's handoff Read.
			h.settled(t, id)
			result, err := h.provider.foregroundResult(t.Context(), "session-1", id, view, run, 1)
			if !errors.Is(err, test.err) || test.err == nil && result.Text != test.want {
				t.Fatalf("handoff = %+v, %v; want %q, %v", result, err, test.want, test.err)
			}
			if strings.Contains(result.Text, "still running") || len(h.jobs.List("session-1")) != 0 {
				t.Fatalf("settled job was handed to the model as running: %+v", result)
			}
			if err := h.jobScope.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if texts := h.notifier.texts(); len(texts) != 0 {
				t.Fatalf("handoff reported completion twice: %q", texts)
			}
		})
	}
}

func TestBash_ForegroundHandoffCancellationKillsOwnedJob(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	runner := runnerFunc(func(ctx context.Context, _ platformProcess.Request) (platformProcess.Result, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return platformProcess.Result{ExitCode: -1, Signal: "SIGTERM"}, ctx.Err()
	})
	h := newHarness(t, runner)
	run := &processRun{}
	spec := h.provider.job(appTool.Invocation{SessionID: "session-1"}, "held command", platformProcess.Request{TerminationGrace: terminationGrace}, run)
	spec.Foreground = true
	id, err := h.jobs.Launch(spec)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	view, err := h.jobs.Wait(ctx, "session-1", id, time.Nanosecond)
	if err != nil || view.Status != appJob.StatusRunning {
		t.Fatalf("timeout = %+v, %v", view, err)
	}
	// Cancellation is ordered after timeout and before the consuming handoff.
	cancel()
	result, handoffErr := h.provider.foregroundResult(ctx, "session-1", id, view, run, 1)
	remaining := h.jobs.List("session-1")
	if handoffErr == nil || handoffErr.Error() != "tool call aborted" || result.Text != "" || len(remaining) != 0 {
		t.Fatalf("cancelled handoff = %q, %v; remaining jobs = %+v", result.Text, handoffErr, remaining)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("handoff cancellation returned before the runner stopped")
	}
	if len(h.notifier.texts()) != 0 {
		t.Fatal("cancelled undisclosed job sent a completion notice")
	}
}

func TestBash_ForegroundHandoffCancellationRunsTERMTrap(t *testing.T) {
	ready := &readyWriter{ready: make(chan struct{})}
	runner := runnerFunc(func(ctx context.Context, request platformProcess.Request) (platformProcess.Result, error) {
		request.Stdout = io.MultiWriter(request.Stdout, ready)
		return platformProcess.New().Run(ctx, request)
	})
	h := newHarness(t, runner)
	if h.provider.bashPath == "" {
		t.Skip("bash is not installed")
	}
	run := &processRun{}
	request := platformProcess.Request{
		Path: h.provider.bashPath, Args: []string{"-c", `trap 'printf cleaned > term-cleanup; exit 0' TERM; printf ready; while :; do :; done`},
		Root: h.root.Path(), Cwd: h.root.Path(), TempDir: h.provider.temporary(), Mode: platformProcess.ModeHost, TerminationGrace: terminationGrace,
	}
	spec := h.provider.job(appTool.Invocation{SessionID: "session-1"}, "TERM trap", request, run)
	spec.Foreground = true
	id, err := h.jobs.Launch(spec)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("command never reached readiness")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	view, err := h.jobs.Wait(ctx, "session-1", id, time.Nanosecond)
	if err != nil || view.Status != appJob.StatusRunning {
		t.Fatalf("timeout = %+v, %v", view, err)
	}
	cancel()
	started := time.Now()
	result, handoffErr := h.provider.foregroundResult(ctx, "session-1", id, view, run, 1)
	if handoffErr == nil || handoffErr.Error() != "tool call aborted" || result.Text != "" || len(h.jobs.List("session-1")) != 0 {
		t.Fatalf("cancelled real handoff = %q, %v; remaining jobs = %+v", result.Text, handoffErr, h.jobs.List("session-1"))
	}
	if data, err := os.ReadFile(filepath.Join(h.root.Path(), "term-cleanup")); err != nil || string(data) != "cleaned" {
		t.Fatalf("handoff cancellation did not join TERM cleanup: %q, %v", data, err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("handoff cancellation exceeded settlement budget")
	}
}

func TestProvider_ShutdownCancelsAndJoinsFallbackBeforeRemovingTemporary(t *testing.T) {
	restoreHooks(t)
	backgroundStarted := make(chan struct{}, 10)
	started := make(chan platformProcess.Request, 1)
	cancelled, release, removed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	runner := runnerFunc(func(ctx context.Context, request platformProcess.Request) (platformProcess.Result, error) {
		if request.Timeout == 0 {
			backgroundStarted <- struct{}{}
			<-ctx.Done()
			return platformProcess.Result{}, ctx.Err()
		}
		started <- request
		select {
		case <-ctx.Done():
			close(cancelled)
			<-release
		case <-removed:
		}
		return platformProcess.Result{}, context.Canceled
	})
	h := newHarness(t, runner)
	for range 10 {
		if _, err := h.jobs.Launch(appJob.Spec{Kind: "bash", Label: "hold", Owner: "session-1", Run: func(ctx context.Context, _ *appJob.Output) appJob.Outcome {
			_, err := runner.Run(ctx, platformProcess.Request{})
			return outcome(platformProcess.Result{}, err)
		}}); err != nil {
			t.Fatal(err)
		}
		<-backgroundStarted
	}
	callDone := make(chan error, 1)
	callContext, cancelCall := context.WithCancel(t.Context())
	defer cancelCall()
	go func() {
		_, err := h.provider.bash(callContext, appTool.Invocation{SessionID: "session-1", Approved: true}, bashArgs{Description: "hold", Command: "hold"})
		callDone <- err
	}()
	request := <-started
	removeTemporary = func(path string) error {
		close(removed)
		return os.RemoveAll(path)
	}
	shutdownDone := make(chan error, 1)
	go func() {
		_ = h.jobScope.Close(context.Background())
		shutdownDone <- h.providerScope.Close(context.Background())
	}()
	select {
	case <-cancelled:
		if _, err := os.Stat(request.TempDir); err != nil {
			t.Errorf("temporary removed before fallback returned: %v", err)
		}
		select {
		case <-removed:
			t.Error("cleanup did not join the cancelled fallback")
		default:
		}
		close(release)
	case <-removed:
		close(release)
		t.Error("cleanup removed temporary directory without cancelling fallback")
	case <-time.After(5 * time.Second):
		t.Error("cleanup failed to cancel fallback")
		cancelCall()
		close(release)
	}
	if err := <-callDone; err == nil || err.Error() != "tool call aborted" {
		t.Errorf("fallback result = %v", err)
	}
	if err := <-shutdownDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(request.TempDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary survived shutdown: %v", err)
	}
	if catalog, _ := h.runtime.Catalog(nil); len(catalog.Definitions) != 0 {
		t.Fatal("bash survived shutdown")
	}
}

func TestBash_FallbackAdmissionAndHandoffRefuseShutdown(t *testing.T) {
	h := newHarness(t, &fakeRunner{})
	for range 10 {
		if _, err := h.jobs.Launch(appJob.Spec{Kind: "bash", Label: "hold", Owner: "session-1", Run: func(ctx context.Context, _ *appJob.Output) appJob.Outcome {
			<-ctx.Done()
			return appJob.Outcome{Status: appJob.StatusKilled}
		}}); err != nil {
			t.Fatal(err)
		}
	}
	request := platformProcess.Request{TempDir: h.provider.temporary()}
	if err := h.providerScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.provider.foreground(t.Context(), appTool.Invocation{SessionID: "session-1"}, "late", request, 1); err == nil || err.Error() != "shell tools are not running" {
		t.Fatalf("fallback admitted after cleanup = %v", err)
	}
	if err := h.jobScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.provider.foregroundResult(t.Context(), "session-1", "bash-1", appJob.View{Status: appJob.StatusRunning}, &processRun{}, 1); err == nil || err.Error() != "tool call aborted" {
		t.Fatalf("handoff after shutdown = %v", err)
	}
}
