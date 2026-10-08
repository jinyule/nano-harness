// Package spill stores complete tool output in private files on this machine,
// mirroring upstream's local spill backend. Artifacts live under
// <root>/workspace-<hash>/session-<hash>/<random>-<name>; read and grep may
// open them through the workspace's read-only spill allowance.
package spill

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

const (
	// maxArtifactBytes bounds one artifact; larger content is refused and
	// the caller keeps its bounded inline result.
	maxArtifactBytes = 64 << 20
	// retention is upstream's default age after which the startup sweep
	// deletes an artifact. Resumed sessions keep their locators until then.
	retention = 30 * 24 * time.Hour
	// createAttempts bounds retries when the sweep prunes a session
	// directory between mkdir and create, or a random name collides.
	createAttempts = 3
	// retrievalHint tells the model how to use a local locator.
	retrievalHint = "Use read with offset/limit, or grep this path to search within it."
)

var (
	// ErrInvalidConfig identifies spill configuration that cannot be honored.
	ErrInvalidConfig = errors.New("invalid spill configuration")
	// ErrClosed reports a create after the store stopped or before it started.
	ErrClosed = errors.New("spill store is not running")
	// ErrTooLarge reports content beyond the artifact limit.
	ErrTooLarge = errors.New("spill artifact exceeds the size limit")

	safeName       = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	workspaceEntry = regexp.MustCompile(`^workspace-[0-9a-f]{16}$`)
	sessionEntry   = regexp.MustCompile(`^session-[0-9a-f]{12}$`)

	makeDirs   = os.MkdirAll
	statPath   = os.Stat
	lstatPath  = os.Lstat
	readDir    = os.ReadDir
	removePath = os.Remove
	readRandom = rand.Read
	now        = time.Now
	openFile   = func(path string) (artifactFile, error) {
		return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the path is derived from the private root and a validated name
	}
)

// artifactFile is the subset of *os.File one artifact uses.
type artifactFile interface {
	io.Writer
	Sync() error
	Close() error
}

// Config selects the private spill root and the workspace whose artifacts
// this store owns. Both must be absolute; the workspace selects a partition
// so one workspace's sessions never see another's artifacts.
type Config struct {
	Root      string
	Workspace string
}

// Store is the spill-local plugin. It validates its private directories,
// publishes itself to the tool runtime, and runs one startup sweep that
// deletes artifacts older than the retention period.
type Store struct {
	runtime   *appTool.Runtime
	root      string
	partition string

	// swept closes when the startup sweep returns.
	swept chan struct{}
	// layout orders session-directory creation against sweep pruning.
	layout sync.Mutex

	mu      sync.Mutex
	started bool
	running bool
	open    int
	idle    chan struct{}
}

// New constructs an inert store. It does not touch the filesystem.
func New(runtime *appTool.Runtime, config Config) (*Store, error) {
	if runtime == nil || !filepath.IsAbs(config.Root) || !filepath.IsAbs(config.Workspace) {
		return nil, ErrInvalidConfig
	}
	root := filepath.Clean(config.Root)
	sum := sha256.Sum256([]byte(filepath.Clean(config.Workspace)))
	partition := filepath.Join(root, "workspace-"+hex.EncodeToString(sum[:8]))
	return &Store{runtime: runtime, root: root, partition: partition}, nil
}

// ID returns the stable plugin identity.
func (*Store) ID() string { return "spill-local" }

// Dir is the workspace partition that holds every artifact this store
// creates. Tools may grant read-only access to it.
func (store *Store) Dir() string { return store.partition }

// Start prepares the private root and partition, starts the sweep, and
// publishes the store until scope cleanup. Cleanup runs in reverse: it first
// withdraws the store from the runtime so new tool calls get none, then
// refuses new artifacts from calls that still hold it and waits for open
// ones to commit or discard, and finally cancels and joins the sweep.
func (store *Store) Start(ctx context.Context, scope *plugin.Scope) error {
	store.mu.Lock()
	if store.started {
		store.mu.Unlock()
		return ErrInvalidConfig
	}
	store.started = true
	store.mu.Unlock()
	if err := preparePrivate(store.root, statPath); err != nil {
		return err
	}
	if err := preparePrivate(store.partition, lstatPath); err != nil {
		return err
	}
	cutoff := now().Add(-retention)
	sweepContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	store.swept = done
	go func() {
		defer close(done)
		sweep(sweepContext, store.root, store.partition, cutoff, &store.layout)
	}()
	// One cleanup drains before it stops the sweep, so the order between
	// the two cannot depend on registration order.
	if err := scope.Defer(func(ctx context.Context) error {
		err := store.stop(ctx)
		cancel()
		<-done
		return err
	}); err != nil {
		cancel()
		<-done
		return err
	}
	store.mu.Lock()
	store.running = true
	store.mu.Unlock()
	return store.runtime.UseSpill(store, scope)
}

