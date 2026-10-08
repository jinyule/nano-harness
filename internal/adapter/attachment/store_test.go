package attachment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	stdimage "image"
	"image/png"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// storeHooks is every filesystem seam the store uses.
type storeHooks struct {
	abs    func(string) (string, error)
	mkdirs func(string, fs.FileMode) error
	mkdir  func(string, fs.FileMode) error
	stat   func(string) (fs.FileInfo, error)
	lstat  func(string) (fs.FileInfo, error)
	link   func(string, string) error
	remove func(string) error
	chmod  func(string, fs.FileMode) error
	random func([]byte) (int, error)
	syncer func(string) error
	source func(string) (io.ReadCloser, error)
	object func(string) (io.ReadCloser, error)
	staged func(string) (stagedFile, error)
}

var productionHooks = storeHooks{absPath, makeDirs, makeDir, statPath, lstatPath, linkPath, removePath, chmodPath, readRandom, syncDir, openSource, openObject, createStaged}

func applyHooks(hooks storeHooks) {
	absPath, makeDirs, makeDir, statPath, lstatPath, linkPath, removePath, chmodPath, readRandom, syncDir, openSource, openObject, createStaged = hooks.abs, hooks.mkdirs, hooks.mkdir, hooks.stat, hooks.lstat, hooks.link, hooks.remove, hooks.chmod, hooks.random, hooks.syncer, hooks.source, hooks.object, hooks.staged
}

// restoreStoreHooks resets every seam to production now and again when the
// test ends.
func restoreStoreHooks(t *testing.T) {
	t.Helper()
	applyHooks(productionHooks)
	t.Cleanup(func() { applyHooks(productionHooks) })
}

// startStore starts a store over a fresh private root and closes it when the
// test ends.
func startStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "attachments")
	store, err := New(Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := store.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := scope.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return store, root
}

func objectPath(root string, ref session.Image) string {
	digest, _ := session.ImageDigest(ref.ID)
	return filepath.Join(root, "v1", "objects", digest[:2], digest)
}

func stagingEntries(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "v1", "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}

func TestStore_LifecycleAndPrivateLayout(t *testing.T) {
	if _, err := New(Config{Root: "relative"}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("relative root = %v", err)
	}
	store, root := startStore(t)
	if store.ID() != "attachments" {
		t.Fatal("wrong plugin ID")
	}
	if err := store.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("second start = %v", err)
	}
	if runtime.GOOS != "windows" {
		for _, directory := range []string{root, filepath.Join(root, "v1"), filepath.Join(root, "v1", "objects"), filepath.Join(root, "v1", "tmp")} {
			if info, err := os.Lstat(directory); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
				t.Fatalf("%s = %v %v", directory, info, err)
			}
		}
	}

	// Operations end with the scope, and cleanup waits for running ones.
	other, err := New(Config{Root: filepath.Join(t.TempDir(), "store")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.SaveImage(context.Background(), "x.png", encodedPNG(t, 2, 2)); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("before start = %v", err)
	}
	scope := &plugin.Scope{}
	if err := other.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if err := other.begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- scope.Close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("cleanup returned with an operation running: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err := other.ReadImage(context.Background(), session.Image{ID: session.ImageID(strings.Repeat("0", 64))}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("read while stopping = %v", err)
	}
	other.end()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}

	// A cleanup that cannot wait reports the deadline.
	late, err := New(Config{Root: filepath.Join(t.TempDir(), "late")})
	if err != nil {
		t.Fatal(err)
	}
	lateScope := &plugin.Scope{}
	if err := late.Start(context.Background(), lateScope); err != nil {
		t.Fatal(err)
	}
	if err := late.begin(context.Background()); err != nil {
		t.Fatal(err)
	}
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lateScope.Close(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired cleanup = %v", err)
	}
	late.end()
	ctx, cancelBegin := context.WithCancel(context.Background())
	cancelBegin()
	if err := store.begin(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled begin = %v", err)
	}
}

