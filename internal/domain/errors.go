package domain

import (
	"errors"
	"fmt"
	"net/http"
	"time"
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
	KindNotFound      FailureKind = "not_found"      // the requested page answered 404/410 or is a soft 404: the content does not exist
	KindSiteError     FailureKind = "site_error"     // the requested site itself answered 5xx — not omnifeed's or crawl4ai's fault
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
	// RetryAfter is how long the upstream (or omnifeed's own pacing) asked the
	// caller to wait before trying again; 0 when nobody said. Transports render
	// it as retry_after_s so a caller can back off instead of hammering.
	RetryAfter time.Duration
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

// IsBlockKind reports whether kind is a block or rate verdict: the upstream
// refused us (http_429, http_403, captcha, bot_block), or omnifeed's own pacing
// did (quota_exhausted). Such a verdict is about the HOST that was asked, which
// is what decides whether a browser render of the page may follow it — see
// SameHostEngine.
func IsBlockKind(kind FailureKind) bool {
	switch kind {
	case KindHTTP429, KindHTTP403, KindCaptcha, KindBotBlock, KindQuotaExhausted:
		return true
	}
	return false
}

// KindForStatus maps an HTTP status code to the matching FailureKind.
func KindForStatus(code int) FailureKind {
	switch {
	case code == http.StatusForbidden:
		return KindHTTP403
	case code == http.StatusTooManyRequests:
		return KindHTTP429
	case code == http.StatusGatewayTimeout:
		// A gateway timeout is a time budget running out (crawl4ai's own
		// wall-clock 504 "Crawl exceeded the time limit"), not an upstream fault.
		return KindTimeout
	case code >= 500:
		return KindUpstreamError
	default:
		return KindError
	}
}

// finalError marks an engine failure the registry must hand back as-is
// instead of retrying the URL on the generic fallback. See NoFallback.
type finalError struct{ error }

func (e finalError) Unwrap() error { return e.error }

// NoFallback marks err as final: the engine already ran the generic fallback
// itself (on a URL it rewrote) or knows the content does not exist, so the
// registry must not spend another browser render on the original URL. The
// message and the wrapped chain — FetchError included — are unchanged.
func NoFallback(err error) error {
	if err == nil {
		return nil
	}
	return finalError{err}
}

// IsNoFallback reports whether err was marked with NoFallback.
func IsNoFallback(err error) bool {
	var f finalError
	return errors.As(err, &f)
}
