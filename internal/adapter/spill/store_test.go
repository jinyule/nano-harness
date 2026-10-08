package spill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type allowAll struct{}

func (allowAll) Decide(context.Context, appTool.ApprovalRequest) (session.ApprovalOutcome, error) {
	return session.ApprovalAllowedOnce, nil
}

func restoreHooks(t *testing.T) {
	t.Helper()
	mkdir, stat, lstat, list, remove, random, clock, open := makeDirs, statPath, lstatPath, readDir, removePath, readRandom, now, openFile
	t.Cleanup(func() {
		makeDirs, statPath, lstatPath, readDir, removePath, readRandom, now, openFile = mkdir, stat, lstat, list, remove, random, clock, open
	})
}

func startedRuntime(t *testing.T) *appTool.Runtime {
	t.Helper()
	runtime, err := appTool.New(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return runtime
}

// startStore starts a store over a fresh private root and closes it when the
// test ends.
func startStore(t *testing.T) (*Store, *plugin.Scope) {
	t.Helper()
	store, err := New(startedRuntime(t), Config{Root: filepath.Join(t.TempDir(), "spill"), Workspace: "/work/space"})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := store.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return store, scope
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
}

func TestNew_ValidatesConfigAndPartitionsByWorkspace(t *testing.T) {
	runtime := startedRuntime(t)
	for _, config := range []Config{{Root: "relative", Workspace: "/w"}, {Root: "/r", Workspace: "relative"}} {
		if _, err := New(runtime, config); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("New(%+v) = %v", config, err)
		}
	}
	if _, err := New(nil, Config{Root: "/r", Workspace: "/w"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil runtime = %v", err)
	}
	first, _ := New(runtime, Config{Root: "/r/", Workspace: "/w"})
	again, _ := New(runtime, Config{Root: "/r", Workspace: "/w/"})
	other, _ := New(runtime, Config{Root: "/r", Workspace: "/x"})
	sum := sha256.Sum256([]byte("/w"))
	if first.ID() != "spill-local" || first.Dir() != "/r/workspace-"+hex.EncodeToString(sum[:8]) || again.Dir() != first.Dir() || other.Dir() == first.Dir() {
		t.Fatalf("partitions = %q %q %q", first.Dir(), again.Dir(), other.Dir())
	}
}

func TestStore_StartPublishesPrivateStorageUntilCleanup(t *testing.T) {
	runtime := startedRuntime(t)
	root := filepath.Join(t.TempDir(), "spill")
	store, _ := New(runtime, Config{Root: root, Workspace: "/w"})
	if _, err := store.Create(context.Background(), "s", "x.txt"); !errors.Is(err, ErrClosed) {
		t.Fatalf("create before start = %v", err)
	}
	scope := &plugin.Scope{}
	if err := store.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if err := store.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("second start = %v", err)
	}
	if mode(t, root).Perm() != 0o700 || mode(t, store.Dir()).Perm() != 0o700 {
		t.Fatalf("modes = %v %v", mode(t, root), mode(t, store.Dir()))
	}
	// The runtime holds the published store, so a second one is refused.
	if err := runtime.UseSpill(store, &plugin.Scope{}); err == nil {
		t.Fatal("store was not published")
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), "s", "x.txt"); !errors.Is(err, ErrClosed) {
		t.Fatalf("create after cleanup = %v", err)
	}
	released := &plugin.Scope{}
	if err := runtime.UseSpill(store, released); err != nil {
		t.Fatalf("store still published after cleanup: %v", err)
	}
	_ = released.Close(context.Background())
}