func TestStore_StartRefusesUnsafeDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits and links describe the private-directory rule")
	}
	wide := filepath.Join(t.TempDir(), "wide")
	if err := os.Mkdir(wide, 0o755); err != nil { //nolint:gosec // the test needs a directory other users can read
		t.Fatal(err)
	}
	linked := t.TempDir()
	if err := os.Chmod(linked, 0o700); err != nil { //nolint:gosec // directories require owner execute permission
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(linked, "v1")); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, root := range map[string]string{"wide root": wide, "linked v1": linked, "file root": file} {
		store, err := New(Config{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Start(context.Background(), &plugin.Scope{}); err == nil {
			t.Errorf("%s started", name)
		}
	}
	// A root that is itself a link to a private directory is accepted.
	target := filepath.Join(t.TempDir(), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	store, err := New(Config{Root: link})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := store.Start(context.Background(), scope); err != nil {
		t.Fatalf("linked root = %v", err)
	}
	_ = scope.Close(context.Background())
}

func TestStore_StartReportsFilesystemFailures(t *testing.T) {
	restoreStoreHooks(t)
	failure := errors.New("filesystem")
	for name, set := range map[string]func(){
		"root":  func() { makeDirs = func(string, fs.FileMode) error { return failure } },
		"stat":  func() { statPath = func(string) (fs.FileInfo, error) { return nil, failure } },
		"mkdir": func() { makeDir = func(string, fs.FileMode) error { return failure } },
		"sync":  func() { syncDir = func(string) error { return failure } },
	} {
		restoreStoreHooks(t)
		set()
		store, _ := New(Config{Root: filepath.Join(t.TempDir(), "store")})
		if err := store.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, failure) {
			t.Errorf("%s = %v", name, err)
		}
	}
	restoreStoreHooks(t)
	store, _ := New(Config{Root: filepath.Join(t.TempDir(), "store")})
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := store.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope = %v", err)
	}
}

func TestStore_SaveReadAndDeduplicate(t *testing.T) {
	store, root := startStore(t)
	source := encodedPNG(t, 6, 4)
	ref, size, err := store.SaveImage(context.Background(), "shot.png", source)
	if err != nil || size != stdimage.Pt(6, 4) || ref.Name != "shot.png" || ref.Width != 6 || ref.MediaType != "image/jpeg" {
		t.Fatalf("save = %#v %v %v", ref, size, err)
	}
	stored, err := os.ReadFile(objectPath(root, ref))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(stored)
	if ref.ID != session.ImageID(hex.EncodeToString(digest[:])) || ref.Bytes != len(stored) {
		t.Fatalf("object does not match its reference: %#v", ref)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Lstat(objectPath(root, ref)); err != nil || info.Mode().Perm() != 0o400 {
			t.Fatalf("object mode = %v %v", info, err)
		}
		if info, err := os.Lstat(filepath.Dir(objectPath(root, ref))); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("prefix mode = %v %v", info, err)
		}
	}
	read, err := store.ReadImage(context.Background(), ref)
	if err != nil || string(read) != string(stored) {
		t.Fatalf("read = %d bytes %v", len(read), err)
	}
	// The same image under another name shares the object.
	again, _, err := store.SaveImage(context.Background(), "copy.png", source)
	if err != nil || again.ID != ref.ID || again.Name != "copy.png" {
		t.Fatalf("dedup = %#v %v", again, err)
	}
	if entries := stagingEntries(t, root); len(entries) != 0 {
		t.Fatalf("staging residue = %v", entries)
	}
	for _, bad := range [][]byte{nil, make([]byte, maxSourceBytes+1)} {
		if _, _, err := store.SaveImage(context.Background(), "x.png", bad); !errors.Is(err, ErrInvalidImage) {
			t.Fatalf("source of %d bytes = %v", len(bad), err)
		}
	}
	if _, size, err := store.SaveImage(context.Background(), "big.png", pngHeader(4001, 4000)); !errors.Is(err, session.ErrImagePixels) || size != stdimage.Pt(4001, 4000) {
		t.Fatalf("pixel refusal = %v %v", size, err)
	}
}

