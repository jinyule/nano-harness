package fetch

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jinyule/nano-harness/internal/app/web"
)

func compressed(t *testing.T, coding string, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	var writer io.WriteCloser
	switch coding {
	case "gzip":
		writer = gzip.NewWriter(&buffer)
	case "deflate":
		writer = zlib.NewWriter(&buffer)
	case "raw-deflate":
		var err error
		writer, err = flate.NewWriter(&buffer, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown test coding %s", coding)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestFetch_DecodesSupportedContentEncodings(t *testing.T) {
	for _, coding := range []string{"gzip", "deflate", "raw-deflate", "gzip, deflate", "identity"} {
		t.Run(coding, func(t *testing.T) {
			data := []byte("hello 世界 😀")
			header := coding
			switch coding {
			case "gzip, deflate":
				data = compressed(t, "deflate", compressed(t, "gzip", data))
			case "identity":
			default:
				data = compressed(t, coding, data)
				if coding == "raw-deflate" {
					header = "deflate"
				}
			}
			current := newFixture(t, func(writer http.ResponseWriter, request *http.Request) {
				if request.Header.Get("Accept-Encoding") != "gzip, deflate" {
					t.Errorf("Accept-Encoding=%q", request.Header.Get("Accept-Encoding"))
				}
				writer.Header().Set("Content-Type", "text/plain")
				writer.Header().Set("Content-Encoding", header)
				_, _ = writer.Write(data)
			})
			result, err := current.client.Fetch(t.Context(), "http://docs.example.test/")
			if err != nil || result.Content != "hello 世界 😀" || result.Truncated {
				t.Fatalf("content=%q truncated=%v err=%v", result.Content, result.Truncated, err)
			}
		})
	}
}

func TestFetch_RejectsUnadvertisedContentEncodings(t *testing.T) {
	for _, coding := range []string{"br", "zstd", "compress", "gzip, br", "deflate,"} {
		t.Run(coding, func(t *testing.T) {
			current := newFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/plain")
				writer.Header().Set("Content-Encoding", coding)
				_, _ = writer.Write([]byte{0x78, 0x9c, 0, 1})
			})
			_, err := current.client.Fetch(t.Context(), "http://docs.example.test/")
			if message := expectCode(t, err, web.CodeProviderError); !strings.Contains(message, "unsupported content encoding") {
				t.Fatalf("message=%q", message)
			}
		})
	}
}

func TestFetch_NormalizesURLBeforeResolutionAndRedirects(t *testing.T) {
	current := newFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Host != "xn--fsqu00a.com" {
			t.Errorf("Host=%q", request.Host)
		}
		if request.URL.Path == "/start" {
			writer.Header().Set("Location", "http://例子.com:80/a/../final")
			writer.WriteHeader(http.StatusFound)
			return
		}
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "normalized")
	})
	current.resolver.set("xn--fsqu00a.com", ips(publicIP))
	result, err := current.client.Fetch(t.Context(), " \thttp:例子.com/start\r\n ")
	if err != nil || result.URL != "http://xn--fsqu00a.com/final" || result.Content != "normalized" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	for _, host := range current.resolver.lookups() {
		if host != "xn--fsqu00a.com" {
			t.Fatalf("resolver received %q", host)
		}
	}
	if len(current.dialer.destinations()) != 2 {
		t.Fatal("every normalized hop must resolve and dial")
	}
}

func TestParseURL_NormalizesNormalURLs(t *testing.T) {
	for raw, want := range map[string]string{
		"http://example.com/a%2F世界#x%2f标题":          "http://example.com/a%2F%E4%B8%96%E7%95%8C#x%2f%E6%A0%87%E9%A2%98",
		"https://例子.com/a":                          "https://xn--fsqu00a.com/a",
		" \nHTTPS://EXAMPLE.com:0443\t ":            "https://example.com/",
		"http:example.com":                          "http://example.com/",
		"http://example.com:80/a/./b/../c":          "http://example.com/a/c",
		"http://example.com/a/%2e%2E/世界?q=中文 空格#标题": "http://example.com/%E4%B8%96%E7%95%8C?q=%E4%B8%AD%E6%96%87%20%E7%A9%BA%E6%A0%BC#%E6%A0%87%E9%A2%98",
		"http://example.com/a/..":                   "http://example.com/",
		"http://example.com/a/.":                    "http://example.com/a/",
		"http://example.com/../../a//b%2fc":         "http://example.com/a//b%2fc",
		"http://[::ffff:8.8.8.8]/":                  "http://[::ffff:808:808]/",
		"http://８.８.８.８/":                           "http://8.8.8.8/",
		"http://example.com:00081/":                 "http://example.com:81/",
	} {
		t.Run(raw, func(t *testing.T) {
			parsed, err := parseURL(raw)
			if err != nil {
				t.Fatal(err)
			}
			if parsed.String() != want {
				t.Fatalf("URL=%q want=%q", parsed.String(), want)
			}
		})
	}
}

