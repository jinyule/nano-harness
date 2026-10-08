package skill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/agent"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type allowAll struct{}

func (allowAll) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalAllowedOnce, nil
}

// contextRegistry mirrors the engine's scope-bound registration contract.
type contextRegistry struct {
	err       error
	providers []agent.ContextProvider
}

func (registry *contextRegistry) RegisterContext(provider agent.ContextProvider, scope *plugin.Scope) error {
	if registry.err != nil {
		return registry.err
	}
	registry.providers = append(registry.providers, provider)
	return scope.Defer(func(context.Context) error {
		registry.providers = slices.DeleteFunc(registry.providers, func(candidate agent.ContextProvider) bool { return candidate == provider })
		return nil
	})
}

// fixture is one started provider over private project and user roots. The
// workspace holds a .git directory, so it is its own project root.
type fixture struct {
	workspace string
	user      string
	agents    string
	runtime   *appTool.Runtime
	contexts  *contextRegistry
	provider  *Provider
	scope     *plugin.Scope
}

func privateDir(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := privateDir(t)
	f := &fixture{
		workspace: filepath.Join(base, "work"),
		user:      filepath.Join(base, "config", "skills"),
		agents:    filepath.Join(base, "home", ".agents", "skills"),
		contexts:  &contextRegistry{},
	}
	if err := os.MkdirAll(filepath.Join(f.workspace, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	runtime, err := appTool.New(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	runtimeScope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	f.runtime = runtime
	f.provider, err = New(runtime, f.contexts, Config{Workspace: f.workspace, UserDir: f.user, AgentsDir: f.agents})
	if err != nil {
		t.Fatal(err)
	}
	f.scope = &plugin.Scope{}
	if err := f.provider.Start(context.Background(), f.scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.scope.Close(context.Background())
		_ = runtimeScope.Close(context.Background())
	})
	return f
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// skillText renders a valid instruction file with optional extra
// frontmatter lines.
func skillText(name, description string, extra ...string) string {
	frontmatter := append([]string{"name: " + name, "description: " + description}, extra...)
	return "---\n" + strings.Join(frontmatter, "\n") + "\n---\n\nBody of " + name + ".\n"
}

func (f *fixture) projectRoot() string { return filepath.Join(f.workspace, ".agents", "skills") }

// call executes one skill tool call through the real runtime.
func (f *fixture) call(t *testing.T, arguments string) session.ToolResult {
	t.Helper()
	results := f.runtime.ExecuteBatch(context.Background(), appTool.BatchRequest{
		SessionID: "session", Cwd: f.workspace, Turn: 1, Step: 1,
		Calls: []session.ToolCall{{ID: "call", Name: toolName, Arguments: json.RawMessage(arguments)}},
	})
	return results[0]
}

// names lists the discovered winners in order.
func (f *fixture) names(t *testing.T) []string {
	t.Helper()
	skills, err := f.provider.discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(skills))
	for index, skill := range skills {
		names[index] = skill.name
	}
	return names
}

// restoreHooks resets every filesystem seam after a test replaces one.
func restoreHooks(t *testing.T) {
	t.Helper()
	stat, lstat, open, read, fileStat := statPath, lstatPath, openPath, readEntries, statFile
	t.Cleanup(func() { statPath, lstatPath, openPath, readEntries, statFile = stat, lstat, open, read, fileStat })
}
