package session

import (
	"strings"
	"unicode/utf8"

	"github.com/jinyule/nano-harness/internal/core/text"
)

// WebSearchRequest is a log-only request intent, committed before dispatch.
// Index orders the distinct queries of one pending web_search call. Endpoint
// is a protocol category, never a URL; credentials and headers are excluded.
type WebSearchRequest struct {
	CallID     string `json:"call_id"`
	Index      int    `json:"index"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Effort     Effort `json:"effort,omitempty"`
	Endpoint   string `json:"endpoint"`
	Query      string `json:"query"`
	TimeoutMS  int64  `json:"timeout_ms"`
	MaxResults int    `json:"max_results"`
	MaxUses    int    `json:"max_uses,omitempty"`
	MaxTokens  int    `json:"max_tokens,omitempty"`
}

func (record Record) requireWebSearch() error {
	if record.Step == 0 || record.Search == nil || record.hasExtras("search") {
		return invalid("web/search-request shape is invalid")
	}
	data := record.Search
	if err := validateIdentifier("call ID", data.CallID, 128); err != nil {
		return err
	}
	if err := validateIdentifier("model", data.Model, 256); err != nil {
		return err
	}
	if data.Effort != "" && !ValidEffort(data.Effort) {
		return invalid("web/search-request effort is invalid")
	}
	if data.Index < 1 || data.Index > 4 || data.TimeoutMS < 1 || data.TimeoutMS > 60000 || data.MaxResults < 1 || data.MaxResults > 8 {
		return invalid("web/search-request budget or index is invalid")
	}
	blank := text.TrimSpace(data.Query) == ""
	if blank || len(data.Query) > MaxArgumentsBytes || !utf8.ValidString(data.Query) || strings.ContainsRune(data.Query, 0) {
		return invalid("web/search-request query is invalid")
	}
	switch data.Endpoint {
	case "openai-responses", "codex-responses":
		if data.Provider != "openai" || data.MaxUses != 0 || data.MaxTokens != 0 {
			return invalid("web/search-request Responses route or budget is invalid")
		}
	case "anthropic-messages":
		if data.Provider != "anthropic" || data.MaxUses != 5 || data.MaxTokens != 4096 {
			return invalid("web/search-request Messages route or budget is invalid")
		}
	case "openrouter-chat-completions":
		if data.Provider != "openrouter" || data.MaxUses != 0 || data.MaxTokens != 0 {
			return invalid("web/search-request Chat Completions route or budget is invalid")
		}
	default:
		return invalid("web/search-request endpoint category is invalid")
	}
	return nil
}
