package main

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/config"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/httpx/redislimit"
	"github.com/kinorai/omnifeed/internal/observability"
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
