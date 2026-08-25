package process

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestRunnerRun_ValidatesRequestAndPaths(t *testing.T) {
	temporary := t.TempDir()
	runner := &Runner{goos: "linux"}
	valid := Request{Path: "/bin/sh", Cwd: temporary, TempDir: temporary, Mode: ModeHost, Timeout: time.Second}
	for _, mutate := range []func(*Request){
		func(request *Request) { request.Path = "" },
		func(request *Request) { request.Cwd = "" },
		func(request *Request) { request.TempDir = "" },
		func(request *Request) { request.Mode = "unknown" },
		func(request *Request) { request.Timeout = 0 },
		func(request *Request) { request.Timeout = 11 * time.Minute },
	} {
		request := valid
		mutate(&request)
		if _, err := runner.Run(context.Background(), request); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("Run(%+v) error = %v", request, err)
		}
	}

	previousAbs := processAbs
	t.Cleanup(func() { processAbs = previousAbs })
	absCalls := 0
	processAbs = func(value string) (string, error) {
		absCalls++
		if absCalls == 1 {
			return "", errors.New("root")
		}
		return filepath.Abs(value)
	}
	if _, err := runner.Run(context.Background(), valid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("root abs error = %v", err)
	}

	absCalls = 0
	processAbs = func(value string) (string, error) {
		absCalls++
		if absCalls == 2 {
			return "", errors.New("temp")
		}
		return filepath.Abs(value)
	}
	if _, err := runner.Run(context.Background(), valid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("temp abs error = %v", err)
	}

	processAbs = filepath.Abs
	valid.TempDir = filepath.Dir(temporary)
	if _, err := runner.Run(context.Background(), valid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("outside temp error = %v", err)
	}
}

func TestRunnerRun_HostSuccessExitStartAndTimeout(t *testing.T) {
	root := t.TempDir()
	runner := &Runner{goos: "linux"}
	if _, err := runner.Run(context.Background(), Request{Path: "/bin/true", Cwd: root, TempDir: root, Mode: ModeWorkspace, Timeout: time.Second}); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("workspace sandbox error = %v", err)
	}
	request := Request{
		Path: "/bin/sh", Args: []string{"-c", `read value; printf '%s|%s|%s' "$value" "$NANO_TEST" "$NANO_WORKSPACE"`},
		Stdin: []byte("input\n"), Cwd: root, TempDir: root, Mode: ModeHost, Timeout: time.Second,
		Additional: map[string]string{"NANO_TEST": "extra", "lower": "ignored", "NUL": "bad\x00value"},
	}
	result, err := runner.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 0 || result.Output != "input|extra|"+root {
		t.Fatalf("result = %+v", result)
	}

	request.Args = []string{"-c", "printf failure; exit 7"}
	result, err = runner.Run(context.Background(), request)
	if err == nil || err.Error() != "process exited with status 7" || result.ExitCode != 7 || result.Output != "failure" {
		t.Fatalf("exit result = %+v, error = %v", result, err)
	}

	request.Path = filepath.Join(root, "missing")
	request.Args = nil
	result, err = runner.Run(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "start process") || result.ExitCode != 0 {
		t.Fatalf("start result = %+v, error = %v", result, err)
	}

	request.Path = "/bin/sh"
	request.Args = []string{"-c", "printf before; sleep 2"}
	request.Timeout = 20 * time.Millisecond
	result, err = runner.Run(context.Background(), request)
	if !errors.Is(err, context.DeadlineExceeded) || result.ExitCode != -1 || result.Output != "before" {
		t.Fatalf("timeout result = %+v, error = %v", result, err)
	}
}

func TestRunnerCommand_BuildsSandboxInvocation(t *testing.T) {
	request := Request{Path: "/bin/tool", Args: []string{"one", "two"}, Mode: ModeHost}
	runner := &Runner{sandboxPath: "/sandbox", goos: "linux"}
	path, args, err := runner.command(`/work/"quoted`, "/work/tmp", request)
	if err != nil || path != request.Path || strings.Join(args, " ") != "one two" {
		t.Fatalf("host command = %q %#v, %v", path, args, err)
	}

	request.Mode = ModeWorkspace
	path, args, err = runner.command("/work", "/work/tmp", request)
	if err != nil || path != "/sandbox" || strings.Join(args[len(args)-3:], " ") != "/bin/tool one two" || args[0] != "--die-with-parent" {
		t.Fatalf("linux command = %q %#v, %v", path, args, err)
	}

	runner.goos = "darwin"
	path, args, err = runner.command(`/work/"quoted`, "/work/tmp", request)
	if err != nil || path != "/sandbox" || args[0] != "-p" || !strings.Contains(args[1], `subpath "/work/\"quoted"`) || args[2] != "/bin/tool" {
		t.Fatalf("darwin command = %q %#v, %v", path, args, err)
	}

	runner.sandboxPath = ""
	if _, _, err := runner.command("/work", "/work/tmp", request); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("missing sandbox error = %v", err)
	}
	runner.sandboxPath, runner.goos = "/sandbox", "plan9"
	if _, _, err := runner.command("/work", "/work/tmp", request); !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("unsupported sandbox error = %v", err)
	}
}

func TestHelpers_ValidateEnvironmentContainmentAndBoundOutput(t *testing.T) {
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

	var buffer limitedBuffer
	if count, err := buffer.Write([]byte("small")); count != 5 || err != nil || buffer.String() != "small" {
		t.Fatalf("small write = %d, %v, %q", count, err, buffer.String())
	}
	data := []byte(strings.Repeat("x", maxOutputBytes))
	if count, err := buffer.Write(data); count != len(data) || err != nil || !strings.HasSuffix(buffer.String(), "\n[output truncated]") {
		t.Fatalf("large write = %d, %v, suffix=%q", count, err, buffer.String()[len(buffer.String())-20:])
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
