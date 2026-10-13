package observability

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Server names, the values of the omnifeed_http_* `server` label.
const (
	ServerLoader = "loader"
	ServerMCP    = "mcp"
	ServerObs    = "obs"
)

// RouteOther is the route label of every path outside the fixed set.
const RouteOther = "other"

// knownRoutes is the closed set of route labels. A path outside it is "other":
// paths are caller-controlled, so labelling by them would let any client mint
// series (and some carry ids).
var knownRoutes = map[string]bool{
	"/crawl": true, "/search": true, "/mcp": true, "/mcp/sse": true,
	"/livez": true, "/readyz": true, "/metrics": true,
}

// RouteOf maps a request path to its bounded route label.
func RouteOf(path string) string {
	if knownRoutes[path] {
		return path
	}
	return RouteOther
}

// isStream reports a request whose response is a long-lived SSE stream (the
// MCP GET endpoints). Its duration is the connection's lifetime, not a
// latency, so it is counted but kept out of the latency histogram.
func isStream(r *http.Request, route string) bool {
	return r.Method == http.MethodGet && (route == "/mcp" || route == "/mcp/sse")
}

// HTTPMiddleware records RED metrics for one server — requests by route and
// status code, latency by route, requests in flight — and recovers a handler
// panic: it is counted (omnifeed_panics_total{where="http"}), logged with its
// stack, and answered with a 500 when nothing was written yet, instead of
// net/http's bare connection reset (after a partial write, the connection is
// aborted instead, so a truncated body never passes for a complete one).
func (m *Metrics) HTTPMiddleware(server string, next http.Handler) http.Handler {
	inFlight := m.HTTPInFlight.WithLabelValues(server)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := RouteOf(r.URL.Path)
		start := time.Now()
		inFlight.Inc()
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				if errors.Is(asError(v), http.ErrAbortHandler) {
					// The deliberate abort net/http documents: re-panic so
					// the server drops the connection quietly, as it would.
					inFlight.Dec()
					m.observeHTTP(server, route, r, rec, start)
					panic(v)
				}
				ObservePanic(PanicHTTP, v)
				if rec.wrote {
					// Headers (and maybe part of the body) are already out:
					// a 500 is impossible, and returning normally would end
					// a truncated response as if it were complete. Abort the
					// connection so the client sees the failure.
					inFlight.Dec()
					m.observeHTTP(server, route, r, rec, start)
					panic(http.ErrAbortHandler)
				}
				http.Error(rec, "internal server error", http.StatusInternalServerError)
			}
			inFlight.Dec()
			m.observeHTTP(server, route, r, rec, start)
		}()
		next.ServeHTTP(rec, r)
	})
}

func (m *Metrics) observeHTTP(server, route string, r *http.Request, rec *statusRecorder, start time.Time) {
	code := rec.status
	if code == 0 {
		code = http.StatusOK
	}
	m.HTTPRequests.WithLabelValues(server, route, strconv.Itoa(code)).Inc()
	if !isStream(r, route) {
		m.HTTPSecs.WithLabelValues(server, route).Observe(time.Since(start).Seconds())
	}
}

func asError(v any) error {
	err, _ := v.(error)
	return err
}

// statusRecorder captures the status code. It keeps the optional interfaces
// the handlers rely on reachable: Flush directly (the SSE endpoint type-asserts
// http.Flusher), the rest through Unwrap (http.ResponseController, used to
// clear the SSE write deadline).
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

// WriteHeader records the first status code and forwards it.
func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status, s.wrote = code, true
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write records an implicit 200 and forwards the bytes.
func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status, s.wrote = http.StatusOK, true
	}
	return s.ResponseWriter.Write(b)
}

// Flush implements http.Flusher when the wrapped writer does.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		if !s.wrote {
			s.status, s.wrote = http.StatusOK, true
		}
		f.Flush()
	}
}

// Hijack implements http.Hijacker when the wrapped writer does.
func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := s.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
