package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/session"
)

// noImages is an attachment store that holds no image.
type noImages struct{}

func (noImages) ReadImage(context.Context, session.Image) ([]byte, error) {
	return nil, session.ErrAttachmentMissing
}

// mapImages counts reads and rejects references that disagree with truth.
type mapImages struct {
	mu       sync.Mutex
	data     map[string][]byte
	truth    map[string]session.Image
	failures map[string]error
	reads    map[string]int
}

func (images *mapImages) ReadImage(_ context.Context, image session.Image) ([]byte, error) {
	images.mu.Lock()
	defer images.mu.Unlock()
	if images.reads == nil {
		images.reads = map[string]int{}
	}
	images.reads[image.ID]++
	if err := images.failures[image.ID]; err != nil {
		return nil, err
	}
	if stored, ok := images.truth[image.ID]; ok && (stored.Bytes != image.Bytes || stored.MediaType != image.MediaType || stored.Width != image.Width || stored.Height != image.Height) {
		return nil, session.ErrAttachmentCorrupt
	}
	return images.data[image.ID], nil
}

// refImage is a reference whose ID is derived from its name.
func refImage(name string, size int) *session.Image {
	digest := sha256.Sum256([]byte(name))
	return &session.Image{ID: session.ImageID(hex.EncodeToString(digest[:])), Name: name, MediaType: "image/jpeg", Bytes: size, Width: 1, Height: 1}
}

func userImages(images ...*session.Image) *session.Message {
	content := []session.ContentBlock{{Type: session.ContentText, Text: "look"}}
	for _, image := range images {
		content = append(content, session.ContentBlock{Type: session.ContentImage, Image: image})
	}
	return &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: content}
}

func TestFitImages_OmitsTheOldestOccurrencesBeyondTheBudget(t *testing.T) {
	if surface := []session.SurfaceNode{{Message: userImages(refImage("a", 3))}}; &fitImages(surface)[0] != &surface[0] {
		t.Fatal("a fitting surface was copied")
	}
	// Twenty-one small images: the oldest exceeds the count budget.
	surface := []session.SurfaceNode{{Message: userImages(refImage("first.png", 3), refImage("second.png", 3))}}
	for range maxRequestImages - 1 {
		surface = append(surface, session.SurfaceNode{Result: &session.ToolResult{CallID: "c", Output: "r", Image: refImage("r", 3)}})
	}
	fitted := fitImages(surface)
	content := fitted[0].Message.Content
	want := `[image omitted to fit request image limits; "first.png" (` + refImage("first.png", 3).ID + `). No local normalized image path is available; ask the user to attach it again if needed.]`
	if content[1].Type != session.ContentText || content[1].Text != want || content[2].Image == nil {
		t.Fatalf("fitted content = %#v", content)
	}
	if surface[0].Message.Content[1].Image == nil || fitted[1].Result != surface[1].Result {
		t.Fatal("fitting changed the input surface or copied untouched nodes")
	}

	// Bytes: the newest image's base64 leaves no room for the older ones,
	// including both images of one message and an image-only result.
	large := maxRequestImageBytes / 4 * 3 // its base64 fills the budget exactly
	surface = []session.SurfaceNode{
		{Message: userImages(refImage("a<b>&c.png", 3), refImage("b.png", 3))},
		{Result: &session.ToolResult{CallID: "old", Image: refImage("old.png", 3)}},
		{Result: &session.ToolResult{CallID: "new", Output: "kept", Image: refImage("new.png", large)}},
	}
	fitted = fitImages(surface)
	if fitted[0].Message.Content[1].Type != session.ContentText || !strings.Contains(fitted[0].Message.Content[1].Text, `"a<b>&c.png" (sha256:`) || fitted[0].Message.Content[2].Type != session.ContentText {
		t.Fatalf("message images = %#v", fitted[0].Message.Content)
	}
	if old := fitted[1].Result; old.Image != nil || !strings.HasPrefix(old.Output, `[image omitted to fit request image limits; "old.png"`) || surface[1].Result.Image == nil {
		t.Fatalf("old result = %#v", old)
	}
	if fitted[2].Result.Image == nil || fitted[2].Result.Output != "kept" {
		t.Fatalf("newest result = %#v", fitted[2].Result)
	}
	surface[1].Result.Output = "envelope"
	if got := fitImages(surface)[1].Result.Output; !strings.HasPrefix(got, "envelope\n[image omitted") {
		t.Fatalf("result placeholder = %q", got)
	}
	if base64Length(1) != 4 || base64Length(3) != 4 || base64Length(4) != 8 {
		t.Fatal("base64 length")
	}
}

