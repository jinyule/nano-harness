package fetch

import (
	"errors"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/web"
)

func TestPublicAddress_AllowsOnlyGlobalUnicast(t *testing.T) {
	public := []string{
		"1.0.0.0", "8.8.8.8", "9.255.255.255", "11.0.0.0", "93.184.216.34", "100.63.255.255", "100.128.0.0",
		"172.15.255.255", "172.32.0.0", "192.0.1.255", "192.169.0.0", "198.17.255.255", "198.20.0.0", "223.255.255.255",
		"::ffff:8.8.8.8", "2001:200::1", "2001:4860:4860::8888", "2606:4700:4700::1111", "2620:4f:7fff::1", "3ffe::1",
	}
	nonPublic := []string{
		"0.0.0.0", "0.1.2.3", "10.0.0.1", "10.255.255.255", "100.64.0.1", "100.127.255.255", "127.0.0.1", "127.255.255.254",
		"169.254.169.254", "172.16.0.1", "172.31.255.255", "192.0.0.170", "192.0.2.1", "192.31.196.1", "192.52.193.1",
		"192.88.99.1", "192.168.0.1", "192.175.48.1", "198.18.0.1", "198.19.255.255", "198.51.100.1", "203.0.113.1",
		"224.0.0.1", "239.255.255.250", "240.0.0.1", "255.255.255.255",
		"::", "::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::1.2.3.4", "64:ff9b::a00:1",
		"64:ff9b:1::1", "100::1", "fc00::1", "fd12:3456::1", "fe80::1", "fe80::1%en0", "fec0::1", "ff02::1", "2001::1",
		"2001:1ff::1", "2001:db8::1", "2002:c000:204::1", "2620:4f:8000::1", "3fff::1", "4000::1",
	}
	for _, value := range public {
		if !publicAddress(netip.MustParseAddr(value)) {
			t.Errorf("%s was refused", value)
		}
	}
	for _, value := range nonPublic {
		if publicAddress(netip.MustParseAddr(value)) {
			t.Errorf("%s was allowed", value)
		}
	}
	if publicAddress(netip.Addr{}) {
		t.Fatal("invalid address was allowed")
	}
}

func TestEmbeddedIPv4_DecodesEveryRFC6052Layout(t *testing.T) {
	// RFC 6052 section 2.4 examples embedding 192.0.2.33.
	want := netip.MustParseAddr("192.0.2.33")
	for bits, value := range map[int]string{
		32: "2001:db8:c000:221::", 40: "2001:db8:1c0:2:21::", 48: "2001:db8:122:c000:2:2100::",
		56: "2001:db8:122:3c0:0:221::", 64: "2001:db8:122:344:c0:2:2100:0", 96: "2001:db8:122:344::192.0.2.33",
	} {
		if got, ok := embeddedIPv4(netip.MustParseAddr(value), bits); !ok || got != want {
			t.Errorf("/%d %s embedded %s ok=%v", bits, value, got, ok)
		}
	}
	if _, ok := embeddedIPv4(netip.MustParseAddr("2001:db8:122:344:ffc0:2:2100:0"), 64); ok {
		t.Fatal("non-zero u octet accepted")
	}
}

func TestNAT64Prefixes_DiscoversSentinelLayoutsOnly(t *testing.T) {
	answers := []netip.Addr{
		netip.MustParseAddr("192.0.0.170"),
		netip.MustParseAddr("::ffff:192.0.0.171"),
		netip.MustParseAddr("64:ff9b::c000:aa"),
		netip.MustParseAddr("64:ff9b::c000:ab"),
		netip.MustParseAddr("2600:1:122:344:c0:0:aa00:0"),
		netip.MustParseAddr("2600:1:122:344::808:808"),
	}
	want := []netip.Prefix{netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("2600:1:122:344::/64")}
	active := nat64Prefixes(answers)
	if !reflect.DeepEqual(active, want) {
		t.Fatalf("prefixes=%v", active)
	}
	if got, ok := translatedIPv4(netip.MustParseAddr("2600:1:122:344:a:0:100:0"), active); !ok || got != netip.MustParseAddr("10.0.0.1") {
		t.Fatalf("translated=%s ok=%v", got, ok)
	}
	if _, ok := translatedIPv4(netip.MustParseAddr("2600:2::1"), active); ok {
		t.Fatal("address outside the active prefixes was translated")
	}
}

