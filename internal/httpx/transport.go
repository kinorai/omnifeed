package httpx

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"syscall"
	"time"
)

// NewTransport returns the outbound transport shared by omnifeed's HTTP
// clients: DefaultTransport's dial/TLS/proxy behavior with a per-host idle
// pool sized for this proxy's traffic. Go's DefaultTransport keeps only 2 idle
// connections per host, so concurrent requests to the same upstream (crawl4ai,
// SearXNG, api.github.com) evict each other's connections and re-handshake TCP
// + TLS on the next call.
func NewTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 10
	return t
}

// NewGuardedClient returns an http.Client for fetching caller-supplied URLs
// directly (no crawl4ai in between). When blockPrivate is true, every dial is
// checked AFTER DNS resolution and refused if the resolved address is
// private/reserved — unlike ValidateURL's lookup-then-fetch, this can't be
// raced by DNS rebinding, and it covers every redirect hop because the guard
// sits below the client's redirect-following. Each redirect target also goes
// through ValidateURL's name checks, so a hop to localhost, an ambiguous
// numeric host or a non-http scheme is refused before any dial; its DNS step is
// skipped, since the dial guard checks every resolved address anyway. The guarded client ignores
// HTTP(S)_PROXY: through a proxy, the dial guard would only see the proxy.
func NewGuardedClient(blockPrivate bool, timeout time.Duration) *http.Client {
	t := NewTransport()
	if blockPrivate {
		t.Proxy = nil
		// Timeout/KeepAlive mirror DefaultTransport's dialer, which Clone
		// carries function-wise but not once we replace DialContext.
		t.DialContext = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			Control:   refuseReservedAddr,
		}).DialContext
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: t,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("stopped after 10 redirects")
			}
			if err := validateURL(req.Context(), req.URL.String(), blockPrivate, false); err != nil {
				return fmt.Errorf("redirect refused: %w", err)
			}
			return nil
		},
	}
}

// maxRedirects is net/http's default redirect limit, kept for the guarded client.
const maxRedirects = 10

// refuseReservedAddr is a net.Dialer Control hook: it sees the concrete
// resolved address of every connection attempt and rejects private/reserved
// targets before the connect syscall.
func refuseReservedAddr(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("refusing dial to unparseable address %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || IsReservedAddr(ip.Unmap()) {
		return fmt.Errorf("refusing dial to private/reserved address %s", host)
	}
	return nil
}
