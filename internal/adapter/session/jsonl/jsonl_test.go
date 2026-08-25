package jsonl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/transcript"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

const testCompositionID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func userMessage(text string) *coresession.Message {
	return &coresession.Message{Role: coresession.RoleUser, Source: coresession.MessageSource{Kind: "test"}, Content: []coresession.ContentBlock{{Type: coresession.ContentText, Text: text}}}
}

func assistantMessage(text string) *coresession.Message {
	return &coresession.Message{Role: coresession.RoleAssistant, Source: coresession.MessageSource{Kind: "provider"}, Content: []coresession.ContentBlock{{Type: coresession.ContentText, Text: text}}}
}

func startManager(t *testing.T) (*Manager, *plugin.Scope) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // directories require owner execute permission
		t.Fatal(err)
	}
	manager, err := New(Config{Root: root, CompositionID: testCompositionID})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := manager.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	return manager, scope
}

func appendRecord(t *testing.T, log *Log, record coresession.Record) coresession.Event {
	t.Helper()
	event, err := log.Append(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func appendClosedTurn(t *testing.T, log *Log, turn uint64) {
	t.Helper()
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnStart, Turn: turn})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: turn, Message: userMessage("hello")})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordStepStart, Turn: turn, Step: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordRequestHeader, Turn: turn, Step: 1, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordAssistantMessage, Turn: turn, Step: 1, Message: assistantMessage("answer")})
	call := &coresession.ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}
	appendRecord(t, log, coresession.Record{Type: coresession.RecordToolCall, Turn: turn, Step: 1, Call: call})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordApprovalAsked, Turn: turn, Step: 1, Approval: &coresession.ApprovalData{ID: "approval", CallID: "call", ToolName: "tool", Reason: "write"}})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordApprovalDecided, Turn: turn, Step: 1, Approval: &coresession.ApprovalData{ID: "approval", Outcome: coresession.ApprovalAllowedOnce}})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordToolResult, Turn: turn, Step: 1, Result: &coresession.ToolResult{CallID: "call", Output: "ok"}})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordStepEnd, Turn: turn, Step: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnEnd, Turn: turn, Outcome: coresession.OutcomeCompleted})
}

func TestManagerAndLogLifecycle(t *testing.T) {
	for _, config := range []Config{{}, {Root: t.TempDir(), CompositionID: "bad"}} {
		if _, err := New(config); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("invalid config=%#v err=%v", config, err)
		}
	}
	manager, scope := startManager(t)
	if manager.ID() != "sessions" {
		t.Fatal("wrong manager ID")
	}
	if err := manager.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("second start=%v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Open(canceled, OpenOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("open cancellation=%v", err)
	}
	if _, err := manager.Open(context.Background(), OpenOptions{SessionID: "bad/id", Create: true, Cwd: "."}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("bad metadata=%v", err)
	}
	options := OpenOptions{SessionID: "session-1", Create: true, Cwd: t.TempDir()}
	log, err := manager.Open(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if log.Header().SessionID != options.SessionID || log.Path() == "" {
		t.Fatalf("header=%#v path=%q", log.Header(), log.Path())
	}
	if _, err := manager.Open(context.Background(), options); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("duplicate open=%v", err)
	}
	if _, err := log.Append(canceled, coresession.Record{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("append cancellation=%v", err)
	}
	if _, err := log.Append(context.Background(), coresession.Record{}); !errors.Is(err, coresession.ErrInvalidRecord) {
		t.Fatalf("invalid record=%v", err)
	}
	appendClosedTurn(t, log, 1)
	if _, err := log.Append(context.Background(), coresession.Record{Type: coresession.RecordTurnStart, Turn: 3}); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("invalid order=%v", err)
	}
	events, err := log.Events(context.Background())
	if err != nil || len(events) != 11 {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	events[1].Record.Message.Content[0].Text = "changed"
	eventsAgain, _ := log.Events(context.Background())
	if coresession.Text(*eventsAgain[1].Record.Message) == "changed" {
		t.Fatal("Events aliases live log")
	}
	if _, err := log.Events(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("events cancellation=%v", err)
	}
	if err := log.Flush(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("flush cancellation=%v", err)
	}
	if err := log.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	header, inspected, err := manager.Inspect(context.Background(), options.SessionID)
	if err != nil || header.SessionID != options.SessionID || len(inspected) != len(events) {
		t.Fatalf("inspect header=%#v events=%d err=%v", header, len(inspected), err)
	}
	if _, _, err := manager.Inspect(canceled, options.SessionID); !errors.Is(err, context.Canceled) {
		t.Fatalf("inspect cancellation=%v", err)
	}
	if _, _, err := manager.Inspect(context.Background(), "bad/id"); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("inspect invalid ID=%v", err)
	}
	if _, _, err := manager.Inspect(context.Background(), "missing"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("inspect missing=%v", err)
	}
	if err := log.Close(context.Background()); err != nil || log.Close(context.Background()) != nil {
		t.Fatalf("close=%v", err)
	}
	if _, err := log.Append(context.Background(), coresession.Record{Type: coresession.RecordTurnStart, Turn: 2}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("append closed=%v", err)
	}
	if _, err := log.Events(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("events closed=%v", err)
	}
	if err := log.Flush(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("flush closed=%v", err)
	}
	resumed, err := manager.OpenSession(context.Background(), transcript.OpenOptions{SessionID: options.SessionID, Cwd: options.Cwd})
	if err != nil || resumed.Header().SessionID != options.SessionID {
		t.Fatalf("resume=%#v err=%v", resumed, err)
	}
	if err := resumed.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := manager.Open(context.Background(), OpenOptions{SessionID: "session-2", Create: true, Cwd: options.Cwd})
	if err != nil {
		t.Fatal(err)
	}
	appendClosedTurn(t, second, 1)
	_ = second.Close(context.Background())
	headers, err := manager.List(context.Background())
	if err != nil || len(headers) != 2 {
		t.Fatalf("headers=%#v err=%v", headers, err)
	}
	if _, err := manager.List(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("list cancellation=%v", err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Open(context.Background(), options); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("open stopped=%v", err)
	}
	if _, _, err := manager.Inspect(context.Background(), options.SessionID); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inspect stopped=%v", err)
	}
	if _, err := manager.List(context.Background()); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("list stopped=%v", err)
	}
}

