package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/config"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/httpx/redislimit"
	"github.com/kinorai/omnifeed/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"
)

// searxngLimiter returns the httpx.Limiter interface, so an unconfigured
// deployment must get a LITERAL nil back. A nil *DomainLimiter stored in the
// interface is non-nil, and every call site tests `limiter != nil` to decide
// whether pacing is on — a typed nil would turn pacing "off" into a nil-pointer
// panic on the first query.
func TestSearxngLimiterUnconfiguredIsNil(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := observability.NewMetrics()

	tests := []struct {
		name    string
		cfg     config.Config
		wantNil bool
	}{
		{
			name:    "neither control set",
			cfg:     config.Config{},
			wantNil: true,
		},
		{
			name:    "delay set",
			cfg:     config.Config{SearXNGDelay: time.Second},
			wantNil: false,
		},
		{
			name:    "quota set",
			cfg:     config.Config{SearXNGQuota: 14, SearXNGQuotaWindow: 90 * time.Second},
			wantNil: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := searxngLimiter(tt.cfg, nil, metrics, logger)
			if (got == nil) != tt.wantNil {
				t.Fatalf("searxngLimiter() == nil is %v, want %v (got %#v)", got == nil, tt.wantNil, got)
			}
		})
	}
}

// TestSearxngLimiterUnconfiguredIsNilWithRedis is the same invariant with the
// distributed backend on: a shared backend has nothing to share when no limit
// is configured, and the redis path must not turn "pacing off" into a
// FallbackLimiter that paces nothing.
func TestSearxngLimiterUnconfiguredIsNilWithRedis(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := observability.NewMetrics()
	// Constructed, never dialed: go-redis connects lazily.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = rdb.Close() })

	if got := searxngLimiter(config.Config{}, rdb, metrics, logger); got != nil {
		t.Fatalf("searxngLimiter() = %#v, want nil", got)
	}
}

// TestBuildLimiterBackends pins which implementation each configuration gets:
// no Redis must return the very limiter it was handed (today's behavior, byte
// for byte), and Redis must wrap it in the fail-open composite.
func TestBuildLimiterBackends(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	metrics := observability.NewMetrics()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = rdb.Close() })

	local := httpx.NewDomainLimiter(2, time.Second)
	got := buildLimiter("domain", nil, local, redislimit.Config{}, metrics, logger)
	if got != httpx.Limiter(local) {
		t.Fatalf("without redis: buildLimiter() = %#v, want the local limiter %p", got, local)
	}

	got = buildLimiter("domain", rdb, httpx.NewDomainLimiter(2, time.Second),
		redislimit.Config{Prefix: "omnifeed:ratelimit", MaxConcurrent: 2, MinDelay: time.Second},
		metrics, logger)
	fb, ok := got.(*httpx.FallbackLimiter)
	if !ok {
		t.Fatalf("with redis: buildLimiter() = %T, want *httpx.FallbackLimiter", got)
	}
	if _, ok := fb.Primary.(*redislimit.Limiter); !ok {
		t.Fatalf("Primary = %T, want *redislimit.Limiter", fb.Primary)
	}
	if _, ok := fb.Fallback.(*httpx.DomainLimiter); !ok {
		t.Fatalf("Fallback = %T, want *httpx.DomainLimiter", fb.Fallback)
	}
	if fb.OnDegraded == nil {
		t.Fatal("OnDegraded is nil — degradation would be invisible in metrics and logs")
	}
}

// stubPacer records what the wiring asked it to penalize.
type stubPacer struct {
	httpx.Limiter
	penalties []time.Duration
	rawURL    string
}

func (s *stubPacer) Penalize(rawURL string, d time.Duration) {
	s.penalties = append(s.penalties, d)
	s.rawURL = rawURL
}

// The cap lives at the wiring site: httpx.Client reports the fact an upstream
// stated, and main decides how much of it to honor. A hostile or misconfigured
// upstream must not be able to hold a host for a day with one header.
func TestRetryAfterHookCapsThePenalty(t *testing.T) {
	metrics := observability.NewMetrics()
	p := &stubPacer{}
	hook := retryAfterHook(p, metrics)

	hook("hackernews", "https://news.ycombinator.com/item?id=1", time.Hour)
	hook("hackernews", "https://news.ycombinator.com/item?id=1", 30*time.Second)

	want := []time.Duration{retryAfterCap, 30 * time.Second}
	if len(p.penalties) != len(want) || p.penalties[0] != want[0] || p.penalties[1] != want[1] {
		t.Fatalf("penalties = %v, want %v", p.penalties, want)
	}
	if p.rawURL != "https://news.ycombinator.com/item?id=1" {
		t.Fatalf("rawURL = %q", p.rawURL)
	}
}

// Pacing can be off (searxngLimiter returns nil) while the client still sees a
// 429: the hook must count it and not panic.
func TestRetryAfterHookWithoutALimiter(t *testing.T) {
	hook := retryAfterHook(nil, observability.NewMetrics())
	hook("searxng", "http://searxng:8080/search", time.Second)
}

