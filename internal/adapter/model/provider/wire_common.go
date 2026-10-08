package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/session"
)

var (
	// errProviderRedirect marks a refused redirect on a credential-bearing request.
	errProviderRedirect = errors.New("provider redirect refused")
	// errIdleTimeout is the cause an exchange watchdog cancels with. It does
	// not wrap context.DeadlineExceeded: the caller's context is still live.
	errIdleTimeout = errors.New("provider exchange idle timeout")
)

// watchdog ends one provider exchange that makes no progress for its idle
// interval. Arming covers connecting, sending the request, and waiting for
// response headers; every body read that returns data rearms it.
type watchdog struct {
	idle  time.Duration
	timer *time.Timer
}

// watch derives the exchange context and its watchdog. The returned stop
// releases both and must run after the response body is closed.
func (provider *Provider) watch(ctx context.Context) (context.Context, *watchdog, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	dog := &watchdog{idle: provider.idleTimeout}
	dog.timer = time.AfterFunc(dog.idle, func() { cancel(errIdleTimeout) })
	return ctx, dog, func() {
		dog.timer.Stop()
		cancel(nil)
	}
}

// body returns reader with each data-bearing read rearming the watchdog.
func (dog *watchdog) body(reader io.Reader) io.Reader {
	return activityReader{reader: reader, dog: dog}
}

type activityReader struct {
	reader io.Reader
	dog    *watchdog
}

func (reader activityReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	if count > 0 {
		reader.dog.timer.Reset(reader.dog.idle)
	}
	return count, err
}

// idleFailure reports a failure of an exchange its watchdog ended as a
// provider timeout. The aborted transport or body read surfaces as a
// cancellation, which would otherwise read as the caller's own.
func (provider *Provider) idleFailure(ctx context.Context, err error) error {
	if err != nil && errors.Is(context.Cause(ctx), errIdleTimeout) {
		return &llm.Error{Code: llm.ErrorTimeout, Provider: provider.id, Cause: fmt.Errorf("no data for %s: %w", provider.idleTimeout, errIdleTimeout)}
	}
	return err
}

func (provider *Provider) streamRequest(ctx context.Context, endpoint string, payload any, headers map[string]string, consume func(io.Reader) (llm.Completion, error)) (llm.Completion, error) {
	return send(ctx, provider, endpoint, "text/event-stream", payload, headers, consume)
}

// do sends one provider request. Every provider request carries credentials
// or OAuth secrets, so the client refuses redirects instead of forwarding them
// or the request body to another URL; a refused redirect is a protocol error.
func (provider *Provider) do(request *http.Request) (*http.Response, error) {
	client := *provider.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errProviderRedirect }
	response, err := client.Do(request)
	if errors.Is(err, errProviderRedirect) {
		return nil, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: errProviderRedirect}
	}
	if err != nil {
		return nil, transportError(provider.id, err)
	}
	return response, nil
}

// send posts one bounded JSON request and hands the bounded success body to consume.
func send[T any](ctx context.Context, provider *Provider, endpoint, accept string, payload any, headers map[string]string, consume func(io.Reader) (T, error)) (T, error) {
	var zero T
	encoded, err := json.Marshal(payload)
	if err != nil {
		return zero, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
	}
	if len(encoded) > maxProviderRequestBytes {
		return zero, &llm.Error{Code: llm.ErrorInvalid, Provider: provider.id, Cause: errors.New("request exceeds size limit")}
	}
	ctx, dog, stop := provider.watch(ctx)
	defer stop()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return zero, &llm.Error{Code: llm.ErrorProtocol, Provider: provider.id, Cause: err}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", accept)
	request.Header.Set("User-Agent", "nano-harness")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := provider.do(request)
	if err != nil {
		return zero, provider.idleFailure(ctx, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failure := statusError(provider.id, response.StatusCode, response.Header.Get("Retry-After"))
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		if contextWindowFailure(body) {
			failure = &llm.Error{Code: llm.ErrorContextWindow, Provider: provider.id, HTTPStatus: response.StatusCode}
		}
		return zero, failure
	}
	value, err := consume(io.LimitReader(dog.body(response.Body), maxProviderResponseBytes+1))
	return value, provider.idleFailure(ctx, err)
}

// decodeJSON strictly bounds and decodes one non-streaming provider response.
func decodeJSON(body io.Reader, providerID string, target any) error {
	encoded, err := io.ReadAll(body)
	if err != nil {
		return readFailure(providerID, err)
	}
	if len(encoded) > maxProviderResponseBytes {
		return &llm.Error{Code: llm.ErrorProtocol, Provider: providerID, Cause: errors.New("response exceeds size limit")}
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return &llm.Error{Code: llm.ErrorProtocol, Provider: providerID, Cause: err}
	}
	return nil
}

func contextWindowFailure(body []byte) bool {
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "context_length_exceeded") || strings.Contains(lower, "context window") || strings.Contains(lower, "too many tokens")
}

func scanSSE(body io.Reader, providerID string, visit func([]byte) error) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), maxProviderSSELineBytes)
	total := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		total += len(line) + 1
		if total > maxProviderResponseBytes {
			return &llm.Error{Code: llm.ErrorProtocol, Provider: providerID, Cause: errors.New("stream exceeds size limit")}
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if err := visit(data); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return readFailure(providerID, err)
	}
	return nil
}

// readFailure classifies a failed body read: a deadline that ended the read
// is a timeout; any other failure leaves the response incomplete.
func readFailure(providerID string, err error) error {
	code := llm.ErrorProtocol
	if errors.Is(err, context.DeadlineExceeded) {
		code = llm.ErrorTimeout
	}
	return &llm.Error{Code: code, Provider: providerID, Cause: err}
}

func parseRetryAfter(value string) int64 {
	if seconds, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && seconds > 0 {
		return int64(seconds * float64(time.Second/time.Millisecond))
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at).Milliseconds(), 0)
	}
	return 0
}

func validToolCall(call *session.ToolCall) error {
	*call = call.LimitArguments()
	return (session.Record{Type: session.RecordToolCall, Turn: 1, Step: 1, Call: call}).Validate()
}

func appendBounded(builder *strings.Builder, value string, limit int, subject, providerID string) error {
	if builder.Len()+len(value) > limit {
		return &llm.Error{Code: llm.ErrorProtocol, Provider: providerID, Cause: fmt.Errorf("%s exceeds size limit", subject)}
	}
	builder.WriteString(value)
	return nil
}

func assistantMessage(text string) session.Message {
	content := []session.ContentBlock(nil)
	if text != "" {
		content = []session.ContentBlock{{Type: session.ContentText, Text: text}}
	}
	return session.Message{Role: session.RoleAssistant, Content: content, Source: session.MessageSource{Kind: "provider"}}
}
