package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	// maxSearchSources bounds the unique sources kept from one provider response.
	maxSearchSources = 64
	// maxSearchURLBytes rejects source URLs no consumer could render or fetch.
	maxSearchURLBytes = 4096
	// anthropicSearchMaxTokens and anthropicSearchMaxUses match the reference
	// Messages search request: one bounded answer and at most five server searches.
	anthropicSearchMaxTokens = 4096
	anthropicSearchMaxUses   = 5
	searchInstructions       = "Search the web for the user's query. Answer briefly from the results and cite every source you use."
)

func searchPrompt(query string) string { return "Perform a web search for the query: " + query }

// Search runs one server-side web search through the provider's own model API.
func (prepared *prepared) Search(ctx context.Context, credential llm.Credential, request llm.SearchRequest) (llm.SearchResult, error) {
	switch prepared.owner.id {
	case "openai":
		return prepared.owner.searchResponses(ctx, prepared.snapshot, prepared.info, credential, request)
	case "anthropic":
		return prepared.owner.searchAnthropic(ctx, prepared.snapshot, prepared.info, credential, request)
	case "openrouter":
		return prepared.owner.searchOpenRouter(ctx, prepared.snapshot, prepared.info, credential, request)
	default:
		return llm.SearchResult{}, llm.ErrUnknownProvider
	}
}

// sourceList keeps unique, bounded sources in provider order.
type sourceList struct {
	sources []llm.SearchSource
	seen    map[string]int
}

func (list *sourceList) add(source llm.SearchSource) {
	if source.URL == "" || len(source.URL) > maxSearchURLBytes || len(list.sources) >= maxSearchSources {
		return
	}
	if list.seen == nil {
		list.seen = map[string]int{}
	}
	if index, ok := list.seen[source.URL]; ok {
		existing := &list.sources[index]
		existing.Title = firstNonEmpty(existing.Title, source.Title)
		existing.Snippet = firstNonEmpty(existing.Snippet, source.Snippet)
		existing.PublishedAt = firstNonEmpty(existing.PublishedAt, source.PublishedAt)
		return
	}
	list.seen[source.URL] = len(list.sources)
	list.sources = append(list.sources, source)
}

func firstNonEmpty(current, candidate string) string {
	if current != "" {
		return current
	}
	return candidate
}

type responsesSearchTool struct {
	Type string `json:"type"`
}

type responsesSearchRequest struct {
	Model        string                `json:"model"`
	Instructions string                `json:"instructions"`
	Input        []responsesInput      `json:"input"`
	Tools        []responsesSearchTool `json:"tools"`
	ToolChoice   string                `json:"tool_choice"`
	Reasoning    *responsesReasoning   `json:"reasoning,omitempty"`
	Stream       bool                  `json:"stream"`
	Store        bool                  `json:"store"`
}

type responsesSearchEvent struct {
	Type string `json:"type"`
	Item struct {
		Type    string `json:"type"`
		Content []struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Annotations []struct {
				Type  string `json:"type"`
				URL   string `json:"url"`
				Title string `json:"title"`
			} `json:"annotations"`
		} `json:"content"`
	} `json:"item"`
}

func (provider *Provider) searchResponses(ctx context.Context, current *snapshot, model llm.ModelInfo, credential llm.Credential, request llm.SearchRequest) (llm.SearchResult, error) {
	endpoint, headers, err := provider.responsesTarget(current, credential)
	if err != nil {
		return llm.SearchResult{}, err
	}
	payload := responsesSearchRequest{
		Model: model.ID, Instructions: searchInstructions,
		Input:      []responsesInput{{Role: "user", Content: []responsesContent{{Type: "input_text", Text: searchPrompt(request.Query)}}}},
		Tools:      []responsesSearchTool{{Type: "web_search"}},
		ToolChoice: "auto", Stream: true, Store: false,
	}
	if model.Effort != "" {
		payload.Reasoning = &responsesReasoning{Effort: model.Effort}
	}
	category := "openai-responses"
	if credential.Kind == llm.CredentialOAuth {
		category = "codex-responses"
	}
	if err := provider.recordSearch(ctx, model, request, category, 0, 0); err != nil {
		return llm.SearchResult{}, err
	}
	return send(ctx, provider, endpoint, "text/event-stream", payload, headers, provider.consumeResponsesSearch)
}

