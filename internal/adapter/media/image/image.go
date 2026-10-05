// Package image normalizes PNG, JPEG, WebP, and GIF images for replay and
// vision APIs: explicit local attachments and bytes read by image tools.
package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	stdimage "image"
	_ "image/gif" // Register the GIF decoder; decoding keeps the first frame.
	"image/jpeg"
	_ "image/png" // Register the standard PNG decoder.
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // Register the still-image WebP decoder.

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	maxSourceBytes  = session.MaxImageSourceBytes
	maxSourcePixels = session.MaxImageSourcePixels
	maxDimension    = 2048
)

// formats are the decoder names admitted as sources.
var formats = map[string]bool{"jpeg": true, "png": true, "gif": true, "webp": true}

var (
	// ErrInvalidImage identifies an attachment that cannot satisfy the normalized image contract.
	ErrInvalidImage = errors.New("invalid image attachment")
	// ErrNotRunning indicates the image normalizer has not started or has stopped.
	ErrNotRunning = errors.New("image normalizer is not running")

	imageAbs   = filepath.Abs
	imageLstat = os.Lstat
	openSource = func(path string) (io.ReadCloser, error) {
		return os.Open(path) //nolint:gosec // the path is an explicit user attachment
	}
	encodeImage = encodeJPEG
)

// Normalizer is a lifecycle-owned decoder and scaler.
type Normalizer struct {
	mu      sync.RWMutex
	started bool
	active  bool
}

// New constructs an inactive image normalizer.
func New() *Normalizer { return &Normalizer{} }

// ID returns the stable plugin identity.
func (*Normalizer) ID() string { return "images" }

// Start activates attachment reads until scope cleanup.
func (normalizer *Normalizer) Start(_ context.Context, scope *plugin.Scope) error {
	normalizer.mu.Lock()
	defer normalizer.mu.Unlock()
	if normalizer.started {
		return ErrInvalidImage
	}
	if err := scope.Defer(func(context.Context) error {
		normalizer.mu.Lock()
		normalizer.active = false
		normalizer.mu.Unlock()
		return nil
	}); err != nil {
		return err
	}
	normalizer.started, normalizer.active = true, true
	return nil
}

// Normalize reads an explicitly selected image file and returns bounded inline data.
func (normalizer *Normalizer) Normalize(ctx context.Context, path string) (session.Image, error) {
	if err := normalizer.ready(ctx); err != nil {
		return session.Image{}, err
	}
	absolute, err := imageAbs(strings.TrimSpace(path))
	if err != nil || path == "" {
		return session.Image{}, fmt.Errorf("%w: resolve path", ErrInvalidImage)
	}
	info, err := imageLstat(absolute)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxSourceBytes {
		return session.Image{}, fmt.Errorf("%w: source must be a regular file within %d bytes", ErrInvalidImage, maxSourceBytes)
	}
	file, err := openSource(absolute)
	if err != nil {
		return session.Image{}, fmt.Errorf("%w: open source", ErrInvalidImage)
	}
	defer func() { _ = file.Close() }()
	encoded, err := io.ReadAll(io.LimitReader(file, maxSourceBytes+1))
	if err != nil || len(encoded) > maxSourceBytes {
		return session.Image{}, fmt.Errorf("%w: read source", ErrInvalidImage)
	}
	normalized, _, err := normalizeBytes(ctx, filepath.Base(absolute), encoded)
	return normalized, err
}

// NormalizeBytes normalizes source bytes that the caller read under its own
// path policy. It returns the normalized image and the decoded source size;
// refusals wrap ErrInvalidImage and, where a limit or format applies,
// session.ErrImageFormat, session.ErrImagePixels, or session.ErrImageBytes.
func (normalizer *Normalizer) NormalizeBytes(ctx context.Context, name string, encoded []byte) (session.Image, stdimage.Point, error) {
	if err := normalizer.ready(ctx); err != nil {
		return session.Image{}, stdimage.Point{}, err
	}
	if len(encoded) == 0 || len(encoded) > maxSourceBytes {
		return session.Image{}, stdimage.Point{}, fmt.Errorf("%w: source must be 1-%d bytes", ErrInvalidImage, maxSourceBytes)
	}
	return normalizeBytes(ctx, name, encoded)
}

func (normalizer *Normalizer) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	normalizer.mu.RLock()
	active := normalizer.active
	normalizer.mu.RUnlock()
	if !active {
		return ErrNotRunning
	}
	return nil
}

func normalizeBytes(ctx context.Context, name string, encoded []byte) (session.Image, stdimage.Point, error) {
	config, format, err := stdimage.DecodeConfig(bytes.NewReader(encoded))
	if err != nil || !formats[format] || config.Width < 1 || config.Height < 1 {
		return session.Image{}, stdimage.Point{}, fmt.Errorf("%w: %w", ErrInvalidImage, session.ErrImageFormat)
	}
	source := stdimage.Pt(config.Width, config.Height)
	if int64(config.Width)*int64(config.Height) > maxSourcePixels {
		return session.Image{}, source, fmt.Errorf("%w: %w", ErrInvalidImage, session.ErrImagePixels)
	}
	decoded, _, err := stdimage.Decode(bytes.NewReader(encoded))
	if err != nil {
		return session.Image{}, source, fmt.Errorf("%w: %w", ErrInvalidImage, session.ErrImageFormat)
	}
	if err := ctx.Err(); err != nil {
		return session.Image{}, source, err
	}
	width, height := fit(config.Width, config.Height, maxDimension)
	current := flatten(decoded)
	if width != config.Width || height != config.Height {
		current = resize(decoded, width, height)
	}
	mediaType := "image/jpeg"
	output, err := encodeImage(current, 88)
	if err != nil {
		return session.Image{}, source, fmt.Errorf("%w: encode pixels", ErrInvalidImage)
	}
	for len(output) > session.MaxImageBytes && width > 256 && height > 256 {
		width = max(width*3/4, 1)
		height = max(height*3/4, 1)
		current = resize(current, width, height)
		output, err = encodeImage(current, 82)
		if err != nil {
			return session.Image{}, source, fmt.Errorf("%w: encode scaled pixels", ErrInvalidImage)
		}
	}
	if len(output) > session.MaxImageBytes {
		return session.Image{}, source, fmt.Errorf("%w: %w", ErrInvalidImage, session.ErrImageBytes)
	}
	digest := sha256.Sum256(output)
	sha := hex.EncodeToString(digest[:])
	attachment := session.Image{
		ID: "img-" + sha[:16], Name: name, MediaType: mediaType,
		Data: base64.StdEncoding.EncodeToString(output), SHA256: sha, Width: width, Height: height,
	}
	if err := (session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &session.Message{
		Role: session.RoleUser, Source: session.MessageSource{Kind: "user"},
		Content: []session.ContentBlock{{Type: session.ContentImage, Image: &attachment}},
	}}).Validate(); err != nil {
		return session.Image{}, source, fmt.Errorf("%w: validate normalized record: %w", ErrInvalidImage, err)
	}
	return attachment, source, nil
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
