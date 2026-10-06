package shell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appJob "github.com/jinyule/nano-harness/internal/app/job"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

type recordingApprover struct {
	outcome session.ApprovalOutcome
	reasons []string
}

func (approver *recordingApprover) Decide(_ context.Context, request appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	approver.reasons = append(approver.reasons, request.Reason)
	return approver.outcome, nil
}

type nopJournal struct{}

func (nopJournal) Append(context.Context, session.Record) (session.Event, error) {
	return session.Event{}, nil
}

type recordingNotifier struct {
	mu      sync.Mutex
	notices []string
	sent    chan struct{}
}

func (notifier *recordingNotifier) QueueNotice(_ context.Context, owner string, message session.Message) error {
	notifier.mu.Lock()
	notifier.notices = append(notifier.notices, owner+": "+session.Text(message))
	notifier.mu.Unlock()
	notifier.sent <- struct{}{}
	return nil
}

func (notifier *recordingNotifier) texts() []string {
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	return slices.Clone(notifier.notices)
}

// fakeRunner records requests, streams stdout to the request observer,
// and optionally blocks until released or cancelled like a killed process.
type fakeRunner struct {
	mu       sync.Mutex
	requests []platformProcess.Request
	result   platformProcess.Result
	err      error
	stdout   string
	stderr   string
	block    chan struct{}
	// wrote receives after stdout reached the observer, when set.
	wrote chan struct{}
}

func (runner *fakeRunner) Run(ctx context.Context, request platformProcess.Request) (platformProcess.Result, error) {
	runner.mu.Lock()
	runner.requests = append(runner.requests, request)
	result, err, stdout, stderr, block, wrote := runner.result, runner.err, runner.stdout, runner.stderr, runner.block, runner.wrote
	runner.mu.Unlock()
	if request.Stderr != nil && stderr != "" {
		_, _ = request.Stderr.Write([]byte(stderr))
	}
	if request.Stdout != nil && stdout != "" {
		_, _ = request.Stdout.Write([]byte(stdout))
		if wrote != nil {
			wrote <- struct{}{}
		}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return platformProcess.Result{Signal: "SIGKILL", ExitCode: -1}, ctx.Err()
		}
	}
	return result, err
}

func (runner *fakeRunner) last() platformProcess.Request {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return runner.requests[len(runner.requests)-1]
}

func (runner *fakeRunner) count() int {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	return len(runner.requests)
}

func restoreHooks(t *testing.T) {
	t.Helper()
	look, mkdir, remove, stat := lookPath, makeTemporary, removeTemporary, statPath
	t.Cleanup(func() { lookPath, makeTemporary, removeTemporary, statPath = look, mkdir, remove, stat })
}

type harness struct {
	root          workspace.Root
	runtime       *appTool.Runtime
	approver      *recordingApprover
	runner        Runner
	jobs          *appJob.Service
	jobScope      *plugin.Scope
	providerScope *plugin.Scope
	notifier      *recordingNotifier
	provider      *Provider
	delegate      bool
}

func newHarness(t *testing.T, runner Runner) *harness {
	t.Helper()
	return newHarnessWith(t, runner, nil)
}

// newHarnessWith also puts store in use for complete-output files.
func newHarnessWith(t *testing.T, runner Runner, store appTool.SpillStore) *harness {
	t.Helper()
	approver := &recordingApprover{outcome: session.ApprovalAllowedOnce}
	runtime, _ := appTool.New(approver)
	notifier := &recordingNotifier{sent: make(chan struct{}, 32)}
	jobs, _ := appJob.New(notifier)
	runtimeScope, jobScope, providerScope := &plugin.Scope{}, &plugin.Scope{}, &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	if store != nil {
		if err := runtime.UseSpill(store, runtimeScope); err != nil {
			t.Fatal(err)
		}
	}
	root, err := workspace.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider, err := New(runtime, runner, root, jobs)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background(), providerScope); err != nil {
		t.Fatal(err)
	}
	// The composition starts jobs after shell tools, so jobs stop first.
	if err := jobs.Start(context.Background(), jobScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = jobScope.Close(context.Background())
		_ = providerScope.Close(context.Background())
		_ = runtimeScope.Close(context.Background())
	})
	return &harness{root: root, runtime: runtime, approver: approver, runner: runner, jobs: jobs, jobScope: jobScope, providerScope: providerScope, notifier: notifier, provider: provider}
}

