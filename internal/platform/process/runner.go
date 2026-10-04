// Package process runs bounded child processes with an explicit sandbox boundary.
package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	// maxStreamBytes is the retained tail of each output stream.
	maxStreamBytes = 64_000
	// pipeDrainDelay bounds waiting for descendants that keep pipes open
	// after the process exits or is killed.
	pipeDrainDelay = time.Second
)

var (
	// ErrInvalidConfig identifies a process request the runner cannot execute safely.
	ErrInvalidConfig = errors.New("invalid process configuration")
	// ErrSandboxUnavailable indicates workspace mode has no supported OS sandbox executable.
	ErrSandboxUnavailable = errors.New("workspace sandbox is unavailable")
	operatingSystem       = runtime.GOOS
	findExecutable        = exec.LookPath
	processAbs            = filepath.Abs
)

// denialSignatures are the case-insensitive stderr fragments each sandbox
// backend produces when it refuses a file effect.
var denialSignatures = map[string]string{
	"darwin": "operation not permitted",
	"linux":  "read-only file system",
}

// Mode selects the enforced filesystem boundary.
type Mode string

const (
	// ModeWorkspace requires the configured OS filesystem sandbox.
	ModeWorkspace Mode = "workspace"
	// ModeHost runs directly on the host after an external approval decision.
	ModeHost Mode = "host"
)

// Request describes one direct executable invocation without stdin.
type Request struct {
	Path string
	Args []string
	// Root is the only directory tree workspace mode may write.
	Root string
	// Cwd is the working directory and must lie inside Root.
	Cwd string
	// TempDir is the private TMPDIR and must lie inside Root.
	TempDir    string
	Mode       Mode
	Timeout    time.Duration
	Additional map[string]string
}

// Output is the retained tail of one stream.
type Output struct {
	Text      string
	Truncated bool
}

// Result describes a process that started and was waited for.
type Result struct {
	Stdout   Output
	Stderr   Output
	ExitCode int
	// Signal names the terminating signal, or is empty after a normal exit.
	Signal string
	// TimedOut reports that the request timeout killed the process group.
	TimedOut bool
	// SandboxDenied reports a failed workspace-mode run whose stderr carries
	// the active sandbox's file-denial signature.
	SandboxDenied bool
}

// Runner resolves sandbox support once and owns no process beyond Run.
type Runner struct {
	sandboxPath string
	goos        string
}

// New resolves the operating-system sandbox executable.
func New() *Runner {
	path := ""
	switch operatingSystem {
	case "darwin":
		path, _ = findExecutable("sandbox-exec")
	case "linux":
		path, _ = findExecutable("bwrap")
	}
	return &Runner{sandboxPath: path, goos: operatingSystem}
}

