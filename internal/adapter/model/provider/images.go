package provider

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	// maxRequestImages bounds the image occurrences sent in one request.
	maxRequestImages = 20
	// maxRequestImageBytes bounds the base64 image payload of one request and
	// leaves room for system, history, tools, and JSON framing inside
	// maxProviderRequestBytes.
	maxRequestImageBytes = 10 << 20
	// imageOnlyResultText stands in for an empty tool result that carries an
	// image, as upstream's adapters send it.
	imageOnlyResultText = "(see attached image)"
	// toolImagesText introduces tool-result images that Chat Completions can
	// only carry in a following user message.
	toolImagesText = "Attached image(s) from tool result:"
)

// imageOccurrence locates one image in a surface: block indexes a message's
// content, and -1 selects the node's tool-result image.
type imageOccurrence struct {
	node, block int
	size        int
}

// fitImages keeps the newest image occurrences that fit the request image
// budget and replaces every older one with upstream's offload placeholder.
// The choice depends only on the surface, so requests rebuilt from the same
// log omit the same images. The input surface is not modified.
func fitImages(surface []session.SurfaceNode) []session.SurfaceNode {
	var occurrences []imageOccurrence
	for index, node := range surface {
		if node.Message != nil {
			for block, content := range node.Message.Content {
				if content.Type == session.ContentImage && content.Image != nil {
					occurrences = append(occurrences, imageOccurrence{node: index, block: block, size: len(content.Image.Data)})
				}
			}
		}
		if node.Result != nil && node.Result.Image != nil {
			occurrences = append(occurrences, imageOccurrence{node: index, block: -1, size: len(node.Result.Image.Data)})
		}
	}
	keep, count, total := len(occurrences), 0, 0
	for keep > 0 {
		size := occurrences[keep-1].size
		if count+1 > maxRequestImages || total+size > maxRequestImageBytes {
			break
		}
		count, total, keep = count+1, total+size, keep-1
	}
	if keep == 0 {
		return surface
	}
	fitted := slices.Clone(surface)
	for _, occurrence := range occurrences[:keep] {
		node := &fitted[occurrence.node]
		if occurrence.block < 0 {
			result := *node.Result
			result.Output = strings.TrimPrefix(result.Output+"\n"+offloadedImageText(*result.Image), "\n")
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
		node.Message.Content[occurrence.block] = session.ContentBlock{Type: session.ContentText, Text: offloadedImageText(*image)}
	}
	return fitted
}

// offloadedImageText is upstream's placeholder for an image omitted to fit
// request limits; this harness has no read-only normalized copy to offer.
func offloadedImageText(image session.Image) string {
	return "[image omitted to fit request image limits; " + quoteJSON(image.Name) + " (" + image.ID + "). No local normalized image path is available; ask the user to attach it again if needed.]"
}

// quoteJSON quotes text as JSON.stringify does, without HTML escaping.
func quoteJSON(text string) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(text) // encoding a string cannot fail
	return strings.TrimSuffix(buffer.String(), "\n")
}

func imageDataURL(image *session.Image) string {
	return "data:" + image.MediaType + ";base64," + image.Data
}

// resultText is the text a tool result sends beside its image.
func resultText(result *session.ToolResult) string {
	if result.Output == "" && result.Image != nil {
		return imageOnlyResultText
	}
	return result.Output
}
