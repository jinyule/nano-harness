package file

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestEditResult_UsesActualChangesInsideLargeReplacement(t *testing.T) {
	line := strings.Repeat("a", 199)
	for _, test := range []struct {
		name      string
		positions []int
	}{
		{"one change", []int{500}},
		{"scattered changes", []int{250, 750}},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			fixtureLine := func(index int) string {
				if len(test.positions) == 1 {
					return line
				}
				return fmt.Sprintf("%04d %s", index, strings.Repeat("a", 194))
			}
			var fixture strings.Builder
			for index := range 1000 {
				fixture.WriteString(fixtureLine(index) + "\n")
			}
			before := fixture.String()
			after := []byte(before)
			want := &session.ToolMeta{Edit: &session.EditMeta{Diffs: []session.FileDiff{}}}
			for _, position := range test.positions {
				after[position*200] = 'b'
				var oldLines, newLines []string
				for index := position - 3; index <= position+3; index++ {
					text := fixtureLine(index)
					oldLines = append(oldLines, text)
					if index == position {
						text = "b" + text[1:]
					}
					newLines = append(newLines, text)
				}
				oldHunk, newHunk := strings.Join(oldLines, "\n"), strings.Join(newLines, "\n")
				want.Edit.Diffs = append(want.Edit.Diffs, session.FileDiff{Path: h.path("f"), OldText: new(oldHunk), NewText: newHunk})
			}
			writeFixture(t, h.path("f"), before)
			h.read(t, "f")
			result := h.call(t, "edit", map[string]any{"file_path": "f", "old_string": before, "new_string": string(after)})
			if got := readFixture(t, h.path("f")); got != string(after) {
				t.Fatal("published file differs from the requested edit")
			}
			result = persistedFileResult(t, "edit", result)
			if result.IsError || result.Output != fmt.Sprintf("The file %s has been updated successfully.", h.path("f")) || !reflect.DeepEqual(result.Meta, want) {
				t.Fatalf("large replacement meta = %+v, want %+v", result.Meta.Edit, want.Edit)
			}
		})
	}
}

func TestReadResult_PermissionFailureStaysUnclassified(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("locked"), "content")
	if err := os.Chmod(h.path("locked"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(h.path("locked"), 0o600) })
	result := h.call(t, "read", map[string]any{"file_path": "locked"})
	if !result.IsError {
		t.Skip("current OS or effective user does not enforce this permission fixture; injected I/O tests cover the same boundary")
	}
	result = persistedFileResult(t, "read", result)
	if result.Error != nil || result.Meta != nil || !strings.Contains(result.Output, "permission denied") {
		t.Fatalf("read permission result = %+v, want an ordinary error without metadata", result)
	}
}

func TestWriteResult_CreateDirectoryCollisionIsNotRegular(t *testing.T) {
	h := newHarness(t)
	restoreHooks(t)
	linkFile = func(from, target string) error {
		if err := os.Mkdir(target, 0o700); err != nil {
			return err
		}
		return os.Link(from, target)
	}
	result := persistedFileResult(t, "write", h.call(t, "write", map[string]any{"file_path": "new", "content": "data"}))
	wantText := fmt.Sprintf("Error: cannot modify %q: file has not been read — read the file, then retry", h.path("new"))
	if !result.IsError || result.Error == nil || *result.Error != (session.ToolError{Name: "FsError", Code: "FS_NOT_REGULAR_FILE"}) || result.Meta != nil || result.Output != wantText {
		t.Fatalf("directory collision = %+v, want FS_NOT_REGULAR_FILE with unchanged text", result)
	}
	info, err := os.Stat(h.path("new"))
	if err != nil || !info.IsDir() {
		t.Fatalf("collision target was changed: %v, %v", info, err)
	}
	entries, err := os.ReadDir(h.root.Path())
	if err != nil || len(entries) != 1 || entries[0].Name() != "new" {
		t.Fatalf("staging residue after collision: %v, %v", entries, err)
	}
}

