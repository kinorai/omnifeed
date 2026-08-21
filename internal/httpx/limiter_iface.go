package httpx

import (
	"context"
	"errors"
)

// Limiter admits one outbound request, blocking until the pacing policy allows
// it or ctx dies. engine names the caller for the wait metric, rawURL selects
// the domain being paced. The returned release func must be called when the
// request is done; on error there is nothing to release.
//
// Implementations: *DomainLimiter (in-process, the default), redislimit.Limiter
// (state shared between replicas via Redis) and *FallbackLimiter (fail-open
// composite of the two). A nil Limiter means pacing is disabled — every call
// site checks for it.
type Limiter interface {
	Acquire(ctx context.Context, engine, rawURL string) (release func(), err error)
}

// ErrLimiterUnavailable wraps backend failures of a distributed limiter (Redis
// unreachable, script error). It never reaches an engine: FallbackLimiter
// absorbs it and paces in process instead.
var ErrLimiterUnavailable = errors.New("rate limiter backend unavailable")

var _ Limiter = (*DomainLimiter)(nil)
