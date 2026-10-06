// Package fetch retrieves public HTTP(S) resources for the web service. Every
// hop resolves its host, refuses any non-public answer, and connects only to the
// validated addresses, so DNS cannot redirect a connection to a private service.
package fetch

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html/charset"
	"golang.org/x/text/transform"

	"github.com/jinyule/nano-harness/internal/app/web"
)

// Fixed transport and response limits. They are safety bounds, not deployment settings.
const (
	fetchTimeout     = 30 * time.Second
	dialTimeout      = 10 * time.Second
	maxRedirects     = 5
	maxResponseBytes = 5_000_000
	maxBodyUnits     = 100_000
	fallbackDelay    = 250 * time.Millisecond
	maxHeaderBytes   = 64 << 10
	userAgent        = "nano-harness (+https://github.com/jinyule/nano-harness)"
	acceptHeader     = "text/html,application/xhtml+xml,text/*;q=0.9,application/json;q=0.8"
)

var errFetchTimeout = errors.New("web fetch deadline")

// Resolver answers the addresses of one hostname.
type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

// DialFunc opens one TCP connection to a validated IP:port. It must support
// concurrent calls and return promptly when ctx is cancelled.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Config injects the network boundary. Nil fields select the system resolver and
// a direct dialer. Injection never bypasses policy: every resolver answer is
// validated and the dialer only receives validated IP:port pairs.
type Config struct {
	Resolver Resolver
	Dial     DialFunc
}

// Client is an anonymous public HTTP(S) fetcher. It sends no cookies or
// credentials, ignores proxy environment variables, and owns no connection pool:
// each hop uses a dedicated transport closed when the hop ends.
type Client struct {
	resolver Resolver
	dial     DialFunc
	roots    *x509.CertPool // nil selects the system roots
	timeout  time.Duration
}

// New constructs a fetcher over the configured network boundary.
func New(config Config) *Client {
	client := &Client{resolver: config.Resolver, dial: config.Dial, timeout: fetchTimeout}
	if client.resolver == nil {
		client.resolver = net.DefaultResolver
	}
	if client.dial == nil {
		client.dial = (&net.Dialer{Timeout: dialTimeout}).DialContext
	}
	return client
}

// Fetch retrieves one URL, following at most five same-origin redirects. Each
// redirect target passes the complete URL and address policy again.
func (client *Client) Fetch(ctx context.Context, rawURL string) (web.FetchResult, error) {
	current, err := parseURL(rawURL)
	if err != nil {
		return web.FetchResult{}, err
	}
	ctx, cancel := context.WithTimeoutCause(ctx, client.timeout, errFetchTimeout)
	defer cancel()
	for followed := 0; ; followed++ {
		response, transport, err := client.request(ctx, current)
		if err != nil {
			return web.FetchResult{}, err
		}
		var (
			next   *url.URL
			result web.FetchResult
		)
		if redirectStatus(response.StatusCode) {
			next, err = redirectTarget(response, current, followed)
		} else {
			result, err = client.read(ctx, response, current)
		}
		// Each hop owns exactly one connection; closing the body and the
		// transport leaves nothing pooled for a later hop to reuse.
		_ = response.Body.Close()
		transport.CloseIdleConnections()
		if err != nil || next == nil {
			return result, err
		}
		current = next
	}
}

func redirectStatus(status int) bool {
	switch status {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	default:
		return false
	}
}

// redirectTarget enforces the hop budget before validating the next URL; a
// cross-origin target requires a new tool call against that URL.
func redirectTarget(response *http.Response, current *url.URL, followed int) (*url.URL, error) {
	if followed >= maxRedirects {
		return nil, &web.Error{Code: web.CodeRedirectBlocked, Message: fmt.Sprintf("exceeded the maximum of %d redirects", maxRedirects)}
	}
	location := response.Header.Get("Location")
	if location == "" {
		return nil, &web.Error{Code: web.CodeProviderError, Message: fmt.Sprintf("redirect response (HTTP %d) without a Location header", response.StatusCode)}
	}
	target, err := current.Parse(location)
	if err != nil {
		return nil, &web.Error{Code: web.CodeProviderError, Message: fmt.Sprintf("invalid redirect Location %q", location), Cause: err}
	}
	next, err := parseURL(target.String())
	if err != nil {
		return nil, err
	}
	if !sameOrigin(current, next) {
		return nil, &web.Error{Code: web.CodeRedirectBlocked, Message: fmt.Sprintf("cross-origin redirect to %s is not followed automatically; retry against that URL directly", origin(next))}
	}
	return next, nil
}

