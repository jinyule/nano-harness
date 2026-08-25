package file

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsettings "github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

type faultSettingsFile struct {
	writeN   int
	writeErr error
	syncErr  error
	closeErr error
}

func (file *faultSettingsFile) Write([]byte) (int, error) { return file.writeN, file.writeErr }
func (file *faultSettingsFile) Sync() error               { return file.syncErr }
func (file *faultSettingsFile) Close() error              { return file.closeErr }

func TestProviderPersistLoadWatchAndLifecycle(t *testing.T) {
	service := appsettings.New()
	serviceScope := &plugin.Scope{}
	if err := service.Start(context.Background(), serviceScope); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "nested", "settings.yaml")
	provider, err := New(service, Config{Path: path, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if provider.ID() != "settings-file" || provider.Path() != path {
		t.Fatal("identity")
	}
	document, err := provider.Load(context.Background())
	if err != nil || document.Route.Provider != "" {
		t.Fatalf("missing load=%#v err=%v", document, err)
	}
	providerScope := &plugin.Scope{}
	if err := provider.Start(context.Background(), providerScope); err != nil {
		t.Fatal(err)
	}
	resolved := appsettings.Defaults()
	resolved.Route = appsettings.Route{Provider: "anthropic", Model: "claude-sonnet-4-5"}
	if err := provider.Persist(context.Background(), resolved); err != nil {
		t.Fatal(err)
	}
	loaded, err := provider.Load(context.Background())
	if err != nil || loaded.Route.Provider != "anthropic" {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode=%v err=%v", info.Mode(), err)
	}
	changed := make(chan appsettings.Document, 1)
	dispose, err := service.Watch(func(document appsettings.Document) {
		if document.Route.Provider == "openrouter" {
			changed <- document
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer dispose()
	encoded := []byte("route:\n  provider: openrouter\n  model: openai/gpt-5.4\n")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("hot reload not published")
	}
	if err := providerScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := serviceScope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProviderValidationAndReadErrors(t *testing.T) {
	service := appsettings.New()
	if _, err := New(nil, Config{Path: "x"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil service=%v", err)
	}
	if _, err := New(service, Config{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("empty path=%v", err)
	}
	if _, err := New(service, Config{Path: "x", PollInterval: time.Millisecond}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("poll=%v", err)
	}
	for _, test := range []struct {
		name, content string
		mode          os.FileMode
	}{
		{"unknown", "unknown: true\n", 0o600},
		{"multiple", "---\n{}\n---\n{}\n", 0o600},
		{"invalid", "route:\n  provider: bad\n", 0o600},
		{"permissions", "{}\n", 0o644},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.yaml")
			if err := os.WriteFile(path, []byte(test.content), test.mode); err != nil {
				t.Fatal(err)
			}
			provider, _ := New(service, Config{Path: path})
			if _, err := provider.Load(context.Background()); err == nil {
				t.Fatal("bad file accepted")
			}
		})
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	link := filepath.Join(root, "link")
	if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	provider, _ := New(service, Config{Path: link})
	if _, err := provider.Load(context.Background()); err == nil {
		t.Fatal("symlink accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	provider, _ = New(service, Config{Path: filepath.Join(root, "missing")})
	if _, err := provider.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	if err := provider.Persist(context.Background(), appsettings.Document{}); err == nil {
		t.Fatal("invalid persist")
	}
}

func TestProviderWatchAndAtomicFailures(t *testing.T) {
	originalWait, originalRandom := settingsLockWait, randomRead
	t.Cleanup(func() { settingsLockWait, randomRead = originalWait, originalRandom })
	service := appsettings.New()
	path := filepath.Join(t.TempDir(), "settings.yaml")
	provider, _ := New(service, Config{Path: path, PollInterval: 10 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := provider.Watch(ctx, func(appsettings.Document, error) {}); err != nil {
		t.Fatal(err)
	}
	lock := path + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	settingsLockWait = time.Millisecond
	if err := withLock(context.Background(), lock, func() error { return nil }); err == nil {
		t.Fatal("timeout missing")
	}
	if err := withLock(ctx, lock, func() error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("operation")
	if err := withLock(context.Background(), lock, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("operation=%v", err)
	}
	randomRead = func([]byte) (int, error) { return 0, errors.New("random") }
	if err := atomicWrite(path, []byte("x")); err == nil {
		t.Fatal("random error missing")
	}
	provider.remember([]byte("x"), true)
	if provider.changed([]byte("x"), true) || !provider.changed([]byte("y"), true) || !provider.changed([]byte("x"), false) {
		t.Fatal("change detection")
	}
}

func TestFilesystemPersistReadAndWatchFailures(t *testing.T) {
	originalAbs, originalMarshal := settingsAbs, settingsMarshal
	originalMkdir, originalLstat := settingsMkdirAll, settingsLstat
	originalRead, originalRandom := settingsReadFile, randomRead
	t.Cleanup(func() {
		settingsAbs, settingsMarshal = originalAbs, originalMarshal
		settingsMkdirAll, settingsLstat = originalMkdir, originalLstat
		settingsReadFile, randomRead = originalRead, originalRandom
	})
	service := appsettings.New()
	settingsAbs = func(string) (string, error) { return "", errors.New("absolute") }
	if _, err := New(service, Config{Path: "x"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("absolute path=%v", err)
	}
	settingsAbs = originalAbs
	path := filepath.Join(t.TempDir(), "nested", "settings.yaml")
	provider, _ := New(service, Config{Path: path, PollInterval: 10 * time.Millisecond})
	document := appsettings.Defaults()
	settingsMarshal = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	if err := provider.Persist(context.Background(), document); err == nil {
		t.Fatal("marshal error missing")
	}
	settingsMarshal = func(any) ([]byte, error) { return make([]byte, maxSettingsBytes+1), nil }
	if err := provider.Persist(context.Background(), document); err == nil {
		t.Fatal("settings size error missing")
	}
	settingsMarshal = originalMarshal
	settingsMkdirAll = func(string, os.FileMode) error { return errors.New("mkdir") }
	if err := provider.Persist(context.Background(), document); err == nil {
		t.Fatal("mkdir error missing")
	}
	settingsMkdirAll = originalMkdir
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := provider.Persist(context.Background(), document); !errors.Is(err, ErrUnsafeFile) {
		t.Fatalf("symlink persist=%v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	settingsLstat = func(string) (os.FileInfo, error) { return nil, errors.New("lstat") }
	if err := provider.Persist(context.Background(), document); err == nil {
		t.Fatal("persist lstat error missing")
	}
	if _, _, _, err := provider.read(); err == nil {
		t.Fatal("read lstat error missing")
	}
	settingsLstat = originalLstat
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settingsReadFile = func(string) ([]byte, error) { return nil, errors.New("read") }
	if _, _, _, err := provider.read(); err == nil {
		t.Fatal("read file error missing")
	}
	settingsReadFile = originalRead
	randomRead = func([]byte) (int, error) { return 0, errors.New("random") }
	if err := provider.Persist(context.Background(), document); err == nil {
		t.Fatal("atomic persist error missing")
	}
	randomRead = originalRandom

	settingsLstat = func(string) (os.FileInfo, error) { return nil, errors.New("watch read") }
	ctx, cancel := context.WithCancel(context.Background())
	published := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		done <- provider.Watch(ctx, func(_ appsettings.Document, err error) { published <- err; cancel() })
	}()
	select {
	case err := <-published:
		if err == nil {
			t.Fatal("watch published nil error")
		}
	case <-time.After(time.Second):
		t.Fatal("watch read error not published")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAtomicWriteAndLockFailurePaths(t *testing.T) {
	originalOpen, originalRename := settingsOpenFile, settingsRename
	originalRemove, originalMarshal := settingsRemove, randomRead
	originalWait := settingsLockWait
	t.Cleanup(func() {
		settingsOpenFile, settingsRename = originalOpen, originalRename
		settingsRemove, randomRead, settingsLockWait = originalRemove, originalMarshal, originalWait
	})
	path := filepath.Join(t.TempDir(), "settings.yaml")
	randomRead = func(buffer []byte) (int, error) {
		for index := range buffer {
			buffer[index] = 1
		}
		return len(buffer), nil
	}
	settingsOpenFile = func(string, int, os.FileMode) (settingsFile, error) { return nil, errors.New("open") }
	if err := atomicWrite(path, []byte("x")); err == nil {
		t.Fatal("temp open error missing")
	}
	settingsOpenFile = func(string, int, os.FileMode) (settingsFile, error) { return &faultSettingsFile{writeN: 0}, nil }
	if err := atomicWrite(path, []byte("x")); err == nil {
		t.Fatal("short write error missing")
	}
	settingsOpenFile = func(string, int, os.FileMode) (settingsFile, error) {
		return &faultSettingsFile{writeErr: errors.New("write")}, nil
	}
	if err := atomicWrite(path, []byte("x")); err == nil {
		t.Fatal("write error missing")
	}
	settingsOpenFile = func(string, int, os.FileMode) (settingsFile, error) {
		return &faultSettingsFile{writeN: 1, syncErr: errors.New("sync")}, nil
	}
	if err := atomicWrite(path, []byte("x")); err == nil {
		t.Fatal("sync error missing")
	}
	settingsOpenFile = func(string, int, os.FileMode) (settingsFile, error) {
		return &faultSettingsFile{writeN: 1, closeErr: errors.New("close")}, nil
	}
	if err := atomicWrite(path, []byte("x")); err == nil {
		t.Fatal("close error missing")
	}
	settingsOpenFile = originalOpen
	settingsRename = func(string, string) error { return errors.New("rename") }
	if err := atomicWrite(path, []byte("x")); err == nil {
		t.Fatal("rename error missing")
	}
	settingsRename = originalRename

	settingsOpenFile = func(string, int, os.FileMode) (settingsFile, error) {
		return &faultSettingsFile{closeErr: errors.New("close")}, nil
	}
	if err := withLock(context.Background(), path+".lock", func() error { return nil }); err == nil {
		t.Fatal("lock close error missing")
	}
	settingsOpenFile = func(string, int, os.FileMode) (settingsFile, error) { return nil, errors.New("create") }
	if err := withLock(context.Background(), path+".lock", func() error { return nil }); err == nil {
		t.Fatal("lock create error missing")
	}
	settingsOpenFile = originalOpen
	lock := path + ".lock"
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	settingsLockWait = time.Second
	timer := time.AfterFunc(35*time.Millisecond, func() { _ = os.Remove(lock) })
	defer timer.Stop()
	if err := withLock(context.Background(), lock, func() error { return nil }); err != nil {
		t.Fatalf("lock retry=%v", err)
	}
}
