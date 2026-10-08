package fetchcache

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/observability"
)

// --- fakes ---

type namedEngine string

func (n namedEngine) Name() string      { return string(n) }
func (namedEngine) Matches(string) bool { return true }
func (namedEngine) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	return domain.Document{}, nil
}

// stubDispatcher stands in for the engine registry: it counts upstream
// crawls and answers with respond (or a fixed page when respond is nil).
type stubDispatcher struct {
	calls   atomic.Int32
	engine  string
	respond func(ctx context.Context, n int32) (domain.Document, error)
}

func (s *stubDispatcher) Resolve(string) domain.Engine { return namedEngine(s.engine) }

func (s *stubDispatcher) Crawl(ctx context.Context, rawURL string, _ domain.EngineOptions) (domain.Document, error) {
	n := s.calls.Add(1)
	if s.respond != nil {
		return s.respond(ctx, n)
	}
	return page(rawURL), nil
}

func page(src string) domain.Document {
	return domain.Document{
		PageContent: "content of " + src,
		Metadata:    map[string]string{"source": src, domain.ContentTypeKey: domain.ContentTypeMarkdown},
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Date(2026, 10, 8, 6, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// errBackend always fails, like a Redis that is down.
type errBackend struct{ gets, sets atomic.Int32 }

func (b *errBackend) Get(context.Context, string) (Entry, bool, error) {
	b.gets.Add(1)
	return Entry{}, false, errors.New("dial tcp: connection refused")
}

func (b *errBackend) Set(context.Context, string, Entry, time.Duration) error {
	b.sets.Add(1)
	return errors.New("dial tcp: connection refused")
}

func newCache(inner *stubDispatcher, b Backend, clock *fakeClock, m *observability.Metrics) *Cache {
	return New(Config{
		Inner:      inner,
		Backend:    b,
		DefaultTTL: 30 * time.Minute,
		EngineTTL:  map[string]time.Duration{"reddit": 10 * time.Minute},
		Metrics:    m,
		Now:        clock.Now,
	})
}

func memBackend(clock *fakeClock) *Memory {
	return NewMemory(MemoryConfig{MaxBytes: 1 << 20, Now: clock.Now})
}

func counter(m *observability.Metrics, result string) float64 {
	var out dto.Metric
	if err := m.CacheRequests.WithLabelValues(result).Write(&out); err != nil {
		panic(err)
	}
	return out.GetCounter().GetValue()
}

const testURL = "https://example.com/article"

// --- key ---

// Every option that reaches an engine changes what it renders, so it must
// change the key; no_cache and URL spellings a server treats alike must not.
func TestKey_IncludesEveryOutputOption(t *testing.T) {
	base := domain.EngineOptions{RedditFormat: "toon", RedditMaxRounds: 3}
	baseKey, ok := Key(testURL, base)
	if !ok {
		t.Fatal("Key failed on a valid URL")
	}
	yes := true
	variants := map[string]func(o *domain.EngineOptions){
		"format":          func(o *domain.EngineOptions) { o.RedditFormat = "json" },
		"format_explicit": func(o *domain.EngineOptions) { o.FormatExplicit = true },
		"expand":          func(o *domain.EngineOptions) { o.RedditMaxRounds = 40 },
		"limit":           func(o *domain.EngineOptions) { o.RedditFetchLimit = 50 },
		"depth":           func(o *domain.EngineOptions) { o.RedditDepth = 2 },
		"sort":            func(o *domain.EngineOptions) { o.RedditSort = "new" },
		"max_comments":    func(o *domain.EngineOptions) { o.RedditMaxComments = 10; o.HNMaxComments = 10 },
		"max_top_level":   func(o *domain.EngineOptions) { o.RedditMaxTopLevel = 2; o.HNMaxTopLevel = 2 },
		"max_per_subtree": func(o *domain.EngineOptions) { o.HNMaxPerSubtree = 12 },
		"keep_depth":      func(o *domain.EngineOptions) { o.RedditKeepDepth = true },
		"keep_created":    func(o *domain.EngineOptions) { o.RedditKeepCreated = true },
		"scan_full_page":  func(o *domain.EngineOptions) { o.ScanFullPage = &yes },
	}
	seen := map[string]string{baseKey: "base"}
	for name, mutate := range variants {
		o := base
		mutate(&o)
		k, _ := Key(testURL, o)
		if prev, dup := seen[k]; dup {
			t.Errorf("option %s produced the same key as %s", name, prev)
		}
		seen[k] = name
	}

	// scan_full_page is tri-state: explicit false differs from unset.
	no := false
	o := base
	o.ScanFullPage = &no
	if k, _ := Key(testURL, o); k == baseKey {
		t.Error("scan_full_page=false keyed like unset")
	}

	o = base
	o.NoCache = true
	if k, _ := Key(testURL, o); k != baseKey {
		t.Error("no_cache changed the key; it must not affect what is cached")
	}

	same := []string{
		"HTTPS://Example.COM/article",
		"https://example.com:443/article",
		"https://example.com/article#comments",
	}
	for _, u := range same {
		if k, _ := Key(u, base); k != baseKey {
			t.Errorf("%s keyed differently from %s", u, testURL)
		}
	}
	a, _ := Key("https://example.com/a?x=1&y=2", base)
	b, _ := Key("https://example.com/a?y=2&x=1", base)
	if a != b {
		t.Error("query parameter order changed the key")
	}
	different := []string{
		"https://example.com/Article",
		"https://example.com/article/",
		"http://example.com/article",
		"https://example.com:8443/article",
		"https://example.com/article?page=2",
	}
	for _, u := range different {
		if k, _ := Key(u, base); k == baseKey {
			t.Errorf("%s keyed like %s", u, testURL)
		}
	}
	if _, ok := Key("not a url", base); ok {
		t.Error("Key accepted a URL with no host")
	}
}

// --- hit / miss / bypass ---

func TestCrawl_MissThenHitThenBypass(t *testing.T) {
	clock := newClock()
	inner := &stubDispatcher{engine: "crawl4ai"}
	m := observability.NewMetrics()
	c := newCache(inner, memBackend(clock), clock, m)
	ctx := context.Background()

	miss, err := c.Crawl(ctx, testURL, domain.EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if miss.Metadata[MetaCache] != ResultMiss || miss.Metadata[MetaCachedAt] != "2026-10-08T06:00:00Z" {
		t.Errorf("first call meta = %v, want cache=miss cached_at=2026-10-08T06:00:00Z", miss.Metadata)
	}

	clock.Advance(time.Minute)
	hit, err := c.Crawl(ctx, testURL, domain.EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("upstream calls = %d after a hit, want 1", inner.calls.Load())
	}
	if hit.PageContent != miss.PageContent || hit.Metadata[MetaCache] != ResultHit ||
		hit.Metadata[MetaCachedAt] != "2026-10-08T06:00:00Z" || hit.Metadata["source"] != testURL {
		t.Errorf("hit = %q %v", hit.PageContent, hit.Metadata)
	}

	// A different option set is a different entry.
	if _, err := c.Crawl(ctx, testURL, domain.EngineOptions{RedditFormat: "json"}); err != nil {
		t.Fatal(err)
	}
	if inner.calls.Load() != 2 {
		t.Fatalf("upstream calls = %d after a new option set, want 2", inner.calls.Load())
	}

	clock.Advance(time.Minute)
	bypass, err := c.Crawl(ctx, testURL, domain.EngineOptions{NoCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if inner.calls.Load() != 3 || bypass.Metadata[MetaCache] != ResultBypass {
		t.Fatalf("bypass: calls=%d meta=%v, want a fresh fetch marked bypass", inner.calls.Load(), bypass.Metadata)
	}
	// The bypass refreshed the entry.
	after, _ := c.Crawl(ctx, testURL, domain.EngineOptions{})
	if after.Metadata[MetaCachedAt] != "2026-10-08T06:02:00Z" {
		t.Errorf("cached_at after bypass = %q, want the bypass fetch time", after.Metadata[MetaCachedAt])
	}

	if counter(m, ResultMiss) != 2 || counter(m, ResultHit) != 2 || counter(m, ResultBypass) != 1 || counter(m, ResultError) != 0 {
		t.Errorf("counters miss=%v hit=%v bypass=%v error=%v, want 2/2/1/0",
			counter(m, ResultMiss), counter(m, ResultHit), counter(m, ResultBypass), counter(m, ResultError))
	}
}

// A hit must hand out a copy: a transport adding keys to one response's
// metadata must not leak into the stored entry.
func TestCrawl_HitIsACopy(t *testing.T) {
	clock := newClock()
	c := newCache(&stubDispatcher{engine: "crawl4ai"}, memBackend(clock), clock, nil)
	first, _ := c.Crawl(context.Background(), testURL, domain.EngineOptions{})
	first.Metadata["truncated"] = "true"
	second, _ := c.Crawl(context.Background(), testURL, domain.EngineOptions{})
	second.Metadata["x"] = "y"
	third, _ := c.Crawl(context.Background(), testURL, domain.EngineOptions{})
	if _, leaked := third.Metadata["truncated"]; leaked {
		t.Error("caller mutation of a miss leaked into the cache")
	}
	if _, leaked := third.Metadata["x"]; leaked {
		t.Error("caller mutation of a hit leaked into the cache")
	}
}

// --- TTL ---

func TestCrawl_TTLByEngineExpires(t *testing.T) {
	cases := []struct {
		engine string
		ttl    time.Duration
	}{
		{"reddit", 10 * time.Minute},   // threads TTL
		{"crawl4ai", 30 * time.Minute}, // pages (default) TTL
	}
	for _, tc := range cases {
		t.Run(tc.engine, func(t *testing.T) {
			clock := newClock()
			inner := &stubDispatcher{engine: tc.engine}
			c := newCache(inner, memBackend(clock), clock, nil)
			ctx := context.Background()

			_, _ = c.Crawl(ctx, testURL, domain.EngineOptions{})
			clock.Advance(tc.ttl - time.Second)
			if doc, _ := c.Crawl(ctx, testURL, domain.EngineOptions{}); doc.Metadata[MetaCache] != ResultHit {
				t.Fatalf("just before the %s TTL: cache=%q, want hit", tc.ttl, doc.Metadata[MetaCache])
			}
			clock.Advance(time.Second)
			if doc, _ := c.Crawl(ctx, testURL, domain.EngineOptions{}); doc.Metadata[MetaCache] != ResultMiss {
				t.Fatalf("at the %s TTL: cache=%q, want miss", tc.ttl, doc.Metadata[MetaCache])
			}
			if inner.calls.Load() != 2 {
				t.Errorf("upstream calls = %d, want 2", inner.calls.Load())
			}
		})
	}
}

// A zero TTL disables caching for that engine class.
func TestCrawl_ZeroTTLDoesNotStore(t *testing.T) {
	clock := newClock()
	inner := &stubDispatcher{engine: "reddit"}
	c := New(Config{Inner: inner, Backend: memBackend(clock), DefaultTTL: time.Hour,
		EngineTTL: map[string]time.Duration{"reddit": 0}, Now: clock.Now})
	_, _ = c.Crawl(context.Background(), testURL, domain.EngineOptions{})
	doc, _ := c.Crawl(context.Background(), testURL, domain.EngineOptions{})
	if inner.calls.Load() != 2 || doc.Metadata[MetaCachedAt] != "" {
		t.Errorf("calls=%d meta=%v, want 2 uncached fetches", inner.calls.Load(), doc.Metadata)
	}
}

// --- what must never be cached ---

func TestCrawl_DoesNotCacheErrorsFallbacksOrPartials(t *testing.T) {
	fallback := page(testURL)
	fallback.Metadata[domain.FallbackFromKey] = "reddit"
	fallback.Metadata["fallback_reason"] = "timeout"
	partial := page(testURL)
	partial.Metadata[domain.PartialKey] = "true"
	partial.Metadata[domain.PartialReasonKey] = "http_429"

	cases := map[string]func(context.Context, int32) (domain.Document, error){
		"error": func(context.Context, int32) (domain.Document, error) {
			return domain.Document{}, &domain.FetchError{Kind: domain.KindHTTP429}
		},
		"fallback": func(context.Context, int32) (domain.Document, error) { return fallback, nil },
		"partial":  func(context.Context, int32) (domain.Document, error) { return partial, nil },
		"empty":    func(context.Context, int32) (domain.Document, error) { return domain.Document{}, nil },
	}
	for name, respond := range cases {
		t.Run(name, func(t *testing.T) {
			clock := newClock()
			inner := &stubDispatcher{engine: "reddit", respond: respond}
			mem := memBackend(clock)
			c := newCache(inner, mem, clock, nil)
			for range 2 {
				doc, err := c.Crawl(context.Background(), testURL, domain.EngineOptions{})
				if err == nil && (doc.Metadata[MetaCache] != ResultMiss || doc.Metadata[MetaCachedAt] != "") {
					t.Errorf("meta = %v, want cache=miss and no cached_at", doc.Metadata)
				}
			}
			if inner.calls.Load() != 2 {
				t.Errorf("upstream calls = %d, want 2 (nothing cached)", inner.calls.Load())
			}
			if mem.Bytes() != 0 {
				t.Errorf("backend holds %d bytes, want 0", mem.Bytes())
			}
		})
	}
}

// --- singleflight ---

func TestCrawl_SingleflightSharesOneFetch(t *testing.T) {
	clock := newClock()
	release := make(chan struct{})
	inner := &stubDispatcher{engine: "reddit", respond: func(context.Context, int32) (domain.Document, error) {
		<-release
		return page(testURL), nil
	}}
	m := observability.NewMetrics()
	// A backend that never stores, so followers can only be served by the
	// flight, not by a write that raced ahead of them.
	c := newCache(inner, NewMemory(MemoryConfig{MaxBytes: 0, Now: clock.Now}), clock, m)

	const n = 8
	var wg sync.WaitGroup
	docs := make([]domain.Document, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			docs[i], errs[i] = c.Crawl(context.Background(), testURL, domain.EngineOptions{})
		}()
	}
	// Wait until the leader is upstream and every follower has joined.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		joined := len(c.flight) == 1
		c.mu.Unlock()
		if joined && counter(m, ResultMiss) == 1 && inner.calls.Load() == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("leader never started")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // let followers block on the flight
	close(release)
	wg.Wait()

	if got := inner.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d for %d identical concurrent requests, want 1", got, n)
	}
	hits := 0
	for i := range n {
		if errs[i] != nil || docs[i].PageContent != "content of "+testURL {
			t.Errorf("request %d: %v %q", i, errs[i], docs[i].PageContent)
		}
		if docs[i].Metadata[MetaCache] == ResultHit {
			hits++
		}
	}
	if hits != n-1 {
		t.Errorf("%d followers marked hit, want %d", hits, n-1)
	}
}

// Followers share the leader's error (the upstream that just refused it would
// refuse them too) — but not the leader's own hang-up.
func TestCrawl_SingleflightLeaderCancelDoesNotFailFollower(t *testing.T) {
	clock := newClock()
	started := make(chan struct{})
	inner := &stubDispatcher{engine: "reddit", respond: func(ctx context.Context, n int32) (domain.Document, error) {
		if n == 1 {
			close(started)
			<-ctx.Done()
			return domain.Document{}, ctx.Err()
		}
		return page(testURL), nil
	}}
	c := newCache(inner, memBackend(clock), clock, nil)

	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := c.Crawl(leaderCtx, testURL, domain.EngineOptions{})
		leaderErr <- err
	}()
	<-started

	followerDone := make(chan struct{})
	var doc domain.Document
	var err error
	go func() {
		doc, err = c.Crawl(context.Background(), testURL, domain.EngineOptions{})
		close(followerDone)
	}()
	time.Sleep(20 * time.Millisecond) // follower joins the flight
	cancel()
	<-followerDone
	if lerr := <-leaderErr; !errors.Is(lerr, context.Canceled) {
		t.Errorf("leader err = %v, want canceled", lerr)
	}
	if err != nil || doc.PageContent == "" {
		t.Fatalf("follower got %v, want its own successful fetch", err)
	}
	if inner.calls.Load() != 2 {
		t.Errorf("upstream calls = %d, want 2 (leader + follower's own)", inner.calls.Load())
	}
}

// --- backend failure ---

// A cache that cannot answer is a miss, never a failed request.
func TestCrawl_BackendDownServesMiss(t *testing.T) {
	clock := newClock()
	inner := &stubDispatcher{engine: "reddit"}
	b := &errBackend{}
	m := observability.NewMetrics()
	c := newCache(inner, b, clock, m)
	for range 2 {
		doc, err := c.Crawl(context.Background(), testURL, domain.EngineOptions{})
		if err != nil {
			t.Fatalf("request failed with the cache down: %v", err)
		}
		if doc.Metadata[MetaCache] != ResultMiss || doc.Metadata[MetaCachedAt] != "" {
			t.Errorf("meta = %v, want an uncached miss", doc.Metadata)
		}
	}
	if inner.calls.Load() != 2 || b.sets.Load() != 2 {
		t.Errorf("calls=%d sets=%d, want 2/2", inner.calls.Load(), b.sets.Load())
	}
	if counter(m, ResultError) != 2 || counter(m, ResultMiss) != 0 {
		t.Errorf("error=%v miss=%v, want 2/0", counter(m, ResultError), counter(m, ResultMiss))
	}
}

// --- Redis backend ---

func redisBackend(t *testing.T, clock *fakeClock, maxItem int) (*Redis, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rb, client := redisBackendOn(t, mr, clock)
	return NewRedis(RedisConfig{Client: client, Prefix: rb.prefix, MaxItemBytes: maxItem, Now: clock.Now}), mr
}

// redisBackendOn builds a Redis backend (no item cap) over an existing
// miniredis, and returns its client too.
func redisBackendOn(t *testing.T, mr *miniredis.Miniredis, clock *fakeClock) (*Redis, redis.UniversalClient) {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1,
		DialTimeout: 100 * time.Millisecond, ReadTimeout: 100 * time.Millisecond, WriteTimeout: 100 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })
	return NewRedis(RedisConfig{Client: client, Prefix: "omnifeed:cache", Now: clock.Now}), client
}

func TestRedis_SharedCompressedWithTTL(t *testing.T) {
	clock := newClock()
	rb, mr := redisBackend(t, clock, 0)
	inner := &stubDispatcher{engine: "reddit", respond: func(context.Context, int32) (domain.Document, error) {
		d := page(testURL)
		d.PageContent = strings.Repeat("comment ", 5000)
		return d, nil
	}}
	c := newCache(inner, rb, clock, nil)
	if _, err := c.Crawl(context.Background(), testURL, domain.EngineOptions{}); err != nil {
		t.Fatal(err)
	}
	key, _ := Key(testURL, domain.EngineOptions{})
	stored, err := mr.Get("omnifeed:cache:" + key)
	if err != nil {
		t.Fatalf("entry not in redis: %v", err)
	}
	if len(stored) >= 40000 || !strings.HasPrefix(stored, "\x1f\x8b") {
		t.Errorf("stored %d bytes, want a gzip value well under the 40000-byte body", len(stored))
	}
	if ttl := mr.TTL("omnifeed:cache:" + key); ttl != 10*time.Minute {
		t.Errorf("redis TTL = %s, want the threads TTL 10m", ttl)
	}

	// A second replica (another Cache on the same Redis) gets a hit.
	other := newCache(&stubDispatcher{engine: "reddit"}, rb, clock, nil)
	doc, err := other.Crawl(context.Background(), testURL, domain.EngineOptions{})
	if err != nil || doc.Metadata[MetaCache] != ResultHit || len(doc.PageContent) != 40000 {
		t.Fatalf("replica read: err=%v meta=%v len=%d", err, doc.Metadata, len(doc.PageContent))
	}

	mr.FastForward(10 * time.Minute)
	if doc, _ := other.Crawl(context.Background(), testURL, domain.EngineOptions{}); doc.Metadata[MetaCache] != ResultMiss {
		t.Errorf("after the redis TTL: cache=%q, want miss", doc.Metadata[MetaCache])
	}
}

func TestRedis_ItemOverCapIsServedNotStored(t *testing.T) {
	clock := newClock()
	rb, mr := redisBackend(t, clock, 64)
	c := newCache(&stubDispatcher{engine: "reddit"}, rb, clock, nil)
	doc, err := c.Crawl(context.Background(), testURL, domain.EngineOptions{})
	if err != nil || doc.PageContent == "" {
		t.Fatalf("oversized item failed the request: %v", err)
	}
	if doc.Metadata[MetaCachedAt] != "" || len(mr.Keys()) != 0 {
		t.Errorf("oversized item stored: meta=%v keys=%v", doc.Metadata, mr.Keys())
	}
}

// Redis going away mid-life: the request still succeeds as a miss, and the
// backend stops paying a timeout per request for the cooldown.
func TestRedis_DownDegradesToMiss(t *testing.T) {
	clock := newClock()
	rb, mr := redisBackend(t, clock, 0)
	inner := &stubDispatcher{engine: "reddit"}
	m := observability.NewMetrics()
	c := newCache(inner, rb, clock, m)
	mr.Close()

	for i := range 3 {
		doc, err := c.Crawl(context.Background(), testURL, domain.EngineOptions{})
		if err != nil {
			t.Fatalf("call %d failed with redis down: %v", i, err)
		}
		if doc.Metadata[MetaCache] != ResultMiss {
			t.Errorf("call %d meta = %v, want miss", i, doc.Metadata)
		}
	}
	if !rb.coolingDown() {
		t.Error("backend not cooling down after a redis failure")
	}
	if _, _, err := rb.Get(context.Background(), "k"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Get during cooldown = %v, want ErrUnavailable (fail fast)", err)
	}
	if counter(m, ResultError) != 3 || inner.calls.Load() != 3 {
		t.Errorf("error=%v calls=%d, want 3/3", counter(m, ResultError), inner.calls.Load())
	}
	clock.Advance(defaultCooldown)
	if rb.coolingDown() {
		t.Error("cooldown did not end")
	}
}

// A caller hanging up mid-GET is not a Redis outage: it must not switch the
// cache off for everyone else for a whole cooldown.
func TestRedis_CanceledCallerDoesNotTripCooldown(t *testing.T) {
	clock := newClock()
	rb, _ := redisBackend(t, clock, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := rb.Get(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get with dead ctx = %v, want context.Canceled", err)
	}
	if rb.coolingDown() {
		t.Error("a canceled caller tripped the outage cooldown")
	}
}

func TestRedis_CorruptValueIsAMiss(t *testing.T) {
	clock := newClock()
	rb, mr := redisBackend(t, clock, 0)
	key, _ := Key(testURL, domain.EngineOptions{})
	_ = mr.Set("omnifeed:cache:"+key, "not gzip")
	inner := &stubDispatcher{engine: "reddit"}
	doc, err := newCache(inner, rb, clock, nil).Crawl(context.Background(), testURL, domain.EngineOptions{})
	if err != nil || inner.calls.Load() != 1 {
		t.Fatalf("corrupt value: err=%v calls=%d", err, inner.calls.Load())
	}
	if doc.Metadata[MetaCachedAt] == "" {
		t.Error("corrupt value was not overwritten by the fresh fetch")
	}
	if rb.coolingDown() {
		t.Error("a corrupt value tripped the outage cooldown")
	}
}

// --- in-process LRU ---

func TestMemory_EvictsLeastRecentlyUsed(t *testing.T) {
	clock := newClock()
	var reported int
	e := Entry{Doc: domain.Document{PageContent: strings.Repeat("x", 1000)}}
	one := entrySize("a", e)
	m := NewMemory(MemoryConfig{MaxBytes: 2*one + one/2, Now: clock.Now, OnSize: func(n int) { reported = n }})
	ctx := context.Background()
	_ = m.Set(ctx, "a", e, time.Hour)
	_ = m.Set(ctx, "b", e, time.Hour)
	_, _, _ = m.Get(ctx, "a") // a is now most recent
	_ = m.Set(ctx, "c", e, time.Hour)

	if _, ok, _ := m.Get(ctx, "b"); ok {
		t.Error("b survived; it was least recently used")
	}
	for _, k := range []string{"a", "c"} {
		if _, ok, _ := m.Get(ctx, k); !ok {
			t.Errorf("%s evicted", k)
		}
	}
	if m.Bytes() != 2*one || reported != 2*one {
		t.Errorf("bytes=%d reported=%d, want %d", m.Bytes(), reported, 2*one)
	}
	if err := m.Set(ctx, "huge", Entry{Doc: domain.Document{PageContent: strings.Repeat("x", 5000)}}, time.Hour); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized Set = %v, want ErrTooLarge", err)
	}
}

// A leader whose upstream crawl panics must not hand its followers a zero
// Document as a success: they fetch for themselves.
func TestCrawl_SingleflightLeaderPanicDoesNotServeEmptyDoc(t *testing.T) {
	clock := newClock()
	release := make(chan struct{})
	inner := &stubDispatcher{engine: "reddit", respond: func(_ context.Context, n int32) (domain.Document, error) {
		if n == 1 {
			<-release
			panic("engine bug")
		}
		return page(testURL), nil
	}}
	c := newCache(inner, NewMemory(MemoryConfig{MaxBytes: 0, Now: clock.Now}), clock, nil)

	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		defer func() { _ = recover() }()
		_, _ = c.Crawl(context.Background(), testURL, domain.EngineOptions{})
	}()
	for inner.calls.Load() != 1 {
		time.Sleep(time.Millisecond)
	}
	followerDone := make(chan struct{})
	var doc domain.Document
	var err error
	go func() {
		defer close(followerDone)
		doc, err = c.Crawl(context.Background(), testURL, domain.EngineOptions{})
	}()
	time.Sleep(20 * time.Millisecond) // let the follower join the flight
	close(release)
	<-leaderDone
	<-followerDone
	if err != nil || doc.PageContent != "content of "+testURL {
		t.Fatalf("follower after a leader panic: err=%v content=%q", err, doc.PageContent)
	}
}
