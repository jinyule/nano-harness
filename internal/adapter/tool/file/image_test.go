package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdimage "image"
	"io"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	pngBytes  = "\x89PNG\r\n\x1a\nrest"
	jpegBytes = "\xff\xd8\xffrest"
)

// fakeImages records normalization requests and returns a fixed image; the
// real normalizer is covered by its own package and the assembled tests.
type fakeImages struct {
	mu     sync.Mutex
	names  []string
	data   []string
	err    error
	source stdimage.Point
	// during runs inside NormalizeBytes before it returns.
	during func()
}

func (images *fakeImages) NormalizeBytes(_ context.Context, name string, data []byte) (session.Image, stdimage.Point, error) {
	images.mu.Lock()
	images.names = append(images.names, name)
	images.data = append(images.data, string(data))
	during, err, source := images.during, images.err, images.source
	images.mu.Unlock()
	if during != nil {
		during()
	}
	normalized := session.Image{ID: "img-1", Name: name, MediaType: "image/jpeg", Data: "anBlZw==", SHA256: strings.Repeat("a", 64), Width: 4, Height: 2}
	if source == (stdimage.Point{}) {
		source = stdimage.Pt(4, 2)
	}
	return normalized, source, err
}

func (images *fakeImages) calls() int {
	images.mu.Lock()
	defer images.mu.Unlock()
	return len(images.names)
}

var visionRoute = appTool.Route{Provider: "openai", Model: "vision-model", ImageInput: true}

func (h *harness) readImage(t *testing.T, route appTool.Route, paths ...string) []session.ToolResult {
	t.Helper()
	calls := make([]session.ToolCall, len(paths))
	for index, path := range paths {
		encoded, _ := json.Marshal(map[string]string{"file_path": path})
		calls[index] = session.ToolCall{ID: fmt.Sprintf("call-%d", index), Name: "read_image", Arguments: encoded}
	}
	return h.runtime.ExecuteBatch(context.Background(), appTool.BatchRequest{
		SessionID: "session", Route: route, Turn: 1, Step: 1, Journal: nopJournal{}, Calls: calls,
	})
}

func TestReadImage_ReturnsTheEnvelopeAndNormalizedImage(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("shots", "red.png"), pngBytes)
	result := h.readImage(t, visionRoute, "shots/red.png")[0]
	want := "<path>" + h.path("shots", "red.png") + "</path>\n<type>image</type>\n<content>\nimage/jpeg image, 4x2 px, 4 bytes\n</content>"
	if result.IsError || result.Output != want || result.Image == nil || result.Image.Name != "red.png" {
		t.Fatalf("result = %+v", result)
	}
	if h.images.names[0] != "red.png" || h.images.data[0] != pngBytes {
		t.Fatalf("normalized %q from %q", h.images.names, h.images.data)
	}
	// read_image observes the file like read, so write may replace it.
	if prior, ok := h.provider.observed.lookup("session", h.path("shots", "red.png")); !ok || !prior.present || prior.version != digest([]byte(pngBytes)) {
		t.Fatalf("observation = %+v %v", prior, ok)
	}
	if result := h.call(t, "write", map[string]any{"file_path": "shots/red.png", "content": "text"}); result.IsError {
		t.Fatalf("write after read_image = %s", result.Output)
	}

	// Extension-less paths and dotfiles are identified by their signature.
	writeFixture(t, h.path("avatar"), jpegBytes)
	writeFixture(t, h.path(".hidden"), pngBytes)
	for _, result := range h.readImage(t, visionRoute, "avatar", ".hidden") {
		if result.IsError || result.Image == nil {
			t.Fatalf("sniffed read = %+v", result)
		}
	}

	// A downscaled read names the source size and coordinate multiplier.
	h.images.source = stdimage.Pt(9, 4)
	if result := h.readImage(t, visionRoute, "avatar")[0]; !strings.Contains(result.Output, "image/jpeg image, 4x2 px, 4 bytes (downscaled from 9x4 px; multiply x coordinates by 2.25 and y coordinates by 2.00 to locate features in the original file)") {
		t.Fatalf("downscaled = %q", result.Output)
	}
	h.images.source = stdimage.Pt(10, 5)
	if result := h.readImage(t, visionRoute, "avatar")[0]; !strings.Contains(result.Output, "(downscaled from 10x5 px; multiply coordinates by 2.50 to locate features in the original file)") {
		t.Fatalf("uniform downscale = %q", result.Output)
	}
}

