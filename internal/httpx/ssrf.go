package httpx

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateURL parses rawURL and enforces the proxy's outbound-request guard:
//
//   - the scheme must be http or https (always — rejects file://, gopher://, …);
//   - the host must be non-empty;
//   - when blockPrivate is true, the host must not be (or resolve to) a
//     private/reserved IP — IPv4 OR IPv6 (loopback, RFC1918, RFC4193 ULA
//     fc00::/7, link-local 169.254/16 & fe80::/10, unspecified, multicast).
//
// When blockPrivate is set, obfuscated IPv4 literals (leading-zero octal
// 0177.0.0.1, short 127.1, decimal 2130706433) are rejected outright: Go's
// net.LookupIP and a browser disagree on what they resolve to, so validating
// one interpretation and letting the downstream fetcher act on another is a
// bypass. DNS failures pass through — the downstream fetcher surfaces them.
//
// NOTE: this is a best-effort, app-layer guard only. The actual fetch is done
// by the crawl4ai upstream's headless browser, which re-resolves DNS and
// follows redirects on its own — so a determined attacker can still defeat this
// via DNS rebinding or a redirect to a private target. The crawl4ai egress
// NetworkPolicy (which blocks RFC1918 + link-local) is the load-bearing
// control; this check is defense in depth that rejects the common cases early.
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
	if ip := net.ParseIP(host); ip != nil {
		if isReservedIP(ip) {
			return fmt.Errorf("private/reserved IP %s not allowed", ip)
		}
		return nil
	}

	// Numeric-looking host that net.ParseIP rejected (octal/short/decimal IPv4
	// forms): ambiguous between Go's resolver and a browser — refuse outright.
	if isNumericHost(host) {
		return fmt.Errorf("ambiguous numeric host %q not allowed (use a canonical IP or a hostname)", host)
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		// Allow DNS failures to pass through — downstream will handle them.
		return nil
	}
	for _, ip := range ips {
		if isReservedIP(ip) {
			return fmt.Errorf("private/reserved IP %s not allowed", ip)
		}
	}
	return nil
}

// isReservedIP reports whether ip is in a private or otherwise non-routable
// range. The stdlib predicates cover both IPv4 and IPv6 — the old IPv4-only
// CIDR list let IPv6 loopback/ULA/link-local (::1, fc00::/7, fe80::/10) past.
func isReservedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified()
}

// isNumericHost reports whether host consists solely of digits and dots — i.e.
// an IPv4-literal attempt. A real hostname always has a non-numeric label (its
// TLD), so this only matches numeric forms. Used to catch obfuscated IPv4
// encodings (leading-zero octal, short, decimal) that net.ParseIP rejects but
// net.LookupIP / browsers may still interpret as a private address.
func isNumericHost(host string) bool {
	return strings.IndexFunc(host, func(r rune) bool {
		return (r < '0' || r > '9') && r != '.'
	}) == -1
}
