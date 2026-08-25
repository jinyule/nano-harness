package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

func execution(arguments string) appTool.Execution {
	return appTool.Execution{Arguments: json.RawMessage(arguments)}
}

func TestReadTool_ReadsBoundedTextRanges(t *testing.T) {
	restoreWorkspaceHooks(t)
	root := testRoot(t)
	owner := &Provider{root: root}
	tool := readTool{owner: owner}
	path := filepath.Join(root, "text.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tool.Definition().Name != "read_file" || tool.Concurrency() != appTool.ConcurrencyParallel || tool.ApprovalReason(nil) != "" {
		t.Fatal("read tool metadata is invalid")
	}

	output, err := tool.Execute(context.Background(), execution(`{"path":"text.txt","offset":2,"limit":1}`))
	if err != nil || output != "2: beta" {
		t.Fatalf("range output = %q, error = %v", output, err)
	}
	output, err = tool.Execute(context.Background(), execution(`{"path":"text.txt"}`))
	if err != nil || output != "1: alpha\n2: beta\n3: gamma" {
		t.Fatalf("default output = %q, error = %v", output, err)
	}
	output, err = tool.Execute(context.Background(), execution(`{"path":"text.txt","offset":99}`))
	if err != nil || output != "" {
		t.Fatalf("past-end output = %q, error = %v", output, err)
	}
}

