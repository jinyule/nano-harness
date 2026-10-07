// Package attachment normalizes PNG, JPEG, WebP, and GIF images and keeps the
// normalized bytes in an owner-only, content-addressed store on this machine,
// mirroring upstream's attachment-local backend. Sessions hold only the
// returned references; provider requests read the bytes back through the
// store, which verifies them against the reference on every read.
//
// Objects live at <root>/v1/objects/<sha256[:2]>/<sha256>. A write stages the
// bytes in <root>/v1/tmp, syncs them, publishes them with an exclusive hard
// link, makes the object read-only, and syncs the directories, so a returned
// reference always names durable bytes. Identical bytes share one object.
// Nothing is deleted automatically.
package attachment

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	stdimage "image"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

var (
	// ErrInvalidImage identifies an image that cannot satisfy the normalized image contract.
	ErrInvalidImage = errors.New("invalid image attachment")
	// ErrNotRunning indicates the store has not started or has stopped.
	ErrNotRunning = errors.New("attachment store is not running")
	// ErrInvalidConfig identifies store configuration that cannot be honored.
	ErrInvalidConfig = errors.New("invalid attachment store configuration")

	absPath    = filepath.Abs
	makeDirs   = os.MkdirAll
	makeDir    = os.Mkdir
	statPath   = os.Stat
	lstatPath  = os.Lstat
	linkPath   = os.Link
	removePath = os.Remove
	chmodPath  = os.Chmod
	readRandom = rand.Read
	syncDir    = syncDirectory
	openSource = func(path string) (io.ReadCloser, error) {
		return os.Open(path) //nolint:gosec // the path is an explicit user attachment
	}
	openObject = func(path string) (io.ReadCloser, error) {
		return os.Open(path) //nolint:gosec // the path is derived from the private root and a validated digest
	}
	createStaged = func(path string) (stagedFile, error) {
		return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the path is a random name in the private staging directory
	}
)

// stagedFile is the subset of *os.File one staged write uses.
type stagedFile interface {
	io.Writer
	Sync() error
	Close() error
}

// Config selects the store root. It must be absolute; the root may be a link
// but must resolve to an owner-only directory.
type Config struct {
	Root string
}

// Store is the attachments plugin: the image normalizer and the local
// content-addressed store behind it.
type Store struct {
	root    string
	version string
	objects string
	staging string

	mu      sync.Mutex
	started bool
	running bool
	active  int
	idle    chan struct{}

	// conversions holds one token per running normalization, bounding the
	// decode and scale buffers of concurrent reads like upstream's limiter.
	conversions chan struct{}

	observers map[*observer]struct{}
}

// observer is one ObserveUnavailable registration. calls counts the
// callbacks running on reader goroutines; idle closes when the last one
// returns after the registration was withdrawn. Both are guarded by the
// store mutex.
type observer struct {
	notify func(session.Image, error)
	calls  int
	idle   chan struct{}
}

// New constructs an inert store. It does not touch the filesystem.
func New(config Config) (*Store, error) {
	if !filepath.IsAbs(config.Root) {
		return nil, ErrInvalidConfig
	}
	root := filepath.Clean(config.Root)
	version := filepath.Join(root, "v1")
	return &Store{
		root: root, version: version, objects: filepath.Join(version, "objects"), staging: filepath.Join(version, "tmp"),
		conversions: make(chan struct{}, maxConversions),
	}, nil
}

// ID returns the stable plugin identity.
func (*Store) ID() string { return "attachments" }

// Start prepares the private directories, makes their entries durable up to
// the filesystem root, and accepts operations until scope cleanup. Cleanup
// refuses new operations and waits for running ones to finish.
func (store *Store) Start(_ context.Context, scope *plugin.Scope) error {
	store.mu.Lock()
	if store.started {
		store.mu.Unlock()
		return ErrInvalidConfig
	}
	store.started = true
	store.mu.Unlock()
	if err := makeDirs(store.root, 0o700); err != nil {
		return fmt.Errorf("create attachment root %s: %w", store.root, err)
	}
	if err := checkPrivate(store.root, statPath); err != nil {
		return err
	}
	for _, directory := range []string{store.version, store.objects, store.staging} {
		if err := privateDir(directory); err != nil {
			return err
		}
	}
	// A directory another process created may not be durable yet, so this
	// process proves every level itself before it reports a reference.
	for level := store.version; ; level = filepath.Dir(level) {
		if err := syncDir(level); err != nil {
			return fmt.Errorf("sync attachment directory %s: %w", level, err)
		}
		if filepath.Dir(level) == level {
			break
		}
	}
	if err := scope.Defer(store.stop); err != nil {
		return err
	}
	store.mu.Lock()
	store.running = true
	store.mu.Unlock()
	return nil
}

