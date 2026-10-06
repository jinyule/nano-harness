package fetch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/web"
)

// memberBomb has a tiny final body but spends its intermediate output on gzip
// framing. The final-body cap cannot account for those empty members.
func memberBomb(t *testing.T) []byte {
	t.Helper()
	empty := compressed(t, "gzip", nil)
	inner := bytes.Repeat(empty, maxResponseBytes*2/len(empty)+1)
	inner = append(inner, compressed(t, "gzip", []byte("ok"))...)
	return compressed(t, "gzip", inner)
}

func TestFetch_RejectsIntermediateDecompressionBomb(t *testing.T) {
	data := memberBomb(t)
	current := newFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		writer.Header().Set("Content-Encoding", "gzip, gzip")
		_, _ = writer.Write(data)
	})
	result, err := current.client.Fetch(t.Context(), "http://docs.example.test/")
	expectCode(t, err, web.CodeFetchTooLarge)
	if result.Content != "" {
		t.Fatalf("rejected response published %q", result.Content)
	}
}

type firstReadAction struct {
	source io.Reader
	action func()
}

type bytesWithEOF struct{ *bytes.Reader }

func (reader bytesWithEOF) Read(buffer []byte) (int, error) {
	count, err := reader.Reader.Read(buffer)
	if err == nil && reader.Len() == 0 {
		err = io.EOF
	}
	return count, err
}

func (reader *firstReadAction) Read(buffer []byte) (int, error) {
	count, err := reader.source.Read(buffer)
	if reader.action != nil {
		action := reader.action
		reader.action = nil
		action()
	}
	return count, err
}

func bodyResponse(source io.Reader, coding string) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"text/plain"}, "Content-Encoding": {coding}},
		ContentLength: -1,
		Body:          io.NopCloser(source),
	}
}

func TestFetch_CancellationAfterFirstBodyReadStopsDecoding(t *testing.T) {
	for _, coding := range []string{"", "identity", "gzip", "deflate", "raw-deflate", "gzip, deflate"} {
		t.Run(coding, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			data := []byte("ok")
			header := coding
			switch coding {
			case "", "identity":
			case "gzip, deflate":
				data = compressed(t, "deflate", compressed(t, "gzip", data))
			default:
				data = compressed(t, coding, data)
				if coding == "raw-deflate" {
					header = "deflate"
				}
			}
			body := &firstReadAction{source: bytesWithEOF{bytes.NewReader(data)}, action: cancel}
			response := bodyResponse(body, header)
			defer func() { _ = response.Body.Close() }()
			_, err := New(Config{}).read(ctx, response, &url.URL{Scheme: "https", Host: "example.com"})
			expectCode(t, err, web.CodeAborted)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation cause: %v", err)
			}
		})
	}
}

func TestFetch_AlreadyCancelledContextDoesNotReadBody(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	read := false
	body := &firstReadAction{source: strings.NewReader("ok"), action: func() { read = true }}
	response := bodyResponse(body, "identity")
	defer func() { _ = response.Body.Close() }()
	_, err := New(Config{}).read(ctx, response, &url.URL{Scheme: "https", Host: "example.com"})
	expectCode(t, err, web.CodeAborted)
	if read || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled fetch read body=%v err=%v", read, err)
	}
}

func TestFetch_ExpiredDeadlineStopsBufferedMultiLayerDecoding(t *testing.T) {
	data := memberBomb(t)
	client := New(Config{})
	client.timeout = 20 * time.Millisecond
	ctx, cancel := context.WithTimeoutCause(t.Context(), client.timeout, errFetchTimeout)
	defer cancel()
	// The first read returns successful, buffered bytes only after the deadline.
	// This barrier fixes the ordering without relying on decoding speed.
	body := &firstReadAction{source: bytes.NewReader(data), action: func() { <-ctx.Done() }}
	response := bodyResponse(body, "gzip, gzip")
	defer func() { _ = response.Body.Close() }()
	started := time.Now()
	_, err := client.read(ctx, response, &url.URL{Scheme: "https", Host: "example.com"})
	expectCode(t, err, web.CodeFetchTimeout)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lost deadline cause: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("decoding did not stop promptly after deadline: %s", elapsed)
	}
}

func TestFetch_RejectsExcessiveContentEncodingLayersBeforeReading(t *testing.T) {
	failure := errors.New("body must not be read")
	response := bodyResponse(errorReader{failure}, strings.Repeat("identity,", 5)+"gzip")
	defer func() { _ = response.Body.Close() }()
	_, err := New(Config{}).read(t.Context(), response, &url.URL{Scheme: "https", Host: "example.com"})
	expectCode(t, err, web.CodeFetchTooLarge)
	if errors.Is(err, failure) {
		t.Fatalf("excessive encoding declaration consumed body: %v", err)
	}
}

func TestDecompress_CancellationStopsBufferedDecoder(t *testing.T) {
	for _, coding := range []string{"gzip", "deflate", "raw-deflate"} {
		t.Run(coding, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			wire := bytes.NewReader(compressed(t, coding, bytes.Repeat([]byte("a"), 100_000)))
			header := coding
			if coding == "raw-deflate" {
				header = "deflate"
			}
			source, decoders, err := decompress(ctx, wire, header)
			t.Cleanup(func() {
				for _, decoder := range decoders {
					_ = decoder.Close()
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			var first [1]byte
			if count, readErr := source.Read(first[:]); count != 1 || readErr != nil || first[0] != 'a' || wire.Len() != 0 {
				t.Fatalf("first read=%d/%q err=%v buffered wire=%d", count, first, readErr, wire.Len())
			}
			cancel()
			remaining, err := io.ReadAll(source)
			if len(remaining) != 0 || !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled decoder returned %d bytes, err=%v", len(remaining), err)
			}
		})
	}
}

func TestFetch_AcceptsFiveEncodingLayers(t *testing.T) {
	data := []byte("ok")
	for range 5 {
		data = compressed(t, "gzip", data)
	}
	response := bodyResponse(bytes.NewReader(data), "gzip,gzip,gzip,gzip,gzip")
	defer func() { _ = response.Body.Close() }()
	result, err := New(Config{}).read(t.Context(), response, &url.URL{Scheme: "https", Host: "example.com"})
	if err != nil || result.Content != "ok" || result.Truncated {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestDecompress_IdentityPreservesFinalBodyTruncation(t *testing.T) {
	data := compressed(t, "gzip", make([]byte, maxResponseBytes+1))
	for _, coding := range []string{"identity, gzip", "gzip, identity"} {
		t.Run(coding, func(t *testing.T) {
			response := bodyResponse(bytes.NewReader(data), coding)
			defer func() { _ = response.Body.Close() }()
			result, err := New(Config{}).read(t.Context(), response, &url.URL{Scheme: "https", Host: "example.com"})
			if err != nil || !result.Truncated || result.Content != strings.Repeat("\x00", maxBodyUnits) {
				t.Fatalf("bytes=%d truncated=%v err=%v", len(result.Content), result.Truncated, err)
			}
		})
	}
}

func TestExpansionReader_ExactBudgetAndOverflow(t *testing.T) {
	for _, data := range []string{"abcd", "abcde"} {
		t.Run(data, func(t *testing.T) {
			reader := &expansionReader{cappedReader{source: strings.NewReader(data), remaining: 4}}
			body, err := io.ReadAll(reader)
			if string(body) != "abcd" || errors.Is(err, errDecompressionLimit) != (len(data) > 4) {
				t.Fatalf("body=%q err=%v", body, err)
			}
			if len(data) == 4 && err != nil {
				t.Fatal(err)
			}
		})
	}
}
