package file

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	stdimage "image"
	"io"
	"io/fs"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// ImageStore is the attachment boundary read_image consumes. SaveImage
// normalizes the source, makes it durable in the attachment store, and
// returns its reference and the decoded source size. It wraps
// session.ErrImageFormat, session.ErrImagePixels, or session.ErrImageBytes
// when the source is refused for that reason.
type ImageStore interface {
	SaveImage(ctx context.Context, name string, data []byte) (session.Image, stdimage.Point, error)
}

// imageExtensions are the extensions read_image accepts; the bytes must
// still carry the matching signature and decode completely.
var imageExtensions = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".webp": "image/webp",
	".gif":  "image/gif",
}

type readImageArgs struct {
	FilePath string `json:"file_path"`
}

func (provider *Provider) readImageTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[readImageArgs]{
		Name:        "read_image",
		Description: "Read a PNG/JPEG/WebP/GIF file and return the image itself. Large images are downscaled automatically; do not install image libraries or create thumbnails to inspect an image.",
		Parameters: appTool.Parameters{
			appTool.Required("file_path", appTool.String("Path to the image file, resolved by the filesystem backend.")),
		},
		Check: checkReadImage,
		// Normalization is deterministic and observations only record what
		// was read, so concurrent reads cannot conflict.
		Concurrent: func(readImageArgs) bool { return true },
		Execute:    provider.readImage,
	})
}

// checkReadImage applies upstream's pre-read gates: a non-empty path, an
// image extension or none, and a calling route that declares image input.
// The route gate is stricter than a provider refusal: an image the calling
// model cannot inspect must never enter the session.
func checkReadImage(invocation appTool.Invocation, arguments readImageArgs) error {
	if strings.TrimSpace(arguments.FilePath) == "" {
		return errors.New("file_path must be a non-empty string")
	}
	extension := strings.ToLower(extname(arguments.FilePath))
	if _, ok := imageExtensions[extension]; !ok && extension != "" {
		return fmt.Errorf("cannot read %q: the %s extension does not declare a supported image format; read_image accepts PNG/JPEG/WebP/GIF files, including extension-less files in those formats", arguments.FilePath, extension)
	}
	route := invocation.Route
	if route.Provider == "" || route.Model == "" {
		return fmt.Errorf("cannot read %q as an image: the current model route could not be resolved", arguments.FilePath)
	}
	if !route.ImageInput {
		return fmt.Errorf("cannot read %q as an image: model %q does not declare image input; switch to an image-capable model to read images", arguments.FilePath, route.Model)
	}
	return nil
}

