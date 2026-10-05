package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/llm"
	appsettings "github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	openAISearchSSE = `data: {"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"in_progress"}}

data: {"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"golang release"}}}

data: {"type":"response.output_text.delta","output_index":1,"delta":"Go 1.27 shipped."}

data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Go 1.27 shipped.","annotations":[{"type":"url_citation","start_index":0,"end_index":16,"url":"https://go.dev/doc/go1.27","title":"Go 1.27 Release Notes"},{"type":"url_citation","url":"https://go.dev/doc/go1.27","title":"duplicate"},{"type":"file_citation","url":"https://ignored.example"},{"type":"url_citation","url":"https://go.dev/blog"}]},{"type":"refusal","refusal":"ignored"},{"type":"output_text","text":""}]}}

data: {"type":"response.output_item.done","output_index":2,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"See the notes."}]}}

data: {"type":"response.completed","response":{"status":"completed"}}

`
	anthropicSearchJSON = `{"id":"msg","type":"message","role":"assistant","content":[
{"type":"text","text":"I will search."},
{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"golang release"}},
{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[
 {"type":"web_search_result","url":"https://go.dev/doc/go1.27","title":"Go 1.27 Release Notes","encrypted_content":"x","page_age":"August 12, 2026"},
 {"type":"web_search_result","url":"https://go.dev/blog","title":"Go Blog","encrypted_content":"y"},
 {"type":"unknown_result","url":"https://ignored.example"},
 {"type":"web_search_result","url":"https://go.dev/doc/go1.27","title":"duplicate"}]},
{"type":"web_search_tool_result","tool_use_id":"srvtoolu_2","content":{"type":"web_search_tool_result_error","error_code":"max_uses_exceeded"}},
{"type":"text","text":" Go 1.27 shipped.","citations":[{"type":"web_search_result_location","url":"https://go.dev/doc/go1.27","title":"Go 1.27","encrypted_index":"z","cited_text":"Go 1.27 is released."},{"type":"web_search_result_location","url":"https://go.dev/doc/go1.27","cited_text":"second excerpt"},{"type":"web_search_result_location","url":"https://go.dev/blog","cited_text":""}]}
],"stop_reason":"end_turn"}`
	openRouterSearchJSON = `{"id":"gen","choices":[{"message":{"role":"assistant","content":"Go 1.27 shipped.","annotations":[{"type":"url_citation","url_citation":{"url":"https://go.dev/doc/go1.27","title":"Go 1.27 Release Notes","content":"Go 1.27 is released.","start_index":0,"end_index":5}},{"type":"file","file":{}},{"type":"url_citation","url_citation":{"url":""}}]},"finish_reason":"stop"}],"error":null}`
)

type recordedSearch struct {
	path    string
	headers http.Header
	body    map[string]any
}

// searchServer answers every provider search wire with a fixed fixture and records requests.
func searchServer(t *testing.T) (*httptest.Server, func() []recordedSearch) {
	t.Helper()
	var (
		mu       sync.Mutex
		requests []recordedSearch
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "invalid JSON", http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, recordedSearch{path: request.URL.Path, headers: request.Header.Clone(), body: body})
		mu.Unlock()
		switch request.URL.Path {
		case "/v1/responses", "/backend-api/codex/responses":
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, openAISearchSSE)
		case "/v1/messages":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, anthropicSearchJSON)
		case "/chat/completions":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, openRouterSearchJSON)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	return server, func() []recordedSearch {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedSearch(nil), requests...)
	}
}

// startSearchProviders starts all three real providers against one loopback endpoint.
func startSearchProviders(t *testing.T, endpoint string, client *http.Client) map[string]*Provider {
	t.Helper()
	configuration := appsettings.New()
	settingsScope := &plugin.Scope{}
	if err := configuration.Start(context.Background(), settingsScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = settingsScope.Close(context.Background()) })
	runtime, _ := llm.New(&providerStore{})
	runtimeScope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtimeScope.Close(context.Background()) })
	document := appsettings.Defaults()
	for id, configured := range document.Providers {
		configured.BaseURL = endpoint
		configured.Models = []appsettings.Model{{ID: "search-model", Name: "Search", Effort: session.EffortLow, ContextWindow: 8192, Tools: true}}
		document.Providers[id] = configured
	}
	document.Route = appsettings.Route{Provider: "openai", Model: "search-model"}
	mountScope := &plugin.Scope{}
	if err := configuration.Mount(context.Background(), &providerSettings{document: document}, mountScope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mountScope.Close(context.Background()) })
	providers := map[string]*Provider{}
	for _, id := range []string{"openai", "anthropic", "openrouter"} {
		candidate, err := New(runtime, configuration, Config{ID: id, HTTPClient: client, ChatGPTBaseURL: endpoint, CodexHome: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		scope := &plugin.Scope{}
		if err := candidate.Start(context.Background(), scope); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = scope.Close(context.Background()) })
		providers[id] = candidate
	}
	return providers
}

func searchWith(t *testing.T, provider *Provider, credential llm.Credential, request llm.SearchRequest) (llm.SearchResult, error) {
	t.Helper()
	prepared, err := provider.Prepare("search-model")
	if err != nil {
		t.Fatal(err)
	}
	return prepared.Search(context.Background(), credential, request)
}

func jsonValue(t *testing.T, encoded string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(encoded), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestSearch_WireRequestsAndNormalizedResults(t *testing.T) {
	server, recorded := searchServer(t)
	providers := startSearchProviders(t, server.URL, server.Client())
	apiKey := llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}
	oauth := llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "access", AccountID: "account"}
	request := llm.SearchRequest{Query: "golang release", MaxResults: 8}
	responsesBody := `{"model":"search-model","instructions":"` + searchInstructions + `","input":[{"role":"user","content":[{"type":"input_text","text":"Perform a web search for the query: golang release"}]}],"tools":[{"type":"web_search"}],"tool_choice":"auto","reasoning":{"effort":"low"},"stream":true,"store":false}`
	messagesBody := `{"model":"search-model","max_tokens":4096,"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: golang release"}]}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5}],"output_config":{"effort":"low"}}`
	chatBody := `{"model":"search-model","messages":[{"role":"user","content":"Perform a web search for the query: golang release"}],"tools":[{"type":"openrouter:web_search","parameters":{"max_results":8}}],"reasoning_effort":"low","stream":false}`
	release := llm.SearchSource{URL: "https://go.dev/doc/go1.27", Title: "Go 1.27 Release Notes"}
	cases := []struct {
		name       string
		provider   string
		credential llm.Credential
		path       string
		body       string
		accept     string
		headers    map[string]string
		want       llm.SearchResult
	}{
		{
			name: "openai api key", provider: "openai", credential: apiKey, path: "/v1/responses", body: responsesBody, accept: "text/event-stream",
			headers: map[string]string{"Authorization": "Bearer key"},
			want:    llm.SearchResult{Content: "Go 1.27 shipped.\n\nSee the notes.", Sources: []llm.SearchSource{release, {URL: "https://go.dev/blog"}}},
		},
		{
			name: "openai codex oauth", provider: "openai", credential: oauth, path: "/backend-api/codex/responses", body: responsesBody, accept: "text/event-stream",
			headers: map[string]string{"Authorization": "Bearer access", "ChatGPT-Account-Id": "account", "Originator": "codex_cli_rs"},
			want:    llm.SearchResult{Content: "Go 1.27 shipped.\n\nSee the notes.", Sources: []llm.SearchSource{release, {URL: "https://go.dev/blog"}}},
		},
		{
			name: "anthropic api key", provider: "anthropic", credential: apiKey, path: "/v1/messages", body: messagesBody, accept: "application/json",
			headers: map[string]string{"x-api-key": "key", "anthropic-version": "2023-06-01"},
			want: llm.SearchResult{Content: "I will search. Go 1.27 shipped.", Sources: []llm.SearchSource{
				{URL: release.URL, Title: release.Title, Snippet: "Go 1.27 is released.", PublishedAt: "August 12, 2026"},
				{URL: "https://go.dev/blog", Title: "Go Blog"},
			}},
		},
		{
			name: "anthropic oauth", provider: "anthropic", credential: oauth, path: "/v1/messages", body: messagesBody, accept: "application/json",
			headers: map[string]string{"Authorization": "Bearer access", "anthropic-beta": "oauth-2025-04-20"},
			want: llm.SearchResult{Content: "I will search. Go 1.27 shipped.", Sources: []llm.SearchSource{
				{URL: release.URL, Title: release.Title, Snippet: "Go 1.27 is released.", PublishedAt: "August 12, 2026"},
				{URL: "https://go.dev/blog", Title: "Go Blog"},
			}},
		},
		{
			name: "openrouter api key", provider: "openrouter", credential: apiKey, path: "/chat/completions", body: chatBody, accept: "application/json",
			headers: map[string]string{"Authorization": "Bearer key"},
			want:    llm.SearchResult{Content: "Go 1.27 shipped.", Sources: []llm.SearchSource{{URL: release.URL, Title: release.Title, Snippet: "Go 1.27 is released."}}},
		},
	}
	for index, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			result, err := searchWith(t, providers[test.provider], test.credential, request)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result, test.want) {
				t.Fatalf("result=%#v\nwant=%#v", result, test.want)
			}
			requests := recorded()
			if len(requests) != index+1 {
				t.Fatalf("requests=%d", len(requests))
			}
			got := requests[index]
			if got.path != test.path || got.headers.Get("Accept") != test.accept || got.headers.Get("Content-Type") != "application/json" {
				t.Fatalf("path=%s headers=%v", got.path, got.headers)
			}
			for name, value := range test.headers {
				if got.headers.Get(name) != value {
					t.Fatalf("header %s=%q", name, got.headers.Get(name))
				}
			}
			if !reflect.DeepEqual(got.body, jsonValue(t, test.body)) {
				encoded, _ := json.Marshal(got.body)
				t.Fatalf("body=%s\nwant=%s", encoded, test.body)
			}
		})
	}

	// Unset effort is omitted from every search wire, matching chat requests.
	model := llm.ModelInfo{Provider: "openai", ID: "plain"}
	if _, err := providers["openai"].searchResponses(context.Background(), &snapshot{baseURL: server.URL}, model, apiKey, request); err != nil {
		t.Fatal(err)
	}
	if _, err := providers["anthropic"].searchAnthropic(context.Background(), &snapshot{baseURL: server.URL}, model, apiKey, request); err != nil {
		t.Fatal(err)
	}
	if _, err := providers["openrouter"].searchOpenRouter(context.Background(), &snapshot{baseURL: server.URL}, model, apiKey, request); err != nil {
		t.Fatal(err)
	}
	for _, request := range recorded()[len(cases):] {
		for _, field := range []string{"reasoning", "output_config", "reasoning_effort"} {
			if _, ok := request.body[field]; ok {
				t.Fatalf("%s sent unset effort field %s", request.path, field)
			}
		}
	}
}

func TestSearch_RejectsCredentialsBeforeNetwork(t *testing.T) {
	var contacted atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		contacted.Add(1)
		return nil, errors.New("unexpected request")
	})}
	providers := startSearchProviders(t, "http://localhost", client)
	request := llm.SearchRequest{Query: "go", MaxResults: 1}
	if _, err := searchWith(t, providers["openai"], llm.Credential{}, request); !errors.Is(err, llm.ErrNoCredential) {
		t.Fatalf("openai missing credential=%v", err)
	}
	_, err := searchWith(t, providers["openai"], llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "access"}, request)
	expectLLMError(t, err, llm.ErrorUnauthorized)
	if _, err := searchWith(t, providers["anthropic"], llm.Credential{}, request); !errors.Is(err, llm.ErrNoCredential) {
		t.Fatalf("anthropic missing credential=%v", err)
	}
	_, err = searchWith(t, providers["openrouter"], llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "access"}, request)
	expectLLMError(t, err, llm.ErrorUnauthorized)
	unknown := &prepared{owner: &Provider{id: "unknown"}}
	if _, err := unknown.Search(context.Background(), llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}, request); !errors.Is(err, llm.ErrUnknownProvider) {
		t.Fatalf("unknown provider=%v", err)
	}
	if contacted.Load() != 0 {
		t.Fatalf("credential failures contacted the network %d times", contacted.Load())
	}
}

func TestSearch_RefusesRedirectsWithoutContactingTarget(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/redirected", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	providers := startSearchProviders(t, origin.URL, origin.Client())
	credential := llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}
	for _, id := range []string{"openai", "anthropic", "openrouter"} {
		_, err := searchWith(t, providers[id], credential, llm.SearchRequest{Query: "go", MaxResults: 1})
		expectLLMError(t, err, llm.ErrorProtocol)
		if !errors.Is(err, errProviderRedirect) {
			t.Fatalf("%s redirect error=%v", id, err)
		}
		prepared, _ := providers[id].Prepare("search-model")
		_, err = prepared.Stream(context.Background(), credential, llm.Request{System: "s"}, func(session.AssistantChunk) error { return nil })
		if !errors.Is(err, errProviderRedirect) {
			t.Fatalf("%s chat redirect error=%v", id, err)
		}
	}
	if targetHits.Load() != 0 {
		t.Fatalf("redirect target received %d credential-bearing requests", targetHits.Load())
	}
}

func TestSearch_HTTPStatusTransportAndCancellation(t *testing.T) {
	status := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Retry-After", "2")
		http.Error(writer, `{"error":"secret remote detail"}`, http.StatusTooManyRequests)
	}))
	t.Cleanup(status.Close)
	providers := startSearchProviders(t, status.URL, status.Client())
	credential := llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}
	for _, id := range []string{"openai", "anthropic", "openrouter"} {
		_, err := searchWith(t, providers[id], credential, llm.SearchRequest{Query: "go", MaxResults: 1})
		var failure *llm.Error
		if !errors.As(err, &failure) || failure.Code != llm.ErrorRateLimit || failure.HTTPStatus != http.StatusTooManyRequests || failure.RetryAfterMS != 2000 {
			t.Fatalf("%s status error=%#v", id, err)
		}
		if strings.Contains(err.Error(), "secret remote detail") {
			t.Fatalf("%s leaked the remote body: %v", id, err)
		}
	}

	transport := startSearchProviders(t, "http://localhost", &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})})
	_, err := searchWith(t, transport["anthropic"], credential, llm.SearchRequest{Query: "go", MaxResults: 1})
	expectLLMError(t, err, llm.ErrorTransport)

	started := make(chan struct{})
	blocking := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		// Consuming the body lets the server observe the client's disconnect.
		_, _ = io.Copy(io.Discard, request.Body)
		close(started)
		<-request.Context().Done()
	}))
	t.Cleanup(blocking.Close)
	cancelled := startSearchProviders(t, blocking.URL, blocking.Client())
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	prepared, _ := cancelled["openrouter"].Prepare("search-model")
	if _, err := prepared.Search(ctx, credential, llm.SearchRequest{Query: "go", MaxResults: 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled search=%v", err)
	}
}

func TestSearch_ConsumersRejectMalformedAndIncompleteResponses(t *testing.T) {
	provider := &Provider{id: "p"}
	oversizedText := strings.Repeat("x", session.MaxTextBytes+1)
	openAI := []struct {
		name string
		body string
		code llm.ErrorCode
	}{
		{"malformed event", "data: {\n\n", llm.ErrorProtocol},
		{"missing completion", sse(`{"type":"response.output_item.done","item":{"type":"web_search_call"}}`), llm.ErrorProtocol},
		{"no search call", sse(`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"memory"}]}}`, `{"type":"response.completed"}`), llm.ErrorEmptyResponse},
		{"failed", sse(`{"type":"response.failed"}`), llm.ErrorInvalid},
		{"incomplete", sse(`{"type":"response.incomplete"}`), llm.ErrorInvalid},
		{"error", sse(`{"type":"error"}`), llm.ErrorInvalid},
		{"oversized answer", sse(fmt.Sprintf(`{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":%q}]}}`, oversizedText)), llm.ErrorProtocol},
		{"oversized stream", strings.Repeat("data: {}\n", maxProviderResponseBytes/9+1), llm.ErrorProtocol},
	}
	for _, test := range openAI {
		_, err := provider.consumeResponsesSearch(strings.NewReader(test.body))
		expectLLMError(t, err, test.code)
	}
	empty, err := provider.consumeResponsesSearch(strings.NewReader(sse(`{"type":"response.output_item.done","item":{"type":"web_search_call"}}`, `{"type":"response.completed"}`)))
	if err != nil || empty.Content != "" || len(empty.Sources) != 0 {
		t.Fatalf("empty search=%#v err=%v", empty, err)
	}

	anthropic := []struct {
		name string
		body string
		code llm.ErrorCode
	}{
		{"malformed JSON", `{`, llm.ErrorProtocol},
		{"oversized body", `"` + strings.Repeat("x", maxProviderResponseBytes) + `"`, llm.ErrorProtocol},
		{"no result block", `{"content":[{"type":"text","text":"memory"}]}`, llm.ErrorEmptyResponse},
		{"rate limited", `{"content":[{"type":"web_search_tool_result","content":{"type":"web_search_tool_result_error","error_code":"too_many_requests"}}]}`, llm.ErrorRateLimit},
		{"unavailable", `{"content":[{"type":"web_search_tool_result","content":{"type":"web_search_tool_result_error","error_code":"unavailable"}}]}`, llm.ErrorServer},
		{"query too long", `{"content":[{"type":"web_search_tool_result","content":{"type":"web_search_tool_result_error","error_code":"query_too_long"}}]}`, llm.ErrorInvalid},
		{"invalid error shape", `{"content":[{"type":"web_search_tool_result","content":{"type":"other"}}]}`, llm.ErrorProtocol},
		{"null result", `{"content":[{"type":"web_search_tool_result","content":null}]}`, llm.ErrorProtocol},
		{"invalid results", `{"content":[{"type":"web_search_tool_result","content":[1]}]}`, llm.ErrorProtocol},
		{"oversized answer", fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, oversizedText), llm.ErrorProtocol},
	}
	for _, test := range anthropic {
		_, err := provider.consumeAnthropicSearch(strings.NewReader(test.body))
		expectLLMError(t, err, test.code)
	}

	openRouter := []struct {
		name string
		body string
		code llm.ErrorCode
	}{
		{"malformed JSON", `[`, llm.ErrorProtocol},
		{"error object", `{"error":{"message":"remote"},"choices":[]}`, llm.ErrorInvalid},
		{"no choices", `{"choices":[]}`, llm.ErrorProtocol},
		{"oversized answer", fmt.Sprintf(`{"choices":[{"message":{"content":%q}}]}`, oversizedText), llm.ErrorProtocol},
	}
	for _, test := range openRouter {
		_, err := provider.consumeOpenRouterSearch(strings.NewReader(test.body))
		expectLLMError(t, err, test.code)
	}
	noSources, err := provider.consumeOpenRouterSearch(strings.NewReader(`{"choices":[{"message":{"content":null}}]}`))
	if err != nil || noSources.Content != "" || noSources.Sources != nil {
		t.Fatalf("empty OpenRouter search=%#v err=%v", noSources, err)
	}
	failing := errorReadCloser{err: errors.New("read")}
	_, err = provider.consumeOpenRouterSearch(failing)
	expectLLMError(t, err, llm.ErrorProtocol)
}

