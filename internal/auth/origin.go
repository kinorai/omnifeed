package auth

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// OriginGuard returns middleware that blocks cross-origin browser requests —
// the DNS-rebinding guard the MCP Streamable HTTP transport spec requires.
// It wraps every HTTP mux in main (not just the MCP one) because the loader
// and search transports are reachable by the same rebinding trick.
//
// Native clients send no Origin header and always pass. Browsers do send
// one: loopback origins pass (browser-based tools like the MCP inspector run
// there), origins in allowed pass (exact value match, expected lowercase —
// config.Load lowercases them), everything else is 403 before auth runs.
func OriginGuard(allowed []string) func(http.Handler) http.Handler {
	allowSet := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		allowSet[o] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && !originAllowed(origin, allowSet) {
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func originAllowed(origin string, allowSet map[string]struct{}) bool {
	if _, ok := allowSet[strings.ToLower(origin)]; ok {
		return true
	}
	return isLoopbackOrigin(origin)
}

// isLoopbackOrigin reports whether origin points at this machine: hostname
// "localhost" or any loopback IP. It parses the IP instead of comparing
// literals — httpx's SSRF guard already learned that hand-written lists miss
// 127.0.0.0/8 and IPv6 loopback. An unparsable origin (including "null",
// which browsers send from sandboxed/opaque contexts) is not loopback.
func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