func TestParseURL_RejectsAmbiguousOrMalformedSpellings(t *testing.T) {
	for _, raw := range []string{
		"http:/example.com", "http:///example.com", "http://example.com/a\\b",
		"http://exa\nmple.com/", "http://example.com/\x7f", "http://example.com/\xff",
		"http://a\u200db.com/", "http://[example.com]/", "http://exa_mple.com/",
		"http://127.1/", "http://2130706433/", "http://0x7f000001/", "http://0177.0.0.1/",
	} {
		t.Run(raw, func(t *testing.T) {
			_, err := parseURL(raw)
			expectCode(t, err, web.CodeInvalidURL)
		})
	}
}

func TestFetch_ValidatesMappedIDNAAddressesBeforeDialing(t *testing.T) {
	current := newFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("blocked address contacted") })
	current.resolver.set("xn--fsqu00a.com", ips("10.0.0.1"))
	for _, raw := range []string{"http://１２７.０.０.１/", "http:例子.com", "http://user:secret@例子.com/"} {
		_, err := current.client.Fetch(t.Context(), raw)
		expectCode(t, err, web.CodeBlockedURL)
	}
	if len(current.dialer.destinations()) != 0 {
		t.Fatal("normalized blocked addresses reached dialer")
	}
}

func TestParseURL_CountsUTF16Units(t *testing.T) {
	prefix := "https://example.com/"
	for _, tail := range []string{"界", "😀"} {
		units := 1
		if tail == "😀" {
			units = 2
		}
		raw := prefix + strings.Repeat(tail, (2048-len(prefix))/units)
		if units == 2 && (2048-len(prefix))%2 != 0 {
			raw += "a"
		}
		if _, err := parseURL(raw); err != nil {
			t.Errorf("exact UTF-16 boundary rejected: %v", err)
		}
		_, err := parseURL(raw + "a")
		expectCode(t, err, web.CodeInvalidURL)
	}
}

func TestFetch_TruncatesUTF16WithoutSplittingEmoji(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want string
		cut  bool
	}{
		{"exact", strings.Repeat("😀", 50_000), strings.Repeat("😀", 50_000), false},
		{"extra", strings.Repeat("😀", 50_001), strings.Repeat("😀", 50_000), true},
		{"split", strings.Repeat("a", 99_999) + "😀", strings.Repeat("a", 99_999), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := newFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(writer, test.body)
			})
			result, err := current.client.Fetch(t.Context(), "http://docs.example.test/")
			if err != nil || result.Content != test.want || result.Truncated != test.cut || !utf8.ValidString(result.Content) {
				t.Fatalf("bytes=%d truncated=%v err=%v", len(result.Content), result.Truncated, err)
			}
		})
	}
}

func TestFetch_FallsBackWhileFirstValidatedAddressIsBlocked(t *testing.T) {
	current := newFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "fallback")
	})
	current.resolver.set("docs.example.test", ips("2606:4700::1111", publicIP))
	started, stopped := make(chan struct{}), make(chan struct{})
	current.client.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "[2606:4700::1111]:80" {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil, ctx.Err()
		}
		<-started
		return current.dialer.dial(ctx, network, address)
	}
	// The deadline only bounds a broken implementation; progress requires the
	// second dial while the first remains blocked until its context is cancelled.
	current.client.timeout = 2 * time.Second
	result, err := current.client.Fetch(t.Context(), "http://docs.example.test/")
	if err != nil || result.Content != "fallback" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	select {
	case <-stopped:
	default:
		t.Fatal("fetch returned before losing dial stopped")
	}
}

func TestFetch_CancellationJoinsDialBeforeReturning(t *testing.T) {
	current := newFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("cancelled fetch sent HTTP") })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	late := trackedPipe(t)
	current.client.dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return late, nil
	}
	result := make(chan error, 1)
	ownerDone := make(chan struct{})
	go func() {
		defer close(ownerDone)
		_, err := current.client.Fetch(ctx, "http://docs.example.test/")
		result <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-release:
		default:
			close(release)
		}
		<-ownerDone
	})
	receiveDial(t, entered)
	cancel()
	receiveDial(t, cancelled)
	select {
	case <-result:
		t.Fatal("Fetch returned before dial cleanup")
	default:
	}
	close(release)
	err := receiveDial(t, result)
	expectCode(t, err, web.CodeAborted)
	if !errors.Is(err, context.Canceled) || !late.closed.Load() {
		t.Fatalf("err=%v late connection closed=%v", err, late.closed.Load())
	}
}

func TestFetch_IgnoresProxyEnvironment(t *testing.T) {
	proxy := newFixture(t, func(http.ResponseWriter, *http.Request) { t.Error("proxy was contacted") })
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, proxy.server.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	current := newFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "direct")
	})
	result, err := current.client.Fetch(t.Context(), "http://docs.example.test/")
	if err != nil || result.Content != "direct" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if got := current.dialer.destinations(); len(got) != 1 || got[0] != publicIP+":80" {
		t.Fatalf("dialed=%v", got)
	}
}
