package observability

import (
	"errors"
	"regexp"
	"strconv"

	"github.com/kinorai/omnifeed/internal/domain"
)

// Redaction patterns for upstream endpoints embedded in error strings. Client
// errors quote the address they failed to reach — a configured internal
// endpoint — in three shapes: an absolute URL (`Post "http://crawl4ai:11235/…"`),
// a dial address (`dial tcp 10.43.12.7:11235`), and a DNS lookup
// (`lookup crawl4ai on 10.43.0.10:53`). The loader/MCP surfaces are publicly
// reachable through the tunnel, so all three must be scrubbed, not just URLs.
var (
	urlPattern    = regexp.MustCompile(`https?://[^\s"'` + "`" + `]+`)
	addrPattern   = regexp.MustCompile(`(\d{1,3}(?:\.\d{1,3}){3}|\[[0-9a-fA-F:.]+\])(:\d+)?`)
	lookupPattern = regexp.MustCompile(`\blookup [^\s:]+`)
)

// redact scrubs upstream endpoints (URLs, IP:port addresses, DNS lookup
// targets) out of an error detail before it is handed to a caller.
func redact(s string) string {
	s = urlPattern.ReplaceAllString(s, "[upstream]")
	s = lookupPattern.ReplaceAllString(s, "lookup [upstream]")
	s = addrPattern.ReplaceAllString(s, "[upstream]")
	return s
}

// Explain renders a failed crawl/search error as a short, caller-safe
// explanation: the classified reason, the upstream HTTP status when the error
// carries one, and the root cause with internal endpoints redacted. Transports
// prefix their own context ("fetch_url failed: " + Explain(err)) so an MCP or
// HTTP client sees what metrics already know instead of an opaque failure.
// Returns "" only when err is nil.
func Explain(err error) string {
	if err == nil {
		return ""
	}

	label := Reason(err)
	var fe *domain.FetchError
	typed := errors.As(err, &fe)

	detail := err.Error()
	switch {
	case typed && fe.Err != nil:
		// FetchError.Error() already prefixes the kind; take the cause alone so
		// the reason is not repeated.
		detail = fe.Err.Error()
	case typed && fe.Marker != "":
		detail = "matched block marker " + strconv.Quote(fe.Marker)
	case typed:
		detail = ""
	}

	if typed && fe.StatusCode != 0 {
		label += " (HTTP " + strconv.Itoa(fe.StatusCode) + ")"
	}
	switch {
	case detail == "":
		return label
	case label == string(domain.KindError):
		// The catch-all reason adds nothing over the cause itself.
		return redact(detail)
	default:
		return label + ": " + redact(detail)
	}
}
