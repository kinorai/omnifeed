package observability

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"
)

// ReadyCheck is a function that reports whether the binary is ready to serve.
// Return nil when ready, an error describing why not otherwise.
type ReadyCheck func(context.Context) error

// Health exposes Kubernetes-style health endpoints.
//
//	/livez   — process is up; always 200 unless we're shutting down
//	/readyz  — runs all ReadyCheck functions; 200 only if all pass
//	/healthz — alias of /readyz for backwards compat
type Health struct {
	ready       []ReadyCheck
	cacheResult atomic.Pointer[readyResult]
	cacheTTL    time.Duration
	shutdown    atomic.Bool
}

type readyResult struct {
	at  time.Time
	err error
}

// NewHealth returns a Health with the given readiness checks. Results are
// cached for `cacheTTL` to avoid hammering upstreams on every probe.
func NewHealth(cacheTTL time.Duration, checks ...ReadyCheck) *Health {
	return &Health{ready: checks, cacheTTL: cacheTTL}
}

// MarkShuttingDown causes /livez to return 503 so the LB stops sending traffic.
func (h *Health) MarkShuttingDown() { h.shutdown.Store(true) }

// Register attaches the endpoints to mux.
func (h *Health) Register(mux *http.ServeMux) {
	mux.HandleFunc("/livez", h.live)
	mux.HandleFunc("/readyz", h.readyHandler)
	mux.HandleFunc("/healthz", h.readyHandler)
	mux.HandleFunc("/health", h.readyHandler) // back-compat with previous endpoint
}

func (h *Health) live(w http.ResponseWriter, _ *http.Request) {
	if h.shutdown.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"alive"}`))
}

func (h *Health) readyHandler(w http.ResponseWriter, r *http.Request) {
	if h.shutdown.Load() {
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	if err := h.check(r.Context()); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "not_ready", "error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ready"}`))
}

func (h *Health) check(ctx context.Context) error {
	if cached := h.cacheResult.Load(); cached != nil && time.Since(cached.at) < h.cacheTTL {
		return cached.err
	}
	for _, check := range h.ready {
		if err := check(ctx); err != nil {
			h.cacheResult.Store(&readyResult{at: time.Now(), err: err})
			return err
		}
	}
	h.cacheResult.Store(&readyResult{at: time.Now()})
	return nil
}
