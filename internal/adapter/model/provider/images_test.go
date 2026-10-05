package provider

import (
	"bytes"
	"encoding/base64"
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
	}}
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
		owner:    &Provider{id: id, client: server.Client()},
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
		request := llm.Request{Surface: imageResultRequest().Surface[1:5]}
		_, err := preparedFor(server, id, false).Stream(t.Context(), llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}, request, func(session.AssistantChunk) error { return nil })
		expectLLMError(t, err, llm.ErrorInvalid)
		if len(bodies()) != 0 {
			t.Fatalf("%s contacted the provider", id)
		}
	}
}

func sizedImage(name string, size int) *session.Image {
	return &session.Image{ID: "img-" + name, Name: name, MediaType: "image/jpeg", Data: strings.Repeat("A", size)}
}

func TestFitImages_OmitsTheOldestOccurrencesBeyondTheBudget(t *testing.T) {
	if surface := imageResultRequest().Surface; &fitImages(surface)[0] != &surface[0] {
		t.Fatal("a fitting surface was copied")
	}
	// Twenty-one small images: the oldest one exceeds the count budget.
	surface := []session.SurfaceNode{{Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{
		{Type: session.ContentImage, Image: sizedImage("first.png", 4)},
		{Type: session.ContentText, Text: "two"},
		{Type: session.ContentImage, Image: sizedImage("second.png", 4)},
	}}}}
	for index := range maxRequestImages - 1 {
		surface = append(surface, session.SurfaceNode{Result: &session.ToolResult{CallID: "c", Output: "r", Image: sizedImage("r", 4+index)}})
	}
	fitted := fitImages(surface)
	content := fitted[0].Message.Content
	if content[0].Type != session.ContentText || content[0].Text != `[image omitted to fit request image limits; "first.png" (img-first.png). No local normalized image path is available; ask the user to attach it again if needed.]` || content[2].Image == nil {
		t.Fatalf("fitted content=%#v", content)
	}
	if surface[0].Message.Content[0].Image == nil || fitted[1].Result != surface[1].Result {
		t.Fatal("fitting changed the input surface or copied untouched nodes")
	}

	// Bytes: a newer large image leaves no room for the older ones, including
	// both images of one message and an image-only tool result.
	large := maxRequestImageBytes - 10
	surface = []session.SurfaceNode{
		{Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{
			{Type: session.ContentImage, Image: sizedImage("a<b>&c.png", 8)},
			{Type: session.ContentImage, Image: sizedImage("b.png", 8)},
		}}},
		{Result: &session.ToolResult{CallID: "old", Image: sizedImage("old.png", 11)}},
		{Result: &session.ToolResult{CallID: "new", Output: "kept", Image: sizedImage("new.png", large)}},
	}
	fitted = fitImages(surface)
	if fitted[0].Message.Content[0].Type != session.ContentText || !strings.Contains(fitted[0].Message.Content[0].Text, `"a<b>&c.png" (img-a<b>&c.png)`) || fitted[0].Message.Content[1].Type != session.ContentText {
		t.Fatalf("message images=%#v", fitted[0].Message.Content)
	}
	if old := fitted[1].Result; old.Image != nil || !strings.HasPrefix(old.Output, `[image omitted to fit request image limits; "old.png"`) || surface[1].Result.Image == nil {
		t.Fatalf("old result=%#v", old)
	}
	if fitted[2].Result.Image == nil || fitted[2].Result.Output != "kept" {
		t.Fatalf("newest result=%#v", fitted[2].Result)
	}
	surface[2].Result.Output = "text"
	surface[1].Result.Output = "envelope"
	if got := fitImages(surface)[1].Result.Output; !strings.HasPrefix(got, "envelope\n[image omitted") {
		t.Fatalf("result placeholder=%q", got)
	}
}

func TestStream_FitsImagesBeforeBuildingTheWireRequest(t *testing.T) {
	server, bodies := recordingServer(t)
	data := make([]byte, maxRequestImageBytes*3/8+30)
	image := &session.Image{ID: "img-big", Name: "big.png", MediaType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(data)}
	request := llm.Request{Surface: []session.SurfaceNode{
		{Call: &session.ToolCall{ID: "call-1", Name: "read_image", Arguments: json.RawMessage(`{}`)}},
		{Result: &session.ToolResult{CallID: "call-1", Output: "first", Image: image}},
		{Call: &session.ToolCall{ID: "call-2", Name: "read_image", Arguments: json.RawMessage(`{}`)}},
		{Result: &session.ToolResult{CallID: "call-2", Output: "second", Image: image}},
	}}
	if _, err := preparedFor(server, "openai", true).Stream(t.Context(), llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}, request, func(session.AssistantChunk) error { return nil }); err != nil {
		t.Fatal(err)
	}
	sent := bodies()[0]
	if strings.Count(sent, `"input_image"`) != 1 || !strings.Contains(sent, `"output":"first\n[image omitted to fit request image limits; \"big.png\" (img-big). No local normalized image path`) {
		t.Fatalf("budgeted request has %d images", strings.Count(sent, `"input_image"`))
	}
	if !bytes.Contains([]byte(sent), []byte(`"call_id":"call-2","output":[{"type":"input_text","text":"second"}`)) {
		t.Fatal("newest image result was not kept")
	}
}

func TestQuoteJSON_MatchesJSONStringify(t *testing.T) {
	if got := quoteJSON("a\"<b>&\n"); got != `"a\"<b>&\n"` {
		t.Fatalf("quoted=%s", got)
	}
}

func TestResultContent_KeepsEmptyTextOnlyResultsOmitted(t *testing.T) {
	empty := &session.ToolResult{CallID: "call"}
	if responsesOutput(empty) != nil || anthropicResultContent(empty) != nil || resultText(empty) != "" {
		t.Fatal("an empty text result gained content")
	}
	encoded, _ := json.Marshal(responsesInput{Type: "function_call_output", CallID: "call", Output: responsesOutput(empty)})
	if strings.Contains(string(encoded), `"output"`) {
		t.Fatalf("empty output encoded as %s", encoded)
	}
}
