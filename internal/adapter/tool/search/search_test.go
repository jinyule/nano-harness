package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

type denyApprover struct{}

func (denyApprover) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalRejected, nil
}

type nopJournal struct{}

func (nopJournal) Append(context.Context, session.Record) (session.Event, error) {
	return session.Event{}, nil
}

// scriptedRunner answers the version probe like a supported ripgrep and
// replays a fixed result for every search.
type scriptedRunner struct {
	version  platformProcess.Result
	result   platformProcess.Result
	err      error
	requests []platformProcess.Request
}

func (runner *scriptedRunner) Run(_ context.Context, request platformProcess.Request) (platformProcess.Result, error) {
	runner.requests = append(runner.requests, request)
	if len(request.Args) == 1 && request.Args[0] == "--version" {
		return runner.version, nil
	}
	return runner.result, runner.err
}

func supported() platformProcess.Result {
	return platformProcess.Result{Stdout: platformProcess.Output{Text: "ripgrep 15.2.0 (rev e89fff89ac)\n\nfeatures:+pcre2\n"}}
}

func restoreHooks(t *testing.T) {
	t.Helper()
	look, lstat := lookPath, lstatPath
	t.Cleanup(func() { lookPath, lstatPath = look, lstat })
}

type harness struct {
	root     string
	runtime  *appTool.Runtime
	provider *Provider
}

// newHarness starts the provider over the given runner. The workspace sits
// below a private parent so ripgrep's repository detection sees only
// directories this test controls.
func newHarness(t *testing.T, runner Runner) *harness {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(base, "workspace")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := workspace.Resolve(directory)
	if err != nil {
		t.Fatal(err)
	}
	runtime, _ := appTool.New(denyApprover{})
	runtimeScope, providerScope := &plugin.Scope{}, &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	provider, err := New(runtime, runner, root)
	if err != nil {
		t.Fatalf("ripgrep is a required test dependency: %v", err)
	}
	if err := provider.Start(context.Background(), providerScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = providerScope.Close(context.Background())
		_ = runtimeScope.Close(context.Background())
	})
	return &harness{root: directory, runtime: runtime, provider: provider}
}

