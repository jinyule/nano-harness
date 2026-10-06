package process

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNew_ResolvesPlatformSandbox(t *testing.T) {
	previousOS, previousLookPath := operatingSystem, findExecutable
	t.Cleanup(func() { operatingSystem, findExecutable = previousOS, previousLookPath })

	for _, test := range []struct {
		name, goos, executable string
		wantLookup             bool
	}{
		{name: "darwin", goos: "darwin", executable: "sandbox-exec", wantLookup: true},
		{name: "linux", goos: "linux", executable: "bwrap", wantLookup: true},
		{name: "unsupported", goos: "plan9"},
	} {
		t.Run(test.name, func(t *testing.T) {
			operatingSystem = test.goos
			lookedUp := ""
			findExecutable = func(name string) (string, error) {
				lookedUp = name
				return "/sandbox/" + name, nil
			}
			runner := New()
			if runner.goos != test.goos {
				t.Fatalf("goos = %q", runner.goos)
			}
			if test.wantLookup && lookedUp != test.executable {
				t.Fatalf("lookup = %q", lookedUp)
			}
			if !test.wantLookup && runner.sandboxPath != "" {
				t.Fatalf("sandboxPath = %q", runner.sandboxPath)
			}
		})
	}

	operatingSystem = "linux"
	findExecutable = func(string) (string, error) { return "", errors.New("missing") }
	if got := New().sandboxPath; got != "" {
		t.Fatalf("sandboxPath after lookup error = %q", got)
	}
}

func TestRunnerRun_RetainsCallerStderrBudgetIndependently(t *testing.T) {
	root := t.TempDir()
	for _, limit := range []int{0, 65536, 70000, 1} {
		request := Request{Path: "/bin/sh", Args: []string{"-c", "head -c 70000 /dev/zero | tr '\\000' o; head -c 70000 /dev/zero | tr '\\000' e >&2"}, Root: root, Cwd: root, Mode: ModeHost, StderrLimit: limit}
		result, err := New().Run(t.Context(), request)
		want := limit
		if want == 0 {
			want = 64000
		}
		if err != nil || result.ExitCode != 0 || result.Stdout.Text != strings.Repeat("o", 64000) || !result.Stdout.Truncated || result.Stderr.Text != strings.Repeat("e", want) || result.Stderr.Truncated != (want < 70000) {
			t.Fatalf("stderr limit=%d: stdout=%d stderr=%d truncated=%v err=%v", limit, len(result.Stdout.Text), len(result.Stderr.Text), result.Stderr.Truncated, err)
		}
	}
}

func TestRunnerRun_ValidatesRequestAndPaths(t *testing.T) {
	temporary := t.TempDir()
	runner := &Runner{goos: "linux"}
	valid := Request{Path: "/bin/sh", Root: temporary, Cwd: temporary, TempDir: temporary, Mode: ModeHost, Timeout: time.Second}
	for _, mutate := range []func(*Request){
		func(request *Request) { request.Path = "" },
		func(request *Request) { request.Root = "" },
		func(request *Request) { request.Cwd = "" },
		func(request *Request) { request.TempDir, request.Mode = "", ModeWorkspace },
		func(request *Request) { request.Mode = "unknown" },
		func(request *Request) { request.StdoutLimit = -1 },
		func(request *Request) { request.StderrLimit = -1 },
		func(request *Request) { request.Timeout = -time.Second },
		func(request *Request) { request.Timeout = 11 * time.Minute },
		func(request *Request) { request.TerminationGrace = -time.Second },
		func(request *Request) { request.TerminationGrace = 4 * time.Second },
		func(request *Request) { request.Cwd = filepath.Dir(temporary) },
		func(request *Request) { request.TempDir = filepath.Dir(temporary) },
	} {
		request := valid
		mutate(&request)
		if _, err := runner.Run(context.Background(), request); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("Run(%+v) error = %v", request, err)
		}
	}
	previousAbs := processAbs
	t.Cleanup(func() { processAbs = previousAbs })
	processAbs = func(string) (string, error) { return "", errors.New("abs") }
	if _, err := runner.Run(context.Background(), valid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("abs error = %v", err)
	}
}

