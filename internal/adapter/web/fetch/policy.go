package fetch

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"

	"github.com/jinyule/nano-harness/internal/app/web"
)

const (
	// maxURLUnits bounds each input URL in UTF-16 code units before normalization.
	maxURLUnits = 2048
	// nat64DiscoveryHost is the RFC 7050 name whose synthesized AAAA answers
	// reveal the active DNS64 prefixes.
	nat64DiscoveryHost = "ipv4only.arpa"
)

// Non-public IPv4 ranges: IANA special-purpose blocks that are not globally
// reachable, plus the AS112 and AMT anycast blocks the reference policy refuses.
var blockedIPv4 = prefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.31.196.0/24", "192.52.193.0/24",
	"192.88.99.0/24", "192.168.0.0/16", "192.175.48.0/24", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
)

// IPv6 destinations must be global unicast (2000::/3) outside the IETF
// protocol-assignment, documentation, 6to4, and AS112 blocks. Everything outside
// 2000::/3 — loopback, unspecified, link/site-local, ULA, multicast, NAT64 and
// IPv4-compatible forms — is therefore refused; IPv4-mapped forms are unmapped first.
var (
	globalUnicastIPv6 = netip.MustParsePrefix("2000::/3")
	blockedIPv6       = prefixes("2001::/23", "2001:db8::/32", "2002::/16", "2620:4f:8000::/48", "3fff::/20")
)

// RFC 6052 prefix lengths and the RFC 7050 sentinel answers.
var (
	nat64PrefixBits = []int{32, 40, 48, 56, 64, 96}
	nat64Sentinels  = []netip.Addr{netip.MustParseAddr("192.0.0.170"), netip.MustParseAddr("192.0.0.171")}
)

func prefixes(values ...string) []netip.Prefix {
	parsed := make([]netip.Prefix, len(values))
	for index, value := range values {
		parsed[index] = netip.MustParsePrefix(value)
	}
	return parsed
}

// publicAddress reports whether address is a globally reachable unicast destination.
func publicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || address.Zone() != "" {
		return false
	}
	contains := func(prefix netip.Prefix) bool { return prefix.Contains(address) }
	if address.Is4() {
		return !slices.ContainsFunc(blockedIPv4, contains)
	}
	return globalUnicastIPv6.Contains(address) && !slices.ContainsFunc(blockedIPv6, contains)
}

// embeddedIPv4 extracts the IPv4 address carried by an RFC 6052 IPv6 layout.
// Layouts shorter than /96 skip the reserved "u" octet, which must be zero.
func embeddedIPv4(address netip.Addr, bits int) (netip.Addr, bool) {
	raw := address.As16()
	if bits == 96 {
		return netip.AddrFrom4([4]byte(raw[12:16])), true
	}
	if raw[8] != 0 {
		return netip.Addr{}, false
	}
	start := bits / 8
	before := 8 - start
	var embedded [4]byte
	copy(embedded[:], raw[start:start+before])
	copy(embedded[before:], raw[9:9+4-before])
	return netip.AddrFrom4(embedded), true
}

// nat64Prefixes derives active DNS64 prefixes from synthesized discovery answers.
func nat64Prefixes(answers []netip.Addr) []netip.Prefix {
	var found []netip.Prefix
	for _, address := range answers {
		if !address.Is6() || address.Is4In6() {
			continue
		}
		for _, bits := range nat64PrefixBits {
			embedded, ok := embeddedIPv4(address, bits)
			prefix := netip.PrefixFrom(address, bits).Masked()
			if ok && slices.Contains(nat64Sentinels, embedded) && !slices.Contains(found, prefix) {
				found = append(found, prefix)
			}
		}
	}
	return found
}

// translatedIPv4 returns the IPv4 destination of an address inside a NAT64 prefix.
func translatedIPv4(address netip.Addr, active []netip.Prefix) (netip.Addr, bool) {
	for _, prefix := range active {
		if prefix.Contains(address) {
			return embeddedIPv4(address, prefix.Bits())
		}
	}
	return netip.Addr{}, false
}

