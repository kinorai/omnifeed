package fetchcache

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	dto "github.com/prometheus/client_model/go"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/observability"
)

// The 2026-10-08 incident: the Redis ACL user only had ~omnifeed:ratelimit:*,
// so every command on omnifeed:cache:* answered this.
const noperm = "NOPERM No permissions to access a key"

// syncBuffer is a log sink safe for concurrent handlers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncBuffer) count(level string) int {
	return strings.Count(s.String(), "level="+level)
}

// failoverOn builds a Failover over mr with metrics and a captured log, the
// way main wires it.
func failoverOn(t *testing.T, mr *miniredis.Miniredis, clock *fakeClock, m *observability.Metrics) (*Failover, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	rb, _ := redisBackendOn(t, mr, clock)
	rb.onError = m.ObserveCacheBackendError
	f := NewFailover(FailoverConfig{
		Redis:     rb,
		Memory:    memBackend(clock),
		Prefix:    "omnifeed:cache",
		Logger:    slog.New(slog.NewTextHandler(logs, nil)),
		OnBackend: m.SetCacheBackend,
	})
	return f, logs
}

func backendGauge(m *observability.Metrics, name string) float64 {
	var out dto.Metric
	if err := m.CacheBackend.WithLabelValues(name).Write(&out); err != nil {
		panic(err)
	}
	return out.GetGauge().GetValue()
}

func backendErrors(m *observability.Metrics, op, kind string) float64 {
	var out dto.Metric
	if err := m.CacheBackendErrors.WithLabelValues(op, kind).Write(&out); err != nil {
		panic(err)
	}
	return out.GetCounter().GetValue()
}

// NOPERM at startup: the probe switches to the in-process cache, says so once
// at ERROR with what to fix, and identical fetches then hit — the incident's
// four sequential fetches were four misses.
func TestFailover_StartupNOPERMFallsBackToMemory(t *testing.T) {
	clock := newClock()
	mr := miniredis.RunT(t)
	mr.SetError(noperm)
	m := observability.NewMetrics()
	f, logs := failoverOn(t, mr, clock, m)

	f.Probe(context.Background(), 100*time.Millisecond)

	if f.Backend() != BackendMemory {
		t.Fatalf("backend after a NOPERM probe = %q, want memory", f.Backend())
	}
	if n := logs.count("ERROR"); n != 1 {
		t.Fatalf("ERROR lines = %d, want 1; log:\n%s", n, logs)
	}
	for _, want := range []string{"cannot write omnifeed:cache:*", "NOPERM", "grant ~omnifeed:cache:*", "OMNIFEED_CACHE_KEY_PREFIX"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("ERROR log lacks %q; log:\n%s", want, logs)
		}
	}
	if backendGauge(m, BackendMemory) != 1 || backendGauge(m, BackendRedis) != 0 {
		t.Errorf("omnifeed_cache_backend memory=%v redis=%v, want 1/0",
			backendGauge(m, BackendMemory), backendGauge(m, BackendRedis))
	}
	if got := backendErrors(m, "probe", ErrKindNoPerm); got != 1 {
		t.Errorf(`backend_errors_total{op="probe",kind="noperm"} = %v, want 1`, got)
	}

	inner := &stubDispatcher{engine: "reddit"}
	c := newCache(inner, f, clock, m)
	for i := range 4 {
		doc, err := c.Crawl(context.Background(), testURL, domain.EngineOptions{})
		if err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
		want := ResultHit
		if i == 0 {
			want = ResultMiss
		}
		if doc.Metadata[MetaCache] != want || doc.Metadata[MetaCachedAt] == "" {
			t.Errorf("fetch %d meta = %v, want %s with cached_at", i, doc.Metadata, want)
		}
	}
	if inner.calls.Load() != 1 {
		t.Errorf("upstream calls = %d, want 1", inner.calls.Load())
	}
	// Redis is never asked again, and nothing more is logged.
	if backendErrors(m, "get", ErrKindNoPerm)+backendErrors(m, "set", ErrKindNoPerm) != 0 || logs.count("ERROR") != 1 {
		t.Errorf("redis still in use after the switch; log:\n%s", logs)
	}
	if counter(m, ResultError) != 0 || counter(m, ResultHit) != 3 || counter(m, ResultMiss) != 1 {
		t.Errorf("requests error=%v hit=%v miss=%v, want 0/3/1",
			counter(m, ResultError), counter(m, ResultHit), counter(m, ResultMiss))
	}
}

