package web

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/llm"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	appWeb "github.com/jinyule/nano-harness/internal/app/web"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func runWebCall(t *testing.T, runtime *appTool.Runtime, name, arguments string) session.ToolResult {
	t.Helper()
	return runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{
		SessionID: "s", Turn: 1, Step: 1, Journal: acceptingJournal{},
		Calls: []session.ToolCall{{ID: name, Name: name, Arguments: json.RawMessage(arguments)}},
	})[0]
}

// Every web failure code reaches the durable result as WebError with the
// unchanged model text; failures without a code stay unclassified.
func TestProvider_PersistsWebErrorClassifications(t *testing.T) {
	codes := []appWeb.Code{
		appWeb.CodeProviderUnavailable, appWeb.CodeCredentialMissing, appWeb.CodeProviderError, appWeb.CodeRequestRecordFailed,
		appWeb.CodeAborted, appWeb.CodeSearchTimeout, appWeb.CodeInvalidURL, appWeb.CodeBlockedURL,
		appWeb.CodeRedirectBlocked, appWeb.CodeFetchTooLarge, appWeb.CodeFetchTimeout, appWeb.CodeUnsupportedContent,
	}
	for _, code := range codes {
		failure := &appWeb.Error{Code: code, Message: "failed", Cause: errors.New("private cause")}
		runtime := startProvider(t, fakeService{
			search: func(context.Context, []string, appWeb.SearchInvocation) (appWeb.SearchResult, error) {
				return appWeb.SearchResult{}, failure
			},
			fetch: func(context.Context, string) (appWeb.FetchResult, error) { return appWeb.FetchResult{}, failure },
		})
		for _, call := range []struct{ name, arguments string }{{"web_search", `{"queries":["go"]}`}, {"web_fetch", `{"url":"https://example.com"}`}} {
			result := runWebCall(t, runtime, call.name, call.arguments)
			if !result.IsError || result.Output != "Error: "+string(code)+": failed" || result.Error == nil || *result.Error != (session.ToolError{Name: "WebError", Code: string(code)}) || result.Meta != nil {
				t.Errorf("%s %s = %+v", call.name, code, result)
			}
		}
	}
	runtime := startProvider(t, fakeService{
		search: func(context.Context, []string, appWeb.SearchInvocation) (appWeb.SearchResult, error) {
			return appWeb.SearchResult{}, errors.New("web_search accepts at most 4 queries")
		},
		fetch: func(context.Context, string) (appWeb.FetchResult, error) {
			return appWeb.FetchResult{}, appWeb.ErrNotRunning
		},
	})
	for _, call := range []struct{ name, arguments, output string }{
		{"web_search", `{"queries":["go"]}`, "Error: web_search accepts at most 4 queries"},
		{"web_fetch", `{"url":"https://example.com"}`, "Error: web service is not running"},
	} {
		if result := runWebCall(t, runtime, call.name, call.arguments); !result.IsError || result.Output != call.output || result.Error != nil {
			t.Errorf("%s plain failure = %+v", call.name, result)
		}
	}
	unowned := runtime.ExecuteBatch(t.Context(), appTool.BatchRequest{Calls: []session.ToolCall{{ID: "search", Name: "web_search", Arguments: json.RawMessage(`{"queries":["go"]}`)}}})[0]
	if !unowned.IsError || unowned.Error != nil {
		t.Fatalf("search without a journal was classified: %+v", unowned)
	}
}

