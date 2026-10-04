package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/core/session"
)

type webResolver map[string]string

func (resolver webResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	if address, ok := resolver[host]; ok {
		return []net.IPAddr{{IP: net.ParseIP(address)}}, nil
	}
	return nil, fmt.Errorf("no such host %s", host)
}

const webSearchSSE = `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","status":"completed"}}

data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","content":[{"type":"output_text","text":"Go 1.27 shipped.","annotations":[{"type":"url_citation","url":"https://go.dev/doc/go1.27","title":"Go 1.27 Release Notes"}]}]}}

data: {"type":"response.completed","response":{}}

`

// webModelServer plays both the chat model and the server-side search model on
// one loopback Responses endpoint, distinguishing them by the request tools.
type webModelServer struct {
	mu       sync.Mutex
	searches []map[string]any
	chats    int
}

func (server *webModelServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var body map[string]any
	if request.URL.Path != "/v1/responses" || json.NewDecoder(request.Body).Decode(&body) != nil {
		http.Error(writer, "bad request", http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	tools, _ := body["tools"].([]any)
	if len(tools) == 1 && tools[0].(map[string]any)["type"] == "web_search" {
		server.mu.Lock()
		server.searches = append(server.searches, body)
		server.mu.Unlock()
		_, _ = io.WriteString(writer, webSearchSSE)
		return
	}
	server.mu.Lock()
	server.chats++
	first := server.chats == 1
	server.mu.Unlock()
	if first {
		for index, call := range []struct{ id, name, arguments string }{
			{"call-search", "web_search", `{"queries":["go release"]}`},
			{"call-fetch", "web_fetch", `{"url":"http://docs.example.test/guide"}`},
		} {
			item, _ := json.Marshal(map[string]any{"type": "function_call", "call_id": call.id, "name": call.name, "arguments": call.arguments})
			_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", index, item)
		}
	} else {
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"done\"}\n\n")
	}
	_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
}

// runWebTurn assembles the real application, submits one task, and returns the durable transcript.
func runWebTurn(t *testing.T, searchSettings string) ([]session.Record, *webModelServer, int32) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key")
	models := &webModelServer{}
	modelServer := httptest.NewServer(models)
	t.Cleanup(modelServer.Close)
	var pageHits atomic.Int32
	pages := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		pageHits.Add(1)
		if request.Host != "docs.example.test" || request.Header.Get("Authorization") != "" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(writer, "<html><body><h1>Guide</h1><script>steal()</script><p>Install <code>nano</code>.</p></body></html>")
	}))
	t.Cleanup(pages.Close)
	var dialed []string
	var dialMu sync.Mutex
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		dialMu.Lock()
		dialed = append(dialed, address)
		dialMu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, pages.Listener.Addr().String())
	}

	root := t.TempDir()
	data := t.TempDir()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 8192\n        vision: false\n        tools: true\n%s", modelServer.URL, searchSettings)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), sessionID: "session-web", maxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := composeTUI(config, dependencies{
		httpClient: modelServer.Client(), webResolver: webResolver{"docs.example.test": "93.184.216.34"}, webDial: dial,
	})
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
	results, err := rootAgent.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "research go"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.Err != nil || result.Outcome != session.OutcomeCompleted || result.Text != "done" {
		t.Fatalf("turn=%#v", result)
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := assembled.runtime.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	dialMu.Lock()
	defer dialMu.Unlock()
	for _, address := range dialed {
		if address != "93.184.216.34:80" {
			t.Fatalf("fetch dialed an unvalidated destination %s", address)
		}
	}
	events := readTranscript(t, filepath.Join(data, "sessions", "session-web.jsonl"))
	records := make([]session.Record, len(events))
	for index, event := range events {
		records[index] = event.Record
	}
	return records, models, pageHits.Load()
}

func toolResults(records []session.Record) map[string]session.ToolResult {
	results := map[string]session.ToolResult{}
	for _, record := range records {
		if record.Type == session.RecordToolResult {
			results[record.Result.CallID] = *record.Result
		}
	}
	return results
}

func TestComposition_WebSearchAndFetchEndToEnd(t *testing.T) {
	records, models, pageHits := runWebTurn(t, "web:\n  search:\n    provider: openai\n    model: test-model\n")
	var header *session.RequestHeader
	for _, record := range records {
		if record.Type == session.RecordRequestHeader {
			header = record.Header
			break
		}
	}
	if header == nil {
		t.Fatal("transcript has no request header")
	}
	schemas := map[string]string{}
	for _, tool := range header.Tools {
		schemas[tool.Name] = string(tool.Parameters)
	}
	if schemas["web_search"] != `{"type":"object","properties":{"queries":{"type":"array","description":"1–4 search queries; their results are merged.","items":{"type":"string"}}},"required":["queries"]}` ||
		schemas["web_fetch"] != `{"type":"object","properties":{"url":{"type":"string","description":"The HTTP(S) URL to fetch."}},"required":["url"]}` {
		t.Fatalf("frozen web schemas=%v", schemas)
	}
	if !strings.Contains(header.System, "Follow up with web_fetch") || !strings.Contains(header.System, "web_fetch returns external, untrusted page content") {
		t.Fatal("request system prompt lacks web guidance")
	}
	results := toolResults(records)
	search, fetch := results["call-search"], results["call-fetch"]
	if search.IsError || !strings.Contains(search.Output, "Go 1.27 shipped.") || !strings.Contains(search.Output, "- [Go 1.27 Release Notes](https://go.dev/doc/go1.27)") {
		t.Fatalf("search result=%#v", search)
	}
	if fetch.IsError || !strings.HasPrefix(fetch.Output, "Fetched http://docs.example.test/guide (HTTP 200)") || !strings.HasSuffix(fetch.Output, "# Guide\n\nInstall `nano`.") || strings.Contains(fetch.Output, "steal") {
		t.Fatalf("fetch result=%#v", fetch)
	}
	if pageHits != 1 {
		t.Fatalf("page hits=%d", pageHits)
	}
	if len(models.searches) != 1 {
		t.Fatalf("search requests=%d", len(models.searches))
	}
	input, _ := json.Marshal(models.searches[0]["input"])
	if !strings.Contains(string(input), "Perform a web search for the query: go release") {
		t.Fatalf("search input=%s", input)
	}
}

func TestComposition_WebSearchUnconfiguredFailsClosed(t *testing.T) {
	records, models, _ := runWebTurn(t, "")
	results := toolResults(records)
	search := results["call-search"]
	if !search.IsError || !strings.HasPrefix(search.Output, "Error: WEB_PROVIDER_UNAVAILABLE: ") {
		t.Fatalf("unconfigured search=%#v", search)
	}
	if fetch := results["call-fetch"]; fetch.IsError {
		t.Fatalf("fetch failed without search configuration: %#v", fetch)
	}
	if len(models.searches) != 0 {
		t.Fatal("unconfigured search reached a provider")
	}
}