// checkPrivate rejects anything that is not an owner-only directory. inspect
// is Stat for the configured root, which may be a link, and Lstat for the
// directories the store derives itself.
func checkPrivate(directory string, inspect func(string) (fs.FileInfo, error)) error {
	info, err := inspect(directory)
	if err != nil {
		return fmt.Errorf("inspect attachment directory %s: %w", directory, err)
	}
	if !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s must be a private directory", ErrInvalidConfig, directory)
	}
	return nil
}

// privateDir creates a derived directory owner-only when it is missing and
// refuses links and directories other users can reach.
func privateDir(directory string) error {
	if err := makeDir(directory, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create attachment directory %s: %w", directory, err)
	}
	return checkPrivate(directory, lstatPath)
}

func (store *Store) stop(ctx context.Context) error {
	store.mu.Lock()
	store.running = false
	if store.active == 0 {
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
		return fmt.Errorf("wait for attachment operations: %w", ctx.Err())
	}
}

// begin admits one operation while the store runs.
func (store *Store) begin(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if !store.running {
		return ErrNotRunning
	}
	store.active++
	return nil
}

func (store *Store) end() {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.active--
	if store.active == 0 && store.idle != nil {
		close(store.idle)
		store.idle = nil
	}
}

// PrepareFile normalizes an image file the user explicitly chose without
// storing it. It does not follow a link at the path. The caller keeps the
// returned bytes and calls Commit before any message cites the reference,
// so an attachment that is never sent leaves nothing behind.
func (store *Store) PrepareFile(ctx context.Context, path string) (session.Image, []byte, error) {
	if err := store.begin(ctx); err != nil {
		return session.Image{}, nil, err
	}
	defer store.end()
	absolute, err := absPath(strings.TrimSpace(path))
	if err != nil || path == "" {
		return session.Image{}, nil, fmt.Errorf("%w: resolve path", ErrInvalidImage)
	}
	info, err := lstatPath(absolute)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxSourceBytes {
		return session.Image{}, nil, fmt.Errorf("%w: source must be a regular file within %d bytes", ErrInvalidImage, maxSourceBytes)
	}
	file, err := openSource(absolute)
	if err != nil {
		return session.Image{}, nil, fmt.Errorf("%w: open source", ErrInvalidImage)
	}
	defer func() { _ = file.Close() }() // read-only; close cannot lose data
	encoded, err := io.ReadAll(io.LimitReader(file, maxSourceBytes+1))
	if err != nil || len(encoded) > maxSourceBytes {
		return session.Image{}, nil, fmt.Errorf("%w: read source", ErrInvalidImage)
	}
	prepared, err := store.convert(ctx, filepath.Base(absolute), encoded)
	if err != nil {
		return session.Image{}, nil, err
	}
	return prepared.ref, prepared.data, nil
}

// Commit makes normalized bytes from PrepareFile durable at their content
// address. Bytes that do not match the reference are refused.
func (store *Store) Commit(ctx context.Context, ref session.Image, data []byte) error {
	if err := store.begin(ctx); err != nil {
		return err
	}
	defer store.end()
	if err := (session.Record{Type: session.RecordToolResult, Turn: 1, Step: 1, Result: &session.ToolResult{CallID: "validate", Image: &ref}}).Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidImage, err)
	}
	if err := verify(data, ref); err != nil {
		return fmt.Errorf("%w: bytes do not match the reference", ErrInvalidImage)
	}
	return store.publish(normalized{data: data, ref: ref})
}