// request sends one GET over a transport pinned to the hop's validated
// addresses. The caller closes the body and then the transport.
func (client *Client) request(ctx context.Context, target *url.URL) (*http.Response, *http.Transport, error) {
	addresses, err := client.resolve(ctx, strings.ToLower(target.Hostname()))
	if err != nil {
		return nil, nil, err
	}
	port := portOf(target)
	// Dial synchronously under the fetch owner. Transport may return from a
	// cancelled request before its own DialContext callback has finished.
	ticker := time.NewTicker(fallbackDelay)
	connection, err := client.dialPinned(ctx, "tcp", addresses, port, ticker.C)
	ticker.Stop()
	if err != nil {
		return nil, nil, client.failure(ctx, "web fetch failed", err)
	}
	transport := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return connection, nil
		},
		TLSClientConfig:        &tls.Config{RootCAs: client.roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout:    dialTimeout,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: maxHeaderBytes,
	}
	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	request := (&http.Request{
		Method: http.MethodGet, URL: target, Host: target.Host,
		Header: http.Header{"User-Agent": {userAgent}, "Accept": {acceptHeader}, "Accept-Encoding": {"gzip, deflate"}},
	}).WithContext(ctx)
	response, err := httpClient.Do(request)
	if err != nil {
		_ = connection.Close()
		transport.CloseIdleConnections()
		return nil, nil, client.failure(ctx, "web fetch failed", err)
	}
	return response, transport, nil
}

// resolve returns the validated destination set for host: an IP literal as
// stated, otherwise every resolver answer. Literals and answers pass the same
// policy. Any non-public address rejects the whole set, as does an IPv6
// address that a discovered NAT64 prefix translates to a non-public IPv4
// destination; a literal inside a network-specific prefix reaches that IPv4
// address just as a resolver answer would.
func (client *Client) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	var addresses []netip.Addr
	if literal, err := netip.ParseAddr(host); err == nil {
		addresses = []netip.Addr{literal.Unmap()}
	} else if addresses, err = client.lookup(ctx, host); err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, &web.Error{Code: web.CodeProviderError, Message: fmt.Sprintf("hostname %q resolved to no addresses", host)}
	}
	hasIPv6 := false
	for _, address := range addresses {
		if !address.IsValid() {
			return nil, &web.Error{Code: web.CodeProviderError, Message: fmt.Sprintf("hostname %q resolved to an invalid IP address", host)}
		}
		if !publicAddress(address) {
			return nil, &web.Error{Code: web.CodeBlockedURL, Message: fmt.Sprintf("URL hostname %q resolves to a non-public IP address", host)}
		}
		hasIPv6 = hasIPv6 || address.Is6()
	}
	if !hasIPv6 {
		return addresses, nil
	}
	discovered, err := client.lookup(ctx, nat64DiscoveryHost)
	if err != nil {
		return nil, err
	}
	active := nat64Prefixes(discovered)
	for _, address := range addresses {
		if embedded, ok := translatedIPv4(address, active); ok && !publicAddress(embedded) {
			return nil, &web.Error{Code: web.CodeBlockedURL, Message: fmt.Sprintf("URL hostname %q resolves through NAT64 to a non-public IPv4 address", host)}
		}
	}
	return addresses, nil
}

// lookup returns resolver answers with IPv4-mapped forms unmapped. An answer
// that is not an IP address becomes the invalid zero Addr, which resolve
// rejects and NAT64 discovery skips.
func (client *Client) lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	answers, err := client.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, client.failure(ctx, fmt.Sprintf("hostname %q could not be resolved", host), err)
	}
	addresses := make([]netip.Addr, len(answers))
	for index, answer := range answers {
		address, _ := netip.AddrFromSlice(answer.IP)
		addresses[index] = address.Unmap().WithZone(answer.Zone)
	}
	return addresses, nil
}