func TestStore_StartRejectsUnsafeDirectoriesAndRollsBack(t *testing.T) {
	base := t.TempDir()
	runtime := startedRuntime(t)
	start := func(root string) error {
		store, _ := New(runtime, Config{Root: root, Workspace: "/w"})
		scope := &plugin.Scope{}
		defer func() { _ = scope.Close(context.Background()) }()
		return store.Start(context.Background(), scope)
	}
	shared := filepath.Join(base, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil { //nolint:gosec // the test needs a group-readable root
		t.Fatal(err)
	}
	if err := start(shared); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("shared root = %v", err)
	}
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := start(file); err == nil {
		t.Fatal("file root accepted")
	}
	// A planted partition symlink is refused even though it points at a
	// private directory.
	root := filepath.Join(base, "root")
	store, _ := New(runtime, Config{Root: root, Workspace: "/w"})
	if err := os.MkdirAll(filepath.Join(base, "elsewhere"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "elsewhere"), store.Dir()); err != nil {
		t.Fatal(err)
	}
	if err := start(root); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("symlinked partition = %v", err)
	}
	// A linked root is followed, like the session root.
	linked := filepath.Join(base, "linked")
	if err := os.Symlink(filepath.Join(base, "elsewhere"), linked); err != nil {
		t.Fatal(err)
	}
	if err := start(linked); err != nil {
		t.Fatalf("linked root = %v", err)
	}

	restoreHooks(t)
	failure := errors.New("io failure")
	statPath = func(string) (fs.FileInfo, error) { return nil, failure }
	if err := start(filepath.Join(base, "stat")); !errors.Is(err, failure) {
		t.Fatalf("stat = %v", err)
	}
	statPath = os.Stat
	makeDirs = func(string, fs.FileMode) error { return failure }
	if err := start(filepath.Join(base, "mkdir")); !errors.Is(err, failure) {
		t.Fatalf("mkdir = %v", err)
	}
	makeDirs = os.MkdirAll

	// A closed scope refuses the first cleanup; a scope that accepts only
	// one cleanup refuses the sweep and joins it before returning.
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	unscoped, _ := New(runtime, Config{Root: filepath.Join(base, "closed"), Workspace: "/w"})
	if err := unscoped.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope = %v", err)
	}
	// A duplicate publication fails after the sweep is owned by the scope.
	published := &plugin.Scope{}
	blocker, _ := New(runtime, Config{Root: filepath.Join(base, "blocker"), Workspace: "/w"})
	if err := blocker.Start(context.Background(), published); err != nil {
		t.Fatal(err)
	}
	duplicate, _ := New(runtime, Config{Root: filepath.Join(base, "duplicate"), Workspace: "/w"})
	scope := &plugin.Scope{}
	if err := duplicate.Start(context.Background(), scope); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("duplicate = %v", err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = published.Close(context.Background())
}

func TestStore_StartJoinsTheSweepWhenItsCleanupIsRefused(t *testing.T) {
	restoreHooks(t)
	store, _ := New(startedRuntime(t), Config{Root: filepath.Join(t.TempDir(), "spill"), Workspace: "/w"})
	scope := &plugin.Scope{}
	// Closing the scope while the cutoff is computed makes the sweep's
	// cleanup registration fail after the store's own cleanup ran.
	now = func() time.Time {
		_ = scope.Close(context.Background())
		return time.Now()
	}
	if err := store.Start(context.Background(), scope); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("refused sweep cleanup = %v", err)
	}
	if _, err := store.Create(context.Background(), "s", "x.txt"); !errors.Is(err, ErrClosed) {
		t.Fatalf("create after rollback = %v", err)
	}
}