// readImage reads one workspace image within the source byte limit,
// normalizes it, and records the observation like read does.
func (provider *Provider) readImage(ctx context.Context, invocation appTool.Invocation, arguments readImageArgs) (appTool.Result, error) {
	display, path, err := provider.root.Readable(arguments.FilePath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		provider.observed.record(invocation.SessionID, display, observation{})
		return appTool.Result{}, fmt.Errorf("cannot read %q: not found", display)
	case err != nil:
		return appTool.Result{}, fmt.Errorf("cannot read %q: %w", arguments.FilePath, err)
	}
	info, err := statFile(path)
	switch {
	case err != nil:
		return appTool.Result{}, fmt.Errorf("cannot read %q: %w", display, err)
	case !info.Mode().IsRegular():
		return appTool.Result{}, fmt.Errorf("cannot read %q: not a regular file", display)
	case info.Size() > session.MaxImageSourceBytes:
		return appTool.Result{}, fmt.Errorf("cannot read %q: %d bytes exceeds the %d-byte limit", display, info.Size(), session.MaxImageSourceBytes)
	}
	data, err := readImageBytes(path)
	if err != nil {
		return appTool.Result{}, fmt.Errorf("cannot read %q: %w", display, err)
	}
	declared := imageExtensions[strings.ToLower(extname(arguments.FilePath))]
	actual := sniffImage(data)
	switch {
	case actual == "" && declared == "":
		return appTool.Result{}, fmt.Errorf("cannot read %q: the file content is not a supported image format; read_image accepts PNG/JPEG/WebP/GIF", display)
	case actual == "":
		return appTool.Result{}, errUndecodable(display)
	case declared != "" && actual != declared:
		return appTool.Result{}, fmt.Errorf("cannot read %q: the %s extension declares %s, but the bytes use a different image format; rename the file to match its actual format if it is PNG/JPEG/WebP/GIF, or convert it to one of those formats", display, strings.ToLower(extname(arguments.FilePath)), declared)
	}
	// The image is durable before the result that references it is
	// committed, so the log never cites bytes the store does not hold.
	normalized, source, err := provider.images.SaveImage(ctx, filepath.Base(display), data)
	switch {
	case errors.Is(err, session.ErrImageFormat):
		return appTool.Result{}, errUndecodable(display)
	case errors.Is(err, session.ErrImagePixels):
		return appTool.Result{}, fmt.Errorf("cannot read %q: the image exceeds the %d-pixel decoded-size limit; downscale the image and read the smaller copy", display, session.MaxImageSourcePixels)
	case errors.Is(err, session.ErrImageBytes):
		return appTool.Result{}, fmt.Errorf("cannot read %q: the image cannot be stored within the deployment's byte limits; downscale the image and read the smaller copy", display)
	case err != nil:
		return appTool.Result{}, fmt.Errorf("cannot read %q: %w", display, err)
	}
	provider.observed.record(invocation.SessionID, path, observed(data))
	return appTool.Result{Text: formatImageRead(display, normalized, source), Image: &normalized}, nil
}

func errUndecodable(display string) error {
	return fmt.Errorf("cannot read %q: the bytes do not decode as a supported PNG/JPEG/WebP/GIF image; the file may be truncated or corrupt", display)
}

// readImageBytes reads a whole source within the byte limit; a file that
// grew after stat is refused rather than truncated.
func readImageBytes(path string) ([]byte, error) {
	reader, err := openFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }() // read-only; close cannot lose data
	data, err := io.ReadAll(io.LimitReader(reader, session.MaxImageSourceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > session.MaxImageSourceBytes {
		return nil, fmt.Errorf("content exceeds the %d-byte limit", session.MaxImageSourceBytes)
	}
	return data, nil
}

// sniffImage identifies a supported image from its complete file signature.
func sniffImage(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif"
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WEBP":
		return "image/webp"
	default:
		return ""
	}
}

// extname mirrors Node's path.extname, which upstream uses to classify a
// path: a leading dot alone is a dotfile name, not an extension.
func extname(path string) string {
	base := filepath.Base(path)
	index := strings.LastIndexByte(base, '.')
	if index <= 0 || base == ".." {
		return ""
	}
	return base[index:]
}

// formatImageRead renders upstream's envelope beside the image. A
// downscaled read names the source size and the multiplier that maps
// coordinates on the attached image back onto the file.
func formatImageRead(display string, image session.Image, source stdimage.Point) string {
	scaled := ""
	if source.X != image.Width || source.Y != image.Height {
		x := toFixed2(float64(source.X) / float64(image.Width))
		y := toFixed2(float64(source.Y) / float64(image.Height))
		advice := "multiply coordinates by " + x
		if x != y {
			advice = "multiply x coordinates by " + x + " and y coordinates by " + y
		}
		scaled = fmt.Sprintf(" (downscaled from %dx%d px; %s to locate features in the original file)", source.X, source.Y, advice)
	}
	return fmt.Sprintf("<path>%s</path>\n<type>image</type>\n<content>\n%s image, %dx%d px, %d bytes%s\n</content>", display, image.MediaType, image.Width, image.Height, image.Bytes, scaled)
}

// toFixed2 formats like JavaScript's toFixed(2). It differs from Go's
// correctly rounded formatting only on exact ties, the odd multiples of
// 1/8, where JavaScript rounds up instead of to even.
func toFixed2(value float64) string {
	if eighths := value * 8; eighths == math.Trunc(eighths) && int64(eighths)%2 != 0 {
		value += 0.001
	}
	return strconv.FormatFloat(value, 'f', 2, 64)
}
