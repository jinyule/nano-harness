package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/core/session"
)

// todoServer streams one todo_write call, then a final answer, as Responses SSE.
func todoServer(t *testing.T, arguments string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			http.Error(writer, "bad path", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		send := func(event map[string]any) {
			encoded, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(writer, "data: %s\n\n", encoded)
		}
		if calls.Add(1) == 1 {
			item := map[string]any{"type": "function_call", "call_id": "call-todo", "name": "todo_write"}
			send(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
			send(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": arguments})
			item["arguments"] = arguments
			send(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		} else {
			send(map[string]any{"type": "response.output_text.delta", "delta": "planned"})
		}
		send(map[string]any{"type": "response.completed", "response": map[string]any{"usage": map[string]any{"input_tokens": 10, "output_tokens": 2}}})
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func todoConfig(t *testing.T, serverURL, root, data string) applicationConfig {
	t.Helper()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 8192\n        tools: true\n", serverURL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-todo", maxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func readTranscript(t *testing.T, path string) []session.Event {
	t.Helper()
	file, err := os.Open(path) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<22)
	scanner.Scan() // The header is covered by the session package; this test inspects records.
	var events []session.Event
	for scanner.Scan() {
		var event session.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestComposition_TodoWritePersistsAndReplaysAfterResume(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	arguments := `{"todos":[{"content":"write tests","status":"completed"},{"content":" implement ","status":"in_progress"},{"content":"document","status":"pending"}]}`
	server, calls := todoServer(t, arguments)
	root, data := t.TempDir(), t.TempDir()
	config := todoConfig(t, server.URL, root, data)
	assembled, err := composeTUI(config, dependencies{httpClient: server.Client()})
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
	results, err := rootAgent.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "plan the work"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.Err != nil || result.Outcome != session.OutcomeCompleted || result.Text != "planned" || calls.Load() != 2 {
		t.Fatalf("turn = %#v calls=%d", result, calls.Load())
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := assembled.runtime.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}

	want := []session.TodoItem{
		{Content: "write tests", Status: session.TodoCompleted},
		{Content: "implement", Status: session.TodoInProgress},
		{Content: "document", Status: session.TodoPending},
	}
	transcript := filepath.Join(data, "sessions", "session-todo.jsonl")
	events := readTranscript(t, transcript)
	var schema *session.ToolDefinition
	var write, result *session.Record
	for index := range events {
		record := &events[index].Record
		switch {
		case record.Type == session.RecordRequestHeader && schema == nil:
			for _, tool := range record.Header.Tools {
				if tool.Name == "todo_write" {
					schema = &tool
				}
			}
		case record.Type == session.RecordTodoWrite:
			write = record
		case record.Type == session.RecordToolResult:
			result = record
		}
	}
	if schema == nil || !bytes.Contains(schema.Parameters, []byte(`"enum":["pending","in_progress","completed"]`)) {
		t.Fatalf("request header lacks the todo_write schema: %+v", schema)
	}
	if write == nil || write.Todo.CallID != "call-todo" || !slices.Equal(write.Todo.Items, want) {
		t.Fatalf("todo/write = %+v", write)
	}
	if result == nil || result.Result.IsError || result.Result.Output != "Updated todo list: 1 pending, 1 in progress, 1 completed." {
		t.Fatalf("tool/result = %+v", result)
	}

	// A fresh process replays the standing plan from the durable log alone.
	config.create = false
	resumed, err := composeTUI(config, dependencies{httpClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resumed.runtime.Shutdown(context.Background()) })
	resumedAgent, err := resumed.root.Agent()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := resumedAgent.Events(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var plan []session.TodoItem
	for _, event := range replayed {
		plan = session.StandingTodos(plan, event.Record)
	}
	if !slices.Equal(plan, want) {
		t.Fatalf("replayed plan = %#v", plan)
	}
}

func TestCompositionID_BindsTodoToolSemantics(t *testing.T) {
	config := applicationConfig{workspaceRoot: "/workspace"}
	// The identity of the same composition without the todo tool provider.
	previous := sha256.Sum256([]byte("nano-harness-v2\x00/workspace\x00fs-tools-v1\x00search-tools-v2\x00shell-tools-v1\x00subagent-tools-v2\x00session-v2"))
	if compositionID(config) == hex.EncodeToString(previous[:]) {
		t.Fatal("sessions created without todo_write would resume under the todo composition")
	}
}
