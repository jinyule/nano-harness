package file

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type recordingApprover struct {
	mu      sync.Mutex
	outcome session.ApprovalOutcome
	reasons []string
	// during runs while the question is pending, before the decision.
	during func()
}

func (approver *recordingApprover) Decide(_ context.Context, request appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	approver.mu.Lock()
	defer approver.mu.Unlock()
	if approver.during != nil {
		approver.during()
	}
	approver.reasons = append(approver.reasons, request.Reason)
	return approver.outcome, nil
}

type nopJournal struct{}

func (nopJournal) Events(context.Context) ([]session.Event, error) { return nil, nil }

func (nopJournal) Append(context.Context, session.Record) (session.Event, error) {
	return session.Event{}, nil
}

func restoreHooks(t *testing.T) {
	t.Helper()
	stat, lstat, mkdir, rename, link, remove, open, create := statFile, lstatFile, makeDirs, renameFile, linkFile, removeFile, openFile, createTemp
	t.Cleanup(func() {
		statFile, lstatFile, makeDirs, renameFile, linkFile, removeFile, openFile, createTemp = stat, lstat, mkdir, rename, link, remove, open, create
	})
}

func testRoot(t *testing.T) workspace.Root {
	t.Helper()
	root, err := workspace.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// harness registers the provider on a live runtime so calls exercise schema
// validation, approval, and execution exactly as the agent loop does.
type harness struct {
	root     workspace.Root
	runtime  *appTool.Runtime
	approver *recordingApprover
	provider *Provider
	images   *fakeImages
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessOver(t, testRoot(t))
}

func newHarnessOver(t *testing.T, root workspace.Root) *harness {
	t.Helper()
	approver := &recordingApprover{outcome: session.ApprovalAllowedOnce}
	runtime, _ := appTool.New(approver)
	runtimeScope, providerScope := &plugin.Scope{}, &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	images := &fakeImages{}
	provider, err := New(runtime, root, images)
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
	return &harness{root: root, runtime: runtime, approver: approver, provider: provider, images: images}
}

func (h *harness) call(t *testing.T, name string, arguments any) session.ToolResult {
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

// read reads path through the runtime so the session observes it.
func (h *harness) read(t *testing.T, path string) {
	t.Helper()
	if result := h.call(t, "read", map[string]any{"file_path": path}); result.IsError {
		t.Fatalf("read %s = %s", path, result.Output)
	}
}

func (h *harness) path(parts ...string) string {
	return filepath.Join(append([]string{h.root.Path()}, parts...)...)
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFixture(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the path is inside this test's temporary workspace
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestProvider_ValidatesRegistersAndCleansTools(t *testing.T) {
	runtime, _ := appTool.New(&recordingApprover{})
	root := testRoot(t)
	if _, err := New(nil, root, &fakeImages{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil runtime = %v", err)
	}
	if _, err := New(runtime, workspace.Root{}, &fakeImages{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero root = %v", err)
	}
	if _, err := New(runtime, root, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil images = %v", err)
	}
	provider, err := New(runtime, root, &fakeImages{})
	if err != nil || provider.ID() != "fs-tools" {
		t.Fatalf("provider = %+v, %v", provider, err)
	}
	if err := provider.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, appTool.ErrNotRunning) {
		t.Fatalf("inactive runtime = %v", err)
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
	catalog, _ := runtime.Catalog(nil)
	if len(catalog.Definitions) != 4 || catalog.Definitions[0].Name != "edit" || catalog.Definitions[1].Name != "read" || catalog.Definitions[2].Name != "read_image" || catalog.Definitions[3].Name != "write" {
		t.Fatalf("definitions = %#v", catalog.Definitions)
	}
	wantGuidance := []string{
		"Use the read tool — not shell commands like cat — to inspect text files. Use offset and limit to continue reading large files.",
		"Read an existing file before overwriting it with write (the default fs-observation-policy requires it) and prefer edit for targeted changes.",
		"Read a file before editing it (the default fs-observation-policy requires it), unless you just created or edited it in this session.",
	}
	if strings.Join(catalog.Guidance, "|") != strings.Join(wantGuidance, "|") {
		t.Fatalf("guidance = %q", catalog.Guidance)
	}
	// Without edit, write drops its pointer to it.
	if catalog, _ := runtime.Catalog([]string{"write"}); len(catalog.Guidance) != 1 || catalog.Guidance[0] != "Read an existing file before overwriting it with write (the default fs-observation-policy requires it)." {
		t.Fatalf("write-only guidance = %q", catalog.Guidance)
	}
	provider.observed.record("s", root.Path(), observation{present: true})
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if catalog, _ := runtime.Catalog(nil); len(catalog.Definitions) != 0 {
		t.Fatalf("tools retained after cleanup: %#v", catalog.Definitions)
	}
	if _, ok := provider.observed.lookup("s", root.Path()); ok {
		t.Fatal("observations survived scope cleanup")
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := provider.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope = %v", err)
	}

	// A registration failure part-way through leaves earlier tools owned by
	// the scope, so closing it rolls the partial start back.
	blocker := appTool.Define(appTool.Spec[struct{}]{Name: "write", Description: "occupies the name", Execute: func(context.Context, appTool.Invocation, struct{}) (appTool.Result, error) {
		return appTool.Result{}, nil
	}})
	blockerScope, partial := &plugin.Scope{}, &plugin.Scope{}
	if err := runtime.Register(blocker, blockerScope); err != nil {
		t.Fatal(err)
	}
	if err := provider.Start(context.Background(), partial); err == nil || !strings.Contains(err.Error(), `duplicate "write"`) {
		t.Fatalf("partial start = %v", err)
	}
	if catalog, _ := runtime.Catalog([]string{"read"}); len(catalog.Definitions) != 1 {
		t.Fatal("read was not registered before the failure")
	}
	if err := partial.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if catalog, _ := runtime.Catalog([]string{"read", "edit"}); len(catalog.Definitions) != 0 {
		t.Fatalf("partial start leaked tools: %#v", catalog.Definitions)
	}
	_ = blockerScope.Close(context.Background())
}

type fakeStaged struct {
	name                                  string
	writeErr, syncErr, chmodErr, closeErr error
	written                               []byte
	mode                                  fs.FileMode
}

func (staged *fakeStaged) Write(data []byte) (int, error) {
	staged.written = append(staged.written, data...)
	return len(data), staged.writeErr
}
func (staged *fakeStaged) Sync() error { return staged.syncErr }
func (staged *fakeStaged) Chmod(mode fs.FileMode) error {
	staged.mode = mode
	return staged.chmodErr
}
func (staged *fakeStaged) Close() error { return staged.closeErr }
func (staged *fakeStaged) Name() string { return staged.name }

func TestWriteAtomic_PublishesOrRemovesStagedFile(t *testing.T) {
	restoreHooks(t)
	directory := t.TempDir()
	target := filepath.Join(directory, "out.txt")
	if err := writeAtomic(context.Background(), target, []byte("first"), 0o640, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Mode().Perm() != 0o640 || readFixture(t, target) != "first" {
		t.Fatalf("published = %v, %v", info, err)
	}
	entries, _ := os.ReadDir(directory)
	if len(entries) != 1 {
		t.Fatalf("staging residue: %v", entries)
	}

	failure := errors.New("failure")
	for _, test := range []struct {
		name   string
		staged *fakeStaged
		rename error
	}{
		{name: "write", staged: &fakeStaged{writeErr: failure}},
		{name: "sync", staged: &fakeStaged{syncErr: failure}},
		{name: "chmod", staged: &fakeStaged{chmodErr: failure}},
		{name: "close", staged: &fakeStaged{closeErr: failure}},
		{name: "rename", staged: &fakeStaged{}, rename: failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			restoreHooks(t)
			removed := ""
			test.staged.name = filepath.Join(directory, "staged")
			createTemp = func(dir, pattern string) (stagedFile, error) {
				if dir != directory || !strings.HasPrefix(pattern, ".out.txt.") {
					t.Fatalf("staged in %q as %q", dir, pattern)
				}
				return test.staged, nil
			}
			renameFile = func(string, string) error { return test.rename }
			removeFile = func(name string) error {
				removed = name
				return nil
			}
			if err := writeAtomic(context.Background(), target, []byte("second"), 0o600, false); !errors.Is(err, failure) || removed != test.staged.name {
				t.Fatalf("error = %v, removed = %q", err, removed)
			}
		})
	}
	createTemp = func(string, string) (stagedFile, error) { return nil, failure }
	if err := writeAtomic(context.Background(), target, nil, 0o600, false); !errors.Is(err, failure) {
		t.Fatalf("create error = %v", err)
	}
	if readFixture(t, target) != "first" {
		t.Fatal("failed writes changed the published file")
	}
}

func TestWriteAtomic_ExclusivePublicationNeverClobbers(t *testing.T) {
	restoreHooks(t)
	directory := t.TempDir()
	target := filepath.Join(directory, "new.txt")
	if err := writeAtomic(context.Background(), target, []byte("mine"), 0o600, true); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(context.Background(), target, []byte("theirs"), 0o600, true); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("second exclusive publication = %v", err)
	}
	entries, _ := os.ReadDir(directory)
	if readFixture(t, target) != "mine" || len(entries) != 1 {
		t.Fatalf("content = %q, entries = %v", readFixture(t, target), entries)
	}
}

func TestWriteAtomic_CancellationBeforeStagingAndAfterCommit(t *testing.T) {
	restoreHooks(t)
	directory := t.TempDir()
	target := filepath.Join(directory, "file.txt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writeAtomic(ctx, target, []byte("before"), 0o600, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("before staging = %v", err)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		t.Fatalf("cancellation created staging: %v, %v", entries, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	t.Cleanup(cancel)
	renameFile = func(from, to string) error {
		err := os.Rename(from, to)
		cancel()
		return err
	}
	if err := writeAtomic(ctx, target, []byte("committed"), 0o600, false); err != nil {
		t.Fatalf("after commit = %v", err)
	}
	if readFixture(t, target) != "committed" {
		t.Fatal("cancellation undid the committed write")
	}
}

type failingReader struct {
	data []byte
	err  error
}

func (reader *failingReader) Read(buffer []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, reader.err
	}
	count := copy(buffer, reader.data)
	reader.data = reader.data[count:]
	return count, nil
}

func (*failingReader) Close() error { return nil }

func openFailing(data []byte, err error) func(string) (io.ReadCloser, error) {
	return func(string) (io.ReadCloser, error) { return &failingReader{data: data, err: err}, nil }
}