func TestParseURL_EnforcesNetworkIndependentPolicy(t *testing.T) {
	for _, value := range []string{"http://example.com/path?q=1#frag", "https://example.com:8443/", "http://[2606:4700::1]/", "HTTP://Example.COM"} {
		if _, err := parseURL(value); err != nil {
			t.Errorf("%s rejected: %v", value, err)
		}
	}
	for _, test := range []struct {
		value string
		code  web.Code
	}{
		{"https://example.com/" + strings.Repeat("a", maxURLUnits), web.CodeInvalidURL},
		{"http://%zz", web.CodeInvalidURL},
		{"ftp://example.com/", web.CodeInvalidURL},
		{"file:///etc/passwd", web.CodeInvalidURL},
		{"javascript:alert(1)", web.CodeInvalidURL},
		{"example.com", web.CodeInvalidURL},
		{"http:///path", web.CodeInvalidURL},
		{"http://example.com:0/", web.CodeInvalidURL},
		{"http://example.com:70000/", web.CodeInvalidURL},
		{"https://user:secret@example.com/", web.CodeBlockedURL},
		{"https://user@example.com/", web.CodeBlockedURL},
	} {
		_, err := parseURL(test.value)
		var failure *web.Error
		if !errors.As(err, &failure) || failure.Code != test.code {
			t.Errorf("%s error=%v, want %s", test.value, err, test.code)
		}
	}
}

func TestOrigin_ComparesSchemeHostAndEffectivePort(t *testing.T) {
	parse := func(value string) *url.URL {
		parsed, err := parseURL(value)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	same := [][2]string{
		{"http://Example.com/a", "http://example.com:80/b"},
		{"https://example.com/", "https://EXAMPLE.com:443/next"},
		{"http://[2606:4700::1]/", "http://[2606:4700::1]:80/"},
	}
	for _, pair := range same {
		if !sameOrigin(parse(pair[0]), parse(pair[1])) {
			t.Errorf("%s and %s differ", pair[0], pair[1])
		}
	}
	different := [][2]string{
		{"http://example.com/", "https://example.com/"},
		{"https://example.com/", "https://example.com:8443/"},
		{"https://example.com/", "https://www.example.com/"},
	}
	for _, pair := range different {
		if sameOrigin(parse(pair[0]), parse(pair[1])) {
			t.Errorf("%s and %s matched", pair[0], pair[1])
		}
	}
	if got := origin(parse("https://Example.com/path")); got != "https://example.com:443" {
		t.Fatalf("origin=%s", got)
	}
	if got := origin(parse("http://[2606:4700::1]:8080/")); got != "http://[2606:4700::1]:8080" {
		t.Fatalf("IPv6 origin=%s", got)
	}
}

func TestClassifyAndCharset(t *testing.T) {
	for contentType, want := range map[string]web.FetchKind{
		"text/html; charset=utf-8": web.FetchHTML, "Application/XHTML+XML": web.FetchHTML,
		"text/plain": web.FetchText, "text/markdown;x=y": web.FetchText, "application/json": web.FetchText,
		"application/xml": web.FetchText, "application/ld+json": web.FetchText, "image/svg+xml": web.FetchText,
	} {
		if got, ok := classify(contentType); !ok || got != want {
			t.Errorf("%q classified %q ok=%v", contentType, got, ok)
		}
	}
	for _, contentType := range []string{"", "image/png", "application/octet-stream", "application/pdf", "application/javascript"} {
		if _, ok := classify(contentType); ok {
			t.Errorf("%q accepted", contentType)
		}
	}
	for contentType, want := range map[string]string{
		"text/html":                           "",
		"text/html; charset=UTF-8":            "utf-8",
		`text/html; Charset=" GBK "`:          "gbk",
		"text/plain; format=flowed; charset=": "",
		"text/plain; charset":                 "",
	} {
		if got := charsetLabel(contentType); got != want {
			t.Errorf("%q charset=%q, want %q", contentType, got, want)
		}
	}
}
