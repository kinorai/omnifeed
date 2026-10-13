package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"
)

func TestRouteOfIsBounded(t *testing.T) {
	cases := map[string]string{
		"/crawl":         "/crawl",
		"/search":        "/search",
		"/mcp":           "/mcp",
		"/mcp/sse":       "/mcp/sse",
		"/livez":         "/livez",
		"/readyz":        "/readyz",
		"/metrics":       "/metrics",
		"/healthz":       RouteOther,
		"/":              RouteOther,
		"/crawl/123":     RouteOther,
		"/debug/pprof/":  RouteOther,
		"/mcp/../secret": RouteOther,
	}
	for path, want := range cases {
		if got := RouteOf(path); got != want {
			t.Errorf("RouteOf(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestHTTPMiddlewareRecordsRED(t *testing.T) {
	m := NewMetrics()
	h := m.HTTPMiddleware(ServerLoader, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/crawl" {
			w.WriteHeader(http.StatusTeapot)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	for _, p := range []string{"/crawl", "/x/1", "/x/2"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, p, nil))
	}
	if got := counterValue(t, m.HTTPRequests.WithLabelValues("loader", "/crawl", "418")); got != 1 {
		t.Fatalf(`http_requests_total{/crawl,418} = %v, want 1`, got)
	}
	if got := counterValue(t, m.HTTPRequests.WithLabelValues("loader", "other", "200")); got != 2 {
		t.Fatalf(`http_requests_total{other,200} = %v, want 2`, got)
	}
	if got := histogramCount(t, m.HTTPSecs.WithLabelValues("loader", "other")); got != 2 {
		t.Fatalf("http_request_seconds{other} count = %d, want 2", got)
	}
	if got := gaugeValue(t, m.HTTPInFlight.WithLabelValues("loader")); got != 0 {
		t.Fatalf("in flight after requests = %v, want 0", got)
	}
}

// A handler panic is counted, answered 500, and the process survives.
func TestHTTPMiddlewareRecoversPanics(t *testing.T) {
	m := NewMetrics()
	before := counterValue(t, panicsTotal.WithLabelValues(PanicHTTP))
	h := m.HTTPMiddleware(ServerMCP, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := counterValue(t, panicsTotal.WithLabelValues(PanicHTTP)) - before; got != 1 {
		t.Fatalf(`panics_total{where="http"} delta = %v, want 1`, got)
	}
	if got := counterValue(t, m.HTTPRequests.WithLabelValues("mcp", "/mcp", "500")); got != 1 {
		t.Fatalf("a recovered panic must be counted as a 500, got %v", got)
	}
}

// A panic after the response started cannot become a 500: the middleware
// counts it, then aborts the connection (http.ErrAbortHandler) so the client
// never takes the truncated body for a complete one.
func TestHTTPMiddlewareAbortsPanicAfterWrite(t *testing.T) {
	m := NewMetrics()
	before := counterValue(t, panicsTotal.WithLabelValues(PanicHTTP))
	h := m.HTTPMiddleware(ServerLoader, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[{\"partial\":"))
		panic("boom")
	}))
	func() {
		defer func() {
			if v := recover(); v != http.ErrAbortHandler { //nolint:errorlint // sentinel identity
				t.Fatalf("recovered %v, want http.ErrAbortHandler", v)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/crawl", nil))
	}()
	if got := counterValue(t, panicsTotal.WithLabelValues(PanicHTTP)) - before; got != 1 {
		t.Fatalf(`panics_total{where="http"} delta = %v, want 1`, got)
	}
	if got := gaugeValue(t, m.HTTPInFlight.WithLabelValues("loader")); got != 0 {
		t.Fatalf("in flight after abort = %v, want 0", got)
	}
}

// The SSE endpoint type-asserts http.Flusher; the recorder must keep it, and
// stream durations stay out of the latency histogram.
func TestHTTPMiddlewareKeepsFlusherAndSkipsStreams(t *testing.T) {
	m := NewMetrics()
	h := m.HTTPMiddleware(ServerMCP, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Error("wrapped writer lost http.Flusher")
		}
		if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
			t.Errorf("ResponseController: %v", err)
		}
		w.(http.Flusher).Flush()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/mcp/sse", nil))
	if got := counterValue(t, m.HTTPRequests.WithLabelValues("mcp", "/mcp/sse", "200")); got != 1 {
		t.Fatalf("stream request count = %v, want 1", got)
	}
	if got := histogramCount(t, m.HTTPSecs.WithLabelValues("mcp", "/mcp/sse")); got != 0 {
		t.Fatalf("stream observed in latency histogram (%d), want 0", got)
	}
}

func TestRecoverCountsAndReturnsError(t *testing.T) {
	before := counterValue(t, panicsTotal.WithLabelValues(PanicGitHubFanout))
	err := func() (err error) {
		defer Recover(PanicGitHubFanout, &err)
		panic("upstream returned nothing")
	}()
	if err == nil || !strings.Contains(err.Error(), "panic in github_fanout") {
		t.Fatalf("err = %v, want a panic error", err)
	}
	if got := counterValue(t, panicsTotal.WithLabelValues(PanicGitHubFanout)) - before; got != 1 {
		t.Fatalf("panics_total delta = %v, want 1", got)
	}
	// An unknown site never mints a new series.
	ObservePanic("made-up", "x")
	if got := counterValue(t, panicsTotal.WithLabelValues("other")); got < 1 {
		t.Fatalf(`unknown site must count under "other", got %v`, got)
	}
}

func TestRequestIDMiddleware(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
	}))

	// A valid caller id is adopted and echoed.
	req := httptest.NewRequest(http.MethodPost, "/crawl", nil)
	req.Header.Set(RequestIDHeader, "digestif-abc.123")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen != "digestif-abc.123" || rec.Header().Get(RequestIDHeader) != "digestif-abc.123" {
		t.Fatalf("caller id not propagated: ctx=%q header=%q", seen, rec.Header().Get(RequestIDHeader))
	}

	// An unsafe one is replaced by a generated id.
	for _, bad := range []string{"", "has space", strings.Repeat("a", 129), "quote\"d"} {
		req = httptest.NewRequest(http.MethodPost, "/crawl", nil)
		req.Header.Set(RequestIDHeader, bad)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		got := rec.Header().Get(RequestIDHeader)
		if got == bad || len(got) != 32 || seen != got {
			t.Fatalf("bad id %q: header=%q ctx=%q, want a generated 32-hex id in both", bad, got, seen)
		}
	}
}

