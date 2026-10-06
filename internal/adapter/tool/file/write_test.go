package file

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestWrite_CreatesAndReplacesFilesAtomically(t *testing.T) {
	h := newHarness(t)
	result := h.call(t, "write", map[string]any{"file_path": "nested/dir/new.txt", "content": "hello\n"})
	target := h.path("nested", "dir", "new.txt")
	if result.IsError || result.Output != envelope(target, "Created file") || readFixture(t, target) != "hello\n" {
		t.Fatalf("create = %#v", result)
	}
	info, _ := os.Stat(target)
	parent, _ := os.Stat(h.path("nested"))
	if info.Mode().Perm() != newFileMode || parent.Mode().Perm() != newDirectoryMode {
		t.Fatalf("modes = %v %v", info.Mode(), parent.Mode())
	}
	writeFixture(t, h.path("existing.txt"), "old")
	if err := os.Chmod(h.path("existing.txt"), 0o640); err != nil { //nolint:gosec // a non-default mode proves replacement preserves it

		t.Fatal(err)
	}
	writeFixture(t, h.path("untouched.txt"), "same")
	h.read(t, "existing.txt")
	result = h.call(t, "write", map[string]any{"file_path": h.path("existing.txt"), "content": "", "sandbox_permissions": "workspace-write", "justification": "repeat the standing mode"})
	info, _ = os.Stat(h.path("existing.txt"))
	if result.IsError || result.Output != envelope(h.path("existing.txt"), "Updated file") || readFixture(t, h.path("existing.txt")) != "" || info.Mode().Perm() != 0o640 {
		t.Fatalf("update = %#v mode=%v", result, info.Mode())
	}
	if readFixture(t, h.path("untouched.txt")) != "same" {
		t.Fatal("unrelated file changed")
	}
	if strings.Join(h.approver.reasons, "|") != `write file "nested/dir/new.txt"|escalate sandbox to workspace-write: repeat the standing mode` {
		t.Fatalf("approval reasons = %q", h.approver.reasons)
	}
	entries, _ := os.ReadDir(h.root.Path())
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("staging residue %s", entry.Name())
		}
	}
}