func TestRunnerRun_ReportsStreamsExitSignalTimeoutAndCancellation(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{goos: "linux"}
	if _, err := runner.Run(context.Background(), Request{Path: "/bin/true", Root: root, Cwd: root, TempDir: root, Mode: ModeWorkspace, Timeout: time.Second}); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("workspace sandbox error = %v", err)
	}
	request := Request{
		Path: "/bin/sh", Args: []string{"-c", `printf '%s|%s|%s|' "$NANO_TEST" "$NANO_WORKSPACE" "$(pwd -P)"; printf oops >&2`},
		Root: root, Cwd: work, TempDir: root, Mode: ModeHost, Timeout: 5 * time.Second,
		Additional: map[string]string{"NANO_TEST": "extra", "lower": "ignored", "NUL": "bad\x00value"},
	}
	result, err := runner.Run(context.Background(), request)
	resolvedWork, _ := filepath.EvalSymlinks(work)
	if err != nil || result.ExitCode != 0 || result.Stdout.Text != "extra|"+root+"|"+resolvedWork+"|" || result.Stderr.Text != "oops" || result.Signal != "" || result.TimedOut {
		t.Fatalf("result = %+v, error = %v", result, err)
	}

	// Host mode may omit the temporary directory; TMPDIR is then unset.
	request.Args, request.TempDir = []string{"-c", `printf '%s' "${TMPDIR-unset}"`}, ""
	if result, err = runner.Run(context.Background(), request); err != nil || result.Stdout.Text != "unset" {
		t.Fatalf("no temporary directory = %+v, error = %v", result, err)
	}
	request.Args, request.StdoutLimit = []string{"-c", "printf 0123456789"}, 8
	if result, err = runner.Run(context.Background(), request); err != nil || result.Stdout.Text != "23456789" || !result.Stdout.Truncated {
		t.Fatalf("limited host result = %+v, error = %v", result, err)
	}
	request.TempDir, request.StdoutLimit = root, 0

	request.Args = []string{"-c", "printf failure; exit 7"}
	result, err = runner.Run(context.Background(), request)
	if err != nil || result.ExitCode != 7 || result.Stdout.Text != "failure" || result.SandboxDenied {
		t.Fatalf("exit result = %+v, error = %v", result, err)
	}

	request.Args = []string{"-c", "kill -TERM $$"}
	if result, err = runner.Run(context.Background(), request); err != nil || result.Signal != "SIGTERM" || result.ExitCode != -1 {
		t.Fatalf("signal result = %+v, error = %v", result, err)
	}
	request.Args = []string{"-c", "kill -VTALRM $$"}
	if result, err = runner.Run(context.Background(), request); err != nil || !strings.HasPrefix(result.Signal, "signal ") {
		t.Fatalf("unnamed signal result = %+v, error = %v", result, err)
	}

	request.Path = filepath.Join(root, "missing")
	request.Args = nil
	if result, err = runner.Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), "start process") || result.ExitCode != 0 {
		t.Fatalf("start result = %+v, error = %v", result, err)
	}

	request.Path = "/bin/sh"
	request.Args = []string{"-c", "printf before; sleep 30"}
	request.Timeout = 50 * time.Millisecond
	started := time.Now()
	result, err = runner.Run(context.Background(), request)
	if err != nil || !result.TimedOut || result.Signal != "SIGKILL" || result.Stdout.Text != "before" || time.Since(started) > 10*time.Second {
		t.Fatalf("timeout result = %+v, error = %v", result, err)
	}

	// A background descendant holding the pipes open is killed with the group.
	request.Args = []string{"-c", "sleep 30 & printf done"}
	request.Timeout = 5 * time.Second
	started = time.Now()
	if result, err = runner.Run(context.Background(), request); err != nil || result.Stdout.Text != "done" || time.Since(started) > 4*time.Second {
		t.Fatalf("background result = %+v, error = %v after %v", result, err, time.Since(started))
	}

	ctx, cancel := context.WithCancel(context.Background())
	marker := filepath.Join(root, "started")
	request.Args = []string{"-c", "printf partial; : > \"$MARKER\"; sleep 30"}
	request.Additional = map[string]string{"MARKER": marker}
	// Cancel only after the command proves it produced output.
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if result, err = runner.Run(ctx, request); !errors.Is(err, context.Canceled) || result.Stdout.Text != "partial" {
		t.Fatalf("canceled result = %+v, error = %v", result, err)
	}
	if _, err = runner.Run(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled error = %v", err)
	}
}