func TestResolveImages_ReadsOnceAndMarksUnavailableImages(t *testing.T) {
	kept, missing, corrupt := refImage("kept.png", 3), refImage("missing.png", 3), refImage("corrupt.png", 3)
	images := &mapImages{
		data:     map[string][]byte{kept.ID: []byte("abc")},
		failures: map[string]error{missing.ID: session.ErrAttachmentMissing, corrupt.ID: session.ErrAttachmentCorrupt},
	}
	surface := []session.SurfaceNode{
		{Message: userImages(kept, missing)},
		{Result: &session.ToolResult{CallID: "c1", Output: "envelope", Image: kept}},
		{Result: &session.ToolResult{CallID: "c2", Output: "envelope", Image: corrupt}},
		{Result: &session.ToolResult{CallID: "c3", Image: missing}},
	}
	resolved, data, err := resolveImages(context.Background(), images, surface)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 1 || string(data[kept.ID]) != "abc" || images.reads[kept.ID] != 1 || images.reads[missing.ID] != 1 || images.reads[corrupt.ID] != 1 {
		t.Fatalf("data = %v reads = %v", data, images.reads)
	}
	unavailable := `[image unavailable: "missing.png" (` + missing.ID + `) is missing or failed verification in the local attachment store]`
	if resolved[0].Message.Content[1].Image != kept || resolved[0].Message.Content[2].Text != unavailable {
		t.Fatalf("message = %#v", resolved[0].Message.Content)
	}
	if resolved[1].Result.Image != kept || resolved[2].Result.Image != nil || !strings.HasPrefix(resolved[2].Result.Output, "envelope\n[image unavailable: \"corrupt.png\"") || resolved[3].Result.Output != unavailable {
		t.Fatalf("results = %#v %#v %#v", resolved[1].Result, resolved[2].Result, resolved[3].Result)
	}
	if surface[0].Message.Content[2].Image != missing || surface[2].Result.Image != corrupt {
		t.Fatal("resolution changed the input surface")
	}

	failure := errors.New("disk")
	images.failures[kept.ID] = failure
	if _, _, err := resolveImages(context.Background(), images, surface); !errors.Is(err, failure) {
		t.Fatalf("read failure = %v", err)
	}
	text := []session.SurfaceNode{{Message: userImages()}}
	if resolved, data, err := resolveImages(context.Background(), images, text); err != nil || data != nil || &resolved[0] != &text[0] {
		t.Fatal("a surface without images was changed")
	}
}

