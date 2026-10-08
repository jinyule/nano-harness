package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// structuredResultRequest replays tool results that carry every kind of
// durable-only data: classifications, metadata of several tools, and an
// image result with metadata. strip removes that data from the same surface.
func structuredResultRequest(strip bool) llm.Request {
	old := "before"
	results := []*session.ToolResult{
		{CallID: "call-read", Output: "read text", Meta: &session.ToolMeta{Read: &session.ReadMeta{Path: "a.go", Offset: 1, Lines: []session.ReadLine{{Number: 1, Text: "line"}}, TotalLines: 1}}},
		{CallID: "call-edit", Output: "edited", Meta: &session.ToolMeta{Edit: &session.EditMeta{Diffs: []session.FileDiff{{Path: "a.go", OldText: &old, NewText: "after"}}}}},
		{CallID: "call-missing", Output: `Error: cannot read "b.go": not found`, IsError: true, Error: &session.ToolError{Name: "FsError", Code: "FS_NOT_FOUND"}},
		{CallID: "call-image", Output: "image envelope", Image: resultImage(), Meta: &session.ToolMeta{ReadImage: &session.ReadImageMeta{Path: "a.png"}}},
		{CallID: "call-fetch", Output: "Fetched https://example.com/", Meta: &session.ToolMeta{WebFetch: &session.WebFetchMeta{URL: "https://example.com/", StatusCode: 404, Truncated: true}}},
	}
	surface := []session.SurfaceNode{{Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "work"}}}}}
	for _, result := range results {
		surface = append(surface, session.SurfaceNode{Call: &session.ToolCall{ID: result.CallID, Name: strings.TrimPrefix(result.CallID, "call-"), Arguments: json.RawMessage(`{}`)}})
	}
	for _, result := range results {
		if strip {
			result.Error, result.Meta = nil, nil
		}
		surface = append(surface, session.SurfaceNode{Result: result})
	}
	return llm.Request{System: "system", Surface: surface, Images: imageBytes()}
}

// structuredServer answers both Responses endpoints, Messages, and Chat
// Completions, recording every raw request body.
func structuredServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		switch request.URL.Path {
		case "/v1/responses", "/backend-api/codex/responses":
			_, _ = io.WriteString(writer, sse(`{"type":"response.output_text.delta","delta":"seen"}`, `{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`))
		case "/v1/messages":
			_, _ = io.WriteString(writer, sse(`{"type":"message_start","message":{"usage":{"input_tokens":1}}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"seen"}}`, `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`, `{"type":"message_stop"}`))
		default:
			_, _ = io.WriteString(writer, sse(`{"choices":[{"delta":{"content":"seen"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		}
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), bodies...)
	}
}

// Every wire format projects only call_id, output, is_error, and the image:
// a request byte-for-byte equals the one built from a surface without the
// durable-only error classification and metadata.
func TestStream_OmitsToolErrorsAndMetadataInEachWireFormat(t *testing.T) {
	apiKey := llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}
	for _, target := range []struct {
		name, id   string
		credential llm.Credential
	}{
		{"openai-responses", "openai", apiKey},
		{"codex-responses", "openai", llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "token", AccountID: "account"}},
		{"anthropic-messages", "anthropic", apiKey},
		{"openrouter-chat-completions", "openrouter", apiKey},
	} {
		t.Run(target.name, func(t *testing.T) {
			server, bodies := structuredServer(t)
			prepared := preparedFor(server, target.id, true)
			prepared.owner.auth.chatGPTBaseURL = server.URL
			for _, strip := range []bool{false, true} {
				completion, err := prepared.Stream(t.Context(), target.credential, structuredResultRequest(strip), func(session.AssistantChunk) error { return nil })
				if err != nil || session.Text(completion.Message) != "seen" {
					t.Fatalf("completion=%#v err=%v", completion, err)
				}
			}
			sent := bodies()
			if len(sent) != 2 || sent[0] != sent[1] {
				t.Fatalf("durable-only data changed the request:\n%s\n%s", sent[0], sent[len(sent)-1])
			}
			for _, private := range []string{"FsError", "FS_NOT_FOUND", "total_lines", "old_text", "read_image", "status_code", `"meta"`, `"error"`} {
				if strings.Contains(sent[0], private) {
					t.Errorf("request contains %s: %s", private, sent[0])
				}
			}
			for _, visible := range []string{"read text", `cannot read \"b.go\": not found`, "image envelope", "aW1hZ2U="} {
				if !strings.Contains(sent[0], visible) {
					t.Errorf("request lacks the model-visible %s", visible)
				}
			}
		})
	}
}