func (h *harness) write(t *testing.T, name, content string) {
	t.Helper()
	path := filepath.Join(h.root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) touch(t *testing.T, name string, offset time.Duration) {
	t.Helper()
	moment := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(offset)
	if err := os.Chtimes(filepath.Join(h.root, filepath.FromSlash(name)), moment, moment); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) call(t *testing.T, name string, arguments map[string]any) session.ToolResult {
	t.Helper()
	encoded, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	return h.runtime.ExecuteBatch(context.Background(), appTool.BatchRequest{
		SessionID: "session", Turn: 1, Step: 1, Journal: nopJournal{},
		Calls: []session.ToolCall{{ID: "call", Name: name, Arguments: encoded}},
	})[0]
}

// grouped sorts grep file groups, because ripgrep's parallel traversal does
// not order files and upstream keeps that output order.
func grouped(output string) string {
	header, body, found := strings.Cut(output, "\n\n")
	if !found {
		return output
	}
	groups := strings.Split(body, "\n\n")
	slices.Sort(groups)
	return header + "\n\n" + strings.Join(groups, "\n\n")
}

func TestProvider_ResolvesVerifiesAndRegisters(t *testing.T) {
	restoreHooks(t)
	runtime, _ := appTool.New(denyApprover{})
	root, _ := workspace.Resolve(t.TempDir())
	runner := &scriptedRunner{version: supported()}
	for _, test := range []struct {
		runtime *appTool.Runtime
		runner  Runner
		root    workspace.Root
	}{{runner: runner, root: root}, {runtime: runtime, root: root}, {runtime: runtime, runner: runner}} {
		if _, err := New(test.runtime, test.runner, test.root); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("New(%+v) = %v", test, err)
		}
	}
	lookPath = func(string) (string, error) { return "", errors.New("not on PATH") }
	if _, err := New(runtime, runner, root); !errors.Is(err, ErrRipgrepUnavailable) || !strings.Contains(err.Error(), "install ripgrep 15.0.0 or newer") {
		t.Fatalf("missing rg = %v", err)
	}
	lookPath = func(string) (string, error) { return "/tools/rg", nil }
	provider, err := New(runtime, runner, root)
	if err != nil || provider.ID() != "search-tools" || provider.ripgrep != "/tools/rg" || provider.timeout != searchTimeout || provider.rawLimit != rawOutputMaxBytes {
		t.Fatalf("provider = %+v, %v", provider, err)
	}
	if err := provider.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, appTool.ErrNotRunning) {
		t.Fatalf("inactive runtime = %v", err)
	}
	probe := runner.requests[0]
	if probe.Path != "/tools/rg" || probe.Mode != platformProcess.ModeHost || probe.Cwd != root.Path() || probe.Timeout != versionTimeout || probe.TempDir != "" {
		t.Fatalf("version probe = %+v", probe)
	}
	runtimeScope, scope := &plugin.Scope{}, &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeScope.Close(context.Background()) })
	if err := provider.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	catalog, _ := runtime.Catalog(nil)
	if len(catalog.Definitions) != 2 || catalog.Definitions[0].Name != "glob" || catalog.Definitions[1].Name != "grep" {
		t.Fatalf("definitions = %#v", catalog.Definitions)
	}
	if strings.Join(catalog.Guidance, "|") != "Use the glob tool — not shell find — to discover files by path pattern.|Use the grep tool — not shell grep or rg — to search file contents." {
		t.Fatalf("guidance = %q", catalog.Guidance)
	}
	read := appTool.Define(appTool.Spec[struct{}]{Name: "read", Description: "read", Execute: func(context.Context, appTool.Invocation, struct{}) (appTool.Result, error) {
		return appTool.Result{}, nil
	}})
	if err := runtime.Register(read, scope); err != nil {
		t.Fatal(err)
	}
	if catalog, _ := runtime.Catalog([]string{"grep", "read"}); len(catalog.Guidance) != 1 || !strings.HasSuffix(catalog.Guidance[0], " Use read on a matched file when you need surrounding context.") {
		t.Fatalf("grep guidance with read = %q", catalog.Guidance)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if catalog, _ := runtime.Catalog(nil); len(catalog.Definitions) != 0 {
		t.Fatal("tools retained after cleanup")
	}

	blocker := appTool.Define(appTool.Spec[struct{}]{Name: "grep", Description: "occupies the name", Execute: func(context.Context, appTool.Invocation, struct{}) (appTool.Result, error) {
		return appTool.Result{}, nil
	}})
	blockerScope, partial := &plugin.Scope{}, &plugin.Scope{}
	if err := runtime.Register(blocker, blockerScope); err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background(), partial); err == nil || !strings.Contains(err.Error(), `duplicate "grep"`) {
		t.Fatalf("partial start = %v", err)
	}
	if err := partial.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if catalog, _ := runtime.Catalog([]string{"glob"}); len(catalog.Definitions) != 0 {
		t.Fatal("partial start leaked glob")
	}
	_ = blockerScope.Close(context.Background())
}

func TestProvider_StartRejectsUnsupportedRipgrep(t *testing.T) {
	restoreHooks(t)
	lookPath = func(string) (string, error) { return "/tools/rg", nil }
	runtime, _ := appTool.New(denyApprover{})
	runtimeScope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeScope.Close(context.Background()) })
	root, _ := workspace.Resolve(t.TempDir())
	output := func(text string) platformProcess.Result {
		return platformProcess.Result{Stdout: platformProcess.Output{Text: text}}
	}
	for _, test := range []struct {
		name    string
		version platformProcess.Result
		want    string
	}{
		{"old major", output("ripgrep 14.1.1\n"), "/tools/rg is ripgrep 14.1.1; 15.0.0 or newer is required"},
		{"not ripgrep", output("grep (GNU grep) 3.11\n"), "did not report a ripgrep version"},
		{"short version", output("ripgrep 15.2\n"), "did not report a ripgrep version"},
		{"bad number", output("ripgrep 15.x.0\n"), "did not report a ripgrep version"},
		{"failed", platformProcess.Result{Stdout: platformProcess.Output{Text: "ripgrep 15.2.0"}, ExitCode: 2}, "did not report"},
		{"signaled", platformProcess.Result{Signal: "SIGKILL", ExitCode: -1}, "did not report"},
		{"timed out", platformProcess.Result{TimedOut: true}, "did not report"},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider, _ := New(runtime, &scriptedRunner{version: test.version}, root)
			scope := &plugin.Scope{}
			if err := provider.Start(context.Background(), scope); !errors.Is(err, ErrRipgrepUnavailable) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Start = %v", err)
			}
			if catalog, _ := runtime.Catalog(nil); len(catalog.Definitions) != 0 {
				t.Fatal("unsupported ripgrep registered tools")
			}
			_ = scope.Close(context.Background())
		})
	}
	failing, _ := New(runtime, failingRunner{}, root)
	if err := failing.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrRipgrepUnavailable) || !strings.Contains(err.Error(), "run /tools/rg --version") {
		t.Fatalf("probe failure = %v", err)
	}
	for _, version := range []string{"ripgrep 15.0.0", "ripgrep 15.0.0-beta.1", "ripgrep 15.0.1+local", "ripgrep 16.0.0", "ripgrep 15.1.3 (rev x)"} {
		accepted, _ := New(runtime, &scriptedRunner{version: output(version)}, root)
		scope := &plugin.Scope{}
		if err := accepted.Start(context.Background(), scope); err != nil {
			t.Fatalf("%s rejected: %v", version, err)
		}
		_ = scope.Close(context.Background())
	}
}