func TestReadTool_RejectsInvalidUnsafeAndUnreadableInput(t *testing.T) {
	restoreWorkspaceHooks(t)
	root := testRoot(t)
	tool := readTool{owner: &Provider{root: root}}
	for _, raw := range []string{
		`{`, `{"path":""}`, `{"path":"x","offset":-1}`, `{"path":"x","limit":-1}`, `{"path":"x","limit":2001}`,
	} {
		if _, err := tool.Execute(context.Background(), execution(raw)); err == nil {
			t.Fatalf("accepted arguments %s", raw)
		}
	}
	if _, err := tool.Execute(context.Background(), execution(`{"path":"missing"}`)); err == nil {
		t.Fatal("missing path accepted")
	}
	if _, err := tool.Execute(context.Background(), execution(`{"path":"."}`)); err == nil || !strings.Contains(err.Error(), "regular text") {
		t.Fatalf("directory error = %v", err)
	}

	large := filepath.Join(root, "large")
	if err := os.WriteFile(large, make([]byte, maxReadBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), execution(`{"path":"large"}`)); err == nil || !strings.Contains(err.Error(), "read limit") {
		t.Fatalf("large error = %v", err)
	}
	invalid := filepath.Join(root, "invalid")
	if err := os.WriteFile(invalid, []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), execution(`{"path":"invalid"}`)); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("UTF-8 error = %v", err)
	}
	if err := os.WriteFile(invalid, []byte{'a', 0, 'b'}, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(context.Background(), execution(`{"path":"invalid"}`)); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("NUL error = %v", err)
	}

	workspaceRead = func(string) ([]byte, error) { return nil, errors.New("read") }
	if _, err := tool.Execute(context.Background(), execution(`{"path":"invalid"}`)); err == nil || err.Error() != "read" {
		t.Fatalf("read error = %v", err)
	}
	workspaceRead = os.ReadFile
	if err := os.WriteFile(invalid, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(ctx, execution(`{"path":"invalid"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("context error = %v", err)
	}
}

func TestListTool_ListsSortedDepthBoundedEntries(t *testing.T) {
	restoreWorkspaceHooks(t)
	root := testRoot(t)
	for _, name := range []string{"b.txt", "a.txt", "dir/inside.txt", "dir/deep/hidden.txt"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tool := listTool{owner: &Provider{root: root}}
	if tool.Definition().Name != "list_files" || tool.Concurrency() != appTool.ConcurrencyParallel || tool.ApprovalReason(nil) != "" {
		t.Fatal("list tool metadata is invalid")
	}
	output, err := tool.Execute(context.Background(), execution(`{"path":".","depth":1}`))
	if err != nil || output != "a.txt\nb.txt\ndir/" {
		t.Fatalf("depth output = %q, error = %v", output, err)
	}
	output, err = tool.Execute(context.Background(), execution(`{"path":"."}`))
	if err != nil || !strings.Contains(output, "dir/inside.txt") || !strings.Contains(output, "dir/deep/") || strings.Contains(output, "hidden.txt") {
		t.Fatalf("default output = %q, error = %v", output, err)
	}
}

func TestListTool_PropagatesValidationWalkCancellationAndLimits(t *testing.T) {
	restoreWorkspaceHooks(t)
	root := testRoot(t)
	tool := listTool{owner: &Provider{root: root}}
	for _, raw := range []string{`{`, `{"path":".","depth":-1}`, `{"path":".","depth":9}`, `{"path":"missing"}`} {
		if _, err := tool.Execute(context.Background(), execution(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}

	walkFailure := errors.New("walk")
	workspaceWalk = func(root string, callback fs.WalkDirFunc) error {
		return callback(root, nil, walkFailure)
	}
	if _, err := tool.Execute(context.Background(), execution(`{"path":"."}`)); !errors.Is(err, walkFailure) {
		t.Fatalf("walk error = %v", err)
	}
	workspaceWalk = filepath.WalkDir
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(ctx, execution(`{"path":"."}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("context error = %v", err)
	}

	workspaceWalk = func(root string, callback fs.WalkDirFunc) error {
		entry := fakeDirEntry{name: "dir", mode: os.ModeDir}
		for index := 0; index <= maxWalkEntries; index++ {
			if err := callback(filepath.Join(root, "file"+strings.Repeat("x", index%2)), entry, nil); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := tool.Execute(context.Background(), execution(`{"path":"."}`)); err == nil || !strings.Contains(err.Error(), "entry limit") {
		t.Fatalf("entry limit error = %v", err)
	}
}

func TestSearchTool_FindsSortedMatchesAndStopsAtLimit(t *testing.T) {
	restoreWorkspaceHooks(t)
	root := testRoot(t)
	for name, data := range map[string]string{"b.txt": "match b\nnone", "a.txt": "none\nmatch a"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "large"), make([]byte, maxSearchBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a.txt"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	tool := searchTool{owner: &Provider{root: root}}
	if tool.Definition().Name != "search_files" || tool.Concurrency() != appTool.ConcurrencyParallel || tool.ApprovalReason(nil) != "" {
		t.Fatal("search tool metadata is invalid")
	}
	output, err := tool.Execute(context.Background(), execution(`{"path":".","pattern":"match"}`))
	if err != nil || output != "a.txt:2:match a\nb.txt:1:match b" {
		t.Fatalf("search output = %q, error = %v", output, err)
	}

	many := strings.Repeat("match\n", maxSearchHits+10)
	if err := os.WriteFile(filepath.Join(root, "many"), []byte(many), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err = tool.Execute(context.Background(), execution(`{"path":"many","pattern":"match"}`))
	if err != nil || len(strings.Split(output, "\n")) != maxSearchHits {
		t.Fatalf("limited matches = %d, error = %v", len(strings.Split(output, "\n")), err)
	}
}

func TestSearchTool_RejectsAndPropagatesBoundaryFailures(t *testing.T) {
	restoreWorkspaceHooks(t)
	root := testRoot(t)
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("match\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tool := searchTool{owner: &Provider{root: root}}
	for _, raw := range []string{
		`{`, `{"path":".","pattern":""}`, `{"path":".","pattern":"` + strings.Repeat("x", 1025) + `"}`,
		`{"path":".","pattern":"["}`, `{"path":"missing","pattern":"x"}`,
	} {
		if _, err := tool.Execute(context.Background(), execution(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}

	walkFailure := errors.New("walk")
	workspaceWalk = func(root string, callback fs.WalkDirFunc) error { return callback(root, nil, walkFailure) }
	if _, err := tool.Execute(context.Background(), execution(`{"path":".","pattern":"x"}`)); !errors.Is(err, walkFailure) {
		t.Fatalf("walk error = %v", err)
	}
	workspaceWalk = filepath.WalkDir
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tool.Execute(ctx, execution(`{"path":".","pattern":"x"}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("context error = %v", err)
	}

	workspaceWalk = func(root string, callback fs.WalkDirFunc) error {
		entry := fakeDirEntry{name: "dir", mode: os.ModeDir}
		for index := 0; index <= maxWalkEntries; index++ {
			if err := callback(root, entry, nil); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := tool.Execute(context.Background(), execution(`{"path":".","pattern":"x"}`)); err == nil || !strings.Contains(err.Error(), "entry limit") {
		t.Fatalf("entry limit error = %v", err)
	}

	workspaceWalk = filepath.WalkDir
	workspaceOpen = func(string) (io.ReadCloser, error) { return nil, errors.New("open") }
	if _, err := tool.Execute(context.Background(), execution(`{"path":"file","pattern":"x"}`)); err == nil || err.Error() != "open" {
		t.Fatalf("open error = %v", err)
	}
	workspaceOpen = func(string) (io.ReadCloser, error) {
		return &errorReadCloser{reader: strings.NewReader(strings.Repeat("x", maxReadBytes+1))}, nil
	}
	if _, err := tool.Execute(context.Background(), execution(`{"path":"file","pattern":"x"}`)); err == nil || !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("scan error = %v", err)
	}
	workspaceOpen = func(string) (io.ReadCloser, error) {
		return &errorReadCloser{reader: strings.NewReader("match\n"), closeErr: errors.New("close")}, nil
	}
	if _, err := tool.Execute(context.Background(), execution(`{"path":"file","pattern":"match"}`)); err == nil || err.Error() != "close" {
		t.Fatalf("close error = %v", err)
	}
}

type fakeFileInfo struct {
	name string
	size int64
	mode os.FileMode
}

func (info fakeFileInfo) Name() string      { return info.name }
func (info fakeFileInfo) Size() int64       { return info.size }
func (info fakeFileInfo) Mode() os.FileMode { return info.mode }
func (fakeFileInfo) ModTime() time.Time     { return time.Time{} }
func (info fakeFileInfo) IsDir() bool       { return info.mode.IsDir() }
func (fakeFileInfo) Sys() any               { return nil }

type fakeDirEntry struct {
	name    string
	mode    os.FileMode
	infoErr error
}

func (entry fakeDirEntry) Name() string      { return entry.name }
func (entry fakeDirEntry) IsDir() bool       { return entry.mode.IsDir() }
func (entry fakeDirEntry) Type() os.FileMode { return entry.mode.Type() }
func (entry fakeDirEntry) Info() (os.FileInfo, error) {
	return fakeFileInfo{name: entry.name, mode: entry.mode}, entry.infoErr
}

type errorReadCloser struct {
	reader   io.Reader
	closeErr error
}

func (reader *errorReadCloser) Read(data []byte) (int, error) { return reader.reader.Read(data) }
func (reader *errorReadCloser) Close() error                  { return reader.closeErr }
