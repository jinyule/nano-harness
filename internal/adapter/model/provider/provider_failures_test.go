package provider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	appsettings "github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type errorReadCloser struct{ err error }

func (reader errorReadCloser) Read([]byte) (int, error) { return 0, reader.err }
func (errorReadCloser) Close() error                    { return nil }

type sourceStub struct {
	document appsettings.Document
	snapshot error
	watch    error
	hook     func()
}

func (source sourceStub) Snapshot() (appsettings.Document, uint64, error) {
	return source.document, 1, source.snapshot
}

func (source sourceStub) Watch(func(appsettings.Document)) (func(), error) {
	if source.hook != nil {
		source.hook()
	}
	return func() {}, source.watch
}

type promptFailure struct{ err error }

func (interaction promptFailure) Prompt(context.Context, llm.AuthPrompt) (string, error) {
	return "", interaction.err
}
func (promptFailure) Notify(llm.AuthNotice) {}

type cancelInteraction struct{ cancel context.CancelFunc }

func (cancelInteraction) Prompt(context.Context, llm.AuthPrompt) (string, error) { return "", nil }
func (interaction cancelInteraction) Notify(llm.AuthNotice)                      { interaction.cancel() }

func sse(events ...string) string {
	var stream strings.Builder
	for _, event := range events {
		stream.WriteString("data: ")
		stream.WriteString(event)
		stream.WriteString("\n\n")
	}
	return stream.String()
}

func expectLLMError(t *testing.T, err error, code llm.ErrorCode) {
	t.Helper()
	var failure *llm.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("error=%#v, want LLM code %s", err, code)
	}
}

func TestConfigAndConstructionFailurePaths(t *testing.T) {
	originalHome, originalAbs := userHomeDir, absPath
	t.Cleanup(func() { userHomeDir, absPath = originalHome, originalAbs })
	t.Setenv("CODEX_HOME", "")
	userHomeDir = func() (string, error) { return "", errors.New("home") }
	if _, err := resolveAuthConfig(Config{}); !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("home error=%v", err)
	}
	userHomeDir = func() (string, error) { return t.TempDir(), nil }
	absPath = func(string) (string, error) { return "", errors.New("absolute") }
	if _, err := resolveAuthConfig(Config{}); !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("absolute error=%v", err)
	}
	absPath = originalAbs
	resolved, err := resolveAuthConfig(Config{AnthropicAuthURL: "http://localhost:1234"})
	if err != nil || resolved.anthropicExchangeURL != "http://localhost:1234" {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	if _, err := resolveAuthConfig(Config{ChatGPTBaseURL: "http://example.com"}); !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("unsafe auth endpoint=%v", err)
	}
	if validateEndpoint("://") == nil || validateEndpoint("https://user@example.com") == nil || validateEndpoint("https://example.com?q=x") == nil || validateEndpoint("https://example.com#x") == nil {
		t.Fatal("invalid endpoint accepted")
	}
	provider, err := New(&llm.Runtime{}, appsettings.New(), Config{ID: "openai", CodexHome: t.TempDir()})
	if err != nil || provider.client.Timeout != defaultRequestTimeout {
		t.Fatalf("default client=%#v err=%v", provider, err)
	}
	if _, err := New(&llm.Runtime{}, appsettings.New(), Config{ID: "openai", ChatGPTBaseURL: "ftp://bad", CodexHome: t.TempDir()}); !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("invalid new=%v", err)
	}
}

