package httpx

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// lookupTimeout bounds ValidateURL's DNS lookup.
const lookupTimeout = 5 * time.Second

// lookupNetIP resolves a hostname; a variable so tests can stay off the network.
var lookupNetIP = net.DefaultResolver.LookupNetIP

// ValidateURL parses rawURL and enforces the proxy's outbound-request guard:
//
//   - the scheme must be http or https (always — rejects file://, gopher://, …);
//   - the host must be non-empty;
//   - when blockPrivate is true, the host must not be (or resolve to) an
//     address that is not globally reachable — see IsReservedAddr.
//
// When blockPrivate is set it also fails closed on everything it cannot judge:
//
//   - non-canonical IPv4 literals (octal 0177.0.0.1, short 127.1, decimal
//     2130706433, hex 0x7f.0.0.1): Go's resolver and a browser disagree on
//     what they mean, so validating one reading while the fetcher acts on
//     another is a bypass. Any host whose last label is numeric is one, as in
//     the WHATWG URL parser browsers follow;
//   - localhost and *.localhost, which browsers resolve to loopback without
//     asking DNS;
//   - a DNS lookup that fails or returns nothing.
//
// NOTE: this is an app-layer guard only. The crawl4ai upstream's headless
// browser re-resolves DNS and follows redirects on its own, so DNS rebinding or
// a redirect to a private target can still get past it there. An egress
// network policy on crawl4ai (public addresses only) is the load-bearing
// control; this check rejects the common cases early. Direct fetches use
// NewGuardedClient, which re-checks every dial and every redirect.
func ValidateURL(rawURL string, blockPrivate bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse error: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q not allowed (http/https only)", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("empty hostname")
	}
	if !blockPrivate {
		return nil
	}

	// Canonical IP literal: check it directly, no DNS lookup needed.
	if ip, err := netip.ParseAddr(host); err == nil {
		if IsReservedAddr(ip) {
			return fmt.Errorf("private/reserved IP %s not allowed", ip)
		}
		return nil
	}

	name := strings.TrimSuffix(strings.ToLower(host), ".")
	if looksLikeIPv4(name) {
		// netip.ParseAddr rejected it, so it is not dotted-quad decimal: an
		// octal, hex, short or integer form a browser would still read as IPv4.
		return fmt.Errorf("ambiguous numeric host %q not allowed (use a canonical IP or a hostname)", host)
	}
	if name == "localhost" || strings.HasSuffix(name, ".localhost") {
		return fmt.Errorf("host %q not allowed", host)
	}

	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	ips, err := lookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("cannot resolve %q: %w", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("cannot resolve %q: no addresses", host)
	}
	for _, ip := range ips {
		if IsReservedAddr(ip.Unmap()) {
			return fmt.Errorf("private/reserved IP %s not allowed", ip)
		}
	}
	return nil
}

// looksLikeIPv4 reports whether a browser would parse host as an IPv4 address:
// per the WHATWG URL standard, that is any host whose last non-empty label is a
// number — decimal digits, or 0x followed by hex digits. A real hostname's TLD
// is never numeric, so this only matches IPv4 attempts.
func looksLikeIPv4(host string) bool {
	last := host[strings.LastIndexByte(host, '.')+1:]
	if last == "" {
		return false
	}
	if rest, ok := strings.CutPrefix(last, "0x"); ok {
		return strings.Trim(rest, "0123456789abcdef") == ""
	}
	return strings.Trim(last, "0123456789") == ""
}

// reservedPrefixes are the ranges a caller-supplied URL may not reach: the IANA
// IPv4 and IPv6 special-purpose registries' entries that are not globally
// reachable, plus multicast, the IPv4-in-IPv6 forms (mapped, NAT64, 6to4) that
// can carry any IPv4 address, and documentation ranges.
var reservedPrefixes = func() []netip.Prefix {
	cidrs := []string{
		// IPv4
		"0.0.0.0/8",       // "this network"
		"10.0.0.0/8",      // private
		"100.64.0.0/10",   // shared address space (CGNAT)
		"127.0.0.0/8",     // loopback
		"169.254.0.0/16",  // link-local (cloud metadata)
		"172.16.0.0/12",   // private
		"192.0.0.0/24",    // IETF protocol assignments
		"192.0.2.0/24",    // documentation (TEST-NET-1)
		"192.88.99.0/24",  // 6to4 relay anycast (deprecated)
		"192.168.0.0/16",  // private
		"198.18.0.0/15",   // benchmarking
		"198.51.100.0/24", // documentation (TEST-NET-2)
		"203.0.113.0/24",  // documentation (TEST-NET-3)
		"224.0.0.0/4",     // multicast
		"240.0.0.0/4",     // reserved, incl. 255.255.255.255 broadcast
		// IPv6
		"::/96",          // unspecified, loopback, deprecated IPv4-compatible
		"::ffff:0:0/96",  // IPv4-mapped
		"64:ff9b::/96",   // NAT64 well-known prefix
		"64:ff9b:1::/48", // local-use NAT64
		"100::/64",       // discard-only
		"2001::/23",      // IETF protocol assignments (Teredo, ORCHID, …)
		"2001:db8::/32",  // documentation
		"2002::/16",      // 6to4
		"3fff::/20",      // documentation
		"5f00::/16",      // SRv6 SIDs
		"fc00::/7",       // unique local
		"fec0::/10",      // site-local (deprecated, still routed on some LANs)
		"fe80::/10",      // link-local
		"ff00::/8",       // multicast
	}
	out := make([]netip.Prefix, len(cidrs))
	for i, c := range cidrs {
		out[i] = netip.MustParsePrefix(c)
	}
	return out
}()

// IsReservedAddr reports whether ip is not a globally reachable unicast
// address. It is the one predicate behind ValidateURL and NewGuardedClient's
// dial guard. An IPv4-mapped IPv6 address is reserved as a literal; callers
// holding a resolver's answer, where Go may represent IPv4 that way, Unmap
// first.
func IsReservedAddr(ip netip.Addr) bool {
	if !ip.IsValid() {
		return true
	}
	ip = ip.WithZone("")
	for _, p := range reservedPrefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}