func TestStore_CreatesPrivateUnpredictableArtifacts(t *testing.T) {
	store, _ := startStore(t)
	ctx := context.Background()
	file, err := store.Create(ctx, "session-1", "grep-results.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(file, "héllo\n"); err != nil {
		t.Fatal(err)
	}
	ref, err := file.Commit()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("session-1"))
	dir := filepath.Join(store.Dir(), "session-"+hex.EncodeToString(sum[:6]))
	if ref.Locator != file.Locator() || filepath.Dir(ref.Locator) != dir || !regexp.MustCompile(`^[0-9a-f]{12}-grep-results\.txt$`).MatchString(filepath.Base(ref.Locator)) {
		t.Fatalf("locator = %q", ref.Locator)
	}
	data, err := os.ReadFile(ref.Locator)
	if err != nil || string(data) != "héllo\n" || ref.Bytes != 7 || ref.Hint != "Use read with offset/limit, or grep this path to search within it." {
		t.Fatalf("artifact = %q %+v %v", data, ref, err)
	}
	if mode(t, ref.Locator).Perm() != 0o600 || mode(t, dir).Perm() != 0o700 {
		t.Fatalf("modes = %v %v", mode(t, ref.Locator), mode(t, dir))
	}
	second, _ := store.Create(ctx, "session-1", "grep-results.txt")
	other, _ := second.Commit()
	if other.Locator == ref.Locator || other.Bytes != 0 {
		t.Fatalf("same name reused: %q", other.Locator)
	}
	if _, err := file.Commit(); !errors.Is(err, ErrClosed) {
		t.Fatalf("second commit = %v", err)
	}
	if err := file.Discard(); err != nil {
		t.Fatalf("discard after commit = %v", err)
	}
	if _, err := os.Stat(ref.Locator); err != nil {
		t.Fatal("discard after commit removed the artifact")
	}
	for _, test := range []struct{ session, name string }{
		{"", "x.txt"}, {"s", ""}, {"s", "../x"}, {"s", "a/b"}, {"s", "."}, {"s", ".."}, {"s", strings.Repeat("x", 65)},
	} {
		if _, err := store.Create(ctx, test.session, test.name); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("Create(%q, %q) = %v", test.session, test.name, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Create(canceled, "s", "x.txt"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
}

func TestStore_RejectsUnsafeSessionDirectories(t *testing.T) {
	for _, phase := range []string{"before start", "after sweep"} {
		for _, kind := range []string{"symlink", "shared directory", "file"} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				if kind == "shared directory" && runtime.GOOS == "windows" {
					t.Skip("Windows does not enforce Unix permission bits")
				}
				store, err := New(startedRuntime(t), Config{Root: filepath.Join(t.TempDir(), "spill"), Workspace: "/w"})
				if err != nil {
					t.Fatal(err)
				}
				scope := &plugin.Scope{}
				t.Cleanup(func() {
					if err := scope.Close(context.Background()); err != nil {
						t.Error(err)
					}
				})
				if phase == "after sweep" {
					if err := store.Start(t.Context(), scope); err != nil {
						t.Fatal(err)
					}
					<-store.swept
				} else if err := os.MkdirAll(store.Dir(), 0o700); err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256([]byte("s"))
				dir := filepath.Join(store.Dir(), "session-"+hex.EncodeToString(sum[:6]))
				outside := t.TempDir()
				switch kind {
				case "symlink":
					if err := os.Symlink(outside, dir); err != nil {
						t.Fatal(err)
					}
				case "shared directory":
					if err := os.Mkdir(dir, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // unsafe permissions are the regression input
						t.Fatal(err)
					}
				case "file":
					if err := os.WriteFile(dir, nil, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "before start" {
					if err := store.Start(t.Context(), scope); err != nil {
						t.Fatal(err)
					}
					<-store.swept
				}
				file, err := store.Create(t.Context(), "s", "x.txt")
				entries, listErr := os.ReadDir(outside)
				if file != nil {
					_ = file.Discard()
				}
				if err == nil {
					t.Error("unsafe session directory accepted")
				}
				if listErr != nil || len(entries) != 0 {
					t.Fatalf("spill escaped to outside directory: %v, %v", entries, listErr)
				}
			})
		}
	}
}

func TestStore_BoundsAndDiscardsFailedArtifacts(t *testing.T) {
	store, _ := startStore(t)
	ctx := context.Background()
	file, _ := store.Create(ctx, "s", "big.txt")
	if _, err := file.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(make([]byte, maxArtifactBytes)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized write = %v", err)
	}
	if _, err := file.Write([]byte("x")); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("write after failure = %v", err)
	}
	path := file.(*artifact).path
	if _, err := file.Commit(); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("commit after failure = %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("failed commit left the partial artifact")
	}
	discarded, _ := store.Create(ctx, "s", "gone.txt")
	path = discarded.(*artifact).path
	if err := discarded.Discard(); err != nil {
		t.Fatal(err)
	}
	if err := discarded.Discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("discard left the artifact")
	}
}