// parseURL applies the network-independent policy: bounded length, HTTP(S),
// a host, a valid port, and no embedded credentials.
func parseURL(raw string) (*url.URL, error) {
	if _, cut := truncateUTF16(raw, maxURLUnits); cut {
		return nil, &web.Error{Code: web.CodeInvalidURL, Message: fmt.Sprintf("URL exceeds the maximum length of %d", maxURLUnits)}
	}
	raw = strings.TrimFunc(raw, func(scalar rune) bool { return scalar <= ' ' })
	if !utf8.ValidString(raw) || strings.ContainsAny(raw, "\\") || strings.IndexFunc(raw, func(scalar rune) bool { return scalar < ' ' || scalar == 0x7f }) >= 0 {
		return nil, &web.Error{Code: web.CodeInvalidURL, Message: "URL contains invalid UTF-8, a control character or a backslash"}
	}
	if scheme, rest, ok := strings.Cut(raw, ":"); ok && (strings.EqualFold(scheme, "http") || strings.EqualFold(scheme, "https")) && rest != "" && !strings.HasPrefix(rest, "/") {
		raw = scheme + "://" + rest
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, &web.Error{Code: web.CodeInvalidURL, Message: "invalid URL: " + raw, Cause: err}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, &web.Error{Code: web.CodeInvalidURL, Message: fmt.Sprintf("unsupported URL scheme %q (only http and https are allowed)", parsed.Scheme)}
	}
	if parsed.User != nil {
		return nil, &web.Error{Code: web.CodeBlockedURL, Message: "credentials in URLs are not allowed"}
	}
	if parsed.Hostname() == "" {
		return nil, &web.Error{Code: web.CodeInvalidURL, Message: "URL has no host: " + raw}
	}
	port := parsed.Port()
	if port != "" {
		if value, err := strconv.ParseUint(port, 10, 16); err != nil || value == 0 {
			return nil, &web.Error{Code: web.CodeInvalidURL, Message: "URL has an invalid port: " + raw}
		}
		port = strconv.Itoa(int(portOf(parsed)))
	}
	host, err := normalizeHostname(parsed.Hostname())
	if err != nil {
		return nil, &web.Error{Code: web.CodeInvalidURL, Message: "invalid URL hostname", Cause: err}
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if (parsed.Scheme == "http" && port == "80") || (parsed.Scheme == "https" && port == "443") {
		port = ""
	}
	parsed.Host = host
	if port != "" {
		parsed.Host += ":" + port
	}
	path := parsed.RawPath
	if path == "" {
		path = parsed.EscapedPath()
	}
	if path == "" {
		path = "/"
	}
	path = normalizePath(escapeComponent(path, "\"#<>?^`{}|"))
	parsed.Path, _ = url.PathUnescape(path) // url.Parse proves all escapes are valid
	parsed.RawPath = path
	parsed.RawQuery = escapeComponent(parsed.RawQuery, "\"#<>'")
	fragment := parsed.RawFragment
	if fragment == "" {
		fragment = parsed.EscapedFragment()
	}
	fragment = escapeComponent(fragment, "\"<>`")
	parsed.Fragment, _ = url.PathUnescape(fragment)
	parsed.RawFragment = fragment
	return parsed, nil
}

// normalizeHostname applies UTS #46 lookup mapping before any address policy.
// Ambiguous numeric IPv4 spellings are refused rather than delegated to DNS.
func normalizeHostname(host string) (string, error) {
	if address, err := netip.ParseAddr(strings.TrimSuffix(host, ".")); err == nil {
		if address.Is4In6() {
			raw := address.As16()
			return fmt.Sprintf("::ffff:%x:%x", uint16(raw[12])<<8|uint16(raw[13]), uint16(raw[14])<<8|uint16(raw[15])), nil
		}
		return address.String(), nil
	}
	host, err := idna.Lookup.ToASCII(host)
	if err != nil {
		return "", fmt.Errorf("normalize IDNA hostname: %w", err)
	}
	// Mapping can turn full-width digits into an ordinary IP literal.
	if address, err := netip.ParseAddr(strings.TrimSuffix(host, ".")); err == nil {
		return address.String(), nil
	}
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	last := labels[len(labels)-1]
	if (last != "" && strings.Trim(last, "0123456789") == "") || strings.HasPrefix(last, "0x") {
		return "", fmt.Errorf("non-canonical numeric hostname is not allowed")
	}
	return host, nil
}

// normalizePath removes literal and percent-encoded dot segments while
// preserving repeated slashes and escaped path separators.
func normalizePath(path string) string {
	segments := strings.Split(path, "/")
	var kept []string
	for index, segment := range segments {
		dot := strings.ReplaceAll(strings.ToLower(segment), "%2e", ".")
		if dot == "." || dot == ".." {
			if dot == ".." && len(kept) > 1 {
				kept = kept[:len(kept)-1]
			}
			if index == len(segments)-1 {
				kept = append(kept, "")
			}
			continue
		}
		kept = append(kept, segment)
	}
	return strings.Join(kept, "/")
}

// escapeComponent preserves existing escapes and literal plus signs while
// encoding non-ASCII bytes, spaces and the component's forbidden characters.
func escapeComponent(value string, forbidden string) string {
	var escaped strings.Builder
	for index := 0; index < len(value); index++ {
		current := value[index]
		if current <= ' ' || current >= 0x7f || strings.ContainsRune(forbidden, rune(current)) {
			fmt.Fprintf(&escaped, "%%%02X", current)
		} else {
			escaped.WriteByte(current)
		}
	}
	return escaped.String()
}

// portOf returns the effective TCP port of a URL accepted by parseURL.
func portOf(target *url.URL) uint16 {
	if port := target.Port(); port != "" {
		value, _ := strconv.ParseUint(port, 10, 16) // parseURL already proved the port is a valid uint16
		return uint16(value)
	}
	if target.Scheme == "https" {
		return 443
	}
	return 80
}

func origin(target *url.URL) string { return target.Scheme + "://" + hostPort(target) }

// hostPort is the case-folded host and effective port that identify an origin.
func hostPort(target *url.URL) string {
	return net.JoinHostPort(strings.ToLower(target.Hostname()), strconv.Itoa(int(portOf(target))))
}

// sameOrigin compares scheme, case-folded host, and effective port.
func sameOrigin(left, right *url.URL) bool {
	return left.Scheme == right.Scheme && hostPort(left) == hostPort(right)
}