// ObserveUnavailable calls notify whenever a read finds a referenced object
// missing or failing verification, until scope cleanup. The call happens on
// the reading goroutine, so notify must not block. Cleanup withdraws the
// registration and waits for callbacks already running, so none runs after
// it returns; a registration whose cleanup cannot be scheduled is withdrawn
// before the error returns.
func (store *Store) ObserveUnavailable(notify func(session.Image, error), scope *plugin.Scope) error {
	if notify == nil || scope == nil {
		return ErrInvalidConfig
	}
	entry := &observer{notify: notify}
	store.mu.Lock()
	if store.observers == nil {
		store.observers = map[*observer]struct{}{}
	}
	store.observers[entry] = struct{}{}
	store.mu.Unlock()
	if err := scope.Defer(func(ctx context.Context) error { return store.withdraw(ctx, entry) }); err != nil {
		// A concurrent read may already have captured the entry, so the
		// rollback also waits for its callback.
		return errors.Join(err, store.withdraw(context.Background(), entry))
	}
	return nil
}

// withdraw removes entry so no new callback starts, then waits until the
// callbacks already running return or ctx ends.
func (store *Store) withdraw(ctx context.Context, entry *observer) error {
	store.mu.Lock()
	delete(store.observers, entry)
	if entry.calls == 0 {
		store.mu.Unlock()
		return nil
	}
	if entry.idle == nil {
		entry.idle = make(chan struct{})
	}
	idle := entry.idle
	store.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for attachment observer callbacks: %w", ctx.Err())
	}
}

// SaveImage normalizes and stores source bytes that the caller read under
// its own path policy. It returns the durable reference and the decoded
// source size; refusals wrap ErrInvalidImage and, where a format or limit
// applies, session.ErrImageFormat, session.ErrImagePixels, or
// session.ErrImageBytes.
func (store *Store) SaveImage(ctx context.Context, name string, encoded []byte) (session.Image, stdimage.Point, error) {
	if err := store.begin(ctx); err != nil {
		return session.Image{}, stdimage.Point{}, err
	}
	defer store.end()
	if len(encoded) == 0 || len(encoded) > maxSourceBytes {
		return session.Image{}, stdimage.Point{}, fmt.Errorf("%w: source must be 1-%d bytes", ErrInvalidImage, maxSourceBytes)
	}
	prepared, err := store.convert(ctx, name, encoded)
	if err != nil {
		return session.Image{}, prepared.source, err
	}
	return prepared.ref, prepared.source, store.publish(prepared)
}

// convert normalizes one source while holding a conversion token. A caller
// waiting for a token stops when ctx ends.
func (store *Store) convert(ctx context.Context, name string, encoded []byte) (normalized, error) {
	select {
	case store.conversions <- struct{}{}:
	case <-ctx.Done():
		return normalized{}, ctx.Err()
	}
	defer func() { <-store.conversions }()
	return normalize(ctx, name, encoded)
}

// publish makes prepared durable at its content address. An existing object
// with the same address is verified and shared.
func (store *Store) publish(prepared normalized) (err error) {
	digest, _ := session.ImageDigest(prepared.ref.ID) // normalize derived the ID from the digest
	prefix := filepath.Join(store.objects, digest[:2])
	if err := privateDir(prefix); err != nil {
		return fmt.Errorf("store attachment %s: %w", prepared.ref.ID, err)
	}
	var random [16]byte
	if _, err := readRandom(random[:]); err != nil {
		return fmt.Errorf("store attachment %s: %w", prepared.ref.ID, err)
	}
	staged := filepath.Join(store.staging, hex.EncodeToString(random[:]))
	if err := writeStaged(staged, prepared.data); err != nil {
		return fmt.Errorf("store attachment %s: %w", prepared.ref.ID, err)
	}
	// The staging name is private; dropping it after a failure or after the
	// object is linked cannot lose a published object.
	defer func() {
		if removeErr := removePath(staged); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			err = errors.Join(err, fmt.Errorf("remove staged attachment: %w", removeErr))
		}
	}()
	target := filepath.Join(prefix, digest)
	if err := linkPath(staged, target); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("store attachment %s: %w", prepared.ref.ID, err)
		}
		if _, err := readObject(target, prepared.ref); err != nil {
			return fmt.Errorf("store attachment %s: existing object: %w", prepared.ref.ID, err)
		}
	}
	if err := chmodPath(target, 0o400); err != nil {
		return fmt.Errorf("store attachment %s: %w", prepared.ref.ID, err)
	}
	for _, directory := range []string{prefix, store.objects, store.version} {
		if err := syncDir(directory); err != nil {
			return fmt.Errorf("store attachment %s: sync %s: %w", prepared.ref.ID, directory, err)
		}
	}
	return nil
}

