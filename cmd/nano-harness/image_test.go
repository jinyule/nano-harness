package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
			case item.Role == "user" && !strings.HasPrefix(item.Content[0].Text, runtimeContextPrefix):
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
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
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

// attachmentObject returns the stored bytes behind ref under the store root
// and checks that the object is read-only.
func attachmentObject(t *testing.T, root string, ref *session.Image) []byte {
	t.Helper()
	digest, ok := session.ImageDigest(ref.ID)
	if !ok {
		t.Fatalf("reference ID %q", ref.ID)
	}
	path := filepath.Join(root, "v1", "objects", digest[:2], digest)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm() != 0o400 {
		t.Fatalf("attachment object = %v %v", info, err)
	}
	data, err := os.ReadFile(path) //nolint:gosec // the path is derived from the test-owned attachment root
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestComposition_ReadImageEndToEnd drives read_image through the real
// composition: the workspace PNG is normalized into the attachment store,
// the transcript keeps only its reference, the next provider request carries
// the stored bytes, a resumed process and a fork resolve the same object, and
// a missing object degrades to a placeholder instead of failing the turn.
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
	normalized := attachmentObject(t, config.attachmentRoot, result.Image)
	digest := sha256.Sum256(normalized)
	wantEnvelope := fmt.Sprintf("<path>%s</path>\n<type>image</type>\n<content>\nimage/jpeg image, 2048x682 px, %d bytes (downscaled from 3000x1000 px; multiply x coordinates by 1.46 and y coordinates by 1.47 to locate features in the original file)\n</content>", filepath.Join(resolved, "shots", "wide.png"), len(normalized))
	if result.Output != wantEnvelope || result.Image.MediaType != "image/jpeg" || result.Image.Width != 2048 || result.Image.Height != 682 || result.Image.Name != "wide.png" || result.Image.ID != session.ImageID(hex.EncodeToString(digest[:])) || result.Image.Bytes != len(normalized) {
		t.Fatalf("image result = %q %+v", result.Output, *result.Image)
	}
	if decoded, format, err := image.DecodeConfig(bytes.NewReader(normalized)); err != nil || format != "jpeg" || decoded.Width != 2048 {
		t.Fatalf("normalized bytes = %v %q %v", decoded, format, err)
	}
	encoded := base64.StdEncoding.EncodeToString(normalized)
	transcript, err := os.ReadFile(filepath.Join(data, "sessions", "session-image.jsonl")) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(transcript, []byte(encoded[:64])) || !bytes.Contains(transcript, []byte(`"image":{"id":"`+result.Image.ID+`"`)) {
		t.Fatal("the transcript holds image bytes instead of only the reference")
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
	dataURL := "data:image/jpeg;base64," + encoded
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
	// The fork shares the parent's object instead of copying its bytes.
	if entries, err := os.ReadDir(filepath.Join(config.attachmentRoot, "v1", "objects", strings.TrimPrefix(result.Image.ID, "sha256:")[:2])); err != nil || len(entries) != 1 {
		t.Fatalf("objects after fork = %v %v", entries, err)
	}

	// A missing object becomes a placeholder; the turn still completes.
	digestHex, _ := session.ImageDigest(result.Image.ID)
	if err := os.Remove(filepath.Join(config.attachmentRoot, "v1", "objects", digestHex[:2], digestHex)); err != nil {
		t.Fatal(err)
	}
	runImageTurn(t, config, scripted.server.Client(), "and again")
	requests = scripted.requests()
	last := requests[len(requests)-1]
	if strings.Contains(last, "input_image") || !strings.Contains(last, `[image unavailable: \"wide.png\" (`+result.Image.ID+`) is missing or failed verification in the local attachment store]`) {
		t.Fatalf("request after the object vanished: %.400s", last)
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

func TestCompositionID_BindsAttachmentReferences(t *testing.T) {
	config := applicationConfig{workspaceRoot: "/workspace"}
	// The identity of the same composition while images were inline.
	inline := sha256.Sum256([]byte("nano-harness-v2\x00/workspace\x00fs-tools-v3\x00search-tools-v3\x00shell-tools-v3\x00job-tools-v1\x00subagent-tools-v3\x00todo-tools-v1\x00web-tools-v1\x00question-tools-v1\x00plan-tools-v1\x00skill-tools-v1\x00goal-tools-v1\x00spill-v1\x00session-v2"))
	if compositionID(config) == hex.EncodeToString(inline[:]) {
		t.Fatal("sessions with inline images would resume under the attachment composition")
	}
}

// TestComposition_DamagedAttachmentsBecomePlaceholders proves that an object
// that is missing, truncated, rewritten with the same length, or of another
// type than its reference never reaches the provider: the request carries
// the placeholder instead, and the turn completes.
func TestComposition_DamagedAttachmentsBecomePlaceholders(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	scripted := newImageServer(t)
	root, data := t.TempDir(), t.TempDir()
	writeWidePNG(t, filepath.Join(root, "shots", "wide.png"))
	config := imageConfig(t, scripted.server.URL, root, data, true)
	runImageTurn(t, config, scripted.server.Client(), "describe shots/wide.png")
	config.create = false
	transcript := filepath.Join(data, "sessions", "session-image.jsonl")
	results := imageResults(readTranscript(t, transcript))
	if len(results) != 1 || results[0].Image == nil {
		t.Fatalf("tool results = %+v", results)
	}
	ref := results[0].Image
	digest, _ := session.ImageDigest(ref.ID)
	object := filepath.Join(config.attachmentRoot, "v1", "objects", digest[:2], digest)
	original := attachmentObject(t, config.attachmentRoot, ref)
	log, err := os.ReadFile(transcript) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	writeObject := func(content []byte) {
		t.Helper()
		_ = os.Chmod(object, 0o600)
		if err := os.WriteFile(object, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	flipped := append([]byte(nil), original...)
	flipped[len(flipped)/2] ^= 0xff
	placeholder := `[image unavailable: \"wide.png\" (` + ref.ID + `) is missing or failed verification in the local attachment store]`
	for _, test := range []struct {
		name   string
		damage func()
	}{
		{"missing", func() {
			if err := os.Remove(object); err != nil {
				t.Fatal(err)
			}
		}},
		{"truncated", func() { writeObject(original[:len(original)-10]) }},
		{"digest mismatch", func() { writeObject(flipped) }},
		{"type mismatch", func() {
			// The reference now claims PNG while the verified bytes are JPEG.
			changed := bytes.Replace(log, []byte(`"media_type":"image/jpeg","bytes":`), []byte(`"media_type":"image/png","bytes":`), 1)
			if bytes.Equal(changed, log) {
				t.Fatal("the transcript has no image reference to change")
			}
			if err := os.WriteFile(transcript, changed, 0o600); err != nil { //nolint:gosec // the path is rooted in this test's private temporary directory
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeObject(original)
			if err := os.WriteFile(transcript, log, 0o600); err != nil { //nolint:gosec // the path is rooted in this test's private temporary directory
				t.Fatal(err)
			}
			test.damage()
			runImageTurn(t, config, scripted.server.Client(), "and again")
			requests := scripted.requests()
			last := requests[len(requests)-1]
			if strings.Contains(last, "input_image") || !strings.Contains(last, placeholder) {
				t.Fatalf("request with a %s object: %.400s", test.name, last)
			}
		})
	}
}

// TestComposition_ConflictingReferencesToOneObjectBecomePlaceholders proves
// with the real store that a later reference to an already verified object
// is verified on its own: after the transcript understates the second
// reference's size, the next request keeps the first image and sends the
// placeholder for the second instead of its bytes.
func TestComposition_ConflictingReferencesToOneObjectBecomePlaceholders(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	scripted := newImageServer(t)
	root, data := t.TempDir(), t.TempDir()
	writeWidePNG(t, filepath.Join(root, "shots", "wide.png"))
	config := imageConfig(t, scripted.server.URL, root, data, true)
	runImageTurn(t, config, scripted.server.Client(), "describe shots/wide.png")
	config.create = false
	runImageTurn(t, config, scripted.server.Client(), "describe it again")
	transcript := filepath.Join(data, "sessions", "session-image.jsonl")
	results := imageResults(readTranscript(t, transcript))
	if len(results) != 2 || results[0].Image == nil || results[1].Image == nil || results[0].Image.ID != results[1].Image.ID {
		t.Fatalf("tool results = %+v", results)
	}
	ref := results[1].Image
	log, err := os.ReadFile(transcript) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	claim := fmt.Sprintf(`"bytes":%d,"width":%d`, ref.Bytes, ref.Width)
	last := bytes.LastIndex(log, []byte(claim))
	if last < 0 || bytes.Index(log, []byte(claim)) == last {
		t.Fatal("the transcript lacks two references to change")
	}
	understated := fmt.Sprintf(`"bytes":%d,"width":%d`, ref.Bytes-1, ref.Width)
	changed := append(append(append([]byte(nil), log[:last]...), understated...), log[last+len(claim):]...)
	if err := os.WriteFile(transcript, changed, 0o600); err != nil { //nolint:gosec // the path is rooted in this test's private temporary directory
		t.Fatal(err)
	}
	runImageTurn(t, config, scripted.server.Client(), "and again")
	requests := scripted.requests()
	request := requests[len(requests)-1]
	placeholder := `[image unavailable: \"wide.png\" (` + ref.ID + `) is missing or failed verification in the local attachment store]`
	if strings.Count(request, "input_image") != 1 || strings.Count(request, placeholder) != 1 {
		t.Fatalf("request keeps %d images and %d placeholders", strings.Count(request, "input_image"), strings.Count(request, placeholder))
	}
}
