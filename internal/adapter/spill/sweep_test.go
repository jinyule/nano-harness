package spill

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/core/plugin"
)

// sweepTree builds a spill root with expired and fresh artifacts, foreign
// entries, a symlink, and an unsafe directory, and returns the paths.
type sweepTree struct {
	root, active, activeSession, foreign, foreignSession string
	expired, fresh, link, unrelated, shared, stray       string
	sharedFile, nested, outside                          string
}

func buildSweepTree(t *testing.T) sweepTree {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "spill")
	tree := sweepTree{
		root:   root,
		active: filepath.Join(root, "workspace-0123456789abcdef"), foreign: filepath.Join(root, "workspace-fedcba9876543210"),
		outside: filepath.Join(base, "outside.txt"),
	}
	tree.activeSession = filepath.Join(tree.active, "session-0123456789ab")
	tree.foreignSession = filepath.Join(tree.foreign, "session-ba9876543210")
	tree.expired = filepath.Join(tree.activeSession, "aaaaaaaaaaaa-old.txt")
	tree.fresh = filepath.Join(tree.activeSession, "bbbbbbbbbbbb-new.txt")
	tree.link = filepath.Join(tree.activeSession, "cccccccccccc-link.txt")
	tree.nested = filepath.Join(tree.activeSession, "nested")
	tree.unrelated = filepath.Join(tree.active, "session-backup")
	tree.shared = filepath.Join(tree.active, "session-111111111111")
	tree.sharedFile = filepath.Join(tree.shared, "old.txt")
	tree.stray = filepath.Join(root, "notes.txt")
	for _, dir := range []string{tree.activeSession, tree.foreignSession, tree.unrelated, tree.nested} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(tree.shared, 0o755); err != nil { //nolint:gosec // an unsafe directory the sweep must skip
		t.Fatal(err)
	}
	old := time.Now().Add(-retention - time.Hour)
	for _, path := range []string{tree.expired, tree.fresh, tree.sharedFile, tree.stray, tree.outside, filepath.Join(tree.foreignSession, "dddddddddddd-old.txt"), filepath.Join(tree.unrelated, "old.txt")} {
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if path != tree.fresh {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.Symlink(tree.outside, tree.link); err != nil {
		t.Fatal(err)
	}
	return tree
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestSweep_DeletesOnlyExpiredArtifactsInPrivateSessionDirectories(t *testing.T) {
	tree := buildSweepTree(t)
	sweep(context.Background(), tree.root, tree.active, time.Now().Add(-retention), &sync.Mutex{})
	for path, want := range map[string]bool{
		tree.expired: false, tree.fresh: true, tree.link: true, tree.outside: true, tree.nested: true,
		tree.unrelated: true, filepath.Join(tree.unrelated, "old.txt"): true,
		tree.sharedFile: true, tree.stray: true,
		tree.foreign: false, tree.active: true,
	} {
		if exists(path) != want {
			t.Errorf("exists(%s) = %v, want %v", path, !want, want)
		}
	}
	// Once the fresh file, the link, and the nested directory are gone, the
	// session directory is pruned; the active partition always stays.
	for _, path := range []string{tree.fresh, tree.link, tree.nested} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	sweep(context.Background(), tree.root, tree.active, time.Now().Add(-retention), &sync.Mutex{})
	if exists(tree.activeSession) || !exists(tree.active) {
		t.Fatalf("session=%v active=%v", exists(tree.activeSession), exists(tree.active))
	}
}

func TestSweep_StopsOnCancellationAndContainsFailures(t *testing.T) {
	tree := buildSweepTree(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	sweep(canceled, tree.root, tree.active, time.Now(), &sync.Mutex{})
	if !exists(tree.expired) || !exists(tree.foreign) {
		t.Fatal("a canceled sweep deleted artifacts")
	}
	if sweepSession(canceled, tree.activeSession, time.Now()) || !exists(tree.fresh) {
		t.Fatal("a canceled session sweep deleted artifacts")
	}

	restoreHooks(t)
	failure := errors.New("io failure")
	readDir = func(string) ([]os.DirEntry, error) { return nil, failure }
	if sweepSession(context.Background(), tree.activeSession, time.Now()) || privateDirectories(context.Background(), tree.root, workspaceEntry) != nil {
		t.Fatal("unreadable directories were swept")
	}
	readDir = os.ReadDir
	lstatPath = func(string) (fs.FileInfo, error) { return nil, failure }
	if len(privateDirectories(context.Background(), tree.root, workspaceEntry)) != 0 {
		t.Fatal("uninspectable directories were listed")
	}
	lstatPath = os.Lstat
	removePath = func(string) error { return failure }
	sweep(context.Background(), tree.root, tree.active, time.Now().Add(-retention), &sync.Mutex{})
	if !exists(tree.foreign) || !exists(tree.expired) {
		t.Fatal("failed removals were reported as pruned")
	}
}

func TestStore_StartSweepsExpiredArtifacts(t *testing.T) {
	tree := buildSweepTree(t)
	store, _ := New(startedRuntime(t), Config{Root: tree.root, Workspace: "/w"})
	scope := &plugin.Scope{}
	if err := store.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	<-store.swept
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exists(tree.expired) || exists(tree.foreign) || !exists(tree.fresh) || !exists(store.Dir()) {
		t.Fatalf("expired=%v foreign=%v fresh=%v partition=%v", exists(tree.expired), exists(tree.foreign), exists(tree.fresh), exists(store.Dir()))
	}
}

func TestStore_CleanupCancelsAndJoinsTheSweep(t *testing.T) {
	restoreHooks(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	readDir = func(path string) ([]os.DirEntry, error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return os.ReadDir(path)
	}
	store, scope := startStore(t)
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- scope.Close(context.Background()) }()
	select {
	case <-store.swept:
		t.Fatal("the sweep finished while blocked")
	case err := <-closed:
		t.Fatalf("cleanup returned before the sweep: %v", err)
	default:
	}
	close(release)
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.swept:
	default:
		t.Fatal("cleanup returned before the sweep joined")
	}
}

// flagLocker records whether it is held.
type flagLocker struct{ held bool }

func (locker *flagLocker) Lock()   { locker.held = true }
func (locker *flagLocker) Unlock() { locker.held = false }

func TestSweep_PrunesSessionDirectoriesUnderTheLayoutLock(t *testing.T) {
	restoreHooks(t)
	tree := buildSweepTree(t)
	layout := &flagLocker{}
	pruned := 0
	removePath = func(path string) error {
		if filepath.Base(filepath.Dir(path)) != "spill" && sessionEntry.MatchString(filepath.Base(path)) {
			if !layout.held {
				t.Errorf("pruned %s without the layout lock", path)
			}
			pruned++
		}
		return os.Remove(path)
	}
	sweep(context.Background(), tree.root, tree.active, time.Now().Add(-retention), layout)
	if pruned == 0 || layout.held {
		t.Fatalf("pruned=%d held=%v", pruned, layout.held)
	}
}