type fakeFile struct {
	writeErr, syncErr, closeErr error
}

func (file *fakeFile) Write(data []byte) (int, error) { return len(data), file.writeErr }
func (file *fakeFile) Sync() error                    { return file.syncErr }
func (file *fakeFile) Close() error                   { return file.closeErr }

func TestStore_ReportsFilesystemFailures(t *testing.T) {
	restoreHooks(t)
	store, _ := startStore(t)
	ctx := context.Background()
	failure := errors.New("io failure")
	readRandom = func([]byte) (int, error) { return 0, failure }
	if _, err := store.Create(ctx, "s", "x.txt"); !errors.Is(err, failure) {
		t.Fatalf("random = %v", err)
	}
	readRandom = func(buffer []byte) (int, error) { return len(buffer), nil }
	makeDirs = func(string, fs.FileMode) error { return failure }
	if _, err := store.Create(ctx, "s", "x.txt"); !errors.Is(err, failure) {
		t.Fatalf("mkdir = %v", err)
	}
	makeDirs = os.MkdirAll
	// A pruned directory or a name collision is retried a bounded number of
	// times; other failures stop at once.
	attempts := 0
	openFile = func(string) (artifactFile, error) {
		attempts++
		if attempts == 1 {
			return nil, fs.ErrNotExist
		}
		return &fakeFile{}, nil
	}
	if file, err := store.Create(ctx, "s", "x.txt"); err != nil || attempts != 2 {
		t.Fatalf("retry = %v after %d attempts", err, attempts)
	} else {
		_ = file.Discard()
	}
	attempts = 0
	openFile = func(string) (artifactFile, error) {
		attempts++
		return nil, fs.ErrExist
	}
	if _, err := store.Create(ctx, "s", "x.txt"); !errors.Is(err, fs.ErrExist) || attempts != createAttempts {
		t.Fatalf("exhausted = %v after %d attempts", err, attempts)
	}
	attempts = 0
	openFile = func(string) (artifactFile, error) {
		attempts++
		return nil, failure
	}
	if _, err := store.Create(ctx, "s", "x.txt"); !errors.Is(err, failure) || attempts != 1 {
		t.Fatalf("open = %v after %d attempts", err, attempts)
	}
	for _, test := range []struct {
		name string
		file *fakeFile
	}{
		{"write", &fakeFile{writeErr: failure}},
		{"sync", &fakeFile{syncErr: failure}},
		{"close", &fakeFile{closeErr: failure}},
	} {
		t.Run(test.name, func(t *testing.T) {
			openFile = func(string) (artifactFile, error) { return test.file, nil }
			file, err := store.Create(ctx, "s", "x.txt")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = file.Write([]byte("x"))
			if _, err := file.Commit(); !errors.Is(err, failure) {
				t.Fatalf("commit = %v", err)
			}
		})
	}
	if store.open != 0 {
		t.Fatalf("%d artifacts still hold the store open", store.open)
	}
}

func TestStore_CleanupWaitsForOpenArtifacts(t *testing.T) {
	store, scope := startStore(t)
	file, err := store.Create(context.Background(), "s", "x.txt")
	if err != nil {
		t.Fatal(err)
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.stop(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired wait = %v", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- scope.Close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("cleanup returned with an open artifact: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := store.Create(context.Background(), "s", "y.txt"); !errors.Is(err, ErrClosed) {
		t.Fatalf("create while stopping = %v", err)
	}
	if _, err := io.WriteString(file, "finish"); err != nil {
		t.Fatal(err)
	}
	ref, err := file.Commit()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(ref.Locator); string(data) != "finish" {
		t.Fatalf("artifact = %q", data)
	}
}
