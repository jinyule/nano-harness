package provider

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	appsettings "github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type providerStore struct{ credential llm.Credential }

type providerSettings struct{ document appsettings.Document }

func (settings *providerSettings) Load(context.Context) (appsettings.Document, error) {
	return settings.document, nil
}
func (settings *providerSettings) Persist(_ context.Context, document appsettings.Document) error {
	settings.document = document
	return nil
}
func (*providerSettings) Watch(ctx context.Context, _ func(appsettings.Document, error)) error {
	<-ctx.Done()
	return nil
}

func (store *providerStore) Resolve(context.Context, string, string) (llm.Credential, error) {
	return store.credential, nil
}
func (store *providerStore) Modify(_ context.Context, _ string, mutate func(*llm.Credential) (*llm.Credential, error)) (llm.Credential, error) {
	next, err := mutate(&store.credential)
	if next != nil {
		store.credential = *next
	}
	return store.credential, err
}
func (*providerStore) Delete(context.Context, string) error            { return nil }
func (*providerStore) List(context.Context) ([]llm.AccountInfo, error) { return nil, nil }

type authInteraction struct {
	secret  string
	notices []llm.AuthNotice
	auto    bool
}

func (interaction *authInteraction) Prompt(context.Context, llm.AuthPrompt) (string, error) {
	return interaction.secret, nil
}
func (interaction *authInteraction) Notify(notice llm.AuthNotice) {
	interaction.notices = append(interaction.notices, notice)
	if !interaction.auto || notice.URL == "" {
		return
	}
	parsed, _ := url.Parse(notice.URL)
	callback, _ := url.QueryUnescape(parsed.Query().Get("redirect_uri"))
	if callback == "" {
		callback, _ = url.QueryUnescape(parsed.Query().Get("callback_url"))
	}
	state := parsed.Query().Get("state")
	if callback != "" {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, callback+"?code=test-code&state="+url.QueryEscape(state), nil)
		if err != nil {
			return
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
		}
	}
}

func jwt(payload map[string]any) string {
	encoded, _ := json.Marshal(payload)
	return "x." + base64.RawURLEncoding.EncodeToString(encoded) + ".x"
}

func imageBlock() session.ContentBlock {
	data := []byte("image")
	digest := sha256.Sum256(data)
	return session.ContentBlock{Type: session.ContentImage, Image: &session.Image{ID: "img", Name: "x.jpg", MediaType: "image/jpeg", Data: base64.StdEncoding.EncodeToString(data), SHA256: hex.EncodeToString(digest[:]), Width: 1, Height: 1}}
}

func providerRequest() llm.Request {
	return llm.Request{System: "system", MaxTokens: 64, Surface: []session.SurfaceNode{
		{Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "hello"}, imageBlock()}}},
		{Message: &session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "provider"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "calling"}}}},
		{Call: &session.ToolCall{ID: "old", Name: "tool", Arguments: json.RawMessage(`{}`)}},
		{Result: &session.ToolResult{CallID: "old", Output: "done"}},
	}, Tools: []session.ToolDefinition{{Name: "tool", Description: "does work", Parameters: json.RawMessage(`{"type":"object"}`)}}}
}

func protocolServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		switch request.URL.Path {
		case "/v1/responses", "/backend-api/codex/responses":
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"think\"}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"c\",\"name\":\"tool\"}}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{}\"}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"c\",\"name\":\"tool\",\"arguments\":\"{}\"}}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"input_tokens_details\":{\"cached_tokens\":1}}}}\n\n")
		case "/v1/messages":
			_, _ = io.WriteString(writer, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":2,\"cache_read_input_tokens\":1}}}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"think\"}}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"c\",\"name\":\"tool\",\"input\":{}}}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"message_stop\"}\n\n")
		case "/chat/completions":
			_, _ = io.WriteString(writer, "data: {\"choices\":[{\"delta\":{\"reasoning\":\"think\",\"content\":\"hello\",\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"function\":{\"name\":\"tool\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n")
		default:
			http.NotFound(writer, request)
		}
	}))
}