func TestFileResults_OrdinaryIOStaysUnclassified(t *testing.T) {
	for _, cause := range []error{fs.ErrPermission, errors.New("disk failure")} {
		for _, test := range []struct {
			name, tool string
			inject     func(error)
		}{
			{"read stat", "read", func(err error) { statFile = func(string) (fs.FileInfo, error) { return nil, err } }},
			{"read open", "read", func(err error) { openFile = func(string) (io.ReadCloser, error) { return nil, err } }},
			{"read content", "read", func(err error) { openFile = openFailing(nil, err) }},
			{"image stat", "read_image", func(err error) { statFile = func(string) (fs.FileInfo, error) { return nil, err } }},
			{"image open", "read_image", func(err error) { openFile = func(string) (io.ReadCloser, error) { return nil, err } }},
			{"image content", "read_image", func(err error) { openFile = openFailing(nil, err) }},
			{"write stat", "write", func(err error) { lstatFile = func(string) (fs.FileInfo, error) { return nil, err } }},
			{"write digest", "write", func(err error) { openFile = openFailing(nil, err) }},
			{"write mkdir", "write", func(err error) { makeDirs = func(string, fs.FileMode) error { return err } }},
			{"write stage", "write", func(err error) { createTemp = func(string, string) (stagedFile, error) { return nil, err } }},
			{"write rename", "write", func(err error) { renameFile = func(string, string) error { return err } }},
			{"edit stat", "edit", func(err error) { lstatFile = func(string) (fs.FileInfo, error) { return nil, err } }},
			{"edit open", "edit", func(err error) { openFile = func(string) (io.ReadCloser, error) { return nil, err } }},
			{"edit content", "edit", func(err error) { openFile = openFailing(nil, err) }},
			{"edit stage", "edit", func(err error) { createTemp = func(string, string) (stagedFile, error) { return nil, err } }},
			{"edit rename", "edit", func(err error) { renameFile = func(string, string) error { return err } }},
		} {
			t.Run(cause.Error()+"/"+test.name, func(t *testing.T) {
				h := newHarness(t)
				writeFixture(t, h.path("f"), "before")
				h.read(t, "f")
				restoreHooks(t)
				test.inject(cause)
				args := map[string]any{"file_path": "f", "content": "after", "old_string": "before", "new_string": "after"}
				var result session.ToolResult
				switch test.tool {
				case "read_image":
					result = h.readImage(t, visionRoute, "f")[0]
				case "read":
					result = h.call(t, "read", map[string]any{"file_path": "f"})
				case "write":
					delete(args, "old_string")
					delete(args, "new_string")
					result = h.call(t, "write", args)
				case "edit":
					delete(args, "content")
					result = h.call(t, "edit", args)
				}
				result = persistedFileResult(t, test.tool, result)
				verb := test.tool
				if verb == "read_image" {
					verb = "read"
				}
				wantText := fmt.Sprintf("Error: cannot %s %q: %v", verb, h.path("f"), cause)
				if !result.IsError || result.Error != nil || result.Meta != nil || result.Output != wantText {
					t.Fatalf("ordinary I/O = %+v, want unclassified %v", result, cause)
				}
				if readFixture(t, h.path("f")) != "before" {
					t.Fatal("failed I/O changed the target")
				}
			})
		}
	}
}

func TestReadResult_PostStatMissingStaysUnclassified(t *testing.T) {
	for _, name := range []string{"read", "read_image"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			writeFixture(t, h.path("f"), "before")
			restoreHooks(t)
			openFile = func(string) (io.ReadCloser, error) { return nil, fs.ErrNotExist }
			result := h.readImage(t, visionRoute, "f")[0]
			if name == "read" {
				result = h.call(t, "read", map[string]any{"file_path": "f"})
			}
			result = persistedFileResult(t, name, result)
			if !result.IsError || result.Error != nil || result.Meta != nil {
				t.Fatalf("post-stat missing = %+v, want an unclassified open failure", result)
			}
		})
	}
}

