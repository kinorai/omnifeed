package httpx

import (
	"context"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TracedLimiter wraps a Limiter in a "limiter.wait" span per Acquire, so a
// trace shows how long a request queued for its pacing slot and in which
// scope. Wired only when tracing is on. Attributes are bounded: the scope, the
// engine, the target host (never the URL) and the outcome.
type TracedLimiter struct {
	inner Limiter
	scope string
}

// TraceLimiter wraps l. l must be non-nil.
func TraceLimiter(scope string, l Limiter) *TracedLimiter {
	return &TracedLimiter{inner: l, scope: scope}
}

// Acquire implements Limiter.
func (t *TracedLimiter) Acquire(ctx context.Context, engine, rawURL string) (func(), error) {
	attrs := []attribute.KeyValue{
		attribute.String("limiter.scope", t.scope),
		attribute.String("engine", engine),
	}
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		attrs = append(attrs, attribute.String("server.address", u.Hostname()))
	}
	ctx, span := otel.Tracer("github.com/kinorai/omnifeed").Start(ctx, "limiter.wait", trace.WithAttributes(attrs...))
	start := time.Now()
	release, err := t.inner.Acquire(ctx, engine, rawURL)
	span.SetAttributes(attribute.Int64("limiter.wait_ms", time.Since(start).Milliseconds()))
	if err != nil {
		span.SetStatus(codes.Error, "limiter refused")
		span.SetAttributes(attribute.String("limiter.outcome", "refused"))
	} else {
		span.SetAttributes(attribute.String("limiter.outcome", "acquired"))
	}
	span.End()
	return release, err
}

// Penalize forwards an upstream's Retry-After hold to the wrapped limiter.
func (t *TracedLimiter) Penalize(rawURL string, d time.Duration) {
	penalize(t.inner, rawURL, d)
}
