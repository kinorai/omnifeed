package observability

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// ContextHandler decorates a slog.Handler with the request-scoped fields the
// context carries: request_id (see RequestID) and, when a span is recording,
// trace_id and span_id — so a log line joins its request and its trace. Only
// the *Context logging calls (InfoContext, WarnContext, …) pass a context; the
// plain ones log without these fields, exactly as before.
type ContextHandler struct {
	inner slog.Handler
}

// NewContextHandler wraps inner.
func NewContextHandler(inner slog.Handler) *ContextHandler {
	return &ContextHandler{inner: inner}
}

// Enabled implements slog.Handler.
func (h *ContextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

// Handle implements slog.Handler.
func (h *ContextHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		if id := RequestIDFrom(ctx); id != "" {
			r.AddAttrs(slog.String("request_id", id))
		}
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			r.AddAttrs(slog.String("trace_id", sc.TraceID().String()),
				slog.String("span_id", sc.SpanID().String()))
		}
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs implements slog.Handler.
func (h *ContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ContextHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup implements slog.Handler.
func (h *ContextHandler) WithGroup(name string) slog.Handler {
	return &ContextHandler{inner: h.inner.WithGroup(name)}
}