// NOPERM after a healthy start (an ACL tightened under a running pod): the
// first refusal switches this replica to memory, once — even when many
// requests hit the refusal together — and that very fetch is stored there.
func TestFailover_RuntimeNOPERMSwitchesOnce(t *testing.T) {
	clock := newClock()
	mr := miniredis.RunT(t)
	m := observability.NewMetrics()
	f, logs := failoverOn(t, mr, clock, m)
	f.Probe(context.Background(), 100*time.Millisecond)
	if f.Backend() != BackendRedis || backendGauge(m, BackendRedis) != 1 {
		t.Fatalf("healthy probe: backend=%q gauge=%v, want redis/1", f.Backend(), backendGauge(m, BackendRedis))
	}
	if !strings.Contains(logs.String(), "redis probe ok") {
		t.Errorf("no probe-ok line; log:\n%s", logs)
	}

	mr.SetError(noperm)
	inner := &stubDispatcher{engine: "reddit"}
	c := newCache(inner, f, clock, m)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u := fmt.Sprintf("%s/%d", testURL, i)
			if doc, err := c.Crawl(context.Background(), u, domain.EngineOptions{}); err != nil || doc.Metadata[MetaCachedAt] == "" {
				t.Errorf("%s: err=%v meta=%v, want a stored miss", u, err, doc.Metadata)
			}
		}()
	}
	wg.Wait()

	if f.Backend() != BackendMemory || backendGauge(m, BackendMemory) != 1 || backendGauge(m, BackendRedis) != 0 {
		t.Fatalf("backend=%q gauges memory=%v redis=%v, want memory 1/0",
			f.Backend(), backendGauge(m, BackendMemory), backendGauge(m, BackendRedis))
	}
	if n := logs.count("ERROR"); n != 1 {
		t.Errorf("ERROR lines = %d, want exactly 1; log:\n%s", n, logs)
	}
	if got := backendErrors(m, "get", ErrKindNoPerm); got < 1 {
		t.Errorf(`backend_errors_total{op="get",kind="noperm"} = %v, want >= 1`, got)
	}
	if got := backendErrors(m, "get", ErrKindTransient); got != 0 {
		t.Errorf("NOPERM counted as transient %v times", got)
	}

	// Redis recovering does not flip the replica back: the switch is for the
	// process lifetime, and the stored entries keep serving.
	mr.SetError("")
	calls := inner.calls.Load()
	doc, err := c.Crawl(context.Background(), testURL+"/0", domain.EngineOptions{})
	if err != nil || doc.Metadata[MetaCache] != ResultHit || inner.calls.Load() != calls {
		t.Errorf("after the switch: err=%v meta=%v calls %d->%d, want a memory hit",
			err, doc.Metadata, calls, inner.calls.Load())
	}
	for _, k := range mr.Keys() {
		if !strings.HasPrefix(k, "omnifeed:cache:probe:") {
			t.Errorf("redis written after the switch: %q", k)
		}
	}
}