func TestProviderStartLoginRefreshAndStreamFailures(t *testing.T) {
	settingsError := errors.New("settings")
	provider := &Provider{id: "openai", settings: sourceStub{snapshot: settingsError}}
	if err := provider.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, settingsError) {
		t.Fatalf("snapshot error=%v", err)
	}

	store := &providerStore{credential: llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}}
	runtime, _ := llm.New(store)
	runtimeScope := &plugin.Scope{}
	if err := runtime.Start(context.Background(), runtimeScope); err != nil {
		t.Fatal(err)
	}
	document := appsettings.Defaults()
	watchError := errors.New("watch")
	provider = &Provider{id: "openai", runtime: runtime, settings: sourceStub{document: document, watch: watchError}}
	provider.auth.codexHome = t.TempDir()
	providerScope := &plugin.Scope{}
	if err := provider.Start(context.Background(), providerScope); !errors.Is(err, watchError) {
		t.Fatalf("watch error=%v", err)
	}
	_ = providerScope.Close(context.Background())

	first := &Provider{id: "openai", runtime: runtime, settings: sourceStub{document: document}}
	firstScope := &plugin.Scope{}
	if err := first.Start(context.Background(), firstScope); err != nil {
		t.Fatal(err)
	}
	duplicate := &Provider{id: "openai", runtime: runtime, settings: sourceStub{document: document}}
	if err := duplicate.Start(context.Background(), &plugin.Scope{}); err == nil {
		t.Fatal("duplicate provider registered")
	}
	_ = firstScope.Close(context.Background())

	closedDuringWatch := &plugin.Scope{}
	provider = &Provider{id: "openai", runtime: runtime, settings: sourceStub{document: document, hook: func() { _ = closedDuringWatch.Close(context.Background()) }}}
	if err := provider.Start(context.Background(), closedDuringWatch); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope=%v", err)
	}

	provider = &Provider{id: "openai"}
	if _, err := provider.Login(context.Background(), "api-key", promptFailure{err: settingsError}); !errors.Is(err, settingsError) {
		t.Fatalf("prompt error=%v", err)
	}
	if _, err := provider.Login(context.Background(), "api-key", &authInteraction{secret: "  "}); !errors.Is(err, llm.ErrInvalidConfig) {
		t.Fatalf("blank API key=%v", err)
	}

	apiPrepared := &prepared{owner: provider}
	apiCredential := llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}
	if refreshed, err := apiPrepared.Refresh(context.Background(), apiCredential); err != nil || refreshed.APIKey != "key" {
		t.Fatalf("API key refresh=%#v err=%v", refreshed, err)
	}
	provider.id = "openrouter"
	oauthCredential := llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "access"}
	if refreshed, err := apiPrepared.Refresh(context.Background(), oauthCredential); err != nil || refreshed.AccessToken != "access" {
		t.Fatalf("OpenRouter refresh=%#v err=%v", refreshed, err)
	}
	provider.id = "unknown"
	if _, err := apiPrepared.Refresh(context.Background(), oauthCredential); !errors.Is(err, llm.ErrUnknownProvider) {
		t.Fatalf("unknown refresh=%v", err)
	}

	request := providerRequest()
	prepared := &prepared{owner: &Provider{id: "openai"}, snapshot: &snapshot{}, info: llm.ModelInfo{Provider: "openai", ID: "m", Vision: true, Tools: true}}
	request.MaxTokens = -1
	completion, err := prepared.Stream(context.Background(), apiCredential, request, func(session.AssistantChunk) error { return nil })
	if completion.Message.Role != "" {
		t.Fatal("invalid stream returned completion")
	}
	expectLLMError(t, err, llm.ErrorInvalid)
	request.MaxTokens = 1
	prepared.info.Vision = false
	_, err = prepared.Stream(context.Background(), apiCredential, request, func(session.AssistantChunk) error { return nil })
	expectLLMError(t, err, llm.ErrorInvalid)
	prepared.info.Vision = true
	prepared.info.Tools = false
	_, err = prepared.Stream(context.Background(), apiCredential, request, func(session.AssistantChunk) error { return nil })
	expectLLMError(t, err, llm.ErrorInvalid)
	prepared.info.Tools = true
	prepared.owner.id = "unknown"
	if _, err := prepared.Stream(context.Background(), apiCredential, request, func(session.AssistantChunk) error { return nil }); !errors.Is(err, llm.ErrUnknownProvider) {
		t.Fatalf("unknown stream=%v", err)
	}
	_ = runtimeScope.Close(context.Background())
}

func TestWireRequestAndSSEFailurePaths(t *testing.T) {
	provider := &Provider{id: "p", client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network")
	})}}
	if _, err := provider.streamRequest(context.Background(), "http://localhost", func() {}, nil, nil); err == nil {
		t.Fatal("marshal error missing")
	}
	if _, err := provider.streamRequest(context.Background(), "http://localhost", strings.Repeat("x", maxProviderRequestBytes), nil, nil); err == nil {
		t.Fatal("request limit missing")
	}
	if _, err := provider.streamRequest(context.Background(), ":", struct{}{}, nil, nil); err == nil {
		t.Fatal("request URL error missing")
	}
	_, err := provider.streamRequest(context.Background(), "http://localhost", struct{}{}, map[string]string{"X-Test": "yes"}, nil)
	expectLLMError(t, err, llm.ErrorTransport)

	provider.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("context window exceeded"))}, nil
	})}
	_, err = provider.streamRequest(context.Background(), "http://localhost", struct{}{}, nil, nil)
	expectLLMError(t, err, llm.ErrorContextWindow)

	sentinel := errors.New("consume")
	provider.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("X-Test") != "yes" || request.Header.Get("Accept") != "text/event-stream" {
			t.Error("headers missing")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("body"))}, nil
	})}
	_, err = provider.streamRequest(context.Background(), "http://localhost", struct{}{}, map[string]string{"X-Test": "yes"}, func(io.Reader) (llm.Completion, error) {
		return llm.Completion{}, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("consume error=%v", err)
	}

	visited := 0
	if err := scanSSE(strings.NewReader("event: ignored\ndata:\ndata: [DONE]\ndata: {}\n"), "p", func([]byte) error { visited++; return nil }); err != nil || visited != 1 {
		t.Fatalf("scan visited=%d err=%v", visited, err)
	}
	if err := scanSSE(strings.NewReader("data: {}\n"), "p", func([]byte) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("visitor error=%v", err)
	}
	if err := scanSSE(strings.NewReader(strings.Repeat("x", maxProviderSSELineBytes+1)), "p", func([]byte) error { return nil }); err == nil {
		t.Fatal("scanner limit missing")
	}
	tooLarge := strings.Repeat("x\n", maxProviderResponseBytes/2+1)
	if err := scanSSE(strings.NewReader(tooLarge), "p", func([]byte) error { return nil }); err == nil {
		t.Fatal("stream limit missing")
	}
	future := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	if parseRetryAfter(future) <= 0 {
		t.Fatal("HTTP-date retry-after not parsed")
	}
}

