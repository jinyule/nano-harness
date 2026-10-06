package main

import (
	"bytes"
	"compress/gzip"
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
	mu               sync.Mutex
	searches         []map[string]any
	chats            int
	expectedSearches int
	allStarted       chan struct{}
	auditPath        string
	t                *testing.T
	chatRequests     []map[string]any
	queries          []string
}

func (server *webModelServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var body map[string]any
	if json.NewDecoder(request.Body).Decode(&body) != nil {
		http.Error(writer, "bad request", http.StatusBadRequest)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	tools, _ := body["tools"].([]any)
	if request.URL.Path != "/v1/responses" || len(tools) == 1 && tools[0].(map[string]any)["type"] == "web_search" {
		server.mu.Lock()
		server.searches = append(server.searches, body)
		if len(server.searches) == server.expectedSearches {
			close(server.allStarted)
		}
		server.mu.Unlock()
		// Read the actual disk bytes while the HTTP request is in flight. A later
		// append may have an incomplete final line, which is not this request's audit.
		disk, err := os.ReadFile(server.auditPath)
		if err != nil {
			server.t.Error(err)
		}
		encoded, _ := json.Marshal(body)
		found := false
		for line := range bytes.SplitSeq(disk, []byte{'\n'}) {
			var event session.Event
			if json.Unmarshal(line, &event) == nil && event.Record.Search != nil {
				prompt, _ := json.Marshal("Perform a web search for the query: " + event.Record.Search.Query)
				found = found || bytes.Contains(encoded, prompt)
			}
		}
		if !found {
			server.t.Error("HTTP search arrived before its audit was durable")
		}
		select {
		case <-server.allStarted:
		case <-request.Context().Done():
			return
		}
		switch request.URL.Path {
		case "/v1/messages":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"content":[{"type":"web_search_tool_result","content":[]}]}`)
		case "/chat/completions":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"content":"searched"}}]}`)
		default:
			_, _ = io.WriteString(writer, webSearchSSE)
		}
		return
	}
	server.mu.Lock()
	server.chats++
	server.chatRequests = append(server.chatRequests, body)
	first := server.chats == 1
	server.mu.Unlock()
	if first {
		for index, call := range []struct{ id, name, arguments string }{
			{"call-search", "web_search", server.queryArguments()},
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
func runWebTurn(t *testing.T, searchSettings string, queries ...string) ([]session.Record, *webModelServer, int32) {
	t.Helper()
	return runWebTurnWithPage(t, searchSettings, nil, queries...)
}

func runWebTurnWithPage(t *testing.T, searchSettings string, page http.HandlerFunc, queries ...string) ([]session.Record, *webModelServer, int32) {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key")
	if len(queries) == 0 {
		queries = []string{"go release"}
	}
	models := &webModelServer{expectedSearches: len(queries), allStarted: make(chan struct{}), t: t}
	modelServer := httptest.NewServer(models)
	t.Cleanup(modelServer.Close)
	var pageHits atomic.Int32
	pages := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		pageHits.Add(1)
		if request.Host != "docs.example.test" || request.Header.Get("Authorization") != "" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		if page != nil {
			page(writer, request)
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
	models.auditPath = filepath.Join(data, "sessions", "session-web.jsonl")
	models.queries = queries
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 8192\n        vision: false\n        tools: true\n%s", modelServer.URL, searchSettings)
	for _, id := range []string{"anthropic", "openrouter"} {
		settingsYAML = strings.Replace(settingsYAML, "providers:\n", fmt.Sprintf("providers:\n  %s:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        tools: true\n        context_window: 8192\n", id, modelServer.URL), 1)
	}
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-web", maxSteps: 4,
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

func TestComposition_WebSearchAuditsNELQuery(t *testing.T) {
	records, models, _ := runWebTurn(t, "web:\n  search:\n    provider: openai\n    model: test-model\n", "\u0085")
	if result := toolResults(records)["call-search"]; result.IsError || len(models.searches) != 1 {
		t.Fatalf("nonblank NEL query blocked: result=%+v searches=%d", result, len(models.searches))
	}
	var audits []session.WebSearchRequest
	for _, record := range records {
		if record.Search != nil {
			audits = append(audits, *record.Search)
		}
	}
	if len(audits) != 1 || audits[0].Query != "\u0085" {
		t.Fatalf("NEL query audit=%+v", audits)
	}
}

func TestComposition_WebSearchAndFetchEndToEnd(t *testing.T) {
	records, models, pageHits := runWebTurn(t, "web:\n  search:\n    provider: openai\n    model: test-model\n")
	auditCount := 0
	for _, record := range records {
		if record.Type == "web/search-request" {
			auditCount++
		}
	}
	if auditCount != 1 {
		t.Fatalf("disk transcript has %d search request audits, want 1", auditCount)
	}
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

func TestComposition_WebFetchRejectsEncodedNetworkBomb(t *testing.T) {
	var empty bytes.Buffer
	member := gzip.NewWriter(&empty)
	if err := member.Close(); err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat(empty.Bytes(), 5_000_000/empty.Len()+1)
	var tail bytes.Buffer
	member = gzip.NewWriter(&tail)
	if _, err := member.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if err := member.Close(); err != nil {
		t.Fatal(err)
	}
	data = append(data, tail.Bytes()...)
	records, models, hits := runWebTurnWithPage(t, "", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		writer.Header().Set("Content-Encoding", "gzip")
		writer.(http.Flusher).Flush()
		_, _ = writer.Write(data)
	})
	const expected = "Error: WEB_FETCH_TOO_LARGE: response exceeds the maximum of 5000000 bytes"
	result := toolResults(records)["call-fetch"]
	if !result.IsError || result.Output != expected || hits != 1 {
		t.Fatalf("encoded network bomb: result=%+v HTTP requests=%d", result, hits)
	}
	if len(models.chatRequests) != 2 {
		t.Fatalf("model requests=%d", len(models.chatRequests))
	}
	input, err := json.Marshal(models.chatRequests[1]["input"])
	if err != nil || !bytes.Contains(input, []byte(expected)) {
		t.Fatalf("next model request lacks durable fetch failure: %s err=%v", input, err)
	}
}

func (server *webModelServer) queryArguments() string {
	encoded, _ := json.Marshal(map[string]any{"queries": server.queries})
	return string(encoded)
}

func TestComposition_WebSearchAuditsConcurrentQueriesOnDisk(t *testing.T) {
	for _, id := range []string{"openai", "anthropic", "openrouter"} {
		for count := 1; count <= 4; count++ {
			t.Run(fmt.Sprintf("%s/%d", id, count), func(t *testing.T) {
				queries := []string{"go", "rust", "swift", "zig"}[:count]
				records, models, _ := runWebTurn(t, fmt.Sprintf("web:\n  search:\n    provider: %s\n    model: test-model\n", id), queries...)
				var audits []session.WebSearchRequest
				pending := false
				for _, record := range records {
					if record.Call != nil && record.Call.ID == "call-search" {
						pending = true
					}
					if record.Search != nil {
						if !pending || record.Turn != 1 || record.Step != 1 || record.Search.CallID != "call-search" {
							t.Fatalf("audit causality=%+v", record)
						}
						audits = append(audits, *record.Search)
					}
					if record.Result != nil && record.Result.CallID == "call-search" {
						pending = false
					}
				}
				if len(audits) != count || len(models.searches) != count || toolResults(records)["call-search"].IsError {
					t.Fatalf("audits=%+v searches=%d result=%+v", audits, len(models.searches), toolResults(records)["call-search"])
				}
				for i, audit := range audits {
					if audit.Index != i+1 || audit.Query != queries[i] || audit.Provider != id || audit.Model != "test-model" || audit.TimeoutMS != 60000 || audit.MaxResults != 8 {
						t.Fatalf("audit=%+v", audit)
					}
				}
				replayed, _ := json.Marshal(models.chatRequests[1]["input"])
				if bytes.Contains(replayed, []byte("timeout_ms")) || bytes.Contains(replayed, []byte("web/search-request")) || bytes.Contains(replayed, []byte(audits[0].Endpoint)) {
					t.Fatal("audit leaked into model surface")
				}
			})
		}
	}
}

// The assembled web tools persist WebError classifications and both
// metadata kinds, while the next chat request carries only the result text.
func TestComposition_PersistsWebStructuredResults(t *testing.T) {
	records, models, _ := runWebTurn(t, "web:\n  search:\n    provider: openai\n    model: test-model\n")
	results := toolResults(records)
	search, fetch := results["call-search"], results["call-fetch"]
	wantSearch := session.WebSearchMeta{Sources: []session.WebSource{{URL: "https://go.dev/doc/go1.27", Title: "Go 1.27 Release Notes"}}, Answer: "Go 1.27 shipped."}
	if search.IsError || search.Meta == nil || search.Meta.WebSearch == nil || fmt.Sprint(*search.Meta.WebSearch) != fmt.Sprint(wantSearch) {
		t.Fatalf("search metadata = %+v", search.Meta)
	}
	if fetch.IsError || fetch.Meta == nil || fetch.Meta.WebFetch == nil || *fetch.Meta.WebFetch != (session.WebFetchMeta{URL: "http://docs.example.test/guide", StatusCode: 200}) {
		t.Fatalf("fetch metadata = %+v", fetch.Meta)
	}
	models.mu.Lock()
	next, err := json.Marshal(models.chatRequests[len(models.chatRequests)-1]["input"])
	models.mu.Unlock()
	if err != nil || !strings.Contains(string(next), "Go 1.27 shipped.") || strings.Contains(string(next), "status_code") || strings.Contains(string(next), `\"web_search\":{`) || strings.Contains(string(next), "WebError") {
		t.Fatalf("next model input = %s", next)
	}

	records, _, _ = runWebTurn(t, "")
	failed := toolResults(records)["call-search"]
	if !failed.IsError || !strings.HasPrefix(failed.Output, "Error: WEB_PROVIDER_UNAVAILABLE: ") || failed.Error == nil || *failed.Error != (session.ToolError{Name: "WebError", Code: "WEB_PROVIDER_UNAVAILABLE"}) || failed.Meta != nil {
		t.Fatalf("unconfigured search = %+v", failed)
	}
}
