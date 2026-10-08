package provider

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestSearch_AuditsActualRequestBeforeDispatch(t *testing.T) {
	server, requests := searchServer(t)
	providers := startSearchProviders(t, server.URL, server.Client())
	for _, test := range []struct {
		id, endpoint string
		credential   llm.Credential
		uses, tokens int
	}{
		{"openai", "openai-responses", llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "secret-key"}, 0, 0},
		{"openai", "codex-responses", llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "secret-access", AccountID: "secret-account"}, 0, 0},
		{"anthropic", "anthropic-messages", llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "secret-key"}, 5, 4096},
		{"anthropic", "anthropic-messages", llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "secret-access"}, 5, 4096},
		{"openrouter", "openrouter-chat-completions", llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "secret-key"}, 0, 0},
	} {
		t.Run(test.endpoint+string(test.credential.Kind), func(t *testing.T) {
			before := len(requests())
			var audits []session.WebSearchRequest
			request := llm.SearchRequest{Query: "  golang release  ", MaxResults: 8, TimeoutMS: 60000, RecordRequest: func(_ context.Context, audit session.WebSearchRequest) error {
				if len(requests()) != before {
					t.Error("request dispatched before audit")
				}
				audits = append(audits, audit)
				return nil
			}}
			if _, err := searchWith(t, providers[test.id], test.credential, request); err != nil {
				t.Fatal(err)
			}
			want := session.WebSearchRequest{Provider: test.id, Model: "search-model", Effort: session.EffortLow, Endpoint: test.endpoint, Query: request.Query, TimeoutMS: 60000, MaxResults: 8, MaxUses: test.uses, MaxTokens: test.tokens}
			if !reflect.DeepEqual(audits, []session.WebSearchRequest{want}) || len(requests()) != before+1 {
				t.Fatalf("audits=%+v requests=%d", audits, len(requests()))
			}
			encoded, _ := json.Marshal(audits)
			for _, secret := range []string{"secret-", server.URL, "Authorization", "cookie", searchInstructions, searchPrompt(request.Query)} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("audit contains forbidden data %q", secret)
				}
			}
		})
	}
}

func TestSearch_AuditFailurePreventsEveryProviderDispatch(t *testing.T) {
	server, requests := searchServer(t)
	providers := startSearchProviders(t, server.URL, server.Client())
	failure := errors.New("journal write failed: private detail")
	for _, id := range []string{"openai", "anthropic", "openrouter"} {
		t.Run(id, func(t *testing.T) {
			prepared, err := providers[id].Prepare("search-model")
			if err != nil {
				t.Fatal(err)
			}
			credential := llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}
			request := llm.SearchRequest{Query: "go", MaxResults: 8, TimeoutMS: 60000}
			if _, err := prepared.Search(t.Context(), credential, request); !errors.Is(err, llm.ErrSearchAudit) {
				t.Fatalf("missing recorder=%v", err)
			}
			request.RecordRequest = func(context.Context, session.WebSearchRequest) error { return failure }
			if _, err := prepared.Search(t.Context(), credential, request); !errors.Is(err, failure) || !errors.Is(err, llm.ErrSearchAudit) {
				t.Fatalf("failed recorder=%v", err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if _, err := prepared.Search(ctx, credential, request); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel before record=%v", err)
			}
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			request.RecordRequest = func(context.Context, session.WebSearchRequest) error { cancel(); return nil }
			if _, err := prepared.Search(ctx, credential, request); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel after record=%v", err)
			}
		})
	}
	if len(requests()) != 0 {
		t.Fatalf("audit failure or cancellation dispatched %d requests", len(requests()))
	}
}