type failingRunner struct{}

func (failingRunner) Run(context.Context, platformProcess.Request) (platformProcess.Result, error) {
	return platformProcess.Result{}, errors.New("exec format error")
}

func TestGlob_RunsRipgrepLikeUpstream(t *testing.T) {
	h := newHarness(t, platformProcess.New())
	for index, name := range []string{"new.ts", "src/old.ts", "src/deep/mid.ts", ".hidden/h.ts", "node_modules/x/n.ts", "src/readme.md", "same-b.ts", ".git/HEAD.ts", "sub/.svn/x.ts", "-dash/file.ts"} {
		h.write(t, name, "x")
		h.touch(t, name, time.Duration(10-index)*time.Hour)
	}
	h.write(t, ".gitignore", "node_modules/\n")
	h.touch(t, ".gitignore", 0)
	if err := os.Symlink(filepath.Join(h.root, "new.ts"), filepath.Join(h.root, "link.ts")); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{"pattern": "*.ts"}, "-dash/file.ts\nsame-b.ts\nnode_modules/x/n.ts\n.hidden/h.ts\nsrc/deep/mid.ts\nsrc/old.ts\nnew.ts"},
		{map[string]any{"pattern": "src/*.ts"}, "src/old.ts"},
		{map[string]any{"pattern": "**/*.md"}, "src/readme.md"},
		{map[string]any{"pattern": "*.ts", "path": "src"}, "src/deep/mid.ts\nsrc/old.ts"},
		{map[string]any{"pattern": "*", "path": filepath.Join(h.root, "src", "deep")}, "src/deep/mid.ts"},
		{map[string]any{"pattern": "*.ts", "path": "-dash"}, "-dash/file.ts"},
		{map[string]any{"pattern": "*", "path": ".git"}, "No files found"},
		{map[string]any{"pattern": "*.none"}, "No files found"},
	} {
		result := h.call(t, "glob", test.arguments)
		if result.IsError || result.Output != test.want {
			t.Errorf("glob(%v)\n got: %q\nwant: %q", test.arguments, result.Output, test.want)
		}
	}
	for index := range globMaxResults + 5 {
		name := fmt.Sprintf("many/f%03d.txt", index)
		h.write(t, name, "x")
		h.touch(t, name, time.Duration(index)*time.Minute)
	}
	result := h.call(t, "glob", map[string]any{"pattern": "*.txt", "path": "many"})
	lines := strings.Split(result.Output, "\n")
	if result.IsError || lines[0] != "many/f000.txt" || lines[99] != "many/f099.txt" || !strings.HasSuffix(result.Output, "\n\n(Showing 100 of 105 paths. The complete result could not be saved; narrow pattern or path to see more.)") {
		t.Fatalf("capped = %q", result.Output[len(result.Output)-160:])
	}
}