// read classifies, bounds, and decodes the final response body. A declared
// oversized body fails; a body that grows past the limit is truncated.
func (client *Client) read(ctx context.Context, response *http.Response, final *url.URL) (web.FetchResult, error) {
	contentType := response.Header.Get("Content-Type")
	kind, ok := classify(contentType)
	if !ok {
		shown := contentType
		if shown == "" {
			shown = "unknown"
		}
		return web.FetchResult{}, &web.Error{Code: web.CodeUnsupportedContent, Message: fmt.Sprintf("unsupported content type %q", shown)}
	}
	label := charsetLabel(contentType)
	if label == "" {
		label = "utf-8"
	}
	encoding, _ := charset.Lookup(label)
	if encoding == nil {
		return web.FetchResult{}, &web.Error{Code: web.CodeUnsupportedContent, Message: fmt.Sprintf("unsupported charset %q", label)}
	}
	if response.ContentLength > maxResponseBytes {
		return web.FetchResult{}, &web.Error{Code: web.CodeFetchTooLarge, Message: fmt.Sprintf("response exceeds the maximum of %d bytes", maxResponseBytes)}
	}
	source, decoders, err := decompress(ctx, response.Body, strings.Join(response.Header.Values("Content-Encoding"), ","))
	defer func() {
		for _, decoder := range decoders {
			_ = decoder.Close()
		}
	}()
	if err != nil {
		return web.FetchResult{}, client.failure(ctx, "web fetch decompression failed", err)
	}
	body := &cappedReader{source: source, remaining: maxResponseBytes}
	decoded, err := io.ReadAll(transform.NewReader(body, encoding.NewDecoder()))
	if err != nil {
		return web.FetchResult{}, client.failure(ctx, "web fetch body read failed", err)
	}
	content := strings.TrimPrefix(string(decoded), "\ufeff")
	content, cut := truncateUTF16(content, maxBodyUnits)
	return web.FetchResult{URL: final.String(), StatusCode: response.StatusCode, Kind: kind, Content: content, Truncated: body.truncated || cut}, nil
}

// failure classifies an I/O error by the fetch context: its own deadline,
// caller or shutdown cancellation, then decompression limits or other failures.
func (client *Client) failure(ctx context.Context, message string, err error) error {
	switch {
	case errors.Is(context.Cause(ctx), errFetchTimeout):
		return &web.Error{Code: web.CodeFetchTimeout, Message: fmt.Sprintf("web fetch timed out after %s", client.timeout), Cause: err}
	case ctx.Err() != nil:
		return &web.Error{Code: web.CodeAborted, Message: "web fetch was cancelled", Cause: err}
	case errors.Is(err, errDecompressionLimit):
		return &web.Error{Code: web.CodeFetchTooLarge, Message: err.Error(), Cause: err}
	default:
		return &web.Error{Code: web.CodeProviderError, Message: message + ": " + err.Error(), Cause: err}
	}
}

// cappedReader passes at most remaining bytes, then probes one more byte so an
// exactly full body is not mistaken for a truncated one.
type cappedReader struct {
	source    io.Reader
	remaining int64
	truncated bool
}

func (reader *cappedReader) Read(buffer []byte) (int, error) {
	if reader.remaining <= 0 {
		var probe [1]byte
		count, err := reader.source.Read(probe[:])
		if count > 0 {
			reader.truncated = true
			return 0, io.EOF
		}
		return 0, err
	}
	if int64(len(buffer)) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	count, err := reader.source.Read(buffer)
	reader.remaining -= int64(count)
	return count, err
}

// classify maps a Content-Type to a decodable kind; binary types are unsupported.
func classify(contentType string) (web.FetchKind, bool) {
	mediaType, _, _ := strings.Cut(contentType, ";")
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	switch {
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		return web.FetchHTML, true
	case strings.HasPrefix(mediaType, "text/"),
		mediaType == "application/json", mediaType == "application/xml",
		strings.HasSuffix(mediaType, "+json"), strings.HasSuffix(mediaType, "+xml"):
		return web.FetchText, true
	default:
		return "", false
	}
}

// charsetLabel returns the lower-cased charset parameter, or empty when absent.
func charsetLabel(contentType string) string {
	parameters := strings.Split(contentType, ";")
	for _, parameter := range parameters[1:] {
		key, value, ok := strings.Cut(parameter, "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "charset") {
			return strings.ToLower(strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `"`)))
		}
	}
	return ""
}

// truncateUTF16 bounds UTF-16 code units without splitting a Unicode scalar.
func truncateUTF16(value string, limit int) (string, bool) {
	count := 0
	for index, scalar := range value {
		count++
		if scalar > 0xffff {
			count++
		}
		if count > limit {
			return value[:index], true
		}
	}
	return value, false
}
