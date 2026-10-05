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
	fixtureImageData   = "/9j/2wCEAAQDAwMDAgQDAwMEBAQFBgoGBgUFBgwICQcKDgwPDg4MDQ0PERYTDxAVEQ0NExoTFRcYGRkZDxIbHRsYHRYYGRgBBAQEBgUGCwYGCxgQDRAYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGBgYGP/AABEIAAEAAQMBIgACEQEDEQH/xAGiAAABBQEBAQEBAQAAAAAAAAAAAQIDBAUGBwgJCgsQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkqNDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+gEAAwEBAQEBAQEBAQAAAAAAAAECAwQFBgcICQoLEQACAQIEBAMEBwUEBAABAncAAQIDEQQFITEGEkFRB2FxEyIygQgUQpGhscEJIzNS8BVictEKFiQ04SXxFxgZGiYnKCkqNTY3ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqCg4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrCw8TFxsfIycrS09TV1tfY2dri4+Tl5ufo6ery8/T19vf4+fr/2gAMAwEAAhEDEQA/APF6KKK/KT+/j//Z"
	fixtureImageSHA256 = "d0eea4b962c16b0f039d6e746d9bb8e78d355cb5e7b5f95fbdc20d774f4a4ff0"
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
	if result == nil || result.Output != fixtureImageOutput || result.Image == nil || result.Image.SHA256 != fixtureImageSHA256 || result.Image.Width != 1 || result.Image.Name != "red.png" {
		t.Fatalf("image result=%+v", result)
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
	image := &coresession.Image{ID: "img-d0eea4b962c16b0f", Name: "red.png", MediaType: "image/jpeg", Data: fixtureImageData, SHA256: fixtureImageSHA256, Width: 1, Height: 1}
	for _, record := range []coresession.Record{
		{Type: coresession.RecordTurnStart, Turn: 1},
		{Type: coresession.RecordUserMessage, Turn: 1, Message: userMessage("look at red.png")},
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
		{"unknown-image-field", `"image":{"id":`, `"image":{"page":1,"id":`},
		{"digest-mismatch", `"sha256":"d0ee`, `"sha256":"00ee`},
		{"unsupported-media-type", `"media_type":"image/jpeg","data"`, `"media_type":"image/gif","data"`},
		{"zero-width", `"width":1,"height":1}}`, `"width":0,"height":1}}`},
		{"oversized-width", `"width":1,"height":1}}`, `"width":4097,"height":1}}`},
		{"invalid-base64", `"data":"/9j/`, `"data":"!9j/`},
		{"empty-name", `"name":"red.png"`, `"name":""`},
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

func TestLog_RemainingTracksTheSessionSizeLimit(t *testing.T) {
	manager, scope := startManager(t)
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	log, err := manager.Open(t.Context(), OpenOptions{SessionID: "remaining", Create: true, Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close(context.Background()) })
	info, err := os.Stat(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := log.Remaining(); got != maxSessionBytes-info.Size() {
		t.Fatalf("remaining after header = %d, file = %d", got, info.Size())
	}
	appendRecord(t, log, coresession.Record{Type: coresession.RecordTurnStart, Turn: 1})
	if info, err = os.Stat(log.Path()); err != nil || log.Remaining() != maxSessionBytes-info.Size() {
		t.Fatalf("remaining after append = %d, file = %d (%v)", log.Remaining(), info.Size(), err)
	}
}