func TestFileResults_StagingIOStaysUnclassified(t *testing.T) {
	for _, tool := range []string{"write", "edit"} {
		for _, operation := range []string{"write", "sync", "chmod", "close"} {
			t.Run(tool+"/"+operation, func(t *testing.T) {
				h := newHarness(t)
				writeFixture(t, h.path("f"), "before")
				h.read(t, "f")
				restoreHooks(t)
				cause := errors.New("staging " + operation + " failure")
				createTemp = func(directory, pattern string) (stagedFile, error) {
					file, err := os.CreateTemp(directory, pattern)
					return &publicationFaultFile{File: file, operation: operation, err: cause}, err
				}
				args := map[string]any{"file_path": "f", "content": "after"}
				if tool == "edit" {
					args = map[string]any{"file_path": "f", "old_string": "before", "new_string": "after"}
				}
				result := persistedFileResult(t, tool, h.call(t, tool, args))
				wantText := fmt.Sprintf("Error: cannot %s %q: %v", tool, h.path("f"), cause)
				if !result.IsError || result.Error != nil || result.Meta != nil || result.Output != wantText || readFixture(t, h.path("f")) != "before" {
					t.Fatalf("staging I/O = %+v, want ordinary failure and unchanged target", result)
				}
				if entries, err := os.ReadDir(h.root.Path()); err != nil || len(entries) != 1 {
					t.Fatalf("staging residue = %v, %v", entries, err)
				}
			})
		}
	}
}

type publicationFaultFile struct {
	*os.File
	operation string
	err       error
}

func (file *publicationFaultFile) Write(data []byte) (int, error) {
	if file.operation == "write" {
		return 0, file.err
	}
	return file.File.Write(data)
}

func (file *publicationFaultFile) Sync() error {
	if file.operation == "sync" {
		return file.err
	}
	return file.File.Sync()
}

func (file *publicationFaultFile) Chmod(mode fs.FileMode) error {
	if file.operation == "chmod" {
		return file.err
	}
	return file.File.Chmod(mode)
}

func (file *publicationFaultFile) Close() error {
	err := file.File.Close()
	if file.operation == "close" {
		return errors.Join(err, file.err)
	}
	return err
}

