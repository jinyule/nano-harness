package provider

import (
	"encoding/base64"
	"errors"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	// imageOnlyResultText stands in for an empty tool result that carries an
	// image, as upstream's adapters send it.
	imageOnlyResultText = "(see attached image)"
	// toolImagesText introduces tool-result images that Chat Completions can
	// only carry in a following user message.
	toolImagesText = "Attached image(s) from tool result:"
)

// encodedImages is the base64 data of every image a request references,
// keyed by image ID.
type encodedImages map[string]string

// encodeImages encodes the verified bytes the LLM runtime attached for every
// image in the surface. A reference without bytes is an invalid request.
func encodeImages(providerID string, request llm.Request) (encodedImages, error) {
	encoded := encodedImages{}
	add := func(image *session.Image) error {
		if image == nil {
			return nil
		}
		data, ok := request.Images[image.ID]
		if !ok {
			return &llm.Error{Code: llm.ErrorInvalid, Provider: providerID, Cause: errors.New("request image bytes are missing")}
		}
		encoded[image.ID] = base64.StdEncoding.EncodeToString(data)
		return nil
	}
	for _, node := range request.Surface {
		if node.Message != nil {
			for _, block := range node.Message.Content {
				if err := add(block.Image); err != nil {
					return nil, err
				}
			}
		}
		if node.Result != nil {
			if err := add(node.Result.Image); err != nil {
				return nil, err
			}
		}
	}
	return encoded, nil
}

func (encoded encodedImages) dataURL(image *session.Image) string {
	return "data:" + image.MediaType + ";base64," + encoded[image.ID]
}

// resultText is the text a tool result sends beside its image.
func resultText(result *session.ToolResult) string {
	if result.Output == "" && result.Image != nil {
		return imageOnlyResultText
	}
	return result.Output
}
