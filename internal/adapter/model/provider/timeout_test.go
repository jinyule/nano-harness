package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	appsettings "github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// stallingServer streams the given prefix and then holds the response open
// until the client leaves or the test ends.
func stallingServer(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, prefix)
		writer.(http.Flusher).Flush()
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); server.Close() })
	return server
}

func consumeInto(provider *Provider) func(io.Reader) (llm.Completion, error) {
	return func(body io.Reader) (llm.Completion, error) {
		return provider.consumeResponses(body, func(session.AssistantChunk) error { return nil })
	}
}

func TestNew_DefaultClientHasNoTotalDeadline(t *testing.T) {
	provider, err := New(&llm.Runtime{}, appsettings.New(), Config{ID: "openai", CodexHome: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	// A client-wide deadline also bounds reading the body, so it would end
	// a long stream that is still delivering events.
	if provider.client.Timeout != 0 {
		t.Fatalf("default client deadline = %s, want none", provider.client.Timeout)
	}
}

// TestSend_BodyDeadlineIsTimeout covers a deadline that ends the body read:
// it is a timeout, not malformed wire data.
func TestSend_BodyDeadlineIsTimeout(t *testing.T) {
	server := stallingServer(t, sse(`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`))
	provider := &Provider{id: "openai", client: &http.Client{Timeout: 200 * time.Millisecond}, idleTimeout: time.Minute}
	_, err := provider.streamRequest(context.Background(), server.URL, struct{}{}, nil, consumeInto(provider))
	expectLLMError(t, err, llm.ErrorTimeout)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errIdleTimeout) {
		t.Fatalf("client deadline = %v", err)
	}
}

// idleProvider returns a provider whose exchange watchdog fires after idle.
func idleProvider(client *http.Client, idle time.Duration) *Provider {
	return &Provider{id: "openai", client: client, idleTimeout: idle}
}

// expectIdleTimeout requires the watchdog's timeout and rejects any reading
// of it as the caller's own cancellation or deadline.
func expectIdleTimeout(t *testing.T, err error) {
	t.Helper()
	expectLLMError(t, err, llm.ErrorTimeout)
	if !errors.Is(err, errIdleTimeout) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("idle timeout chain = %v", err)
	}
}

func TestSend_ActiveStreamOutlivesIdleWindow(t *testing.T) {
	const idle, interval, ticks = 300 * time.Millisecond, 50 * time.Millisecond, 15
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", "text/event-stream")
		for range ticks {
			_, _ = io.WriteString(writer, sse(`{"type":"response.reasoning_summary_text.delta","delta":"."}`))
			writer.(http.Flusher).Flush()
			time.Sleep(interval)
		}
		_, _ = io.WriteString(writer, sse(`{"type":"response.output_text.delta","delta":"done"}`, `{"type":"response.completed","response":{}}`))
	}))
	t.Cleanup(server.Close)
	provider := idleProvider(server.Client(), idle)
	started := time.Now()
	completion, err := provider.streamRequest(context.Background(), server.URL, struct{}{}, nil, consumeInto(provider))
	if err != nil || session.Text(completion.Message) != "done" {
		t.Fatalf("completion=%#v err=%v", completion, err)
	}
	// The stream ran well past one idle window without being cut.
	if elapsed := time.Since(started); elapsed < 2*idle {
		t.Fatalf("stream finished after %s; the fixture must outlast the idle window", elapsed)
	}
}

func TestSend_StalledStreamTimesOut(t *testing.T) {
	const idle = 200 * time.Millisecond
	server := stallingServer(t, sse(`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`))
	provider := idleProvider(server.Client(), idle)
	started := time.Now()
	_, err := provider.streamRequest(context.Background(), server.URL, struct{}{}, nil, consumeInto(provider))
	expectIdleTimeout(t, err)
	if elapsed := time.Since(started); elapsed < idle {
		t.Fatalf("timed out after %s, before the idle window", elapsed)
	}
}

func TestSend_ResponseHeaderStallTimesOut(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); server.Close() })
	provider := idleProvider(server.Client(), 200*time.Millisecond)
	_, err := provider.streamRequest(context.Background(), server.URL, struct{}{}, nil, consumeInto(provider))
	expectIdleTimeout(t, err)
}

func TestSend_ConnectStallTimesOut(t *testing.T) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	t.Cleanup(transport.CloseIdleConnections)
	provider := idleProvider(&http.Client{Transport: transport}, 200*time.Millisecond)
	_, err := provider.streamRequest(context.Background(), "http://provider.invalid", struct{}{}, nil, consumeInto(provider))
	expectIdleTimeout(t, err)
}

func TestSend_CallerCancellationIsNotATimeout(t *testing.T) {
	server := stallingServer(t, sse(`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`))
	provider := idleProvider(server.Client(), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	consume := func(body io.Reader) (llm.Completion, error) {
		return provider.consumeResponses(body, func(session.AssistantChunk) error {
			cancel() // the caller gives up after the first event
			return nil
		})
	}
	_, err := provider.streamRequest(ctx, server.URL, struct{}{}, nil, consume)
	var failure *llm.Error
	if !errors.Is(err, context.Canceled) || errors.Is(err, errIdleTimeout) || errors.As(err, &failure) && failure.Code == llm.ErrorTimeout {
		t.Fatalf("caller cancellation = %v", err)
	}
}

// TestSend_CallerDeadlineKeepsItsCause keeps the caller's own deadline
// visible, so the agent classifies it by the caller's context.
func TestSend_CallerDeadlineKeepsItsCause(t *testing.T) {
	server := stallingServer(t, sse(`{"type":"response.reasoning_summary_text.delta","delta":"thinking"}`))
	provider := idleProvider(server.Client(), time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := provider.streamRequest(ctx, server.URL, struct{}{}, nil, consumeInto(provider))
	expectLLMError(t, err, llm.ErrorTimeout)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errIdleTimeout) {
		t.Fatalf("caller deadline = %v", err)
	}
}

func TestPostOAuth_StalledResponseTimesOut(t *testing.T) {
	server := stallingServer(t, "")
	provider := idleProvider(server.Client(), 200*time.Millisecond)
	_, err := provider.postJSON(context.Background(), server.URL, struct{}{}, nil)
	expectIdleTimeout(t, err)
}