func writeStaged(path string, data []byte) (err error) {
	file, err := createStaged(path)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = removePath(path) // the staged file is private and unpublished
		}
	}()
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	return errors.Join(err, file.Close())
}

// ReadImage returns the stored bytes of ref after verifying their length,
// digest, media type, and dimensions. A missing object wraps
// session.ErrAttachmentMissing and a mismatch wraps
// session.ErrAttachmentCorrupt; other failures are I/O errors.
func (store *Store) ReadImage(ctx context.Context, ref session.Image) ([]byte, error) {
	if err := store.begin(ctx); err != nil {
		return nil, err
	}
	defer store.end()
	digest, ok := session.ImageDigest(ref.ID)
	if !ok {
		return nil, store.unavailable(ref, fmt.Errorf("read attachment %q: %w", ref.ID, session.ErrAttachmentCorrupt))
	}
	prefix := filepath.Join(store.objects, digest[:2])
	info, err := lstatPath(prefix)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, store.unavailable(ref, fmt.Errorf("read attachment %s: %w", ref.ID, session.ErrAttachmentMissing))
	case err != nil:
		return nil, fmt.Errorf("read attachment %s: %w", ref.ID, err)
	case !info.IsDir():
		return nil, store.unavailable(ref, fmt.Errorf("read attachment %s: %w", ref.ID, session.ErrAttachmentCorrupt))
	}
	data, err := readObject(filepath.Join(prefix, digest), ref)
	if err != nil {
		return nil, store.unavailable(ref, fmt.Errorf("read attachment %s: %w", ref.ID, err))
	}
	return data, nil
}

// unavailable reports a missing or mismatched object to every observer and
// returns err unchanged.
func (store *Store) unavailable(ref session.Image, err error) error {
	if !errors.Is(err, session.ErrAttachmentMissing) && !errors.Is(err, session.ErrAttachmentCorrupt) {
		return err
	}
	// Each captured observer counts as running until its callback returns,
	// so withdrawing it waits instead of racing this goroutine.
	store.mu.Lock()
	observers := make([]*observer, 0, len(store.observers))
	for entry := range store.observers {
		entry.calls++
		observers = append(observers, entry)
	}
	store.mu.Unlock()
	for _, entry := range observers {
		entry.notify(ref, err)
		store.mu.Lock()
		entry.calls--
		if entry.calls == 0 && entry.idle != nil {
			close(entry.idle)
			entry.idle = nil
		}
		store.mu.Unlock()
	}
	return err
}

// readObject reads one object without following a link and verifies it
// against ref.
func readObject(path string, ref session.Image) ([]byte, error) {
	info, err := lstatPath(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, session.ErrAttachmentMissing
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular():
		return nil, session.ErrAttachmentCorrupt
	}
	file, err := openObject(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }() // read-only; close cannot lose data
	data, err := io.ReadAll(io.LimitReader(file, session.MaxImageBytes+1))
	if err != nil {
		return nil, err
	}
	if err := verify(data, ref); err != nil {
		return nil, err
	}
	return data, nil
}

// verify proves data is exactly what ref describes: its length, SHA-256,
// media type, and dimensions. Any difference is session.ErrAttachmentCorrupt,
// so mismatched bytes never reach a provider.
func verify(data []byte, ref session.Image) error {
	digest := sha256.Sum256(data)
	if len(data) != ref.Bytes || session.ImageID(hex.EncodeToString(digest[:])) != ref.ID {
		return session.ErrAttachmentCorrupt
	}
	config, format, err := stdimage.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != ref.MediaType || config.Width != ref.Width || config.Height != ref.Height {
		return session.ErrAttachmentCorrupt
	}
	return nil
}
