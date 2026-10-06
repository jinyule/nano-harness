package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// startPruneComposition runs the real shared composition against server on
// a model whose 8000-token window puts one long read result above the
// compaction threshold while its pruned form is well below it. Calling it
// again with the same directories resumes the session.
func startPruneComposition(t *testing.T, server *httptest.Server, root, data string) (*plugin.Runtime, agent.Controller) {
	t.Helper()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 8000\n        tools: true\n", server.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-prune", maxSteps: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := composeApplication(config, dependencies{httpClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := plugin.New(app.plugins...)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	controller, err := app.root.Agent()
	if err != nil {
		t.Fatal(err)
	}
	return runtime, controller
}

func submitText(t *testing.T, controller agent.Controller, text string) agent.TurnResult {
	t.Helper()
	results, err := controller.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}})
	if err != nil {
		t.Fatal(err)
	}
	return <-results
}

func shutdownRuntime(t *testing.T, runtime *plugin.Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

// callOutput returns the function_call_output a request carried for callID.
func callOutput(t *testing.T, request seenRequest, callID string) string {
	t.Helper()
	for _, raw := range request.Input {
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Output string `json:"output"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Type == "function_call_output" && item.CallID == callID {
			return item.Output
		}
	}
	t.Fatalf("request carries no output for %s", callID)
	return ""
}

func TestComposition_PrunesLongToolResultsWithoutSummarizing(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	server, seen := scriptedModel(t, []modelStep{
		{tool: "read", arguments: `{"file_path":"big.txt"}`},
		{text: "read done"},
		{tool: "subagent_fork", arguments: `{"description":"check the read","prompt":"FORK_TASK"}`},
		{text: "child done"},
		{text: "fork done"},
	})
	root, data := t.TempDir(), t.TempDir()
	var lines strings.Builder
	for index := range 3000 {
		fmt.Fprintf(&lines, "line-%04d %s\n", index, strings.Repeat("abcdefghij", 2))
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(lines.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, controller := startPruneComposition(t, server, root, data)
	if result := submitText(t, controller, "read big.txt"); result.Err != nil || result.Text != "read done" {
		t.Fatalf("first turn = %+v", result)
	}
	shutdownRuntime(t, runtime)
	runtime, controller = startPruneComposition(t, server, root, data)
	if result := submitText(t, controller, "fork a check"); result.Err != nil || result.Text != "fork done" {
		t.Fatalf("resumed turn = %+v", result)
	}
	shutdownRuntime(t, runtime)

	requests := seen()
	if len(requests) != 5 {
		t.Fatalf("model requests = %d; a summary request would add one", len(requests))
	}
	pruned := callOutput(t, requests[1], "call-0")
	if !strings.Contains(pruned, session.PruneMarker) || len([]rune(pruned)) > session.PruneThreshold || !strings.HasPrefix(pruned, "<path>") {
		t.Fatalf("the next request did not carry the pruned result: %d code points", len([]rune(pruned)))
	}
	if resumed := callOutput(t, requests[2], "call-0"); resumed != pruned {
		t.Fatal("the resumed session rebuilt a different surface")
	}
	if forked := callOutput(t, requests[3], "call-0"); forked != pruned {
		t.Fatal("the forked child rebuilt a different surface")
	}

	records := readTranscript(t, filepath.Join(data, "sessions", "session-prune.jsonl"))
	var original string
	var prunes []session.ToolResultPrune
	for _, event := range records {
		record := event.Record
		if record.Type == session.RecordToolResult && record.Result.CallID == "call-0" {
			original = record.Result.Output
		}
		if record.Type == session.RecordCompactionPrune {
			prunes = append(prunes, *record.Prune)
		}
		if record.Type == session.RecordCompactionStart {
			t.Fatal("pruning that relieved pressure still opened a summary")
		}
	}
	if want, ok := session.PruneToolOutput(original); !ok || len(prunes) != 1 || prunes[0].Output != want || want != pruned {
		t.Fatalf("prunes = %d, original %d bytes", len(prunes), len(original))
	}
}

// requestText joins the text of every input item a request carried.
func requestText(t *testing.T, request seenRequest) string {
	t.Helper()
	var text strings.Builder
	for _, raw := range request.Input {
		text.Write(raw)
	}
	return text.String()
}

func TestComposition_TruncatedSummaryNeverReplacesHistory(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	server, seen := scriptedModel(t, []modelStep{
		{text: "noted"},
		{text: "listed"},
		{text: "Do not modify the", truncated: true},
		{text: "still careful"},
	})
	root, data := t.TempDir(), t.TempDir()
	runtime, controller := startPruneComposition(t, server, root, data)
	if result := submitText(t, controller, "Never modify protected.txt; it holds the release keys."); result.Err != nil || result.Text != "noted" {
		t.Fatalf("first turn = %+v", result)
	}
	if result := submitText(t, controller, "list the files"); result.Err != nil || result.Text != "listed" {
		t.Fatalf("second turn = %+v", result)
	}
	if compacted, err := controller.Compact(t.Context()); err == nil || compacted {
		t.Errorf("truncated summary compaction = %t, %v", compacted, err)
	}
	shutdownRuntime(t, runtime)
	runtime, controller = startPruneComposition(t, server, root, data)
	if result := submitText(t, controller, "continue"); result.Err != nil || result.Text != "still careful" {
		t.Fatalf("resumed turn = %+v", result)
	}
	shutdownRuntime(t, runtime)
	requests := seen()
	if len(requests) != 4 {
		t.Fatalf("model requests = %d, want 4 without a summary retry", len(requests))
	}
	if !strings.Contains(requestText(t, requests[3]), "Never modify protected.txt") {
		t.Error("the resumed request lost the protected history")
	}
	var ends []string
	for _, event := range readTranscript(t, filepath.Join(data, "sessions", "session-prune.jsonl")) {
		if event.Record.Type == session.RecordCompactionSummary {
			t.Error("a truncated summary was published")
		}
		if event.Record.Type == session.RecordCompactionEnd {
			ends = append(ends, event.Record.Compaction.Error)
		}
	}
	if len(ends) != 1 || ends[0] != "max_tokens" {
		t.Fatalf("compaction ends = %q", ends)
	}
}
