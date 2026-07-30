package observability

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/kinorai/omnifeed/internal/domain"
)

// urlPattern matches any absolute http(s) URL embedded in an error string.
// Upstream client errors quote the endpoint they failed to reach (e.g. `Post
// "http://crawl4ai:11235/crawl": dial tcp …`), which is a configured internal
// address — Explain redacts it rather than handing it to a caller.
var urlPattern = regexp.MustCompile(`https?://[^\s"'` + "`" + `]+`)

// Explain renders a failed crawl/search error as a short, caller-safe
// explanation: the classified reason, the upstream HTTP status when the error
// carries one, and the root cause. Transports prefix their own context
// ("fetch_url failed: " + Explain(err)) so an MCP or HTTP client sees what
// metrics already know instead of an opaque failure.
//
// The reason prefix is dropped when it would only stutter — when the root cause
// text already names it (`search degraded: …`) or when it is the catch-all
// "error". Absolute URLs are redacted so internal endpoints don't leak. Returns
// "" only when err is nil.
func Explain(err error) string {
	if err == nil {
		return ""
	}

	reason := Reason(err)
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
	detail = urlPattern.ReplaceAllString(detail, "[upstream]")

	label := reason
	if reason == string(domain.KindError) || (detail != "" && strings.Contains(strings.ToLower(detail), reason)) {
		label = ""
	}
	if typed && fe.StatusCode != 0 {
		status := "HTTP " + strconv.Itoa(fe.StatusCode)
		if label == "" {
			label = status
		} else {
			label += " (" + status + ")"
		}
	}

	switch {
	case label != "" && detail != "":
		return label + ": " + detail
	case label != "":
		return label
	case detail != "":
		return detail
	default:
		return reason // nothing else to say; never return an empty explanation
	}
}
