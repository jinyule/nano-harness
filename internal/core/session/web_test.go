package session

import (
	"errors"
	"strings"
	"testing"
)

func searchRecord() Record {
	return Record{Type: RecordWebSearchRequest, Turn: 1, Step: 1, Search: &WebSearchRequest{
		CallID: "call", Index: 1, Provider: "openai", Model: "model", Endpoint: "openai-responses", Query: " go ", TimeoutMS: 60000, MaxResults: 8,
	}}
}

func TestRecord_WebSearchRequestShapeAndBudgets(t *testing.T) {
	for _, endpoint := range []string{"openai-responses", "codex-responses", "anthropic-messages", "openrouter-chat-completions"} {
		record := searchRecord()
		record.Search.Endpoint = endpoint
		if endpoint == "anthropic-messages" {
			record.Search.Provider, record.Search.MaxUses, record.Search.MaxTokens = "anthropic", 5, 4096
		}
		if endpoint == "openrouter-chat-completions" {
			record.Search.Provider = "openrouter"
		}
		if err := record.Validate(); err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
	}
	for name, mutate := range map[string]func(*Record){
		"missing payload":    func(r *Record) { r.Search = nil },
		"missing step":       func(r *Record) { r.Step = 0 },
		"extra field":        func(r *Record) { r.Plan = &PlanMode{} },
		"missing call":       func(r *Record) { r.Search.CallID = "" },
		"missing model":      func(r *Record) { r.Search.Model = "" },
		"unknown effort":     func(r *Record) { r.Search.Effort = "huge" },
		"zero index":         func(r *Record) { r.Search.Index = 0 },
		"excess index":       func(r *Record) { r.Search.Index = 5 },
		"zero timeout":       func(r *Record) { r.Search.TimeoutMS = 0 },
		"excess timeout":     func(r *Record) { r.Search.TimeoutMS = 60001 },
		"zero results":       func(r *Record) { r.Search.MaxResults = 0 },
		"excess results":     func(r *Record) { r.Search.MaxResults = 9 },
		"blank query":        func(r *Record) { r.Search.Query = " \t" },
		"large query":        func(r *Record) { r.Search.Query = strings.Repeat("x", MaxArgumentsBytes+1) },
		"bad UTF8":           func(r *Record) { r.Search.Query = string([]byte{255}) },
		"NUL query":          func(r *Record) { r.Search.Query = "x\x00" },
		"URL endpoint":       func(r *Record) { r.Search.Endpoint = "https://secret.example" },
		"responses provider": func(r *Record) { r.Search.Provider = "anthropic" },
		"responses budget":   func(r *Record) { r.Search.MaxUses = 1 },
		"messages budget":    func(r *Record) { r.Search.Endpoint, r.Search.Provider = "anthropic-messages", "anthropic" },
		"router provider":    func(r *Record) { r.Search.Endpoint = "openrouter-chat-completions" },
		"router budget": func(r *Record) {
			r.Search.Endpoint, r.Search.Provider, r.Search.MaxTokens = "openrouter-chat-completions", "openrouter", 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			record := searchRecord()
			mutate(&record)
			if err := record.Validate(); !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("invalid search accepted: %v", err)
			}
		})
	}
	bare := Record{Type: RecordTurnStart, Turn: 1, Search: searchRecord().Search}
	if err := bare.Validate(); !errors.Is(err, ErrInvalidRecord) {
		t.Fatal("search payload accepted on bare record")
	}
}

func TestCloneEvent_WebSearchIntentIsLogOnlyAndDetached(t *testing.T) {
	event := Event{Sequence: 1, Record: searchRecord()}
	cloned := CloneEvent(event)
	cloned.Record.Search.Query = "changed"
	if event.Record.Search.Query != " go " {
		t.Fatal("cloned audit aliases its source")
	}
	if nodes, err := Surface([]Event{event}); err != nil || len(nodes) != 0 {
		t.Fatalf("audit entered surface: %v %v", nodes, err)
	}
}

func TestRecord_WebSearchQuerySharesArgumentBudget(t *testing.T) {
	for _, size := range []int{(128 << 10) + 1, MaxArgumentsBytes} {
		record := searchRecord()
		record.Search.Query = strings.Repeat("q", size)
		if err := record.Validate(); err != nil {
			t.Fatalf("query of %d bytes rejected: %v", size, err)
		}
	}
}

func TestRecord_WebSearchQueryUsesECMAScriptBlankSet(t *testing.T) {
	for _, test := range []struct {
		name, query string
		valid       bool
	}{
		{"NEL", "\u0085", true},
		{"NEL surrounded by BOM", "\ufeff\u0085\ufeff", true},
		{"BOM", "\ufeff", false},
		{"all ECMAScript whitespace", "\t\v\f \u00a0\ufeff\n\r\u2028\u2029\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u202f\u205f\u3000", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := searchRecord()
			record.Search.Query = test.query
			if err := record.Validate(); (err == nil) != test.valid {
				t.Fatalf("query=%q valid=%v error=%v", test.query, test.valid, err)
			}
		})
	}
}