// lockedBuffer is a stream observer safe for the runner's copy goroutines.
type lockedBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (buffer *lockedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	buffer.data = append(buffer.data, data...)
	return len(data), nil
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(buffer.data)
}

func TestRunnerRun_ObservesStreamsWithoutDeadline(t *testing.T) {
	root := t.TempDir()
	runner := &Runner{goos: "linux"}
	var stdout, stderr lockedBuffer
	request := Request{
		Path: "/bin/sh", Args: []string{"-c", "printf out; printf err >&2; exit 4"},
		Root: root, Cwd: root, TempDir: root, Mode: ModeHost, Stdout: &stdout, Stderr: &stderr,
	}
	result, err := runner.Run(context.Background(), request)
	if err != nil || result.ExitCode != 4 || result.TimedOut || result.Stdout.Text != "out" || result.Stderr.Text != "err" || stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("result = %+v, observed %q/%q, error = %v", result, stdout.String(), stderr.String(), err)
	}

	// Without a deadline only the caller stops the process; the observer
	// sees output produced before cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	var live lockedBuffer
	request.Args, request.Stdout, request.Stderr = []string{"-c", "printf ready; sleep 30"}, &live, nil
	go func() {
		for live.String() == "" {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	started := time.Now()
	result, err = runner.Run(ctx, request)
	if !errors.Is(err, context.Canceled) || result.Signal != "SIGKILL" || result.TimedOut || live.String() != "ready" || time.Since(started) > 10*time.Second {
		t.Fatalf("canceled result = %+v, error = %v", result, err)
	}
}

func TestRunnerRun_ClassifiesSandboxDenial(t *testing.T) {
	root := t.TempDir()
	// A fake sandbox executable runs the command itself, so the denial
	// classifier sees real process output without a host sandbox.
	fake := filepath.Join(root, "sandbox")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf 'touch: x: Operation not permitted\\n' >&2\nexit 1\n"), 0o700); err != nil { //nolint:gosec // the fake runner must be executable
		t.Fatal(err)
	}
	runner := &Runner{goos: "darwin", sandboxPath: fake}
	request := Request{Path: "/bin/sh", Args: []string{"-c", "true"}, Root: root, Cwd: root, TempDir: root, Mode: ModeWorkspace, Timeout: 5 * time.Second}
	result, err := runner.Run(context.Background(), request)
	if err != nil || !result.SandboxDenied || result.ExitCode != 1 {
		t.Fatalf("denied = %+v, %v", result, err)
	}
	runner.goos = "linux"
	if result, _ := runner.Run(context.Background(), request); result.SandboxDenied {
		t.Fatal("linux signature matched darwin text")
	}
	request.Mode = ModeHost
	request.Path = fake
	runner.goos = "darwin"
	if result, _ := runner.Run(context.Background(), request); result.SandboxDenied {
		t.Fatal("host mode reported a sandbox denial")
	}
}

