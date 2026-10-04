package shell

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
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

type fakeRunner struct {
	requests []platformProcess.Request
	result   platformProcess.Result
	err      error
}

func (runner *fakeRunner) Run(_ context.Context, request platformProcess.Request) (platformProcess.Result, error) {
	runner.requests = append(runner.requests, request)
	return runner.result, runner.err
}

func restoreHooks(t *testing.T) {
	t.Helper()
	look, mkdir, remove, stat := lookPath, makeTemporary, removeTemporary, statPath
	t.Cleanup(func() { lookPath, makeTemporary, removeTemporary, statPath = look, mkdir, remove, stat })
}

type harness struct {
	root     workspace.Root
	runtime  *appTool.Runtime
	approver *recordingApprover
	runner   Runner
	provider *Provider
	delegate bool
}

func newHarness(t *testing.T, runner Runner) *harness {
	t.Helper()
	approver := &recordingApprover{outcome: session.ApprovalAllowedOnce}
	runtime, _ := appTool.New(approver)
	runtimeScope, providerScope := &plugin.Scope{}, &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider, err := New(runtime, runner, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background(), providerScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = providerScope.Close(context.Background())
		_ = runtimeScope.Close(context.Background())
	})
	return &harness{root: root, runtime: runtime, approver: approver, runner: runner, provider: provider}
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
	for _, test := range []struct {
		runtime *appTool.Runtime
		runner  Runner
		root    workspace.Root
	}{{runner: runner, root: root}, {runtime: runtime, root: root}, {runtime: runtime, runner: runner}} {
		if _, err := New(test.runtime, test.runner, test.root); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(%+v) = %v", test, err)
		}
	}
	lookPath = func(string) (string, error) { return "", errors.New("missing") }
	provider, err := New(runtime, runner, root)
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
	stopped, _ := New(inactive, runner, root)
	stoppedScope := &plugin.Scope{}
	if err := stopped.Start(context.Background(), stoppedScope); !errors.Is(err, appTool.ErrNotRunning) {
		t.Fatalf("registration failure = %v", err)
	}
	if err := stoppedScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	removeTemporary = func(string) error { return failure }
	failing, _ := New(runtime, runner, root)
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
		{map[string]any{"description": "List", "command": "ls", "run_in_background": true}, `"run_in_background" is not a declared property`},
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
		timeout   time.Duration
		reason    string
	}{
		{map[string]any{"description": "List files", "command": "ls"}, platformProcess.ModeWorkspace, h.root.Path(), time.Minute, "run a shell command in the workspace sandbox: List files"},
		{map[string]any{"description": "Sub", "command": "pwd", "workdir": "sub", "timeoutMs": 1500.5, "justification": "  "}, platformProcess.ModeWorkspace, filepath.Join(h.root.Path(), "sub"), 1500500 * time.Microsecond, "run a shell command in the workspace sandbox: Sub"},
		{map[string]any{"description": "Repeat", "command": "ls", "sandbox_permissions": "workspace-write", "timeoutMs": 9e9, "workdir": filepath.Join(h.root.Path(), "sub")}, platformProcess.ModeWorkspace, filepath.Join(h.root.Path(), "sub"), 10 * time.Minute, "run a shell command in the workspace sandbox: Repeat"},
		{map[string]any{"description": "Host", "command": "id", "sandbox_permissions": "danger-full-access", "justification": "needs host keychain", "timeoutMs": 1e-9}, platformProcess.ModeHost, h.root.Path(), time.Nanosecond, "escalate sandbox to danger-full-access: needs host keychain"},
	} {
		result := h.call(t, test.arguments)
		request := runner.requests[len(runner.requests)-1]
		if result.IsError || result.Output != "ok\n" {
			t.Fatalf("bash(%v) = %#v", test.arguments, result)
		}
		if request.Mode != test.mode || request.Cwd != test.cwd || request.Timeout != test.timeout || request.Root != h.root.Path() || request.TempDir != h.provider.temporary() || request.Args[0] != "-c" || request.Path != h.provider.bashPath {
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
	h.delegate = true
	if result := h.call(t, map[string]any{"description": "Host", "command": "id", "sandbox_permissions": "danger-full-access", "justification": "x"}); !result.IsError || !strings.Contains(result.Output, "subagents cannot request sandbox escalation") {
		t.Fatalf("delegated escalation = %#v", result)
	}
	h.approver.outcome = session.ApprovalRejected
	before := len(runner.requests)
	if result := h.call(t, map[string]any{"description": "List", "command": "ls"}); !result.IsError || result.Output != "tool error: approval rejected" || len(runner.requests) != before {
		t.Fatalf("rejected = %#v", result)
	}
}

func TestBash_ExecutionPointGuardsAndFailures(t *testing.T) {
	restoreHooks(t)
	runner := &fakeRunner{}
	h := newHarness(t, runner)
	approved := appTool.Invocation{Approved: true}
	valid := bashArgs{Description: "d", Command: "c"}
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
	workdir := "gone"
	if _, err := h.provider.bash(context.Background(), approved, bashArgs{Description: "d", Command: "c", Workdir: &workdir}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("execution-point workdir = %v", err)
	}
	statPath = func(string) (os.FileInfo, error) { return nil, failure }
	dot := "."
	if _, err := h.provider.bash(context.Background(), approved, bashArgs{Description: "d", Command: "c", Workdir: &dot}); !errors.Is(err, failure) {
		t.Fatalf("workdir stat = %v", err)
	}
	h.provider.bashPath = ""
	if _, err := h.provider.bash(context.Background(), approved, valid); err == nil || !strings.Contains(err.Error(), "bash executable is unavailable") {
		t.Fatalf("missing bash = %v", err)
	}
	h.provider.bashPath = "/bin/bash"
	h.provider.mu.Lock()
	h.provider.temp = ""
	h.provider.mu.Unlock()
	if _, err := h.provider.bash(context.Background(), approved, valid); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("stopped = %v", err)
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
		if got := render(test.result, 1500.5); got != test.want {
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