// The startup probe tells apart a Redis that refused (switch) from one that
// did not answer or asked to retry (keep Redis: the cooldown covers it).
func TestFailover_ProbeClassifiesStartupErrors(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(*miniredis.Miniredis)
		backend string
		kind    string
		level   string
	}{
		{"healthy", func(*miniredis.Miniredis) {}, BackendRedis, "", "INFO"},
		{"noperm", func(mr *miniredis.Miniredis) { mr.SetError(noperm) }, BackendMemory, ErrKindNoPerm, "ERROR"},
		{"noauth", func(mr *miniredis.Miniredis) { mr.SetError("NOAUTH Authentication required.") }, BackendMemory, ErrKindNoPerm, "ERROR"},
		{"readonly replica", func(mr *miniredis.Miniredis) {
			mr.SetError("READONLY You can't write against a read only replica.")
		}, BackendMemory, ErrKindOther, "ERROR"},
		{"connection refused", func(mr *miniredis.Miniredis) { mr.Close() }, BackendRedis, ErrKindTransient, "WARN"},
		{"loading", func(mr *miniredis.Miniredis) {
			mr.SetError("LOADING Redis is loading the dataset in memory")
		}, BackendRedis, ErrKindTransient, "WARN"},
		{"oom", func(mr *miniredis.Miniredis) {
			mr.SetError("OOM command not allowed when used memory > 'maxmemory'.")
		}, BackendRedis, ErrKindTransient, "WARN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := newClock()
			mr := miniredis.RunT(t)
			m := observability.NewMetrics()
			f, logs := failoverOn(t, mr, clock, m)
			tc.setup(mr)

			f.Probe(context.Background(), 100*time.Millisecond)

			if f.Backend() != tc.backend || backendGauge(m, tc.backend) != 1 {
				t.Errorf("backend=%q gauge=%v, want %s/1", f.Backend(), backendGauge(m, tc.backend), tc.backend)
			}
			if tc.kind != "" && backendErrors(m, "probe", tc.kind) != 1 {
				t.Errorf(`backend_errors_total{op="probe",kind=%q} = %v, want 1`, tc.kind, backendErrors(m, "probe", tc.kind))
			}
			if logs.count(tc.level) != 1 || (tc.level != "ERROR" && logs.count("ERROR") != 0) {
				t.Errorf("want one %s line and no other ERROR; log:\n%s", tc.level, logs)
			}
			if tc.name == "healthy" {
				for _, k := range mr.Keys() {
					if !strings.HasPrefix(k, "omnifeed:cache:probe:") {
						t.Errorf("probe wrote %q outside the cache prefix", k)
					}
					if ttl := mr.TTL(k); ttl <= 0 || ttl > probeTTL {
						t.Errorf("probe key %q TTL %s, want (0, %s]", k, ttl, probeTTL)
					}
				}
			}
		})
	}
}

// A transient error at runtime keeps the Redis backend on its cooldown path:
// counted as transient, served as a miss, no switch, no ERROR.
func TestFailover_RuntimeTransientKeepsRedis(t *testing.T) {
	clock := newClock()
	mr := miniredis.RunT(t)
	m := observability.NewMetrics()
	f, logs := failoverOn(t, mr, clock, m)
	f.Probe(context.Background(), 100*time.Millisecond)
	mr.Close()

	inner := &stubDispatcher{engine: "reddit"}
	c := newCache(inner, f, clock, m)
	for range 2 {
		doc, err := c.Crawl(context.Background(), testURL, domain.EngineOptions{})
		if err != nil || doc.Metadata[MetaCache] != ResultMiss {
			t.Fatalf("err=%v meta=%v, want a miss", err, doc.Metadata)
		}
	}
	if f.Backend() != BackendRedis || backendGauge(m, BackendRedis) != 1 {
		t.Errorf("backend=%q, want redis kept", f.Backend())
	}
	if !f.redis.coolingDown() {
		t.Error("transient error did not trip the cooldown")
	}
	// One GET reached the dead Redis; everything after failed fast.
	if got := backendErrors(m, "get", ErrKindTransient); got != 1 {
		t.Errorf(`backend_errors_total{op="get",kind="transient"} = %v, want 1`, got)
	}
	if logs.count("ERROR") != 0 {
		t.Errorf("transient error logged at ERROR; log:\n%s", logs)
	}
}
