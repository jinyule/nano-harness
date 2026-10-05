package file

import (
	"crypto/sha256"
	"fmt"
	"io"
	"sync"
)

// version identifies the exact bytes a session observed: the SHA-256 digest
// of the complete file as read, written, or edited.
type version [sha256.Size]byte

// observation is what one session last learned about a target: confirmed
// absent, or present at a version.
type observation struct {
	present bool
	version version
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

// digestFile hashes the complete current content of path.
func digestFile(path string) (version, error) {
	reader, err := openFile(path)
	if err != nil {
		return version{}, err
	}
	defer func() { _ = reader.Close() }() // read-only; close cannot lose data
	hasher := sha256.New()
	if _, err := io.Copy(hasher, reader); err != nil {
		return version{}, err
	}
	return version(hasher.Sum(nil)), nil
}

// errNotRead and errStale carry upstream's model-facing remedies for the two
// guarded-mutation failures.
func errNotRead(target string) error {
	return fmt.Errorf("cannot modify %q: file has not been read — read the file, then retry", target)
}

func errStale(operation, target, reason string) error {
	return fmt.Errorf("cannot %s %q: %s — re-read the file, then retry", operation, target, reason)
}