func TestCallStream_AttachesVerifiedBytesOnlyForVisionModels(t *testing.T) {
	image := refImage("shot.png", 3)
	images := &mapImages{data: map[string][]byte{image.ID: []byte("abc")}}
	request := Request{Surface: []session.SurfaceNode{
		{Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "look"}, {Type: session.ContentImage, Image: image}}}},
	}}
	prepared := &fakePrepared{info: ModelInfo{ID: "vision", Vision: true}}
	call := &Call{provider: "fake", prepared: prepared, images: images}
	if _, err := call.Stream(context.Background(), request, func(session.AssistantChunk) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if string(prepared.request.Images[image.ID]) != "abc" || images.reads[image.ID] != 1 {
		t.Fatalf("request images = %v reads = %v", prepared.request.Images, images.reads)
	}
	images.failures = map[string]error{image.ID: errors.New("disk")}
	if _, err := call.Stream(context.Background(), request, func(session.AssistantChunk) error { return nil }); err == nil || !strings.Contains(err.Error(), "disk") {
		t.Fatalf("read failure = %v", err)
	}

	// A model without vision never triggers image reads; its provider refuses.
	textModel := &fakePrepared{info: ModelInfo{ID: "text"}}
	images.reads = nil
	call = &Call{provider: "fake", prepared: textModel, images: images}
	if _, err := call.Stream(context.Background(), request, func(session.AssistantChunk) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if textModel.request.Images != nil || len(images.reads) != 0 {
		t.Fatalf("text model read images: %v %v", textModel.request.Images, images.reads)
	}
}

func TestQuoteJSON_MatchesJSONStringify(t *testing.T) {
	if got := quoteJSON("a\"<b>&\n"); got != `"a\"<b>&\n"` {
		t.Fatalf("quoted = %s", got)
	}
}

func TestResolveImages_VerifiesEveryReferenceToOneObject(t *testing.T) {
	stored := refImage("shot.png", 3)
	for _, test := range []struct {
		name   string
		change func(*session.Image)
	}{
		{"bytes", func(image *session.Image) { image.Bytes = 1 }},
		{"media-type", func(image *session.Image) { image.MediaType = "image/png" }},
		{"width", func(image *session.Image) { image.Width = 9 }},
		{"height", func(image *session.Image) { image.Height = 9 }},
	} {
		for _, corruptFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/corrupt-first=%t", test.name, corruptFirst), func(t *testing.T) {
				conflicting := *stored
				test.change(&conflicting)
				again := *stored
				again.Name = "renamed.png"
				images := &mapImages{data: map[string][]byte{stored.ID: []byte("abc")}, truth: map[string]session.Image{stored.ID: *stored}}
				surface := []session.SurfaceNode{
					{Message: userImages(stored, &conflicting)},
					{Result: &session.ToolResult{CallID: "bad", Output: "second", Image: &conflicting}},
					{Result: &session.ToolResult{CallID: "good", Output: "third", Image: &again}},
				}
				if corruptFirst {
					surface[0].Message = userImages(&conflicting, stored)
				}
				resolved, data, err := resolveImages(context.Background(), images, surface)
				if err != nil {
					t.Fatal(err)
				}
				goodBlock, badBlock := 1, 2
				if corruptFirst {
					goodBlock, badBlock = 2, 1
				}
				content := resolved[0].Message.Content
				if content[goodBlock].Image != stored || resolved[2].Result.Image != &again || len(data) != 1 || string(data[stored.ID]) != "abc" {
					t.Fatalf("matching references were not kept: %#v %#v", content, resolved[2].Result)
				}
				placeholder := `[image unavailable: "shot.png" (` + stored.ID + `) is missing or failed verification in the local attachment store]`
				if content[badBlock].Type != session.ContentText || content[badBlock].Text != placeholder || resolved[1].Result.Image != nil || resolved[1].Result.Output != "second\n"+placeholder {
					t.Fatalf("conflicting references = %#v %#v", content[badBlock], resolved[1].Result)
				}
				if surface[0].Message.Content[badBlock].Image != &conflicting || surface[1].Result.Image != &conflicting {
					t.Fatal("resolution changed the input surface")
				}
				if images.reads[stored.ID] != 2 {
					t.Fatalf("reads = %v", images.reads)
				}
			})
		}
	}
}

func TestCallStream_KeepsTheImagePayloadWithinTheBudget(t *testing.T) {
	stored := refImage("large.png", session.MaxImageBytes)
	images := &mapImages{data: map[string][]byte{stored.ID: make([]byte, session.MaxImageBytes)}, truth: map[string]session.Image{stored.ID: *stored}}
	surface := []session.SurfaceNode{{Result: &session.ToolResult{CallID: "real", Output: "real", Image: stored}}}
	for index := range 5 {
		understated := *stored
		understated.Bytes = 1
		surface = append(surface, session.SurfaceNode{Result: &session.ToolResult{CallID: fmt.Sprintf("small-%d", index), Output: "small", Image: &understated}})
	}
	prepared := &fakePrepared{info: ModelInfo{ID: "vision", Vision: true}}
	call := &Call{provider: "fake", prepared: prepared, images: images}
	if _, err := call.Stream(context.Background(), Request{Surface: surface}, func(session.AssistantChunk) error { return nil }); err != nil {
		t.Fatal(err)
	}
	payload, kept, omitted := 0, 0, 0
	for _, node := range prepared.request.Surface {
		if node.Result != nil && node.Result.Image != nil {
			payload += base64Length(len(prepared.request.Images[node.Result.Image.ID]))
			kept++
		} else if node.Result != nil && strings.Contains(node.Result.Output, "[image unavailable:") {
			omitted++
		}
	}
	if payload == 0 || payload > maxRequestImageBytes {
		t.Fatalf("request image payload = %d bytes, budget %d", payload, maxRequestImageBytes)
	}
	if kept != 1 || omitted != 5 || images.reads[stored.ID] != 2 {
		t.Fatalf("kept=%d omitted=%d reads=%v", kept, omitted, images.reads)
	}
}
