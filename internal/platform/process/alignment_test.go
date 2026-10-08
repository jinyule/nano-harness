package process

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// upstreamUnavailable is upstream SandboxUnavailableError's message for the
// workspace-write mode (packages/sandbox/sandbox/src/index.ts:132-145).
const upstreamUnavailable = "sandbox mode \"workspace-write\" is requested but no sandbox backend is usable on this host; refusing to run the command unconfined. Install bubblewrap or run a Landlock-enforcing kernel (Linux), ensure sandbox-exec is usable (macOS), or ensure the ACL restricted-token runner can start (Windows) — otherwise switch the consumer to danger-full-access."

func TestRunnerRun_RunnerFailureOutranksDenial(t *testing.T) {
	for _, test := range []struct {
		goos, diagnostic string
	}{
		{"darwin", "sandbox-exec: sandbox initialization failed: Operation not permitted"},
		{"linux", "bwrap: creating namespace: Read-only file system"},
	} {
		t.Run(test.goos, func(t *testing.T) {
			root := t.TempDir()
			fake := filepath.Join(root, "sandbox")
			// Like upstream's /\r?\n/ split, the CR goes and the line's indentation stays.
			if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\r\\n' 'informational line' '  "+test.diagnostic+"' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // test-owned executable fixture
				t.Fatal(err)
			}
			runner := &Runner{goos: test.goos, sandboxPath: fake}
			result, err := runner.Run(t.Context(), Request{Path: "/bin/true", Root: root, Cwd: root, TempDir: root, Mode: ModeWorkspace})
			if !errors.Is(err, ErrSandboxUnavailable) || err.Error() != upstreamUnavailable+" Runner failure:   "+test.diagnostic || !result.RunnerFailed || result.SandboxDenied {
				t.Fatalf("runner failure = %+v, %q; must identify unavailable sandbox without escalation", result, err)
			}
		})
	}
}

type readiness struct {
	ready chan struct{}
}

func (writer *readiness) Write(data []byte) (int, error) {
	select {
	case writer.ready <- struct{}{}:
	default:
	}
	return len(data), nil
}

func TestRunnerRun_GraceEscalatesAndZeroGraceKillsImmediately(t *testing.T) {
	for _, grace := range []time.Duration{0, 50 * time.Millisecond} {
		root := t.TempDir()
		ready := &readiness{ready: make(chan struct{}, 1)}
		ctx, cancel := context.WithCancel(t.Context())
		request := Request{Path: "/bin/sh", Args: []string{"-c", "trap '' TERM; printf ready; while :; do :; done"}, Root: root, Cwd: root, Mode: ModeHost, TerminationGrace: grace, Stdout: ready}
		done := make(chan struct{})
		go func() {
			defer close(done)
			select {
			case <-ready.ready:
				cancel()
			case <-ctx.Done():
			}
		}()
		t.Cleanup(cancel)
		started := time.Now()
		result, err := New().Run(ctx, request)
		cancel()
		<-done
		if !errors.Is(err, context.Canceled) || result.Signal != "SIGKILL" || result.Stdout.Text != "ready" || time.Since(started) < grace || time.Since(started) > 5*time.Second {
			t.Fatalf("grace=%v result=%+v err=%v elapsed=%v", grace, result, err, time.Since(started))
		}
	}
}

func TestRunnerRun_GraceAllowsTERMTrapToFinish(t *testing.T) {
	root := t.TempDir()
	ready := &readiness{ready: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		select {
		case <-ready.ready:
			cancel()
		case <-ctx.Done():
		}
	}()
	result, err := New().Run(ctx, Request{Path: "/bin/sh", Args: []string{"-c", "trap 'printf cleaned; exit 0' TERM; printf ready; while :; do :; done"}, Root: root, Cwd: root, Mode: ModeHost, TerminationGrace: 3 * time.Second, Stdout: ready})
	cancel()
	<-joined
	if !errors.Is(err, context.Canceled) || result.Signal != "" || result.ExitCode != 0 || result.Stdout.Text != "readycleaned" {
		t.Fatalf("TERM cleanup = %+v, %v", result, err)
	}
}