func TestWriteResult_ClassifiesOnlyFailedCreatePublication(t *testing.T) {
	for _, test := range []struct {
		name, code      string
		cause, metadata error
		collide         bool
	}{
		{"regular collision", "FS_NOT_OBSERVED", nil, nil, true},
		{"link I/O", "FS_IO_ERROR", syscall.EIO, fs.ErrNotExist, false},
		{"link permissions", "FS_IO_ERROR", fs.ErrPermission, fs.ErrNotExist, false},
		{"metadata I/O", "FS_IO_ERROR", syscall.EIO, fs.ErrPermission, false},
		{"parent vanished", "FS_IO_ERROR", syscall.EIO, syscall.ENOTDIR, false},
		{"collision vanished", "FS_NOT_OBSERVED", fs.ErrExist, fs.ErrNotExist, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			restoreHooks(t)
			var publicationCause error
			linkFile = func(from, target string) error {
				if test.collide {
					writeFixture(t, target, "external")
					publicationCause = os.Link(from, target)
				} else {
					publicationCause = test.cause
					lstatFile = func(string) (fs.FileInfo, error) { return nil, test.metadata }
				}
				return publicationCause
			}
			result := persistedFileResult(t, "write", h.call(t, "write", map[string]any{"file_path": "new", "content": "data"}))
			wantText := fmt.Sprintf("Error: cannot write %q: %v", h.path("new"), publicationCause)
			if test.collide {
				wantText = fmt.Sprintf("Error: cannot modify %q: file has not been read — read the file, then retry", h.path("new"))
				if readFixture(t, h.path("new")) != "external" {
					t.Fatal("external creator was overwritten")
				}
			} else if _, err := os.Stat(h.path("new")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("failed create published a target: %v", err)
			}
			if !result.IsError || result.Error == nil || *result.Error != (session.ToolError{Name: "FsError", Code: test.code}) || result.Meta != nil || result.Output != wantText {
				t.Fatalf("create publication = %+v, want FsError/%s with unchanged text", result, test.code)
			}
			failure := guardedCreateFailure(h.path("new"), publicationCause)
			if !errors.Is(failure, publicationCause) || test.metadata != nil && !errors.Is(test.metadata, fs.ErrNotExist) && !errors.Is(test.metadata, syscall.ENOTDIR) && !errors.Is(failure, test.metadata) {
				t.Fatalf("publication causes lost: %v", failure)
			}
		})
	}
	t.Run("staging failure with directory collision", func(t *testing.T) {
		h := newHarness(t)
		restoreHooks(t)
		createTemp = func(string, string) (stagedFile, error) {
			if err := os.Mkdir(h.path("new"), 0o700); err != nil {
				t.Fatal(err)
			}
			return nil, syscall.EIO
		}
		result := persistedFileResult(t, "write", h.call(t, "write", map[string]any{"file_path": "new", "content": "data"}))
		wantText := fmt.Sprintf("Error: cannot modify %q: file has not been read — read the file, then retry", h.path("new"))
		if !result.IsError || result.Error != nil || result.Meta != nil || result.Output != wantText {
			t.Fatalf("pre-publication I/O = %+v, want unclassified failure", result)
		}
		if err := os.Remove(h.path("new")); err != nil {
			t.Fatal(err)
		}
		_, plain := h.provider.write(t.Context(), appTool.Invocation{Approved: true, Journal: nopJournal{}}, writeArgs{FilePath: "new", Content: "data"})
		var classified appTool.Failure
		if !errors.Is(plain, syscall.EIO) || errors.As(plain, &classified) || "Error: "+plain.Error() != wantText {
			t.Fatalf("ordinary cause was lost or classified: %v", plain)
		}
	})
}

func TestChangedLines_ProducesShortestValidPaths(t *testing.T) {
	// Exhaustive small sequences check the edit distance against an independent
	// dynamic-programming LCS oracle, including repeated lines and both parities.
	sequences := [][]string{nil}
	for size := 1; size <= 4; size++ {
		previous := slices.Clone(sequences)
		for _, sequence := range previous {
			if len(sequence) != size-1 {
				continue
			}
			for _, line := range []string{"a\n", "b\n", "c"} {
				sequences = append(sequences, append(slices.Clone(sequence), line))
			}
		}
	}
	for _, a := range sequences {
		for _, b := range sequences {
			changes := changedLines(a, b, 0, 0)
			posA, posB, cost := 0, 0, 0
			for _, change := range changes {
				if change.startA < posA || change.startB < posB || change.endA < change.startA || change.endB < change.startB || change.endA > len(a) || change.endB > len(b) || change.startA == change.endA && change.startB == change.endB || !slices.Equal(a[posA:change.startA], b[posB:change.startB]) {
					t.Fatalf("invalid path %v -> %v: %v", a, b, changes)
				}
				cost += change.endA - change.startA + change.endB - change.startB
				posA, posB = change.endA, change.endB
			}
			if !slices.Equal(a[posA:], b[posB:]) {
				t.Fatalf("invalid tail %v -> %v: %v", a, b, changes)
			}
			lcs := make([][]int, len(a)+1)
			for index := range lcs {
				lcs[index] = make([]int, len(b)+1)
			}
			for i := range a {
				for j := range b {
					if a[i] == b[j] {
						lcs[i+1][j+1] = lcs[i][j] + 1
					} else {
						lcs[i+1][j+1] = max(lcs[i][j+1], lcs[i+1][j])
					}
				}
			}
			if want := len(a) + len(b) - 2*lcs[len(a)][len(b)]; cost != want {
				t.Fatalf("path %v -> %v has cost %d, want %d: %v", a, b, cost, want, changes)
			}
		}
	}
}