func TestReadImage_RefusesBeforeReadingFiles(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("red.png"), pngBytes)
	textOnly := appTool.Route{Provider: "openai", Model: "text-model"}
	for _, test := range []struct {
		route      appTool.Route
		path, want string
	}{
		{visionRoute, "   ", "Error: file_path must be a non-empty string"},
		{visionRoute, "notes.txt", `Error: cannot read "notes.txt": the .txt extension does not declare a supported image format; read_image accepts PNG/JPEG/WebP/GIF files, including extension-less files in those formats`},
		{visionRoute, "foo.", `Error: cannot read "foo.": the . extension does not declare a supported image format`},
		{appTool.Route{}, "red.png", `Error: cannot read "red.png" as an image: the current model route could not be resolved`},
		{appTool.Route{Provider: "openai"}, "red.png", `Error: cannot read "red.png" as an image: the current model route could not be resolved`},
		{textOnly, "red.png", `Error: cannot read "red.png" as an image: model "text-model" does not declare image input; switch to an image-capable model to read images`},
	} {
		result := h.readImage(t, test.route, test.path)[0]
		if !result.IsError || !strings.HasPrefix(result.Output, test.want) || result.Image != nil {
			t.Errorf("%q = %q", test.path, result.Output)
		}
	}
	if h.images.calls() != 0 {
		t.Fatal("a refused call reached the normalizer")
	}
	if _, ok := h.provider.observed.lookup("session", h.path("red.png")); ok {
		t.Fatal("a refused call recorded an observation")
	}
}

func TestReadImage_ReportsFileAndFormatFailures(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("notes"), "plain text, not an image")
	writeFixture(t, h.path("text.png"), "plain text, not an image")
	writeFixture(t, h.path("wrong.jpg"), pngBytes)
	writeFixture(t, h.path("anim.gif"), "GIF89a....")
	writeFixture(t, h.path("still.webp"), "RIFF\x00\x00\x00\x00WEBPVP8 ")
	if err := os.Mkdir(h.path("folder.png"), 0o700); err != nil {
		t.Fatal(err)
	}
	large := h.path("large.png")
	writeFixture(t, large, "")
	if err := os.Truncate(large, session.MaxImageSourceBytes+1); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing.png": fmt.Sprintf("Error: cannot read %q: not found", h.path("missing.png")),
		"folder.png":  fmt.Sprintf("Error: cannot read %q: not a regular file", h.path("folder.png")),
		"../out.png":  `Error: cannot read "../out.png": `,
		"large.png":   fmt.Sprintf("Error: cannot read %q: %d bytes exceeds the %d-byte limit", large, session.MaxImageSourceBytes+1, session.MaxImageSourceBytes),
		"notes":       fmt.Sprintf("Error: cannot read %q: the file content is not a supported image format; read_image accepts PNG/JPEG/WebP/GIF", h.path("notes")),
		"text.png":    fmt.Sprintf("Error: cannot read %q: the bytes do not decode as a supported PNG/JPEG/WebP/GIF image; the file may be truncated or corrupt", h.path("text.png")),
		"wrong.jpg":   fmt.Sprintf("Error: cannot read %q: the .jpg extension declares image/jpeg, but the bytes use a different image format; rename the file to match its actual format if it is PNG/JPEG/WebP/GIF, or convert it to one of those formats", h.path("wrong.jpg")),
	}
	for path, want := range cases {
		if result := h.readImage(t, visionRoute, path)[0]; !result.IsError || !strings.HasPrefix(result.Output, want) {
			t.Errorf("%s = %q", path, result.Output)
		}
	}
	if prior, ok := h.provider.observed.lookup("session", h.path("missing.png")); !ok || prior.present {
		t.Fatalf("missing observation = %+v %v", prior, ok)
	}
	if h.images.calls() != 0 {
		t.Fatal("a refused file reached the normalizer")
	}
	for _, path := range []string{"anim.gif", "still.webp"} {
		if result := h.readImage(t, visionRoute, path)[0]; result.IsError {
			t.Fatalf("%s = %q", path, result.Output)
		}
	}

	// Normalizer refusals map to upstream's guidance.
	for _, test := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("wrapped: %w", session.ErrImageFormat), "the bytes do not decode as a supported PNG/JPEG/WebP/GIF image; the file may be truncated or corrupt"},
		{session.ErrImagePixels, fmt.Sprintf("the image exceeds the %d-pixel decoded-size limit; downscale the image and read the smaller copy", session.MaxImageSourcePixels)},
		{session.ErrImageBytes, "the image cannot be stored within the deployment's byte limits; downscale the image and read the smaller copy"},
		{context.Canceled, "context canceled"},
	} {
		h.images.err = test.err
		result := h.readImage(t, visionRoute, "anim.gif")[0]
		if want := fmt.Sprintf("Error: cannot read %q: %s", h.path("anim.gif"), test.want); !result.IsError || result.Output != want {
			t.Errorf("%v = %q", test.err, result.Output)
		}
	}
	if _, ok := h.provider.observed.lookup("session", h.path("text.png")); ok {
		t.Fatal("a refused image recorded an observation")
	}
}