func TestRunnerRun_FailedSandboxSpawnPreservesCause(t *testing.T) {
	root := t.TempDir()
	runner := &Runner{goos: "linux", sandboxPath: filepath.Join(root, "missing")}
	result, err := runner.Run(t.Context(), Request{Path: "/bin/true", Root: root, Cwd: root, TempDir: root, Mode: ModeWorkspace})
	if !errors.Is(err, ErrSandboxUnavailable) || !errors.Is(err, os.ErrNotExist) || !strings.HasPrefix(err.Error(), upstreamUnavailable+" Runner failure: fork/exec "+runner.sandboxPath+": ") || !result.RunnerFailed || result.SandboxDenied {
		t.Fatalf("failed sandbox spawn = %+v, %q", result, err)
	}
	// Without a backend there is no runner failure to report.
	runner.sandboxPath = ""
	if _, err := runner.Run(t.Context(), Request{Path: "/bin/true", Root: root, Cwd: root, TempDir: root, Mode: ModeWorkspace}); !errors.Is(err, ErrSandboxUnavailable) || err.Error() != upstreamUnavailable {
		t.Fatalf("missing backend = %q", err)
	}
}

func TestRunnerRun_InvalidCwdIsNotSandboxRunnerFailure(t *testing.T) {
	root := t.TempDir()
	runner := &Runner{goos: "linux", sandboxPath: "/bin/true"}
	result, err := runner.Run(t.Context(), Request{Path: "/bin/true", Root: root, Cwd: filepath.Join(root, "missing"), TempDir: root, Mode: ModeWorkspace})
	if !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrSandboxUnavailable) || result.RunnerFailed {
		t.Fatalf("invalid cwd was attributed to the sandbox executable: %+v, %v", result, err)
	}
}

func TestRunnerSpawnFailure_RequiresExecutableEvidenceAndUsableCwd(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		err  error
		cwd  string
		want bool
	}{
		{&os.PathError{Op: "fork/exec", Path: "/runner", Err: os.ErrNotExist}, root, true},
		{&os.PathError{Op: "fork/exec", Path: "/runner", Err: os.ErrPermission}, root, true},
		{&os.PathError{Op: "fork/exec", Path: "/runner", Err: os.ErrNotExist}, filepath.Join(root, "missing"), false},
		{&os.PathError{Op: "fork/exec", Path: "/other", Err: os.ErrNotExist}, root, false},
		{errors.New("fork resources unavailable"), root, false},
	} {
		if got := runnerSpawnFailure(test.err, "/runner", test.cwd); got != test.want {
			t.Errorf("spawn classification(%v,%s)=%v; want %v", test.err, test.cwd, got, test.want)
		}
	}
}

// Like upstream's spawn, output a descendant writes after the command exits
// is drained for the termination grace; zero-grace search keeps a short
// fixed drain and its descendants are killed with the group.
func TestRunnerRun_DrainsDescendantOutputForTheGrace(t *testing.T) {
	for _, test := range []struct {
		grace time.Duration
		want  string
	}{{3 * time.Second, "early\nlate\n"}, {0, "early\n"}} {
		root := t.TempDir()
		started := time.Now()
		result, err := New().Run(t.Context(), Request{Path: "/bin/sh", Args: []string{"-c", "(sleep 2; echo late) & echo early"}, Root: root, Cwd: root, Mode: ModeHost, TerminationGrace: test.grace})
		if elapsed := time.Since(started); err != nil || result.Stdout.Text != test.want || test.grace == 0 && elapsed > 1900*time.Millisecond {
			t.Fatalf("grace=%v: %+v, %v after %v", test.grace, result, err, elapsed)
		}
	}
}
