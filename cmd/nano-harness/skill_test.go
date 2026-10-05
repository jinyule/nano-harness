package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/core/session"
	coreskill "github.com/jinyule/nano-harness/internal/core/skill"
)

// requestTexts flattens every string in one decoded provider request body.
func requestTexts(value any) []string {
	switch typed := value.(type) {
	case string:
		return []string{typed}
	case []any:
		var texts []string
		for _, item := range typed {
			texts = append(texts, requestTexts(item)...)
		}
		return texts
	case map[string]any:
		var texts []string
		for _, item := range typed {
			texts = append(texts, requestTexts(item)...)
		}
		return texts
	}
	return nil
}

// skillServer is a loopback Responses endpoint that replies with scripted
// SSE bodies and keeps every request's strings for assertions.
type skillServer struct {
	mu       sync.Mutex
	replies  []string
	requests [][]string
}

func (server *skillServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var body any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		http.Error(writer, "bad json", http.StatusBadRequest)
		return
	}
	server.mu.Lock()
	server.requests = append(server.requests, requestTexts(body))
	index := len(server.requests) - 1
	reply := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"unexpected request\"}\n\n"
	if index < len(server.replies) {
		reply = server.replies[index]
	}
	server.mu.Unlock()
	writer.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(writer, reply+"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n")
}

func (server *skillServer) request(t *testing.T, index int) string {
	t.Helper()
	server.mu.Lock()
	defer server.mu.Unlock()
	if index >= len(server.requests) {
		t.Fatalf("provider received %d requests, want at least %d", len(server.requests), index+1)
	}
	return strings.Join(server.requests[index], "\n")
}

func skillCall(name string) string {
	arguments := fmt.Sprintf(`{\"name\":\"%s\"}`, name)
	return "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call-skill\",\"name\":\"skill\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"" + arguments + "\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call-skill\",\"name\":\"skill\",\"arguments\":\"" + arguments + "\"}}\n\n"
}

func textReply(text string) string {
	return "data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + text + "\"}\n\n"
}

// userKinds lists the source kinds of user messages committed in one turn.
func userKinds(t *testing.T, transcript []byte, turn uint64) []string {
	t.Helper()
	var kinds []string
	for _, line := range bytes.Split(bytes.TrimSpace(transcript), []byte("\n"))[1:] {
		var event session.Event
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Record.Turn == turn && event.Record.Type == session.RecordUserMessage {
			kinds = append(kinds, event.Record.Message.Source.Kind)
		}
	}
	return kinds
}

// TestComposition_SkillCatalogToolAndGesture drives the real composition:
// the first request carries the durable catalog, the model loads a skill
// with the skill tool, a hot-added skill produces a replacement catalog, a
// /name gesture injects a user-only skill, and a resumed process derives the
// published catalog from disk instead of republishing it.
func TestComposition_SkillCatalogToolAndGesture(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	server := &skillServer{replies: []string{
		skillCall("proof-skill"), textReply("loaded"),
		textReply("gesture handled"),
		textReply("resumed"),
	}}
	loopback := httptest.NewServer(server)
	t.Cleanup(loopback.Close)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	projectSkills := filepath.Join(root, ".agents", "skills")
	userSkills := filepath.Join(data, "skills")
	writeSkill := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeSkill(filepath.Join(projectSkills, "proof-skill", "SKILL.md"), "---\nname: proof-skill\ndescription: Prove   the <catalog> path.\n---\n\nAnswer with the proof marker PINEAPPLE.\n")
	writeSkill(filepath.Join(userSkills, "user-only.md"), "---\nname: user-only\ndescription: Only for users.\ndisable-model-invocation: true\n---\nSay the user marker MANGO.\n")
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 65536\n        tools: true\n", loopback.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(data, "sessions", "session-skill.jsonl")
	submit := func(texts ...string) []byte {
		t.Helper()
		config, err := normalizeConfig(applicationConfig{
			workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), settingsPath: settingsPath,
			credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: userSkills,
			agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-skill", maxSteps: 4,
		})
		if err != nil {
			t.Fatal(err)
		}
		assembled, err := composeTUI(config, dependencies{httpClient: loopback.Client()})
		if err != nil {
			t.Fatal(err)
		}
		if err := assembled.runtime.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		controller, err := assembled.root.Agent()
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range texts {
			results, err := controller.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}})
			if err != nil {
				t.Fatal(err)
			}
			if result := <-results; result.Err != nil || result.Outcome != session.OutcomeCompleted {
				t.Fatalf("turn %q = %+v", text, result)
			}
			if text == "use the proof skill" {
				writeSkill(filepath.Join(userSkills, "late-skill", "SKILL.md"), "---\nname: late-skill\ndescription: Added while running.\n---\nLate body.\n")
			}
		}
		shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if err := assembled.runtime.Shutdown(shutdown); err != nil {
			t.Fatal(err)
		}
		transcript, err := os.ReadFile(transcriptPath) //nolint:gosec // the path is rooted in this test's private temporary directory
		if err != nil {
			t.Fatal(err)
		}
		return transcript
	}

	submit("use the proof skill", "/user-only now")

	first := server.request(t, 0)
	for _, fact := range []string{"<available_skills>", "- `proof-skill`: Prove the &lt;catalog&gt; path.", "call the `skill` tool with the exact skill name"} {
		if !strings.Contains(first, fact) {
			t.Fatalf("first request lacks %q:\n%s", fact, first)
		}
	}
	if strings.Contains(first, "user-only") || strings.Contains(first, "PINEAPPLE") {
		t.Fatalf("first request leaked a hidden skill or a body:\n%s", first)
	}
	loaded := coreskill.RenderContent("proof-skill", filepath.Join(projectSkills, "proof-skill"), "Answer with the proof marker PINEAPPLE.")
	if second := server.request(t, 1); !strings.Contains(second, loaded) {
		t.Fatalf("second request lacks the loaded skill:\n%s", second)
	}
	third := server.request(t, 2)
	for _, fact := range []string{"The available skill catalog changed.", "- `late-skill`: Added while running.", `<skill_content name="user-only">`, "Say the user marker MANGO."} {
		if !strings.Contains(third, fact) {
			t.Fatalf("third request lacks %q:\n%s", fact, third)
		}
	}

	transcript := submit("after restart")
	if strings.Count(server.request(t, 3), "<available_skills>") != 2 {
		t.Fatalf("resumed request does not carry exactly the two durable catalogs:\n%s", server.request(t, 3))
	}
	for turn, want := range map[uint64][]string{
		1: {"user", coreskill.SourceCatalog},
		2: {"user", coreskill.SourceCatalog, coreskill.SourceInvocation},
		3: {"user"},
	} {
		if got := userKinds(t, transcript, turn); !slices.Equal(got, want) {
			t.Errorf("turn %d user messages = %v, want %v", turn, got, want)
		}
	}
	for _, fact := range []string{`"type":"tool/call"`, `"name":"skill"`, `"plugin":"skill-tools"`, "PINEAPPLE", "MANGO"} {
		if !bytes.Contains(transcript, []byte(fact)) {
			t.Fatalf("transcript lacks %s", fact)
		}
	}
}