// settled waits for one of the session's jobs to finish.
func (h *harness) settled(t *testing.T, id string) appJob.View {
	t.Helper()
	view, err := h.jobs.Wait(context.Background(), "session-1", id, time.Minute)
	if err != nil || view.Status == appJob.StatusRunning || view.Status == appJob.StatusStopping {
		t.Fatalf("job %s = %+v, %v", id, view, err)
	}
	return view
}

func (h *harness) call(t *testing.T, arguments map[string]any) session.ToolResult {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	return h.runtime.ExecuteBatch(context.Background(), appTool.BatchRequest{
		SessionID: "session-1", Turn: 1, Step: 1, Journal: nopJournal{}, Delegated: h.delegate,
		Calls: []session.ToolCall{{ID: "call", Name: "bash", Arguments: encoded}},
	})[0]
}

func TestProvider_OwnsTemporaryDirectoryAndRegistration(t *testing.T) {
	restoreHooks(t)
	runtime, _ := appTool.New(&recordingApprover{})
	root, _ := workspace.Resolve(t.TempDir())
	runner := &fakeRunner{}
	jobs, _ := appJob.New(&recordingNotifier{})
	for _, test := range []struct {
		runtime *appTool.Runtime
		runner  Runner
		root    workspace.Root
		jobs    *appJob.Service
	}{
		{runner: runner, root: root, jobs: jobs}, {runtime: runtime, root: root, jobs: jobs},
		{runtime: runtime, runner: runner, jobs: jobs}, {runtime: runtime, runner: runner, root: root},
	} {
		if _, err := New(test.runtime, test.runner, test.root, test.jobs); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(%+v) = %v", test, err)
		}
	}
	lookPath = func(string) (string, error) { return "", errors.New("missing") }
	provider, err := New(runtime, runner, root, jobs)
	if err != nil || provider.ID() != "shell-tools" || provider.bashPath != "" {
		t.Fatalf("provider = %+v, %v", provider, err)
	}
	runtimeScope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeScope.Close(context.Background()) })
	scope := &plugin.Scope{}
	if err := provider.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	temporary := provider.temporary()
	if filepath.Dir(temporary) != root.Path() || !strings.HasPrefix(filepath.Base(temporary), ".nano-harness-tmp-") {
		t.Fatalf("temporary = %q", temporary)
	}
	catalog, _ := runtime.Catalog(nil)
	if len(catalog.Definitions) != 1 || catalog.Definitions[0].Name != "bash" || strings.Join(catalog.Guidance, "") != "Check the [exit code: N] marker on every bash result; investigate failures before moving on." {
		t.Fatalf("catalog = %#v", catalog)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) || provider.temporary() != "" {
		t.Fatalf("temporary survived cleanup: %v", err)
	}
	if catalog, _ := runtime.Catalog(nil); len(catalog.Definitions) != 0 {
		t.Fatal("bash survived cleanup")
	}

	failure := errors.New("mkdir")
	makeTemporary = func(string, string) (string, error) { return "", failure }
	if err := provider.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, failure) {
		t.Fatalf("temporary failure = %v", err)
	}
	makeTemporary = os.MkdirTemp //nolint:usetesting // restore the production function before later branches
	removed := ""
	removeTemporary = func(path string) error {
		removed = path
		return os.RemoveAll(path)
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := provider.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) || removed == "" {
		t.Fatalf("closed scope = %v, removed %q", err, removed)
	}
	inactive, _ := appTool.New(&recordingApprover{})
	stopped, _ := New(inactive, runner, root, jobs)
	stoppedScope := &plugin.Scope{}
	if err := stopped.Start(context.Background(), stoppedScope); !errors.Is(err, appTool.ErrNotRunning) {
		t.Fatalf("registration failure = %v", err)
	}
	if err := stoppedScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	removeTemporary = func(string) error { return failure }
	failing, _ := New(runtime, runner, root, jobs)
	failingScope := &plugin.Scope{}
	if err := failing.Start(context.Background(), failingScope); err != nil {
		t.Fatal(err)
	}
	leftover := failing.temporary()
	if err := failingScope.Close(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("cleanup failure = %v", err)
	}
	if err := os.RemoveAll(leftover); err != nil {
		t.Fatal(err)
	}
}

