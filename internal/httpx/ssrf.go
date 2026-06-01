package httpx

import (
	"fmt"
	"net"
	"net/url"
)

// ValidateURL parses rawURL and, when blockPrivate is true, rejects URLs whose
// hostname resolves to a private/reserved IP — IPv4 OR IPv6 (loopback, RFC1918,
// RFC4193 ULA fc00::/7, link-local 169.254/16 & fe80::/10, unspecified,
// multicast). DNS failures pass through — the downstream fetcher surfaces them.
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

	ips, err := net.LookupIP(host)
	if err != nil {
		// Allow DNS failures to pass through — downstream will handle them.
		return nil
	}
	for _, ip := range ips {
		// stdlib predicates cover both IPv4 and IPv6 — the old IPv4-only CIDR
		// list let IPv6 loopback/ULA/link-local (::1, fc00::/7, fe80::/10) past.
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("private/reserved IP %s not allowed", ip)
		}
	}
	return nil
}