// Search metadata holds the same deduplicated sources, in the same order, and
// the same answer and truncation as the rendered text.
func TestProvider_PersistsSearchMetadata(t *testing.T) {
	merged := appWeb.SearchResult{
		Content: "Go 1.27 shipped.",
		Sources: []llm.SearchSource{
			{URL: "https://go.dev/doc", Title: "Release Notes", Snippet: "Go 1.27 is out.", PublishedAt: "2026-08-12"},
			{URL: "https://blog.example/post", PublishedAt: "yesterday"},
			{URL: "https://news.example/item", Title: "News"},
		},
		Truncated: true,
	}
	for _, test := range []struct {
		name   string
		result appWeb.SearchResult
		want   session.WebSearchMeta
	}{
		{"merged", merged, session.WebSearchMeta{
			Sources: []session.WebSource{
				{URL: "https://go.dev/doc", Title: "Release Notes", Snippet: "Go 1.27 is out.", PublishedAt: "2026-08-12"},
				{URL: "https://blog.example/post", PublishedAt: "yesterday"},
				{URL: "https://news.example/item", Title: "News"},
			},
			Answer: "Go 1.27 shipped.", Truncated: true,
		}},
		{"empty", appWeb.SearchResult{}, session.WebSearchMeta{Sources: []session.WebSource{}}},
	} {
		runtime := startProvider(t, fakeService{search: func(context.Context, []string, appWeb.SearchInvocation) (appWeb.SearchResult, error) {
			return test.result, nil
		}})
		result := runWebCall(t, runtime, "web_search", `{"queries":["go","golang"]}`)
		if result.IsError || result.Output != formatSearch(test.result) || result.Meta == nil || result.Meta.WebSearch == nil {
			t.Fatalf("%s: %+v", test.name, result)
		}
		got, err := json.Marshal(result.Meta.WebSearch)
		want, _ := json.Marshal(test.want)
		if err != nil || string(got) != string(want) || result.Meta.WebSearch.Sources == nil {
			t.Fatalf("%s meta = %s, want %s", test.name, got, want)
		}
		position := 0
		for _, source := range result.Meta.WebSearch.Sources {
			next := strings.Index(result.Output[position:], "]("+source.URL+")")
			if next < 0 {
				t.Fatalf("%s: source %s is out of the text's order", test.name, source.URL)
			}
			position += next + 1
		}
	}
}

// Fetch metadata records the final URL, the status of a non-2xx success,
// and truncation exactly when the text carries the truncation footer.
func TestProvider_PersistsFetchMetadata(t *testing.T) {
	for _, test := range []struct {
		name      string
		result    appWeb.FetchResult
		truncated bool
	}{
		{"not found is a result", appWeb.FetchResult{URL: "https://example.com/missing", StatusCode: 404, Kind: appWeb.FetchText, Content: "gone"}, false},
		{"provider truncation", appWeb.FetchResult{URL: "https://example.com/", StatusCode: 200, Kind: appWeb.FetchText, Content: "partial", Truncated: true}, true},
		{"source cut", appWeb.FetchResult{URL: "https://example.com/big", StatusCode: 200, Kind: appWeb.FetchText, Content: strings.Repeat("x", maxFetchOutputUnits+1)}, true},
		{"header pushes output over the cap", appWeb.FetchResult{URL: "https://example.com/full", StatusCode: 200, Kind: appWeb.FetchText, Content: strings.Repeat("y", maxFetchOutputUnits)}, true},
		{"redirected", appWeb.FetchResult{URL: "https://example.com/final", StatusCode: 200, Kind: appWeb.FetchHTML, Content: "<p>ok</p>"}, false},
	} {
		runtime := startProvider(t, fakeService{fetch: func(context.Context, string) (appWeb.FetchResult, error) { return test.result, nil }})
		result := runWebCall(t, runtime, "web_fetch", `{"url":"https://example.com/start"}`)
		if result.IsError || result.Meta == nil || result.Meta.WebFetch == nil {
			t.Fatalf("%s: %+v", test.name, result.Output[:min(len(result.Output), 200)])
		}
		want := session.WebFetchMeta{URL: test.result.URL, StatusCode: test.result.StatusCode, Truncated: test.truncated}
		if *result.Meta.WebFetch != want || strings.HasSuffix(result.Output, fetchFooter) != test.truncated {
			t.Errorf("%s: meta=%+v footer=%v, want %+v", test.name, *result.Meta.WebFetch, strings.HasSuffix(result.Output, fetchFooter), want)
		}
		if encoded, _ := json.Marshal(result.Meta); strings.Contains(string(encoded), "partial") || strings.Contains(string(encoded), "gone") {
			t.Errorf("%s: metadata copied the page content: %s", test.name, encoded)
		}
	}
}