func TestStore_ConcurrentWritersShareOneObject(t *testing.T) {
	store, root := startStore(t)
	source := encodedPNG(t, 8, 8)
	const writers = 8
	refs := make([]session.Image, writers)
	errs := make([]error, writers)
	var group sync.WaitGroup
	for index := range writers {
		group.Go(func() { refs[index], _, errs[index] = store.SaveImage(context.Background(), "same.png", source) })
	}
	group.Wait()
	for index := range writers {
		if errs[index] != nil || refs[index].ID != refs[0].ID {
			t.Fatalf("writer %d = %#v %v", index, refs[index], errs[index])
		}
	}
	entries, err := os.ReadDir(filepath.Dir(objectPath(root, refs[0])))
	if err != nil || len(entries) != 1 {
		t.Fatalf("objects = %v %v", entries, err)
	}
	if entries := stagingEntries(t, root); len(entries) != 0 {
		t.Fatalf("staging residue = %v", entries)
	}
}

func TestStore_ReadVerifiesTheObject(t *testing.T) {
	store, root := startStore(t)
	ref, _, err := store.SaveImage(context.Background(), "shot.png", encodedPNG(t, 6, 4))
	if err != nil {
		t.Fatal(err)
	}
	path := objectPath(root, ref)
	original, err := os.ReadFile(path) //nolint:gosec // the path is derived from the test-owned store root
	if err != nil {
		t.Fatal(err)
	}
	replace := func(data []byte) {
		t.Helper()
		_ = os.Chmod(path, 0o600)
		if err := os.WriteFile(path, data, 0o600); err != nil { //nolint:gosec // the path is derived from the test-owned store root
			t.Fatal(err)
		}
	}
	expect := func(name string, ref session.Image, want error) {
		t.Helper()
		if _, err := store.ReadImage(context.Background(), ref); !errors.Is(err, want) {
			t.Errorf("%s = %v, want %v", name, err, want)
		}
	}
	other := ref
	other.Width = 7
	expect("dimensions", other, session.ErrAttachmentCorrupt)
	other = ref
	other.MediaType = "image/png"
	expect("media type", other, session.ErrAttachmentCorrupt)
	other = ref
	other.Bytes++
	expect("length", other, session.ErrAttachmentCorrupt)
	expect("malformed ID", session.Image{ID: "img-1"}, session.ErrAttachmentCorrupt)
	expect("missing prefix", session.Image{ID: session.ImageID(strings.Repeat("f", 64)), Bytes: 1}, session.ErrAttachmentMissing)
	replace(append(slices.Clone(original[:len(original)-1]), original[len(original)-1]^0xff))
	expect("changed byte", ref, session.ErrAttachmentCorrupt)
	digest, _ := session.ImageDigest(ref.ID)
	garbage := []byte("not an image at all")
	sum := sha256.Sum256(garbage)
	garbageRef := session.Image{ID: session.ImageID(hex.EncodeToString(sum[:])), Name: "g", MediaType: "image/jpeg", Bytes: len(garbage), Width: 1, Height: 1}
	garbagePath := objectPath(root, garbageRef)
	if err := os.MkdirAll(filepath.Dir(garbagePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(garbagePath, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	expect("undecodable", garbageRef, session.ErrAttachmentCorrupt)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	expect("missing object", ref, session.ErrAttachmentMissing)
	if runtime.GOOS != "windows" {
		elsewhere := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(elsewhere, original, 0o600); err != nil { //nolint:gosec // the path is in this test's private temporary directory
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, path); err != nil {
			t.Fatal(err)
		}
		expect("linked object", ref, session.ErrAttachmentCorrupt)
	}
	prefix := filepath.Join(root, "v1", "objects", digest[:2])
	if err := os.RemoveAll(prefix); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prefix, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	expect("prefix file", ref, session.ErrAttachmentCorrupt)
}

func TestStore_ReadReportsIOFailures(t *testing.T) {
	store, root := startStore(t)
	ref, _, err := store.SaveImage(context.Background(), "shot.png", encodedPNG(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	restoreStoreHooks(t)
	failure := errors.New("io")
	prefix := filepath.Dir(objectPath(root, ref))
	for name, set := range map[string]func(){
		"prefix stat": func() {
			lstatPath = func(path string) (fs.FileInfo, error) {
				if path == prefix {
					return nil, failure
				}
				return os.Lstat(path)
			}
		},
		"object stat": func() {
			lstatPath = func(path string) (fs.FileInfo, error) {
				if path != prefix {
					return nil, failure
				}
				return os.Lstat(path)
			}
		},
		"open": func() { openObject = func(string) (io.ReadCloser, error) { return nil, failure } },
		"read": func() { openObject = func(string) (io.ReadCloser, error) { return failingReader{err: failure}, nil } },
	} {
		restoreStoreHooks(t)
		set()
		if _, err := store.ReadImage(context.Background(), ref); !errors.Is(err, failure) || errors.Is(err, session.ErrAttachmentCorrupt) || errors.Is(err, session.ErrAttachmentMissing) {
			t.Errorf("%s = %v", name, err)
		}
	}
}

func TestStore_PublishFailuresLeaveNoPartialObject(t *testing.T) {
	failure := errors.New("publish")
	cases := map[string]func(){
		"prefix": func() { makeDir = func(string, fs.FileMode) error { return failure } },
		"random": func() { readRandom = func([]byte) (int, error) { return 0, failure } },
		"create": func() { createStaged = func(string) (stagedFile, error) { return nil, failure } },
		"write": func() {
			createStaged = func(path string) (stagedFile, error) { return failingStaged{path: path, write: failure}, nil }
		},
		"sync": func() {
			createStaged = func(path string) (stagedFile, error) { return failingStaged{path: path, sync: failure}, nil }
		},
		"link":    func() { linkPath = func(string, string) error { return failure } },
		"chmod":   func() { chmodPath = func(string, fs.FileMode) error { return failure } },
		"dirsync": func() { syncDir = func(string) error { return failure } },
		"remove": func() {
			removePath = func(path string) error {
				_ = os.Remove(path)
				return failure
			}
		},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			store, root := startStore(t)
			restoreStoreHooks(t)
			set()
			ref, _, err := store.SaveImage(context.Background(), "shot.png", encodedPNG(t, 3, 3))
			if !errors.Is(err, failure) {
				t.Fatalf("save = %v", err)
			}
			restoreStoreHooks(t)
			if entries := stagingEntries(t, root); len(entries) != 0 {
				t.Fatalf("staging residue = %v", entries)
			}
			if name == "chmod" || name == "dirsync" || name == "remove" || ref.ID == "" {
				return
			}
			if _, err := os.Lstat(objectPath(root, ref)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("partial object = %v", err)
			}
		})
	}
}

// failingStaged is a staged file whose write or sync fails; it creates the
// real file so the store's cleanup can be observed.
type failingStaged struct {
	path        string
	write, sync error
}

func (file failingStaged) Write(data []byte) (int, error) {
	if err := os.WriteFile(file.path, data, 0o600); err != nil {
		return 0, err
	}
	if file.write != nil {
		return 0, file.write
	}
	return len(data), nil
}
func (file failingStaged) Sync() error { return file.sync }
func (failingStaged) Close() error     { return nil }

func TestStore_ExistingObjectMustMatch(t *testing.T) {
	store, root := startStore(t)
	source := encodedPNG(t, 5, 5)
	prepared, err := normalize(context.Background(), "shot.png", source)
	if err != nil {
		t.Fatal(err)
	}
	path := objectPath(root, prepared.ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("planted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SaveImage(context.Background(), "shot.png", source); !errors.Is(err, session.ErrAttachmentCorrupt) {
		t.Fatalf("planted object = %v", err)
	}
	if entries := stagingEntries(t, root); len(entries) != 0 {
		t.Fatalf("staging residue = %v", entries)
	}
}

func TestStore_PrepareFileValidatesTheChosenFileAndCommitStoresIt(t *testing.T) {
	store, root := startStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.PrepareFile(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	directory := t.TempDir()
	empty := filepath.Join(directory, "empty.png")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(directory, "large.png")
	if err := os.WriteFile(large, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(large, maxSourceBytes+1); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(directory, "image.png")
	if err := os.WriteFile(valid, encodedPNG(t, 2, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	text := filepath.Join(directory, "notes.png")
	if err := os.WriteFile(text, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"empty path": "", "missing": filepath.Join(directory, "missing"), "directory": directory, "empty file": empty, "large file": large, "not an image": text} {
		if _, _, err := store.PrepareFile(context.Background(), path); !errors.Is(err, ErrInvalidImage) {
			t.Errorf("%s = %v", name, err)
		}
	}
	if runtime.GOOS != "windows" {
		link := filepath.Join(directory, "link.png")
		if err := os.Symlink(valid, link); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.PrepareFile(context.Background(), link); !errors.Is(err, ErrInvalidImage) {
			t.Fatalf("link = %v", err)
		}
	}
	restoreStoreHooks(t)
	absPath = func(string) (string, error) { return "", errors.New("absolute") }
	if _, _, err := store.PrepareFile(context.Background(), "x"); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("absolute = %v", err)
	}
	restoreStoreHooks(t)
	openSource = func(string) (io.ReadCloser, error) { return nil, errors.New("open") }
	if _, _, err := store.PrepareFile(context.Background(), valid); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("open = %v", err)
	}
	openSource = func(string) (io.ReadCloser, error) { return failingReader{err: errors.New("read")}, nil }
	if _, _, err := store.PrepareFile(context.Background(), valid); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("read = %v", err)
	}
	restoreStoreHooks(t)
	ref, data, err := store.PrepareFile(context.Background(), valid)
	if err != nil || ref.Name != "image.png" || ref.Width != 2 || len(data) != ref.Bytes {
		t.Fatalf("prepare = %#v %v", ref, err)
	}
	// Preparing stores nothing, so an attachment that is never sent leaves
	// no object behind.
	if _, err := os.Lstat(objectPath(root, ref)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("prepared object = %v", err)
	}
	tampered := append([]byte(nil), data...)
	tampered[len(tampered)-1] ^= 0xff
	for name, attempt := range map[string]func() error{
		"invalid reference": func() error { return store.Commit(context.Background(), session.Image{ID: "img"}, data) },
		"changed bytes":     func() error { return store.Commit(context.Background(), ref, tampered) },
	} {
		if err := attempt(); !errors.Is(err, ErrInvalidImage) {
			t.Errorf("%s = %v", name, err)
		}
	}
	canceled, cancelCommit := context.WithCancel(context.Background())
	cancelCommit()
	if err := store.Commit(canceled, ref, data); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled commit = %v", err)
	}
	if err := store.Commit(context.Background(), ref, data); err != nil {
		t.Fatal(err)
	}
	if read, err := store.ReadImage(context.Background(), ref); err != nil || string(read) != string(data) {
		t.Fatalf("committed object = %v", err)
	}
	if err := (session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "test"}, Content: []session.ContentBlock{{Type: session.ContentImage, Image: &ref}}}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSyncDirectory_ReportsMissingDirectories(t *testing.T) {
	if err := syncDirectory(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := syncDirectory(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing = %v", err)
	}
}

func TestStore_ObserversSeeOnlyUnavailableImagesWhileRegistered(t *testing.T) {
	store, root := startStore(t)
	if err := store.ObserveUnavailable(nil, &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil observer = %v", err)
	}
	if err := store.ObserveUnavailable(func(session.Image, error) {}, nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil scope = %v", err)
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := store.ObserveUnavailable(func(session.Image, error) {}, closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope = %v", err)
	}
	var seen []error
	scope := &plugin.Scope{}
	if err := store.ObserveUnavailable(func(_ session.Image, err error) { seen = append(seen, err) }, scope); err != nil {
		t.Fatal(err)
	}
	ref, _, err := store.SaveImage(context.Background(), "shot.png", encodedPNG(t, 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadImage(context.Background(), ref); err != nil || len(seen) != 0 {
		t.Fatalf("a verified read reported %v (%v)", seen, err)
	}
	if _, err := store.ReadImage(context.Background(), session.Image{ID: "img"}); !errors.Is(err, session.ErrAttachmentCorrupt) {
		t.Fatal(err)
	}
	if err := os.Remove(objectPath(root, ref)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadImage(context.Background(), ref); !errors.Is(err, session.ErrAttachmentMissing) {
		t.Fatal(err)
	}
	if _, err := store.ReadImage(context.Background(), session.Image{ID: session.ImageID(strings.Repeat("e", 64))}); !errors.Is(err, session.ErrAttachmentMissing) {
		t.Fatal(err)
	}
	if len(seen) != 3 || !errors.Is(seen[0], session.ErrAttachmentCorrupt) || !errors.Is(seen[1], session.ErrAttachmentMissing) || !errors.Is(seen[2], session.ErrAttachmentMissing) {
		t.Fatalf("observed = %v", seen)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, _ = store.ReadImage(context.Background(), ref)
	if len(seen) != 3 {
		t.Fatal("an observer outlived its scope")
	}
}

// transparentPNG encodes a fully transparent image of the given size.
func transparentPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, stdimage.NewNRGBA(stdimage.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func assertWhite(t *testing.T, name string, data []byte) {
	t.Helper()
	decoded, _, err := stdimage.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	bounds := decoded.Bounds()
	for _, point := range []stdimage.Point{bounds.Min, {X: bounds.Dx() / 2, Y: bounds.Dy() / 2}, {X: bounds.Max.X - 1, Y: bounds.Max.Y - 1}} {
		if red, green, blue, _ := decoded.At(point.X, point.Y).RGBA(); red>>8 < 240 || green>>8 < 240 || blue>>8 < 240 {
			t.Fatalf("%s pixel %v = (%d,%d,%d), want white", name, point, red>>8, green>>8, blue>>8)
		}
	}
}

// TestStore_TransparentImagesStayWhiteWhenScaled covers the white-background
// contract on both write paths, with and without downscaling.
func TestStore_TransparentImagesStayWhiteWhenScaled(t *testing.T) {
	store, root := startStore(t)
	directory := t.TempDir()
	for _, size := range []stdimage.Point{{X: 16, Y: 2}, {X: 4096, Y: 2}} {
		source := transparentPNG(t, size.X, size.Y)
		saved, _, err := store.SaveImage(context.Background(), "clear.png", source)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := os.ReadFile(objectPath(root, saved))
		if err != nil {
			t.Fatal(err)
		}
		assertWhite(t, "read_image", stored)
		path := filepath.Join(directory, "clear.png")
		if err := os.WriteFile(path, source, 0o600); err != nil {
			t.Fatal(err)
		}
		attached, data, err := store.PrepareFile(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		assertWhite(t, "/attach", data)
		if size.X > maxDimension && (attached.Width != maxDimension || saved.Width != maxDimension) {
			t.Fatalf("scaled widths = %d and %d", attached.Width, saved.Width)
		}
	}
}

// TestStore_LimitsConcurrentConversions proves that at most two images are
// decoded and scaled at once and that a waiting caller can be cancelled.
func TestStore_LimitsConcurrentConversions(t *testing.T) {
	store, _ := startStore(t)
	original := encodeImage
	t.Cleanup(func() { encodeImage = original })
	var mu sync.Mutex
	running, peak := 0, 0
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	// A failing assertion still unblocks the conversions, so the store's
	// cleanup, which waits for them, can finish.
	t.Cleanup(releaseAll)
	encodeImage = func(source stdimage.Image, quality int) ([]byte, error) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		entered <- struct{}{}
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return original(source, quality)
	}
	var group sync.WaitGroup
	errs := make([]error, 3)
	for index := range 3 {
		group.Go(func() {
			_, _, errs[index] = store.SaveImage(context.Background(), "shot.png", encodedPNG(t, 4+index, 4))
		})
	}
	<-entered
	<-entered
	select {
	case <-entered:
		t.Fatal("a third conversion started while two were running")
	case <-time.After(50 * time.Millisecond):
	}
	// Both tokens are held, so a caller must wait and gives up when its
	// context ends.
	waiting, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.convert(waiting, "late.png", encodedPNG(t, 3, 3)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = %v", err)
	}
	releaseAll()
	group.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("conversion %d = %v", index, err)
		}
	}
	if peak != maxConversions {
		t.Fatalf("peak conversions = %d, want %d", peak, maxConversions)
	}
}

// TestStore_FailedObserverRegistrationLeavesNoCallback proves that a
// registration whose cleanup cannot be scheduled is rolled back: the
// observer never runs.
func TestStore_FailedObserverRegistrationLeavesNoCallback(t *testing.T) {
	store, _ := startStore(t)
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	calls := 0
	if err := store.ObserveUnavailable(func(session.Image, error) { calls++ }, closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope = %v", err)
	}
	if _, err := store.ReadImage(context.Background(), session.Image{ID: session.ImageID(strings.Repeat("d", 64)), Bytes: 1}); !errors.Is(err, session.ErrAttachmentMissing) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("failed registration still received %d callbacks", calls)
	}
}

// TestStore_ObserverCleanupWaitsForRunningCallbacks proves quiescence: once
// an observer's scope cleanup returns, no callback is running or can start,
// even when a read captured the observer just before cleanup began.
func TestStore_ObserverCleanupWaitsForRunningCallbacks(t *testing.T) {
	store, _ := startStore(t)
	entered := make(chan struct{})
	var firstCall sync.Once
	release := make(chan struct{})
	// A failing assertion still releases blocked callbacks, so the store's
	// cleanup, which runs after these, can drain the reads.
	releaseAll := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseAll)
	var mu sync.Mutex
	running, after := false, 0
	stopped := false
	scope := &plugin.Scope{}
	if err := store.ObserveUnavailable(func(session.Image, error) {
		mu.Lock()
		if stopped {
			after++
		}
		running = true
		mu.Unlock()
		// Only the first callback blocks; closing entered signals it even
		// when the test has not started waiting yet.
		blocking := false
		firstCall.Do(func() { blocking = true })
		if blocking {
			close(entered)
			<-release
		}
		mu.Lock()
		running = false
		mu.Unlock()
	}, scope); err != nil {
		t.Fatal(err)
	}
	missing := session.Image{ID: session.ImageID(strings.Repeat("c", 64)), Bytes: 1}
	read := make(chan error, 1)
	go func() {
		_, err := store.ReadImage(context.Background(), missing)
		read <- err
	}()
	<-entered
	closed := make(chan error, 1)
	go func() { closed <- scope.Close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("observer cleanup returned while its callback ran: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseAll()
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	stopped = true
	stillRunning := running
	mu.Unlock()
	if stillRunning {
		t.Fatal("a callback was running after cleanup returned")
	}
	if err := <-read; !errors.Is(err, session.ErrAttachmentMissing) {
		t.Fatal(err)
	}
	_, _ = store.ReadImage(context.Background(), missing)
	mu.Lock()
	defer mu.Unlock()
	if after != 0 {
		t.Fatalf("%d callbacks ran after cleanup returned", after)
	}

	// A cleanup that cannot wait reports its deadline.
	late := &plugin.Scope{}
	block := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(block) })
	t.Cleanup(unblock)
	hold := make(chan struct{})
	if err := store.ObserveUnavailable(func(session.Image, error) {
		close(hold)
		<-block
	}, late); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = store.ReadImage(context.Background(), missing) }()
	<-hold
	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := late.Close(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired cleanup = %v", err)
	}
	unblock()
}
