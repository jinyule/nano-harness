package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

type acceptingApprover struct{}

func (acceptingApprover) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalAllowedOnce, nil
}

type runnerResult struct {
	result platformProcess.Result
	err    error
}

type fakeRunner struct {
	requests []platformProcess.Request
	results  []runnerResult
}

func (runner *fakeRunner) Run(_ context.Context, request platformProcess.Request) (platformProcess.Result, error) {
	runner.requests = append(runner.requests, request)
	if len(runner.results) == 0 {
		return platformProcess.Result{}, nil
	}
	result := runner.results[0]
	runner.results = runner.results[1:]
	return result.result, result.err
}

func restoreWorkspaceHooks(t *testing.T) {
	t.Helper()
	abs, eval, stat, look := workspaceAbs, workspaceEval, workspaceStat, workspaceLook
	temp, remove, relative := workspaceTemp, workspaceRemove, workspaceRel
	read, walk, open, lstat := workspaceRead, workspaceWalk, workspaceOpen, workspaceLstat
	t.Cleanup(func() {
		workspaceAbs, workspaceEval, workspaceStat, workspaceLook = abs, eval, stat, look
		workspaceTemp, workspaceRemove, workspaceRel = temp, remove, relative
		workspaceRead, workspaceWalk, workspaceOpen, workspaceLstat = read, walk, open, lstat
	})
}

func testRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func newToolRuntime(t *testing.T, start bool) (*appTool.Runtime, *plugin.Scope) {
	t.Helper()
	runtime, err := appTool.New(acceptingApprover{})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if start {
		if err := runtime.Start(context.Background(), scope); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = scope.Close(context.Background()) })
	}
	return runtime, scope
}

func TestNew_ValidatesAndResolvesWorkspace(t *testing.T) {
	restoreWorkspaceHooks(t)
	runtime, _ := newToolRuntime(t, false)
	runner := &fakeRunner{}
	root := testRoot(t)

	for _, test := range []struct {
		name    string
		runtime *appTool.Runtime
		runner  processRunner
		root    string
	}{
		{name: "nil runtime", runner: runner, root: root},
		{name: "nil runner", runtime: runtime, root: root},
		{name: "empty root", runtime: runtime, runner: runner, root: " \t"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.runtime, test.runner, test.root); !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("New() error = %v", err)
			}
		})
	}

	workspaceAbs = func(string) (string, error) { return "", errors.New("abs") }
	if _, err := New(runtime, runner, root); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "resolve root") {
		t.Fatalf("absolute error = %v", err)
	}
	workspaceAbs = filepath.Abs
	workspaceEval = func(string) (string, error) { return "", errors.New("eval") }
	if _, err := New(runtime, runner, root); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "root links") {
		t.Fatalf("symlink error = %v", err)
	}
	workspaceEval = filepath.EvalSymlinks
	workspaceStat = func(string) (os.FileInfo, error) { return nil, errors.New("stat") }
	if _, err := New(runtime, runner, root); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("stat error = %v", err)
	}
	workspaceStat = os.Stat
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(runtime, runner, file); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("file root error = %v", err)
	}

	workspaceLook = func(string) (string, error) { return "", errors.New("missing") }
	provider, err := New(runtime, runner, root)
	if err != nil || provider.root != root || provider.git != "" || provider.ID() != "workspace-tools" {
		t.Fatalf("provider = %+v, error = %v", provider, err)
	}
}

