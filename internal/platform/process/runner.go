// Package process runs bounded child processes with an explicit sandbox boundary.
package process

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	// maxStreamBytes is the default retained tail of each output stream.
	maxStreamBytes = 64_000
	// zeroGraceDrain bounds waiting for descendants that keep pipes open
	// after the process exits or is killed when the request has no
	// termination grace, as for read-only search. A request with a grace
	// drains for that grace, like upstream's spawn.
	zeroGraceDrain = time.Second
)

var (
	// ErrInvalidConfig identifies a process request the runner cannot execute safely.
	ErrInvalidConfig = errors.New("invalid process configuration")
	// ErrSandboxUnavailable identifies a missing or failed workspace sandbox
	// runner. Its text is upstream's SandboxUnavailableError for the
	// workspace-write mode; a runner failure appends " Runner failure: <detail>".
	ErrSandboxUnavailable = errors.New(`sandbox mode "workspace-write" is requested but no sandbox backend is usable on this host; ` + //nolint:staticcheck // ST1005: upstream's model-facing message ends with a period and is reproduced byte for byte
		"refusing to run the command unconfined. Install bubblewrap or run a Landlock-enforcing kernel (Linux), " +
		"ensure sandbox-exec is usable (macOS), or ensure the ACL restricted-token runner can start (Windows) " +
		"— otherwise switch the consumer to danger-full-access.")
	operatingSystem = runtime.GOOS
	findExecutable  = exec.LookPath
	processAbs      = filepath.Abs
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
	// ModeHost runs directly on the host. Callers choose it only for an
	// approved command or a fixed, read-only helper invocation.
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
	// TempDir is the private TMPDIR and must lie inside Root. Workspace mode
	// requires it; an empty value in host mode leaves TMPDIR unset.
	TempDir string
	Mode    Mode
	// Timeout terminates the process group when it expires; zero leaves ctx as
	// the only bound, for work a background job owner cancels explicitly.
	Timeout    time.Duration
	Additional map[string]string
	// TerminationGrace permits SIGTERM cleanup before SIGKILL on cancellation
	// or timeout. It is bounded to three seconds; zero kills immediately,
	// as required by read-only search.
	TerminationGrace time.Duration
	// StdoutLimit is the retained stdout tail in bytes; zero selects the
	// default. Output.Truncated reports that more was written.
	StdoutLimit int
	// StderrLimit is the retained stderr tail in bytes; zero selects the
	// default independently of StdoutLimit.
	StderrLimit int
	// Stdout and Stderr, when set, observe each stream as it is produced, in
	// addition to the retained tails. Each is written from one goroutine,
	// concurrently with the other, and must not fail.
	Stdout io.Writer
	Stderr io.Writer
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
	// TimedOut reports that the request timeout requested group termination.
	TimedOut bool
	// SandboxDenied reports a failed workspace-mode run whose stderr carries
	// the active sandbox's file-denial signature.
	SandboxDenied bool
	// RunnerFailed distinguishes sandbox infrastructure failure from a command exit.
	RunnerFailed bool
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
	if request.Path == "" || request.Root == "" || request.Cwd == "" || request.TempDir == "" && request.Mode != ModeHost || request.Mode != ModeWorkspace && request.Mode != ModeHost || request.Timeout < 0 || request.Timeout > 10*time.Minute || request.TerminationGrace < 0 || request.TerminationGrace > 3*time.Second || request.StdoutLimit < 0 || request.StderrLimit < 0 {
		return Result{}, ErrInvalidConfig
	}
	paths := make([]string, 3)
	for index, value := range []string{request.Root, request.Cwd, request.TempDir} {
		if value == "" {
			continue
		}
		absolute, err := processAbs(value)
		if err != nil {
			return Result{}, ErrInvalidConfig
		}
		paths[index] = absolute
	}
	root, cwd, temporary := paths[0], paths[1], paths[2]
	if !within(root, cwd) || temporary != "" && !within(root, temporary) {
		return Result{}, ErrInvalidConfig
	}
	path, args, err := runner.command(root, cwd, temporary, request)
	if err != nil {
		return Result{}, err
	}
	runContext, cancel := ctx, context.CancelFunc(func() {})
	if request.Timeout > 0 {
		runContext, cancel = context.WithTimeout(ctx, request.Timeout)
	}
	defer cancel()
	if err := runContext.Err(); err != nil {
		return Result{}, err
	}
	command := exec.Command(path, args...) //nolint:gosec,noctx // approved argv; the joined observer below owns group cancellation so exec's pipe deadline cannot shorten TERM grace
	command.Dir = cwd
	command.Env = cleanEnvironment(root, temporary, request.Additional)
	configureProcess(command)
	var killed atomic.Bool
	command.WaitDelay = request.TerminationGrace
	if command.WaitDelay == 0 {
		command.WaitDelay = zeroGraceDrain
	}
	stdout, stderr := tailBuffer{limit: request.StdoutLimit}, tailBuffer{limit: request.StderrLimit}
	command.Stdout, command.Stderr = observed(&stdout, request.Stdout), observed(&stderr, request.Stderr)
	if err = command.Start(); err != nil {
		if request.Mode == ModeWorkspace && runnerSpawnFailure(err, path, cwd) {
			return Result{RunnerFailed: true}, fmt.Errorf("%w Runner failure: %w", ErrSandboxUnavailable, err)
		}
		return Result{}, fmt.Errorf("start process: %w", err)
	}
	// The cancellation observer belongs to this invocation and is joined
	// before returning, so no delayed signal outlives the invocation.
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		select {
		case <-done:
			return
		case <-runContext.Done():
			killed.Store(true)
		}
		if request.TerminationGrace > 0 {
			terminateProcessGroup(command)
			timer := time.NewTimer(request.TerminationGrace)
			defer timer.Stop()
			select {
			case <-done:
				return
			case <-timer.C:
			}
		}
		killProcessGroup(command)
	}()
	_ = command.Wait() // nonzero exit and pipe drain expiry are represented by the process facts
	close(done)
	<-stopped
	// Descendants left in the group are stopped so the call reaches quiescence.
	killProcessGroup(command)
	result := Result{
		Stdout: stdout.output(), Stderr: stderr.output(),
		ExitCode: command.ProcessState.ExitCode(), Signal: exitSignal(command.ProcessState),
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	result.TimedOut = killed.Load()
	if request.Mode == ModeWorkspace && result.ExitCode > 0 {
		prefix := map[string]string{"darwin": "sandbox-exec: ", "linux": "bwrap: "}[runner.goos]
		for line := range strings.SplitSeq(result.Stderr.Text, "\n") {
			// Like upstream's /\r?\n/ split, the matched line is otherwise unchanged.
			if line = strings.TrimSuffix(line, "\r"); prefix != "" && strings.Contains(strings.ToLower(line), prefix) {
				result.RunnerFailed = true
				return result, fmt.Errorf("%w Runner failure: %s", ErrSandboxUnavailable, line)
			}
		}
	}
	signature, ok := denialSignatures[runner.goos]
	result.SandboxDenied = request.Mode == ModeWorkspace && ok && result.ExitCode > 0 && strings.Contains(strings.ToLower(result.Stderr.Text), signature)
	return result, nil
}

