package file

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

// awkwardName holds the characters Go's %q would escape or a shell would
// split on: a double quote, a backslash, a space, and non-ASCII text.
const awkwardName = `say "hi" back\slash 文件`

// TestFileTools_QuotePathsLikeUpstream proves model-visible file errors put
// the path between plain double quotes, byte for byte like upstream's
// `"${displayPath}"`. Expectations are written in upstream's format by
// concatenation; nothing below formats a path with %q.
func TestFileTools_QuotePathsLikeUpstream(t *testing.T) {
	h := newHarness(t)
	base := awkwardName
	path := func(suffix string) string { return h.path(base + suffix) }
	writeFixture(t, path(".txt"), "one\ntwo two\n")
	writeFixture(t, path(".bin"), "a\x00b")
	writeFixture(t, path(".unread"), "kept")
	writeFixture(t, path(".stale"), "before")
	if err := os.Mkdir(path(".dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	h.read(t, base+".stale")
	writeFixture(t, path(".stale"), "changed")
	if result := h.call(t, "read", map[string]any{"file_path": base + ".absent"}); !result.IsError {
		t.Fatal("read of a missing file succeeded")
	}
	h.read(t, base+".txt")
	for _, test := range []struct {
		tool      string
		arguments map[string]any
		want      string
	}{
		{"read", map[string]any{"file_path": base + ".missing"}, `Error: cannot read "` + path(".missing") + `": not found`},
		{"read", map[string]any{"file_path": base + ".dir"}, `Error: cannot read "` + path(".dir") + `": not a regular file`},
		{"read", map[string]any{"file_path": base + ".bin"}, `Error: cannot read "` + path(".bin") + `": binary file`},
		{"read", map[string]any{"file_path": base + ".txt", "offset": 9}, `Error: offset 9 is out of range for "` + path(".txt") + `" (2 lines)`},
		{"write", map[string]any{"file_path": base + ".unread", "content": "x"}, `Error: cannot modify "` + path(".unread") + `": file has not been read — read the file, then retry`},
		{"write", map[string]any{"file_path": base + ".stale", "content": "x"}, `Error: cannot write "` + path(".stale") + `": file changed since it was read — re-read the file, then retry`},
		{"write", map[string]any{"file_path": base + ".dir", "content": "x"}, `Error: cannot write "` + path(".dir") + `": not a regular file`},
		{"edit", map[string]any{"file_path": base + ".absent", "old_string": "a", "new_string": "b"}, `Error: cannot edit "` + path(".absent") + `": not found`},
		{"edit", map[string]any{"file_path": base + ".txt", "old_string": "three", "new_string": "3"}, `Error: old_string was not found in "` + path(".txt") + `"`},
		{"edit", map[string]any{"file_path": base + ".txt", "old_string": "two", "new_string": "2"}, `Error: old_string matched 2 times in "` + path(".txt") + `"; provide a more specific old_string or set replace_all to true`},
		{"read_image", map[string]any{"file_path": base + ".txt"}, `Error: cannot read "` + base + `.txt": the .txt extension does not declare a supported image format; read_image accepts PNG/JPEG/WebP/GIF files, including extension-less files in those formats`},
	} {
		if result := h.call(t, test.tool, test.arguments); result.Output != test.want {
			t.Errorf("%s(%v)\n got: %s\nwant: %s", test.tool, test.arguments, result.Output, test.want)
		}
	}
	if result := h.readImage(t, appTool.Route{}, base+".png")[0]; result.Output != `Error: cannot read "`+base+`.png" as an image: the current model route could not be resolved` {
		t.Errorf("read_image route = %s", result.Output)
	}
	if result := h.readImage(t, appTool.Route{Provider: "openai", Model: `m"odel`}, base+".png")[0]; result.Output != `Error: cannot read "`+base+`.png" as an image: model "m"odel" does not declare image input; switch to an image-capable model to read images` {
		t.Errorf("read_image model = %s", result.Output)
	}
	// A path running through a regular file reports the traversed segment.
	if result := h.call(t, "read", map[string]any{"file_path": base + ".txt/inner"}); !strings.Contains(result.Output, `cannot traverse "`+path(".txt")+`": parent path segment is not a directory`) {
		t.Errorf("traversal = %s", result.Output)
	}
}

// TestGuardedCreateFailure_QuotesPathsLikeUpstream covers the three guarded
// create outcomes whose texts come from fsio.ts and error.ts.
func TestGuardedCreateFailure_QuotesPathsLikeUpstream(t *testing.T) {
	restoreHooks(t)
	target := "/work/" + awkwardName
	for _, test := range []struct {
		name     string
		info     fs.FileInfo
		metadata error
		cause    error
		want     string
	}{
		{"directory", dirInfo{}, nil, fs.ErrExist, `cannot write "` + target + `": not a regular file`},
		{"vanished", nil, fs.ErrNotExist, fs.ErrExist, `cannot modify "` + target + `": file has not been read — read the file, then retry`},
		{"metadata", nil, errors.New("metadata probe failed"), syscall.EIO, `cannot write "` + target + `": metadata probe failed`},
	} {
		t.Run(test.name, func(t *testing.T) {
			lstatFile = func(string) (fs.FileInfo, error) { return test.info, test.metadata }
			if got := guardedCreateFailure(target, test.cause).Error(); got != test.want {
				t.Fatalf("got  %s\nwant %s", got, test.want)
			}
		})
	}
}

// dirInfo is a directory entry for injected metadata.
type dirInfo struct{ fs.FileInfo }

func (dirInfo) Mode() fs.FileMode { return fs.ModeDir | 0o700 }
