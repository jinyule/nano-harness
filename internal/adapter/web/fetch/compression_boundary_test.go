package fetch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// emptyMemberStream spends the wire budget on valid gzip framing while the
// decoded body remains "ok". An optional filename pads the final member.
func emptyMemberStream(t *testing.T, size int) []byte {
	t.Helper()
	empty := compressed(t, "gzip", nil)
	tail := compressed(t, "gzip", []byte("ok"))
	count := (size - len(tail)) / len(empty)
	padding := size - count*len(empty) - len(tail)
	if padding > 0 {
		tail[3] |= 8 // FNAME, terminated by NUL
		name := make([]byte, padding)
		for index := range name[:padding-1] {
			name[index] = 'a'
		}
		tail = append(append(append([]byte(nil), tail[:10]...), name...), tail[10:]...)
	}
	return append(bytes.Repeat(empty, count), tail...)
}

type countedBody struct {
	source io.Reader
	bytes  int
}

func (body *countedBody) Read(buffer []byte) (int, error) {
	count, err := body.source.Read(buffer)
	body.bytes += count
	return count, err
}

func TestFetch_BoundsEncodedNetworkInput(t *testing.T) {
	for _, size := range []int{maxResponseBytes, maxResponseBytes + 1, maxResponseBytes * 2} {
		data := emptyMemberStream(t, size)
		for _, coding := range []string{"gzip", "x-gzip", "identity, gzip", "gzip, identity"} {
			t.Run(fmt.Sprintf("%s/%d", coding, size), func(t *testing.T) {
				body := &countedBody{source: bytesWithEOF{bytes.NewReader(data)}}
				response := bodyResponse(body, coding)
				defer func() { _ = response.Body.Close() }()
				result, err := New(Config{}).read(t.Context(), response, &url.URL{Scheme: "https", Host: "example.com"})
				if size == maxResponseBytes {
					if err != nil || result.Content != "ok" || result.Truncated || body.bytes != size {
						t.Fatalf("exact wire budget: result=%+v bytes=%d err=%v", result, body.bytes, err)
					}
					return
				}
				expectCode(t, err, web.CodeFetchTooLarge)
				var failure *web.Error
				if !errors.As(err, &failure) || failure.Message != "response exceeds the maximum of 5000000 bytes" || !errors.Is(err, errDecompressionLimit) {
					t.Fatalf("oversized wire error lost message or cause: %v", err)
				}
				if result.Content != "" || body.bytes > maxResponseBytes+1 {
					t.Fatalf("oversized wire published %q after reading %d bytes", result.Content, body.bytes)
				}
			})
		}
	}
}

func TestFetch_RejectsSingleLayerCompressedNetworkBomb(t *testing.T) {
	data := emptyMemberStream(t, maxResponseBytes*2)
	current := newFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		writer.Header().Set("Content-Encoding", "gzip")
		// Flush headers before the body to force chunked transfer, without a
		// declared Content-Length that could reject the response ahead of reading.
		writer.(http.Flusher).Flush()
		_, _ = writer.Write(data)
	})
	result, err := current.client.Fetch(t.Context(), "http://docs.example.test/")
	expectCode(t, err, web.CodeFetchTooLarge)
	if result.Content != "" || !errors.Is(err, errDecompressionLimit) {
		t.Fatalf("network bomb: result=%+v err=%v", result, err)
	}
}

func TestFetch_IdentityNetworkOverflowRequiresEncodingHeader(t *testing.T) {
	for _, coding := range []string{"", "identity"} {
		t.Run(coding, func(t *testing.T) {
			body := &countedBody{source: bytes.NewReader(make([]byte, maxResponseBytes+1))}
			response := bodyResponse(body, coding)
			defer func() { _ = response.Body.Close() }()
			result, err := New(Config{}).read(t.Context(), response, &url.URL{Scheme: "https", Host: "example.com"})
			if coding == "identity" {
				expectCode(t, err, web.CodeFetchTooLarge)
				if result.Content != "" || !errors.Is(err, errDecompressionLimit) {
					t.Fatalf("encoded identity overflow: result=%+v err=%v", result, err)
				}
			} else if err != nil || !result.Truncated || len(result.Content) != maxBodyUnits {
				t.Fatalf("unencoded body truncation: result=%+v err=%v", result, err)
			}
			if body.bytes != maxResponseBytes+1 {
				t.Fatalf("read %d bytes, want budget plus one overflow probe", body.bytes)
			}
		})
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