func TestContextHandlerInjectsIDs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewContextHandler(slog.NewJSONHandler(&buf, nil))).With("component", "x")

	tid, _ := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	sid, _ := trace.SpanIDFromHex("0102030405060708")
	ctx := trace.ContextWithSpanContext(WithRequestID(context.Background(), "req-1"),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	logger.InfoContext(ctx, "hello")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatal(err)
	}
	if line["request_id"] != "req-1" || line["trace_id"] != tid.String() || line["span_id"] != sid.String() || line["component"] != "x" {
		t.Fatalf("line = %v", line)
	}

	buf.Reset()
	logger.Info("plain")
	if strings.Contains(buf.String(), "request_id") || strings.Contains(buf.String(), "trace_id") {
		t.Fatalf("context-less line carries ids: %s", buf.String())
	}
}

func TestDependencyCheckerPublishesUp(t *testing.T) {
	m := NewMetrics()
	down := errors.New("refused")
	d := &DependencyChecker{
		Metrics: m,
		Logger:  slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		Checks: map[string]DependencyCheck{
			"crawl4ai": func(context.Context) error { return down },
			"searxng":  func(context.Context) error { return nil },
			"redis":    func(context.Context) error { panic("bad check") },
		},
	}
	d.CheckOnce(context.Background())
	for dep, want := range map[string]float64{"crawl4ai": 0, "searxng": 1, "redis": 0} {
		if got := gaugeValue(t, m.DependencyUp.WithLabelValues(dep)); got != want {
			t.Errorf("dependency_up{%s} = %v, want %v", dep, got, want)
		}
	}
	down = nil
	d.CheckOnce(context.Background())
	if got := gaugeValue(t, m.DependencyUp.WithLabelValues("crawl4ai")); got != 1 {
		t.Fatalf("crawl4ai recovered but gauge = %v", got)
	}
}

// Readiness no longer depends on upstreams: a Health with no checks is ready
// while live, and only shutdown takes it out of rotation.
func TestReadyzWithoutUpstreamChecks(t *testing.T) {
	h := NewHealth(time.Second)
	if rec := get(h, "/readyz"); rec.Code != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200", rec.Code)
	}
	h.MarkShuttingDown()
	if rec := get(h, "/readyz"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz while shutting down = %d, want 503", rec.Code)
	}
}

func TestBuildInfo(t *testing.T) {
	m := NewMetrics()
	m.SetBuildInfo("v1.2.3")
	if got := gaugeValue(t, m.BuildInfo.WithLabelValues("v1.2.3")); got != 1 {
		t.Fatalf("build_info = %v, want 1", got)
	}
}

// The registry exports the runtime GC/scheduler subset, not just the default
// Go collector set.
func TestGoRuntimeMetricsSubset(t *testing.T) {
	m := NewMetrics()
	mfs, err := m.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var gc, sched, cpu bool
	for _, mf := range mfs {
		n := mf.GetName()
		gc = gc || strings.HasPrefix(n, "go_gc_")
		sched = sched || strings.HasPrefix(n, "go_sched_")
		cpu = cpu || strings.HasPrefix(n, "go_cpu_classes_gc_")
		if strings.HasPrefix(n, "go_cpu_classes_scavenge") {
			t.Errorf("unexpected runtime metric outside the subset: %s", n)
		}
	}
	if !gc || !sched || !cpu {
		t.Fatalf("runtime subset missing: gc=%v sched=%v cpu_classes_gc=%v", gc, sched, cpu)
	}
}
