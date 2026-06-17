package observability

import (
	"context"
	"errors"

	"github.com/kinorai/omnifeed/internal/domain"
)

// Reason maps a crawl error to the bounded `reason` label recorded on
// omnifeed_requests_total, so metrics and alerts can tell a 403 from a CAPTCHA
// from a timeout. It reads the classified cause from a domain.FetchError that
// the engines attach as data — no error-string parsing. Errors that aren't a
// FetchError fall back to canceled (caller abort), timeout (context deadline),
// or "error".
func Reason(err error) string {
	if err == nil {
		return "ok" // success sentinel; not a FailureKind
	}
	var fe *domain.FetchError
	if errors.As(err, &fe) {
		return string(fe.Kind)
	}
	if errors.Is(err, context.Canceled) {
		return string(domain.KindCanceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return string(domain.KindTimeout)
	}
	return string(domain.KindError)
}

// StatusOf reports the coarse request status recorded alongside Reason on the
// crawl/search metrics: "ok" when err is nil, "error" otherwise.
func StatusOf(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}
