package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kinorai/omnifeed/internal/observability"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// An incoming W3C traceparent is continued: the handler's log line carries the
// caller's trace id (and the request id), and the server span is a child of
// the caller's span, named by the bounded route. Probes are not traced.
func TestTracedHandlerContinuesTraceparent(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())), sdktrace.WithSyncer(mem)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })

	var buf bytes.Buffer
	logger := slog.New(observability.NewContextHandler(slog.NewJSONHandler(&buf, nil)))
	h := tracedHandler("loader", observability.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		logger.InfoContext(r.Context(), "handled")
	})))

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	req := httptest.NewRequest(http.MethodPost, "/crawl?u=secret", nil)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	req.Header.Set(observability.RequestIDHeader, "digestif-42")
	h.ServeHTTP(httptest.NewRecorder(), req)

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line: %v (%s)", err, buf.String())
	}
	if line["trace_id"] != traceID || line["request_id"] != "digestif-42" || line["span_id"] == nil {
		t.Fatalf("log line = %v, want the caller's trace_id and request id", line)
	}
	spans := mem.GetSpans()
	if len(spans) != 1 || spans[0].Name != "POST /crawl" || spans[0].Parent.SpanID().String() != "00f067aa0ba902b7" {
		t.Fatalf("spans = %+v", spans)
	}

	mem.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if n := len(mem.GetSpans()); n != 0 {
		t.Fatalf("probe traced: %d spans", n)
	}
}