func TestGlob_RejectsInvalidInputAndUnsafePaths(t *testing.T) {
	h := newHarness(t, platformProcess.New())
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(h.root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{}, `missing required property "pattern"`},
		{map[string]any{"pattern": " "}, "pattern must be a non-empty string"},
		{map[string]any{"pattern": "*", "path": ""}, "path must be a non-empty string when given"},
		{map[string]any{"pattern": "*", "path": 3}, `"path" must be a string`},
		{map[string]any{"pattern": "[x"}, "Error: glob pattern rejected by ripgrep: rg: error parsing glob '[x'"},
		{map[string]any{"pattern": "*", "path": "missing"}, `glob search failed: "missing" not found`},
		{map[string]any{"pattern": "*", "path": outside}, "path is outside the workspace"},
		{map[string]any{"pattern": "*", "path": "escape"}, "path is outside the workspace"},
		{map[string]any{"pattern": "*", "path": ".."}, "path is outside the workspace"},
	} {
		result := h.call(t, "glob", test.arguments)
		if !result.IsError || !strings.Contains(result.Output, test.want) {
			t.Errorf("glob(%v) = %#v, want %q", test.arguments, result, test.want)
		}
	}
}

func TestGrep_RunsRipgrepLikeUpstream(t *testing.T) {
	h := newHarness(t, platformProcess.New())
	h.write(t, "b.txt", "hello world\r\nnope\nhello again\n")
	h.write(t, "a.txt", "say hello")
	h.write(t, ".hidden/h.txt", "hello hidden")
	h.write(t, ".dot.txt", "hello dot")
	h.write(t, "build/out.txt", "hello build")
	h.write(t, "logs/app.log", "hello log")
	h.write(t, "logs/keep.log", "hello keep")
	h.write(t, "notes.ignored", "hello ignored")
	h.write(t, ".gitignore", "build/\n*.log\n")
	h.write(t, "logs/.gitignore", "!keep.log\n")
	h.write(t, ".ignore", "*.ignored\n")
	h.write(t, "bin.dat", "hello\x00binary")
	h.write(t, "latin.txt", "hello caf\xe9")
	// Outside a repository .gitignore is inert while .ignore still applies.
	want := "Found 7 matches\n\na.txt\nLine 1: say hello\n\nb.txt\nLine 1: hello world\nLine 3: hello again\n\nbuild/out.txt\nLine 1: hello build\n\nlatin.txt\nLine 1: (line is not valid UTF-8)\n\nlogs/app.log\nLine 1: hello log\n\nlogs/keep.log\nLine 1: hello keep"
	if result := h.call(t, "grep", map[string]any{"pattern": "hello"}); result.IsError || grouped(result.Output) != want {
		t.Fatalf("no repository:\n got: %q\nwant: %q", grouped(result.Output), want)
	}
	if err := os.Mkdir(filepath.Join(h.root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{"pattern": "hello"}, "Found 5 matches\n\na.txt\nLine 1: say hello\n\nb.txt\nLine 1: hello world\nLine 3: hello again\n\nlatin.txt\nLine 1: (line is not valid UTF-8)\n\nlogs/keep.log\nLine 1: hello keep"},
		{map[string]any{"pattern": "hello", "path": "logs"}, "Found 1 match\n\nlogs/keep.log\nLine 1: hello keep"},
		{map[string]any{"pattern": "hello", "include": "*.log"}, "Found 2 matches\n\nlogs/app.log\nLine 1: hello log\n\nlogs/keep.log\nLine 1: hello keep"},
		{map[string]any{"pattern": "hello", "include": "*.{txt,ignored}", "path": "."}, "Found 6 matches\n\n.dot.txt\nLine 1: hello dot\n\na.txt\nLine 1: say hello\n\nb.txt\nLine 1: hello world\nLine 3: hello again\n\nlatin.txt\nLine 1: (line is not valid UTF-8)\n\nnotes.ignored\nLine 1: hello ignored"},
		{map[string]any{"pattern": "hello", "path": ".hidden"}, "Found 1 match\n\n.hidden/h.txt\nLine 1: hello hidden"},
		{map[string]any{"pattern": "hello", "path": filepath.Join(h.root, "build", "out.txt")}, "Found 1 match\n\nbuild/out.txt\nLine 1: hello build"},
		{map[string]any{"pattern": "^hello (world|again)$"}, "Found 1 match\n\nb.txt\nLine 3: hello again"},
		{map[string]any{"pattern": "absent"}, "No matches found"},
	} {
		result := h.call(t, "grep", test.arguments)
		if result.IsError || grouped(result.Output) != test.want {
			t.Errorf("grep(%v)\n got: %q\nwant: %q", test.arguments, grouped(result.Output), test.want)
		}
	}
	h.write(t, "long.txt", strings.Repeat("界", grepMaxLineBytes/3+10)+"needle")
	if result := h.call(t, "grep", map[string]any{"pattern": "needle", "path": "long.txt"}); result.IsError || result.Output != "Found 1 match\n\nlong.txt\nLine 1: "+strings.Repeat("界", grepMaxLineBytes/3)+" (line truncated)" {
		t.Fatalf("long line = %q", result.Output[:60])
	}
	h.write(t, "many.txt", strings.Repeat("needle\n", grepMaxMatches+3))
	result := h.call(t, "grep", map[string]any{"pattern": "needle", "path": "many.txt"})
	if result.IsError || !strings.HasPrefix(result.Output, "Found 250 of 253 matches\n\nmany.txt\nLine 1: needle\n") || !strings.HasSuffix(result.Output, "Line 250: needle\n\n(The complete result could not be saved; narrow pattern, path, or include to see more.)") {
		t.Fatalf("capped = %q", result.Output[len(result.Output)-160:])
	}
}

