package image

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	stdimage "image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type failingReader struct{ err error }

func (reader failingReader) Read([]byte) (int, error) { return 0, reader.err }
func (failingReader) Close() error                    { return nil }

func pngChunk(kind string, data []byte) []byte {
	var output bytes.Buffer
	_ = binary.Write(&output, binary.BigEndian, uint32(len(data))) //nolint:gosec // test PNG chunks are fixed, tiny fixtures
	output.WriteString(kind)
	output.Write(data)
	checksum := crc32.ChecksumIEEE(append([]byte(kind), data...))
	_ = binary.Write(&output, binary.BigEndian, checksum)
	return output.Bytes()
}

func pngHeader(width, height uint32) []byte {
	header := make([]byte, 13)
	binary.BigEndian.PutUint32(header[0:4], width)
	binary.BigEndian.PutUint32(header[4:8], height)
	header[8], header[9] = 8, 2
	output := append([]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", header)...)
	return append(output, pngChunk("IEND", nil)...)
}

func encodedPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	picture := stdimage.NewRGBA(stdimage.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			picture.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: uint8(x + y), A: 255})
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, picture); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestNormalizerLifecycleAndFileValidation(t *testing.T) {
	normalizer := New()
	if normalizer.ID() != "images" {
		t.Fatal("wrong normalizer ID")
	}
	if _, err := normalizer.Normalize(context.Background(), "x"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive normalize=%v", err)
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := normalizer.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope=%v", err)
	}
	normalizer = New()
	scope := &plugin.Scope{}
	if err := normalizer.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if err := normalizer.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("second start=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := normalizer.Normalize(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("normalize cancellation=%v", err)
	}
	if _, err := normalizer.Normalize(context.Background(), ""); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("empty path=%v", err)
	}
	if _, err := normalizer.Normalize(context.Background(), filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("missing path=%v", err)
	}
	if _, err := normalizer.Normalize(context.Background(), t.TempDir()); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("directory path=%v", err)
	}
	empty := filepath.Join(t.TempDir(), "empty.png")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizer.Normalize(context.Background(), empty); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("empty file=%v", err)
	}
	large := filepath.Join(t.TempDir(), "large.png")
	file, err := os.OpenFile(large, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxSourceBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err := normalizer.Normalize(context.Background(), large); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("large file=%v", err)
	}

	originalAbs, originalOpen := imageAbs, openSource
	t.Cleanup(func() { imageAbs, openSource = originalAbs, originalOpen })
	imageAbs = func(string) (string, error) { return "", errors.New("absolute") }
	if _, err := normalizer.Normalize(context.Background(), "x"); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("absolute error=%v", err)
	}
	imageAbs = originalAbs
	validPath := filepath.Join(t.TempDir(), "image.png")
	if err := os.WriteFile(validPath, encodedPNG(t, 2, 2), 0o600); err != nil {
		t.Fatal(err)
	}
	openSource = func(string) (io.ReadCloser, error) { return nil, errors.New("open") }
	if _, err := normalizer.Normalize(context.Background(), validPath); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("open error=%v", err)
	}
	openSource = func(string) (io.ReadCloser, error) { return failingReader{err: errors.New("read")}, nil }
	if _, err := normalizer.Normalize(context.Background(), validPath); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("read error=%v", err)
	}
	openSource = originalOpen
	attachment, err := normalizer.Normalize(context.Background(), validPath)
	if err != nil || attachment.Width != 2 || attachment.Height != 2 || attachment.MediaType != "image/jpeg" || attachment.ID == "" {
		t.Fatalf("attachment=%#v err=%v", attachment, err)
	}
	if err := (session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "test"}, Content: []session.ContentBlock{{Type: session.ContentImage, Image: &attachment}}}}).Validate(); err != nil {
		t.Fatal(err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizer.Normalize(context.Background(), validPath); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("normalize after close=%v", err)
	}
}

func TestNormalizeBytesFormatsScalingAndFailures(t *testing.T) {
	valid := encodedPNG(t, 4, 3)
	if _, err := normalizeBytes(context.Background(), "image.png", []byte("bad")); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("bad format=%v", err)
	}
	if _, err := normalizeBytes(context.Background(), "large.png", pngHeader(4001, 4000)); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("pixel limit=%v", err)
	}
	if _, err := normalizeBytes(context.Background(), "truncated.png", pngHeader(1, 1)); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("decode failure=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := normalizeBytes(ctx, "image.png", valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("decode cancellation=%v", err)
	}
	wide, err := normalizeBytes(context.Background(), "wide.png", encodedPNG(t, 2200, 2))
	if err != nil || wide.Width != maxDimension || wide.Height != 1 {
		t.Fatalf("wide=%#v err=%v", wide, err)
	}
	tall, err := normalizeBytes(context.Background(), "tall.png", encodedPNG(t, 2, 2200))
	if err != nil || tall.Width != 1 || tall.Height != maxDimension {
		t.Fatalf("tall=%#v err=%v", tall, err)
	}
	if width, height := fit(10, 20, 100); width != 10 || height != 20 {
		t.Fatalf("small fit=%dx%d", width, height)
	}

	originalEncode := encodeImage
	t.Cleanup(func() { encodeImage = originalEncode })
	encodeImage = func(stdimage.Image, int) ([]byte, error) { return nil, errors.New("encode") }
	if _, err := normalizeBytes(context.Background(), "image.png", valid); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("encode error=%v", err)
	}
	largeSource := encodedPNG(t, 1000, 1000)
	oversized := []byte(strings.Repeat("x", session.MaxImageBytes+1))
	calls := 0
	encodeImage = func(_ stdimage.Image, _ int) ([]byte, error) {
		calls++
		if calls == 1 {
			return oversized, nil
		}
		return nil, errors.New("scaled encode")
	}
	if _, err := normalizeBytes(context.Background(), "image.png", largeSource); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("scaled encode error=%v", err)
	}
	encodeImage = func(stdimage.Image, int) ([]byte, error) { return oversized, nil }
	if _, err := normalizeBytes(context.Background(), "image.png", largeSource); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("oversized normalized image=%v", err)
	}
	calls = 0
	encodeImage = func(source stdimage.Image, quality int) ([]byte, error) {
		calls++
		if calls == 1 {
			return oversized, nil
		}
		return encodeJPEG(source, quality)
	}
	if attachment, err := normalizeBytes(context.Background(), "image.png", largeSource); err != nil || attachment.Width >= 1000 {
		t.Fatalf("scaled attachment=%#v err=%v", attachment, err)
	}
	encodeImage = originalEncode
	if _, err := normalizeBytes(context.Background(), strings.Repeat("n", 256), valid); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("invalid normalized metadata=%v", err)
	}
}