// Run starts one process, drains both streams, and waits until the process
// group is killed and reaped. Exit status, signals, and timeouts are facts
// in Result; errors mean the process could not run or the caller canceled.
func (runner *Runner) Run(ctx context.Context, request Request) (Result, error) {
	if request.Path == "" || request.Root == "" || request.Cwd == "" || request.TempDir == "" || request.Mode != ModeWorkspace && request.Mode != ModeHost || request.Timeout <= 0 || request.Timeout > 10*time.Minute {
		return Result{}, ErrInvalidConfig
	}
	paths := make([]string, 3)
	for index, value := range []string{request.Root, request.Cwd, request.TempDir} {
		absolute, err := processAbs(value)
		if err != nil {
			return Result{}, ErrInvalidConfig
		}
		paths[index] = absolute
	}
	root, cwd, temporary := paths[0], paths[1], paths[2]
	if !within(root, cwd) || !within(root, temporary) {
		return Result{}, ErrInvalidConfig
	}
	path, args, err := runner.command(root, cwd, temporary, request)
	if err != nil {
		return Result{}, err
	}
	runContext, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	command := exec.CommandContext(runContext, path, args...) //nolint:gosec // executable and arguments are intentionally selected by the approved tool call
	command.Dir = cwd
	command.Env = cleanEnvironment(root, temporary, request.Additional)
	configureProcess(command)
	var killed atomic.Bool
	command.Cancel = func() error {
		killed.Store(true)
		killProcessGroup(command)
		return nil
	}
	command.WaitDelay = pipeDrainDelay
	var stdout, stderr tailBuffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	// Descendants left in the group are stopped so the call reaches quiescence.
	killProcessGroup(command)
	if command.ProcessState == nil {
		return Result{}, fmt.Errorf("start process: %w", err)
	}
	result := Result{
		Stdout: stdout.output(), Stderr: stderr.output(),
		ExitCode: command.ProcessState.ExitCode(), Signal: exitSignal(command.ProcessState),
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	result.TimedOut = killed.Load()
	signature, ok := denialSignatures[runner.goos]
	result.SandboxDenied = request.Mode == ModeWorkspace && ok && result.ExitCode > 0 && strings.Contains(strings.ToLower(result.Stderr.Text), signature)
	return result, nil
}

func (runner *Runner) command(root, cwd, temporary string, request Request) (string, []string, error) {
	if request.Mode == ModeHost {
		return request.Path, request.Args, nil
	}
	if runner.sandboxPath == "" {
		return "", nil, ErrSandboxUnavailable
	}
	switch runner.goos {
	case "darwin":
		profile := `(version 1)(allow default)(deny file-write*)(allow file-write* (subpath "` + escapeSandbox(root) + `") (literal "/dev/null"))`
		return runner.sandboxPath, append([]string{"-p", profile, request.Path}, request.Args...), nil
	case "linux":
		arguments := []string{
			"--die-with-parent", "--unshare-all", "--ro-bind", "/", "/",
			"--bind", root, root, "--bind", temporary, "/tmp", "--dev", "/dev", "--proc", "/proc",
			"--chdir", cwd, "--", request.Path,
		}
		return runner.sandboxPath, append(arguments, request.Args...), nil
	default:
		return "", nil, ErrSandboxUnavailable
	}
}

func escapeSandbox(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}

func cleanEnvironment(root, temporary string, additional map[string]string) []string {
	values := map[string]string{
		"PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
		"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "TMPDIR": temporary, "NANO_WORKSPACE": root,
	}
	for name, value := range additional {
		if validEnvironmentName(name) && !strings.ContainsRune(value, '\x00') {
			values[name] = value
		}
	}
	environment := make([]string, 0, len(values))
	for name, value := range values {
		environment = append(environment, name+"="+value)
	}
	return environment
}

func validEnvironmentName(name string) bool {
	if name == "" || name[0] != '_' && (name[0] < 'A' || name[0] > 'Z') {
		return false
	}
	for _, char := range name[1:] {
		if char != '_' && (char < 'A' || char > 'Z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}

func within(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// tailBuffer keeps the last maxStreamBytes written to it.
type tailBuffer struct {
	data      []byte
	truncated bool
}

func (buffer *tailBuffer) Write(data []byte) (int, error) {
	buffer.data = append(buffer.data, data...)
	if len(buffer.data) > 2*maxStreamBytes {
		buffer.data = append(buffer.data[:0], buffer.data[len(buffer.data)-maxStreamBytes:]...)
		buffer.truncated = true
	}
	return len(data), nil
}

// output trims the tail to the limit at a rune boundary.
func (buffer *tailBuffer) output() Output {
	data, truncated := buffer.data, buffer.truncated
	if len(data) > maxStreamBytes {
		data, truncated = data[len(data)-maxStreamBytes:], true
	}
	if truncated {
		for len(data) > 0 && !utf8.RuneStart(data[0]) {
			data = data[1:]
		}
	}
	return Output{Text: string(data), Truncated: truncated}
}

var _ io.Writer = (*tailBuffer)(nil)