func TestOAuthTransportAndCallbackFailures(t *testing.T) {
	provider := &Provider{id: "p", client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	})}}
	if _, err := provider.postForm(context.Background(), ":", nil, nil); err == nil {
		t.Fatal("post form URL accepted")
	}
	if _, err := provider.postJSON(context.Background(), "http://localhost", func() {}, nil); err == nil {
		t.Fatal("post JSON marshal accepted")
	}
	if _, err := provider.postJSON(context.Background(), ":", struct{}{}, nil); err == nil {
		t.Fatal("post JSON URL accepted")
	}
	_, err := provider.postForm(context.Background(), "http://localhost", nil, map[string]string{"X-Test": "yes"})
	expectLLMError(t, err, llm.ErrorTimeout)

	provider.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"1"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	_, err = provider.postJSON(context.Background(), "http://localhost", struct{}{}, map[string]string{"X-Test": "yes"})
	expectLLMError(t, err, llm.ErrorRateLimit)

	provider.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: errorReadCloser{err: errors.New("read")}}, nil
	})}
	if _, err := provider.doOAuth(mustRequest(t, "http://localhost")); err == nil {
		t.Fatal("OAuth read error missing")
	}
	provider.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxOAuthResponseBytes+1)))}, nil
	})}
	if _, err := provider.doOAuth(mustRequest(t, "http://localhost")); err == nil {
		t.Fatal("OAuth size error missing")
	}

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	if _, _, _, err := callbackServer(context.Background(), listener.Addr().String(), "/callback"); err == nil {
		t.Fatal("callback bind conflict accepted")
	}
	redirect, results, shutdown, err := callbackServer(context.Background(), "127.0.0.1:0", "/callback")
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, redirect, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("callback POST status=%v err=%v", responseStatus(response), err)
	}
	_ = response.Body.Close()
	for _, query := range []string{"?error=denied", "", "?code=x&state=wrong", "?code=x&state=right"} {
		request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, redirect+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err = http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
	if _, err := awaitCallback(context.Background(), results, "right"); err == nil {
		t.Fatal("callback rejection missing")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, result := range []oauthCallback{{err: errors.New("rejected")}, {code: "x", state: "wrong"}, {code: "x", state: "right"}} {
		channel := make(chan oauthCallback, 1)
		channel <- result
		code, err := awaitCallback(context.Background(), channel, "right")
		if result.state == "right" && result.err == nil {
			if err != nil || code != "x" {
				t.Fatalf("callback code=%q err=%v", code, err)
			}
		} else if err == nil {
			t.Fatal("invalid callback accepted")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := awaitCallback(ctx, make(chan oauthCallback), "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("callback cancellation=%v", err)
	}
}

func mustRequest(t *testing.T, endpoint string) *http.Request {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func responseStatus(response *http.Response) any {
	if response == nil {
		return nil
	}
	return response.StatusCode
}

func TestProtocolConsumersRejectMalformedAndIncompleteStreams(t *testing.T) {
	provider := &Provider{id: "p"}
	emitOK := func(session.AssistantChunk) error { return nil }
	emitFailure := errors.New("emit")
	emitBad := func(session.AssistantChunk) error { return emitFailure }

	for name, run := range map[string]func() error{
		"responses-json": func() error { _, err := provider.consumeResponses(strings.NewReader(sse("{")), emitOK); return err },
		"responses-emit-text": func() error {
			_, err := provider.consumeResponses(strings.NewReader(sse(`{"type":"response.output_text.delta","delta":"x"}`)), emitBad)
			return err
		},
		"responses-emit-reason": func() error {
			_, err := provider.consumeResponses(strings.NewReader(sse(`{"type":"response.reasoning_text.delta","delta":"x"}`)), emitBad)
			return err
		},
		"responses-index-add": func() error {
			_, err := provider.consumeResponses(strings.NewReader(sse(`{"type":"response.output_item.added","output_index":64,"item":{"type":"function_call"}}`)), emitOK)
			return err
		},
		"responses-emit-add": func() error {
			_, err := provider.consumeResponses(strings.NewReader(sse(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"c","name":"t"}}`)), emitBad)
			return err
		},
		"responses-index-delta": func() error {
			_, err := provider.consumeResponses(strings.NewReader(sse(`{"type":"response.function_call_arguments.delta","output_index":-1,"delta":"{}"}`)), emitOK)
			return err
		},
		"responses-emit-delta": func() error {
			_, err := provider.consumeResponses(strings.NewReader(sse(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`)), emitBad)
			return err
		},
		"responses-arguments": func() error {
			_, err := provider.consumeResponses(strings.NewReader(sse(fmt.Sprintf(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":%q}`, strings.Repeat("x", session.MaxArgumentsBytes+1)))), emitOK)
			return err
		},
		"responses-index-done": func() error {
			_, err := provider.consumeResponses(strings.NewReader(sse(`{"type":"response.output_item.done","output_index":64,"item":{"type":"function_call"}}`)), emitOK)
			return err
		},
		"responses-provider-failed": func() error {
			_, err := provider.consumeResponses(strings.NewReader(sse(`{"type":"response.failed"}`)), emitOK)
			return err
		},
		"responses-incomplete": func() error { _, err := provider.consumeResponses(strings.NewReader(""), emitOK); return err },
		"responses-empty": func() error {
			_, err := provider.consumeResponses(strings.NewReader(sse(`{"type":"response.completed"}`)), emitOK)
			return err
		},
		"responses-invalid-call": func() error {
			_, err := provider.consumeResponses(strings.NewReader(sse(`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`, `{"type":"response.completed"}`)), emitOK)
			return err
		},

		"anthropic-json": func() error { _, err := provider.consumeAnthropic(strings.NewReader(sse("{")), emitOK); return err },
		"anthropic-index": func() error {
			_, err := provider.consumeAnthropic(strings.NewReader(sse(`{"type":"content_block_start","index":64,"content_block":{"type":"tool_use"}}`)), emitOK)
			return err
		},
		"anthropic-emit-start": func() error {
			_, err := provider.consumeAnthropic(strings.NewReader(sse(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"t","input":{}}}`)), emitBad)
			return err
		},
		"anthropic-emit-text": func() error {
			_, err := provider.consumeAnthropic(strings.NewReader(sse(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"x"}}`)), emitBad)
			return err
		},
		"anthropic-emit-reason": func() error {
			_, err := provider.consumeAnthropic(strings.NewReader(sse(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"x"}}`)), emitBad)
			return err
		},
		"anthropic-before-start": func() error {
			_, err := provider.consumeAnthropic(strings.NewReader(sse(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)), emitOK)
			return err
		},
		"anthropic-emit-input": func() error {
			_, err := provider.consumeAnthropic(strings.NewReader(sse(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"t","input":{}}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)), emitBad)
			return err
		},
		"anthropic-arguments": func() error {
			_, err := provider.consumeAnthropic(strings.NewReader(sse(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"t","input":{}}}`, fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, strings.Repeat("x", session.MaxArgumentsBytes+1)))), emitOK)
			return err
		},
		"anthropic-provider-error": func() error {
			_, err := provider.consumeAnthropic(strings.NewReader(sse(`{"type":"error"}`)), emitOK)
			return err
		},
		"anthropic-incomplete": func() error { _, err := provider.consumeAnthropic(strings.NewReader(""), emitOK); return err },
		"anthropic-empty": func() error {
			_, err := provider.consumeAnthropic(strings.NewReader(sse(`{"type":"message_stop"}`)), emitOK)
			return err
		},
		"anthropic-invalid-call": func() error {
			_, err := provider.consumeAnthropic(strings.NewReader(sse(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","input":{}}}`, `{"type":"message_stop"}`)), emitOK)
			return err
		},

		"chat-json": func() error { _, err := provider.consumeChat(strings.NewReader(sse("{")), emitOK); return err },
		"chat-provider-error": func() error {
			_, err := provider.consumeChat(strings.NewReader(sse(`{"error":{}}`)), emitOK)
			return err
		},
		"chat-emit-text": func() error {
			_, err := provider.consumeChat(strings.NewReader(sse(`{"choices":[{"delta":{"content":"x"}}]}`)), emitBad)
			return err
		},
		"chat-emit-reason": func() error {
			_, err := provider.consumeChat(strings.NewReader(sse(`{"choices":[{"delta":{"reasoning":"x"}}]}`)), emitBad)
			return err
		},
		"chat-emit-tool": func() error {
			_, err := provider.consumeChat(strings.NewReader(sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"t","arguments":"{}"}}]}}]}`)), emitBad)
			return err
		},
		"chat-arguments": func() error {
			_, err := provider.consumeChat(strings.NewReader(sse(fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"t","arguments":%q}}]}}]}`, strings.Repeat("x", session.MaxArgumentsBytes+1)))), emitOK)
			return err
		},
		"chat-no-finish": func() error {
			_, err := provider.consumeChat(strings.NewReader(sse(`{"choices":[{"delta":{"content":"x"}}]}`)), emitOK)
			return err
		},
		"chat-empty": func() error {
			_, err := provider.consumeChat(strings.NewReader(sse(`{"choices":[{"finish_reason":"stop"}]}`)), emitOK)
			return err
		},
		"chat-invalid-call": func() error {
			_, err := provider.consumeChat(strings.NewReader(sse(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)), emitOK)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := run(); err == nil {
				t.Fatal("expected failure")
			}
		})
	}

	responseDone := sse(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"c","name":"t","arguments":"{}"}}`, `{"type":"response.completed"}`)
	if completion, err := provider.consumeResponses(strings.NewReader(responseDone), emitOK); err != nil || len(completion.Calls) != 1 {
		t.Fatalf("response done completion=%#v err=%v", completion, err)
	}
	anthropicDone := sse(`{"type":"message_start","message":{"usage":{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":1,"cache_creation_input_tokens":1}}}`, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"t","input":{}}}`, `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":2}}`, `{"type":"message_stop"}`)
	if completion, err := provider.consumeAnthropic(strings.NewReader(anthropicDone), emitOK); err != nil || len(completion.Calls) != 1 || completion.Usage.CacheWriteTokens != 1 {
		t.Fatalf("anthropic completion=%#v err=%v", completion, err)
	}
	chatDone := sse(`{"choices":[{"delta":{"reasoning_content":"think","tool_calls":[{"index":0,"id":"c","function":{"name":"t"}}]},"finish_reason":"tool_calls"}]}`)
	if completion, err := provider.consumeChat(strings.NewReader(chatDone), emitOK); err != nil || len(completion.Calls) != 1 {
		t.Fatalf("chat completion=%#v err=%v", completion, err)
	}
}

func repeatedSSE(count int, event func(string) string, delta string) string {
	events := make([]string, count)
	for index := range events {
		events[index] = event(delta)
	}
	return sse(events...)
}

func TestProtocolAccumulatorLimitsAndToolCount(t *testing.T) {
	provider := &Provider{id: "p"}
	emit := func(session.AssistantChunk) error { return nil }
	textDelta := strings.Repeat("x", 100<<10)
	argumentDelta := strings.Repeat("x", 70<<10)

	responseText := repeatedSSE(3, func(delta string) string {
		return fmt.Sprintf(`{"type":"response.output_text.delta","delta":%q}`, delta)
	}, textDelta)
	if _, err := provider.consumeResponses(strings.NewReader(responseText), emit); err == nil {
		t.Fatal("Responses text limit missing")
	}
	responseReason := repeatedSSE(3, func(delta string) string {
		return fmt.Sprintf(`{"type":"response.reasoning_summary_text.delta","delta":%q}`, delta)
	}, textDelta)
	if _, err := provider.consumeResponses(strings.NewReader(responseReason), emit); err == nil {
		t.Fatal("Responses reasoning limit missing")
	}

	anthropicText := repeatedSSE(3, func(delta string) string {
		return fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":"text_delta","text":%q}}`, delta)
	}, textDelta)
	if _, err := provider.consumeAnthropic(strings.NewReader(anthropicText), emit); err == nil {
		t.Fatal("Anthropic text limit missing")
	}
	anthropicReason := repeatedSSE(3, func(delta string) string {
		return fmt.Sprintf(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":%q}}`, delta)
	}, textDelta)
	if _, err := provider.consumeAnthropic(strings.NewReader(anthropicReason), emit); err == nil {
		t.Fatal("Anthropic reasoning limit missing")
	}
	anthropicArguments := sse(
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c","name":"t","input":{}}}`,
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, argumentDelta),
		fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, argumentDelta),
	)
	if _, err := provider.consumeAnthropic(strings.NewReader(anthropicArguments), emit); err == nil {
		t.Fatal("Anthropic argument limit missing")
	}

	chatText := repeatedSSE(3, func(delta string) string {
		return fmt.Sprintf(`{"choices":[{"delta":{"content":%q}}]}`, delta)
	}, textDelta)
	if _, err := provider.consumeChat(strings.NewReader(chatText), emit); err == nil {
		t.Fatal("chat text limit missing")
	}
	chatReason := repeatedSSE(3, func(delta string) string {
		return fmt.Sprintf(`{"choices":[{"delta":{"reasoning":%q}}]}`, delta)
	}, textDelta)
	if _, err := provider.consumeChat(strings.NewReader(chatReason), emit); err == nil {
		t.Fatal("chat reasoning limit missing")
	}
	chatArguments := sse(
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"t","arguments":%q}}]}}]}`, argumentDelta),
		fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}}]}`, argumentDelta),
	)
	if _, err := provider.consumeChat(strings.NewReader(chatArguments), emit); err == nil {
		t.Fatal("chat argument limit missing")
	}
	toolEvents := make([]string, maxProviderToolCalls+1)
	for index := range toolEvents {
		toolEvents[index] = fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":%d,"id":"c%d","function":{"name":"t","arguments":"{}"}}]}}]}`, index, index)
	}
	if _, err := provider.consumeChat(strings.NewReader(sse(toolEvents...)), emit); err == nil {
		t.Fatal("chat tool count limit missing")
	}
}

