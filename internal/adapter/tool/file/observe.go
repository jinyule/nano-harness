package file

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"
)

// version identifies the exact bytes a session observed: the SHA-256 digest
// of the complete file as read, written, or edited.
type version [sha256.Size]byte

// observation is what one session last learned about a target: confirmed
// absent, or present with a size and a version. A different current size
// proves a change without reading the file.
type observation struct {
	present bool
	size    int64
	version version
}

// observed builds the present observation of data.
func observed(data []byte) observation {
	return observation{present: true, size: int64(len(data)), version: digest(data)}
}

// observations is the in-memory prior-observation record behind the
// read-before-write guard, mirroring upstream's fs-observation-policy. It is
// keyed by session ID, then by resolved absolute path. Nothing is persisted:
// a resumed session, or a delegated child with its own session, starts with
// no observations and must read targets again.
type observations struct {
	mu        sync.Mutex
	bySession map[string]map[string]observation
}

// record stores an observation; calls without a session record nothing, so
// such callers can create new files but never overwrite or edit.
func (record *observations) record(sessionID, path string, value observation) {
	if sessionID == "" {
		return
	}
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.bySession == nil {
		record.bySession = map[string]map[string]observation{}
	}
	byPath := record.bySession[sessionID]
	if byPath == nil {
		byPath = map[string]observation{}
		record.bySession[sessionID] = byPath
	}
	byPath[path] = value
}

func (record *observations) lookup(sessionID, path string) (observation, bool) {
	record.mu.Lock()
	defer record.mu.Unlock()
	value, ok := record.bySession[sessionID][path]
	return value, ok
}

// clear drops every observation when the provider's scope closes.
func (record *observations) clear() {
	record.mu.Lock()
	record.bySession = nil
	record.mu.Unlock()
}

func digest(data []byte) version { return sha256.Sum256(data) }

// digestBufferBytes is the read size between cancellation checks.
const digestBufferBytes = 64 << 10

// digestFile hashes the complete current content of path, calling stop
// before every read so a caller can cancel hashing a large file. When retain
// is true, the same read keeps a diff basis strictly below maxEditBytes; nil
// means no basis, while a non-nil empty slice represents an empty file.
func digestFile(path string, retain bool, stop func() error) (version, []byte, error) {
	reader, err := openFile(path)
	if err != nil {
		return version{}, nil, err
	}
	defer func() { _ = reader.Close() }() // read-only; close cannot lose data
	hasher := sha256.New()
	var before []byte
	buffer := make([]byte, digestBufferBytes)
	for {
		if err := stop(); err != nil {
			return version{}, nil, err
		}
		count, err := reader.Read(buffer)
		_, _ = hasher.Write(buffer[:count]) // hash writes never fail
		if retain {
			if len(before)+count >= maxEditBytes {
				before, retain = nil, false
			} else {
				if size := len(before) + count; size > cap(before) {
					grown := make([]byte, len(before), min(maxEditBytes-1, max(size, cap(before)*2)))
					copy(grown, before)
					before = grown
				}
				before = append(before, buffer[:count]...)
			}
		}
		if errors.Is(err, io.EOF) {
			if retain && before == nil {
				before = []byte{}
			}
			return version(hasher.Sum(nil)), before, nil
		}
		if err != nil {
			return version{}, nil, err
		}
	}
}

// pathLocks serializes guarded check-and-publish per target path, so a slow
// verification of one file never blocks writes or edits of others.
type pathLocks struct {
	mu   sync.Mutex
	held map[string]*pathLock
}

type pathLock struct {
	sync.Mutex
	users int
}

// lock acquires path's lock and returns its release.
func (locks *pathLocks) lock(path string) func() {
	locks.mu.Lock()
	if locks.held == nil {
		locks.held = map[string]*pathLock{}
	}
	entry := locks.held[path]
	if entry == nil {
		entry = &pathLock{}
		locks.held[path] = entry
	}
	entry.users++
	locks.mu.Unlock()
	entry.Lock()
	return func() {
		entry.Unlock()
		locks.mu.Lock()
		if entry.users--; entry.users == 0 {
			delete(locks.held, path)
		}
		locks.mu.Unlock()
	}
}

// errNotRead and errStale carry upstream's model-facing remedies for the two
// guarded-mutation failures.
func errNotRead(target string) error {
	return fsFailure("FS_NOT_OBSERVED", fmt.Errorf("cannot modify %q: file has not been read — read the file, then retry", target))
}

func errStale(operation, target, reason string) error {
	return fsFailure("FS_STALE_VERSION", fmt.Errorf("cannot %s %q: %s — re-read the file, then retry", operation, target, reason))
}