func TestProviderProtocolsAndLifecycle(t *testing.T) {
	server := protocolServer(t)
	defer server.Close()
	configuration := appsettings.New()
	settingsScope := &plugin.Scope{}
	if err := configuration.Start(context.Background(), settingsScope); err != nil {
		t.Fatal(err)
	}
	store := &providerStore{credential: llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}}
	runtime, _ := llm.New(store)
	runtimeScope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	document := appsettings.Defaults()
	for id, configured := range document.Providers {
		configured.BaseURL = server.URL
		configured.Models = []appsettings.Model{{ID: "model", Name: "Model", ContextWindow: 8192, Vision: true, Tools: true}}
		document.Providers[id] = configured
	}
	document.Route = appsettings.Route{Provider: "openai", Model: "model"}
	mountScope := &plugin.Scope{}
	if err := configuration.Mount(context.Background(), &providerSettings{document: document}, mountScope); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"openai", "anthropic", "openrouter"} {
		candidate, err := New(runtime, configuration, Config{ID: id, HTTPClient: server.Client(), ChatGPTBaseURL: server.URL, OpenAIAuthURL: server.URL, AnthropicAuthURL: server.URL, OpenRouterAuthURL: server.URL, CodexHome: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		scope := &plugin.Scope{}
		if candidate.ID() != id || candidate.Start(context.Background(), scope) != nil {
			t.Fatal("start provider")
		}
		models := candidate.Models()
		models[0].Name = "changed"
		if candidate.Models()[0].Name == "changed" {
			t.Fatal("models alias")
		}
		prepared, err := candidate.Prepare("model")
		if err != nil || prepared.Info().ID != "model" || prepared.CredentialEnv() == "" {
			t.Fatal(err)
		}
		var chunks []session.AssistantChunk
		completion, err := prepared.Stream(context.Background(), llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}, providerRequest(), func(chunk session.AssistantChunk) error { chunks = append(chunks, chunk); return nil })
		if err != nil || session.Text(completion.Message) != "hello" || len(completion.Calls) != 1 || len(chunks) == 0 {
			t.Fatalf("%s completion=%#v chunks=%#v err=%v", id, completion, chunks, err)
		}
		if id != "openrouter" {
			oauth := llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "access", RefreshToken: "refresh", AccountID: "account"}
			if _, err := prepared.Stream(context.Background(), oauth, providerRequest(), func(session.AssistantChunk) error { return nil }); err != nil {
				t.Fatalf("%s OAuth stream=%v", id, err)
			}
		}
		if _, err := candidate.Prepare("missing"); !errors.Is(err, llm.ErrUnknownModel) {
			t.Fatalf("missing=%v", err)
		}
		if len(candidate.AuthMethods()) < 2 {
			t.Fatal("OAuth method missing")
		}
		interaction := &authInteraction{secret: "secret"}
		credential, err := candidate.Login(context.Background(), "api-key", interaction)
		if err != nil || credential.APIKey != "secret" {
			t.Fatalf("login=%#v err=%v", credential, err)
		}
		if _, err := candidate.Login(context.Background(), "bad", interaction); err == nil {
			t.Fatal("bad login accepted")
		}
		if err := scope.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if candidate.Models() != nil {
			t.Fatal("models after close")
		}
		if _, err := candidate.Prepare("model"); !errors.Is(err, llm.ErrNotRunning) {
			t.Fatalf("prepare closed=%v", err)
		}
	}
	_ = runtimeScope.Close(context.Background())
	_ = mountScope.Close(context.Background())
	_ = settingsScope.Close(context.Background())
}