func TestStreamCredentialAndRequestFailures(t *testing.T) {
	provider := &Provider{id: "openai", client: http.DefaultClient}
	provider.auth.chatGPTBaseURL = "http://localhost"
	current := &snapshot{baseURL: "http://localhost"}
	model := llm.ModelInfo{Provider: "openai", ID: "m", Vision: true, Tools: true}
	request := providerRequest()
	badRequest := request
	badRequest.Surface = []session.SurfaceNode{{Message: &session.Message{Role: session.RoleAssistant, Source: session.MessageSource{Kind: "provider"}, Content: []session.ContentBlock{{Type: session.ContentImage, Image: imageBlock().Image}}}}}
	if _, err := provider.streamResponses(context.Background(), current, model, llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}, badRequest, func(session.AssistantChunk) error { return nil }); err == nil {
		t.Fatal("responses request failure missing")
	}
	if _, err := provider.streamResponses(context.Background(), current, model, llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "token"}, request, func(session.AssistantChunk) error { return nil }); err == nil {
		t.Fatal("OAuth account accepted")
	}
	if _, err := provider.streamResponses(context.Background(), current, model, llm.Credential{}, request, func(session.AssistantChunk) error { return nil }); !errors.Is(err, llm.ErrNoCredential) {
		t.Fatalf("responses credential=%v", err)
	}
	provider.id = "anthropic"
	if _, err := provider.streamAnthropic(context.Background(), current, model, llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}, badRequest, func(session.AssistantChunk) error { return nil }); err == nil {
		t.Fatal("Anthropic request failure missing")
	}
	if _, err := provider.streamAnthropic(context.Background(), current, model, llm.Credential{}, request, func(session.AssistantChunk) error { return nil }); !errors.Is(err, llm.ErrNoCredential) {
		t.Fatalf("Anthropic credential=%v", err)
	}
	provider.id = "openrouter"
	if _, err := provider.streamOpenRouter(context.Background(), current, model, llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "token"}, request, func(session.AssistantChunk) error { return nil }); err == nil {
		t.Fatal("OpenRouter OAuth accepted")
	}
	if _, err := provider.streamOpenRouter(context.Background(), current, model, llm.Credential{Kind: llm.CredentialAPIKey, APIKey: "key"}, badRequest, func(session.AssistantChunk) error { return nil }); err == nil {
		t.Fatal("OpenRouter request failure missing")
	}
}

