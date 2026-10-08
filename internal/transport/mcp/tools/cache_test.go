package tools

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"github.com/kinorai/omnifeed/internal/engine/reddit"
	"github.com/kinorai/omnifeed/internal/fetchcache"
)

// no_cache must reach the dispatcher as EngineOptions.NoCache, and must be in
// the schema or no caller can pass it.
func TestFetchURL_NoCacheArg(t *testing.T) {
	var got domain.EngineOptions
	tool := FetchURL(engine.New().Fallback(optsCapturingEngine{got: &got}), reddit.Options{}, nil, 0)

	if _, err := tool.Handle(context.Background(), map[string]any{"url": "https://example.com/"}); err != nil {
		t.Fatal(err)
	}
	if got.NoCache {
		t.Error("absent no_cache: NoCache = true, want false")
	}
	if _, err := tool.Handle(context.Background(), map[string]any{"url": "https://example.com/", "no_cache": true}); err != nil {
		t.Fatal(err)
	}
	if !got.NoCache {
		t.Error("no_cache=true: NoCache = false, want true")
	}
	props := tool.InputSchema["properties"].(map[string]any)
	if _, ok := props["no_cache"]; !ok {
		t.Error("no_cache missing from input schema")
	}
	if !strings.Contains(tool.Description, "no_cache") {
		t.Error("tool description does not document no_cache")
	}
}

type countingEngine struct{ calls *atomic.Int32 }

func (countingEngine) Name() string        { return "count" }
func (countingEngine) Matches(string) bool { return true }
func (e countingEngine) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	e.calls.Add(1)
	return domain.Document{
		PageContent: strings.Repeat("abcdefghij", 10),
		Metadata:    map[string]string{domain.ContentTypeKey: domain.ContentTypeMarkdown},
	}, nil
}

// fetch_url cuts max_chars/start_char windows AFTER the cache, so paging
// through one long page is one upstream fetch, and each window still comes
// back right and marked as a hit.
func TestFetchURL_CachedPagingIsOneFetch(t *testing.T) {
	var calls atomic.Int32
	reg := engine.New().Fallback(countingEngine{calls: &calls})
	cache := fetchcache.New(fetchcache.Config{
		Inner:      reg,
		Backend:    fetchcache.NewMemory(fetchcache.MemoryConfig{MaxBytes: 1 << 20}),
		DefaultTTL: time.Minute,
	})
	tool := FetchURL(cache, reddit.Options{}, nil, 0)

	first, err := tool.Handle(context.Background(), map[string]any{"url": "https://example.com/long", "max_chars": float64(30)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := tool.Handle(context.Background(), map[string]any{"url": "https://example.com/long", "max_chars": float64(30), "start_char": float64(30)})
	if err != nil {
		t.Fatal(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("upstream fetches = %d, want 1", n)
	}
	if first.Meta["cache"] != "miss" || second.Meta["cache"] != "hit" {
		t.Errorf("cache marks = %q, %q; want miss, hit", first.Meta["cache"], second.Meta["cache"])
	}
	if !strings.HasPrefix(first.Text, "abcdefghij") || !strings.HasPrefix(second.Text, "abcdefghij") || second.Meta["next_start_char"] != "60" {
		t.Errorf("windows wrong: first=%.40q second=%.40q next=%s", first.Text, second.Text, second.Meta["next_start_char"])
	}
}
