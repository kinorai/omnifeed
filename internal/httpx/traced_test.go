package httpx

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TracedLimiter records one limiter.wait span per Acquire with the scope and
// the host only, and still forwards Penalize to the wrapped limiter.
func TestTracedLimiter(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(mem)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	inner := &penalizingStub{}
	l := TraceLimiter("domain", inner)
	release, err := l.Acquire(context.Background(), "reddit", "https://www.reddit.com/r/x/comments/abc")
	if err != nil {
		t.Fatal(err)
	}
	release()
	l.Penalize("https://www.reddit.com/", 3*time.Second)
	if len(inner.penalties) != 1 || inner.penalties[0] != 3*time.Second {
		t.Fatalf("Penalize not forwarded: %v", inner.penalties)
	}
	spans := mem.GetSpans()
	if len(spans) != 1 || spans[0].Name != "limiter.wait" {
		t.Fatalf("spans = %+v", spans)
	}
	attrs := map[string]string{}
	for _, kv := range spans[0].Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
	}
	if attrs["limiter.scope"] != "domain" || attrs["server.address"] != "www.reddit.com" || attrs["limiter.outcome"] != "acquired" {
		t.Fatalf("attrs = %v", attrs)
	}
}