// consumeResponsesSearch reads complete output items. A response without a
// web_search_call is an empty search, not an answer from model memory.
func (provider *Provider) consumeResponsesSearch(body io.Reader) (llm.SearchResult, error) {
	var (
		text      strings.Builder
		sources   sourceList
		searched  bool
		completed bool
	)
	err := scanSSE(body, provider.id, func(data []byte) error {
		var event responsesSearchEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
		}
		switch event.Type {
		case "response.output_item.done":
			switch event.Item.Type {
			case "web_search_call":
				searched = true
			case "message":
				for _, content := range event.Item.Content {
					if content.Type != "output_text" {
						continue
					}
					if err := appendParagraph(&text, content.Text, provider.id); err != nil {
						return err
					}
					for _, annotation := range content.Annotations {
						if annotation.Type == "url_citation" {
							sources.add(llm.SearchSource{URL: annotation.URL, Title: annotation.Title})
						}
					}
				}
			}
		case "response.completed":
			completed = true
		case "response.failed", "response.incomplete", "error":
			return &llm.Error{Code: llm.ErrorInvalid, Provider: provider.id, Cause: fmt.Errorf("provider event %s", event.Type)}
		}
		return nil
	})
	if err != nil {
		return llm.SearchResult{}, err
	}
	if !completed {
		return llm.SearchResult{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("stream ended before response.completed")}
	}
	if !searched {
		return llm.SearchResult{}, &llm.Error{Code: llm.ErrorEmptyResponse, Provider: provider.id, Cause: errors.New("response contained no web_search_call")}
	}
	return llm.SearchResult{Content: text.String(), Sources: sources.sources}, nil
}

func appendParagraph(builder *strings.Builder, value, providerID string) error {
	if value == "" {
		return nil
	}
	separator := ""
	if builder.Len() > 0 {
		separator = "\n\n"
	}
	return appendBounded(builder, separator+value, session.MaxTextBytes, "search answer", providerID)
}

type anthropicSearchTool struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	MaxUses int    `json:"max_uses"`
}

type anthropicSearchRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens"`
	Messages     []anthropicMessage     `json:"messages"`
	Tools        []anthropicSearchTool  `json:"tools"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
}

type anthropicSearchResponse struct {
	Content []struct {
		Type      string `json:"type"`
		Text      string `json:"text"`
		Citations []struct {
			URL       string `json:"url"`
			CitedText string `json:"cited_text"`
		} `json:"citations"`
		Content json.RawMessage `json:"content"`
	} `json:"content"`
}

type anthropicSearchHit struct {
	Type    string `json:"type"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	PageAge string `json:"page_age"`
}

type anthropicSearchFailure struct {
	Type      string `json:"type"`
	ErrorCode string `json:"error_code"`
}

func (provider *Provider) searchAnthropic(ctx context.Context, current *snapshot, model llm.ModelInfo, credential llm.Credential, request llm.SearchRequest) (llm.SearchResult, error) {
	headers, err := anthropicHeaders(credential)
	if err != nil {
		return llm.SearchResult{}, err
	}
	payload := anthropicSearchRequest{
		Model: model.ID, MaxTokens: anthropicSearchMaxTokens,
		Messages: []anthropicMessage{{Role: "user", Content: []anthropicBlock{{Type: "text", Text: searchPrompt(request.Query)}}}},
		Tools:    []anthropicSearchTool{{Type: "web_search_20250305", Name: "web_search", MaxUses: anthropicSearchMaxUses}},
	}
	if model.Effort != "" {
		payload.OutputConfig = &anthropicOutputConfig{Effort: model.Effort}
	}
	if err := provider.recordSearch(ctx, model, request, "anthropic-messages", payload.Tools[0].MaxUses, payload.MaxTokens); err != nil {
		return llm.SearchResult{}, err
	}
	return send(ctx, provider, current.baseURL+"/v1/messages", "application/json", payload, headers, provider.consumeAnthropicSearch)
}

