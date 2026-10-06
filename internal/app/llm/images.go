package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	// maxRequestImages bounds the image occurrences sent in one request.
	maxRequestImages = 20
	// maxRequestImageBytes bounds the base64 image payload of one request and
	// leaves room for system, history, tools, and JSON framing inside every
	// provider's 16 MiB request body limit.
	maxRequestImageBytes = 10 << 20
)

// ImageReader reads the verified bytes of one stored image reference. A
// missing object wraps session.ErrAttachmentMissing and bytes that no longer
// match the reference wrap session.ErrAttachmentCorrupt.
type ImageReader interface {
	ReadImage(context.Context, session.Image) ([]byte, error)
}

// imageOccurrence locates one image in a surface: block indexes a message's
// content, and -1 selects the node's tool-result image.
type imageOccurrence struct {
	node, block int
	image       *session.Image
}

func imageOccurrences(surface []session.SurfaceNode) []imageOccurrence {
	var occurrences []imageOccurrence
	for index, node := range surface {
		if node.Message != nil {
			for block, content := range node.Message.Content {
				if content.Type == session.ContentImage && content.Image != nil {
					occurrences = append(occurrences, imageOccurrence{node: index, block: block, image: content.Image})
				}
			}
		}
		if node.Result != nil && node.Result.Image != nil {
			occurrences = append(occurrences, imageOccurrence{node: index, block: -1, image: node.Result.Image})
		}
	}
	return occurrences
}

// fitImages keeps the newest image occurrences that fit the request image
// budget and replaces every older one with upstream's offload placeholder.
// The choice depends only on the surface, so requests rebuilt from the same
// log omit the same images. The input surface is not modified.
func fitImages(surface []session.SurfaceNode) []session.SurfaceNode {
	occurrences := imageOccurrences(surface)
	keep, count, total := len(occurrences), 0, 0
	for keep > 0 {
		size := base64Length(occurrences[keep-1].image.Bytes)
		if count+1 > maxRequestImages || total+size > maxRequestImageBytes {
			break
		}
		count, total, keep = count+1, total+size, keep-1
	}
	return replaceImages(surface, occurrences[:keep], offloadedImageText)
}

// imageContent is what a read verifies against the stored object: the
// content address and the metadata the reference claims for it. The display
// name is not part of the content.
type imageContent struct {
	id            string
	mediaType     string
	bytes         int
	width, height int
}

func contentOf(image *session.Image) imageContent {
	return imageContent{id: image.ID, mediaType: image.MediaType, bytes: image.Bytes, width: image.Width, height: image.Height}
}

// resolveImages reads every image the surface still references. References
// that make the same claim about one object share a read; a reference whose
// metadata differs is read and verified on its own, so it can never borrow
// bytes verified for another claim and understate the request payload. An
// image whose object is missing or fails verification is replaced by a
// placeholder for this request, so a damaged store never blocks a session;
// any other read failure fails the request.
func resolveImages(ctx context.Context, reader ImageReader, surface []session.SurfaceNode) ([]session.SurfaceNode, map[string][]byte, error) {
	occurrences := imageOccurrences(surface)
	if len(occurrences) == 0 {
		return surface, nil, nil
	}
	images := map[string][]byte{}
	verified := map[imageContent]bool{}
	var omitted []imageOccurrence
	for _, occurrence := range occurrences {
		content := contentOf(occurrence.image)
		available, read := verified[content]
		if !read {
			data, err := reader.ReadImage(ctx, *occurrence.image)
			switch {
			case errors.Is(err, session.ErrAttachmentMissing), errors.Is(err, session.ErrAttachmentCorrupt):
			case err != nil:
				return nil, nil, fmt.Errorf("read request image %s: %w", content.id, err)
			default:
				images[content.id], available = data, true
			}
			verified[content] = available
		}
		if !available {
			omitted = append(omitted, occurrence)
		}
	}
	return replaceImages(surface, omitted, unavailableImageText), images, nil
}

// replaceImages swaps the given occurrences for placeholder text without
// modifying the input surface. A user image becomes a text block in place;
// a tool-result image becomes a line after the result text.
func replaceImages(surface []session.SurfaceNode, occurrences []imageOccurrence, placeholder func(session.Image) string) []session.SurfaceNode {
	if len(occurrences) == 0 {
		return surface
	}
	replaced := slices.Clone(surface)
	for _, occurrence := range occurrences {
		node := &replaced[occurrence.node]
		if occurrence.block < 0 {
			result := *node.Result
			result.Output = strings.TrimPrefix(result.Output+"\n"+placeholder(*result.Image), "\n")
			result.Image = nil
			node.Result = &result
			continue
		}
		if node.Message == surface[occurrence.node].Message {
			message := *node.Message
			message.Content = slices.Clone(message.Content)
			node.Message = &message
		}
		image := node.Message.Content[occurrence.block].Image
		node.Message.Content[occurrence.block] = session.ContentBlock{Type: session.ContentText, Text: placeholder(*image)}
	}
	return replaced
}

func base64Length(size int) int { return (size + 2) / 3 * 4 }

// offloadedImageText is upstream's placeholder for an image omitted to fit
// request limits; this harness offers no read-only path to the stored copy.
func offloadedImageText(image session.Image) string {
	return "[image omitted to fit request image limits; " + imageIdentity(image) + ". No local normalized image path is available; ask the user to attach it again if needed.]"
}

// unavailableImageText stands in for an image whose stored object is
// missing or no longer matches its reference.
func unavailableImageText(image session.Image) string {
	return "[image unavailable: " + imageIdentity(image) + " is missing or failed verification in the local attachment store]"
}

// imageIdentity names an image as upstream does: its quoted display name
// and its attachment ID.
func imageIdentity(image session.Image) string {
	return quoteJSON(image.Name) + " (" + image.ID + ")"
}

// quoteJSON quotes text as JSON.stringify does, without HTML escaping.
func quoteJSON(text string) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(text) // encoding a string cannot fail
	return strings.TrimSuffix(buffer.String(), "\n")
}
