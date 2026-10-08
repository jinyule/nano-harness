package process

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRunner_ReadOnlyUnavailableUsesRequestedMode(t *testing.T) {
	root := t.TempDir()
	request := Request{Path: "/bin/true", Root: root, Cwd: root, TempDir: root, Mode: ModeReadOnly}
	want := strings.Replace(upstreamUnavailable, "workspace-write", "read-only", 1)
	for _, goos := range []string{"linux", "darwin", "unsupported"} {
		runner := &Runner{goos: goos}
		if _, err := runner.Run(t.Context(), request); !errors.Is(err, ErrSandboxUnavailable) || err.Error() != want {
			t.Errorf("%s missing backend: %v; want %s", goos, err, want)
		}
		runner.sandboxPath = filepath.Join(root, "missing")
		result, err := runner.Run(t.Context(), request)
		if goos == "unsupported" {
			if !errors.Is(err, ErrSandboxUnavailable) || err.Error() != want {
				t.Errorf("unsupported backend: %v", err)
			}
		} else if !errors.Is(err, ErrSandboxUnavailable) || !errors.Is(err, os.ErrNotExist) || !result.RunnerFailed || !strings.HasPrefix(err.Error(), want+" Runner failure: ") {
			t.Errorf("%s failed spawn: %+v, %v", goos, result, err)
		}
	}
	for _, goos := range []string{"linux", "darwin"} {
		fake := filepath.Join(root, goos)
		prefix := "bwrap: "
		if goos == "darwin" {
			prefix = "sandbox-exec: "
		}
		diagnostic := prefix + "initialization failed: Operation not permitted; Read-only file system"
		if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' '"+diagnostic+"' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // test-owned executable fixture
			t.Fatal(err)
		}
		result, err := (&Runner{goos: goos, sandboxPath: fake}).Run(t.Context(), request)
		if !errors.Is(err, ErrSandboxUnavailable) || err.Error() != want+" Runner failure: "+diagnostic || !result.RunnerFailed || result.SandboxDenied {
			t.Errorf("%s fatal diagnosis: %+v, %v", goos, result, err)
		}
	}
}

func TestRunner_HostAllowsExternalWorkingDirectory(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	runner := &Runner{}
	result, err := runner.Run(t.Context(), Request{Path: "/bin/sh", Args: []string{"-c", "printf host > proof"}, Root: root, Cwd: outside, Mode: ModeHost})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("host directory: %+v, %v", result, err)
	}
	data, err := os.ReadFile(filepath.Join(outside, "proof")) //nolint:gosec // fixed proof name in this test private directory
	if err != nil || string(data) != "host" {
		t.Fatalf("host effect: %s, %v", data, err)
	}
}

