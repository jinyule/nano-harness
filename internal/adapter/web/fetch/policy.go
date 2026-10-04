package fetch

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/jinyule/nano-harness/internal/app/web"
)

const (
	// maxURLBytes bounds every requested and redirected URL.
	maxURLBytes = 2048
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
	if len(raw) > maxURLBytes {
		return nil, &web.Error{Code: web.CodeInvalidURL, Message: fmt.Sprintf("URL exceeds the maximum length of %d", maxURLBytes)}
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
	if port := parsed.Port(); port != "" {
		if value, err := strconv.ParseUint(port, 10, 16); err != nil || value == 0 {
			return nil, &web.Error{Code: web.CodeInvalidURL, Message: "URL has an invalid port: " + raw}
		}
	}
	return parsed, nil
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
