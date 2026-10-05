package image

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	stdimage "image"
	"image/color"
	"image/gif"
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
	if _, _, err := normalizeBytes(context.Background(), "image.png", []byte("bad")); !errors.Is(err, ErrInvalidImage) || !errors.Is(err, session.ErrImageFormat) {
		t.Fatalf("bad format=%v", err)
	}
	if _, source, err := normalizeBytes(context.Background(), "large.png", pngHeader(4001, 4000)); !errors.Is(err, ErrInvalidImage) || !errors.Is(err, session.ErrImagePixels) || source != stdimage.Pt(4001, 4000) {
		t.Fatalf("pixel limit=%v source=%v", err, source)
	}
	if _, _, err := normalizeBytes(context.Background(), "truncated.png", pngHeader(1, 1)); !errors.Is(err, ErrInvalidImage) || !errors.Is(err, session.ErrImageFormat) {
		t.Fatalf("decode failure=%v", err)
	}
	if _, _, err := normalizeBytes(context.Background(), "image.bmp", append([]byte("BM"), make([]byte, 64)...)); !errors.Is(err, session.ErrImageFormat) {
		t.Fatalf("unregistered format=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := normalizeBytes(ctx, "image.png", valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("decode cancellation=%v", err)
	}
	wide, source, err := normalizeBytes(context.Background(), "wide.png", encodedPNG(t, 2200, 2))
	if err != nil || wide.Width != maxDimension || wide.Height != 1 || source != stdimage.Pt(2200, 2) {
		t.Fatalf("wide=%#v source=%v err=%v", wide, source, err)
	}
	tall, _, err := normalizeBytes(context.Background(), "tall.png", encodedPNG(t, 2, 2200))
	if err != nil || tall.Width != 1 || tall.Height != maxDimension {
		t.Fatalf("tall=%#v err=%v", tall, err)
	}
	if width, height := fit(10, 20, 100); width != 10 || height != 20 {
		t.Fatalf("small fit=%dx%d", width, height)
	}

	originalEncode := encodeImage
	t.Cleanup(func() { encodeImage = originalEncode })
	encodeImage = func(stdimage.Image, int) ([]byte, error) { return nil, errors.New("encode") }
	if _, _, err := normalizeBytes(context.Background(), "image.png", valid); !errors.Is(err, ErrInvalidImage) {
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
	if _, _, err := normalizeBytes(context.Background(), "image.png", largeSource); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("scaled encode error=%v", err)
	}
	encodeImage = func(stdimage.Image, int) ([]byte, error) { return oversized, nil }
	if _, _, err := normalizeBytes(context.Background(), "image.png", largeSource); !errors.Is(err, ErrInvalidImage) || !errors.Is(err, session.ErrImageBytes) {
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
	if attachment, source, err := normalizeBytes(context.Background(), "image.png", largeSource); err != nil || attachment.Width >= 1000 || source != stdimage.Pt(1000, 1000) {
		t.Fatalf("scaled attachment=%#v source=%v err=%v", attachment, source, err)
	}
	encodeImage = originalEncode
	if _, _, err := normalizeBytes(context.Background(), strings.Repeat("n", 256), valid); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("invalid normalized metadata=%v", err)
	}
}

// alphaWebP is a 3x2 lossless WebP whose top-left pixel is transparent and
// whose other pixels are opaque RGB(200,30,30), produced by cwebp -lossless.
const alphaWebP = "UklGRiAAAABXRUJQVlA4TBQAAAAvAkAAEA8wHoM8HvMf8LjBQUT/Qw=="

func decodeNormalized(t *testing.T, normalized session.Image) stdimage.Image {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(normalized.Data)
	if err != nil {
		t.Fatal(err)
	}
	decoded, format, err := stdimage.Decode(bytes.NewReader(data))
	if err != nil || format != "jpeg" {
		t.Fatalf("normalized bytes format=%q err=%v", format, err)
	}
	return decoded
}

