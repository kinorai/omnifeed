package observability

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the instrumentation scope of omnifeed's own spans.
const TracerName = "github.com/kinorai/omnifeed"

// Tracer returns omnifeed's tracer from the global provider: a no-op until
// SetupTracing installs a real one, so instrumented code costs ~nothing when
// tracing is off.
func Tracer() trace.Tracer { return otel.Tracer(TracerName) }

// TracingConfig configures SetupTracing. Endpoint is OTEL_EXPORTER_OTLP_ENDPOINT
// (a base URL; the exporter appends /v1/traces); empty disables tracing.
type TracingConfig struct {
	Endpoint    string
	ServiceName string
	Version     string
}

// SetupTracing installs an OTLP/HTTP tracer provider (parent-based,
// always-on sampling) and the W3C trace-context propagator as the globals.
// With an empty Endpoint it installs nothing and returns enabled=false and a
// no-op shutdown. shutdown flushes buffered spans; call it on exit.
//
// The exporter reads the rest of the standard OTEL_EXPORTER_OTLP_* variables
// (headers, timeout, …) itself. Every exported span passes through
// scrubExporter first, which removes full URLs, queries and paths: the HTTP
// instrumentation records them by default, and they carry the caller's URLs.
func SetupTracing(ctx context.Context, cfg TracingConfig) (shutdown func(context.Context) error, enabled bool, err error) {
	noop := func(context.Context) error { return nil }
	if cfg.Endpoint == "" {
		return noop, false, nil
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return noop, false, fmt.Errorf("otlp trace exporter: %w", err)
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = "omnifeed"
	}
	res := resource.NewWithAttributes(semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
		semconv.ServiceVersion(cfg.Version),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
		sdktrace.WithBatcher(scrubExporter{exp}),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return tp.Shutdown(ctx)
	}, true, nil
}

// droppedAttrs are HTTP semantic-convention attributes that carry a URL, a
// query or a path — user content (a pasted URL, a search query, an id in a
// path). server.address (the host) is kept: hosts are what a trace is read by.
var droppedAttrs = map[attribute.Key]bool{
	"url.full":     true,
	"url.query":    true,
	"url.path":     true,
	"url.original": true,
	"http.url":     true,
	"http.target":  true,
	// The caller's IP: not needed to read a trace.
	"client.address":       true,
	"network.peer.address": true,
	"network.peer.port":    true,
}

// urlInText matches an absolute URL inside free text (an error message): the
// HTTP client's errors quote the full request URL.
var urlInText = regexp.MustCompile(`https?://[^\s"'<>]+`)

// ScrubText replaces every absolute URL in s with its scheme and host.
func ScrubText(s string) string {
	return urlInText.ReplaceAllStringFunc(s, func(m string) string {
		u, err := url.Parse(m)
		if err != nil || u.Host == "" {
			return "<url>"
		}
		return u.Scheme + "://" + u.Host
	})
}

func scrubAttrs(in []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(in))
	for _, kv := range in {
		if droppedAttrs[kv.Key] {
			continue
		}
		if kv.Value.Type() == attribute.STRING {
			kv = attribute.String(string(kv.Key), ScrubText(kv.Value.AsString()))
		}
		out = append(out, kv)
	}
	return out
}

// scrubExporter removes URL-bearing attributes and URLs inside status and
// event texts before spans leave the process.
type scrubExporter struct {
	sdktrace.SpanExporter
}

// ExportSpans scrubs each span, then exports it.
func (e scrubExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = scrubbedSpan{ReadOnlySpan: s}
	}
	return e.SpanExporter.ExportSpans(ctx, out)
}

// scrubbedSpan overrides the fields that can carry user content. Embedding the
// interface also satisfies ReadOnlySpan's unexported method.
type scrubbedSpan struct {
	sdktrace.ReadOnlySpan
}

// Attributes drops URL-bearing keys and scrubs URLs out of string values.
func (s scrubbedSpan) Attributes() []attribute.KeyValue {
	return scrubAttrs(s.ReadOnlySpan.Attributes())
}

// Status scrubs URLs out of the description.
func (s scrubbedSpan) Status() sdktrace.Status {
	st := s.ReadOnlySpan.Status()
	st.Description = ScrubText(st.Description)
	return st
}

// Events scrubs each event's attributes (exception.message quotes URLs).
func (s scrubbedSpan) Events() []sdktrace.Event {
	in := s.ReadOnlySpan.Events()
	out := make([]sdktrace.Event, len(in))
	for i, ev := range in {
		ev.Attributes = scrubAttrs(ev.Attributes)
		out[i] = ev
	}
	return out
}

// SpanError marks span failed with err (scrubbed at export). A nil err is a
// no-op, so callers can defer it unconditionally.
func SpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}
