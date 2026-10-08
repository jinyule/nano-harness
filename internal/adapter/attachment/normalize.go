package attachment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	stdimage "image"
	_ "image/gif" // Register the GIF decoder; decoding keeps the first frame.
	"image/jpeg"
	_ "image/png" // Register the standard PNG decoder.

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // Register the still-image WebP decoder.

	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	maxSourceBytes  = session.MaxImageSourceBytes
	maxSourcePixels = session.MaxImageSourcePixels
	maxDimension    = 2048
	// maxConversions is upstream's default limit on concurrent image
	// normalizations in one attachment store.
	maxConversions = 2
)

var (
	// formats are the decoder names admitted as sources.
	formats = map[string]bool{"jpeg": true, "png": true, "gif": true, "webp": true}

	encodeImage = encodeJPEG
)

// normalized is one prepared image: the bytes to store, the reference that
// describes them, and the decoded source size.
type normalized struct {
	data   []byte
	ref    session.Image
	source stdimage.Point
}

// normalize decodes a source image, scales its longest edge to 2048, composites
// transparency onto white, and re-encodes it as a JPEG within
// session.MaxImageBytes. Refusals wrap ErrInvalidImage and, where a format or
// limit applies, session.ErrImageFormat, ErrImagePixels, or ErrImageBytes.
func normalize(ctx context.Context, name string, encoded []byte) (normalized, error) {
	config, format, err := stdimage.DecodeConfig(bytes.NewReader(encoded))
	if err != nil || !formats[format] || config.Width < 1 || config.Height < 1 {
		return normalized{}, fmt.Errorf("%w: %w", ErrInvalidImage, session.ErrImageFormat)
	}
	source := stdimage.Pt(config.Width, config.Height)
	if int64(config.Width)*int64(config.Height) > maxSourcePixels {
		return normalized{source: source}, fmt.Errorf("%w: %w", ErrInvalidImage, session.ErrImagePixels)
	}
	decoded, _, err := stdimage.Decode(bytes.NewReader(encoded))
	if err != nil {
		return normalized{source: source}, fmt.Errorf("%w: %w", ErrInvalidImage, session.ErrImageFormat)
	}
	if err := ctx.Err(); err != nil {
		return normalized{source: source}, err
	}
	width, height := fit(config.Width, config.Height, maxDimension)
	current := flatten(decoded)
	if width != config.Width || height != config.Height {
		current = resize(current, width, height)
	}
	output, err := encodeImage(current, 88)
	if err != nil {
		return normalized{source: source}, fmt.Errorf("%w: encode pixels", ErrInvalidImage)
	}
	for len(output) > session.MaxImageBytes && width > 256 && height > 256 {
		width = max(width*3/4, 1)
		height = max(height*3/4, 1)
		current = resize(current, width, height)
		output, err = encodeImage(current, 82)
		if err != nil {
			return normalized{source: source}, fmt.Errorf("%w: encode scaled pixels", ErrInvalidImage)
		}
	}
	if len(output) > session.MaxImageBytes {
		return normalized{source: source}, fmt.Errorf("%w: %w", ErrInvalidImage, session.ErrImageBytes)
	}
	digest := sha256.Sum256(output)
	ref := session.Image{
		ID: session.ImageID(hex.EncodeToString(digest[:])), Name: name, MediaType: "image/jpeg",
		Bytes: len(output), Width: width, Height: height,
	}
	// A tool result carries the same reference rules as a user message
	// without attributing anything to a person.
	if err := (session.Record{Type: session.RecordToolResult, Turn: 1, Step: 1, Result: &session.ToolResult{CallID: "validate", Image: &ref}}).Validate(); err != nil {
		return normalized{source: source}, fmt.Errorf("%w: validate normalized reference: %w", ErrInvalidImage, err)
	}
	return normalized{data: output, ref: ref, source: source}, nil
}

func fit(width, height, limit int) (int, int) {
	if width <= limit && height <= limit {
		return width, height
	}
	if width >= height {
		return limit, max(1, height*limit/width)
	}
	return max(1, width*limit/height), limit
}

// flatten composites transparent pixels onto white: JPEG has no alpha, and
// encoding premultiplied pixels directly would turn transparency black.
func flatten(source stdimage.Image) stdimage.Image {
	if opaque, ok := source.(interface{ Opaque() bool }); ok && opaque.Opaque() {
		return source
	}
	bounds := source.Bounds()
	target := stdimage.NewRGBA(stdimage.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(target, target.Bounds(), stdimage.White, stdimage.Point{}, draw.Src)
	draw.Draw(target, target.Bounds(), source, bounds.Min, draw.Over)
	return target
}

func resize(source stdimage.Image, width, height int) stdimage.Image {
	target := stdimage.NewRGBA(stdimage.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(target, target.Bounds(), source, source.Bounds(), draw.Over, nil)
	return target
}

func encodeJPEG(source stdimage.Image, quality int) ([]byte, error) {
	var buffer bytes.Buffer
	err := jpeg.Encode(&buffer, source, &jpeg.Options{Quality: quality})
	return buffer.Bytes(), err
}
