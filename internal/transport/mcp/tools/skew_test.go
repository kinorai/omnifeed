package tools

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"github.com/kinorai/omnifeed/internal/engine/reddit"
	"github.com/kinorai/omnifeed/internal/fetchcache"
	"github.com/kinorai/omnifeed/internal/observability"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func count(t *testing.T, c prometheus.Collector) (counter float64, histogram uint64) {
	t.Helper()
	var m dto.Metric
	switch v := c.(type) {
	case prometheus.Counter:
		if err := v.Write(&m); err != nil {
			t.Fatal(err)
		}
		return m.GetCounter().GetValue(), 0
	case prometheus.Histogram:
		if err := v.Write(&m); err != nil {
			t.Fatal(err)
		}
		return 0, m.GetHistogram().GetSampleCount()
	}
	t.Fatalf("unsupported collector %T", c)
	return 0, 0
}

// The cache-hit skew: fetch_url times every call through the cache-wrapped
// dispatcher, so before the fix a hit (microseconds, no engine involved) was
// observed in omnifeed_request_seconds under the engine's name. Now a hit is
// counted under cache="hit" and kept out of the latency histogram.
func TestFetchURL_CacheHitsDoNotSkewLatency(t *testing.T) {
	var calls atomic.Int32
	reg := engine.New().Fallback(countingEngine{calls: &calls})
	cache := fetchcache.New(fetchcache.Config{
		Inner:      reg,
		Backend:    fetchcache.NewMemory(fetchcache.MemoryConfig{MaxBytes: 1 << 20}),
		DefaultTTL: time.Minute,
	})
	m := observability.NewMetrics()
	tool := FetchURL(cache, reddit.Options{}, m, 0)
	for range 3 {
		if _, err := tool.Handle(context.Background(), map[string]any{"url": "https://example.com/a"}); err != nil {
			t.Fatal(err)
		}
	}
	if hits, _ := count(t, m.RequestsTotal.WithLabelValues("count", "mcp", "ok", "ok", "hit")); hits != 2 {
		t.Fatalf(`requests_total{cache="hit"} = %v, want 2`, hits)
	}
	if misses, _ := count(t, m.RequestsTotal.WithLabelValues("count", "mcp", "ok", "ok", "miss")); misses != 1 {
		t.Fatalf(`requests_total{cache="miss"} = %v, want 1`, misses)
	}
	if _, n := count(t, m.RequestSecs.WithLabelValues("count", "ok", "ok").(prometheus.Histogram)); n != 1 {
		t.Fatalf("request_seconds observations = %d, want 1 (the miss only)", n)
	}
}

type failingEngine struct{}

func (failingEngine) Name() string        { return "count" }
func (failingEngine) Matches(string) bool { return true }
func (failingEngine) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	return domain.Document{}, errors.New("boom")
}

// Errors are never served from the cache: miss with a cache in front, bypass
// on no_cache, off without a cache.
func TestFetchURL_CacheLabelOnErrors(t *testing.T) {
	reg := engine.New().Fallback(failingEngine{})
	cache := fetchcache.New(fetchcache.Config{
		Inner: reg, Backend: fetchcache.NewMemory(fetchcache.MemoryConfig{MaxBytes: 1 << 20}), DefaultTTL: time.Minute,
	})
	m := observability.NewMetrics()
	_, _ = FetchURL(cache, reddit.Options{}, m, 0).Handle(context.Background(), map[string]any{"url": "https://example.com/a"})
	_, _ = FetchURL(cache, reddit.Options{}, m, 0).Handle(context.Background(), map[string]any{"url": "https://example.com/a", "no_cache": true})
	_, _ = FetchURL(reg, reddit.Options{}, m, 0).Handle(context.Background(), map[string]any{"url": "https://example.com/a"})
	for _, label := range []string{"miss", "bypass", "off"} {
		if v, _ := count(t, m.RequestsTotal.WithLabelValues("count", "mcp", "error", "error", label)); v != 1 {
			t.Errorf(`requests_total{status="error",cache=%q} = %v, want 1`, label, v)
		}
	}
}

type idSearcher struct{ seen string }

func (*idSearcher) Name() string { return "searxng" }
func (s *idSearcher) Search(ctx context.Context, _ string, _ domain.SearchOptions) ([]domain.SearchResult, error) {
	s.seen = observability.QueryIDFrom(ctx)
	return nil, nil
}

// web_search hands the searcher the query_id its audit lines use and returns
// it in _meta, which the MCP completion line logs.
func TestWebSearch_QueryIDJoinsAuditLines(t *testing.T) {
	s := &idSearcher{}
	res, err := WebSearch(s, 10, nil).Handle(context.Background(), map[string]any{"query": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if s.seen == "" || res.Meta["query_id"] != s.seen {
		t.Fatalf("query_id: searcher saw %q, _meta has %q", s.seen, res.Meta["query_id"])
	}
}
