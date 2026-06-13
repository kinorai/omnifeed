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
	KindTimeout       FailureKind = "timeout"        // context deadline exceeded / cancelled
	KindUpstreamError FailureKind = "upstream_error" // upstream 5xx or unreachable
	KindBadResponse   FailureKind = "bad_response"   // unparseable or empty upstream response
	KindError         FailureKind = "error"          // anything else
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