func TestBash_ValidatesBeforeApproval(t *testing.T) {
	h := newHarness(t, &fakeRunner{})
	if err := os.WriteFile(filepath.Join(h.root.Path(), "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{"command": "ls"}, `missing required property "description"`},
		{map[string]any{"description": "List", "command": "ls", "run_in_background": "yes"}, `"run_in_background" must be a boolean`},
		{map[string]any{"description": "List", "command": "ls", "timeout_ms": 5}, `"timeout_ms" is not a declared property`},
		{map[string]any{"description": "List", "command": " "}, "invalid command: expected a non-empty string"},
		{map[string]any{"description": "\n", "command": "ls"}, "invalid description: expected a non-empty string"},
		{map[string]any{"description": "List", "command": "ls", "timeoutMs": 0}, "invalid timeoutMs: expected a positive number, got 0"},
		{map[string]any{"description": "List", "command": "ls", "timeoutMs": -2.5}, "got -2.5"},
		{map[string]any{"description": "List", "command": "ls", "sandbox_permissions": "danger-full-access"}, "requires a justification"},
		{map[string]any{"description": "List", "command": "ls", "sandbox_permissions": "danger-full-access", "justification": " "}, "non-empty sentence"},
		{map[string]any{"description": "List", "command": "ls", "justification": "why"}, "only valid together"},
		{map[string]any{"description": "List", "command": "ls", "workdir": "missing"}, `invalid workdir "missing": not found`},
		{map[string]any{"description": "List", "command": "ls", "workdir": "file"}, "not a directory"},
		{map[string]any{"description": "List", "command": "ls", "workdir": "../"}, "path is outside the workspace"},
	} {
		result := h.call(t, test.arguments)
		if !result.IsError || !strings.Contains(result.Output, test.want) {
			t.Errorf("bash(%v) = %#v, want %q", test.arguments, result, test.want)
		}
	}
	if len(h.approver.reasons) != 0 {
		t.Fatalf("invalid calls reached approval: %q", h.approver.reasons)
	}
}

func TestBash_MapsArgumentsToSandboxedRequests(t *testing.T) {
	runner := &fakeRunner{result: platformProcess.Result{Stdout: platformProcess.Output{Text: "ok\n"}}}
	h := newHarness(t, runner)
	if err := os.Mkdir(filepath.Join(h.root.Path(), "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		arguments map[string]any
		mode      platformProcess.Mode
		cwd       string
		reason    string
	}{
		{map[string]any{"description": "List files", "command": "ls"}, platformProcess.ModeWorkspace, h.root.Path(), "run a shell command in the workspace sandbox: List files"},
		{map[string]any{"description": "Sub", "command": "pwd", "workdir": "sub", "timeoutMs": 1500.5, "justification": "  ", "run_in_background": false}, platformProcess.ModeWorkspace, filepath.Join(h.root.Path(), "sub"), "run a shell command in the workspace sandbox: Sub"},
		{map[string]any{"description": "Repeat", "command": "ls", "sandbox_permissions": "workspace-write", "workdir": filepath.Join(h.root.Path(), "sub")}, platformProcess.ModeWorkspace, filepath.Join(h.root.Path(), "sub"), "run a shell command in the workspace sandbox: Repeat"},
		{map[string]any{"description": "Host", "command": "id", "sandbox_permissions": "danger-full-access", "justification": "needs host keychain"}, platformProcess.ModeHost, h.root.Path(), "escalate sandbox to danger-full-access: needs host keychain"},
	} {
		result := h.call(t, test.arguments)
		request := runner.last()
		if result.IsError || result.Output != "ok\n" {
			t.Fatalf("bash(%v) = %#v", test.arguments, result)
		}
		// Job-backed commands have no runner deadline; the call's wait
		// applies the timeout.
		if request.Mode != test.mode || request.Cwd != test.cwd || request.Timeout != 0 || request.Root != h.root.Path() || request.TempDir != h.provider.temporary() || request.Args[0] != "-c" || request.Path != h.provider.bashPath || request.Stdout == nil || request.Stderr == nil {
			t.Fatalf("request = %+v", request)
		}
		if h.approver.reasons[len(h.approver.reasons)-1] != test.reason {
			t.Fatalf("reason = %q", h.approver.reasons[len(h.approver.reasons)-1])
		}
		for name, value := range map[string]string{"DSH_SHELL": "1", "DSH_SESSION_ID": "session-1", "NO_COLOR": "1", "TERM": "dumb", "PAGER": "cat", "GIT_PAGER": "cat"} {
			if request.Additional[name] != value {
				t.Fatalf("environment %s = %q", name, request.Additional[name])
			}
		}
	}
	// A foreground call that finished in time leaves no job behind.
	if views := h.jobs.List("session-1"); len(views) != 0 {
		t.Fatalf("foreground jobs survived: %+v", views)
	}
	h.delegate = true
	if result := h.call(t, map[string]any{"description": "Host", "command": "id", "sandbox_permissions": "danger-full-access", "justification": "x"}); !result.IsError || !strings.Contains(result.Output, "subagents cannot request sandbox escalation") {
		t.Fatalf("delegated escalation = %#v", result)
	}
	h.approver.outcome = session.ApprovalRejected
	before := runner.count()
	if result := h.call(t, map[string]any{"description": "List", "command": "ls", "run_in_background": true}); !result.IsError || result.Output != "Error: approval rejected" || runner.count() != before {
		t.Fatalf("rejected = %#v", result)
	}
	if len(h.jobs.List("session-1")) != 0 {
		t.Fatal("rejected background call started a job")
	}
}

func TestBash_RunsInBackgroundAndNotifiesCompletion(t *testing.T) {
	runner := &fakeRunner{stdout: "progress\n", block: make(chan struct{}), result: platformProcess.Result{Stdout: platformProcess.Output{Text: "progress\n"}, ExitCode: 2}}
	h := newHarness(t, runner)
	result := h.call(t, map[string]any{"description": "Build", "command": "make build", "run_in_background": true, "timeoutMs": 1})
	if result.IsError || result.Output != "started background job bash-1" {
		t.Fatalf("background = %#v", result)
	}
	if reason := h.approver.reasons[0]; reason != "run a background shell command in the workspace sandbox: Build" {
		t.Fatalf("reason = %q", reason)
	}
	// No timeout applies: the job is still running long after timeoutMs.
	if view, err := h.jobs.Wait(context.Background(), "session-1", "bash-1", 20*time.Millisecond); err != nil || view.Status != appJob.StatusRunning || view.Label != "make build" || view.Kind != "bash" {
		t.Fatalf("view = %+v, %v", view, err)
	}
	if request := runner.last(); request.Timeout != 0 || request.Mode != platformProcess.ModeWorkspace {
		t.Fatalf("request = %+v", request)
	}
	close(runner.block)
	<-h.notifier.sent
	if texts := h.notifier.texts(); len(texts) != 1 || texts[0] != "session-1: background job bash-1 (bash: make build) finished [status: completed, exit code: 2]. Read its output with job_output." {
		t.Fatalf("notices = %q", texts)
	}
	if read, _ := h.jobs.Read("session-1", "bash-1"); read.Stdout != "progress\n" {
		t.Fatalf("read = %+v", read)
	}
}

func TestBash_PromotesForegroundCommandAfterTimeout(t *testing.T) {
	runner := &fakeRunner{stdout: "partial", block: make(chan struct{}), wrote: make(chan struct{}, 1)}
	h := newHarness(t, runner)
	result := h.call(t, map[string]any{"description": "Serve", "command": "serve", "timeoutMs": 5})
	handoff := "[still running after 5ms; moved to background job bash-1]\n" +
		"The command keeps running in the background. You will be notified when it finishes; read newer output with job_output, stop it with job_kill."
	if result.IsError || !strings.HasSuffix(result.Output, handoff) {
		t.Fatalf("promoted = %#v", result)
	}
	// The hand-off and later reads deliver every byte exactly once, however
	// the write raced the timeout.
	<-runner.wrote
	read, _ := h.jobs.Read("session-1", "bash-1")
	if handed := strings.TrimSuffix(strings.TrimSuffix(result.Output, handoff), "\n"); handed+read.Stdout != "partial" || read.Job.Status != appJob.StatusRunning {
		t.Fatalf("handed %q then read %+v", handed, read)
	}
	if _, requested, err := h.jobs.Kill("session-1", "bash-1", new("done")); !requested || err != nil {
		t.Fatalf("kill = %v, %v", requested, err)
	}
	if view := h.settled(t, "bash-1"); view.StatusLine() != "[status: killed, signal: SIGKILL; done]" {
		t.Fatalf("killed = %q", view.StatusLine())
	}
	if got := promoted("", "bash-2", 60000); !strings.HasPrefix(got, "[still running after 60000ms; moved to background job bash-2]\n") {
		t.Fatalf("empty promotion = %q", got)
	}
	if got := promoted("line\n", "bash-3", 1); !strings.HasPrefix(got, "line\n[still running") {
		t.Fatalf("terminated promotion = %q", got)
	}
}

func TestBash_AbortKillsForegroundJob(t *testing.T) {
	runner := &fakeRunner{block: make(chan struct{})}
	h := newHarness(t, runner)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.provider.bash(ctx, appTool.Invocation{SessionID: "session-1", Approved: true}, bashArgs{Description: "d", Command: "sleep"})
		done <- err
	}()
	for runner.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil || err.Error() != "tool call aborted" {
		t.Fatalf("aborted = %v", err)
	}
	if views := h.jobs.List("session-1"); len(views) != 0 {
		t.Fatalf("aborted job survived: %+v", views)
	}
	if texts := h.notifier.texts(); len(texts) != 0 {
		t.Fatalf("abort notified: %q", texts)
	}

	// Shutdown kills a waiting foreground command the same way.
	go func() {
		_, err := h.provider.bash(context.Background(), appTool.Invocation{SessionID: "session-1", Approved: true}, bashArgs{Description: "d", Command: "sleep"})
		done <- err
	}()
	for runner.count() < 2 {
		time.Sleep(time.Millisecond)
	}
	if err := h.jobScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || err.Error() != "tool call aborted" {
		t.Fatalf("shutdown = %v", err)
	}
}

// TestBash_FallsBackToDeadlineAtJobLimit proves admission refusal runs a
// foreground command under the timeout kill, and the timeout mapping.
func TestBash_FallsBackToDeadlineAtJobLimit(t *testing.T) {
	runner := &fakeRunner{block: make(chan struct{})}
	h := newHarness(t, runner)
	for range 10 {
		if result := h.call(t, map[string]any{"description": "Hold", "command": "sleep", "run_in_background": true}); result.IsError {
			t.Fatalf("background = %#v", result)
		}
	}
	if result := h.call(t, map[string]any{"description": "Hold", "command": "sleep", "run_in_background": true}); !result.IsError || !strings.Contains(result.Output, "background job limit reached for this owner (limit: 10)") {
		t.Fatalf("over limit = %#v", result)
	}
	for runner.count() < 10 {
		time.Sleep(time.Millisecond)
	}
	runner.mu.Lock()
	runner.block, runner.result = nil, platformProcess.Result{TimedOut: true, Signal: "SIGKILL", ExitCode: -1}
	runner.mu.Unlock()
	for _, test := range []struct {
		timeout any
		want    time.Duration
	}{
		{nil, time.Minute}, {1500.5, 1500500 * time.Microsecond}, {9e9, 10 * time.Minute}, {1e-9, time.Nanosecond},
	} {
		arguments := map[string]any{"description": "Fallback", "command": "ls"}
		if test.timeout != nil {
			arguments["timeoutMs"] = test.timeout
		}
		result := h.call(t, arguments)
		if request := runner.last(); request.Timeout != test.want || request.Stdout == nil || result.IsError || !strings.Contains(result.Output, "[timed out after ") {
			t.Fatalf("fallback(%v) = %#v, request %+v", test.timeout, result, request)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.provider.bash(ctx, appTool.Invocation{SessionID: "session-1", Approved: true}, bashArgs{Description: "d", Command: "c"}); err == nil || err.Error() != "tool call aborted" {
		t.Fatalf("canceled fallback = %v", err)
	}
}

func TestBash_ExecutionPointGuardsAndFailures(t *testing.T) {
	restoreHooks(t)
	runner := &fakeRunner{}
	h := newHarness(t, runner)
	approved := appTool.Invocation{SessionID: "session-1", Approved: true}
	valid := bashArgs{Description: "d", Command: "c"}
	background := true
	if _, err := h.provider.bash(context.Background(), appTool.Invocation{}, valid); err == nil || !strings.Contains(err.Error(), "approval was not granted") {
		t.Fatalf("unapproved = %v", err)
	}
	failure := errors.New("sandbox missing")
	runner.err = failure
	if _, err := h.provider.bash(context.Background(), approved, valid); !errors.Is(err, failure) {
		t.Fatalf("runner error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.provider.bash(ctx, approved, valid); err == nil || err.Error() != "tool call aborted" {
		t.Fatalf("canceled = %v", err)
	}
	if _, err := h.provider.bash(ctx, approved, bashArgs{Description: "d", Command: "c", RunInBackground: &background}); err == nil || err.Error() != "tool call aborted" {
		t.Fatalf("canceled background = %v", err)
	}
	workdir := "gone"
	if _, err := h.provider.bash(context.Background(), approved, bashArgs{Description: "d", Command: "c", Workdir: &workdir}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("execution-point workdir = %v", err)
	}
	statPath = func(string) (os.FileInfo, error) { return nil, failure }
	dot := "."
	if _, err := h.provider.bash(context.Background(), approved, bashArgs{Description: "d", Command: "c", Workdir: &dot}); !errors.Is(err, failure) {
		t.Fatalf("workdir stat = %v", err)
	}
	statPath = os.Stat
	h.provider.bashPath = ""
	if _, err := h.provider.bash(context.Background(), approved, valid); err == nil || !strings.Contains(err.Error(), "bash executable is unavailable") {
		t.Fatalf("missing bash = %v", err)
	}
	h.provider.bashPath = "/bin/bash"
	if err := h.jobScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, arguments := range []bashArgs{valid, {Description: "d", Command: "c", RunInBackground: &background}} {
		if _, err := h.provider.bash(context.Background(), approved, arguments); !errors.Is(err, appJob.ErrNotRunning) {
			t.Fatalf("stopped jobs = %v", err)
		}
	}
	h.provider.mu.Lock()
	h.provider.temp = ""
	h.provider.mu.Unlock()
	if _, err := h.provider.bash(context.Background(), approved, valid); err == nil || !strings.Contains(err.Error(), "shell tools are not running") {
		t.Fatalf("stopped = %v", err)
	}
}

func TestOutcome_MapsProcessFactsToJobStatus(t *testing.T) {
	denied := "exit code: 1; [sandbox: file access denied under workspace-write mode] [sandbox: escalation available — retry this exact command once with sandbox_permissions (the narrowest wider mode that suffices) + justification; the approval prompt asks the user]"
	for _, test := range []struct {
		result platformProcess.Result
		err    error
		want   appJob.Outcome
	}{
		{platformProcess.Result{}, errors.New("start process: missing"), appJob.Outcome{Status: appJob.StatusFailed, Detail: "start process: missing"}},
		{platformProcess.Result{Signal: "SIGKILL", ExitCode: -1}, context.Canceled, appJob.Outcome{Status: appJob.StatusKilled, Detail: "signal: SIGKILL"}},
		{platformProcess.Result{}, fmt.Errorf("start process: %w", context.Canceled), appJob.Outcome{Status: appJob.StatusKilled, Detail: "killed before exit"}},
		{platformProcess.Result{Signal: "SIGTERM", ExitCode: -1}, nil, appJob.Outcome{Status: appJob.StatusKilled, Detail: "signal: SIGTERM"}},
		{platformProcess.Result{}, nil, appJob.Outcome{Status: appJob.StatusCompleted, Detail: "exit code: 0"}},
		{platformProcess.Result{ExitCode: 1, SandboxDenied: true}, nil, appJob.Outcome{Status: appJob.StatusCompleted, Detail: denied}},
	} {
		if got := outcome(test.result, test.err); got != test.want {
			t.Errorf("outcome(%+v, %v) = %+v, want %+v", test.result, test.err, got, test.want)
		}
	}
}

func TestRender_MatchesUpstreamMarkers(t *testing.T) {
	output := func(text string, truncated bool) platformProcess.Output {
		return platformProcess.Output{Text: text, Truncated: truncated}
	}
	for _, test := range []struct {
		result platformProcess.Result
		want   string
	}{
		{platformProcess.Result{}, "(no output)"},
		{platformProcess.Result{Stdout: output("out\n", false)}, "out\n"},
		{platformProcess.Result{Stdout: output("out", false), Stderr: output("err", false)}, "out\n[stderr]\nerr"},
		{platformProcess.Result{Stderr: output("err\n", false), ExitCode: 2}, "[stderr]\nerr\n[exit code: 2]"},
		{platformProcess.Result{Stdout: output("tail", true)}, "tail\n[output truncated; full output: (unavailable)]"},
		{platformProcess.Result{TimedOut: true, Signal: "SIGKILL", ExitCode: -1}, "(no output)\n[timed out after 1500.5ms]\n[killed by signal: SIGKILL]"},
		{platformProcess.Result{Stderr: output("touch: x: Operation not permitted\n", false), ExitCode: 1, SandboxDenied: true},
			"[stderr]\ntouch: x: Operation not permitted\n[sandbox: file access denied under workspace-write mode]\n[sandbox: escalation available — retry this exact command once with sandbox_permissions (the narrowest wider mode that suffices) + justification; the approval prompt asks the user]\n[exit code: 1]"},
	} {
		if got := render(test.result, [2]string{}, 1500.5); got != test.want {
			t.Errorf("render(%+v)\n got: %q\nwant: %q", test.result, got, test.want)
		}
	}
}

// TestBash_RunsRealHostProcess proves the bash -c invocation, environment,
// workdir, and exit reporting through the real runner. Escalation selects
// host mode so the test does not depend on a host sandbox executable.
func TestBash_RunsRealHostProcess(t *testing.T) {
	h := newHarness(t, platformProcess.New())
	if h.provider.bashPath == "" {
		t.Skip("bash is not installed")
	}
	if err := os.Mkdir(filepath.Join(h.root.Path(), "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	result := h.call(t, map[string]any{
		"description": "Print facts", "workdir": "sub",
		"command":             `printf '%s|%s|%s|%s\n' "$BASH_VERSION" "$DSH_SHELL" "$DSH_SESSION_ID" "$(basename "$PWD")"; echo warn >&2; exit 3`,
		"sandbox_permissions": "danger-full-access", "justification": "test host execution",
	})
	if result.IsError || !strings.Contains(result.Output, "|1|session-1|sub\n[stderr]\nwarn\n[exit code: 3]") || strings.HasPrefix(result.Output, "|") {
		t.Fatalf("result = %#v", result)
	}
}

// TestBash_WorkspaceSandboxAllowsInsideAndDeniesOutside runs the real OS
// sandbox where one is installed and checks the model-facing denial marker.
func TestBash_WorkspaceSandboxAllowsInsideAndDeniesOutside(t *testing.T) {
	h := newHarness(t, platformProcess.New())
	outside := t.TempDir()
	result := h.call(t, map[string]any{"description": "Write inside", "command": "printf inside > inside.txt"})
	if result.IsError && strings.Contains(result.Output, "workspace sandbox is unavailable") || h.provider.bashPath == "" {
		t.Skip("no OS sandbox or bash on this host")
	}
	if result.IsError || result.Output != "(no output)" {
		t.Fatalf("inside write = %#v", result)
	}
	if data, err := os.ReadFile(filepath.Join(h.root.Path(), "inside.txt")); err != nil || string(data) != "inside" {
		t.Fatalf("inside file = %q, %v", data, err)
	}
	result = h.call(t, map[string]any{"description": "Write outside", "command": "printf x > '" + filepath.Join(outside, "escape.txt") + "'"})
	if result.IsError || !strings.Contains(result.Output, "[sandbox: file access denied under workspace-write mode]\n[sandbox: escalation available") || !strings.HasSuffix(result.Output, "[exit code: 1]") {
		t.Fatalf("outside write = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("sandboxed command wrote outside the workspace")
	}
}

// TestBash_RealBackgroundAndPromotedProcesses runs real host processes as
// jobs: a background command streams both channels, and a promoted command
// keeps running until job_kill terminates its whole process group.
func TestBash_RealBackgroundAndPromotedProcesses(t *testing.T) {
	h := newHarness(t, platformProcess.New())
	if h.provider.bashPath == "" {
		t.Skip("bash is not installed")
	}
	host := map[string]any{"sandbox_permissions": "danger-full-access", "justification": "test host jobs"}
	with := func(arguments map[string]any) map[string]any {
		maps.Copy(arguments, host)
		return arguments
	}
	result := h.call(t, with(map[string]any{"description": "Stream", "command": "printf start; sleep 0.2; printf end >&2", "run_in_background": true}))
	if result.Output != "started background job bash-1" {
		t.Fatalf("background = %#v", result)
	}
	if view := h.settled(t, "bash-1"); view.StatusLine() != "[status: completed, exit code: 0]" {
		t.Fatalf("background view = %q", view.StatusLine())
	}
	if read, _ := h.jobs.Read("session-1", "bash-1"); read.Stdout != "start" || read.Stderr != "end" {
		t.Fatalf("background read = %+v", read)
	}

	pidFile := filepath.Join(h.root.Path(), "child.pid")
	result = h.call(t, with(map[string]any{"description": "Hold", "command": "sleep 30 & echo $! > child.pid; printf before; wait", "timeoutMs": 300}))
	handoff := "[still running after 300ms; moved to background job bash-2]\n"
	if result.IsError || !strings.Contains(result.Output, handoff) {
		t.Fatalf("promoted = %#v", result)
	}
	started := time.Now()
	if _, requested, err := h.jobs.Kill("session-1", "bash-2", new("test done")); !requested || err != nil {
		t.Fatalf("kill = %v, %v", requested, err)
	}
	if view := h.settled(t, "bash-2"); view.StatusLine() != "[status: killed, signal: SIGTERM; test done]" || time.Since(started) > 10*time.Second {
		t.Fatalf("killed view = %q after %v", view.StatusLine(), time.Since(started))
	}
	read, _ := h.jobs.Read("session-1", "bash-2")
	before, _, _ := strings.Cut(result.Output, handoff)
	if handed := strings.TrimSuffix(before, "\n"); handed+read.Stdout != "before" {
		t.Fatalf("handed %q then read %q", handed, read.Stdout)
	}
	pid, err := os.ReadFile(pidFile) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	// The group kill reached the backgrounded child; allow init to reap it.
	for deadline := time.Now().Add(5 * time.Second); exec.CommandContext(t.Context(), "kill", "-0", strings.TrimSpace(string(pid))).Run() == nil; { //nolint:gosec // probes the PID this test's own command recorded
		if time.Now().After(deadline) {
			t.Fatalf("child %s survived the job kill", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
