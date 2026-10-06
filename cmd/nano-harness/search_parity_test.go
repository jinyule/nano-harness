package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestComposition_ECMAScriptBlankQueriesAndSearchArguments(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	var chats, searches atomic.Int32
	query := "\ufeff\u0085\ufeff"
	queries := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body struct {
			Tools []struct {
				Type string `json:"type"`
			} `json:"tools"`
			Input []struct {
				Content json.RawMessage `json:"content"`
			} `json:"input"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		if len(body.Tools) == 1 && body.Tools[0].Type == "web_search" {
			searches.Add(1)
			if len(body.Input) > 0 {
				var content []struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(body.Input[0].Content, &content); err != nil || len(content) != 1 {
					http.Error(writer, "invalid search query", http.StatusBadRequest)
					return
				}
				queries <- content[0].Text
			}
			_, _ = io.WriteString(writer, webSearchSSE)
			return
		}
		if chats.Add(1) == 1 {
			_, _ = io.WriteString(writer, functionCalls(
				[2]string{"web_search", `{"queries":["\ufeff"]}`},
				[2]string{"web_search", fmt.Sprintf(`{"queries":[%q,%q]}`, query, query)},
				[2]string{"glob", `{"pattern":"\ufeff"}`},
				[2]string{"glob", `{"pattern":"*","path":"\ufeff"}`},
				[2]string{"grep", `{"pattern":"x","path":"\ufeff"}`},
				[2]string{"grep", `{"pattern":"x","include":"\ufeff"}`},
				[2]string{"grep", `{"pattern":" "}`},
			))
		} else {
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\n")
		}
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	t.Cleanup(server.Close)
	config := todoConfig(t, server.URL, t.TempDir(), t.TempDir())
	if err := os.WriteFile(filepath.Join(config.workspaceRoot, "spaces.txt"), []byte("a b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(config.settingsPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(file, "web:\n  search:\n    provider: openai\n    model: test-model\n")
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("configure search: %v %v", err, closeErr)
	}
	assembled, err := composeTUI(config, dependencies{httpClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = assembled.runtime.Shutdown(context.Background()) })
	if err := assembled.runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	controller, err := assembled.root.Agent()
	if err != nil {
		t.Fatal(err)
	}
	results, err := controller.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.Err != nil || result.Outcome != session.OutcomeCompleted {
		t.Fatalf("turn: %+v", result)
	}
	if err := assembled.runtime.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if searches.Load() != 1 {
		t.Fatalf("blank or duplicate query contacted provider: searches=%d", searches.Load())
	}
	select {
	case got := <-queries:
		if got != "Perform a web search for the query: "+query {
			t.Fatalf("query original spelling changed: %q", got)
		}
	default:
		t.Fatal("search provider received no query text")
	}
	var outputs []session.ToolResult
	for _, event := range readTranscript(t, filepath.Join(config.sessionRoot, config.sessionID+".jsonl")) {
		if event.Record.Type == session.RecordToolResult {
			outputs = append(outputs, *event.Record.Result)
		}
	}
	if len(outputs) != 7 || !outputs[0].IsError || outputs[0].Output != "Error: each query must be a non-empty string" || outputs[1].IsError || !strings.Contains(outputs[1].Output, "Go 1.27 shipped.") || outputs[6].IsError {
		t.Fatalf("query/regex results: %+v", outputs)
	}
	for _, output := range outputs[2:6] {
		if !output.IsError || !strings.Contains(output.Output, "must be a non-empty") {
			t.Fatalf("blank search argument reached execution: %+v", output)
		}
	}
}

func TestComposition_ReadsHistoricalSpillsAfterRootChange(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	var step atomic.Int32
	var locator atomic.Value
	var errorLocator atomic.Value
	longPattern := strings.Repeat("x", 60000) + "("
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		body, _ := io.ReadAll(request.Body)
		if strings.Contains(string(body), "Summarize the supplied conversation prefix") {
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"history summarized\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{}}\n\n")
			return
		}
		switch step.Add(1) {
		case 1:
			_, _ = io.WriteString(writer, functionCalls(
				[2]string{"grep", `{"pattern":"needle","path":"many.txt"}`},
				[2]string{"grep", fmt.Sprintf(`{"pattern":%q}`, longPattern)},
			))
		case 3, 5:
			path := locator.Load().(string)
			_, _ = io.WriteString(writer, functionCalls(
				[2]string{"read", fmt.Sprintf(`{"file_path":%q,"limit":1}`, path)},
				[2]string{"grep", fmt.Sprintf(`{"pattern":"Line 300:","path":%q}`, path)},
				[2]string{"read", fmt.Sprintf(`{"file_path":%q,"limit":1}`, errorLocator.Load().(string))},
			))
		case 4:
			_, _ = io.WriteString(writer, functionCalls([2]string{"subagent_fork", `{"description":"historical spill","prompt":"read historical results","run_in_background":false}`}))
		default:
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\n")
		}
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{}}\n\n")
	}))
	t.Cleanup(server.Close)
	root, data := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "many.txt"), []byte(strings.Repeat("needle\n", 300)), 0o600); err != nil {
		t.Fatal(err)
	}
	config := todoConfig(t, server.URL, root, data)
	settings, err := os.ReadFile(config.settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	settings = []byte(strings.ReplaceAll(string(settings), "context_window: 8192", "context_window: 200000") + "compaction:\n  threshold_ratio: 0.8\n  retain_ratio: 0\n  max_tokens: 8192\n  retries: 1\n")
	if err := os.WriteFile(config.settingsPath, settings, 0o600); err != nil { //nolint:gosec // settings path is fixed by this test's temporary config fixture
		t.Fatal(err)
	}
	config.spillRoot = filepath.Join(data, "old spill root")
	path := filepath.Join(config.sessionRoot, config.sessionID+".jsonl")
	run := func(config applicationConfig) {
		t.Helper()
		app, err := composeApplication(config, dependencies{httpClient: server.Client()})
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := plugin.New(app.plugins...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
		if err := runtime.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		controller, err := app.root.Agent()
		if err != nil {
			t.Fatal(err)
		}
		results, err := controller.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "go"}}})
		if err != nil {
			t.Fatal(err)
		}
		if result := <-results; result.Err != nil || result.Outcome != session.OutcomeCompleted {
			t.Fatalf("turn: %+v", result)
		}
		if config.create {
			if err := controller.WhenIdle(t.Context()); err != nil {
				t.Fatal(err)
			}
			if compacted, err := controller.Compact(t.Context()); err != nil || !compacted {
				t.Fatalf("compact initial results: %v %v", compacted, err)
			}
			surface, err := controller.Surface(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, node := range surface {
				if node.Result != nil {
					t.Fatal("compaction retained a result instead of shadowing its locator")
				}
			}
		}
		if err := runtime.Shutdown(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	run(config)
	for _, event := range readTranscript(t, path) {
		if event.Record.Type == session.RecordToolResult {
			if event.Record.Result.IsError {
				_, rest, ok := strings.Cut(event.Record.Result.Output, "Full formatted result stored at: ")
				if !ok || !strings.HasPrefix(event.Record.Result.Output, "Error: grep pattern rejected by ripgrep:") {
					t.Fatal("long regex error lost status/envelope or was not spilled")
				}
				value, _, _ := strings.Cut(rest, ". Use read")
				errorLocator.Store(value)
				content, err := os.ReadFile(value) //nolint:gosec // locator comes from this test's real store
				if err != nil || !strings.Contains(string(content), longPattern) || len(content) < 60000 {
					t.Fatalf("complete regex diagnostic lost: bytes=%d err=%v", len(content), err)
				}
			}
			_, rest, ok := strings.Cut(event.Record.Result.Output, "Full grep result stored at: ")
			if ok {
				value, _, _ := strings.Cut(rest, ". Use read")
				locator.Store(value)
			}
		}
	}
	if locator.Load() == nil || errorLocator.Load() == nil {
		t.Fatal("initial grep did not record an artifact")
	}
	if content, err := os.ReadFile(locator.Load().(string)); err != nil || !strings.HasSuffix(string(content), "Line 300: needle") {
		t.Fatalf("complete artifact: %v", err)
	}
	config.create, config.spillRoot = false, filepath.Join(data, "new-spill")
	run(config)
	var outputs []string
	for _, event := range readTranscript(t, path) {
		if event.Record.Type == session.RecordToolResult && event.Record.Turn == 2 {
			if event.Record.Result.IsError {
				t.Errorf("historical read denied after successful resume: %s", event.Record.Result.Output)
			}
			outputs = append(outputs, event.Record.Result.Output)
		}
	}
	if len(outputs) != 4 || !strings.Contains(outputs[0], "1: Found 300 matches") || !strings.Contains(outputs[1], "Line 300: needle") || !strings.Contains(outputs[2], "1: Error: grep pattern rejected") {
		t.Fatalf("historical read/grep results: %q", outputs)
	}
	files, err := filepath.Glob(filepath.Join(config.sessionRoot, "*.jsonl"))
	if err != nil || len(files) != 2 {
		t.Fatalf("fork transcripts: %v %v", files, err)
	}
	for _, child := range files {
		if child == path {
			continue
		}
		var childOutputs []string
		for _, event := range session.OwnEvents(readTranscript(t, child)) {
			if event.Record.Type == session.RecordToolResult {
				if event.Record.Result.IsError {
					t.Errorf("fork historical read denied: %s", event.Record.Result.Output)
				}
				childOutputs = append(childOutputs, event.Record.Result.Output)
			}
		}
		if len(childOutputs) != 3 || !strings.Contains(childOutputs[0], "1: Found 300 matches") || !strings.Contains(childOutputs[1], "Line 300: needle") || !strings.Contains(childOutputs[2], "1: Error: grep pattern rejected") {
			t.Fatalf("fork historical results: %q", childOutputs)
		}
	}
}
