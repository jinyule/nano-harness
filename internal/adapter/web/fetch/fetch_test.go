package fetch

import (
	"compress/gzip"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/app/web"
)

// publicIP is a globally routable address that tests map to a loopback server;
// the policy still validates it exactly as it would in production.
const publicIP = "93.184.216.34"

type fakeResolver struct {
	mu      sync.Mutex
	answers map[string][][]net.IPAddr
	errs    map[string]error
	block   bool
	calls   []string
}

func (resolver *fakeResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	resolver.mu.Lock()
	resolver.calls = append(resolver.calls, host)
	block, err := resolver.block, resolver.errs[host]
	queue := resolver.answers[host]
	var answer []net.IPAddr
	if len(queue) > 0 {
		answer = queue[0]
		if len(queue) > 1 {
			resolver.answers[host] = queue[1:]
		}
	}
	resolver.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return answer, err
}

func (resolver *fakeResolver) set(host string, answers ...[]net.IPAddr) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if resolver.answers == nil {
		resolver.answers = map[string][][]net.IPAddr{}
	}
	resolver.answers[host] = answers
}

func (resolver *fakeResolver) lookups() []string {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return append([]string(nil), resolver.calls...)
}

func ips(values ...string) []net.IPAddr {
	answers := make([]net.IPAddr, len(values))
	for index, value := range values {
		answers[index] = net.IPAddr{IP: net.ParseIP(value)}
	}
	return answers
}

// routedDialer records the validated destination and connects to a loopback listener instead.
type routedDialer struct {
	mu     sync.Mutex
	target string
	fail   map[string]error
	dialed []string
}

func (dialer *routedDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	dialer.mu.Lock()
	dialer.dialed = append(dialer.dialed, address)
	err := dialer.fail[address]
	dialer.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, network, dialer.target)
}

func (dialer *routedDialer) failAt(address string, err error) {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	if dialer.fail == nil {
		dialer.fail = map[string]error{}
	}
	dialer.fail[address] = err
}

func (dialer *routedDialer) destinations() []string {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	return append([]string(nil), dialer.dialed...)
}

type fixture struct {
	client   *Client
	resolver *fakeResolver
	dialer   *routedDialer
	server   *httptest.Server
}

func newFixture(t *testing.T, handler http.HandlerFunc) *fixture {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	resolver := &fakeResolver{}
	resolver.set("docs.example.test", ips(publicIP))
	dialer := &routedDialer{target: server.Listener.Addr().String()}
	return &fixture{client: New(Config{Resolver: resolver, Dial: dialer.dial}), resolver: resolver, dialer: dialer, server: server}
}

// expectCode asserts the structured code and returns the model-visible message.
func expectCode(t *testing.T, err error, code web.Code) string {
	t.Helper()
	var failure *web.Error
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("error=%v, want %s", err, code)
	}
	return failure.Message
}

func TestFetch_RetrievesThroughValidatedPinnedAddress(t *testing.T) {
	var seen http.Header
	var host string
	current := newFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		seen, host = request.Header.Clone(), request.Host
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(writer, "\ufeff<h1>Docs</h1>")
	})
	result, err := current.client.Fetch(context.Background(), "http://docs.example.test/guide?q=1#part")
	if err != nil {
		t.Fatal(err)
	}
	want := web.FetchResult{URL: "http://docs.example.test/guide?q=1#part", StatusCode: 200, Kind: web.FetchHTML, Content: "<h1>Docs</h1>"}
	if result != want {
		t.Fatalf("result=%#v", result)
	}
	if got := current.dialer.destinations(); len(got) != 1 || got[0] != publicIP+":80" {
		t.Fatalf("dialed=%v", got)
	}
	if host != "docs.example.test" || seen.Get("User-Agent") != userAgent || seen.Get("Accept") != acceptHeader {
		t.Fatalf("host=%q headers=%v", host, seen)
	}
	for _, name := range []string{"Cookie", "Authorization", "Proxy-Authorization"} {
		if seen.Get(name) != "" {
			t.Fatalf("anonymous fetch sent %s", name)
		}
	}
}

