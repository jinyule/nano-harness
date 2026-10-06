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
	"unicode/utf8"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
)

func envelope(path, body string) string {
	return "<path>" + path + "</path>\n<type>file</type>\n<content>\n" + body + "\n</content>"
}

func TestRead_RejectsOffsetBeyondSupportedRange(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("file.txt"), "line")
	for _, offset := range []float64{1 << 53, 18014398509481984, 1e300} {
		result := h.call(t, "read", map[string]any{"file_path": "file.txt", "offset": offset})
		want := "Error: offset must be less than or equal to 9007199254740991"
		if !result.IsError || result.Output != want {
			t.Errorf("offset %g = %q, want %q", offset, result.Output, want)
		}
	}
	result := h.call(t, "read", map[string]any{"file_path": "file.txt", "offset": 9007199254740991})
	want := fmt.Sprintf("Error: offset 9007199254740991 is out of range for %q (1 lines)", h.path("file.txt"))
	if result.Output != want {
		t.Errorf("largest supported offset = %q, want %q", result.Output, want)
	}
}

func TestRead_ReturnsUpstreamWindowEnvelope(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("text.txt"), "alpha\r\nbeta\ngamma\n")
	writeFixture(t, h.path("empty.txt"), "")
	writeFixture(t, h.path("bom.txt"), "\ufeffhead\nlast")
	writeFixture(t, h.path("blank.txt"), "\n\r")
	writeFixture(t, h.path("late-nul.txt"), strings.Repeat("x", binarySampleBytes)+"\x00tail")
	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{"file_path": "text.txt"}, envelope(h.path("text.txt"), "1: alpha\n2: beta\n3: gamma\n\n(End of file - total 3 lines)")},
		{map[string]any{"file_path": h.path("text.txt"), "offset": 2, "limit": 1}, envelope(h.path("text.txt"), "2: beta\n\n(Showing lines 2-2 of 3. Use offset=3 to continue.)")},
		{map[string]any{"file_path": "./text.txt", "offset": 3.0}, envelope(h.path("text.txt"), "3: gamma\n\n(End of file - total 3 lines)")},
		{map[string]any{"file_path": "empty.txt"}, envelope(h.path("empty.txt"), "(End of file - total 0 lines)")},
		{map[string]any{"file_path": "bom.txt"}, envelope(h.path("bom.txt"), "1: head\n2: last\n\n(End of file - total 2 lines)")},
		{map[string]any{"file_path": "blank.txt"}, envelope(h.path("blank.txt"), "1: \n2: \n\n(End of file - total 2 lines)")},
		{map[string]any{"file_path": "late-nul.txt", "limit": 1}, envelope(h.path("late-nul.txt"), "1: "+strings.Repeat("x", 2000)+"... (line truncated to 2000 chars)\n\n(End of file - total 1 lines)")},
	} {
		result := h.call(t, "read", test.arguments)
		if result.IsError || result.Output != test.want {
			t.Errorf("read(%v)\n got: %q\nwant: %q", test.arguments, result.Output, test.want)
		}
	}
}

