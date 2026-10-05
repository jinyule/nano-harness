package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	coresession "github.com/jinyule/nano-harness/internal/core/session"
)

const (
	fixtureImageID     = "sha256:d0eea4b962c16b0f039d6e746d9bb8e78d355cb5e7b5f95fbdc20d774f4a4ff0"
	fixtureImageOutput = "<path>/synthetic/workspace/red.png</path>\n<type>image</type>\n<content>\nimage/jpeg image, 1x1 px, 600 bytes\n</content>"
)

func TestSessionV2Image_FrozenContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-image.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	manager, scope := startManager(t)
	t.Cleanup(func() {
		if err := scope.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(manager.config.Root, "fixture.jsonl")
	if err := os.WriteFile(path, fixture, 0o600); err != nil { //nolint:gosec // fixed fixture name under the test-owned private manager root
		t.Fatal(err)
	}
	_, events, err := manager.Inspect(t.Context(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 13 || events[12].Record.Outcome != coresession.OutcomeCompleted {
		t.Fatalf("events=%v", events)
	}
	surface, err := coresession.Surface(events)
	if err != nil || len(surface) != 5 {
		t.Fatalf("surface=%+v err=%v", surface, err)
	}
	result := surface[3].Result
	if result == nil || result.Output != fixtureImageOutput || result.Image == nil || result.Image.ID != fixtureImageID || result.Image.Bytes != 600 || result.Image.Width != 1 || result.Image.Name != "red.png" {
		t.Fatalf("image result=%+v", result)
	}
	if attached := surface[0].Message.Content[1].Image; attached == nil || *attached != *result.Image {
		t.Fatalf("user image=%+v", attached)
	}
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "fixture", Cwd: "/synthetic/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // fixed fixture name under the test-owned private manager root
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, fixture) {
		t.Fatal("read and closed resume changed a committed image fixture")
	}

	// The writer uses independently constructed records, never decoded fixture values.
	output := temporaryFile(t)
	header := coresession.Header{SessionID: "fixture", CompositionID: testCompositionID, CreatedAtUnixMS: 1, Cwd: "/synthetic/workspace"}
	if _, err := writeHeader(output, header, nil); err != nil {
		t.Fatal(err)
	}
	writer := &Log{file: output, header: header, active: true, size: int64(bytes.IndexByte(fixture, '\n') + 1)}
	image := &coresession.Image{ID: fixtureImageID, Name: "red.png", MediaType: "image/jpeg", Bytes: 600, Width: 1, Height: 1}
	attachment := *image
	user := userMessage("look at red.png")
	user.Content = append(user.Content, coresession.ContentBlock{Type: coresession.ContentImage, Image: &attachment})
	for _, record := range []coresession.Record{
		{Type: coresession.RecordTurnStart, Turn: 1},
		{Type: coresession.RecordUserMessage, Turn: 1, Message: user},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 1},
		{Type: coresession.RecordRequestHeader, Turn: 1, Step: 1, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 1, Message: assistantMessage("reading")},
		{Type: coresession.RecordToolCall, Turn: 1, Step: 1, Call: &coresession.ToolCall{ID: "call-image", Name: "read_image", Arguments: json.RawMessage(`{"file_path":"red.png"}`)}},
		{Type: coresession.RecordToolResult, Turn: 1, Step: 1, Result: &coresession.ToolResult{CallID: "call-image", Output: fixtureImageOutput, Image: image}},
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 1},
		{Type: coresession.RecordStepStart, Turn: 1, Step: 2},
		{Type: coresession.RecordRequestHeader, Turn: 1, Step: 2, Header: &coresession.RequestHeader{Provider: "openai", Model: "model"}},
		{Type: coresession.RecordAssistantMessage, Turn: 1, Step: 2, Message: assistantMessage("a red pixel")},
		{Type: coresession.RecordStepEnd, Turn: 1, Step: 2},
		{Type: coresession.RecordTurnEnd, Turn: 1, Outcome: coresession.OutcomeCompleted},
	} {
		appendRecord(t, writer, record)
	}
	actual, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, fixture) {
		t.Fatalf("writer differs from frozen v2 image fixture:\n%s", actual)
	}
}

func TestSessionV2Image_RejectsChangedContract(t *testing.T) {
	fixture, err := os.ReadFile("testdata/session-v2-image.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, from, to string }{
		{"inline-data", `"bytes":600,"width":1,"height":1}}}}`, `"bytes":600,"width":1,"height":1,"data":"/9j/"}}}}`},
		{"inline-digest", `"image":{"id":`, `"image":{"sha256":"d0ee","id":`},
		{"unknown-image-field", `"image":{"id":`, `"image":{"page":1,"id":`},
		{"legacy-id", `"image":{"id":"sha256:d0ee`, `"image":{"id":"img-d0ee`},
		{"uppercase-digest", `"image":{"id":"sha256:d0ee`, `"image":{"id":"sha256:D0EE`},
		{"short-digest", `4a4ff0","name":"red.png","media_type":"image/jpeg","bytes":600,"width":1,"height":1}}}}`, `4a4ff","name":"red.png","media_type":"image/jpeg","bytes":600,"width":1,"height":1}}}}`},
		{"unsupported-media-type", `"media_type":"image/jpeg","bytes":600,"width":1,"height":1}}}}`, `"media_type":"image/gif","bytes":600,"width":1,"height":1}}}}`},
		{"zero-bytes", `"bytes":600,"width":1,"height":1}}}}`, `"bytes":0,"width":1,"height":1}}}}`},
		{"oversized", `"bytes":600,"width":1,"height":1}}}}`, `"bytes":4194305,"width":1,"height":1}}}}`},
		{"zero-width", `"width":1,"height":1}}}}`, `"width":0,"height":1}}}}`},
		{"oversized-width", `"width":1,"height":1}}}}`, `"width":4097,"height":1}}}}`},
		{"empty-name", `"is_error":false,"image":{"id":"sha256:d0eea4b962c16b0f039d6e746d9bb8e78d355cb5e7b5f95fbdc20d774f4a4ff0","name":"red.png"`, `"is_error":false,"image":{"id":"sha256:d0eea4b962c16b0f039d6e746d9bb8e78d355cb5e7b5f95fbdc20d774f4a4ff0","name":""`},
		{"user-image-name", `"name":"red.png","media_type":"image/jpeg","bytes":600,"width":1,"height":1}}],`, `"name":"","media_type":"image/jpeg","bytes":600,"width":1,"height":1}}],`},
		{"error-with-image", `"is_error":false,"image"`, `"is_error":true,"image"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := bytes.Replace(fixture, []byte(test.from), []byte(test.to), 1)
			if bytes.Equal(changed, fixture) {
				t.Fatalf("replacement %q did not change the fixture", test.from)
			}
			file := temporaryFile(t)
			if _, err := file.Write(changed); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := readSession(file, testCompositionID); !errors.Is(err, ErrCorruptSession) {
				t.Fatalf("got %v, want %v", err, ErrCorruptSession)
			}
		})
	}
}