func TestFetch_ReturnsNonSuccessStatusAsResult(t *testing.T) {
	current := newFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		writer.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(writer, "missing")
	})
	result, err := current.client.Fetch(context.Background(), "http://docs.example.test/missing")
	if err != nil || result.StatusCode != http.StatusNotFound || result.Content != "missing" || result.Kind != web.FetchText {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestFetch_DecodesDeclaredCharsetAndCompression(t *testing.T) {
	gbk := []byte{0xd6, 0xd0, 0xce, 0xc4} // "中文" in GBK
	current := newFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/gbk":
			writer.Header().Set("Content-Type", `text/plain; charset="GBK"`)
			_, _ = writer.Write(gbk)
		case "/gzip":
			if !strings.Contains(request.Header.Get("Accept-Encoding"), "gzip") {
				http.Error(writer, "gzip not offered", http.StatusBadRequest)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Content-Encoding", "gzip")
			compressed := gzip.NewWriter(writer)
			_, _ = io.WriteString(compressed, `{"ok":true}`)
			_ = compressed.Close()
		case "/invalid-utf8":
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = writer.Write([]byte{'a', 0xff, 'b'})
		}
	})
	for path, want := range map[string]string{"/gbk": "中文", "/gzip": `{"ok":true}`, "/invalid-utf8": "a\ufffdb"} {
		result, err := current.client.Fetch(context.Background(), "http://docs.example.test"+path)
		if err != nil || result.Content != want || result.Truncated {
			t.Fatalf("%s result=%#v err=%v", path, result, err)
		}
	}
}

func TestFetch_RejectsUnsupportedContentAndDeclaredOversize(t *testing.T) {
	current := newFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/png":
			writer.Header().Set("Content-Type", "image/png")
		case "/untyped":
			writer.Header()["Content-Type"] = nil
		case "/charset":
			writer.Header().Set("Content-Type", "text/plain; charset=x-unknown")
		case "/large":
			writer.Header().Set("Content-Type", "text/plain")
			writer.Header().Set("Content-Length", fmt.Sprint(maxResponseBytes+1))
			_, _ = io.WriteString(writer, "partial")
		}
	})
	for path, code := range map[string]web.Code{
		"/png": web.CodeUnsupportedContent, "/untyped": web.CodeUnsupportedContent,
		"/charset": web.CodeUnsupportedContent, "/large": web.CodeFetchTooLarge,
	} {
		_, err := current.client.Fetch(context.Background(), "http://docs.example.test"+path)
		message := expectCode(t, err, code)
		if path == "/untyped" && !strings.Contains(message, `"unknown"`) {
			t.Fatalf("untyped message=%q", message)
		}
	}
}

func TestFetch_TruncatesStreamedBytesDecompressedBytesAndRunes(t *testing.T) {
	current := newFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		switch request.URL.Path {
		case "/exact-runes":
			_, _ = io.WriteString(writer, strings.Repeat("界", maxBodyRunes))
		case "/extra-rune":
			_, _ = io.WriteString(writer, strings.Repeat("界", maxBodyRunes)+"!")
		case "/stream":
			// Chunked encoding hides the size until the body exceeds the byte cap.
			writer.(http.Flusher).Flush()
			_, _ = io.WriteString(writer, strings.Repeat("x", maxResponseBytes+1))
		case "/bomb":
			writer.Header().Set("Content-Encoding", "gzip")
			compressed := gzip.NewWriter(writer)
			_, _ = compressed.Write(make([]byte, maxResponseBytes*2))
			_ = compressed.Close()
		}
	})
	exact, err := current.client.Fetch(context.Background(), "http://docs.example.test/exact-runes")
	if err != nil || exact.Truncated || len([]rune(exact.Content)) != maxBodyRunes {
		t.Fatalf("exact runes truncated=%v len=%d err=%v", exact.Truncated, len([]rune(exact.Content)), err)
	}
	extra, err := current.client.Fetch(context.Background(), "http://docs.example.test/extra-rune")
	if err != nil || !extra.Truncated || extra.Content != exact.Content {
		t.Fatalf("extra rune truncated=%v err=%v", extra.Truncated, err)
	}
	for _, path := range []string{"/stream", "/bomb"} {
		result, err := current.client.Fetch(context.Background(), "http://docs.example.test"+path)
		if err != nil || !result.Truncated || len([]rune(result.Content)) != maxBodyRunes {
			t.Fatalf("%s truncated=%v len=%d err=%v", path, result.Truncated, len(result.Content), err)
		}
	}
}