// runnerSpawnFailure rules out cwd failure before attributing an executable
// launch error to confinement. Go's fork/exec error names argv[0] even when
// the child's chdir fails, so the path alone does not establish the stage.
func runnerSpawnFailure(err error, path, cwd string) bool {
	var failure *os.PathError
	if !errors.As(err, &failure) || failure.Path != path || !errors.Is(err, os.ErrNotExist) && !errors.Is(err, os.ErrPermission) {
		return false
	}
	info, statErr := os.Stat(cwd)
	return statErr == nil && info.IsDir() && canEnter(cwd)
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
		"LANG": "C.UTF-8", "LC_ALL": "C.UTF-8", "NANO_WORKSPACE": root,
	}
	if temporary != "" {
		values["TMPDIR"] = temporary
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

// observed tees a stream to its optional observer after the retained tail.
func observed(tail *tailBuffer, observer io.Writer) io.Writer {
	if observer == nil {
		return tail
	}
	return io.MultiWriter(tail, observer)
}

// tailBuffer keeps the last limit bytes written to it; zero selects
// maxStreamBytes.
type tailBuffer struct {
	limit     int
	data      []byte
	truncated bool
}

func (buffer *tailBuffer) size() int {
	if buffer.limit == 0 {
		return maxStreamBytes
	}
	return buffer.limit
}

func (buffer *tailBuffer) Write(data []byte) (int, error) {
	buffer.data = append(buffer.data, data...)
	if limit := buffer.size(); len(buffer.data) > 2*limit {
		buffer.data = append(buffer.data[:0], buffer.data[len(buffer.data)-limit:]...)
		buffer.truncated = true
	}
	return len(data), nil
}

// output trims the tail to the limit at a rune boundary.
func (buffer *tailBuffer) output() Output {
	data, truncated := buffer.data, buffer.truncated
	if limit := buffer.size(); len(data) > limit {
		data, truncated = data[len(data)-limit:], true
	}
	if truncated {
		for len(data) > 0 && !utf8.RuneStart(data[0]) {
			data = data[1:]
		}
	}
	return Output{Text: string(data), Truncated: truncated}
}

var _ io.Writer = (*tailBuffer)(nil)
