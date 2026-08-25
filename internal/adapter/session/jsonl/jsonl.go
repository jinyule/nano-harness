// Package jsonl provides a private, append-only session manager.
package jsonl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jinyule/nano-harness/internal/app/transcript"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

const (
	maxSessionBytes = 64 << 20
	maxRecordBytes  = 6 << 20
)

var (
	// ErrInvalidConfig identifies JSONL manager configuration that cannot be honored.
	ErrInvalidConfig = errors.New("invalid JSONL session configuration")
	// ErrNotRunning indicates the JSONL manager or log has not started or has stopped.
	ErrNotRunning = errors.New("JSONL session manager is not running")
	// ErrSessionInUse indicates the session already has an exclusive writer.
	ErrSessionInUse = errors.New("session is already open")
	// ErrSessionNotFound indicates the requested transcript does not exist.
	ErrSessionNotFound = errors.New("session not found")
	// ErrCorruptSession identifies a transcript that violates the strict format or event order.
	ErrCorruptSession = errors.New("corrupt session")
	// ErrUnsupported identifies a transcript format version this build cannot read.
	ErrUnsupported = errors.New("unsupported session version")
	// ErrSessionSize indicates an append would exceed the fixed transcript limit.
	ErrSessionSize = errors.New("session size limit reached")

	absPath       = filepath.Abs
	makeAll       = os.MkdirAll
	evalLinks     = filepath.EvalSymlinks
	statPath      = os.Stat
	readDirectory = os.ReadDir
	openReadFile  = func(path string) (durableFile, error) {
		return os.Open(path) //nolint:gosec // callers validate and root the path
	}
	openDiskFile = func(path string, flag int, permission os.FileMode) (durableFile, error) {
		return os.OpenFile(path, flag, permission) //nolint:gosec // callers validate and root the path
	}
	removeFile  = os.Remove
	marshalJSON = json.Marshal
)

type durableFile interface {
	io.Reader
	io.Writer
	io.Seeker
	Stat() (os.FileInfo, error)
	Sync() error
	Truncate(int64) error
	Close() error
}

// Config defines one session directory and semantic composition.
type Config struct {
	Root          string
	CompositionID string
}

// OpenOptions chooses create or resume and supplies immutable create metadata.
type OpenOptions struct {
	SessionID       string
	Create          bool
	Cwd             string
	ParentSessionID string
	DelegationDepth int
}

// Manager owns every live log and its writer lock.
type Manager struct {
	config Config

	mu      sync.Mutex
	started bool
	active  bool
	root    string
	logs    map[string]*Log
}

// New validates configuration without touching the filesystem.
func New(config Config) (*Manager, error) {
	if strings.TrimSpace(config.Root) == "" || !validCompositionID(config.CompositionID) {
		return nil, fmt.Errorf("%w: root and lowercase SHA-256 composition ID are required", ErrInvalidConfig)
	}
	return &Manager{config: config, logs: map[string]*Log{}}, nil
}

// ID returns the stable persistence-provider identity.
func (*Manager) ID() string { return "sessions" }

// Start prepares the private root and owns all open logs.
func (manager *Manager) Start(_ context.Context, scope *plugin.Scope) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.started {
		return fmt.Errorf("%w: already started", ErrInvalidConfig)
	}
	root, err := prepareRoot(manager.config.Root)
	if err != nil {
		return err
	}
	if err := scope.Defer(manager.stop); err != nil {
		return fmt.Errorf("register session manager cleanup: %w", err)
	}
	manager.started = true
	manager.active = true
	manager.root = root
	return nil
}

