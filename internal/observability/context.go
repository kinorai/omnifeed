package observability

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"math/rand/v2"
	"net/http"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// RequestIDHeader is the header a caller may send to name its request, and the
// header every response carries back, so one id joins the caller's logs,
// omnifeed's logs and the MCP result (_meta.request_id).
const RequestIDHeader = "X-Request-Id"

// maxRequestIDLen bounds a caller-supplied id: it lands in every log line of
// the request, so an unbounded header would be a log-amplification lever.
const maxRequestIDLen = 128

type ctxKey int

const (
	requestIDKey ctxKey = iota
	queryIDKey
)

// WithRequestID returns ctx carrying id as the request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestIDFrom returns the request id carried by ctx, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey).(string)
	return id
}

// NewRequestID returns a fresh random request id (16 bytes, hex).
func NewRequestID() string {
	var b [16]byte
	_, _ = crand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ValidRequestID reports whether a caller-supplied id is safe to adopt: 1-128
// characters from [A-Za-z0-9._:-]. Anything else (empty, too long, spaces,
// quotes, control bytes) is replaced by a generated id rather than echoed into
// logs and response headers.
func ValidRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

// RequestID is the HTTP middleware that adopts the caller's X-Request-Id when
// it is valid (see ValidRequestID) or generates one, puts it in the request
// context and returns it in the response header.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !ValidRequestID(id) {
			id = NewRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		// Joins the trace to the logs and to what the caller quotes.
		trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("request.id", id))
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

// WithQueryID returns ctx carrying the search correlation id (query_id) the
// searcher should use for its audit lines, so the caller can log the same id
// on its own completion line.
func WithQueryID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, queryIDKey, id)
}

// QueryIDFrom returns the search correlation id carried by ctx, or "".
func QueryIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(queryIDKey).(string)
	return id
}

// NewQueryID returns a correlation id for one search. Non-cryptographic: it
// only has to be unique among the searches a log store holds at once, and it
// is never a secret or a capability.
func NewQueryID() string {
	return strconv.FormatUint(rand.Uint64(), 36) //nolint:gosec // log correlation id, not a security token
}