func TestRequestValidationAndParsers(t *testing.T) {
	provider := &Provider{id: "openai"}
	model := llm.ModelInfo{Provider: "openai", ID: "m", Vision: true, Tools: true}
	request := providerRequest()
	responses, err := provider.responsesRequest(model, request)
	if err != nil || len(responses.Input) != 4 || len(responses.Tools) != 1 {
		t.Fatal(err)
	}
	responsesJSON, _ := json.Marshal(responses)
	if strings.Contains(string(responsesJSON), `"strict"`) {
		t.Fatal("Responses enabled strict schemas for tools with optional properties")
	}
	toolOnly, err := provider.responsesRequest(model, llm.Request{Surface: []session.SurfaceNode{
		{Message: &session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "provider"}}},
		{Call: &session.ToolCall{ID: "call", Name: "tool", Arguments: json.RawMessage(`{}`)}},
		{Result: &session.ToolResult{CallID: "call", Output: "done"}},
	}})
	if err != nil || len(toolOnly.Input) != 2 || toolOnly.Input[0].Type != "function_call" || toolOnly.Input[1].Type != "function_call_output" {
		t.Fatalf("tool-only Responses replay=%#v err=%v", toolOnly.Input, err)
	}
	provider.id = "anthropic"
	anthropic, err := provider.anthropicRequest(model, request)
	if err != nil || len(anthropic.Messages) == 0 || anthropic.MaxTokens != 64 {
		t.Fatal(err)
	}
	request.MaxTokens = 0
	anthropic, _ = provider.anthropicRequest(model, request)
	if anthropic.MaxTokens != 8192 {
		t.Fatal("default max tokens")
	}
	provider.id = "openrouter"
	chat, err := provider.chatRequest(model, request)
	if err != nil || len(chat.Messages) == 0 || !chat.StreamOptions.IncludeUsage {
		t.Fatal(err)
	}
	chatJSON, _ := json.Marshal(chat)
	if strings.Contains(string(chatJSON), `"strict"`) {
		t.Fatal("OpenRouter enabled strict schemas for tools with optional properties")
	}
	callOnly, err := provider.chatRequest(model, llm.Request{Surface: []session.SurfaceNode{{Call: &session.ToolCall{ID: "c", Name: "tool", Arguments: json.RawMessage(`{}`)}}}})
	if err != nil || len(callOnly.Messages) != 1 || len(callOnly.Messages[0].ToolCalls) != 1 {
		t.Fatalf("call-only chat=%#v err=%v", callOnly, err)
	}
	bad := providerRequest()
	bad.Surface[0].Message.Role = session.RoleAssistant
	provider.id = "openai"
	if _, err := provider.responsesRequest(model, bad); err == nil {
		t.Fatal("assistant image accepted")
	}
	provider.id = "anthropic"
	if _, err := provider.anthropicRequest(model, bad); err == nil {
		t.Fatal("assistant image accepted")
	}
	provider.id = "openrouter"
	if _, err := provider.chatRequest(model, bad); err == nil {
		t.Fatal("assistant image accepted")
	}
	if !surfaceHasImage(request.Surface) || surfaceHasImage(nil) {
		t.Fatal("surfaceHasImage")
	}
}