func TestCappedReader_DistinguishesExactAndOversizedBodies(t *testing.T) {
	exact := &cappedReader{source: strings.NewReader("abcd"), remaining: 4}
	if data, err := io.ReadAll(exact); err != nil || string(data) != "abcd" || exact.truncated {
		t.Fatalf("exact=%q truncated=%v err=%v", data, exact.truncated, err)
	}
	longer := &cappedReader{source: strings.NewReader("abcde"), remaining: 4}
	if data, err := io.ReadAll(longer); err != nil || string(data) != "abcd" || !longer.truncated {
		t.Fatalf("longer=%q truncated=%v err=%v", data, longer.truncated, err)
	}
	failure := errors.New("read")
	failing := &cappedReader{source: io.MultiReader(strings.NewReader("ab"), errorReader{failure}), remaining: 2}
	if _, err := io.ReadAll(failing); !errors.Is(err, failure) {
		t.Fatalf("probe error=%v", err)
	}
}

type errorReader struct{ err error }

func (reader errorReader) Read([]byte) (int, error) { return 0, reader.err }

func TestFetch_FollowsSameOriginRedirectsWithFreshValidation(t *testing.T) {
	current := newFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/start":
			writer.Header().Set("Location", "/middle")
			writer.WriteHeader(http.StatusFound)
		case "/middle":
			http.Redirect(writer, request, "http://DOCS.example.test:80/final", http.StatusPermanentRedirect)
		case "/final":
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(writer, "arrived")
		}
	})
	result, err := current.client.Fetch(context.Background(), "http://docs.example.test/start")
	if err != nil || result.Content != "arrived" || result.URL != "http://DOCS.example.test:80/final" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if lookups := current.resolver.lookups(); len(lookups) != 3 {
		t.Fatalf("each hop must resolve again: %v", lookups)
	}
	if dialed := current.dialer.destinations(); len(dialed) != 3 {
		t.Fatalf("each hop must dial a validated address: %v", dialed)
	}
}

func TestFetch_RefusesUnsafeRedirects(t *testing.T) {
	var foreign atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { foreign.Add(1) }))
	t.Cleanup(other.Close)
	current := newFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		hop := 0
		_, _ = fmt.Sscanf(request.URL.Query().Get("hop"), "%d", &hop)
		switch request.URL.Path {
		case "/cross":
			http.Redirect(writer, request, other.URL+"/steal", http.StatusFound)
		case "/scheme":
			http.Redirect(writer, request, "https://docs.example.test/secure", http.StatusMovedPermanently)
		case "/loop":
			http.Redirect(writer, request, fmt.Sprintf("/loop?hop=%d", hop+1), http.StatusSeeOther)
		case "/chain":
			if hop < maxRedirects {
				http.Redirect(writer, request, fmt.Sprintf("/chain?hop=%d", hop+1), http.StatusTemporaryRedirect)
				return
			}
			writer.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(writer, "five hops")
		case "/nolocation":
			writer.WriteHeader(http.StatusFound)
		case "/badlocation":
			writer.Header().Set("Location", "http://[::1")
			writer.WriteHeader(http.StatusFound)
		case "/credentials":
			writer.Header().Set("Location", "http://user:pass@docs.example.test/")
			writer.WriteHeader(http.StatusFound)
		case "/ftp":
			writer.Header().Set("Location", "ftp://docs.example.test/")
			writer.WriteHeader(http.StatusFound)
		}
	})
	for path, code := range map[string]web.Code{
		"/cross": web.CodeRedirectBlocked, "/scheme": web.CodeRedirectBlocked, "/loop": web.CodeRedirectBlocked,
		"/nolocation": web.CodeProviderError, "/badlocation": web.CodeProviderError,
		"/credentials": web.CodeBlockedURL, "/ftp": web.CodeInvalidURL,
	} {
		_, err := current.client.Fetch(context.Background(), "http://docs.example.test"+path)
		message := expectCode(t, err, code)
		if path == "/cross" && !strings.Contains(message, "retry against that URL directly") {
			t.Fatalf("cross-origin message=%q", message)
		}
	}
	if foreign.Load() != 0 {
		t.Fatal("cross-origin redirect target was contacted")
	}
	result, err := current.client.Fetch(context.Background(), "http://docs.example.test/chain")
	if err != nil || result.Content != "five hops" {
		t.Fatalf("five redirects result=%#v err=%v", result, err)
	}
}