func TestReadImage_ReportsIOFailures(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("red.png"), pngBytes)
	restoreHooks(t)
	statFile = func(string) (fs.FileInfo, error) { return nil, errors.New("stat failed") }
	if result := h.readImage(t, visionRoute, "red.png")[0]; !strings.HasSuffix(result.Output, ": stat failed") {
		t.Fatalf("stat = %q", result.Output)
	}
	statFile = os.Stat
	openFile = func(string) (io.ReadCloser, error) { return nil, errors.New("open failed") }
	if result := h.readImage(t, visionRoute, "red.png")[0]; !strings.HasSuffix(result.Output, ": open failed") {
		t.Fatalf("open = %q", result.Output)
	}
	openFile = openFailing(nil, errors.New("read failed"))
	if result := h.readImage(t, visionRoute, "red.png")[0]; !strings.HasSuffix(result.Output, ": read failed") {
		t.Fatalf("read = %q", result.Output)
	}
	// A file that grows after stat is refused rather than truncated.
	openFile = func(string) (io.ReadCloser, error) {
		return io.NopCloser(io.LimitReader(zeroReader{}, session.MaxImageSourceBytes+1)), nil
	}
	if result := h.readImage(t, visionRoute, "red.png")[0]; !strings.HasSuffix(result.Output, fmt.Sprintf(": content exceeds the %d-byte limit", session.MaxImageSourceBytes)) {
		t.Fatalf("growth = %q", result.Output)
	}
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

func TestReadImage_ConcurrentCallsOverlap(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("a.png"), pngBytes)
	writeFixture(t, h.path("b.png"), pngBytes)
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	h.images.during = func() {
		arrived <- struct{}{}
		<-release
	}
	done := make(chan []session.ToolResult, 1)
	go func() { done <- h.readImage(t, visionRoute, "a.png", "b.png") }()
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(5 * time.Second):
			t.Fatal("read_image calls did not overlap")
		}
	}
	close(release)
	results := <-done
	if results[0].IsError || results[1].IsError || results[0].CallID != "call-0" || results[1].CallID != "call-1" {
		t.Fatalf("results = %+v", results)
	}
}

func TestImageHelpers_MatchUpstreamSemantics(t *testing.T) {
	for path, want := range map[string]string{
		"a.png": ".png", "a.JPG": ".JPG", "dir.d/name": "", ".hidden": "", ".a.gif": ".gif", "foo.": ".", "..": "", "...": ".", "png": "",
	} {
		if got := extname(path); got != want {
			t.Errorf("extname(%q) = %q, want %q", path, got, want)
		}
	}
	for data, want := range map[string]string{
		pngBytes: "image/png", jpegBytes: "image/jpeg", "GIF87a": "image/gif", "GIF89a": "image/gif",
		"RIFF\x00\x00\x00\x00WEBPVP8L": "image/webp", "": "", "\x89PN": "", "\xff\xd8": "", "GIF90a": "", "RIFF\x00\x00\x00\x00WAVE": "", "RIFF\x00\x00\x00": "",
	} {
		if got := sniffImage([]byte(data)); got != want {
			t.Errorf("sniffImage(%q) = %q, want %q", data, got, want)
		}
	}
	for value, want := range map[float64]string{2: "2.00", 1.125: "1.13", 1.375: "1.38", 0.875: "0.88", 2.5: "2.50", 1.005: "1.00", 1.953125: "1.95"} {
		if got := toFixed2(value); got != want {
			t.Errorf("toFixed2(%v) = %q, want %q", value, got, want)
		}
	}
	for data, want := range map[string]int{"": 0, "eA==": 1, "eHk=": 2, "eHl6": 3} {
		if got := decodedLength(data); got != want {
			t.Errorf("decodedLength(%q) = %d", data, got)
		}
	}
	image := session.Image{MediaType: "image/jpeg", Data: "eHl6", Width: 2, Height: 1}
	if got := formatImageRead("/w/p.jpg", image, stdimage.Pt(5, 2)); !strings.Contains(got, "(downscaled from 5x2 px; multiply x coordinates by 2.50 and y coordinates by 2.00 to locate features in the original file)") {
		t.Fatalf("per-axis = %q", got)
	}
	if got := formatImageRead("/w/p.jpg", image, stdimage.Pt(2, 1)); strings.Contains(got, "downscaled") || !strings.Contains(got, "image/jpeg image, 2x1 px, 3 bytes\n</content>") {
		t.Fatalf("unscaled = %q", got)
	}
}
