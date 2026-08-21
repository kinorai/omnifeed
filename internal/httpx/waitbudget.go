package httpx

import (
	"context"
	"fmt"
	"time"
)

// WaitBudgetError reports that the pacing wait a limiter computed is longer
// than the time the caller's context has left, so nothing was admitted and the
// caller was not made to queue for a slot it could never use. The alternative
// is what omnifeed did before: hold the caller for its whole budget and then
// fail it with a deadline error anyway, which costs an agent the wait AND the
// answer.
//
// It is a pacing VERDICT, not a backend failure: it is deliberately NOT wrapped
// in ErrLimiterUnavailable, so *FallbackLimiter passes it through untouched
// instead of absorbing it and opening its circuit against a healthy backend.
//
// RetryAfter is the wait that was refused — how long the caller must leave
// before the same request can be admitted.
type WaitBudgetError struct {
	RetryAfter time.Duration
}

func (e *WaitBudgetError) Error() string {
	return fmt.Sprintf("pacing wait %s exceeds the caller's budget", e.RetryAfter.Round(time.Millisecond))
}

// RemainingBudget reports how long ctx has left, and whether it has a deadline
// at all. A context without one has no budget to exceed, so pacing queues as
// before. Exported for the out-of-package limiter implementations (redislimit),
// which make the same fast-fail decision as *DomainLimiter.
func RemainingBudget(ctx context.Context) (time.Duration, bool) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, false
	}
	return time.Until(deadline), true
}