func TestProviderStart_RegistersAndCleansOwnedResources(t *testing.T) {
	restoreWorkspaceHooks(t)
	runtime, _ := newToolRuntime(t, true)
	root := testRoot(t)
	provider, err := New(runtime, &fakeRunner{}, root)
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := provider.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	temporary := provider.temp
	definitions, err := runtime.Definitions(nil)
	if err != nil || len(definitions) != 5 || !within(root, temporary) {
		t.Fatalf("definitions = %#v, temp = %q, error = %v", definitions, temporary, err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if provider.temp != "" {
		t.Fatalf("temp retained: %q", provider.temp)
	}
	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary still exists: %v", err)
	}
	definitions, _ = runtime.Definitions(nil)
	if len(definitions) != 0 {
		t.Fatalf("tools retained: %#v", definitions)
	}
}

func TestProviderStart_ContainsCreationRegistrationAndCleanupFailures(t *testing.T) {
	restoreWorkspaceHooks(t)
	runtime, _ := newToolRuntime(t, true)
	root := testRoot(t)
	provider := &Provider{runtime: runtime, runner: &fakeRunner{}, root: root}

	workspaceTemp = func(string, string) (string, error) { return "", errors.New("temp") }
	if err := provider.Start(context.Background(), &plugin.Scope{}); err == nil || !strings.Contains(err.Error(), "create workspace") {
		t.Fatalf("temp error = %v", err)
	}
	workspaceTemp = os.MkdirTemp //nolint:usetesting // restore the injected production function before exercising later branches

	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	removed := 0
	workspaceRemove = func(path string) error {
		removed++
		return os.RemoveAll(path)
	}
	if err := provider.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) || removed != 1 {
		t.Fatalf("closed scope error = %v, removed = %d", err, removed)
	}

	inactive, _ := newToolRuntime(t, false)
	inactiveProvider := &Provider{runtime: inactive, runner: &fakeRunner{}, root: root}
	inactiveScope := &plugin.Scope{}
	if err := inactiveProvider.Start(context.Background(), inactiveScope); !errors.Is(err, appTool.ErrNotRunning) {
		t.Fatalf("registration error = %v", err)
	}
	if err := inactiveScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	cleanupProvider := &Provider{runtime: runtime, runner: &fakeRunner{}, root: root}
	cleanupScope := &plugin.Scope{}
	workspaceRemove = func(string) error { return errors.New("remove") }
	if err := cleanupProvider.Start(context.Background(), cleanupScope); err != nil {
		t.Fatal(err)
	}
	cleanupTemporary := cleanupProvider.temp
	if err := cleanupScope.Close(context.Background()); err == nil || !strings.Contains(err.Error(), "remove") || cleanupProvider.temp != "" {
		t.Fatalf("cleanup error = %v, temp = %q", err, cleanupProvider.temp)
	}
	if err := os.RemoveAll(cleanupTemporary); err != nil {
		t.Fatal(err)
	}
}

func TestArgumentAndPathHelpers_RejectAmbiguityAndEscape(t *testing.T) {
	restoreWorkspaceHooks(t)
	var target struct {
		Value string `json:"value"`
	}
	if err := decodeArguments(json.RawMessage(`{"value":"ok"}`), &target); err != nil || target.Value != "ok" {
		t.Fatalf("decode = %+v, %v", target, err)
	}
	for _, raw := range []string{`{"unknown":true}`, `{"value":`, `{"value":"ok"} {}`} {
		if err := decodeArguments(json.RawMessage(raw), &target); err == nil {
			t.Fatalf("accepted %q", raw)
		}
	}

	root := testRoot(t)
	provider := &Provider{root: root}
	inside := filepath.Join(root, "inside")
	if err := os.WriteFile(inside, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	outsideRoot := testRoot(t)
	outside := filepath.Join(outsideRoot, "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if path, err := provider.existingPath("inside"); err != nil || path != inside {
		t.Fatalf("existing inside = %q, %v", path, err)
	}
	for _, name := range []string{"../outside", outside, "link"} {
		if _, err := provider.existingPath(name); !errors.Is(err, ErrOutsideRoot) {
			t.Fatalf("existingPath(%q) error = %v", name, err)
		}
	}
	if path, err := provider.lexicalPath(""); err != nil || path != root {
		t.Fatalf("empty lexical = %q, %v", path, err)
	}
	if _, err := provider.existingPath("missing"); err == nil {
		t.Fatal("missing path accepted")
	}
	workspaceEval = func(string) (string, error) { return "", errors.New("eval") }
	if _, err := provider.existingPath("inside"); err == nil || err.Error() != "eval" {
		t.Fatalf("eval error = %v", err)
	}

	workspaceRel = func(string, string) (string, error) { return "", errors.New("relative") }
	if within(root, inside) {
		t.Fatal("relative error reported containment")
	}
	if name := relativeName(root, inside); name != "inside" {
		t.Fatalf("relative fallback = %q", name)
	}
	workspaceRel = filepath.Rel
	if name := relativeName(root, filepath.Join(root, "nested", "file")); name != "nested/file" {
		t.Fatalf("relative name = %q", name)
	}
}

func TestDefinition_ConstructsSchema(t *testing.T) {
	value := definition("name", "description", `{"type":"object"}`)
	if value.Name != "name" || value.Description != "description" || string(value.Parameters) != `{"type":"object"}` {
		t.Fatalf("definition = %#v", value)
	}
}
