package observability

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestScrubText(t *testing.T) {
	in := `Get "https://api.github.com/repos/a/b/issues/1?token=x": context deadline exceeded`
	want := `Get "https://api.github.com": context deadline exceeded`
	if got := ScrubText(in); got != want {
		t.Fatalf("ScrubText = %q, want %q", got, want)
	}
}

// No endpoint, no tracing: nothing is installed and shutdown is a no-op.
func TestSetupTracingDisabledWithoutEndpoint(t *testing.T) {
	shutdown, enabled, err := SetupTracing(context.Background(), TracingConfig{})
	if err != nil || enabled {
		t.Fatalf("SetupTracing(no endpoint) = enabled %v, err %v; want disabled", enabled, err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Spans leave the process without URLs, queries, paths or caller IPs —
// including URLs quoted inside error texts — while the host and bounded
// attributes survive.
func TestScrubExporterRemovesURLs(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(scrubExporter{mem}))
	_, span := tp.Tracer("t").Start(context.Background(), "GET", trace.WithAttributes(
		attribute.String("url.full", "https://www.reddit.com/r/x/comments/abc/secret?q=1"),
		attribute.String("url.query", "q=private"),
		attribute.String("url.path", "/r/x/comments/abc"),
		attribute.String("client.address", "203.0.113.9"),
		attribute.String("server.address", "www.reddit.com"),
		attribute.String("engine", "reddit"),
	))
	SpanError(span, errors.New(`Get "https://www.reddit.com/r/x/comments/abc": EOF`))
	span.End()

	got := mem.GetSpans()
	if len(got) != 1 {
		t.Fatalf("spans = %d", len(got))
	}
	attrs := map[attribute.Key]string{}
	for _, kv := range got[0].Attributes {
		attrs[kv.Key] = kv.Value.String()
	}
	for _, k := range []attribute.Key{"url.full", "url.query", "url.path", "client.address"} {
		if v, ok := attrs[k]; ok {
			t.Errorf("%s exported: %q", k, v)
		}
	}
	if attrs["server.address"] != "www.reddit.com" || attrs["engine"] != "reddit" {
		t.Errorf("bounded attributes lost: %v", attrs)
	}
	if d := got[0].Status.Description; d != `Get "https://www.reddit.com": EOF` {
		t.Errorf("status description = %q", d)
	}
	for _, ev := range got[0].Events {
		for _, kv := range ev.Attributes {
			if v := kv.Value.String(); v != ScrubText(v) {
				t.Errorf("event attribute %s carries a URL: %q", kv.Key, v)
			}
		}
	}
}
