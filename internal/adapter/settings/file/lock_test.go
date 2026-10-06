package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	appsettings "github.com/jinyule/nano-harness/internal/app/settings"
)

func TestProvider_PersistCanceledBeforeLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	provider, err := New(appsettings.New(), Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	originalOpen := settingsOpenFile
	t.Cleanup(func() { settingsOpenFile = originalOpen })
	settingsOpenFile = func(string, int, os.FileMode) (settingsFile, error) {
		t.Error("pre-canceled persist attempted to create a writer lock")
		return nil, errors.New("unexpected lock attempt")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := provider.Persist(ctx, appsettings.Defaults()); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled persist = %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 0 {
		t.Fatalf("canceled persist left files: %v, err=%v", entries, err)
	}
	if provider.changed(nil, false) {
		t.Fatal("canceled persist changed the remembered document")
	}
}

func TestProvider_PersistCanceledDuringLockAcquisition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	provider, err := New(appsettings.New(), Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	document := appsettings.Defaults()
	if err := provider.Persist(context.Background(), document); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path) //nolint:gosec // this test owns the private settings path
	if err != nil {
		t.Fatal(err)
	}
	originalOpen := settingsOpenFile
	t.Cleanup(func() { settingsOpenFile = originalOpen })
	acquired, release := make(chan struct{}), make(chan struct{})
	settingsOpenFile = func(name string, flag int, mode os.FileMode) (settingsFile, error) {
		file, openErr := originalOpen(name, flag, mode)
		if name == path+".lock" {
			close(acquired)
			<-release
		}
		return file, openErr
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	finished := make(chan error, 1)
	document.Route = appsettings.Route{Provider: "anthropic", Model: "claude-sonnet-4-5"}
	go func() { finished <- provider.Persist(ctx, document) }()
	<-acquired
	cancel()
	close(release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Errorf("persist canceled during acquisition = %v", err)
	}
	current, err := os.ReadFile(path) //nolint:gosec // this test owns the private settings path
	if err != nil || string(current) != string(original) {
		t.Errorf("canceled persist replaced the document, err=%v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != "settings.yaml" {
		t.Errorf("canceled persist left lock or temp files: %v, err=%v", entries, err)
	}
	if provider.changed(original, true) {
		t.Error("canceled persist changed the remembered document")
	}
}

func TestWithLock_CancellationWinsExpiredWait(t *testing.T) {
	originalOpen, originalWait := settingsOpenFile, settingsLockWait
	t.Cleanup(func() { settingsOpenFile, settingsLockWait = originalOpen, originalWait })
	settingsLockWait = time.Millisecond
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.lock")
		// Every interleaving must return cancellation, whichever select case wins.
		for range 32 {
			testCanceledExpiredWait(t, path)
		}
	})
}

func testCanceledExpiredWait(t *testing.T, path string) {
	t.Helper()
	attempted, release := make(chan struct{}), make(chan struct{})
	settingsOpenFile = func(string, int, os.FileMode) (settingsFile, error) {
		close(attempted)
		<-release
		return nil, os.ErrExist
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- withLock(ctx, path, func() error {
			t.Error("canceled waiter ran the operation")
			return nil
		})
	}()
	<-attempted
	// The virtual clock reaches the lock deadline while the open is blocked.
	deadline := time.NewTimer(settingsLockWait)
	defer deadline.Stop()
	<-deadline.C
	cancel()
	close(release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Errorf("expired lock wait masked cancellation: %v", err)
	}
}

func TestWithLock_CancellationWhileWaiting(t *testing.T) {
	originalOpen := settingsOpenFile
	t.Cleanup(func() { settingsOpenFile = originalOpen })
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.lock")
		settingsOpenFile = func(string, int, os.FileMode) (settingsFile, error) {
			return nil, os.ErrExist
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		finished := make(chan error, 1)
		go func() {
			finished <- withLock(ctx, path, func() error {
				t.Error("canceled waiter ran the operation")
				return nil
			})
		}()
		synctest.Wait()
		cancel()
		if err := <-finished; !errors.Is(err, context.Canceled) {
			t.Fatalf("waiting cancellation = %v", err)
		}
	})
}

func TestProvider_PersistFinishesAfterWriteStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.yaml")
	provider, err := New(appsettings.New(), Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	originalRandom := randomRead
	t.Cleanup(func() { randomRead = originalRandom })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	randomRead = func(buffer []byte) (int, error) {
		cancel()
		return originalRandom(buffer)
	}
	document := appsettings.Defaults()
	if err := provider.Persist(ctx, document); err != nil {
		t.Fatalf("write interrupted after it started: %v", err)
	}
	loaded, err := provider.Load(context.Background())
	if err != nil || loaded.Route != document.Route || ctx.Err() == nil {
		t.Fatalf("completed write: route=%v err=%v context=%v", loaded.Route, err, ctx.Err())
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 || entries[0].Name() != "settings.yaml" {
		t.Fatalf("completed write left lock or temp files: %v, err=%v", entries, err)
	}
}
