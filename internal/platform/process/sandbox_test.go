package process

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
				_, args, err := runner.command("/work", "/work", "/work/tmp", Request{Path: "/bin/tool", Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				if goos == "linux" {
					if !slices.Contains(args, "--unshare-pid") || slices.Contains(args, "--unshare-all") || slices.Contains(args, "--unshare-net") {
						t.Fatalf("network/PID namespaces: %v", args)
					}
					if writable := slices.Contains(args, "--bind"); writable != (mode == ModeWorkspace) {
						t.Fatalf("writable mounts: %v", args)
					}
				} else if writable := strings.Contains(args[1], "(subpath"); writable != (mode == ModeWorkspace) {
					t.Fatalf("writable Seatbelt grant: %s", args[1])
				}
			})
		}
	}
}
