package file

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func requireFsCode(t *testing.T, result session.ToolResult, code string) {
	t.Helper()
	result = persistedFileResult(t, "read", result)
	if !result.IsError || result.Error == nil || *result.Error != (session.ToolError{Name: "FsError", Code: code}) || result.Meta != nil {
		t.Fatalf("classification = %+v, want FsError/%s without meta", result, code)
	}
}

// Persist the producer result through the real JSONL boundary and read bytes
// independently of its in-memory projection.
func persistedFileResult(t *testing.T, name string, result session.ToolResult) session.ToolResult {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // directories require owner execute permission
		t.Fatal(err)
	}
	manager, err := sessionjsonl.New(sessionjsonl.Config{Root: root, CompositionID: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := manager.Start(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	log, err := manager.Open(t.Context(), sessionjsonl.OpenOptions{SessionID: "session", Cwd: t.TempDir(), Create: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close(context.Background()) })
	for _, record := range []session.Record{
		{Type: session.RecordTurnStart, Turn: 1},
		{Type: session.RecordUserMessage, Turn: 1, Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "test"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "tools"}}}},
		{Type: session.RecordStepStart, Turn: 1, Step: 1},
		{Type: session.RecordRequestHeader, Turn: 1, Step: 1, Header: &session.RequestHeader{Provider: "openai", Model: "test"}},
		{Type: session.RecordAssistantMessage, Turn: 1, Step: 1, Message: &session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "provider"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "call"}}}},
		{Type: session.RecordToolCall, Turn: 1, Step: 1, Call: &session.ToolCall{ID: result.CallID, Name: name, Arguments: json.RawMessage(`{}`)}},
		{Type: session.RecordToolResult, Turn: 1, Step: 1, Result: &result},
		{Type: session.RecordStepEnd, Turn: 1, Step: 1},
		{Type: session.RecordTurnEnd, Turn: 1, Outcome: session.OutcomeCompleted},
	} {
		if _, err := log.Append(t.Context(), record); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" {
			continue
		}
		var entry struct {
			Record session.Record `json:"record"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Record.Result != nil {
			return *entry.Record.Result
		}
	}
	t.Fatal("missing disk result")
	return session.ToolResult{}
}

func TestFileResults_ClassifyFailures(t *testing.T) {
	for _, test := range []struct {
		name, tool, code string
		args             map[string]any
		prepare          func(*testing.T, *harness)
	}{
		{"missing", "read", "FS_NOT_FOUND", map[string]any{"file_path": "absent"}, nil},
		{"parent file", "read", "FS_NOT_FOUND", map[string]any{"file_path": "f/child"}, nil},
		{"directory", "read", "FS_NOT_REGULAR_FILE", map[string]any{"file_path": "."}, nil},
		{"binary", "read", "FS_NOT_TEXT", map[string]any{"file_path": "f"}, func(t *testing.T, h *harness) { writeFixture(t, h.path("f"), "a\x00b") }},
		{"invalid UTF8", "read", "FS_NOT_TEXT", map[string]any{"file_path": "f"}, func(t *testing.T, h *harness) { writeFixture(t, h.path("f"), "a\xff") }},
		{"outside", "read", "FS_SANDBOX_DENIED", map[string]any{"file_path": "../f"}, nil},
		{"unobserved", "write", "FS_NOT_OBSERVED", map[string]any{"file_path": "f", "content": "after"}, nil},
		{"stale", "write", "FS_STALE_VERSION", map[string]any{"file_path": "f", "content": "after"}, func(t *testing.T, h *harness) { h.read(t, "f"); writeFixture(t, h.path("f"), "other") }},
		{"deleted", "edit", "FS_STALE_VERSION", map[string]any{"file_path": "f", "old_string": "before", "new_string": "after"}, func(t *testing.T, h *harness) {
			h.read(t, "f")
			if err := os.Remove(h.path("f")); err != nil {
				t.Fatal(err)
			}
		}},
		{"observed absent", "edit", "FS_NOT_FOUND", map[string]any{"file_path": "absent", "old_string": "a", "new_string": "b"}, func(t *testing.T, h *harness) { h.call(t, "read", map[string]any{"file_path": "absent"}) }},
		{"no match", "edit", "FS_EDIT_NOT_FOUND", map[string]any{"file_path": "f", "old_string": "nope", "new_string": "b"}, func(t *testing.T, h *harness) { h.read(t, "f") }},
		{"ambiguous", "edit", "FS_AMBIGUOUS_EDIT", map[string]any{"file_path": "f", "old_string": "e", "new_string": "b"}, func(t *testing.T, h *harness) { h.read(t, "f") }},
		{"edit binary", "edit", "FS_NOT_TEXT", map[string]any{"file_path": "f", "old_string": "a", "new_string": "b"}, func(t *testing.T, h *harness) {
			writeFixture(t, h.path("f"), strings.Repeat("a", binarySampleBytes)+"\x00")
			h.read(t, "f")
		}},
		{"edit too large", "edit", "FS_TOO_LARGE", map[string]any{"file_path": "f", "old_string": "a", "new_string": "b"}, func(t *testing.T, h *harness) {
			writeFixture(t, h.path("f"), strings.Repeat("a", maxEditBytes+1))
			h.read(t, "f")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			writeFixture(t, h.path("f"), "before")
			if test.prepare != nil {
				test.prepare(t, h)
			}
			requireFsCode(t, h.call(t, test.tool, test.args), test.code)
		})
	}
}

func TestReadResult_RecordsTheReturnedWindow(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("f"), "\ufefffirst\r\n"+strings.Repeat("中", 2001)+"\r\nlast")
	result := h.call(t, "read", map[string]any{"file_path": "f", "offset": 2, "limit": 1})
	want := &session.ToolMeta{Read: &session.ReadMeta{Path: h.path("f"), Offset: 2, Lines: []session.ReadLine{{Number: 2, Text: strings.Repeat("中", 2000) + "... (line truncated to 2000 chars)"}}, TotalLines: 3}}
	if result.IsError || !reflect.DeepEqual(result.Meta, want) {
		t.Fatalf("window meta = %+v, want %+v", result.Meta, want)
	}
	writeFixture(t, h.path("empty"), "")
	result = h.call(t, "read", map[string]any{"file_path": "empty"})
	want = &session.ToolMeta{Read: &session.ReadMeta{Path: h.path("empty"), Offset: 1, Lines: []session.ReadLine{}, TotalLines: 0}}
	if !reflect.DeepEqual(result.Meta, want) {
		t.Fatalf("empty meta = %+v", result.Meta)
	}
}

func TestFileResults_DiffsMatchPublishedFiles(t *testing.T) {
	for _, test := range []struct {
		name, tool, before, after, old, new string
		all                                 bool
		oldHunks, newHunks                  []string
	}{
		{"bom crlf", "edit", "\ufeffa\r\nb\r\nc\r\n", "\ufeffa\r\nB\r\nc\r\n", "b", "B", false, []string{"a\nb\nc"}, []string{"a\nB\nc"}},
		{"write LF comparison", "write", "\ufeffa\r\nb\r\n", "a\nB\n", "", "", false, []string{"a\nb"}, []string{"a\nB"}},
		{"trailing newline", "write", "a", "a\n", "", "", false, []string{"a"}, []string{"a"}},
		{"delete", "edit", "a\nb\nc\n", "a\nc\n", "b\n", "", false, []string{"a\nb\nc"}, []string{"a\nc"}},
		{"insert", "write", "", "a\n", "", "", false, []string{""}, []string{"a"}},
		{"separate all", "edit", "x\n1\n2\n3\n4\n5\n6\n7\nx\n", "y\n1\n2\n3\n4\n5\n6\n7\ny\n", "x", "y", true, []string{"x\n1\n2\n3", "5\n6\n7\nx"}, []string{"y\n1\n2\n3", "5\n6\n7\ny"}},
		{"merged all", "edit", "x\n1\nx\n", "y\n1\ny\n", "x", "y", true, []string{"x\n1\nx"}, []string{"y\n1\ny"}},
		{"coarse write", "write", "x\n1\n2\n3\n4\n5\n6\n7\nx\n", "y\n1\n2\n3\n4\n5\n6\n7\ny\n", "", "", false, []string{"x\n1\n2\n3\n4\n5\n6\n7\nx"}, []string{"y\n1\n2\n3\n4\n5\n6\n7\ny"}},
		{"same", "write", "a\r\n", "a\n", "", "", false, []string{}, []string{}},
		{"same normalized edit", "edit", "a\r\n", "a\r\n", "a\r\n", "a\n", false, []string{}, []string{}},
		{"suffix context", "write", "0\n1\n2\n3\n4\n5\n6\n7\n8\n", "0\n1\n2\n3\nX\n5\n6\n7\n8\n", "", "", false, []string{"1\n2\n3\n4\n5\n6\n7"}, []string{"1\n2\n3\nX\n5\n6\n7"}},
		{"partial deletion", "edit", "0\n1\n2\n3\npreDROPpost\n5\n6\n7\n8\n", "0\n1\n2\n3\nprepost\n5\n6\n7\n8\n", "DROP", "", false, []string{"1\n2\n3\npreDROPpost\n5\n6\n7"}, []string{"1\n2\n3\nprepost\n5\n6\n7"}},
		{"insert BOM", "edit", "a", "\ufeffb", "a", "\ufeffb", false, []string{"a"}, []string{"b"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			writeFixture(t, h.path("f"), test.before)
			h.read(t, "f")
			args := map[string]any{"file_path": "f", "content": test.after}
			if test.tool == "edit" {
				args = map[string]any{"file_path": "f", "old_string": test.old, "new_string": test.new, "replace_all": test.all}
			}
			result := h.call(t, test.tool, args)
			if got := readFixture(t, h.path("f")); got != test.after {
				t.Fatalf("published = %q, want %q", got, test.after)
			}
			if result.IsError || result.Meta == nil {
				t.Fatalf("result lacks meta: %+v", result)
			}
			var diffs []session.FileDiff
			if test.tool == "write" {
				if result.Meta.Write == nil || result.Meta.Write.Operation != "update" || result.Meta.Write.Truncated {
					t.Fatalf("write meta = %+v", result.Meta.Write)
				}
				diffs = result.Meta.Write.Diffs
			} else {
				if result.Meta.Edit == nil || result.Meta.Edit.Truncated {
					t.Fatalf("edit meta = %+v", result.Meta.Edit)
				}
				diffs = result.Meta.Edit.Diffs
			}
			if diffs == nil || len(diffs) != len(test.oldHunks) {
				t.Fatalf("hunks = %+v, want %d", diffs, len(test.oldHunks))
			}
			for i, diff := range diffs {
				var old *string
				if test.before != "" {
					old = &test.oldHunks[i]
				}
				want := session.FileDiff{Path: h.path("f"), OldText: old, NewText: test.newHunks[i]}
				if !reflect.DeepEqual(diff, want) {
					t.Errorf("hunk = %+v, want %+v", diff, want)
				}
			}
		})
	}
}

func TestFileResults_ApprovalChangeAndCommittedCancellation(t *testing.T) {
	for _, name := range []string{"create", "update", "edit"} {
		t.Run(name, func(t *testing.T) {
			restoreHooks(t)
			h := newHarness(t)
			if name != "create" {
				writeFixture(t, h.path("f"), "before")
				h.read(t, "f")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			linkFile = func(from, to string) error { err := os.Link(from, to); cancel(); return err }
			renameFile = func(from, to string) error { err := os.Rename(from, to); cancel(); return err }
			tool, args := "write", map[string]any{"file_path": "f", "content": "after"}
			if name == "edit" {
				tool, args = "edit", map[string]any{"file_path": "f", "old_string": "before", "new_string": "after"}
			}
			encoded, _ := json.Marshal(args)
			result := h.runtime.ExecuteBatch(ctx, appTool.BatchRequest{SessionID: "session", Journal: nopJournal{}, Calls: []session.ToolCall{{ID: "call", Name: tool, Arguments: encoded}}})[0]
			if result.Error == nil || *result.Error != (session.ToolError{Name: "AbortError", Code: "ABORTED"}) || result.Meta != nil || readFixture(t, h.path("f")) != "after" {
				t.Fatalf("commit cancellation = %+v", result)
			}
		})
	}
}

func TestFileResults_StaleWhileApprovalPending(t *testing.T) {
	for _, tool := range []string{"write", "edit"} {
		t.Run(tool, func(t *testing.T) {
			h := newHarness(t)
			writeFixture(t, h.path("f"), "before")
			h.read(t, "f")
			entered, release := make(chan struct{}), make(chan struct{})
			h.approver.during = func() { close(entered); <-release }
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			finished, exited := make(chan session.ToolResult, 1), make(chan struct{})
			var unblock sync.Once
			t.Cleanup(func() { cancel(); unblock.Do(func() { close(release) }); <-exited })
			args := map[string]any{"file_path": "f", "content": "after"}
			if tool == "edit" {
				args = map[string]any{"file_path": "f", "old_string": "before", "new_string": "after"}
			}
			encoded, _ := json.Marshal(args)
			go func() {
				defer close(exited)
				finished <- h.runtime.ExecuteBatch(ctx, appTool.BatchRequest{SessionID: "session", Journal: nopJournal{}, Calls: []session.ToolCall{{ID: "call", Name: tool, Arguments: encoded}}})[0]
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("approval did not reach barrier")
			}
			writeFixture(t, h.path("f"), "other!")
			unblock.Do(func() { close(release) })
			requireFsCode(t, <-finished, "FS_STALE_VERSION")
			if readFixture(t, h.path("f")) != "other!" {
				t.Fatal("stale approval overwrote external modification")
			}
		})
	}
}

func TestWriteResult_BoundsTheBasisAndHunks(t *testing.T) {
	for _, test := range []struct {
		name, before, after string
		truncated           bool
	}{
		{"binary old", "a\x00b", "after", true},
		{"invalid old", "a\xff", "after", true},
		{"binary new", "before", "a\x00b", true},
		{"exact basis cap", strings.Repeat("a", maxEditBytes), "after", true},
		{"below basis cap", strings.Repeat("a", maxEditBytes-1), "after", true},
		{"escaped hunk", strings.Repeat("<", 50000), "after", true},
		{"small hunk", "\"\\\b\f\r\t<>&\u2028\u2029\x01中", "after", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			restoreHooks(t)
			h := newHarness(t)
			writeFixture(t, h.path("f"), test.before)
			// read_image can observe arbitrary source bytes before write replaces
			// them; seed precisely that observation for non-text fixtures.
			h.provider.observed.record("session", h.path("f"), observed([]byte(test.before)))
			opens := 0
			openFile = func(path string) (io.ReadCloser, error) {
				opens++
				return os.Open(path) //nolint:gosec // counts opens of this test's confined temporary target
			}
			result := persistedFileResult(t, "write", h.call(t, "write", map[string]any{"file_path": "f", "content": test.after}))
			if result.IsError || result.Meta == nil || result.Meta.Write == nil || result.Meta.Write.Truncated != test.truncated {
				t.Fatalf("write meta = %+v", result)
			}
			if opens != 2 {
				t.Fatalf("opened %d times, want one Check hash and one locked hash/basis", opens)
			}
			if test.truncated && len(result.Meta.Write.Diffs) != 0 {
				t.Fatal("unavailable or oversized basis retained")
			}
			if got := readFixture(t, h.path("f")); got != test.after {
				t.Fatal("wrong published bytes")
			}
		})
	}
	t.Run("new content cap", func(t *testing.T) {
		h := newHarness(t)
		writeFixture(t, h.path("f"), "before")
		h.read(t, "f")
		result, err := h.provider.write(t.Context(), appTool.Invocation{SessionID: "session", Approved: true, Journal: nopJournal{}}, writeArgs{FilePath: "f", Content: strings.Repeat("a", maxEditBytes)})
		if err != nil || result.Meta == nil || !result.Meta.Write.Truncated || len(result.Meta.Write.Diffs) != 0 {
			t.Fatalf("large new = %+v, %v", result, err)
		}
		if len(readFixture(t, h.path("f"))) != maxEditBytes {
			t.Fatal("write truncated file")
		}
	})
}

func TestDigestFile_RetentionAndCancellation(t *testing.T) {
	for _, size := range []int{0, maxEditBytes - 1, maxEditBytes, maxEditBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			restoreHooks(t)
			data := []byte(strings.Repeat("x", size))
			openFile = openFailing(data, io.EOF)
			got, basis, err := digestFile("f", true, func() error { return nil })
			if err != nil || got != digest(data) {
				t.Fatalf("digest = %x %v", got, err)
			}
			if size < maxEditBytes {
				if basis == nil || !bytes.Equal(basis, data) || cap(basis) >= maxEditBytes {
					t.Fatalf("basis len=%d cap=%d", len(basis), cap(basis))
				}
			} else if basis != nil {
				t.Fatal("over-cap basis retained")
			}
		})
	}
	restoreHooks(t)
	openFile = openFailing([]byte("before"), io.EOF)
	checks := 0
	_, basis, err := digestFile("f", true, func() error {
		checks++
		if checks == 2 {
			return context.Canceled
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) || basis != nil || checks != 2 {
		t.Fatalf("cancelled digest = %v %d %v", basis, checks, err)
	}
}

func TestReadImageResult_ClassifiesSourceFailuresAndKeepsOnlyPath(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("f.png"), pngBytes)
	result := persistedFileResult(t, "read_image", h.readImage(t, visionRoute, "f.png")[0])
	if result.IsError || result.Image == nil || result.Meta == nil || !reflect.DeepEqual(result.Meta, &session.ToolMeta{ReadImage: &session.ReadImageMeta{Path: h.path("f.png")}}) {
		t.Fatalf("image result = %+v", result)
	}
	for _, test := range []struct{ path, code string }{{"absent.png", "FS_NOT_FOUND"}, {".", "FS_NOT_REGULAR_FILE"}, {"../f.png", "FS_SANDBOX_DENIED"}} {
		requireFsCode(t, h.readImage(t, visionRoute, test.path)[0], test.code)
	}
	file, err := os.OpenFile(h.path("huge.png"), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(session.MaxImageSourceBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	requireFsCode(t, h.readImage(t, visionRoute, "huge.png")[0], "FS_TOO_LARGE")
	if result := h.readImage(t, visionRoute, "f.txt")[0]; result.Error != nil {
		t.Fatal("image format semantic failure classified")
	}
}

func TestFsError_PreservesCauseAndUnclassifiedFailures(t *testing.T) {
	for _, cause := range []error{fs.ErrPermission, context.Canceled, context.DeadlineExceeded, errors.New("disk failure")} {
		failure := classifyKnown(fmt.Errorf("cannot read: %w", cause))
		var declared appTool.Failure
		wantClassified := errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded)
		if !errors.Is(failure, cause) || errors.As(failure, &declared) != wantClassified || failure.Error() != "cannot read: "+cause.Error() {
			t.Fatalf("wrapped = %v", failure)
		}
		if declared != nil && declared.ToolError() != (session.ToolError{Name: "FsError", Code: "FS_ABORTED"}) {
			t.Fatalf("cancel classification = %+v", declared.ToolError())
		}
	}
	h := newHarness(t)
	_, missing := h.provider.read(t.Context(), appTool.Invocation{SessionID: "session", Journal: nopJournal{}}, readArgs{FilePath: "absent"})
	var pathError *fs.PathError
	if !errors.Is(missing, fs.ErrNotExist) || !errors.As(missing, &pathError) {
		t.Fatalf("missing cause = %v", missing)
	}
	writeFixture(t, h.path("f"), "before")
	for _, result := range []session.ToolResult{h.call(t, "read", map[string]any{"file_path": " "}), h.call(t, "read", map[string]any{"file_path": "f", "offset": 2})} {
		if !result.IsError || result.Error != nil || result.Meta != nil {
			t.Fatalf("semantic = %+v", result)
		}
	}
	h.approver.outcome = session.ApprovalRejected
	if result := h.call(t, "write", map[string]any{"file_path": "new", "content": "a"}); !result.IsError || result.Error != nil || result.Meta != nil {
		t.Fatalf("approval = %+v", result)
	}
	for _, cause := range []error{fs.ErrPermission, fs.ErrNotExist} {
		broken := &sandboxJournal{err: &fs.PathError{Op: "read", Path: "journal", Err: cause}}
		for _, name := range []string{"read", "read_image"} {
			result := h.runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{SessionID: "session", Journal: broken, Route: visionRoute, Calls: []session.ToolCall{{ID: "call", Name: name, Arguments: json.RawMessage(`{"file_path":"f"}`)}}})[0]
			if !result.IsError || result.Error != nil {
				t.Fatalf("%s policy journal = %+v", name, result)
			}
			if errors.Is(cause, fs.ErrNotExist) && result.Output != `Error: cannot read "": not found` {
				t.Fatalf("changed policy failure text: %q", result.Output)
			}
		}
	}
}

type cancelSyncFile struct {
	*os.File
	cancel context.CancelFunc
}

func (file *cancelSyncFile) Sync() error { err := file.File.Sync(); file.cancel(); return err }

func TestFileResults_PersistsCancelledPublication(t *testing.T) {
	for _, tool := range []string{"write", "edit"} {
		t.Run(tool, func(t *testing.T) {
			restoreHooks(t)
			h := newHarness(t)
			writeFixture(t, h.path("f"), "before")
			h.read(t, "f")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			createTemp = func(dir, pattern string) (stagedFile, error) {
				file, err := os.CreateTemp(dir, pattern)
				return &cancelSyncFile{File: file, cancel: cancel}, err
			}
			args := map[string]any{"file_path": "f", "content": "after"}
			if tool == "edit" {
				args = map[string]any{"file_path": "f", "old_string": "before", "new_string": "after"}
			}
			encoded, _ := json.Marshal(args)
			result := h.runtime.ExecuteBatch(ctx, appTool.BatchRequest{SessionID: "session", Journal: nopJournal{}, Calls: []session.ToolCall{{ID: "call", Name: tool, Arguments: encoded}}})[0]
			requireFsCode(t, result, "FS_ABORTED")
			if readFixture(t, h.path("f")) != "before" {
				t.Fatal("cancelled publication changed target")
			}
			entries, err := os.ReadDir(h.root.Path())
			if err != nil || len(entries) != 1 {
				t.Fatalf("staging residue: %v %v", entries, err)
			}
		})
	}
}

func TestFileDiff_UsesTheFinalJSONBudget(t *testing.T) {
	for _, tool := range []string{"write", "edit"} {
		for _, extra := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/%d", tool, extra), func(t *testing.T) {
				h := newHarness(t)
				writeFixture(t, h.path("f"), "before")
				h.read(t, "f")
				old := "before"
				proto := session.FileDiff{Path: h.path("f"), OldText: &old}
				meta := session.ToolMeta{Edit: &session.EditMeta{Diffs: []session.FileDiff{proto}}}
				if tool == "write" {
					meta = session.ToolMeta{Write: &session.WriteMeta{Operation: "update", Diffs: []session.FileDiff{proto}}}
				}
				encoded, _ := json.Marshal(meta)
				content := strings.Repeat("x", session.MaxToolMetaBytes-len(encoded)+extra)
				args := map[string]any{"file_path": "f", "content": content}
				if tool == "edit" {
					args = map[string]any{"file_path": "f", "old_string": "before", "new_string": content}
				}
				result := persistedFileResult(t, tool, h.call(t, tool, args))
				if result.IsError || result.Meta == nil {
					t.Fatalf("result = %+v", result)
				}
				var diffs []session.FileDiff
				var truncated bool
				if tool == "write" {
					diffs, truncated = result.Meta.Write.Diffs, result.Meta.Write.Truncated
				} else {
					diffs, truncated = result.Meta.Edit.Diffs, result.Meta.Edit.Truncated
				}
				if truncated != (extra == 1) || len(diffs) != 1-extra {
					t.Fatalf("budget extra=%d hunks=%d truncated=%v", extra, len(diffs), truncated)
				}
				if readFixture(t, h.path("f")) != content {
					t.Fatal("hunk budget altered file")
				}
				encoded, _ = json.Marshal(result.Meta)
				if extra == 0 && len(encoded) != session.MaxToolMetaBytes {
					t.Fatalf("exact envelope = %d", len(encoded))
				}
			})
		}
	}
}

func TestFileResults_SandboxDenialIsClassified(t *testing.T) {
	for _, tool := range []string{"write", "edit"} {
		t.Run(tool, func(t *testing.T) {
			h := newHarness(t)
			writeFixture(t, h.path("f"), "before")
			h.read(t, "f")
			journal := &sandboxJournal{}
			journal.set(t, "read-only")
			args := map[string]any{"file_path": "f", "content": "after"}
			if tool == "edit" {
				args = map[string]any{"file_path": "f", "old_string": "before", "new_string": "after"}
			}
			encoded, _ := json.Marshal(args)
			result := h.runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{SessionID: "session", Journal: journal, Calls: []session.ToolCall{{ID: "call", Name: tool, Arguments: encoded}}})[0]
			requireFsCode(t, result, "FS_SANDBOX_DENIED")
			if len(h.approver.reasons) != 0 || readFixture(t, h.path("f")) != "before" {
				t.Fatal("denial reached approval or publication")
			}
			if err := os.Symlink(h.path("f"), h.path("alias")); err != nil {
				t.Fatal(err)
			}
			args["file_path"] = "alias"
			requireFsCode(t, h.call(t, tool, args), "FS_SANDBOX_DENIED")
		})
	}
}

func TestReadResult_DistinguishesWindowAndMetadataCaps(t *testing.T) {
	h := newHarness(t)
	content := strings.Repeat(strings.Repeat("<", 2000)+"\n", 26)
	writeFixture(t, h.path("f"), content)
	result := persistedFileResult(t, "read", h.call(t, "read", map[string]any{"file_path": "f"}))
	if result.IsError || result.Meta == nil || result.Meta.Read == nil {
		t.Fatalf("read = %+v", result)
	}
	meta := result.Meta.Read
	if meta.TotalLines != 26 || meta.Offset != 1 || len(meta.Lines) != 21 || !meta.Truncated {
		t.Fatalf("meta = %+v", meta)
	}
	for i, line := range meta.Lines {
		if line.Number != int64(i+1) || line.Text != strings.Repeat("<", 2000) {
			t.Fatal("wrong retained window")
		}
	}
	if !strings.Contains(result.Output, "Showing lines 1-25") {
		t.Fatal("metadata cap altered read text")
	}
}