func (manager *Manager) stop(ctx context.Context) error {
	manager.mu.Lock()
	manager.active = false
	logs := make([]*Log, 0, len(manager.logs))
	for _, log := range manager.logs {
		logs = append(logs, log)
	}
	manager.mu.Unlock()
	var failures []error
	for _, log := range logs {
		if err := log.Close(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Open creates or resumes one exclusively written session and repairs an interrupted tail.
func (manager *Manager) Open(ctx context.Context, options OpenOptions) (*Log, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validSessionID(options.SessionID) || strings.TrimSpace(options.Cwd) == "" || options.DelegationDepth < 0 || options.DelegationDepth > 16 || options.ParentSessionID != "" && !validSessionID(options.ParentSessionID) {
		return nil, fmt.Errorf("%w: invalid session metadata", ErrInvalidConfig)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if !manager.active {
		return nil, ErrNotRunning
	}
	if _, exists := manager.logs[options.SessionID]; exists {
		return nil, fmt.Errorf("%w: %s", ErrSessionInUse, options.SessionID)
	}
	path := filepath.Join(manager.root, options.SessionID+".jsonl")
	lockPath := path + ".lock"
	if err := acquireLock(lockPath); err != nil {
		return nil, err
	}
	header := coresession.Header{
		SessionID: options.SessionID, CompositionID: manager.config.CompositionID,
		CreatedAtUnixMS: time.Now().UnixMilli(), Cwd: options.Cwd,
		ParentSessionID: options.ParentSessionID, DelegationDepth: options.DelegationDepth,
	}
	file, loadedHeader, events, size, err := openSession(path, header, options.Create)
	if err != nil {
		_ = removeFile(lockPath)
		return nil, err
	}
	log := &Log{manager: manager, header: loadedHeader, path: path, lockPath: lockPath, file: file, events: events, size: size, active: true}
	if err := log.repairInterrupted(ctx); err != nil {
		_ = file.Close()
		_ = removeFile(lockPath)
		return nil, err
	}
	manager.logs[options.SessionID] = log
	return log, nil
}

// OpenSession adapts the concrete JSONL log to the app repository boundary.
func (manager *Manager) OpenSession(ctx context.Context, options transcript.OpenOptions) (transcript.Log, error) {
	return manager.Open(ctx, OpenOptions{
		SessionID: options.SessionID, Create: options.Create, Cwd: options.Cwd,
		ParentSessionID: options.ParentSessionID, DelegationDepth: options.DelegationDepth,
	})
}

// Inspect reads one session without acquiring its writer lock or repairing it.
func (manager *Manager) Inspect(ctx context.Context, sessionID string) (coresession.Header, []coresession.Event, error) {
	if err := ctx.Err(); err != nil {
		return coresession.Header{}, nil, err
	}
	root, err := manager.activeRoot()
	if err != nil {
		return coresession.Header{}, nil, err
	}
	if !validSessionID(sessionID) {
		return coresession.Header{}, nil, ErrInvalidConfig
	}
	path := filepath.Join(root, sessionID+".jsonl")
	file, err := openReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return coresession.Header{}, nil, ErrSessionNotFound
	}
	if err != nil {
		return coresession.Header{}, nil, fmt.Errorf("open session inspection: %w", err)
	}
	defer func() { _ = file.Close() }()
	header, events, _, err := readSession(file, manager.config.CompositionID)
	return header, events, err
}

// List returns every valid session header in creation order.
func (manager *Manager) List(ctx context.Context) ([]coresession.Header, error) {
	root, err := manager.activeRoot()
	if err != nil {
		return nil, err
	}
	entries, err := readDirectory(root)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	headers := make([]coresession.Header, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := entry.Name()
		if entry.Type().IsRegular() && strings.HasSuffix(name, ".jsonl") {
			header, _, err := manager.Inspect(ctx, strings.TrimSuffix(name, ".jsonl"))
			if err != nil {
				return nil, err
			}
			headers = append(headers, header)
		}
	}
	sort.Slice(headers, func(left, right int) bool {
		if headers[left].CreatedAtUnixMS == headers[right].CreatedAtUnixMS {
			return headers[left].SessionID < headers[right].SessionID
		}
		return headers[left].CreatedAtUnixMS < headers[right].CreatedAtUnixMS
	})
	return headers, nil
}

func (manager *Manager) activeRoot() (string, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if !manager.active {
		return "", ErrNotRunning
	}
	return manager.root, nil
}

// Log is one exclusively-written append-only session.
type Log struct {
	manager  *Manager
	header   coresession.Header
	path     string
	lockPath string
	file     durableFile

	mu     sync.Mutex
	active bool
	events []coresession.Event
	size   int64
}

// Header returns immutable session metadata by value.
func (log *Log) Header() coresession.Header { return log.header }

// Path returns the transcript path for external verification.
func (log *Log) Path() string { return log.path }

// Append validates, fsyncs, and then publishes one event.
func (log *Log) Append(ctx context.Context, record coresession.Record) (coresession.Event, error) {
	if err := ctx.Err(); err != nil {
		return coresession.Event{}, err
	}
	if err := record.Validate(); err != nil {
		return coresession.Event{}, err
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	return log.appendLocked(record)
}

func (log *Log) appendLocked(record coresession.Record) (coresession.Event, error) {
	if !log.active {
		return coresession.Event{}, ErrNotRunning
	}
	event := coresession.Event{Sequence: uint64(len(log.events) + 1), Record: record}
	candidate := append(cloneEvents(log.events), event)
	if _, err := validateOrder(candidate, false); err != nil {
		return coresession.Event{}, err
	}
	encoded, err := marshalJSON(event)
	if err != nil {
		return coresession.Event{}, fmt.Errorf("encode session event: %w", err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > maxRecordBytes || log.size+int64(len(encoded)) > maxSessionBytes {
		return coresession.Event{}, ErrSessionSize
	}
	before := log.size
	if written, writeErr := log.file.Write(encoded); writeErr != nil || written != len(encoded) {
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return coresession.Event{}, errors.Join(fmt.Errorf("append session event: %w", writeErr), log.rollback(before))
	}
	if err := log.file.Sync(); err != nil {
		return coresession.Event{}, errors.Join(fmt.Errorf("sync session event: %w", err), log.rollback(before))
	}
	log.size += int64(len(encoded))
	log.events = append(log.events, coresession.CloneEvent(event))
	return coresession.CloneEvent(event), nil
}

func (log *Log) rollback(size int64) error {
	if err := log.file.Truncate(size); err != nil {
		return fmt.Errorf("rollback session length: %w", err)
	}
	if _, err := log.file.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek after session rollback: %w", err)
	}
	return log.file.Sync()
}

// Events returns an immutable-by-copy replay snapshot.
func (log *Log) Events(ctx context.Context) ([]coresession.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if !log.active {
		return nil, ErrNotRunning
	}
	return cloneEvents(log.events), nil
}

// Flush forces the current durable prefix to storage.
func (log *Log) Flush(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	if !log.active {
		return ErrNotRunning
	}
	return log.file.Sync()
}

// Close releases the writer lock after the file is quiet. It is idempotent.
func (log *Log) Close(_ context.Context) error {
	log.mu.Lock()
	if !log.active {
		log.mu.Unlock()
		return nil
	}
	log.active = false
	closeErr := log.file.Close()
	removeErr := removeFile(log.lockPath)
	log.mu.Unlock()
	log.manager.mu.Lock()
	delete(log.manager.logs, log.header.SessionID)
	log.manager.mu.Unlock()
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

func (log *Log) repairInterrupted(ctx context.Context) error {
	state, err := validateOrder(log.events, false)
	if err != nil || state.turn == 0 && state.compaction == "" {
		return err
	}
	for _, approvalID := range state.approvals {
		_, err = log.Append(ctx, coresession.Record{Type: coresession.RecordApprovalDecided, Turn: state.turn, Step: state.step, Approval: &coresession.ApprovalData{ID: approvalID, Outcome: coresession.ApprovalCancelled}})
		if err != nil {
			return fmt.Errorf("repair approval: %w", err)
		}
	}
	for _, callID := range state.calls {
		_, err = log.Append(ctx, coresession.Record{Type: coresession.RecordToolResult, Turn: state.turn, Step: state.step, Result: &coresession.ToolResult{CallID: callID, Output: "tool error: interrupted before a result was committed", IsError: true}})
		if err != nil {
			return fmt.Errorf("repair tool result: %w", err)
		}
	}
	if state.compaction != "" {
		_, err = log.Append(ctx, coresession.Record{Type: coresession.RecordCompactionEnd, Turn: state.turn, Compaction: &coresession.CompactionData{ID: state.compaction, Error: "interrupted"}})
		if err != nil {
			return fmt.Errorf("repair compaction: %w", err)
		}
	}
	if state.step != 0 {
		_, err = log.Append(ctx, coresession.Record{Type: coresession.RecordStepEnd, Turn: state.turn, Step: state.step})
		if err != nil {
			return fmt.Errorf("repair step: %w", err)
		}
	}
	if state.turn != 0 {
		_, err = log.Append(ctx, coresession.Record{Type: coresession.RecordTurnEnd, Turn: state.turn, Outcome: coresession.OutcomeInterrupted})
		if err != nil {
			return fmt.Errorf("repair turn: %w", err)
		}
	}
	return nil
}

func cloneEvents(events []coresession.Event) []coresession.Event {
	cloned := make([]coresession.Event, len(events))
	for index, event := range events {
		cloned[index] = coresession.CloneEvent(event)
	}
	return cloned
}

func prepareRoot(configured string) (string, error) {
	absolute, err := absPath(configured)
	if err != nil {
		return "", fmt.Errorf("resolve session root: %w", err)
	}
	if err := makeAll(absolute, 0o700); err != nil {
		return "", fmt.Errorf("create session root: %w", err)
	}
	resolved, err := evalLinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve session root links: %w", err)
	}
	info, err := statPath(resolved)
	if err != nil {
		return "", fmt.Errorf("stat session root: %w", err)
	}
	if !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%w: root must be a private directory", ErrInvalidConfig)
	}
	return resolved, nil
}

func acquireLock(path string) error {
	file, err := openDiskFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return fmt.Errorf("%w: %s", ErrSessionInUse, filepath.Base(path))
	}
	if err != nil {
		return fmt.Errorf("create session lock: %w", err)
	}
	return file.Close()
}

func openSession(path string, wanted coresession.Header, create bool) (durableFile, coresession.Header, []coresession.Event, int64, error) {
	flags := os.O_RDWR | os.O_APPEND
	if create {
		flags |= os.O_CREATE | os.O_EXCL
	}
	file, err := openDiskFile(path, flags, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return nil, coresession.Header{}, nil, 0, ErrSessionNotFound
	}
	if err != nil {
		return nil, coresession.Header{}, nil, 0, fmt.Errorf("open session: %w", err)
	}
	if create {
		size, err := writeHeader(file, wanted)
		if err != nil {
			_ = file.Close()
			_ = removeFile(path)
			return nil, coresession.Header{}, nil, 0, err
		}
		return file, wanted, nil, size, nil
	}
	header, events, size, err := readSession(file, wanted.CompositionID)
	if err != nil {
		_ = file.Close()
		return nil, coresession.Header{}, nil, 0, err
	}
	if header.SessionID != wanted.SessionID {
		_ = file.Close()
		return nil, coresession.Header{}, nil, 0, fmt.Errorf("%w: filename and header ID differ", ErrCorruptSession)
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		_ = file.Close()
		return nil, coresession.Header{}, nil, 0, fmt.Errorf("seek session end: %w", err)
	}
	return file, header, events, size, nil
}

type fileHeader struct {
	Type    string             `json:"type"`
	Version int                `json:"version"`
	Header  coresession.Header `json:"header"`
}

func writeHeader(file durableFile, header coresession.Header) (int64, error) {
	encoded, err := marshalJSON(fileHeader{Type: "session", Version: coresession.FormatVersion, Header: header})
	if err != nil {
		return 0, fmt.Errorf("encode session header: %w", err)
	}
	encoded = append(encoded, '\n')
	if written, err := file.Write(encoded); err != nil || written != len(encoded) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return 0, fmt.Errorf("write session header: %w", err)
	}
	if err := file.Sync(); err != nil {
		return 0, fmt.Errorf("sync session header: %w", err)
	}
	return int64(len(encoded)), nil
}

func readSession(file durableFile, compositionID string) (coresession.Header, []coresession.Event, int64, error) {
	info, err := file.Stat()
	if err != nil {
		return coresession.Header{}, nil, 0, fmt.Errorf("stat session: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxSessionBytes || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return coresession.Header{}, nil, 0, fmt.Errorf("%w: transcript must be a private regular file within the size limit", ErrCorruptSession)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return coresession.Header{}, nil, 0, fmt.Errorf("seek session start: %w", err)
	}
	reader := bufio.NewReader(io.LimitReader(file, maxSessionBytes+1))
	lines := make([][]byte, 0)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > maxRecordBytes {
			return coresession.Header{}, nil, 0, fmt.Errorf("%w: record exceeds limit", ErrCorruptSession)
		}
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				return coresession.Header{}, nil, 0, fmt.Errorf("%w: torn final record", ErrCorruptSession)
			}
			lines = append(lines, bytes.TrimSuffix(line, []byte{'\n'}))
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return coresession.Header{}, nil, 0, fmt.Errorf("read session: %w", readErr)
		}
	}
	if len(lines) == 0 {
		return coresession.Header{}, nil, 0, fmt.Errorf("%w: missing header", ErrCorruptSession)
	}
	var header fileHeader
	if err := decodeStrict(lines[0], &header); err != nil {
		return coresession.Header{}, nil, 0, fmt.Errorf("%w: invalid header", ErrCorruptSession)
	}
	if header.Version != coresession.FormatVersion {
		return coresession.Header{}, nil, 0, fmt.Errorf("%w: got %d", ErrUnsupported, header.Version)
	}
	if header.Type != "session" || header.Header.CompositionID != compositionID || !validHeader(header.Header) {
		return coresession.Header{}, nil, 0, fmt.Errorf("%w: invalid header fields", ErrCorruptSession)
	}
	events := make([]coresession.Event, 0, len(lines)-1)
	for index, line := range lines[1:] {
		var event coresession.Event
		if err := decodeStrict(line, &event); err != nil || event.Sequence != uint64(index+1) || event.Record.Validate() != nil {
			return coresession.Header{}, nil, 0, fmt.Errorf("%w: invalid event line %d", ErrCorruptSession, index+2)
		}
		events = append(events, event)
	}
	if _, err := validateOrder(events, false); err != nil {
		return coresession.Header{}, nil, 0, err
	}
	return header.Header, events, info.Size(), nil
}

func decodeStrict(line []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

func validHeader(header coresession.Header) bool {
	return validSessionID(header.SessionID) && validCompositionID(header.CompositionID) && header.CreatedAtUnixMS > 0 && strings.TrimSpace(header.Cwd) != "" && header.DelegationDepth >= 0 && header.DelegationDepth <= 16 && (header.ParentSessionID == "" || validSessionID(header.ParentSessionID))
}

func validCompositionID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 32 && id == strings.ToLower(id)
}

func validSessionID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for index, char := range id {
		alphaNumeric := char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9'
		if !alphaNumeric && (index == 0 || char != '.' && char != '_' && char != '-') {
			return false
		}
	}
	return true
}