func TestSearch_SourceListDeduplicatesAndBounds(t *testing.T) {
	var list sourceList
	list.add(llm.SearchSource{URL: ""})
	list.add(llm.SearchSource{URL: "https://" + strings.Repeat("x", maxSearchURLBytes)})
	list.add(llm.SearchSource{URL: "https://a.example"})
	list.add(llm.SearchSource{URL: "https://a.example", Title: "A", Snippet: "first", PublishedAt: "today"})
	list.add(llm.SearchSource{URL: "https://a.example", Title: "B", Snippet: "second", PublishedAt: "yesterday"})
	if want := []llm.SearchSource{{URL: "https://a.example", Title: "A", Snippet: "first", PublishedAt: "today"}}; !reflect.DeepEqual(list.sources, want) {
		t.Fatalf("sources=%#v", list.sources)
	}
	for index := range maxSearchSources + 4 {
		list.add(llm.SearchSource{URL: fmt.Sprintf("https://%d.example", index)})
	}
	if len(list.sources) != maxSearchSources || list.sources[maxSearchSources-1].URL != fmt.Sprintf("https://%d.example", maxSearchSources-2) {
		t.Fatalf("bounded sources=%d last=%s", len(list.sources), list.sources[len(list.sources)-1].URL)
	}
}

// OAuth requests carry codes, verifiers, refresh tokens, and key exchanges in
// their bodies; a 307/308 would resend that body to the redirect target.
func TestOAuth_RefusesRedirectsWithoutContactingTarget(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetHits.Add(1) }))
	t.Cleanup(target.Close)
	var originHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		originHits.Add(1)
		http.Redirect(writer, request, target.URL+"/stolen", http.StatusPermanentRedirect)
	}))
	t.Cleanup(origin.Close)
	provider := &Provider{id: "openai", client: origin.Client(), auth: authConfig{openAIAuthURL: origin.URL, anthropicExchangeURL: origin.URL}}
	_, err := provider.refreshOpenAI(context.Background(), llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "old", RefreshToken: "refresh-secret"})
	expectLLMError(t, err, llm.ErrorProtocol)
	if !errors.Is(err, errProviderRedirect) {
		t.Fatalf("form redirect error=%v", err)
	}
	provider.id = "anthropic"
	_, err = provider.refreshAnthropic(context.Background(), llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "old", RefreshToken: "refresh-secret"})
	if !errors.Is(err, errProviderRedirect) {
		t.Fatalf("JSON redirect error=%v", err)
	}
	if originHits.Load() != 2 || targetHits.Load() != 0 {
		t.Fatalf("origin hits=%d target hits=%d", originHits.Load(), targetHits.Load())
	}
}
