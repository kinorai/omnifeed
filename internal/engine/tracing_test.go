package engine

import (
	"context"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// With a tracer installed, a dispatch that falls back produces an
// engine.dispatch span for the dedicated engine and an engine.fallback span
// naming where it came from — bounded attributes only, no URL.
func TestRegistryCrawl_Spans(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(mem))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	r := New().Register(&failingEngine{}).Fallback(&stubEngine{})
	if _, err := r.Crawl(context.Background(), "http://8.8.8.8/some/path?q=secret", domain.EngineOptions{}); err != nil {
		t.Fatal(err)
	}
	spans := map[string]map[attribute.Key]string{}
	for _, s := range mem.GetSpans() {
		attrs := map[attribute.Key]string{}
		for _, kv := range s.Attributes {
			attrs[kv.Key] = kv.Value.String()
		}
		spans[s.Name] = attrs
	}
	d, f := spans["engine.dispatch"], spans["engine.fallback"]
	if d["engine"] != "failing" || d["error.reason"] != "upstream_error" || d["server.address"] != "8.8.8.8" {
		t.Errorf("engine.dispatch attrs = %v", d)
	}
	if f["engine"] != "stub" || f["fallback.from"] != "failing" || f["fallback.reason"] != "upstream_error" {
		t.Errorf("engine.fallback attrs = %v", f)
	}
	for name, attrs := range spans {
		for k, v := range attrs {
			if v == "http://8.8.8.8/some/path?q=secret" {
				t.Errorf("span %s attribute %s carries the URL", name, k)
			}
		}
	}
}