func TestFetch_RejectsRebindingOnRedirectHop(t *testing.T) {
	current := newFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, "/internal", http.StatusFound)
	})
	current.resolver.set("docs.example.test", ips(publicIP), ips("10.0.0.7"))
	_, err := current.client.Fetch(context.Background(), "http://docs.example.test/start")
	expectCode(t, err, web.CodeBlockedURL)
	if dialed := current.dialer.destinations(); len(dialed) != 1 {
		t.Fatalf("rebound hop was dialed: %v", dialed)
	}
}

func TestFetch_AddressPolicyMatrix(t *testing.T) {
	var contacted atomic.Int32
	current := newFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		contacted.Add(1)
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "ok")
	})
	resolver := current.resolver
	resolver.set("private.example.test", ips(publicIP, "192.168.1.10"))
	resolver.set("metadata.example.test", ips("169.254.169.254"))
	resolver.set("mapped.example.test", ips("::ffff:127.0.0.1"))
	resolver.set("empty.example.test", nil)
	resolver.set("invalid.example.test", []net.IPAddr{{IP: net.IP{1, 2, 3}}})
	resolver.set("nat64.example.test", ips("2600:1:122:344:a:0:100:0"))
	resolver.set("ipv6.example.test", ips("2606:4700::1111"))
	resolver.set(nat64DiscoveryHost, ips("192.0.0.170", "2600:1:122:344:c0:0:aa00:0"))
	resolver.errs = map[string]error{"missing.example.test": errors.New("no such host")}
	for target, code := range map[string]web.Code{
		"http://127.0.0.1/":             web.CodeBlockedURL,
		"http://[::1]/":                 web.CodeBlockedURL,
		"http://[fe80::1%25en0]/":       web.CodeBlockedURL,
		"http://private.example.test/":  web.CodeBlockedURL,
		"http://metadata.example.test/": web.CodeBlockedURL,
		"http://mapped.example.test/":   web.CodeBlockedURL,
		"http://invalid.example.test/":  web.CodeBlockedURL,
		"http://nat64.example.test/":    web.CodeBlockedURL,
		"http://empty.example.test/":    web.CodeProviderError,
		"http://missing.example.test/":  web.CodeProviderError,
	} {
		_, err := current.client.Fetch(context.Background(), target)
		expectCode(t, err, code)
	}
	if contacted.Load() != 0 || len(current.dialer.destinations()) != 0 {
		t.Fatalf("refused destinations were contacted: %v", current.dialer.destinations())
	}
	for target, destination := range map[string]string{
		"http://" + publicIP + ":8080/": publicIP + ":8080",
		"http://ipv6.example.test/":     "[2606:4700::1111]:80",
	} {
		before := len(current.dialer.destinations())
		if result, err := current.client.Fetch(context.Background(), target); err != nil || result.Content != "ok" {
			t.Fatalf("%s result=%#v err=%v", target, result, err)
		}
		if dialed := current.dialer.destinations()[before:]; len(dialed) != 1 || dialed[0] != destination {
			t.Fatalf("%s dialed %v", target, dialed)
		}
	}
	resolver.errs[nat64DiscoveryHost] = errors.New("discovery failed")
	_, err := current.client.Fetch(context.Background(), "http://ipv6.example.test/")
	expectCode(t, err, web.CodeProviderError)
}

