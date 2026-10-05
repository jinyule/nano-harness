package session

import "errors"

// Image normalization admits sources within these limits. The normalizer
// enforces them and tools that explain its refusals name the same values.
const (
	// MaxImageSourceBytes bounds one encoded source image before normalization.
	MaxImageSourceBytes = 20 << 20
	// MaxImageSourcePixels bounds the decoded width times height of one source.
	MaxImageSourcePixels = 16_000_000
)

var (
	// ErrImageFormat reports source bytes that are not a supported image or do not decode.
	ErrImageFormat = errors.New("unsupported or undecodable image")
	// ErrImagePixels reports a source whose decoded size exceeds MaxImageSourcePixels.
	ErrImagePixels = errors.New("image exceeds the decoded pixel limit")
	// ErrImageBytes reports a source that cannot be normalized within MaxImageBytes.
	ErrImageBytes = errors.New("normalized image exceeds the byte limit")
)