func TestRunnerCommand_BuildsSandboxInvocation(t *testing.T) {
	request := Request{Path: "/bin/tool", Args: []string{"one", "two"}, Mode: ModeHost}
	runner := &Runner{sandboxPath: "/sandbox", goos: "linux"}
	path, args, err := runner.command(`/work/"quoted`, "/work/sub", "/work/tmp", request)
	if err != nil || path != request.Path || strings.Join(args, " ") != "one two" {
		t.Fatalf("host command = %q %#v, %v", path, args, err)
	}

	request.Mode = ModeWorkspace
	path, args, err = runner.command("/work", "/work/sub", "/work/tmp", request)
	joined := strings.Join(args, " ")
	if err != nil || path != "/sandbox" || !strings.HasSuffix(joined, "--chdir /work/sub -- /bin/tool one two") || !strings.Contains(joined, "--bind /work /work --bind /work/tmp /tmp") || args[0] != "--die-with-parent" {
		t.Fatalf("linux command = %q %#v, %v", path, args, err)
	}

	runner.goos = "darwin"
	path, args, err = runner.command(`/work/"quoted`, "/work/sub", "/work/tmp", request)
	if err != nil || path != "/sandbox" || args[0] != "-p" || !strings.Contains(args[1], `subpath "/work/\"quoted"`) || args[2] != "/bin/tool" {
		t.Fatalf("darwin command = %q %#v, %v", path, args, err)
	}

	runner.sandboxPath = ""
	if _, _, err := runner.command("/work", "/work", "/work/tmp", request); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("missing sandbox error = %v", err)
	}
	runner.sandboxPath, runner.goos = "/sandbox", "plan9"
	if _, _, err := runner.command("/work", "/work", "/work/tmp", request); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("unsupported sandbox error = %v", err)
	}
}

func TestHelpers_ValidateEnvironmentContainmentAndTailOutput(t *testing.T) {
	for _, test := range []struct {
		name string
		want bool
	}{
		{name: "A", want: true}, {name: "_A9", want: true}, {name: "", want: false},
		{name: "lower", want: false}, {name: "9A", want: false}, {name: "A-b", want: false},
	} {
		if got := validEnvironmentName(test.name); got != test.want {
			t.Errorf("validEnvironmentName(%q) = %v", test.name, got)
		}
	}

	environment := cleanEnvironment("/work", "/work/tmp", map[string]string{"OK_1": "yes", "bad": "no", "NUL": "no\x00"})
	joined := strings.Join(environment, "\n")
	for _, expected := range []string{"PATH=", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "TMPDIR=/work/tmp", "NANO_WORKSPACE=/work", "OK_1=yes"} {
		if !strings.Contains(joined, expected) {
			t.Errorf("environment missing %q: %s", expected, joined)
		}
	}
	if strings.Contains(joined, "bad=") || strings.Contains(joined, "NUL=") {
		t.Fatalf("environment retained invalid values: %s", joined)
	}

	if !within("/work", "/work") || !within("/work", "/work/nested") || within("/work", "/other") {
		t.Fatal("within returned an unexpected containment result")
	}

	var small tailBuffer
	if count, err := small.Write([]byte("small")); count != 5 || err != nil || small.output() != (Output{Text: "small"}) {
		t.Fatalf("small write = %d, %v, %+v", count, err, small.output())
	}
	var large tailBuffer
	for range 5 {
		_, _ = large.Write([]byte(strings.Repeat("界", maxStreamBytes/3)))
	}
	_, _ = large.Write([]byte("END"))
	output := large.output()
	if !output.Truncated || len(output.Text) > maxStreamBytes || !strings.HasSuffix(output.Text, "END") || !strings.HasPrefix(output.Text, "界") {
		t.Fatalf("tail = %d bytes truncated=%v prefix=%q", len(output.Text), output.Truncated, output.Text[:6])
	}
	var exact tailBuffer
	_, _ = exact.Write([]byte(strings.Repeat("x", maxStreamBytes+1)))
	if output := exact.output(); !output.Truncated || len(output.Text) != maxStreamBytes {
		t.Fatalf("just over the limit = %d bytes truncated=%v", len(output.Text), output.Truncated)
	}
}

func TestProcessHelpers_ConfigureAndIgnoreMissingProcess(t *testing.T) {
	command := exec.CommandContext(t.Context(), "/bin/true")
	configureProcess(command)
	if command.SysProcAttr == nil || !command.SysProcAttr.Setpgid {
		t.Fatalf("SysProcAttr = %+v", command.SysProcAttr)
	}
	killProcessGroup(&exec.Cmd{})
}