// preparePrivate creates dir owner-only and rejects anything that is not a
// private directory. inspect is Stat for the configured root, which may be a
// link, and Lstat for directories the store derives itself.
func preparePrivate(dir string, inspect func(string) (fs.FileInfo, error)) error {
	if err := makeDirs(dir, 0o700); err != nil {
		return fmt.Errorf("create spill directory %s: %w", dir, err)
	}
	info, err := inspect(dir)
	if err != nil {
		return fmt.Errorf("inspect spill directory %s: %w", dir, err)
	}
	if !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s must be a private directory", ErrInvalidConfig, dir)
	}
	return nil
}

// stop refuses new artifacts and waits until every open artifact has been
// committed or discarded, or ctx ends.
func (store *Store) stop(ctx context.Context) error {
	store.mu.Lock()
	store.running = false
	if store.open == 0 {
		store.mu.Unlock()
		return nil
	}
	if store.idle == nil {
		store.idle = make(chan struct{})
	}
	idle := store.idle
	store.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for open spill artifacts: %w", ctx.Err())
	}
}

func (store *Store) release() {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.open--
	if store.open == 0 && store.idle != nil {
		close(store.idle)
		store.idle = nil
	}
}

// Create opens a fresh owner-only artifact for sessionID. The file name pairs
// an unpredictable prefix with the validated name hint, and the exclusive
// create refuses any existing entry, including a planted symbolic link.
func (store *Store) Create(ctx context.Context, sessionID, name string) (appTool.SpillFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("create spill artifact: %w", err)
	}
	if sessionID == "" || !safeName.MatchString(name) || name == "." || name == ".." {
		return nil, fmt.Errorf("%w: invalid session or artifact name", ErrInvalidConfig)
	}
	store.mu.Lock()
	if !store.running {
		store.mu.Unlock()
		return nil, ErrClosed
	}
	store.open++
	store.mu.Unlock()
	sum := sha256.Sum256([]byte(sessionID))
	dir := filepath.Join(store.partition, "session-"+hex.EncodeToString(sum[:6]))
	var lastErr error
	for range createAttempts {
		var random [6]byte
		if _, err := readRandom(random[:]); err != nil {
			store.release()
			return nil, fmt.Errorf("create spill artifact: %w", err)
		}
		path := filepath.Join(dir, hex.EncodeToString(random[:])+"-"+name)
		file, err := store.createIn(dir, path)
		if err == nil {
			return &artifact{store: store, file: file, path: path}, nil
		}
		lastErr = err
		if errors.Is(err, errDirectory) || !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrExist) {
			break
		}
	}
	store.release()
	return nil, fmt.Errorf("create spill artifact: %w", lastErr)
}

// errDirectory marks a failure to prepare the session directory, which a
// retry cannot fix.
var errDirectory = errors.New("cannot create the session directory")

// createIn prepares a private, real directory and exclusively opens path under
// the layout lock, so this process's sweep cannot prune dir in between.
func (store *Store) createIn(dir, path string) (artifactFile, error) {
	store.layout.Lock()
	defer store.layout.Unlock()
	if err := preparePrivate(dir, lstatPath); err != nil {
		return nil, fmt.Errorf("%w: %w", errDirectory, err)
	}
	return openFile(path)
}

// artifact is one open spill file; it holds the store open until Commit or
// Discard.
type artifact struct {
	store  *Store
	file   artifactFile
	path   string
	bytes  int
	err    error
	closed bool
}

// Locator returns the artifact's absolute path, valid from creation.
func (artifact *artifact) Locator() string { return artifact.path }

// Write appends data. Content beyond the artifact limit fails the artifact;
// the caller must then Discard it.
func (artifact *artifact) Write(data []byte) (int, error) {
	if artifact.err != nil {
		return 0, artifact.err
	}
	if artifact.bytes+len(data) > maxArtifactBytes {
		artifact.err = fmt.Errorf("%w (%d bytes)", ErrTooLarge, maxArtifactBytes)
		return 0, artifact.err
	}
	written, err := artifact.file.Write(data)
	artifact.bytes += written
	if err != nil {
		artifact.err = err
	}
	return written, err
}

// Commit syncs and closes the artifact and returns its locator. A failed
// commit removes the partial file.
func (artifact *artifact) Commit() (appTool.SpillRef, error) {
	if artifact.closed {
		return appTool.SpillRef{}, ErrClosed
	}
	err := artifact.err
	if err == nil {
		err = artifact.file.Sync()
	}
	if err != nil {
		return appTool.SpillRef{}, errors.Join(err, artifact.Discard())
	}
	artifact.closed = true
	defer artifact.store.release()
	if err := artifact.file.Close(); err != nil {
		return appTool.SpillRef{}, errors.Join(err, removePath(artifact.path))
	}
	return appTool.SpillRef{Locator: artifact.path, Bytes: artifact.bytes, Hint: retrievalHint}, nil
}

// Discard closes and removes the artifact. It is a no-op after Commit or an
// earlier Discard.
func (artifact *artifact) Discard() error {
	if artifact.closed {
		return nil
	}
	artifact.closed = true
	defer artifact.store.release()
	return errors.Join(artifact.file.Close(), removePath(artifact.path))
}