func TestWrite_RejectsUnsafeTargetsWithoutTouchingFiles(t *testing.T) {
	h := newHarness(t)
	outside := t.TempDir()
	writeFixture(t, filepath.Join(outside, "victim.txt"), "safe")
	if err := os.Symlink(filepath.Join(outside, "victim.txt"), h.path("link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, h.path("linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.path("dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{"file_path": "x"}, `missing required property "content"`},
		{map[string]any{"file_path": "\t", "content": "x"}, "file_path must be a non-empty string"},
		{map[string]any{"file_path": "x", "content": "x", "sandbox_permissions": "workspace-write"}, "requires a justification"},
		{map[string]any{"file_path": "x", "content": "x", "justification": "why"}, "only valid together"},
		{map[string]any{"file_path": "x", "content": "x", "sandbox_permissions": "root", "justification": "why"}, `"sandbox_permissions" must be one of`},
		{map[string]any{"file_path": "link.txt", "content": "x"}, "path crosses a symbolic link"},
		{map[string]any{"file_path": "linkdir/new.txt", "content": "x"}, "path crosses a symbolic link"},
		{map[string]any{"file_path": "../escape.txt", "content": "x"}, "path is outside the workspace"},
		{map[string]any{"file_path": filepath.Join(outside, "victim.txt"), "content": "x"}, "path is outside the workspace"},
		{map[string]any{"file_path": "dir", "content": "x"}, "not a regular file"},
	} {
		result := h.call(t, "write", test.arguments)
		if !result.IsError || !strings.Contains(result.Output, test.want) {
			t.Errorf("write(%v) = %#v, want %q", test.arguments, result, test.want)
		}
	}
	if readFixture(t, filepath.Join(outside, "victim.txt")) != "safe" {
		t.Fatal("write escaped the workspace")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("write created a file through a symlinked directory")
	}
	if len(h.approver.reasons) != 0 {
		t.Fatalf("invalid arguments reached approval: %q", h.approver.reasons)
	}
	h.approver.outcome = session.ApprovalRejected
	if result := h.call(t, "write", map[string]any{"file_path": "denied.txt", "content": "x"}); !result.IsError || result.Output != "Error: approval rejected" {
		t.Fatalf("rejected = %#v", result)
	}
	if _, err := os.Stat(h.path("denied.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("rejected write created a file")
	}
}

func TestWrite_ExecutionPointGuardsAndFilesystemFailures(t *testing.T) {
	restoreHooks(t)
	h := newHarness(t)
	approved := appTool.Invocation{Approved: true, Journal: nopJournal{}}
	if _, err := h.provider.write(context.Background(), appTool.Invocation{}, writeArgs{FilePath: "x"}); err == nil || !strings.Contains(err.Error(), "approval was not granted") {
		t.Fatalf("unapproved = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.provider.write(ctx, approved, writeArgs{FilePath: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	// Execution re-checks confinement after approval in case the target changed.
	if _, err := h.provider.write(context.Background(), approved, writeArgs{FilePath: "../x"}); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("execution-point confinement = %v", err)
	}
	failure := errors.New("io failure")
	lstatFile = func(string) (os.FileInfo, error) { return nil, failure }
	if _, err := h.provider.write(context.Background(), approved, writeArgs{FilePath: "x"}); !errors.Is(err, failure) {
		t.Fatalf("lstat = %v", err)
	}
	lstatFile = os.Lstat
	makeDirs = func(string, os.FileMode) error { return failure }
	if _, err := h.provider.write(context.Background(), approved, writeArgs{FilePath: "a/b"}); !errors.Is(err, failure) {
		t.Fatalf("mkdir = %v", err)
	}
	makeDirs = os.MkdirAll
	createTemp = func(string, string) (stagedFile, error) { return nil, failure }
	if _, err := h.provider.write(context.Background(), approved, writeArgs{FilePath: "x"}); !errors.Is(err, failure) {
		t.Fatalf("stage = %v", err)
	}
}

func TestEdit_ReplacesLiteralTextLikeUpstream(t *testing.T) {
	h := newHarness(t)
	for _, test := range []struct {
		name, content string
		arguments     map[string]any
		want          string
		all           bool
	}{
		{name: "unique", content: "a\nneedle\nb\n", arguments: map[string]any{"old_string": "needle", "new_string": "pin"}, want: "a\npin\nb\n"},
		{name: "delete", content: "keep drop keep", arguments: map[string]any{"old_string": " drop", "new_string": ""}, want: "keep keep"},
		{name: "all", content: "x-x-x", arguments: map[string]any{"old_string": "x", "new_string": "y", "replace_all": true}, want: "y-y-y", all: true},
		{name: "explicit single", content: "only once", arguments: map[string]any{"old_string": "once", "new_string": "twice", "replace_all": false}, want: "only twice"},
		{name: "crlf", content: "one\r\ntwo\r\nthree\r\n", arguments: map[string]any{"old_string": "one\ntwo", "new_string": "1\r\n2\n2b"}, want: "1\r\n2\r\n2b\r\nthree\r\n"},
		{name: "mixed lf", content: "a\r\nb\nc\n", arguments: map[string]any{"old_string": "c", "new_string": "d"}, want: "a\nb\nd\n"},
		{name: "bom", content: "\ufeffhead\ntail", arguments: map[string]any{"old_string": "head", "new_string": "top"}, want: "\ufefftop\ntail"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := h.path(test.name + ".txt")
			writeFixture(t, path, test.content)
			h.read(t, test.name+".txt")
			test.arguments["file_path"] = test.name + ".txt"
			result := h.call(t, "edit", test.arguments)
			want := fmt.Sprintf("The file %s has been updated successfully.", path)
			if test.all {
				want = fmt.Sprintf("The file %s has been updated. All occurrences were successfully replaced.", path)
			}
			if result.IsError || result.Output != want || readFixture(t, path) != test.want {
				t.Fatalf("edit = %#v, content = %q", result, readFixture(t, path))
			}
		})
	}
	if h.approver.reasons[0] != `edit file "unique.txt"` {
		t.Fatalf("reason = %q", h.approver.reasons[0])
	}
}

func TestEdit_RejectsInvalidAndUnsafeEdits(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("file.txt"), "dup dup")
	// A NUL past the read tool's binary sample is readable but not editable.
	writeFixture(t, h.path("binary"), strings.Repeat("a", binarySampleBytes)+"\x00b")
	writeFixture(t, h.path("latin1"), "caf\xe9")
	if err := os.Mkdir(h.path("dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.path("file.txt"), h.path("alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.path("large"), []byte(strings.Repeat("a", maxEditBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"file.txt", "binary", "large"} {
		h.read(t, path)
	}
	if result := h.call(t, "read", map[string]any{"file_path": "absent.txt"}); !result.IsError {
		t.Fatal("read of a missing file succeeded")
	}
	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{"file_path": "file.txt", "old_string": "dup"}, `missing required property "new_string"`},
		{map[string]any{"file_path": " ", "old_string": "a", "new_string": "b"}, "file_path must be a non-empty string"},
		{map[string]any{"file_path": "file.txt", "old_string": "", "new_string": "b"}, "old_string must be a non-empty string"},
		{map[string]any{"file_path": "file.txt", "old_string": "same", "new_string": "same"}, "old_string and new_string must differ"},
		{map[string]any{"file_path": "file.txt", "old_string": "a", "new_string": "b", "replace_all": "yes"}, `"replace_all" must be a boolean`},
		{map[string]any{"file_path": "file.txt", "old_string": "dup", "new_string": "x"}, fmt.Sprintf("old_string matched 2 times in %q; provide a more specific old_string or set replace_all to true", h.path("file.txt"))},
		{map[string]any{"file_path": "file.txt", "old_string": "absent", "new_string": "x"}, fmt.Sprintf("old_string was not found in %q", h.path("file.txt"))},
		{map[string]any{"file_path": "missing.txt", "old_string": "a", "new_string": "b"}, fmt.Sprintf("cannot modify %q: file has not been read — read the file, then retry", h.path("missing.txt"))},
		{map[string]any{"file_path": "absent.txt", "old_string": "a", "new_string": "b"}, fmt.Sprintf("cannot edit %q: not found", h.path("absent.txt"))},
		{map[string]any{"file_path": "dir", "old_string": "a", "new_string": "b"}, "file has not been read"},
		{map[string]any{"file_path": "alias", "old_string": "dup", "new_string": "b"}, "path crosses a symbolic link"},
		{map[string]any{"file_path": "../x", "old_string": "a", "new_string": "b"}, "path is outside the workspace"},
		{map[string]any{"file_path": "binary", "old_string": "a", "new_string": "b"}, "binary file"},
		{map[string]any{"file_path": "latin1", "old_string": "caf", "new_string": "b"}, "file has not been read"},
		{map[string]any{"file_path": "large", "old_string": "a", "new_string": "b"}, "exceeds the 10485760-byte limit"},
	} {
		result := h.call(t, "edit", test.arguments)
		if !result.IsError || !strings.Contains(result.Output, test.want) {
			t.Errorf("edit(%v) = %#v, want %q", test.arguments, result, test.want)
		}
	}
	if readFixture(t, h.path("file.txt")) != "dup dup" {
		t.Fatal("rejected edits changed the file")
	}
}

func TestEdit_ExecutionPointGuardsAndFilesystemFailures(t *testing.T) {
	restoreHooks(t)
	h := newHarness(t)
	writeFixture(t, h.path("file.txt"), "old")
	approved := appTool.Invocation{SessionID: "s", Approved: true, Journal: nopJournal{}}
	h.provider.observed.record("s", h.path("file.txt"), observed([]byte("old")))
	arguments := editArgs{FilePath: "file.txt", OldString: "old", NewString: "new"}
	if _, err := h.provider.edit(context.Background(), appTool.Invocation{}, arguments); err == nil || !strings.Contains(err.Error(), "approval was not granted") {
		t.Fatalf("unapproved = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.provider.edit(ctx, approved, arguments); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	if _, err := h.provider.edit(context.Background(), approved, editArgs{FilePath: "../x", OldString: "a", NewString: "b"}); err == nil || !strings.Contains(err.Error(), "outside the workspace") {
		t.Fatalf("execution-point confinement = %v", err)
	}
	failure := errors.New("io failure")
	lstatFile = func(string) (os.FileInfo, error) { return nil, failure }
	if _, err := h.provider.edit(context.Background(), approved, arguments); !errors.Is(err, failure) {
		t.Fatalf("lstat = %v", err)
	}
	lstatFile = os.Lstat
	openFile = func(string) (io.ReadCloser, error) { return nil, failure }
	if _, err := h.provider.edit(context.Background(), approved, arguments); !errors.Is(err, failure) {
		t.Fatalf("open = %v", err)
	}
	openFile = openFailing([]byte("o"), failure)
	if _, err := h.provider.edit(context.Background(), approved, arguments); !errors.Is(err, failure) {
		t.Fatalf("read = %v", err)
	}
	openFile = openFailing(make([]byte, maxEditBytes+1), io.EOF)
	if _, err := h.provider.edit(context.Background(), approved, arguments); err == nil || !strings.Contains(err.Error(), "content exceeds") {
		t.Fatalf("grown file = %v", err)
	}
	openFile = openFailing([]byte("old"), io.EOF)
	createTemp = func(string, string) (stagedFile, error) { return nil, failure }
	if _, err := h.provider.edit(context.Background(), approved, arguments); !errors.Is(err, failure) {
		t.Fatalf("stage = %v", err)
	}
	if readFixture(t, h.path("file.txt")) != "old" {
		t.Fatal("failed edit changed the file")
	}
}
