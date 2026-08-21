package httpx

import (
	"context"
	"errors"
	"sync"
	"time"
)

// defaultLimiterCooldown is how long a FallbackLimiter paces in process after
// its primary backend failed, before it probes it again. Long enough that a
// dead backend costs one timeout per cooldown rather than one per request,
// short enough that a restarted Redis is picked up within a search or two.
const defaultLimiterCooldown = 30 * time.Second

// FallbackLimiter runs Primary while it is healthy and falls back to Fallback
// when Primary's backend is down.
//
// The distributed limiter's whole point is to share pacing state between
// replicas, and its backend (Redis) is a single point of failure. Retrieval must
// never depend on it: a Redis outage degrades pacing to per-pod limits (the
// behavior omnifeed had before it was introduced), it does not fail a crawl.
// Hence fail OPEN — Primary errors are absorbed, not returned.
//
// On the first ErrLimiterUnavailable the circuit opens: this acquire and every
// later one are served by Fallback until Cooldown elapses, then the next
// acquire probes Primary again. Context errors are the caller's own timeout,
// not backend health, so they pass through and leave the circuit alone.
type FallbackLimiter struct {
	Primary  Limiter
	Fallback Limiter

	// Cooldown is how long to stay on Fallback after a Primary failure.
	// 0 means defaultLimiterCooldown.
	Cooldown time.Duration

	// OnDegraded, when non-nil, is called on TRANSITIONS only: true when the
	// circuit opens, false when a probe finds Primary healthy again. It is the
	// single hook for logging and metrics — this type stays logger-free so it
	// can be unit-tested with stubs. Set once at wiring time.
	OnDegraded func(down bool)

	// now is the clock, nil meaning time.Now. Tests inject a fake one to drive
	// the cooldown without sleeping.
	now func() time.Time

	mu        sync.Mutex
	downUntil time.Time // zero when healthy
}

var _ Limiter = (*FallbackLimiter)(nil)

// Acquire admits one request through whichever backend is currently serving.
// The release func returned always belongs to that same backend, so a caller
// can never release a slot it did not take.
func (f *FallbackLimiter) Acquire(ctx context.Context, engine, rawURL string) (func(), error) {
	if !f.primaryReady() {
		return f.Fallback.Acquire(ctx, engine, rawURL)
	}

	release, err := f.Primary.Acquire(ctx, engine, rawURL)
	switch {
	case err == nil:
		f.markUp()
		return release, nil
	case errors.Is(err, ErrLimiterUnavailable):
		// The backend is down, not the request: serve THIS acquire from the
		// fallback so the caller never sees the failure.
		f.markDown()
		return f.Fallback.Acquire(ctx, engine, rawURL)
	default:
		// Context errors and anything else the primary chose to report are the
		// caller's business, and say nothing about backend health.
		return nil, err
	}
}

// primaryReady reports whether Primary should be tried: either the circuit is
// closed, or the cooldown has expired and this call is the probe.
func (f *FallbackLimiter) primaryReady() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.downUntil.IsZero() || !f.clock().Before(f.downUntil)
}

func (f *FallbackLimiter) markDown() {
	f.mu.Lock()
	first := f.downUntil.IsZero()
	f.downUntil = f.clock().Add(f.cooldown())
	f.mu.Unlock()
	if first {
		f.notify(true)
	}
}

func (f *FallbackLimiter) markUp() {
	f.mu.Lock()
	recovered := !f.downUntil.IsZero()
	f.downUntil = time.Time{}
	f.mu.Unlock()
	if recovered {
		f.notify(false)
	}
}

func (f *FallbackLimiter) notify(down bool) {
	if f.OnDegraded != nil {
		f.OnDegraded(down)
	}
}

func (f *FallbackLimiter) cooldown() time.Duration {
	if f.Cooldown > 0 {
		return f.Cooldown
	}
	return defaultLimiterCooldown
}

func (f *FallbackLimiter) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}
