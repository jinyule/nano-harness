package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// resultImage is a tool-result image whose base64 data is "aW1hZ2U=".
func resultImage() *session.Image { return imageBlock().Image }

// imageResultRequest replays one step with two parallel image reads, then a
// steer, so Chat Completions must place both images after the result run.
func imageResultRequest() llm.Request {
	user := func(text string) *session.Message {
		return &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}
	}
	return llm.Request{Surface: []session.SurfaceNode{
		{Message: user("look")},
		{Call: &session.ToolCall{ID: "call-1", Name: "read_image", Arguments: json.RawMessage(`{"file_path":"a.png"}`)}},
		{Call: &session.ToolCall{ID: "call-2", Name: "read_image", Arguments: json.RawMessage(`{"file_path":"b.png"}`)}},
		{Result: &session.ToolResult{CallID: "call-1", Output: "envelope", Image: resultImage()}},
		{Result: &session.ToolResult{CallID: "call-2", Output: "", Image: resultImage()}},
		{Message: user("steer")},
	}, Images: imageBytes()}
}

// recordingServer answers every protocol with a short text completion and
// records each raw request body.
func recordingServer(t *testing.T) (*httptest.Server, func() []string) {
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
		case "/v1/responses":
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

func preparedFor(server *httptest.Server, id string, vision bool) *prepared {
	return &prepared{
		owner:    &Provider{id: id, client: server.Client(), idleTimeout: time.Minute},
		snapshot: &snapshot{baseURL: server.URL},
		info:     llm.ModelInfo{Provider: id, ID: "m", Vision: vision, Tools: true},
	}
}

func TestStream_SendsToolResultImagesInEachWireFormat(t *testing.T) {
	const url = `data:image/jpeg;base64,aW1hZ2U=`
	for _, test := range []struct {
		id    string
		wants []string
	}{
		{"openai", []string{
			`{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_text","text":"envelope"},{"type":"input_image","image_url":"` + url + `","detail":"auto"}]}`,
			`{"type":"function_call_output","call_id":"call-2","output":[{"type":"input_image","image_url":"` + url + `","detail":"auto"}]}`,
			`{"role":"user","content":[{"type":"input_text","text":"steer"}]}`,
		}},
		{"anthropic", []string{
			`{"type":"tool_result","tool_use_id":"call-1","content":[{"type":"text","text":"envelope"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"aW1hZ2U="}}]}`,
			`{"type":"tool_result","tool_use_id":"call-2","content":[{"type":"text","text":"(see attached image)"},{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"aW1hZ2U="}}]}`,
		}},
		{"openrouter", []string{
			`{"role":"tool","content":"envelope","tool_call_id":"call-1"},{"role":"tool","content":"(see attached image)","tool_call_id":"call-2"},` +
				`{"role":"user","content":[{"type":"text","text":"Attached image(s) from tool result:"},{"type":"image_url","image_url":{"url":"` + url + `"}},{"type":"image_url","image_url":{"url":"` + url + `"}}]},` +
				`{"role":"user","content":"steer"}`,
		}},
	} {
		t.Run(test.id, func(t *testing.T) {
			server, bodies := recordingServer(t)
			completion, err := preparedFor(server, test.id, true).Stream(t.Context(), llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}, imageResultRequest(), func(session.AssistantChunk) error { return nil })
			if err != nil || session.Text(completion.Message) != "seen" {
				t.Fatalf("completion=%#v err=%v", completion, err)
			}
			sent := bodies()
			if len(sent) != 1 {
				t.Fatalf("requests=%d", len(sent))
			}
			for _, want := range test.wants {
				if !strings.Contains(sent[0], want) {
					t.Errorf("request lacks %s\nbody: %s", want, sent[0])
				}
			}
		})
	}
}

func TestStream_RefusesToolResultImagesForTextModelsBeforeNetwork(t *testing.T) {
	for _, id := range []string{"openai", "anthropic", "openrouter"} {
		server, bodies := recordingServer(t)
		// The bytes are attached, so only the vision gate keeps this request
		// off the network.
		request := llm.Request{Surface: imageResultRequest().Surface[1:5], Images: imageBytes()}
		_, err := preparedFor(server, id, false).Stream(t.Context(), llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}, request, func(session.AssistantChunk) error { return nil })
		expectLLMError(t, err, llm.ErrorInvalid)
		if len(bodies()) != 0 {
			t.Fatalf("%s contacted the provider", id)
		}
	}
}

func TestResultContent_KeepsEmptyTextOnlyResultsOmitted(t *testing.T) {
	empty := &session.ToolResult{CallID: "call"}
	if responsesOutput(empty, nil) != nil || anthropicResultContent(empty, nil) != nil || resultText(empty) != "" {
		t.Fatal("an empty text result gained content")
	}
	encoded, _ := json.Marshal(responsesInput{Type: "function_call_output", CallID: "call", Output: responsesOutput(empty, nil)})
	if strings.Contains(string(encoded), `"output"`) {
		t.Fatalf("empty output encoded as %s", encoded)
	}
}

func TestRequests_RefuseImagesWithoutAttachedBytes(t *testing.T) {
	provider := &Provider{id: "openai"}
	model := llm.ModelInfo{Provider: "openai", ID: "m", Vision: true, Tools: true}
	for _, surface := range [][]session.SurfaceNode{
		providerRequest().Surface[:1],
		imageResultRequest().Surface[1:4],
	} {
		request := llm.Request{Surface: surface}
		for _, id := range []string{"openai", "anthropic", "openrouter"} {
			provider.id = id
			var err error
			switch id {
			case "openai":
				_, err = provider.responsesRequest(model, request)
			case "anthropic":
				_, err = provider.anthropicRequest(model, request)
			default:
				_, err = provider.chatRequest(model, request)
			}
			expectLLMError(t, err, llm.ErrorInvalid)
		}
	}
}
