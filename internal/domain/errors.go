package domain

import (
	"fmt"
	"net/http"
)

// FailureKind is a bounded classification of why a crawl/fetch failed. It is
// the single source of truth for the taxonomy that observability renders as the
// `reason` metric label — carried as data on FetchError so callers never parse
// error strings to recover the cause.
type FailureKind string

// The complete set of failure reasons. Keep this small: every value becomes a
// distinct metric series and a distinct thing to alert on.
const (
	KindCaptcha       FailureKind = "captcha"        // bot wall / human-verification challenge page
	KindHTTP403       FailureKind = "http_403"       // explicit HTTP 403
	KindHTTP429       FailureKind = "http_429"       // rate limited
	KindBotBlock      FailureKind = "bot_block"      // blocked with no clean status (nav blocked, non-JSON body)
	KindThinContent   FailureKind = "thin_content"   // crawl4ai content-gate: too little usable content rendered (JS-only SPA shell, PDF/binary, near-empty) — not a wall, not an upstream fault
	KindTimeout       FailureKind = "timeout"        // context deadline exceeded — omnifeed's own timeout budget (crawl4ai/reddit)
	KindCanceled      FailureKind = "canceled"       // caller hung up before the fetch finished (client abort — not an omnifeed fault)
	KindUpstreamError FailureKind = "upstream_error" // upstream 5xx or unreachable
	// KindUpstreamRejected is crawl4ai's application-level 500 with the verdict
	// scrubbed out of the response body (crawl4ai 0.9.2+ logs the real reason —
	// bot wall, content-gate, or crash — server-side under a correlation id and
	// returns a generic body). Indistinguishable client-side and dominated by
	// per-page non-faults, so it gets one bounded retry (for the transient
	// minority sharing the channel) and is not treated as an upstream outage.
	KindUpstreamRejected FailureKind = "upstream_rejected"
	// KindQuotaExhausted is omnifeed's OWN pacing verdict, not an upstream
	// answer: the politeness quota for the host is spent and the wait until the
	// next slot is longer than the caller's budget, so nothing was sent. The
	// retry-after is in the error message. Not a fault — the deployment is
	// working as configured, and the caller should retry later.
	KindQuotaExhausted FailureKind = "quota_exhausted"
	KindBadResponse    FailureKind = "bad_response" // unparseable or empty upstream response
	KindError          FailureKind = "error"        // anything else
)

// FetchError carries the classified cause of a failed crawl/fetch. Engines
// return it (optionally wrapping the underlying error) so observability.Reason
// can read Kind via errors.As instead of matching error text. StatusCode and
// Marker are optional context (0 / "" when not applicable).
type FetchError struct {
	Kind       FailureKind
	StatusCode int
	Marker     string // matched anti-bot marker, set when Kind == KindCaptcha
	Err        error  // underlying error, if any
}

func (e *FetchError) Error() string {
	switch {
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Kind, e.Err)
	case e.Marker != "":
		return fmt.Sprintf("%s (marker=%q, status=%d)", e.Kind, e.Marker, e.StatusCode)
	default:
		return string(e.Kind)
	}
}

// Unwrap exposes the underlying error to errors.Is / errors.As.
func (e *FetchError) Unwrap() error { return e.Err }

// KindForStatus maps an HTTP status code to the matching FailureKind.
func KindForStatus(code int) FailureKind {
	switch {
	case code == http.StatusForbidden:
		return KindHTTP403
	case code == http.StatusTooManyRequests:
		return KindHTTP429
	case code >= 500:
		return KindUpstreamError
	default:
		return KindError
	}
}