func TestRunnerCommand_SandboxModesShareNetwork(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		for _, mode := range []Mode{Mode("read-only"), ModeWorkspace} {
			t.Run(goos+"/"+string(mode), func(t *testing.T) {
				runner := &Runner{goos: goos, sandboxPath: "/sandbox"}
				_, args, err := runner.command("/work", "/work", Request{Path: "/bin/tool", Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				if goos == "linux" {
					if !slices.Contains(args, "--unshare-pid") || slices.Contains(args, "--unshare-all") || slices.Contains(args, "--unshare-net") {
						t.Fatalf("network/PID namespaces: %v", args)
					}
					if writable := slices.Contains(args, "--bind") || slices.Contains(args, "--tmpfs"); writable != (mode == ModeWorkspace) {
						t.Fatalf("writable mounts: %v", args)
					}
				} else if writable := strings.Contains(args[1], "(subpath"); writable != (mode == ModeWorkspace) {
					t.Fatalf("writable Seatbelt grant: %s", args[1])
				}
			})
		}
	}
}

// sandboxUnusable skips a real-backend test whose host precondition is
// missing. NANO_HARNESS_REQUIRE_SANDBOX makes the precondition mandatory
// instead, so CI cannot pass by skipping; see docs/testing.md.
func sandboxUnusable(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("NANO_HARNESS_REQUIRE_SANDBOX") != "" {
		t.Fatalf("NANO_HARNESS_REQUIRE_SANDBOX is set but "+format, args...)
	}
	t.Skipf(format, args...)
}

// requireSandbox runs the smallest invocation of the host backend, outside
// the runner's profile, so a runner defect fails the test rather than
// passing for a missing backend.
func requireSandbox(t *testing.T) {
	t.Helper()
	probe := map[string][]string{
		"linux":  {"bwrap", "--ro-bind", "/", "/", "--", "true"},
		"darwin": {"sandbox-exec", "-p", "(version 1)(allow default)", "true"},
	}[runtime.GOOS]
	if probe == nil {
		sandboxUnusable(t, "no sandbox backend exists for %s", runtime.GOOS)
	}
	if output, err := exec.CommandContext(t.Context(), probe[0], probe[1:]...).CombinedOutput(); err != nil {
		sandboxUnusable(t, "the sandbox backend probe failed: %v: %s", err, output)
	}
}

// deniedTarget returns a directory confined commands see read-only. Linux
// gives each command a private /tmp, so the target must lie outside both the
// workspace and the host /tmp; the package directory qualifies unless the
// checkout itself is under /tmp.
func deniedTarget(t *testing.T) string {
	t.Helper()
	outside, err := os.MkdirTemp(".", ".sandbox-outside-") //nolint:usetesting // t.TempDir lies under the host /tmp, which confined Linux commands cannot see
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	if outside, err = filepath.Abs(outside); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "linux" && strings.HasPrefix(outside, "/tmp/") {
		sandboxUnusable(t, "the checkout %s is under /tmp, which confined Linux commands see as a private tmpfs", outside)
	}
	return outside
}

// TestRunner_RealSandboxConfinesWrites runs the host backend and observes
// every file effect from the host. On Linux the workspace sits under the
// host /tmp, the layout the private /tmp mount once hid.
func TestRunner_RealSandboxConfinesWrites(t *testing.T) {
	requireSandbox(t)
	root := t.TempDir()
	if runtime.GOOS == "linux" {
		var err error
		if root, err = os.MkdirTemp("/tmp", "nano-harness-sandbox-"); err != nil { //nolint:usetesting // the regression needs /tmp itself, wherever TMPDIR points
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(root) })
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(root, "tmp")
	if err := os.Mkdir(temporary, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := deniedTarget(t)
	runner := New()
	run := func(mode Mode, script string) Result {
		t.Helper()
		result, err := runner.Run(t.Context(), Request{Path: "/bin/sh", Args: []string{"-c", script}, Root: root, Cwd: root, TempDir: temporary, Mode: mode, Timeout: 10 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := run(ModeWorkspace, `printf inside > inside && printf temp > "$TMPDIR/temp"`); result.ExitCode != 0 {
		t.Fatalf("workspace write = %+v", result)
	}
	for path, want := range map[string]string{filepath.Join(root, "inside"): "inside", filepath.Join(temporary, "temp"): "temp"} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want { //nolint:gosec // fixed names in this test's private directories
			t.Fatalf("%s = %q, %v", path, data, err)
		}
	}
	escape := filepath.Join(outside, "escape")
	if result := run(ModeWorkspace, "printf x > '"+escape+"'"); result.ExitCode == 0 || !result.SandboxDenied {
		t.Fatalf("outside write = %+v", result)
	}
	if result := run(ModeReadOnly, "printf x > blocked"); result.ExitCode == 0 || !result.SandboxDenied {
		t.Fatalf("read-only write = %+v", result)
	}
	for _, path := range []string{escape, filepath.Join(root, "blocked")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("confined command wrote %s: %v", path, err)
		}
	}
	if runtime.GOOS == "linux" {
		// Like upstream's tmpfs, /tmp is writable but private to the command.
		private := "/tmp/" + filepath.Base(outside)
		if result := run(ModeWorkspace, "printf x > "+private+" && cat "+private); result.ExitCode != 0 || result.Stdout.Text != "x" {
			t.Fatalf("private /tmp = %+v", result)
		}
		if _, err := os.Stat(private); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private /tmp reached the host: %v", err)
		}
		if result := run(ModeWorkspace, "test ! -e "+private); result.ExitCode != 0 {
			t.Fatalf("private /tmp outlived its command: %+v", result)
		}
	}
}