func TestRead_RejectsInvalidArgumentsAndUnsafePaths(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("file.txt"), "one\ntwo\n")
	if err := os.Mkdir(h.path("dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	writeFixture(t, outside, "secret")
	if err := os.Symlink(outside, h.path("escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.path("file.txt"), h.path("alias")); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, h.path("binary"), "a\x00b")
	writeFixture(t, h.path("latin1"), "caf\xe9")
	writeFixture(t, h.path("partial"), "ok\n\xe4\xb8")
	for _, test := range []struct {
		arguments map[string]any
		want      string
	}{
		{map[string]any{}, `missing required property "file_path"`},
		{map[string]any{"file_path": "file.txt", "path": "file.txt"}, `"path" is not a declared property`},
		{map[string]any{"file_path": " "}, "file_path must be a non-empty string"},
		{map[string]any{"file_path": "file.txt", "offset": 0}, "offset must be a positive integer"},
		{map[string]any{"file_path": "file.txt", "offset": 1.5}, "offset must be a positive integer"},
		{map[string]any{"file_path": "file.txt", "limit": -1}, "limit must be a positive integer"},
		{map[string]any{"file_path": "file.txt", "limit": 2001}, "limit must be less than or equal to 2000"},
		{map[string]any{"file_path": "file.txt", "limit": "2"}, `"limit" must be a number`},
		{map[string]any{"file_path": "file.txt", "offset": 4}, fmt.Sprintf("offset 4 is out of range for %q (2 lines)", h.path("file.txt"))},
		{map[string]any{"file_path": "file.txt", "offset": 1e300}, "offset must be less than or equal to"},
		{map[string]any{"file_path": "missing.txt"}, fmt.Sprintf("cannot read %q: not found", h.path("missing.txt"))},
		{map[string]any{"file_path": "dir"}, "not a regular file"},
		{map[string]any{"file_path": "escape"}, "path is outside the workspace"},
		{map[string]any{"file_path": "../" + filepath.Base(filepath.Dir(outside)) + "/secret.txt"}, "path is outside the workspace"},
		{map[string]any{"file_path": outside}, "path is outside the workspace"},
		{map[string]any{"file_path": "binary"}, "binary file"},
		{map[string]any{"file_path": "latin1"}, "invalid UTF-8 text"},
		{map[string]any{"file_path": "partial"}, "invalid UTF-8 text"},
	} {
		result := h.call(t, "read", test.arguments)
		if !result.IsError || !strings.Contains(result.Output, test.want) {
			t.Errorf("read(%v) = %#v, want %q", test.arguments, result, test.want)
		}
	}
	if result := h.call(t, "read", map[string]any{"file_path": "alias"}); result.IsError || !strings.Contains(result.Output, "<path>"+h.path("alias")+"</path>") || !strings.Contains(result.Output, "1: one") {
		t.Fatalf("symlink inside workspace = %#v", result)
	}
	if empty := h.call(t, "read", map[string]any{"file_path": "missing-empty", "offset": 1}); !empty.IsError {
		t.Fatal("missing file read")
	}
	writeFixture(t, h.path("empty"), "")
	if result := h.call(t, "read", map[string]any{"file_path": "empty", "offset": 2}); !result.IsError || !strings.Contains(result.Output, "out of range") {
		t.Fatalf("empty offset 2 = %#v", result)
	}
}

func TestReadWindow_EnforcesLineRuneAndByteCaps(t *testing.T) {
	long := strings.Repeat("界", readMaxLineLength+5) + "\r"
	window, err := readWindow(context.Background(), strings.NewReader(long+"\nnext"), 1, 10)
	if err != nil || window.total != 2 || len(window.lines) != 2 {
		t.Fatalf("window = %+v, %v", window, err)
	}
	if want := strings.Repeat("界", readMaxLineLength) + "... (line truncated to 2000 chars)"; window.lines[0].text != want || window.lines[1].text != "next" {
		t.Fatalf("truncated line = %q", window.lines[0].text[:20])
	}
	exact := strings.Repeat("é", readMaxLineLength) + "\r\n"
	window, _ = readWindow(context.Background(), strings.NewReader(exact), 1, 10)
	if window.lines[0].text != strings.Repeat("é", readMaxLineLength) {
		t.Fatal("a line at the cap was truncated")
	}

	// Each 1900-byte line plus its separator crosses the 50 KiB cap at line 27.
	var many strings.Builder
	for index := range 40 {
		fmt.Fprintf(&many, "%04d%s\n", index, strings.Repeat("y", 1896))
	}
	window, err = readWindow(context.Background(), strings.NewReader(many.String()), 2, 2000)
	if err != nil || !window.capped || window.total != 40 || len(window.lines) != 26 || window.lines[0].number != 2 {
		t.Fatalf("capped window = total %d lines %d capped %v err %v", window.total, len(window.lines), window.capped, err)
	}
	output := formatRead("f", 2, window)
	if !strings.HasSuffix(output, "\n\n(Output capped. Showing lines 2-27. Use offset=28 to continue.)\n</content>") {
		t.Fatalf("capped footer = %q", output[len(output)-120:])
	}
	if formatted := formatRead("f", 5, window2(0)); !strings.Contains(formatted, "(End of file - total 0 lines)") {
		t.Fatalf("empty footer = %q", formatted)
	}

	// A single line longer than the read buffer splits a rune across slices.
	huge := strings.Repeat("a", readBufferBytes-1) + "界" + strings.Repeat("b", 10)
	window, err = readWindow(context.Background(), strings.NewReader(huge+"\nend\n"), 2, 5)
	if err != nil || window.total != 2 || window.lines[0].text != "end" {
		t.Fatalf("huge line = %+v, %v", window, err)
	}
	if _, err := readWindow(context.Background(), strings.NewReader(strings.Repeat("a", readBufferBytes-1)+"\xe7\x95"), 1, 1); !errors.Is(err, errNotText) {
		t.Fatalf("truncated rune at EOF = %v", err)
	}
	if _, err := readWindow(context.Background(), strings.NewReader(strings.Repeat("a", readBufferBytes)+"\x80\x80\x80\x80"), 1, 1); !errors.Is(err, errNotText) {
		t.Fatalf("stray continuation bytes = %v", err)
	}
}

func window2(total int64) window { return window{total: total} }

func TestReadWindow_PropagatesCancellationAndReadFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readWindow(ctx, strings.NewReader("x"), 1, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled = %v", err)
	}
	failure := errors.New("disk")
	if _, err := readWindow(context.Background(), &failingReader{err: failure}, 1, 1); !errors.Is(err, failure) {
		t.Fatalf("peek failure = %v", err)
	}
	reader := &failingReader{data: []byte(strings.Repeat("z", binarySampleBytes) + "\n" + strings.Repeat("q", readBufferBytes)), err: failure}
	if _, err := readWindow(context.Background(), reader, 1, 1); !errors.Is(err, failure) {
		t.Fatalf("slice failure = %v", err)
	}
	if carry, err := validateUTF8(nil, []byte("ok")); err != nil || carry != nil {
		t.Fatalf("complete piece = %q, %v", carry, err)
	}
	if carry, err := validateUTF8([]byte{0xe7}, []byte{0x95, 0x8c, 'x'}); err != nil || len(carry) != 0 {
		t.Fatalf("carried rune = %q, %v", carry, err)
	}
}

func TestRead_SurfacesFilesystemFailures(t *testing.T) {
	restoreHooks(t)
	h := newHarness(t)
	writeFixture(t, h.path("file.txt"), "data")
	failure := errors.New("io failure")
	statFile = func(string) (os.FileInfo, error) { return nil, failure }
	if _, err := h.provider.read(context.Background(), appTool.Invocation{}, readArgs{FilePath: "file.txt"}); !errors.Is(err, failure) {
		t.Fatalf("stat error = %v", err)
	}
	statFile = os.Stat
	openFile = func(string) (io.ReadCloser, error) { return nil, failure }
	if _, err := h.provider.read(context.Background(), appTool.Invocation{}, readArgs{FilePath: "file.txt"}); !errors.Is(err, failure) {
		t.Fatalf("open error = %v", err)
	}
	openFile = openFailing([]byte("x"), failure)
	if _, err := h.provider.read(context.Background(), appTool.Invocation{}, readArgs{FilePath: "file.txt"}); !errors.Is(err, failure) {
		t.Fatalf("read error = %v", err)
	}
	if !utf8.ValidString(envelope("é", "x")) {
		t.Fatal("envelope helper")
	}
}