func TestImportCodexSafetyFailures(t *testing.T) {
	provider := &Provider{id: "openai", auth: authConfig{codexHome: t.TempDir()}}
	if _, err := provider.importCodex(); !errors.Is(err, llm.ErrNoCredential) {
		t.Fatalf("missing import=%v", err)
	}
	path := provider.auth.codexHome + "/auth.json"
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.importCodex(); !errors.Is(err, llm.ErrNoCredential) {
		t.Fatalf("directory import=%v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"auth_mode":"bad"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.importCodex(); !errors.Is(err, llm.ErrNoCredential) {
		t.Fatalf("invalid import=%v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // this negative test deliberately makes the credential cache unsafe
		t.Fatal(err)
	}
	if _, err := provider.importCodex(); !errors.Is(err, llm.ErrNoCredential) {
		t.Fatalf("unsafe import=%v", err)
	}
}

func TestOAuthFlowFailurePaths(t *testing.T) {
	originalRandom, originalListen := randomRead, callbackListen
	originalFloor, originalOpen := devicePollFloor, openAuthFile
	t.Cleanup(func() {
		randomRead, callbackListen = originalRandom, originalListen
		devicePollFloor, openAuthFile = originalFloor, originalOpen
	})
	interaction := &authInteraction{}
	provider := &Provider{id: "openai", client: http.DefaultClient, auth: authConfig{
		openAIAuthURL: "http://localhost", anthropicAuthURL: "http://localhost",
		anthropicExchangeURL: "http://localhost", openRouterAuthURL: "http://localhost",
	}}
	flows := []func(context.Context) error{
		func(ctx context.Context) error { _, err := provider.loginOpenAIBrowser(ctx, interaction); return err },
		func(ctx context.Context) error { _, err := provider.loginAnthropic(ctx, interaction); return err },
		func(ctx context.Context) error { _, err := provider.loginOpenRouter(ctx, interaction); return err },
	}
	for index, flow := range flows {
		randomRead = func([]byte) (int, error) { return 0, errors.New("random") }
		if err := flow(context.Background()); err == nil {
			t.Fatalf("flow %d accepted first random failure", index)
		}
		calls := 0
		randomRead = func(buffer []byte) (int, error) {
			calls++
			if calls == 2 {
				return 0, errors.New("random")
			}
			for index := range buffer {
				buffer[index] = byte(index)
			}
			return len(buffer), nil
		}
		if err := flow(context.Background()); err == nil {
			t.Fatalf("flow %d accepted second random failure", index)
		}
	}
	randomRead = originalRandom
	callbackListen = func(string, string) (net.Listener, error) { return nil, errors.New("listen") }
	for index, flow := range flows {
		if err := flow(context.Background()); err == nil {
			t.Fatalf("flow %d accepted listener failure", index)
		}
	}
	callbackListen = originalListen
	for index, flow := range flows {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := flow(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("flow %d cancellation=%v", index, err)
		}
	}

	serverMode := "token-error"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch serverMode {
		case "token-error":
			http.Error(writer, "no", http.StatusInternalServerError)
		case "malformed":
			_, _ = io.WriteString(writer, "{")
		case "missing-refresh":
			_ = json.NewEncoder(writer).Encode(oauthToken{AccessToken: jwt(map[string]any{}), ExpiresIn: 1})
		case "alternate-account":
			_ = json.NewEncoder(writer).Encode(oauthToken{AccessToken: jwt(map[string]any{"chatgpt_account_id": "alternate"}), RefreshToken: "next", ExpiresIn: 1})
		case "valid":
			_ = json.NewEncoder(writer).Encode(oauthToken{AccessToken: jwt(map[string]any{}), RefreshToken: "next", ExpiresIn: 1})
		case "device-invalid-start":
			_, _ = io.WriteString(writer, `{}`)
		case "device-cancel":
			_ = json.NewEncoder(writer).Encode(map[string]any{"device_auth_id": "d", "user_code": "u", "interval": 0})
		case "device-poll-error":
			if strings.HasSuffix(request.URL.Path, "/usercode") {
				_ = json.NewEncoder(writer).Encode(map[string]any{"device_auth_id": "d", "user_code": "u", "verification_uri": serverURL(request), "interval": 0})
			} else {
				http.Error(writer, "no", http.StatusInternalServerError)
			}
		case "device-invalid-token":
			if strings.HasSuffix(request.URL.Path, "/usercode") {
				_ = json.NewEncoder(writer).Encode(map[string]any{"device_auth_id": "d", "user_code": "u", "verification_uri": serverURL(request), "interval": 0})
			} else {
				_, _ = io.WriteString(writer, `{}`)
			}
		case "router-blank":
			_ = json.NewEncoder(writer).Encode(map[string]string{"key": " "})
		}
	}))
	defer server.Close()
	provider.client = server.Client()
	provider.auth.openAIAuthURL = server.URL
	provider.auth.anthropicAuthURL = server.URL
	provider.auth.anthropicExchangeURL = server.URL
	provider.auth.openRouterAuthURL = server.URL

	if _, err := provider.exchangeOpenAI(context.Background(), "c", "v", "http://localhost"); err == nil {
		t.Fatal("OpenAI exchange transport error missing")
	}
	serverMode = "malformed"
	if _, err := provider.exchangeOpenAI(context.Background(), "c", "v", "http://localhost"); err == nil {
		t.Fatal("OpenAI malformed token accepted")
	}
	serverMode = "alternate-account"
	credential, err := provider.exchangeOpenAI(context.Background(), "c", "v", "http://localhost")
	if err != nil || credential.AccountID != "alternate" {
		t.Fatalf("alternate account=%#v err=%v", credential, err)
	}
	if _, err := provider.refreshOpenAI(context.Background(), llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "old"}); err == nil {
		t.Fatal("OpenAI missing refresh accepted")
	}
	serverMode = "token-error"
	if _, err := provider.refreshOpenAI(context.Background(), llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "old", RefreshToken: "old-refresh"}); err == nil {
		t.Fatal("OpenAI refresh HTTP error missing")
	}
	serverMode = "malformed"
	if _, err := provider.refreshOpenAI(context.Background(), llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "old", RefreshToken: "old-refresh"}); err == nil {
		t.Fatal("OpenAI malformed refresh accepted")
	}
	serverMode = "missing-refresh"
	credential, err = provider.refreshOpenAI(context.Background(), llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "old", RefreshToken: "old-refresh", AccountID: "old-account"})
	if err != nil || credential.RefreshToken != "old-refresh" || credential.AccountID != "old-account" {
		t.Fatalf("OpenAI refresh fallback=%#v err=%v", credential, err)
	}

	devicePollFloor = time.Nanosecond
	serverMode = "token-error"
	if _, err := provider.loginOpenAIDevice(context.Background(), interaction); err == nil {
		t.Fatal("device start HTTP error missing")
	}
	serverMode = "device-invalid-start"
	if _, err := provider.loginOpenAIDevice(context.Background(), interaction); err == nil {
		t.Fatal("invalid device start accepted")
	}
	serverMode = "device-cancel"
	devicePollFloor = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := provider.loginOpenAIDevice(ctx, cancelInteraction{cancel: cancel}); !errors.Is(err, context.Canceled) {
		t.Fatalf("device wait cancellation=%v", err)
	}
	devicePollFloor = time.Nanosecond
	serverMode = "device-poll-error"
	if _, err := provider.loginOpenAIDevice(context.Background(), interaction); err == nil {
		t.Fatal("device poll HTTP error missing")
	}
	serverMode = "device-invalid-token"
	if _, err := provider.loginOpenAIDevice(context.Background(), interaction); err == nil {
		t.Fatal("invalid device token accepted")
	}

	provider.id = "anthropic"
	serverMode = "token-error"
	auto := &authInteraction{auto: true}
	if _, err := provider.loginAnthropic(context.Background(), auto); err == nil {
		t.Fatal("Anthropic token HTTP error missing")
	}
	serverMode = "malformed"
	if _, err := provider.loginAnthropic(context.Background(), auto); err == nil {
		t.Fatal("Anthropic malformed token accepted")
	}
	if _, err := provider.refreshAnthropic(context.Background(), llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "old"}); err == nil {
		t.Fatal("Anthropic missing refresh accepted")
	}
	serverMode = "token-error"
	if _, err := provider.refreshAnthropic(context.Background(), llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "old", RefreshToken: "old-refresh"}); err == nil {
		t.Fatal("Anthropic refresh HTTP error missing")
	}
	serverMode = "malformed"
	if _, err := provider.refreshAnthropic(context.Background(), llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "old", RefreshToken: "old-refresh"}); err == nil {
		t.Fatal("Anthropic malformed refresh accepted")
	}
	serverMode = "missing-refresh"
	credential, err = provider.refreshAnthropic(context.Background(), llm.Credential{Kind: llm.CredentialOAuth, AccessToken: "old", RefreshToken: "old-refresh"})
	if err != nil || credential.RefreshToken != "old-refresh" {
		t.Fatalf("Anthropic refresh fallback=%#v err=%v", credential, err)
	}

	provider.id = "openrouter"
	serverMode = "token-error"
	if _, err := provider.loginOpenRouter(context.Background(), auto); err == nil {
		t.Fatal("OpenRouter token HTTP error missing")
	}
	serverMode = "router-blank"
	if _, err := provider.loginOpenRouter(context.Background(), auto); err == nil {
		t.Fatal("OpenRouter blank token accepted")
	}
}

