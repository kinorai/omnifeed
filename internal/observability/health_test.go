package observability

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// get drives a path through a freshly-registered mux against the same Health,
// so the readiness cache (held on h) is shared across calls.
func get(h *Health, path string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	h.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func okCheck(context.Context) error { return nil }

func TestLivez_OK(t *testing.T) {
	rec := get(NewHealth(0), "/livez")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "alive") {
		t.Fatalf("body: got %q, want it to contain 'alive'", rec.Body.String())
	}
}

func TestReadyz_AllChecksPass(t *testing.T) {
	rec := get(NewHealth(0, okCheck), "/readyz")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ready") {
		t.Fatalf("body: got %q, want it to contain 'ready'", rec.Body.String())
	}
}

func TestReadyz_FailingCheckIs503(t *testing.T) {
	h := NewHealth(0, func(context.Context) error { return errors.New("crawl4ai down") })
	rec := get(h, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "crawl4ai down") {
		t.Fatalf("body: got %q, want the failing check's error", rec.Body.String())
	}
}

// /healthz and /health are back-compat aliases of /readyz. Deleting either route
// trips this test — turning a silent probe break into a loud one (item 7).
func TestReadyAliases(t *testing.T) {
	for _, path := range []string{"/healthz", "/health"} {
		t.Run(path, func(t *testing.T) {
			rec := get(NewHealth(0, okCheck), path)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: got %d, want 200 (alias of /readyz)", path, rec.Code)
			}
		})
	}
}

func TestMarkShuttingDown_FailsLiveAndReady(t *testing.T) {
	h := NewHealth(0, okCheck)
	h.MarkShuttingDown()
	for _, path := range []string{"/livez", "/readyz"} {
		if rec := get(h, path); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s after shutdown: got %d, want 503", path, rec.Code)
		}
	}
}

// Readiness results are cached for cacheTTL — the check runs once across two
// probes inside the window.
func TestReadyz_CachesWithinTTL(t *testing.T) {
	var calls atomic.Int64
	h := NewHealth(time.Minute, func(context.Context) error { calls.Add(1); return nil })
	get(h, "/readyz")
	get(h, "/readyz")
	if calls.Load() != 1 {
		t.Fatalf("check calls: got %d, want 1 (cached)", calls.Load())
	}
}