func TestFetch_FailsOverAcrossValidatedAddresses(t *testing.T) {
	current := newFixture(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "second")
	})
	current.resolver.set("docs.example.test", ips("8.8.8.8", publicIP))
	current.dialer.failAt("8.8.8.8:80", errors.New("connection refused"))
	result, err := current.client.Fetch(context.Background(), "http://docs.example.test/")
	if err != nil || result.Content != "second" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	current.dialer.failAt(publicIP+":80", errors.New("connection refused"))
	_, err = current.client.Fetch(context.Background(), "http://docs.example.test/")
	if message := expectCode(t, err, web.CodeProviderError); !strings.Contains(message, "connection refused") {
		t.Fatalf("message=%q", message)
	}
}

func TestFetch_ClassifiesDeadlineCancellationAndBrokenBodies(t *testing.T) {
	released := make(chan struct{})
	current := newFixture(t, func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/slow":
			select {
			case <-request.Context().Done():
			case <-released:
			}
		case "/broken":
			writer.Header().Set("Content-Type", "text/plain")
			writer.Header().Set("Content-Length", "100")
			_, _ = io.WriteString(writer, "short")
		}
	})
	// Registered after the server, so handlers are released before Close waits for them.
	t.Cleanup(func() { close(released) })
	current.client.timeout = 50 * time.Millisecond
	_, err := current.client.Fetch(context.Background(), "http://docs.example.test/slow")
	expectCode(t, err, web.CodeFetchTimeout)

	current.resolver.block = true
	_, err = current.client.Fetch(context.Background(), "http://docs.example.test/slow")
	expectCode(t, err, web.CodeFetchTimeout)
	current.resolver.block = false

	current.client.timeout = fetchTimeout
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	_, err = current.client.Fetch(ctx, "http://docs.example.test/slow")
	expectCode(t, err, web.CodeAborted)

	_, err = current.client.Fetch(context.Background(), "http://docs.example.test/broken")
	if message := expectCode(t, err, web.CodeProviderError); !strings.Contains(message, "body read failed") {
		t.Fatalf("message=%q", message)
	}
	_, err = current.client.Fetch(context.Background(), "ftp://docs.example.test/")
	expectCode(t, err, web.CodeInvalidURL)
}

func TestFetch_VerifiesTLSAgainstURLHostname(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(writer, "secure")
	}))
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	resolver := &fakeResolver{}
	resolver.set("example.com", ips(publicIP))
	resolver.set("other.example", ips(publicIP))
	dialer := &routedDialer{target: server.Listener.Addr().String()}
	client := New(Config{Resolver: resolver, Dial: dialer.dial})
	client.roots = roots
	// The test certificate names example.com, so verification must use the URL
	// hostname even though the connection is pinned to an IP address.
	result, err := client.Fetch(context.Background(), "https://example.com/")
	if err != nil || result.Content != "secure" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if dialed := dialer.destinations(); dialed[0] != publicIP+":443" {
		t.Fatalf("dialed=%v", dialed)
	}
	_, err = client.Fetch(context.Background(), "https://other.example/")
	expectCode(t, err, web.CodeProviderError)
}

func TestNew_ProductionDefaultsRefuseLoopback(t *testing.T) {
	var contacted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { contacted.Add(1) }))
	t.Cleanup(server.Close)
	address, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := New(Config{})
	if client.resolver != net.DefaultResolver || client.dial == nil || client.roots != nil || client.timeout != fetchTimeout {
		t.Fatal("production defaults")
	}
	for _, target := range []string{server.URL, "http://localhost:" + address.Port() + "/"} {
		_, err := client.Fetch(context.Background(), target)
		expectCode(t, err, web.CodeBlockedURL)
	}
	if contacted.Load() != 0 {
		t.Fatal("production policy contacted a loopback server")
	}
}

func TestRedirectTarget_RejectsUnparsableLocation(t *testing.T) {
	// net/http normally rejects this before CheckRedirect; the policy still owns the check.
	current, _ := parseURL("http://docs.example.test/")
	response := &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"http://[::1"}}}
	_, err := redirectTarget(response, current, 0)
	expectCode(t, err, web.CodeProviderError)
}

func TestRedirectStatus(t *testing.T) {
	for status, want := range map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true, 300: false, 304: false, 200: false} {
		if redirectStatus(status) != want {
			t.Errorf("status %d", status)
		}
	}
	if truncated, cut := truncateRunes("é", 1); truncated != "é" || cut {
		t.Fatal("rune boundary")
	}
}
