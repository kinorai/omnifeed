package main

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"

	"github.com/kinorai/omnifeed/internal/config"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"github.com/kinorai/omnifeed/internal/fetchcache"
	"github.com/kinorai/omnifeed/internal/observability"
)

type countedEngine struct {
	name  string
	calls *atomic.Int32
}

func (e countedEngine) Name() string      { return e.name }
func (countedEngine) Matches(string) bool { return true }
func (e countedEngine) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	e.calls.Add(1)
	return domain.Document{PageContent: "thread", Metadata: map[string]string{}}, nil
}

// fetchCache with no Redis is the in-process LRU, and the engines it is handed
// get OMNIFEED_CACHE_TTL_THREADS rather than the pages TTL. A zero threads TTL
// makes that observable: those engines stop being cached even though the pages
// TTL is an hour.
func TestFetchCacheWiring(t *testing.T) {
	var threadCalls atomic.Int32
	thread := countedEngine{name: "reddit", calls: &threadCalls}
	reg := engine.New().Register(thread).Fallback(countedEngine{name: "crawl4ai", calls: new(atomic.Int32)})
	cfg := config.Config{CacheTTLThreads: 0, CacheTTLPages: time.Hour, CacheMaxBytes: 1 << 20, CacheMaxItemBytes: 1 << 20}
	c := fetchCache(cfg, reg, nil, observability.NewMetrics(), slog.New(slog.NewTextHandler(io.Discard, nil)), thread)

	for range 2 {
		doc, err := c.Crawl(context.Background(), "https://www.reddit.com/r/x/comments/abc/", domain.EngineOptions{})
		if err != nil || doc.Metadata[fetchcache.MetaCache] != fetchcache.ResultMiss {
			t.Fatalf("thread crawl: err=%v meta=%v", err, doc.Metadata)
		}
	}
	if threadCalls.Load() != 2 {
		t.Errorf("thread engine calls = %d, want 2 (threads TTL 0 = uncached)", threadCalls.Load())
	}
}

// The 2026-10-08 wiring: a Redis whose ACL user may not touch the cache prefix.
// fetchCache probes it at build time and serves from the in-process LRU, so a
// repeated fetch is a hit (in the incident, every one was a miss).
func TestFetchCacheWiring_RedisNOPERMFallsBackToMemory(t *testing.T) {
	mr := miniredis.RunT(t)
	mr.SetError("NOPERM No permissions to access a key")
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })

	var calls atomic.Int32
	thread := countedEngine{name: "reddit", calls: &calls}
	reg := engine.New().Register(thread).Fallback(countedEngine{name: "crawl4ai", calls: new(atomic.Int32)})
	cfg := config.Config{CacheTTLThreads: time.Minute, CacheTTLPages: time.Hour, CacheMaxBytes: 1 << 20,
		CacheMaxItemBytes: 1 << 20, CacheKeyPrefix: "omnifeed:cache", RedisTimeout: 250 * time.Millisecond}
	m := observability.NewMetrics()
	c := fetchCache(cfg, reg, rdb, m, slog.New(slog.NewTextHandler(io.Discard, nil)), thread)

	for i, want := range []string{fetchcache.ResultMiss, fetchcache.ResultHit, fetchcache.ResultHit, fetchcache.ResultHit} {
		doc, err := c.Crawl(context.Background(), "https://www.reddit.com/r/x/comments/abc/", domain.EngineOptions{})
		if err != nil || doc.Metadata[fetchcache.MetaCache] != want || doc.Metadata[fetchcache.MetaCachedAt] == "" {
			t.Fatalf("fetch %d: err=%v meta=%v, want %s with cached_at", i, err, doc.Metadata, want)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("engine calls = %d, want 1", calls.Load())
	}
	var g dto.Metric
	if err := m.CacheBackend.WithLabelValues("memory").Write(&g); err != nil || g.GetGauge().GetValue() != 1 {
		t.Errorf(`omnifeed_cache_backend{backend="memory"} = %v (err %v), want 1`, g.GetGauge().GetValue(), err)
	}
}
