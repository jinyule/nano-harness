package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// imageServer scripts a Responses model by the latest user text: a task
// naming CHILD_VIEW answers at once, "fork it" calls subagent_fork, "describe"
// calls read_image, and every task answers once its call has a result. It
// records every request body.
type imageServer struct {
	server *httptest.Server
	mu     sync.Mutex
	bodies []string
}

func newImageServer(t *testing.T) *imageServer {
	t.Helper()
	scripted := &imageServer{}
	scripted.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		scripted.mu.Lock()
		scripted.bodies = append(scripted.bodies, string(body))
		scripted.mu.Unlock()
		var parsed struct {
			Input []struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		_ = json.Unmarshal(body, &parsed)
		task, answered := "", false
		for _, item := range parsed.Input {
			switch {
			case item.Role == "user":
				task, answered = "", false
				for _, block := range item.Content {
					task += block.Text
				}
			case item.Type == "function_call_output":
				answered = true
			}
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		send := func(event map[string]any) {
			encoded, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(writer, "data: %s\n\n", encoded)
		}
		calls := 0
		call := func(name, arguments string) {
			item := map[string]any{"type": "function_call", "call_id": fmt.Sprintf("call-%s", name), "name": name}
			if calls > 0 {
				item["call_id"] = fmt.Sprintf("call-%s-%d", name, calls)
			}
			send(map[string]any{"type": "response.output_item.added", "output_index": calls, "item": item})
			send(map[string]any{"type": "response.function_call_arguments.delta", "output_index": calls, "delta": arguments})
			item["arguments"] = arguments
			send(map[string]any{"type": "response.output_item.done", "output_index": calls, "item": item})
			calls++
		}
		switch {
		case strings.Contains(task, "CHILD_VIEW"):
			send(map[string]any{"type": "response.output_text.delta", "delta": "CHILD_SAW"})
		case strings.Contains(task, "fork it") && !answered:
			call("subagent_fork", `{"description":"viewer","prompt":"CHILD_VIEW"}`)
		case strings.Contains(task, "describe both") && !answered:
			call("read_image", `{"file_path":"shots/tiny.png"}`)
			call("read_image", `{"file_path":"shots/wide.png"}`)
		case strings.Contains(task, "describe") && !answered:
			call("read_image", `{"file_path":"shots/wide.png"}`)
		default:
			send(map[string]any{"type": "response.output_text.delta", "delta": "a wide red banner"})
		}
		send(map[string]any{"type": "response.completed", "response": map[string]any{"usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}})
	}))
	t.Cleanup(scripted.server.Close)
	return scripted
}

func (scripted *imageServer) requests() []string {
	scripted.mu.Lock()
	defer scripted.mu.Unlock()
	return append([]string(nil), scripted.bodies...)
}

func imageConfig(t *testing.T, serverURL, root, data string, vision bool) applicationConfig {
	t.Helper()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 65536\n        vision: %t\n        tools: true\n", serverURL, vision)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-image", maxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// writeWidePNG writes a 3000x1000 PNG, wider than the 2048 normalized edge.
func writeWidePNG(t *testing.T, path string) { writePNG(t, path, 3000, 1000) }

func writePNG(t *testing.T, path string, width, height int) {
	t.Helper()
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			picture.SetRGBA(x, y, color.RGBA{R: 220, G: uint8(x / 12), B: uint8(y), A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runImageTurn(t *testing.T, config applicationConfig, client *http.Client, text string) {
	t.Helper()
	assembled, err := composeTUI(config, dependencies{httpClient: client})
	if err != nil {
		t.Fatal(err)
	}
	if err := assembled.runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	rootAgent, err := assembled.root.Agent()
	if err != nil {
		t.Fatal(err)
	}
	results, err := rootAgent.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.Err != nil || result.Outcome != session.OutcomeCompleted {
		t.Fatalf("turn = %#v (%v)", result, result.Err)
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := assembled.runtime.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
}

func imageResults(events []session.Event) []*session.ToolResult {
	var results []*session.ToolResult
	for _, event := range events {
		if event.Record.Type == session.RecordToolResult {
			results = append(results, event.Record.Result)
		}
	}
	return results
}

// TestComposition_ReadImageEndToEnd drives read_image through the real
// composition: the workspace PNG is normalized into the durable tool result,
// the next provider request carries that exact image, and a resumed process
// replays it from the log.
func TestComposition_ReadImageEndToEnd(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	scripted := newImageServer(t)
	root, data := t.TempDir(), t.TempDir()
	writeWidePNG(t, filepath.Join(root, "shots", "wide.png"))
	config := imageConfig(t, scripted.server.URL, root, data, true)
	runImageTurn(t, config, scripted.server.Client(), "describe shots/wide.png")

	events := readTranscript(t, filepath.Join(data, "sessions", "session-image.jsonl"))
	results := imageResults(events)
	if len(results) != 1 || results[0].IsError || results[0].Image == nil {
		t.Fatalf("tool results = %+v", results)
	}
	result := results[0]
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := base64.StdEncoding.DecodeString(result.Image.Data)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(normalized)
	wantEnvelope := fmt.Sprintf("<path>%s</path>\n<type>image</type>\n<content>\nimage/jpeg image, 2048x682 px, %d bytes (downscaled from 3000x1000 px; multiply x coordinates by 1.46 and y coordinates by 1.47 to locate features in the original file)\n</content>", filepath.Join(resolved, "shots", "wide.png"), len(normalized))
	if result.Output != wantEnvelope || result.Image.MediaType != "image/jpeg" || result.Image.Width != 2048 || result.Image.Height != 682 || result.Image.Name != "wide.png" || result.Image.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("image result = %q %+v", result.Output, *result.Image)
	}
	if decoded, format, err := image.DecodeConfig(bytes.NewReader(normalized)); err != nil || format != "jpeg" || decoded.Width != 2048 {
		t.Fatalf("normalized bytes = %v %q %v", decoded, format, err)
	}
	var schema bool
	for _, event := range events {
		if header := event.Record.Header; header != nil {
			for _, tool := range header.Tools {
				schema = schema || tool.Name == "read_image"
			}
		}
	}
	if !schema {
		t.Fatal("request header lacks read_image")
	}

	// The next request carries the persisted image inside the tool output.
	dataURL := "data:image/jpeg;base64," + result.Image.Data
	requests := scripted.requests()
	if len(requests) != 2 || strings.Contains(requests[0], "input_image") {
		t.Fatalf("requests = %d", len(requests))
	}
	var second struct {
		Input []struct {
			Type   string          `json:"type"`
			CallID string          `json:"call_id"`
			Output json.RawMessage `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(requests[1]), &second); err != nil {
		t.Fatal(err)
	}
	// Field order follows the wire structs: type, text, image_url, detail.
	wantOutput, _ := json.Marshal([]struct {
		Type     string `json:"type"`
		Text     string `json:"text,omitempty"`
		ImageURL string `json:"image_url,omitempty"`
		Detail   string `json:"detail,omitempty"`
	}{{Type: "input_text", Text: wantEnvelope}, {Type: "input_image", ImageURL: dataURL, Detail: "auto"}})
	var found bool
	for _, item := range second.Input {
		if item.Type == "function_call_output" && item.CallID == "call-read_image" {
			found = compactJSON(t, item.Output) == compactJSON(t, wantOutput)
		}
	}
	if !found {
		t.Fatalf("second request lacks the image output: %.400s", requests[1])
	}

	// A resumed process rebuilds the same image from the durable log, and a
	// fork seeded with the completed turn sends it from the child session.
	config.create = false
	runImageTurn(t, config, scripted.server.Client(), "fork it")
	requests = scripted.requests()
	if len(requests) != 5 || !strings.Contains(requests[2], dataURL) {
		t.Fatalf("resumed request lacks the replayed image (%d requests)", len(requests))
	}
	if child := requests[3]; !strings.Contains(child, "CHILD_VIEW") || !strings.Contains(child, `"function_call_output","call_id":"call-read_image","output":[`) || !strings.Contains(child, dataURL) {
		t.Fatalf("forked child request lacks the inherited image: %.300s", child)
	}
	if !strings.Contains(requests[4], "CHILD_SAW") {
		t.Fatal("parent did not receive the fork result")
	}
}

// TestComposition_ReadImageRefusesTextOnlyModels proves the route gate in
// the real composition: no image enters the log or the provider request.
func TestComposition_ReadImageRefusesTextOnlyModels(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	scripted := newImageServer(t)
	root, data := t.TempDir(), t.TempDir()
	writeWidePNG(t, filepath.Join(root, "shots", "wide.png"))
	runImageTurn(t, imageConfig(t, scripted.server.URL, root, data, false), scripted.server.Client(), "describe shots/wide.png")
	results := imageResults(readTranscript(t, filepath.Join(data, "sessions", "session-image.jsonl")))
	want := `Error: cannot read "shots/wide.png" as an image: model "test-model" does not declare image input; switch to an image-capable model to read images`
	if len(results) != 1 || !results[0].IsError || results[0].Output != want || results[0].Image != nil {
		t.Fatalf("tool results = %+v", results)
	}
	for _, request := range scripted.requests() {
		if strings.Contains(request, "input_image") {
			t.Fatal("a text-only request carried an image")
		}
	}
}

func TestCompositionID_BindsReadImageSemantics(t *testing.T) {
	config := applicationConfig{workspaceRoot: "/workspace"}
	// The identity of the same composition before fs-tools gained read_image.
	previous := sha256.Sum256([]byte("nano-harness-v2\x00/workspace\x00fs-tools-v2\x00search-tools-v3\x00shell-tools-v3\x00job-tools-v1\x00subagent-tools-v2\x00todo-tools-v1\x00web-tools-v1\x00question-tools-v1\x00plan-tools-v1\x00skill-tools-v1\x00spill-v1\x00session-v2"))
	if compositionID(config) == hex.EncodeToString(previous[:]) {
		t.Fatal("sessions created without read_image would resume under the image composition")
	}
}

// imageReserve mirrors the agent's image reserve: images may never use the
// last 8 MiB of a session's capacity.
const imageReserve = 8 << 20

// fillSession appends one closed turn of text input to the session until it
// can accept only about imageReserve+headroom more bytes.
func fillSession(t *testing.T, config applicationConfig, headroom int64) {
	t.Helper()
	manager, err := sessionjsonl.New(sessionjsonl.Config{Root: config.sessionRoot, CompositionID: compositionID(config)})
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := manager.Start(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = scope.Close(context.Background()) }()
	log, err := manager.Open(t.Context(), sessionjsonl.OpenOptions{SessionID: config.sessionID, Cwd: config.workspaceRoot})
	if err != nil {
		t.Fatal(err)
	}
	events, err := log.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	turn := nextTurnNumber(events)
	if _, err := log.Append(t.Context(), session.Record{Type: session.RecordTurnStart, Turn: turn}); err != nil {
		t.Fatal(err)
	}
	// A compaction summary shadows the filler, so requests stay small while
	// the raw log keeps every byte. The filler stops early enough for the
	// compaction and closing records to fit.
	closing := int64(16 << 10)
	target := int64(imageReserve) + headroom + closing
	var filler []uint64
	for {
		gap := log.Remaining() - target
		size := min(int64(session.MaxTextBytes), gap-256)
		if size < 1 {
			break
		}
		message := &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: strings.Repeat("x", int(size))}}}
		event, err := log.Append(t.Context(), session.Record{Type: session.RecordUserMessage, Turn: turn, Message: message})
		if err != nil {
			t.Fatal(err)
		}
		filler = append(filler, event.Sequence)
	}
	summary := &session.CompactionData{ID: "filler", ShadowedSeqs: filler, ShadowedTokenCount: 1, Summary: []session.ContentBlock{{Type: session.ContentText, Text: "earlier filler"}}, Provider: "openai", Model: "test-model"}
	for _, record := range []session.Record{
		{Type: session.RecordCompactionStart, Turn: turn, Compaction: &session.CompactionData{ID: "filler"}},
		{Type: session.RecordCompactionSummary, Turn: turn, Compaction: summary},
		{Type: session.RecordCompactionEnd, Turn: turn, Compaction: &session.CompactionData{ID: "filler"}},
		{Type: session.RecordTurnEnd, Turn: turn, Outcome: session.OutcomeCompleted},
	} {
		if _, err := log.Append(t.Context(), record); err != nil {
			t.Fatal(err)
		}
	}
	if remaining := log.Remaining(); remaining < imageReserve+headroom || remaining > imageReserve+headroom+closing+1024 {
		t.Fatalf("filled session has %d bytes left", remaining)
	}
	if err := log.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func nextTurnNumber(events []session.Event) uint64 {
	var turn uint64
	for _, event := range events {
		turn = max(turn, event.Record.Turn)
	}
	return turn + 1
}

// TestComposition_ReadImageRefusesImagesTheSessionCannotHold proves that a
// session near its size limit keeps working: an image that would eat into
// the reserve becomes a tool error the model sees, a smaller image in the
// same batch is kept, attachments are refused before anything is written,
// and later text turns still commit.
func TestComposition_ReadImageRefusesImagesTheSessionCannotHold(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	scripted := newImageServer(t)
	root, data := t.TempDir(), t.TempDir()
	writeWidePNG(t, filepath.Join(root, "shots", "wide.png"))
	writePNG(t, filepath.Join(root, "shots", "tiny.png"), 2, 2)
	config := imageConfig(t, scripted.server.URL, root, data, true)
	runImageTurn(t, config, scripted.server.Client(), "hello")
	config.create = false
	// The tiny image needs about 2 KB; the normalized wide image needs far
	// more than the 30-46 KB left above the reserve.
	fillSession(t, config, 30_000)
	runImageTurn(t, config, scripted.server.Client(), "describe both")

	transcript := filepath.Join(data, "sessions", "session-image.jsonl")
	results := imageResults(readTranscript(t, transcript))
	if len(results) != 2 || results[0].IsError || results[0].Image == nil || !results[1].IsError || results[1].Image != nil {
		t.Fatalf("tool results = %+v", results)
	}
	if want := "Error: the image was not kept: it needs about "; !strings.HasPrefix(results[1].Output, want) || !strings.HasSuffix(results[1].Output, "start a new session to read more images") {
		t.Fatalf("refusal = %q", results[1].Output)
	}
	requests := scripted.requests()
	followup := requests[len(requests)-1]
	if strings.Count(followup, "input_image") != 1 || !strings.Contains(followup, "the image was not kept") {
		t.Fatal("the model did not see the kept image and the refusal")
	}

	// An attachment that does not fit is refused before anything is written.
	before, err := os.Stat(transcript)
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := composeTUI(config, dependencies{httpClient: scripted.server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := assembled.runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	rootAgent, err := assembled.root.Agent()
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0xa5}, 64<<10)
	digest := sha256.Sum256(payload)
	attachment := &session.Image{ID: "img-large", Name: "large.jpg", MediaType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(payload), SHA256: hex.EncodeToString(digest[:]), Width: 64, Height: 64}
	message := session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "look"}, {Type: session.ContentImage, Image: attachment}}}
	if _, err := rootAgent.Submit(t.Context(), message); !errors.Is(err, agent.ErrImageCapacity) || !strings.Contains(err.Error(), "start a new session to attach them") {
		t.Fatalf("attachment = %v", err)
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := assembled.runtime.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Stat(transcript); err != nil || after.Size() != before.Size() {
		t.Fatalf("refused attachment changed the transcript: %v", err)
	}

	// Text turns still commit, and the log stays a valid session.
	runImageTurn(t, config, scripted.server.Client(), "and now just text")
	manager, err := sessionjsonl.New(sessionjsonl.Config{Root: config.sessionRoot, CompositionID: compositionID(config)})
	if err != nil {
		t.Fatal(err)
	}
	inspectScope := &plugin.Scope{}
	if err := manager.Start(t.Context(), inspectScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inspectScope.Close(context.Background()) })
	if _, events, err := manager.Inspect(t.Context(), config.sessionID); err != nil || events[len(events)-1].Record.Outcome != session.OutcomeCompleted {
		t.Fatalf("inspect = %v", err)
	}
}