func TestJWTHelpersAndImportOpenFailures(t *testing.T) {
	invalidJSON := "x." + base64.RawURLEncoding.EncodeToString([]byte("{")) + ".x"
	stringNumber := jwt(map[string]any{"exp": "soon"})
	for _, token := range []string{"x.!.x", invalidJSON} {
		if jwtStringClaim(token, "x") != "" || jwtNumericClaim(token, "exp") != 0 {
			t.Fatalf("invalid JWT parsed: %q", token)
		}
	}
	if jwtNumericClaim(stringNumber, "exp") != 0 {
		t.Fatal("string expiry parsed as number")
	}
	home := t.TempDir()
	path := home + "/auth.json"
	access := jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()})
	encoded := fmt.Appendf(nil, `{"auth_mode":"chatgpt","tokens":{"access_token":%q,"refresh_token":"r","account_id":"a"}}`, access)
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	original := openAuthFile
	openAuthFile = func(string) (*os.File, error) { return nil, errors.New("open") }
	t.Cleanup(func() { openAuthFile = original })
	provider := &Provider{id: "openai", auth: authConfig{codexHome: home}}
	if _, err := provider.importCodex(); !errors.Is(err, llm.ErrNoCredential) {
		t.Fatalf("import open error=%v", err)
	}
}
