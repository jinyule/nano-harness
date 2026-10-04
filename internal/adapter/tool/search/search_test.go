package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type denyApprover struct{}

func (denyApprover) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalRejected, nil
}

type nopJournal struct{}

func (nopJournal) Append(context.Context, session.Record) (session.Event, error) {
	return session.Event{}, nil
}

func restoreHooks(t *testing.T) {
	t.Helper()
	read, lstat, file, open, info := readDirectory, lstatPath, readFile, openFile, fileInfo
	t.Cleanup(func() { readDirectory, lstatPath, readFile, openFile, fileInfo = read, lstat, file, open, info })
}

type harness struct {
	root     string
	runtime  *appTool.Runtime
	provider *Provider
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The workspace sits below a private parent so the Git probe above the
	// root sees only directories this test controls.
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
	provider, err := New(runtime, root)
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

func (h *harness) touch(t *testing.T, name string, offset time.Duration) {
	t.Helper()
	moment := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Add(offset)
	if err := os.Chtimes(filepath.Join(h.root, filepath.FromSlash(name)), moment, moment); err != nil {
		t.Fatal(err)
	}
}

func TestProvider_ValidatesRegistersAndCleansTools(t *testing.T) {
	runtime, _ := appTool.New(denyApprover{})
	root, _ := workspace.Resolve(t.TempDir())
	if _, err := New(nil, root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil runtime = %v", err)
	}
	if _, err := New(runtime, workspace.Root{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero root = %v", err)
	}
	provider, err := New(runtime, root)
	if err != nil || provider.ID() != "search-tools" || provider.timeout != searchTimeout || provider.rawLimit != rawOutputMaxBytes {
		t.Fatalf("provider = %+v, %v", provider, err)
	}
	if err := provider.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, appTool.ErrNotRunning) {
		t.Fatalf("inactive runtime = %v", err)
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

func TestGlob_ListsMatchingFilesOldestFirst(t *testing.T) {
	h := newHarness(t)
	for index, name := range []string{"new.ts", "src/old.ts", "src/deep/mid.ts", ".hidden/h.ts", "node_modules/x/n.ts", "src/readme.md", "same-b.ts", "same-a.ts", ".git/HEAD.ts", "sub/.svn/x.ts"} {
		h.write(t, name, "x")
		h.touch(t, name, time.Duration(10-index)*time.Hour)
	}
	h.touch(t, "same-a.ts", 0)
	h.touch(t, "same-b.ts", 0)
	h.write(t, ".gitignore", "node_modules/\n")
	if err := os.Symlink(filepath.Join(h.root, "new.ts"), filepath.Join(h.root, "link.ts")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(h.root, "pipe.ts"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{"pattern": "*.ts"}, "same-a.ts\nsame-b.ts\nnode_modules/x/n.ts\n.hidden/h.ts\nsrc/deep/mid.ts\nsrc/old.ts\nnew.ts"},
		{map[string]any{"pattern": "src/*.ts"}, "src/old.ts"},
		{map[string]any{"pattern": "**/*.md"}, "src/readme.md"},
		{map[string]any{"pattern": "*.ts", "path": "src"}, "src/deep/mid.ts\nsrc/old.ts"},
		{map[string]any{"pattern": "*", "path": filepath.Join(h.root, "src", "deep")}, "src/deep/mid.ts"},
		{map[string]any{"pattern": "*.ts", "path": "new.ts"}, "new.ts"},
		{map[string]any{"pattern": "*.md", "path": "new.ts"}, "No files found"},
		{map[string]any{"pattern": "!*.ts", "path": "src"}, "src/readme.md"},
		{map[string]any{"pattern": "!deep"}, "same-a.ts\nsame-b.ts\nsrc/readme.md\nnode_modules/x/n.ts\n.hidden/h.ts\nsrc/old.ts\nnew.ts\n.gitignore"},
		{map[string]any{"pattern": "*", "path": ".git"}, "No files found"},
		{map[string]any{"pattern": "*.none"}, "No files found"},
	} {
		result := h.call(t, "glob", test.arguments)
		if result.IsError || result.Output != test.want {
			t.Errorf("glob(%v)\n got: %q\nwant: %q", test.arguments, result.Output, test.want)
		}
	}
}

func TestGlob_CapsResultsAndRejectsInvalidInput(t *testing.T) {
	h := newHarness(t)
	for index := range globMaxResults + 5 {
		name := fmt.Sprintf("f%03d.txt", index)
		h.write(t, name, "x")
		h.touch(t, name, time.Duration(index)*time.Minute)
	}
	result := h.call(t, "glob", map[string]any{"pattern": "*.txt"})
	lines := strings.Split(result.Output, "\n")
	if result.IsError || lines[0] != "f000.txt" || lines[99] != "f099.txt" || !strings.HasSuffix(result.Output, "\n\n(Showing 100 of 105 paths. The complete result could not be saved; narrow pattern or path to see more.)") {
		t.Fatalf("capped = %q", result.Output[len(result.Output)-200:])
	}
	outside := t.TempDir()
	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{}, `missing required property "pattern"`},
		{map[string]any{"pattern": " "}, "pattern must be a non-empty string"},
		{map[string]any{"pattern": "*", "path": ""}, "path must be a non-empty string when given"},
		{map[string]any{"pattern": "*", "path": 3}, `"path" must be a string`},
		{map[string]any{"pattern": "[x"}, "glob pattern rejected: unclosed character class"},
		{map[string]any{"pattern": "*", "path": "missing"}, `glob search failed: "missing" not found`},
		{map[string]any{"pattern": "*", "path": outside}, "path is outside the workspace"},
		{map[string]any{"pattern": "*", "path": ".."}, "path is outside the workspace"},
	} {
		result := h.call(t, "glob", test.arguments)
		if !result.IsError || !strings.Contains(result.Output, test.want) {
			t.Errorf("glob(%v) = %#v, want %q", test.arguments, result, test.want)
		}
	}
	h.provider.rawLimit = 20
	if result := h.call(t, "glob", map[string]any{"pattern": "*.txt"}); !result.IsError || !strings.Contains(result.Output, "more than 20 bytes of raw output") {
		t.Fatalf("raw limit = %#v", result)
	}
	// A zero budget expires when the context is created, so cancellation is
	// observed deterministically instead of racing the deadline timer.
	h.provider.rawLimit, h.provider.timeout = rawOutputMaxBytes, 0
	if result := h.call(t, "glob", map[string]any{"pattern": "*.txt"}); !result.IsError || result.Output != "Error: glob was aborted before completion (tool timeout or caller cancellation)" {
		t.Fatalf("timeout = %#v", result)
	}
}

func TestGlob_SurfacesTraversalFailures(t *testing.T) {
	restoreHooks(t)
	h := newHarness(t)
	h.write(t, "a.txt", "x")
	failure := errors.New("io failure")
	fileInfo = func(fs.DirEntry) (fs.FileInfo, error) { return nil, failure }
	if result := h.call(t, "glob", map[string]any{"pattern": "*"}); !result.IsError || !strings.Contains(result.Output, "glob search failed: io failure") {
		t.Fatalf("info failure = %#v", result)
	}
	fileInfo = func(entry fs.DirEntry) (fs.FileInfo, error) { return entry.Info() }
	readDirectory = func(string) ([]os.DirEntry, error) { return nil, failure }
	if result := h.call(t, "glob", map[string]any{"pattern": "*"}); !result.IsError || !strings.Contains(result.Output, "io failure") {
		t.Fatalf("readdir failure = %#v", result)
	}
	readDirectory = os.ReadDir
	lstatPath = func(string) (os.FileInfo, error) { return nil, failure }
	if result := h.call(t, "glob", map[string]any{"pattern": "*"}); !result.IsError || !strings.Contains(result.Output, "glob search failed: io failure") {
		t.Fatalf("lstat failure = %#v", result)
	}
}

func TestGrep_SearchesLikeRipgrepDefaults(t *testing.T) {
	h := newHarness(t)
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
	h.write(t, "late.txt", strings.Repeat("x\n", binaryPeekBytes/2)+"hello late\x00\nhello after")
	h.write(t, "latin.txt", "hello caf\xe9")
	// Without a repository .gitignore is inert but .ignore still applies.
	result := h.call(t, "grep", map[string]any{"pattern": "hello"})
	want := "Found 7 matches\n\na.txt\nLine 1: say hello\n\nb.txt\nLine 1: hello world\nLine 3: hello again\n\nbuild/out.txt\nLine 1: hello build\n\nlatin.txt\nLine 1: (line is not valid UTF-8)\n\nlogs/app.log\nLine 1: hello log"
	if result.IsError || result.Output != want+"\n\nlogs/keep.log\nLine 1: hello keep" {
		t.Fatalf("no repository:\n got: %q\nwant: %q", result.Output, want)
	}
	// A nested repository applies its own .gitignore below its marker.
	h.write(t, "nested/.git/HEAD", "ref")
	h.write(t, "nested/.gitignore", "skip.txt\n")
	h.write(t, "nested/skip.txt", "hello skip")
	h.write(t, "nested/seen.txt", "hello seen")
	if result := h.call(t, "grep", map[string]any{"pattern": "hello", "path": "nested"}); result.IsError || result.Output != "Found 1 match\n\nnested/seen.txt\nLine 1: hello seen" {
		t.Fatalf("nested repository = %q", result.Output)
	}
	if err := os.RemoveAll(filepath.Join(h.root, "nested")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(filepath.Dir(h.root), ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	result = h.call(t, "grep", map[string]any{"pattern": "hello"})
	want = "Found 5 matches\n\na.txt\nLine 1: say hello\n\nb.txt\nLine 1: hello world\nLine 3: hello again\n\nlatin.txt\nLine 1: (line is not valid UTF-8)\n\nlogs/keep.log\nLine 1: hello keep"
	if result.IsError || result.Output != want {
		t.Fatalf("repository:\n got: %q\nwant: %q", result.Output, want)
	}
	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{"pattern": "hello", "path": "logs"}, "Found 1 match\n\nlogs/keep.log\nLine 1: hello keep"},
		{map[string]any{"pattern": "hello", "include": "*.log"}, "Found 2 matches\n\nlogs/app.log\nLine 1: hello log\n\nlogs/keep.log\nLine 1: hello keep"},
		{map[string]any{"pattern": "hello", "include": "*.{txt,ignored}", "path": "."}, "Found 6 matches\n\n.dot.txt\nLine 1: hello dot\n\na.txt\nLine 1: say hello\n\nb.txt\nLine 1: hello world\nLine 3: hello again\n\nlatin.txt\nLine 1: (line is not valid UTF-8)\n\nnotes.ignored\nLine 1: hello ignored"},
		{map[string]any{"pattern": "hello", "path": ".hidden"}, "Found 1 match\n\n.hidden/h.txt\nLine 1: hello hidden"},
		{map[string]any{"pattern": "hello", "path": "build/out.txt"}, "Found 1 match\n\nbuild/out.txt\nLine 1: hello build"},
		{map[string]any{"pattern": "binary", "path": "bin.dat"}, "Found 1 match\n\nbin.dat\nLine 2: binary"},
		{map[string]any{"pattern": "late|after", "path": "late.txt"}, "Found 2 matches\n\nlate.txt\nLine 32769: hello late\nLine 32771: hello after"},
		{map[string]any{"pattern": "^hello (world|again)$"}, "Found 1 match\n\nb.txt\nLine 3: hello again"},
		{map[string]any{"pattern": "absent"}, "No matches found"},
	} {
		result := h.call(t, "grep", test.arguments)
		if result.IsError || result.Output != test.want {
			t.Errorf("grep(%v)\n got: %q\nwant: %q", test.arguments, result.Output, test.want)
		}
	}
}

func TestGrep_CapsPreviewsAndMatches(t *testing.T) {
	h := newHarness(t)
	h.write(t, "long.txt", strings.Repeat("界", grepMaxLineBytes/3+10)+"needle")
	h.write(t, "many.txt", strings.Repeat("needle\n", grepMaxMatches+3))
	result := h.call(t, "grep", map[string]any{"pattern": "needle", "path": "long.txt"})
	if result.IsError || result.Output != "Found 1 match\n\nlong.txt\nLine 1: "+strings.Repeat("界", grepMaxLineBytes/3)+" (line truncated)" {
		t.Fatalf("long line = %q", result.Output[:80])
	}
	result = h.call(t, "grep", map[string]any{"pattern": "needle", "path": "many.txt"})
	if result.IsError || !strings.HasPrefix(result.Output, "Found 250 of 253 matches\n\nmany.txt\nLine 1: needle\n") || !strings.HasSuffix(result.Output, "Line 250: needle\n\n(The complete result could not be saved; narrow pattern, path, or include to see more.)") {
		t.Fatalf("capped = %q", result.Output[len(result.Output)-200:])
	}
	h.write(t, "huge.txt", "needle\n"+strings.Repeat("y", maxScanLineBytes+1)+"\nneedle\n")
	if result := h.call(t, "grep", map[string]any{"pattern": "needle", "path": "huge.txt"}); result.IsError || result.Output != "Found 1 match\n\nhuge.txt\nLine 1: needle" {
		t.Fatalf("oversized line = %q", result.Output)
	}
}

func TestGrep_RejectsInvalidInputAndSurfacesFailures(t *testing.T) {
	restoreHooks(t)
	h := newHarness(t)
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
		{map[string]any{"pattern": "x", "include": "*.{go,ts}}"}, ""},
		{map[string]any{"pattern": "("}, "grep pattern rejected"},
		{map[string]any{"pattern": "x", "include": "[x"}, "grep include rejected"},
		{map[string]any{"pattern": "x", "path": "missing"}, `grep search failed: "missing" not found`},
		{map[string]any{"pattern": "x", "path": "pipe"}, "is not a regular file or directory"},
		{map[string]any{"pattern": "x", "path": "../"}, "path is outside the workspace"},
	} {
		result := h.call(t, "grep", test.arguments)
		if test.want == "" {
			if result.IsError {
				t.Errorf("grep(%v) rejected: %s", test.arguments, result.Output)
			}
			continue
		}
		if !result.IsError || !strings.Contains(result.Output, test.want) {
			t.Errorf("grep(%v) = %#v, want %q", test.arguments, result, test.want)
		}
	}
	failure := errors.New("io failure")
	openFile = func(string) (io.ReadCloser, error) { return nil, failure }
	if result := h.call(t, "grep", map[string]any{"pattern": "x"}); !result.IsError || !strings.Contains(result.Output, "io failure") {
		t.Fatalf("open failure = %#v", result)
	}
	openFile = func(string) (io.ReadCloser, error) { return &failingReader{err: failure}, nil }
	if result := h.call(t, "grep", map[string]any{"pattern": "x"}); !result.IsError || !strings.Contains(result.Output, "io failure") {
		t.Fatalf("peek failure = %#v", result)
	}
	if result := h.call(t, "grep", map[string]any{"pattern": "x", "path": "a.txt"}); !result.IsError || !strings.Contains(result.Output, "io failure") {
		t.Fatalf("read failure = %#v", result)
	}
	openFile = func(path string) (io.ReadCloser, error) { return os.Open(path) } //nolint:gosec // test-owned workspace
	h.write(t, ".ignore", "x")
	readFile = func(string) ([]byte, error) { return nil, failure }
	if result := h.call(t, "grep", map[string]any{"pattern": "x"}); !result.IsError || !strings.Contains(result.Output, "io failure") {
		t.Fatalf("ignore read failure = %#v", result)
	}
	readFile = os.ReadFile
	h.write(t, "sub/.rgignore", strings.Repeat("x", maxIgnoreFileBytes+1))
	if result := h.call(t, "grep", map[string]any{"pattern": "x", "path": "sub/.."}); !result.IsError || !strings.Contains(result.Output, "exceeds 1048576 bytes") {
		t.Fatalf("large ignore file = %#v", result)
	}
	if result := h.call(t, "grep", map[string]any{"pattern": "x", "path": "sub/.rgignore"}); result.IsError {
		t.Fatalf("explicit file below a large ignore file = %#v", result)
	}
	h.write(t, "deep/inner/file.txt", "x")
	h.write(t, "deep/.rgignore", strings.Repeat("x", maxIgnoreFileBytes+1))
	if result := h.call(t, "grep", map[string]any{"pattern": "x", "path": "deep/inner"}); !result.IsError || !strings.Contains(result.Output, "exceeds") {
		t.Fatalf("ancestor ignore file = %#v", result)
	}
	if err := os.Remove(filepath.Join(h.root, "deep", ".rgignore")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(h.root, "a.txt"), filepath.Join(h.root, "deep", ".rgignore")); err != nil {
		t.Fatal(err)
	}
	if result := h.call(t, "grep", map[string]any{"pattern": "x", "path": "deep"}); result.IsError || !strings.Contains(result.Output, "deep/inner/file.txt") {
		t.Fatalf("symlinked ignore file must be skipped: %#v", result)
	}
	lstatPath = func(name string) (os.FileInfo, error) {
		if strings.HasSuffix(name, ".ignore") {
			return nil, failure
		}
		return os.Lstat(name)
	}
	if result := h.call(t, "grep", map[string]any{"pattern": "x"}); !result.IsError || !strings.Contains(result.Output, "io failure") {
		t.Fatalf("ignore lstat failure = %#v", result)
	}
	lstatPath = os.Lstat
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.provider.grep(ctx, appTool.Invocation{}, grepArgs{Pattern: "x", Path: new("a.txt")}); err == nil || !strings.Contains(err.Error(), "aborted before completion") {
		t.Fatalf("canceled = %v", err)
	}
}

type failingReader struct{ err error }

func (reader *failingReader) Read([]byte) (int, error) { return 0, reader.err }
func (*failingReader) Close() error                    { return nil }

func TestGitAbove_FindsRepositoryMarkers(t *testing.T) {
	base := t.TempDir()
	nested := filepath.Join(base, "a", "b")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if gitAbove(nested) {
		t.Skip("the temporary directory is already inside a Git repository")
	}
	if err := os.WriteFile(filepath.Join(base, "a", ".git"), []byte("gitdir: elsewhere"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !gitAbove(nested) {
		t.Fatal("worktree marker file not found")
	}
}
