package provider

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"testing/synctest"
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

// exitedBeforeReturn fails unless the watchdog worker had already exited
// when stop returned.
func exitedBeforeReturn(t *testing.T, dog *watchdog) {
	t.Helper()
	select {
	case <-dog.exited:
	default:
		t.Fatal("stop returned while the watchdog worker was still running")
	}
}

// TestWatchdog_ActivityRearmsAndStopJoinsAnExpiredWorker runs on the
// synctest clock: reads rearm the interval, expiry cancels with the idle
// cause, reads after expiry do not revive it, and stop returns only after
// the worker exits.
func TestWatchdog_ActivityRearmsAndStopJoinsAnExpiredWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		provider := idleProvider(nil, time.Second)
		ctx, dog, stop := provider.watch(context.Background())
		body := dog.body(iotest.OneByteReader(strings.NewReader("abcd")))
		buffer := make([]byte, 1)
		time.Sleep(900 * time.Millisecond)
		if _, err := body.Read(buffer); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		time.Sleep(900 * time.Millisecond)
		synctest.Wait()
		if ctx.Err() != nil {
			t.Fatalf("a read did not rearm the watchdog: %v", context.Cause(ctx))
		}
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()
		if !errors.Is(context.Cause(ctx), errIdleTimeout) {
			t.Fatalf("expired cause = %v", context.Cause(ctx))
		}
		// The worker no longer drains activity; the second read finds the
		// pending rearm and moves on.
		for range 2 {
			if _, err := body.Read(buffer); err != nil {
				t.Fatal(err)
			}
		}
		stop()
		exitedBeforeReturn(t, dog)
	})
}

// TestWatchdog_StopOverlappingExpiryJoinsTheWorker stops the watchdog at the
// instant its interval expires, so stop and expiry race on the worker.
func TestWatchdog_StopOverlappingExpiryJoinsTheWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		provider := idleProvider(nil, time.Second)
		ctx, dog, stop := provider.watch(context.Background())
		time.Sleep(time.Second)
		stop()
		exitedBeforeReturn(t, dog)
		if cause := context.Cause(ctx); !errors.Is(cause, errIdleTimeout) && !errors.Is(cause, context.Canceled) {
			t.Fatalf("cause = %v", cause)
		}
	})
	// Without expiry, stop releases the context as an ordinary cancellation.
	synctest.Test(t, func(t *testing.T) {
		provider := idleProvider(nil, time.Second)
		ctx, dog, stop := provider.watch(context.Background())
		stop()
		exitedBeforeReturn(t, dog)
		if !errors.Is(context.Cause(ctx), context.Canceled) {
			t.Fatalf("cause = %v", context.Cause(ctx))
		}
	})
}

// errorBodyServer answers with status and writes body one byte per
// interval; with hold it then keeps the response open until the client
// leaves, otherwise it completes the response.
func errorBodyServer(t *testing.T, status int, body string, interval time.Duration, hold bool) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		writer.WriteHeader(status)
		writer.(http.Flusher).Flush()
		for index := range len(body) {
			_, _ = io.WriteString(writer, body[index:index+1])
			writer.(http.Flusher).Flush()
			time.Sleep(interval)
		}
		if !hold {
			return
		}
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() { close(release); server.Close() })
	return server
}

// TestSend_ErrorBodyIsWatched covers a non-2xx answer: the status decides
// the class, the body is read under the watchdog so a slowly delivered
// context-window hint still arrives, and a stalled body keeps its timeout
// as the cause instead of being dropped.
func TestSend_ErrorBodyIsWatched(t *testing.T) {
	const idle = 300 * time.Millisecond
	t.Run("slow body", func(t *testing.T) {
		// Fifteen bytes 60 ms apart span three idle windows.
		server := errorBodyServer(t, http.StatusBadRequest, "too many tokens", 60*time.Millisecond, false)
		provider := idleProvider(server.Client(), idle)
		_, err := provider.streamRequest(context.Background(), server.URL, struct{}{}, nil, consumeInto(provider))
		expectLLMError(t, err, llm.ErrorContextWindow)
	})
	t.Run("stalled body", func(t *testing.T) {
		server := errorBodyServer(t, http.StatusUnauthorized, "denied", 0, true)
		provider := idleProvider(server.Client(), idle)
		_, err := provider.streamRequest(context.Background(), server.URL, struct{}{}, nil, consumeInto(provider))
		expectLLMError(t, err, llm.ErrorUnauthorized)
		var cause *llm.Error
		if !errors.Is(err, errIdleTimeout) || !errors.As(errors.Unwrap(err), &cause) || cause.Code != llm.ErrorTimeout {
			t.Fatalf("stalled error body lost its timeout: %v", err)
		}
	})
	t.Run("caller cancels", func(t *testing.T) {
		server := errorBodyServer(t, http.StatusInternalServerError, "x", 0, true)
		// The cancellation waits until the error body is being read, so the
		// headers have arrived and the transport cannot be the one to see it.
		entered := make(chan struct{})
		inner := server.Client().Transport
		client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			response, err := inner.RoundTrip(request)
			if err == nil {
				response.Body = &signalingBody{ReadCloser: response.Body, entered: entered}
			}
			return response, err
		})}
		provider := idleProvider(client, time.Minute)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-entered
			cancel()
		}()
		_, err := provider.streamRequest(ctx, server.URL, struct{}{}, nil, consumeInto(provider))
		select {
		case <-entered:
		default:
			t.Fatal("the error body was never read")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("caller cancellation while reading the error body = %v", err)
		}
	})
}

// signalingBody closes entered on its first Read.
type signalingBody struct {
	io.ReadCloser
	entered chan struct{}
	once    sync.Once
}

func (body *signalingBody) Read(buffer []byte) (int, error) {
	body.once.Do(func() { close(body.entered) })
	return body.ReadCloser.Read(buffer)
}
