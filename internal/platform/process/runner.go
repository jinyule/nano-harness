// Package process runs bounded child processes with an explicit sandbox boundary.
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const maxOutputBytes = 256 << 10

var (
	// ErrInvalidConfig identifies a process request the runner cannot execute safely.
	ErrInvalidConfig = errors.New("invalid process configuration")
	// ErrSandboxUnavailable indicates workspace mode has no supported OS sandbox executable.
	ErrSandboxUnavailable = errors.New("workspace sandbox is unavailable")
	operatingSystem       = runtime.GOOS
	findExecutable        = exec.LookPath
	processAbs            = filepath.Abs
)

// Mode selects the enforced filesystem boundary.
type Mode string

const (
	// ModeWorkspace requires the configured OS filesystem sandbox.
	ModeWorkspace Mode = "workspace"
	// ModeHost runs directly on the host after an external approval decision.
	ModeHost Mode = "host"
)

// Request describes one direct executable invocation.
type Request struct {
	Path       string
	Args       []string
	Stdin      []byte
	Cwd        string
	TempDir    string
	Mode       Mode
	Timeout    time.Duration
	Additional map[string]string
}

// Result preserves bounded combined output and an exit status.
type Result struct {
	Output   string
	ExitCode int
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

// Run starts one process, drains it, and waits for complete termination.
func (runner *Runner) Run(ctx context.Context, request Request) (Result, error) {
	if request.Path == "" || request.Cwd == "" || request.TempDir == "" || request.Mode != ModeWorkspace && request.Mode != ModeHost || request.Timeout <= 0 || request.Timeout > 10*time.Minute {
		return Result{}, ErrInvalidConfig
	}
	root, err := processAbs(request.Cwd)
	if err != nil {
		return Result{}, ErrInvalidConfig
	}
	temporary, err := processAbs(request.TempDir)
	if err != nil || !within(root, temporary) {
		return Result{}, ErrInvalidConfig
	}
	path, args, err := runner.command(root, temporary, request)
	if err != nil {
		return Result{}, err
	}
	runContext, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	command := exec.CommandContext(runContext, path, args...) //nolint:gosec // executable and arguments are intentionally selected by the approved tool call
	command.Dir = root
	command.Stdin = bytes.NewReader(request.Stdin)
	command.Env = cleanEnvironment(root, temporary, request.Additional)
	configureProcess(command)
	var output limitedBuffer
	command.Stdout, command.Stderr = &output, &output
	err = command.Run()
	if runContext.Err() != nil {
		killProcessGroup(command)
		return Result{Output: output.String(), ExitCode: -1}, runContext.Err()
	}
	result := Result{Output: output.String()}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		return result, fmt.Errorf("process exited with status %d", result.ExitCode)
	}
	return result, fmt.Errorf("start process: %w", err)
}

func (runner *Runner) command(root, temporary string, request Request) (string, []string, error) {
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
			"--chdir", root, "--", request.Path,
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

type limitedBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	original := len(data)
	remaining := maxOutputBytes - buffer.buffer.Len()
	if remaining > 0 {
		_, _ = buffer.buffer.Write(data[:min(len(data), remaining)])
	}
	if original > remaining {
		buffer.truncated = true
	}
	return original, nil
}

func (buffer *limitedBuffer) String() string {
	value := buffer.buffer.String()
	if buffer.truncated {
		value += "\n[output truncated]"
	}
	return value
}

var _ io.Writer = (*limitedBuffer)(nil)
