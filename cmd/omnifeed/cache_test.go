package main

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

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
