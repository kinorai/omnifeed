package httpx

import (
	"fmt"
	"net"
	"net/http"
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
// sits below the client's redirect-following.
func NewGuardedClient(blockPrivate bool, timeout time.Duration) *http.Client {
	t := NewTransport()
	if blockPrivate {
		// Timeout/KeepAlive mirror DefaultTransport's dialer, which Clone
		// carries function-wise but not once we replace DialContext.
		t.DialContext = (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			Control:   refuseReservedAddr,
		}).DialContext
	}
	return &http.Client{Timeout: timeout, Transport: t}
}

// refuseReservedAddr is a net.Dialer Control hook: it sees the concrete
// resolved address of every connection attempt and rejects private/reserved
// targets before the connect syscall.
func refuseReservedAddr(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("refusing dial to unparseable address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || isReservedIP(ip) {
		return fmt.Errorf("refusing dial to private/reserved address %s", host)
	}
	return nil
}