func TestInterruptedTailRepair(t *testing.T) {
	manager, scope := startManager(t)
	defer func() { _ = scope.Close(context.Background()) }()
	root := t.TempDir()
	log, err := manager.Open(context.Background(), OpenOptions{SessionID: "repair", Create: true, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("hello")})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordStepStart, Turn: 1, Step: 1})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("call")})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}})
	appendRecord(t, log, coresession.Record{Type: coresession.RecordApprovalAsked, Turn: 1, Step: 1, Approval: &coresession.ApprovalData{ID: "approval", CallID: "call", ToolName: "tool", Reason: "write"}})
	_ = log.Close(context.Background())
	resumed, err := manager.Open(context.Background(), OpenOptions{SessionID: "repair", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	events, _ := resumed.Events(context.Background())
	if len(events) != 10 || events[6].Record.Type != coresession.RecordApprovalDecided || events[7].Record.Type != coresession.RecordToolResult || events[8].Record.Type != coresession.RecordStepEnd || events[9].Record.Outcome != coresession.OutcomeInterrupted {
		t.Fatalf("repair events=%#v", events)
	}
	_ = resumed.Close(context.Background())

	compact, err := manager.Open(context.Background(), OpenOptions{SessionID: "compact", Create: true, Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	appendRecord(t, compact, coresession.Record{Type: coresession.RecordCompactionStart, Compaction: &coresession.CompactionData{ID: "compact"}})
	_ = compact.Close(context.Background())
	compact, err = manager.Open(context.Background(), OpenOptions{SessionID: "compact", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	events, _ = compact.Events(context.Background())
	if len(events) != 2 || events[1].Record.Type != coresession.RecordCompactionEnd {
		t.Fatalf("compaction repair=%#v", events)
	}
	_ = compact.Close(context.Background())
}

func TestStrictDecodeAndIdentifiers(t *testing.T) {
	if decodeStrict([]byte(`{"x":1}`), &struct{}{}) == nil || decodeStrict([]byte(`{} {}`), &struct{}{}) == nil || decodeStrict([]byte(`{`), &struct{}{}) == nil {
		t.Fatal("strict decoder accepted invalid JSON")
	}
	if decodeStrict([]byte(`{}`), &struct{}{}) != nil {
		t.Fatal("strict decoder rejected object")
	}
	if !validSessionID("a._-Z9") || validSessionID("") || validSessionID(".bad") || validSessionID("bad/id") || validSessionID(strings.Repeat("a", 65)) {
		t.Fatal("session ID validation")
	}
	if !validCompositionID(testCompositionID) || validCompositionID(strings.ToUpper(testCompositionID)) || validCompositionID("bad") {
		t.Fatal("composition ID validation")
	}
	header := coresession.Header{SessionID: "session", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "."}
	if !validHeader(header) {
		t.Fatal("valid header rejected")
	}
	header.DelegationDepth = 17
	if validHeader(header) {
		t.Fatal("invalid header accepted")
	}
}

type faultFile struct {
	*os.File
	writeN      int
	writeErr    error
	readErr     error
	seekErr     error
	seekCalls   int
	seekFailAt  int
	statErr     error
	syncErr     error
	truncateErr error
	closeErr    error
}

func (file *faultFile) Read(buffer []byte) (int, error) {
	if file.readErr != nil {
		return 0, file.readErr
	}
	return file.File.Read(buffer)
}

func (file *faultFile) Write(buffer []byte) (int, error) {
	if file.writeErr != nil || file.writeN != 0 {
		return file.writeN, file.writeErr
	}
	return file.File.Write(buffer)
}

func (file *faultFile) Seek(offset int64, whence int) (int64, error) {
	file.seekCalls++
	if file.seekErr != nil && (file.seekFailAt == 0 || file.seekCalls == file.seekFailAt) {
		return 0, file.seekErr
	}
	return file.File.Seek(offset, whence)
}

func (file *faultFile) Stat() (os.FileInfo, error) {
	if file.statErr != nil {
		return nil, file.statErr
	}
	return file.File.Stat()
}

func (file *faultFile) Sync() error {
	if file.syncErr != nil {
		return file.syncErr
	}
	return file.File.Sync()
}

func (file *faultFile) Truncate(size int64) error {
	if file.truncateErr != nil {
		return file.truncateErr
	}
	return file.File.Truncate(size)
}

func (file *faultFile) Close() error {
	if file.closeErr != nil {
		_ = file.File.Close()
		return file.closeErr
	}
	return file.File.Close()
}

func temporaryFile(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "session-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestLogIOFailurePaths(t *testing.T) {
	manager := &Manager{active: true, logs: map[string]*Log{}}
	base := temporaryFile(t)
	log := &Log{manager: manager, header: coresession.Header{SessionID: "session"}, lockPath: filepath.Join(t.TempDir(), "lock"), file: base, active: true}
	record := coresession.Record{Type: coresession.RecordTurnStart, Turn: 1}
	originalMarshal := marshalJSON
	marshalJSON = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	if _, err := log.appendLocked(record); err == nil {
		t.Fatal("marshal error missing")
	}
	marshalJSON = originalMarshal
	log.size = maxSessionBytes
	if _, err := log.appendLocked(record); !errors.Is(err, ErrSessionSize) {
		t.Fatalf("size error=%v", err)
	}
	log.size = 0
	log.file = &faultFile{File: base, writeN: 1}
	if _, err := log.appendLocked(record); err == nil {
		t.Fatal("short write missing")
	}
	log.file = &faultFile{File: base, writeErr: errors.New("write"), truncateErr: errors.New("truncate")}
	if _, err := log.appendLocked(record); err == nil {
		t.Fatal("write/rollback error missing")
	}
	log.file = &faultFile{File: base, syncErr: errors.New("sync")}
	if _, err := log.appendLocked(record); err == nil {
		t.Fatal("sync error missing")
	}
	log.file = &faultFile{File: base, seekErr: errors.New("seek")}
	if err := log.rollback(0); err == nil {
		t.Fatal("rollback seek error missing")
	}
	log.file = &faultFile{File: base, syncErr: errors.New("sync")}
	if err := log.rollback(0); err == nil {
		t.Fatal("rollback sync error missing")
	}
	log.file = &faultFile{File: base, syncErr: errors.New("sync")}
	if err := log.Flush(context.Background()); err == nil {
		t.Fatal("flush sync error missing")
	}
	log.file = &faultFile{File: base, closeErr: errors.New("close")}
	originalRemove := removeFile
	removeFile = func(string) error { return errors.New("remove") }
	if err := log.Close(context.Background()); err == nil {
		t.Fatal("close errors missing")
	}
	removeFile = originalRemove
	marshalJSON = originalMarshal
}

func TestPrepareRootAndManagerFailurePaths(t *testing.T) {
	originalAbs, originalMake := absPath, makeAll
	originalEval, originalStat := evalLinks, statPath
	originalReadDir := readDirectory
	t.Cleanup(func() {
		absPath, makeAll, evalLinks, statPath, readDirectory = originalAbs, originalMake, originalEval, originalStat, originalReadDir
	})
	absPath = func(string) (string, error) { return "", errors.New("absolute") }
	if _, err := prepareRoot("x"); err == nil {
		t.Fatal("absolute failure missing")
	}
	absPath = originalAbs
	makeAll = func(string, os.FileMode) error { return errors.New("mkdir") }
	if _, err := prepareRoot(t.TempDir()); err == nil {
		t.Fatal("mkdir failure missing")
	}
	makeAll = originalMake
	evalLinks = func(string) (string, error) { return "", errors.New("links") }
	if _, err := prepareRoot(t.TempDir()); err == nil {
		t.Fatal("link failure missing")
	}
	evalLinks = originalEval
	statPath = func(string) (os.FileInfo, error) { return nil, errors.New("stat") }
	if _, err := prepareRoot(t.TempDir()); err == nil {
		t.Fatal("stat failure missing")
	}
	statPath = originalStat
	unsafe := t.TempDir()
	if err := os.Chmod(unsafe, 0o755); err != nil { //nolint:gosec // this negative test deliberately makes the session root unsafe
		t.Fatal(err)
	}
	if _, err := prepareRoot(unsafe); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("unsafe root=%v", err)
	}

	privateRoot := t.TempDir()
	if err := os.Chmod(privateRoot, 0o700); err != nil { //nolint:gosec // directories require owner execute permission
		t.Fatal(err)
	}
	manager, _ := New(Config{Root: privateRoot, CompositionID: testCompositionID})
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := manager.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope start=%v", err)
	}
	manager, scope := startManager(t)
	readDirectory = func(string) ([]os.DirEntry, error) { return nil, errors.New("read directory") }
	if _, err := manager.List(context.Background()); err == nil {
		t.Fatal("list error missing")
	}
	readDirectory = originalReadDir
	_ = scope.Close(context.Background())

	manager, _ = New(Config{Root: filepath.Join(t.TempDir(), "file"), CompositionID: testCompositionID})
	if err := os.WriteFile(manager.config.Root, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background(), &plugin.Scope{}); err == nil {
		t.Fatal("file root accepted")
	}
}

func TestReadSessionCorruptionPaths(t *testing.T) {
	write := func(t *testing.T, data string) *os.File {
		t.Helper()
		path := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(path, os.O_RDWR, 0o600) //nolint:gosec // the path is rooted in this test's private temporary directory
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	validHeader := `{"type":"session","version":2,"header":{"session_id":"session","composition_id":"` + testCompositionID + `","created_at_unix_ms":1,"cwd":"."}}` + "\n"
	for name, data := range map[string]string{
		"torn":                 validHeader + `{}`,
		"missing":              "",
		"invalid header":       "{}\n",
		"unsupported":          strings.Replace(validHeader, `"version":2`, `"version":1`, 1),
		"header fields":        strings.Replace(validHeader, `"session_id":"session"`, `"session_id":"bad/id"`, 1),
		"invalid event":        validHeader + "{}\n",
		"event order":          validHeader + `{"sequence":1,"record":{"type":"turn/start","turn":2,"step":0}}` + "\n",
		"multiple header JSON": strings.TrimSuffix(validHeader, "\n") + " {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			file := write(t, data)
			defer func() { _ = file.Close() }()
			if _, _, _, err := readSession(file, testCompositionID); err == nil {
				t.Fatal("corrupt session accepted")
			}
		})
	}
	oversized := write(t, strings.Repeat("x", maxRecordBytes+1)+"\n")
	if _, _, _, err := readSession(oversized, testCompositionID); err == nil {
		t.Fatal("oversized record accepted")
	}
	_ = oversized.Close()
	private := write(t, validHeader)
	if err := private.Chmod(0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readSession(private, testCompositionID); err == nil {
		t.Fatal("public transcript accepted")
	}
	_ = private.Close()

	base := write(t, validHeader)
	for name, fault := range map[string]*faultFile{
		"stat": {File: base, statErr: errors.New("stat")},
		"seek": {File: base, seekErr: errors.New("seek")},
		"read": {File: base, readErr: errors.New("read")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, _, err := readSession(fault, testCompositionID); err == nil {
				t.Fatal("file failure missing")
			}
		})
	}
	_ = base.Close()
}

func TestWriteHeaderFailures(t *testing.T) {
	base := temporaryFile(t)
	defer func() { _ = base.Close() }()
	header := coresession.Header{SessionID: "session", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "."}
	original := marshalJSON
	marshalJSON = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	if _, err := writeHeader(base, header); err == nil {
		t.Fatal("header marshal failure missing")
	}
	marshalJSON = original
	if _, err := writeHeader(&faultFile{File: base, writeN: 1}, header); err == nil {
		t.Fatal("header short write missing")
	}
	if _, err := writeHeader(&faultFile{File: base, writeErr: errors.New("write")}, header); err == nil {
		t.Fatal("header write failure missing")
	}
	if _, err := writeHeader(&faultFile{File: base, syncErr: errors.New("sync")}, header); err == nil {
		t.Fatal("header sync failure missing")
	}
}

func TestOpenAndInspectFailurePaths(t *testing.T) {
	originalOpenDisk, originalOpenRead := openDiskFile, openReadFile
	originalRemove := removeFile
	t.Cleanup(func() { openDiskFile, openReadFile, removeFile = originalOpenDisk, originalOpenRead, originalRemove })
	if err := acquireLock(filepath.Join(t.TempDir(), "missing", "lock")); err == nil {
		t.Fatal("lock creation error missing")
	}
	lock := filepath.Join(t.TempDir(), "lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := acquireLock(lock); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("existing lock=%v", err)
	}
	header := coresession.Header{SessionID: "session", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "."}
	if _, _, _, _, err := openSession(filepath.Join(t.TempDir(), "missing"), header, false); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("missing open=%v", err)
	}
	openDiskFile = func(string, int, os.FileMode) (durableFile, error) { return nil, errors.New("open") }
	if _, _, _, _, err := openSession("x", header, false); err == nil {
		t.Fatal("open error missing")
	}
	openDiskFile = originalOpenDisk

	path := filepath.Join(t.TempDir(), "other.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	other := header
	other.SessionID = "other"
	if _, err := writeHeader(file, other); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, _, _, _, err := openSession(path, header, false); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("header mismatch=%v", err)
	}
	corruptPath := filepath.Join(t.TempDir(), "corrupt.jsonl")
	if err := os.WriteFile(corruptPath, []byte("bad\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := openSession(corruptPath, header, false); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("corrupt open=%v", err)
	}

	base := temporaryFile(t)
	if _, err := writeHeader(base, header); err != nil {
		t.Fatal(err)
	}
	_, _ = base.Seek(0, io.SeekStart)
	openDiskFile = func(string, int, os.FileMode) (durableFile, error) {
		return &faultFile{File: base, seekErr: errors.New("seek"), seekFailAt: 2}, nil
	}
	if _, _, _, _, err := openSession("x", header, false); err == nil {
		t.Fatal("seek-end error missing")
	}
	openDiskFile = originalOpenDisk

	manager, scope := startManager(t)
	defer func() { _ = scope.Close(context.Background()) }()
	openReadFile = func(string) (durableFile, error) { return nil, errors.New("inspect open") }
	if _, _, err := manager.Inspect(context.Background(), "session"); err == nil {
		t.Fatal("inspect open error missing")
	}
}

func TestManagerOpenListStopAndRepairFailurePaths(t *testing.T) {
	originalMarshal := marshalJSON
	t.Cleanup(func() { marshalJSON = originalMarshal })
	manager, scope := startManager(t)
	defer func() { _ = scope.Close(context.Background()) }()
	cwd := t.TempDir()

	lockPath := filepath.Join(manager.root, "locked.jsonl.lock")
	if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Open(context.Background(), OpenOptions{SessionID: "locked", Create: true, Cwd: cwd}); !errors.Is(err, ErrSessionInUse) {
		t.Fatalf("manager lock error=%v", err)
	}
	if _, err := manager.Open(context.Background(), OpenOptions{SessionID: "missing", Cwd: cwd}); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("manager missing open=%v", err)
	}

	partial, err := manager.Open(context.Background(), OpenOptions{SessionID: "repair-error", Create: true, Cwd: cwd})
	if err != nil {
		t.Fatal(err)
	}
	appendRecord(t, partial, coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})
	_ = partial.Close(context.Background())
	marshalJSON = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	if _, err := manager.Open(context.Background(), OpenOptions{SessionID: "repair-error", Cwd: cwd}); err == nil {
		t.Fatal("manager repair error missing")
	}
	marshalJSON = originalMarshal

	invalidPath := filepath.Join(manager.root, "invalid.jsonl")
	if err := os.WriteFile(invalidPath, []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.List(context.Background()); err == nil {
		t.Fatal("list inspect error missing")
	}
	if err := os.Remove(invalidPath); err != nil {
		t.Fatal(err)
	}

	equalHeaders := []coresession.Header{
		{SessionID: "equal-b", CompositionID: testCompositionID, CreatedAtUnixMS: 10, Cwd: cwd},
		{SessionID: "equal-a", CompositionID: testCompositionID, CreatedAtUnixMS: 10, Cwd: cwd},
		{SessionID: "later", CompositionID: testCompositionID, CreatedAtUnixMS: 20, Cwd: cwd},
	}
	for _, header := range equalHeaders {
		file, err := os.OpenFile(filepath.Join(manager.root, header.SessionID+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writeHeader(file, header); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
	}
	headers, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) < 3 || headers[0].SessionID != "equal-a" || headers[1].SessionID != "equal-b" {
		t.Fatalf("equal timestamp ordering=%#v", headers)
	}

	base := temporaryFile(t)
	failingLog := &Log{manager: manager, header: coresession.Header{SessionID: "stop-error"}, file: &faultFile{File: base, closeErr: errors.New("close")}, lockPath: filepath.Join(t.TempDir(), "absent"), active: true}
	manager.logs[failingLog.header.SessionID] = failingLog
	if err := manager.stop(context.Background()); err == nil {
		t.Fatal("manager stop close error missing")
	}
}

func TestCloseAbsentLockAndRepairStageFailures(t *testing.T) {
	manager := &Manager{active: true, logs: map[string]*Log{}}
	base := temporaryFile(t)
	log := &Log{manager: manager, header: coresession.Header{SessionID: "close"}, file: base, lockPath: filepath.Join(t.TempDir(), "absent"), active: true}
	manager.logs["close"] = log
	if err := log.Close(context.Background()); err != nil {
		t.Fatalf("absent lock close=%v", err)
	}

	prefix := orderPrefix()
	assistant := withAssistant(orderPrefix())
	call := withCall(assistant)
	approval := withApproval(call)
	states := map[string][]coresession.Event{
		"approval":   approval,
		"call":       call,
		"compaction": {orderedEvent(coresession.Record{Type: coresession.RecordCompactionStart, Compaction: &coresession.CompactionData{ID: "compact"}})},
		"step":       prefix,
		"turn":       {orderedEvent(coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})},
	}
	for name, events := range states {
		t.Run(name, func(t *testing.T) {
			file := temporaryFile(t)
			journal := &Log{manager: manager, header: coresession.Header{SessionID: name}, file: &faultFile{File: file, writeErr: errors.New("write")}, active: true, events: events}
			if err := journal.repairInterrupted(context.Background()); err == nil {
				t.Fatal("repair stage error missing")
			}
			_ = file.Close()
		})
	}
	invalid := &Log{manager: manager, file: temporaryFile(t), active: true, events: []coresession.Event{orderedEvent(coresession.Record{Type: coresession.RecordTurnStart, Turn: 2})}}
	if err := invalid.repairInterrupted(context.Background()); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("invalid repair order=%v", err)
	}
	_ = invalid.file.Close()
}

func TestOpenSessionCreateAndReadOrderFailures(t *testing.T) {
	originalMarshal := marshalJSON
	t.Cleanup(func() { marshalJSON = originalMarshal })
	header := coresession.Header{SessionID: "session", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "."}
	path := filepath.Join(t.TempDir(), "session.jsonl")
	marshalJSON = func(any) ([]byte, error) { return nil, errors.New("marshal") }
	if _, _, _, _, err := openSession(path, header, true); err == nil {
		t.Fatal("create header error missing")
	}
	marshalJSON = originalMarshal
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed create left file: %v", err)
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeHeader(file, header); err != nil {
		t.Fatal(err)
	}
	event := coresession.Event{Sequence: 1, Record: coresession.Record{Type: coresession.RecordTurnStart, Turn: 2}}
	encoded, _ := json.Marshal(event)
	if _, err := file.Write(append(encoded, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := readSession(file, testCompositionID); !errors.Is(err, ErrCorruptSession) {
		t.Fatalf("read order error=%v", err)
	}
	_ = file.Close()
}
