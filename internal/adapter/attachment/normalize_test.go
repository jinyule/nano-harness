package attachment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	stdimage "image"
	"image/color"
	"image/gif"
	"image/png"
	"strings"
	"testing"

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

func TestNormalize_FormatsScalingAndFailures(t *testing.T) {
	valid := encodedPNG(t, 4, 3)
	if _, err := normalize(context.Background(), "image.png", []byte("bad")); !errors.Is(err, ErrInvalidImage) || !errors.Is(err, session.ErrImageFormat) {
		t.Fatalf("bad format=%v", err)
	}
	if prepared, err := normalize(context.Background(), "large.png", pngHeader(4001, 4000)); !errors.Is(err, ErrInvalidImage) || !errors.Is(err, session.ErrImagePixels) || prepared.source != stdimage.Pt(4001, 4000) {
		t.Fatalf("pixel limit=%v source=%v", err, prepared.source)
	}
	if _, err := normalize(context.Background(), "truncated.png", pngHeader(1, 1)); !errors.Is(err, ErrInvalidImage) || !errors.Is(err, session.ErrImageFormat) {
		t.Fatalf("decode failure=%v", err)
	}
	if _, err := normalize(context.Background(), "image.bmp", append([]byte("BM"), make([]byte, 64)...)); !errors.Is(err, session.ErrImageFormat) {
		t.Fatalf("unregistered format=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := normalize(ctx, "image.png", valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("decode cancellation=%v", err)
	}
	wide, err := normalize(context.Background(), "wide.png", encodedPNG(t, 2200, 2))
	if err != nil || wide.ref.Width != maxDimension || wide.ref.Height != 1 || wide.source != stdimage.Pt(2200, 2) {
		t.Fatalf("wide=%#v err=%v", wide.ref, err)
	}
	digest := sha256.Sum256(wide.data)
	if wide.ref.ID != session.ImageID(hex.EncodeToString(digest[:])) || wide.ref.Bytes != len(wide.data) || wide.ref.MediaType != "image/jpeg" || wide.ref.Name != "wide.png" {
		t.Fatalf("wide reference=%#v", wide.ref)
	}
	tall, err := normalize(context.Background(), "tall.png", encodedPNG(t, 2, 2200))
	if err != nil || tall.ref.Width != 1 || tall.ref.Height != maxDimension {
		t.Fatalf("tall=%#v err=%v", tall.ref, err)
	}
	if width, height := fit(10, 20, 100); width != 10 || height != 20 {
		t.Fatalf("small fit=%dx%d", width, height)
	}

	originalEncode := encodeImage
	t.Cleanup(func() { encodeImage = originalEncode })
	encodeImage = func(stdimage.Image, int) ([]byte, error) { return nil, errors.New("encode") }
	if _, err := normalize(context.Background(), "image.png", valid); !errors.Is(err, ErrInvalidImage) {
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
	if _, err := normalize(context.Background(), "image.png", largeSource); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("scaled encode error=%v", err)
	}
	encodeImage = func(stdimage.Image, int) ([]byte, error) { return oversized, nil }
	if _, err := normalize(context.Background(), "image.png", largeSource); !errors.Is(err, ErrInvalidImage) || !errors.Is(err, session.ErrImageBytes) {
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
	if scaled, err := normalize(context.Background(), "image.png", largeSource); err != nil || scaled.ref.Width >= 1000 || scaled.source != stdimage.Pt(1000, 1000) {
		t.Fatalf("scaled attachment=%#v err=%v", scaled.ref, err)
	}
	encodeImage = originalEncode
	if _, err := normalize(context.Background(), strings.Repeat("n", 256), valid); !errors.Is(err, ErrInvalidImage) {
		t.Fatalf("invalid normalized metadata=%v", err)
	}
}

// alphaWebP is a 3x2 lossless WebP whose top-left pixel is transparent and
// whose other pixels are opaque RGB(200,30,30), produced by cwebp -lossless.
const alphaWebP = "UklGRiAAAABXRUJQVlA4TBQAAAAvAkAAEA8wHoM8HvMf8LjBQUT/Qw=="

func decodeNormalized(t *testing.T, prepared normalized) stdimage.Image {
	t.Helper()
	decoded, format, err := stdimage.Decode(bytes.NewReader(prepared.data))
	if err != nil || format != "jpeg" {
		t.Fatalf("normalized bytes format=%q err=%v", format, err)
	}
	return decoded
}

func TestNormalize_AcceptsWebPAndFirstGIFFrameOnWhite(t *testing.T) {
	webp, err := base64.StdEncoding.DecodeString(alphaWebP)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := normalize(context.Background(), "alpha.webp", webp)
	if err != nil || prepared.ref.MediaType != "image/jpeg" || prepared.ref.Width != 3 || prepared.ref.Height != 2 || prepared.source != stdimage.Pt(3, 2) {
		t.Fatalf("webp=%#v err=%v", prepared.ref, err)
	}
	// The transparent corner becomes white instead of the black that
	// encoding premultiplied pixels would produce.
	if red, green, blue, _ := decodeNormalized(t, prepared).At(0, 0).RGBA(); red>>8 < 200 || green>>8 < 200 || blue>>8 < 200 {
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
	prepared, err = normalize(context.Background(), "anim.gif", animation.Bytes())
	if err != nil || prepared.ref.Width != 16 || prepared.ref.Height != 16 {
		t.Fatalf("gif=%#v err=%v", prepared.ref, err)
	}
	pixels := decodeNormalized(t, prepared)
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