func TestNormalizeBytes_AcceptsWebPAndFirstGIFFrameOnWhite(t *testing.T) {
	webp, err := base64.StdEncoding.DecodeString(alphaWebP)
	if err != nil {
		t.Fatal(err)
	}
	normalized, source, err := normalizeBytes(context.Background(), "alpha.webp", webp)
	if err != nil || normalized.MediaType != "image/jpeg" || normalized.Width != 3 || normalized.Height != 2 || source != stdimage.Pt(3, 2) {
		t.Fatalf("webp=%#v source=%v err=%v", normalized, source, err)
	}
	// The transparent corner becomes white instead of the black that
	// encoding premultiplied pixels would produce.
	if red, green, blue, _ := decodeNormalized(t, normalized).At(0, 0).RGBA(); red>>8 < 200 || green>>8 < 200 || blue>>8 < 200 {
		t.Fatalf("transparent corner=(%d,%d,%d)", red>>8, green>>8, blue>>8)
	}

	// JPEG subsampling blends neighboring pixels, so the frames use 16x16
	// halves and the assertions sample well inside each half.
	palette := color.Palette{color.Transparent, color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255}}
	first := stdimage.NewPaletted(stdimage.Rect(0, 0, 16, 16), palette)
	second := stdimage.NewPaletted(stdimage.Rect(0, 0, 16, 16), palette)
	for y := range 16 {
		for x := range 16 {
			if x < 8 {
				first.SetColorIndex(x, y, 1)
			}
			second.SetColorIndex(x, y, 2)
		}
	}
	var animation bytes.Buffer
	if err := gif.EncodeAll(&animation, &gif.GIF{Image: []*stdimage.Paletted{first, second}, Delay: []int{0, 0}}); err != nil {
		t.Fatal(err)
	}
	normalized, _, err = normalizeBytes(context.Background(), "anim.gif", animation.Bytes())
	if err != nil || normalized.Width != 16 || normalized.Height != 16 {
		t.Fatalf("gif=%#v err=%v", normalized, err)
	}
	pixels := decodeNormalized(t, normalized)
	if red, _, blue, _ := pixels.At(3, 8).RGBA(); red>>8 < 200 || blue>>8 > 60 {
		t.Fatalf("first frame pixel=(%d,_,%d)", red>>8, blue>>8)
	}
	if red, green, blue, _ := pixels.At(12, 8).RGBA(); red>>8 < 200 || green>>8 < 200 || blue>>8 < 200 {
		t.Fatalf("transparent GIF pixel=(%d,%d,%d)", red>>8, green>>8, blue>>8)
	}
}

// opaqueless lacks an Opaque method, so flatten must composite it.
type opaqueless struct{ stdimage.Image }

func TestFlatten_KeepsOpaqueSourcesAndCompositesTheRest(t *testing.T) {
	opaque := stdimage.NewRGBA(stdimage.Rect(0, 0, 1, 1))
	opaque.Set(0, 0, color.Black)
	if flatten(opaque) != stdimage.Image(opaque) {
		t.Fatal("opaque source was copied")
	}
	transparent := stdimage.NewNRGBA(stdimage.Rect(5, 5, 6, 6))
	flattened := flatten(opaqueless{transparent})
	if flattened.Bounds() != stdimage.Rect(0, 0, 1, 1) {
		t.Fatalf("bounds=%v", flattened.Bounds())
	}
	if red, green, blue, alpha := flattened.At(0, 0).RGBA(); red != 0xffff || green != 0xffff || blue != 0xffff || alpha != 0xffff {
		t.Fatalf("composited=(%d,%d,%d,%d)", red, green, blue, alpha)
	}
}

func TestNormalizer_NormalizeBytesLifecycleAndBounds(t *testing.T) {
	normalizer := New()
	valid := encodedPNG(t, 3, 2)
	if _, _, err := normalizer.NormalizeBytes(context.Background(), "x.png", valid); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("inactive=%v", err)
	}
	scope := &plugin.Scope{}
	if err := normalizer.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := normalizer.NormalizeBytes(ctx, "x.png", valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled=%v", err)
	}
	for _, data := range [][]byte{nil, make([]byte, maxSourceBytes+1)} {
		if _, _, err := normalizer.NormalizeBytes(context.Background(), "x.png", data); !errors.Is(err, ErrInvalidImage) || errors.Is(err, session.ErrImageFormat) {
			t.Fatalf("source size %d=%v", len(data), err)
		}
	}
	normalized, source, err := normalizer.NormalizeBytes(context.Background(), "x.png", valid)
	if err != nil || normalized.Name != "x.png" || source != stdimage.Pt(3, 2) {
		t.Fatalf("normalized=%#v source=%v err=%v", normalized, source, err)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := normalizer.NormalizeBytes(context.Background(), "x.png", valid); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("closed=%v", err)
	}
}
