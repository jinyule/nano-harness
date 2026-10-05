package session

import (
	"errors"
	"strings"
)

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
	// ErrAttachmentMissing reports a referenced image the attachment store does not hold.
	ErrAttachmentMissing = errors.New("image attachment is missing")
	// ErrAttachmentCorrupt reports stored bytes that no longer match their reference.
	ErrAttachmentCorrupt = errors.New("image attachment failed verification")
)

// imageIDPrefix starts every content-addressed image identifier.
const imageIDPrefix = "sha256:"

// ImageID is the content-addressed identifier of stored bytes with the
// given lowercase hex SHA-256 digest.
func ImageID(digest string) string { return imageIDPrefix + digest }

// ImageDigest returns the lowercase hex SHA-256 digest an image identifier
// names, and whether the identifier is well formed.
func ImageDigest(id string) (string, bool) {
	digest, found := strings.CutPrefix(id, imageIDPrefix)
	if !found || len(digest) != 64 {
		return "", false
	}
	for _, char := range digest {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return "", false
		}
	}
	return digest, true
}
