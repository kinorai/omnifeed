package httpx

import (
	"fmt"
	"net"
	"net/url"
)

// privateRanges enumerates the IPv4 CIDRs we treat as off-limits to fetch.
var privateRanges = []net.IPNet{
	{IP: net.IP{10, 0, 0, 0}, Mask: net.CIDRMask(8, 32)},
	{IP: net.IP{172, 16, 0, 0}, Mask: net.CIDRMask(12, 32)},
	{IP: net.IP{192, 168, 0, 0}, Mask: net.CIDRMask(16, 32)},
	{IP: net.IP{169, 254, 0, 0}, Mask: net.CIDRMask(16, 32)},
	{IP: net.IP{127, 0, 0, 0}, Mask: net.CIDRMask(8, 32)},
	{IP: net.IP{0, 0, 0, 0}, Mask: net.CIDRMask(8, 32)},
}

// ValidateURL parses rawURL and, when blockPrivate is true, rejects URLs
// whose hostname resolves to a private/reserved IPv4 range. DNS failures are
// allowed to pass through — the downstream fetcher will surface them.
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
		if ip4 := ip.To4(); ip4 != nil {
			for _, cidr := range privateRanges {
				if cidr.Contains(ip4) {
					return fmt.Errorf("private/reserved IP %s not allowed", ip4)
				}
			}
		}
	}
	return nil
}