// crawl4ai is a proxy: the request that gets the 429 goes to the crawl4ai
// endpoint, while the limiter paced the target site's host. Penalizing would
// hold back a host no limiter reads, and the counter must not claim a penalty
// that never happened.
func TestRetryAfterHookSkipsTheCrawl4aiProxy(t *testing.T) {
	metrics := observability.NewMetrics()
	p := &stubPacer{}
	hook := retryAfterHook(p, metrics)

	hook("crawl4ai", "http://crawl4ai:11235/crawl", time.Minute)

	if len(p.penalties) != 0 {
		t.Fatalf("penalties = %v, want none — the paced host is not the one that answered", p.penalties)
	}
	if got := counterValue(t, metrics.RatelimitPenalties.WithLabelValues("crawl4ai")); got != 0 {
		t.Fatalf("omnifeed_ratelimit_penalties_total{upstream=crawl4ai} = %v, want 0", got)
	}

	// Every other upstream is an engine's own call, and still penalized.
	hook("hackernews", "https://news.ycombinator.com/item?id=1", time.Minute)
	if len(p.penalties) != 1 {
		t.Fatalf("penalties = %v, want one", p.penalties)
	}
}

// counterValue reads a CounterVec child's current value.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var dm dto.Metric
	if err := c.Write(&dm); err != nil {
		t.Fatal(err)
	}
	return dm.GetCounter().GetValue()
}

// A GaugeVec emits no series until it is written, so an alert cannot tell a
// healthy deployment from a metric that never appeared. Redis-on wiring must
// publish the scope at 0; Redis-off wiring must publish nothing, because there
// is no distributed backend to be degraded from.
func TestBuildLimiterPreRegistersDegradedGauge(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	off := observability.NewMetrics()
	buildLimiter("domain", nil, httpx.NewDomainLimiter(2, time.Second),
		redislimit.Config{}, off, logger)
	if got := gaugeVecSeries(t, off.RatelimitDegraded); len(got) != 0 {
		t.Fatalf("without redis: series = %v, want none", got)
	}

	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = rdb.Close() })

	on := observability.NewMetrics()
	buildLimiter("searxng", rdb, httpx.NewDomainLimiter(2, time.Second),
		redislimit.Config{Prefix: "omnifeed:ratelimit", MaxConcurrent: 2, MinDelay: time.Second},
		on, logger)
	got := gaugeVecSeries(t, on.RatelimitDegraded)
	want := map[string]float64{"searxng": 0}
	if len(got) != len(want) || got["searxng"] != want["searxng"] {
		t.Fatalf("with redis: series = %v, want %v", got, want)
	}
}

// gaugeVecSeries collects every series a GaugeVec currently emits, keyed by its
// single label value.
func gaugeVecSeries(t *testing.T, v *prometheus.GaugeVec) map[string]float64 {
	t.Helper()
	ch := make(chan prometheus.Metric, 16)
	v.Collect(ch)
	close(ch)

	out := map[string]float64{}
	for m := range ch {
		var dm dto.Metric
		if err := m.Write(&dm); err != nil {
			t.Fatal(err)
		}
		labels := dm.GetLabel()
		if len(labels) != 1 {
			t.Fatalf("series has %d labels, want 1", len(labels))
		}
		out[labels[0].GetValue()] = dm.GetGauge().GetValue()
	}
	return out
}

// The concurrency cap must work without Redis: a single instance IS the
// deployment, so the number the operator set is the number the engines see. The
// limiter is checked by behaviour rather than by type, since both modes are a
// *httpx.DomainLimiter.
func TestSearxngLocalLimiterHonorsConcurrencyWithoutRedis(t *testing.T) {
	cfg := config.Config{SearXNGDelay: time.Second, SearXNGConcurrency: 3}
	limiter := searxngLocalLimiter(cfg, nil)

	// Three callers arrive together and none releases. All three must be
	// admitted, and the delay must space them rather than serialize them: the
	// completion-spaced limiter would cap this at one.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	admitted := make(chan struct{}, 3)
	for range 3 {
		go func() {
			if _, err := limiter.Acquire(ctx, "searxng", "https://searxng.test/search"); err != nil {
				return
			}
			admitted <- struct{}{}
		}()
	}
	for i := range 3 {
		select {
		case <-admitted:
		case <-ctx.Done():
			t.Fatalf("only %d of 3 callers admitted before the deadline", i)
		}
	}
}

// With Redis on, the local limiter is the degraded-mode fallback and stays at
// concurrency 1 on purpose: a blip must not release the whole cluster cap at
// once from every replica.
func TestSearxngLocalLimiterStaysSerialAsTheRedisFallback(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = rdb.Close() })

	cfg := config.Config{SearXNGDelay: time.Second, SearXNGConcurrency: 8}
	limiter := searxngLocalLimiter(cfg, rdb)

	if _, err := limiter.Acquire(context.Background(), "searxng", "https://searxng.test/search"); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := limiter.Acquire(ctx, "searxng", "https://searxng.test/search"); err == nil {
		t.Fatal("second Acquire succeeded, want it held behind the single fallback slot")
	}
}