func TestGrep_RejectsInvalidInputAndSpecialFiles(t *testing.T) {
	h := newHarness(t, platformProcess.New())
	h.write(t, "a.txt", "text\n")
	if err := syscall.Mkfifo(filepath.Join(h.root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{"pattern": ""}, "pattern must be a non-empty string"},
		{map[string]any{"pattern": "x", "path": " "}, "path must be a non-empty string when given"},
		{map[string]any{"pattern": "x", "include": " "}, "include must be a non-empty glob when given"},
		{map[string]any{"pattern": "x", "include": "!*.go"}, "negated patterns"},
		{map[string]any{"pattern": "x", "include": "*.go,*.ts"}, "not a comma-separated list"},
		{map[string]any{"pattern": "("}, "Error: grep pattern rejected by ripgrep: rg: regex parse error:"},
		{map[string]any{"pattern": "x", "include": "[x"}, "grep pattern rejected by ripgrep: rg: error parsing glob"},
		{map[string]any{"pattern": "x", "path": "missing"}, `grep search failed: "missing" not found`},
		{map[string]any{"pattern": "x", "path": "pipe"}, `grep search failed: "pipe" is not a regular file or directory`},
		{map[string]any{"pattern": "x", "path": "../"}, "path is outside the workspace"},
	} {
		result := h.call(t, "grep", test.arguments)
		if !result.IsError || !strings.Contains(result.Output, test.want) {
			t.Errorf("grep(%v) = %#v, want %q", test.arguments, result, test.want)
		}
	}
	// Check only rejects lists; ripgrep owns the rest of the glob grammar.
	if result := h.call(t, "grep", map[string]any{"pattern": "text", "include": "*.{txt,md}}"}); !result.IsError || !strings.Contains(result.Output, "unopened alternate group") {
		t.Fatalf("unbalanced brace = %#v", result)
	}
}