// consumeAnthropicSearch maps web_search_tool_result items to sources and joins
// each URL with its first citation excerpt. Without a result block the model did
// not search, so the response is rejected rather than scraped for prose.
func (provider *Provider) consumeAnthropicSearch(body io.Reader) (llm.SearchResult, error) {
	var response anthropicSearchResponse
	if err := decodeJSON(body, provider.id, &response); err != nil {
		return llm.SearchResult{}, err
	}
	var (
		text      strings.Builder
		snippets  = map[string]string{}
		hits      []anthropicSearchHit
		blocks    int
		failure   string
		succeeded bool
	)
	for _, block := range response.Content {
		switch block.Type {
		case "text":
			if err := appendBounded(&text, block.Text, session.MaxTextBytes, "search answer", provider.id); err != nil {
				return llm.SearchResult{}, err
			}
			for _, citation := range block.Citations {
				if _, ok := snippets[citation.URL]; !ok && citation.CitedText != "" {
					snippets[citation.URL] = citation.CitedText
				}
			}
		case "web_search_tool_result":
			blocks++
			if bytes.HasPrefix(bytes.TrimSpace(block.Content), []byte("[")) {
				var items []anthropicSearchHit
				if err := json.Unmarshal(block.Content, &items); err != nil {
					return llm.SearchResult{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
				}
				hits, succeeded = append(hits, items...), true
				continue
			}
			var result anthropicSearchFailure
			if err := json.Unmarshal(block.Content, &result); err != nil || result.Type != "web_search_tool_result_error" {
				return llm.SearchResult{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("web search result has an invalid shape")}
			}
			failure = firstNonEmpty(failure, result.ErrorCode)
		}
	}
	if blocks == 0 {
		return llm.SearchResult{}, &llm.Error{Code: llm.ErrorEmptyResponse, Provider: provider.id, Cause: errors.New("response contained no web_search_tool_result")}
	}
	if !succeeded {
		return llm.SearchResult{}, anthropicSearchError(provider.id, failure)
	}
	var sources sourceList
	for _, hit := range hits {
		if hit.Type == "web_search_result" {
			sources.add(llm.SearchSource{URL: hit.URL, Title: hit.Title, Snippet: snippets[hit.URL], PublishedAt: hit.PageAge})
		}
	}
	return llm.SearchResult{Content: text.String(), Sources: sources.sources}, nil
}

// anthropicSearchError maps the server tool's documented error codes to stable classes.
func anthropicSearchError(providerID, code string) error {
	class := llm.ErrorInvalid
	switch code {
	case "too_many_requests":
		class = llm.ErrorRateLimit
	case "unavailable":
		class = llm.ErrorServer
	}
	return &llm.Error{Code: class, Provider: providerID, Cause: fmt.Errorf("web search tool error %q", code)}
}

type openRouterSearchTool struct {
	Type       string `json:"type"`
	Parameters struct {
		MaxResults int `json:"max_results"`
	} `json:"parameters"`
}

type openRouterSearchRequest struct {
	Model           string                 `json:"model"`
	Messages        []chatMessage          `json:"messages"`
	Tools           []openRouterSearchTool `json:"tools"`
	ReasoningEffort session.Effort         `json:"reasoning_effort,omitempty"`
	Stream          bool                   `json:"stream"`
}

type openRouterSearchResponse struct {
	Choices []struct {
		Message struct {
			Content     string `json:"content"`
			Annotations []struct {
				Type        string `json:"type"`
				URLCitation struct {
					URL     string `json:"url"`
					Title   string `json:"title"`
					Content string `json:"content"`
				} `json:"url_citation"`
			} `json:"annotations"`
		} `json:"message"`
	} `json:"choices"`
	Error json.RawMessage `json:"error"`
}

func (provider *Provider) searchOpenRouter(ctx context.Context, current *snapshot, model llm.ModelInfo, credential llm.Credential, request llm.SearchRequest) (llm.SearchResult, error) {
	if credential.Kind != llm.CredentialAPIKey {
		return llm.SearchResult{}, &llm.Error{Code: llm.ErrorUnauthorized, Provider: provider.id, Cause: errors.New("OpenRouter requires an API key or exchanged OAuth key")}
	}
	tool := openRouterSearchTool{Type: "openrouter:web_search"}
	tool.Parameters.MaxResults = request.MaxResults
	payload := openRouterSearchRequest{
		Model: model.ID, Messages: []chatMessage{{Role: "user", Content: searchPrompt(request.Query)}},
		Tools: []openRouterSearchTool{tool}, ReasoningEffort: model.Effort, Stream: false,
	}
	headers := map[string]string{"Authorization": "Bearer " + credential.APIKey}
	if err := provider.recordSearch(ctx, model, request, "openrouter-chat-completions", 0, 0); err != nil {
		return llm.SearchResult{}, err
	}
	return send(ctx, provider, current.baseURL+"/chat/completions", "application/json", payload, headers, provider.consumeOpenRouterSearch)
}

func (provider *Provider) consumeOpenRouterSearch(body io.Reader) (llm.SearchResult, error) {
	var response openRouterSearchResponse
	if err := decodeJSON(body, provider.id, &response); err != nil {
		return llm.SearchResult{}, err
	}
	if len(response.Error) != 0 && string(response.Error) != "null" {
		return llm.SearchResult{}, &llm.Error{Code: llm.ErrorInvalid, Provider: provider.id, Cause: errors.New("provider returned an error object")}
	}
	if len(response.Choices) == 0 {
		return llm.SearchResult{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("response contained no choices")}
	}
	message := response.Choices[0].Message
	if len(message.Content) > session.MaxTextBytes {
		return llm.SearchResult{}, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errors.New("search answer exceeds size limit")}
	}
	var sources sourceList
	for _, annotation := range message.Annotations {
		if annotation.Type == "url_citation" {
			citation := annotation.URLCitation
			sources.add(llm.SearchSource{URL: citation.URL, Title: citation.Title, Snippet: citation.Content})
		}
	}
	return llm.SearchResult{Content: message.Content, Sources: sources.sources}, nil
}

// recordSearch excludes transport addresses, headers, and credentials. A committed
// intent is not proof of dispatch: cancellation can still win after the append.
func (provider *Provider) recordSearch(ctx context.Context, model llm.ModelInfo, request llm.SearchRequest, endpoint string, maxUses, maxTokens int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.RecordRequest == nil {
		return llm.ErrSearchAudit
	}
	if err := request.RecordRequest(ctx, session.WebSearchRequest{
		Provider: provider.id, Model: model.ID, Effort: model.Effort,
		Endpoint: endpoint, Query: request.Query, TimeoutMS: request.TimeoutMS,
		MaxResults: request.MaxResults, MaxUses: maxUses, MaxTokens: maxTokens,
	}); err != nil {
		return fmt.Errorf("%w: %w", llm.ErrSearchAudit, err)
	}
	return ctx.Err()
}