func TestOAuthFlowsImportRefreshAndHelpers(t *testing.T) {
	account := "account"
	expires := time.Now().Add(time.Hour).Unix()
	access := jwt(map[string]any{"https://api.openai.com/auth.chatgpt_account_id": account, "exp": expires})
	var deviceCalls int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/oauth/token", "/v1/oauth/token":
			_ = json.NewEncoder(writer).Encode(map[string]any{"access_token": access, "refresh_token": "refresh", "expires_in": 3600})
		case "/api/accounts/deviceauth/usercode":
			_ = json.NewEncoder(writer).Encode(map[string]any{"device_auth_id": "device", "user_code": "code", "verification_uri": serverURL(request), "interval": 0})
		case "/api/accounts/deviceauth/token":
			deviceCalls++
			if deviceCalls == 1 {
				http.Error(writer, "pending", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(writer).Encode(map[string]string{"authorization_code": "auth", "code_verifier": "verify"})
		case "/api/v1/auth/keys":
			_ = json.NewEncoder(writer).Encode(map[string]string{"key": "router-key"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	authDocument := fmt.Sprintf(`{"OPENAI_API_KEY":null,"auth_mode":"chatgpt","last_refresh":"now","tokens":{"access_token":%q,"refresh_token":"refresh","account_id":"account","id_token":"id"}}`, access)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(authDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := New(&llm.Runtime{}, appsettings.New(), Config{ID: "openai", HTTPClient: server.Client(), ChatGPTBaseURL: server.URL, OpenAIAuthURL: server.URL, AnthropicAuthURL: server.URL, OpenRouterAuthURL: server.URL, CodexHome: home})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := provider.Login(context.Background(), "codex-import", &authInteraction{})
	if err != nil || credential.AccountID != account || credential.ExpiresUnixMS == 0 {
		t.Fatalf("import=%#v err=%v", credential, err)
	}
	interaction := &authInteraction{auto: true}
	credential, err = provider.Login(context.Background(), "oauth-browser", interaction)
	if err != nil || credential.AccountID != account {
		t.Fatalf("browser=%#v err=%v", credential, err)
	}
	originalFloor := devicePollFloor
	devicePollFloor = time.Nanosecond
	defer func() { devicePollFloor = originalFloor }()
	credential, err = provider.Login(context.Background(), "oauth-device", interaction)
	if err != nil || deviceCalls != 2 {
		t.Fatalf("device=%#v calls=%d err=%v", credential, deviceCalls, err)
	}
	prepared := &prepared{owner: provider}
	if credential, err = prepared.Refresh(context.Background(), credential); err != nil || credential.RefreshToken == "" {
		t.Fatal(err)
	}
	provider.id = "anthropic"
	credential, err = provider.Login(context.Background(), "oauth-browser", interaction)
	if err != nil {
		t.Fatalf("anthropic login=%v", err)
	}
	if _, err = prepared.Refresh(context.Background(), credential); err != nil {
		t.Fatalf("anthropic refresh=%v", err)
	}
	provider.id = "openrouter"
	credential, err = provider.Login(context.Background(), "oauth-browser", interaction)
	if err != nil || credential.APIKey != "router-key" {
		t.Fatalf("router=%#v err=%v", credential, err)
	}
	if jwtStringClaim(access, "missing") != "" || jwtNumericClaim(access, "exp") != expires || jwtStringClaim("bad", "x") != "" || jwtNumericClaim("bad", "x") != 0 {
		t.Fatal("JWT helpers")
	}
	if challenge("x") == "" {
		t.Fatal("challenge")
	}
}

func serverURL(request *http.Request) string { return "http://" + request.Host }

func TestWireAndConfigFailureHelpers(t *testing.T) {
	if _, err := New(nil, nil, Config{}); !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("invalid new=%v", err)
	}
	if validateEndpoint("ftp://example.com") == nil || validateEndpoint("http://example.com") == nil || validateEndpoint("http://localhost:1") != nil || validateEndpoint("https://example.com") != nil {
		t.Fatal("validateEndpoint")
	}
	if defaultString("", "fallback") != "fallback" || defaultString("https://x/", "") != "https://x" {
		t.Fatal("defaultString")
	}
	for status, code := range map[int]llm.ErrorCode{401: llm.ErrorUnauthorized, 429: llm.ErrorRateLimit, 500: llm.ErrorServer, 400: llm.ErrorInvalid} {
		var failure *llm.Error
		if !errors.As(statusError("p", status, "1"), &failure) || failure.Code != code {
			t.Fatalf("status %d=%#v", status, failure)
		}
	}
	if !errors.Is(transportError("p", context.Canceled), context.Canceled) {
		t.Fatal("cancel transport")
	}
	var failure *llm.Error
	if !errors.As(transportError("p", context.DeadlineExceeded), &failure) || failure.Code != llm.ErrorTimeout {
		t.Fatal("timeout transport")
	}
	if !errors.As(transportError("p", errors.New("net")), &failure) || failure.Code != llm.ErrorTransport {
		t.Fatal("net transport")
	}
	if !contextWindowFailure([]byte("context_length_exceeded")) || contextWindowFailure([]byte("other")) {
		t.Fatal("context helper")
	}
	if parseRetryAfter("1") <= 0 || parseRetryAfter("bad") != 0 {
		t.Fatal("retry-after")
	}
	var builder strings.Builder
	if appendBounded(&builder, "x", 1, "x", "p") != nil || appendBounded(&builder, "y", 1, "x", "p") == nil {
		t.Fatal("bounded append")
	}
	if session.Text(assistantMessage("x")) != "x" || len(assistantMessage("").Content) != 0 {
		t.Fatal("assistant message")
	}
	if err := validToolCall(session.ToolCall{ID: "c", Name: "t", Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	originalRandom := randomRead
	defer func() { randomRead = originalRandom }()
	randomRead = func([]byte) (int, error) { return 0, errors.New("random") }
	if _, err := randomURLToken(4); err == nil {
		t.Fatal("random error")
	}
}