func TestRun_ClassifiesRipgrepOutcomes(t *testing.T) {
	runner := &scriptedRunner{version: supported()}
	h := newHarness(t, runner)
	request := func() platformProcess.Request { return runner.requests[len(runner.requests)-1] }
	stderr := func(text string, truncated bool) platformProcess.Output {
		return platformProcess.Output{Text: text, Truncated: truncated}
	}
	for _, test := range []struct {
		name   string
		result platformProcess.Result
		err    error
		want   string
	}{
		{"timeout", platformProcess.Result{TimedOut: true, Signal: "SIGKILL", ExitCode: -1}, nil, "Error: grep was aborted before completion (tool timeout or caller cancellation)"},
		{"launch", platformProcess.Result{}, errors.New("permission denied"), "Error: grep could not start its search command (ripgrep launch failed): permission denied"},
		{"signal", platformProcess.Result{Signal: "SIGSEGV", ExitCode: -1}, nil, "Error: grep search command was killed by signal SIGSEGV"},
		{"failed", platformProcess.Result{ExitCode: 2, Stderr: stderr("rg: x: Permission denied (os error 13)\n", false)}, nil, "Error: grep search failed (exit 2): rg: x: Permission denied (os error 13)"},
		{"silent", platformProcess.Result{ExitCode: 2}, nil, "Error: grep search failed (exit 2)"},
		{"truncated stderr", platformProcess.Result{ExitCode: 2, Stderr: stderr("tail", true)}, nil, "Error: grep search failed (exit 2): tail [stderr truncated]"},
		{"overflow", platformProcess.Result{Stdout: platformProcess.Output{Text: "x", Truncated: true}}, nil, "Error: grep produced more raw output than the 20000000-byte cap; narrow pattern, path, or include and retry"},
		{"not json", platformProcess.Result{Stdout: platformProcess.Output{Text: "plain\n"}}, nil, "Error: grep received malformed ripgrep --json output (a line is not JSON)"},
		{"not object", platformProcess.Result{Stdout: platformProcess.Output{Text: "[1]\n"}}, nil, "(a record is not an object)"},
		{"no data", platformProcess.Result{Stdout: platformProcess.Output{Text: `{"type":"match"}`}}, nil, "(a match record has no data)"},
		{"no path", platformProcess.Result{Stdout: platformProcess.Output{Text: `{"type":"match","data":{"path":{"bytes":"eA=="}}}`}}, nil, "(a match record has no path text)"},
		{"no line", platformProcess.Result{Stdout: platformProcess.Output{Text: `{"type":"match","data":{"path":{"text":"a"}}}`}}, nil, "(a match record has no line number)"},
		{"no lines", platformProcess.Result{Stdout: platformProcess.Output{Text: `{"type":"match","data":{"path":{"text":"a"},"line_number":1}}`}}, nil, "(a match record has no line content)"},
		{"empty lines", platformProcess.Result{Stdout: platformProcess.Output{Text: `{"type":"match","data":{"path":{"text":"a"},"line_number":1,"lines":{}}}`}}, nil, "(a match record has neither line text nor bytes)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner.result, runner.err = test.result, test.err
			result := h.call(t, "grep", map[string]any{"pattern": "x"})
			if !result.IsError || !strings.HasPrefix(result.Output, test.want) && !strings.HasSuffix(result.Output, test.want) {
				t.Fatalf("result = %#v, want %q", result, test.want)
			}
		})
	}
	runner.result, runner.err = platformProcess.Result{Stdout: platformProcess.Output{Text: `{"type":"begin","data":{}}` + "\n" + `{"type":"match","data":{"path":{"text":"a"},"line_number":2,"lines":{"text":"x\r"}}}` + "\n"}}, nil
	if result := h.call(t, "grep", map[string]any{"pattern": "x", "path": "."}); result.IsError || result.Output != "Found 1 match\n\na\nLine 2: x\r" {
		t.Fatalf("framing and bare CR = %#v", result)
	}
	sent := request()
	if sent.Mode != platformProcess.ModeHost || sent.Cwd != h.root || sent.Root != h.root || sent.TempDir != "" || sent.Timeout != searchTimeout || sent.StdoutLimit != rawOutputMaxBytes ||
		strings.Join(sent.Args, " ") != "--no-config --json --regexp=x" {
		t.Fatalf("grep request = %+v", sent)
	}
	runner.result = platformProcess.Result{ExitCode: 1}
	if result := h.call(t, "glob", map[string]any{"pattern": "*.go", "path": "."}); result.Output != "No files found" {
		t.Fatalf("glob no match = %#v", result)
	}
	if args := strings.Join(request().Args, " "); !strings.HasPrefix(args, "--no-config --files --glob=*.go --sort=modified --no-ignore --hidden --glob=!**/.git --glob=!**/.git/** ") || strings.Contains(args, " -- ") {
		t.Fatalf("glob argv = %q", args)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner.result, runner.err = platformProcess.Result{}, context.Canceled
	if _, err := h.provider.glob(ctx, appTool.Invocation{}, globArgs{Pattern: "*"}); err == nil || err.Error() != "glob was aborted before completion (tool timeout or caller cancellation)" {
		t.Fatalf("canceled = %v", err)
	}
}

func TestLocate_SurfacesMetadataFailures(t *testing.T) {
	restoreHooks(t)
	h := newHarness(t, &scriptedRunner{version: supported()})
	lstatPath = func(string) (os.FileInfo, error) { return nil, errors.New("io failure") }
	if result := h.call(t, "grep", map[string]any{"pattern": "x"}); !result.IsError || result.Output != "Error: grep search failed: io failure" {
		t.Fatalf("lstat failure = %#v", result)
	}
}
