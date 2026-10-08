// Package fetchcache is the fetch_url response cache: a decorator around the
// engine dispatcher (engine.Dispatcher), so the MCP tool and the Open WebUI
// loader share one cache without either knowing it exists.
//
// Why it exists: the same Reddit/HN threads are re-fetched every few minutes
// (a retry job, agents re-reading a URL they already have), and every repeat
// spends Reddit's tight per-IP budget — on 2026-10-08 that budget ran out and
// the egress IP was blocked. A short TTL turns those repeats into free hits.
//
// What it caches: successful, complete documents only. Errors, generic
// fallback renders (domain.FallbackFromKey) and partial crawls
// (domain.PartialKey) are never stored, so the next caller gets a fresh chance
// at the real thing. Concurrent identical requests share one upstream fetch
// (singleflight). Backend failures degrade to a miss, never to a failed
// request.
package fetchcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"github.com/kinorai/omnifeed/internal/observability"
)

// Metadata keys the cache adds to every document it returns.
const (
	// MetaCache is "hit" (served from the cache, or shared with an identical
	// in-flight request), "miss" (fetched upstream) or "bypass" (the caller
	// asked for no_cache).
	MetaCache = "cache"
	// MetaCachedAt is when the content was fetched upstream and stored
	// (RFC 3339, UTC). Absent when the response is not in the cache — an
	// error, a fallback render, a partial crawl, or a failed store.
	MetaCachedAt = "cached_at"
)

// Cache results, also the values of the omnifeed_cache_requests_total
// `result` label. ResultError is a backend failure that was served as a miss.
const (
	ResultHit    = "hit"
	ResultMiss   = "miss"
	ResultBypass = "bypass"
	ResultError  = "error"
)

// Entry is one cached document and the time it was fetched upstream.
type Entry struct {
	Doc      domain.Document `json:"doc"`
	CachedAt time.Time       `json:"cached_at"`
}

// Backend stores entries. Get reports a miss as (Entry{}, false, nil); an
// error means the backend could not answer, which the cache serves as a miss.
type Backend interface {
	Get(ctx context.Context, key string) (Entry, bool, error)
	Set(ctx context.Context, key string, e Entry, ttl time.Duration) error
}

// Config configures a Cache.
type Config struct {
	Inner   engine.Dispatcher
	Backend Backend
	// DefaultTTL applies to documents from engines EngineTTL does not list
	// (the generic page fallback). EngineTTL overrides it per engine name.
	// A TTL <= 0 means "do not cache".
	DefaultTTL time.Duration
	EngineTTL  map[string]time.Duration
	Metrics    *observability.Metrics
	Logger     *slog.Logger
	Now        func() time.Time // defaults to time.Now; tests inject a fake clock
}

// Cache decorates an engine.Dispatcher with a response cache.
type Cache struct {
	inner      engine.Dispatcher
	backend    Backend
	defaultTTL time.Duration
	engineTTL  map[string]time.Duration
	metrics    *observability.Metrics
	logger     *slog.Logger
	now        func() time.Time

	mu     sync.Mutex
	flight map[string]*call
}

var _ engine.Dispatcher = (*Cache)(nil)

// call is one in-flight upstream fetch that identical requests wait on.
type call struct {
	done chan struct{}
	doc  domain.Document // as returned to the leader, already marked
	err  error
	// leaderGone is true when the leader's own context ended before the fetch
	// did: its error is about that caller hanging up, not about the URL.
	leaderGone bool
}

// New builds a Cache.
func New(cfg Config) *Cache {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Cache{
		inner:      cfg.Inner,
		backend:    cfg.Backend,
		defaultTTL: cfg.DefaultTTL,
		engineTTL:  cfg.EngineTTL,
		metrics:    cfg.Metrics,
		logger:     cfg.Logger,
		now:        cfg.Now,
		flight:     make(map[string]*call),
	}
}

// Resolve delegates to the wrapped dispatcher.
func (c *Cache) Resolve(rawURL string) domain.Engine { return c.inner.Resolve(rawURL) }

// Crawl serves rawURL from the cache when it can, and otherwise fetches it
// through the wrapped dispatcher, storing a cacheable result.
func (c *Cache) Crawl(ctx context.Context, rawURL string, opts domain.EngineOptions) (domain.Document, error) {
	key, ok := Key(rawURL, opts)
	if !ok {
		// Unparseable: the registry's URL validation owns the error.
		return c.inner.Crawl(ctx, rawURL, opts)
	}

	if opts.NoCache {
		c.observe(ResultBypass)
		doc, err := c.inner.Crawl(ctx, rawURL, opts)
		if err != nil {
			return doc, err
		}
		// A fresh fetch is still the freshest copy anyone has: refresh the
		// entry so the next ordinary caller gets it.
		return c.store(ctx, key, rawURL, doc, ResultBypass), nil
	}

	e, hit, err := c.backend.Get(ctx, key)
	switch {
	case err != nil:
		c.observe(ResultError)
		if !errors.Is(err, ErrUnavailable) {
			c.logger.Warn("fetch cache read failed; serving as a miss", "err", err)
		}
	case hit:
		c.observe(ResultHit)
		return mark(e.Doc, ResultHit, e.CachedAt), nil
	}
	return c.fetchShared(ctx, key, rawURL, opts, err == nil)
}

// fetchShared runs one upstream fetch per key at a time: the first caller
// (the leader) fetches, identical callers arriving meanwhile wait for its
// result instead of spending the upstream's budget again.
func (c *Cache) fetchShared(ctx context.Context, key, rawURL string, opts domain.EngineOptions, countMiss bool) (domain.Document, error) {
	c.mu.Lock()
	if cl, ok := c.flight[key]; ok {
		c.mu.Unlock()
		return c.follow(ctx, cl, rawURL, opts, key)
	}
	cl := &call{done: make(chan struct{})}
	c.flight[key] = cl
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.flight, key)
		c.mu.Unlock()
		close(cl.done)
	}()

	if countMiss {
		c.observe(ResultMiss)
	}
	doc, err := c.inner.Crawl(ctx, rawURL, opts)
	if err == nil {
		doc = c.store(ctx, key, rawURL, doc, ResultMiss)
	}
	cl.doc, cl.err, cl.leaderGone = doc, err, ctx.Err() != nil
	return doc, err
}

// follow waits for an identical in-flight fetch and shares its result, error
// included: an upstream that just refused the leader would refuse this caller
// too. The exception is a leader whose own context ended (its client hung up
// or ran out of time): then a still-live follower fetches for itself rather
// than inherit someone else's hang-up.
func (c *Cache) follow(ctx context.Context, cl *call, rawURL string, opts domain.EngineOptions, key string) (domain.Document, error) {
	select {
	case <-cl.done:
	case <-ctx.Done():
		return domain.Document{}, ctx.Err()
	}
	if cl.err != nil {
		if cl.leaderGone && ctx.Err() == nil {
			return c.fetchShared(ctx, key, rawURL, opts, true)
		}
		return domain.Document{}, cl.err
	}
	c.observe(ResultHit)
	doc := copyDoc(cl.doc)
	doc.Metadata[MetaCache] = ResultHit
	return doc, nil
}

// store writes a cacheable document to the backend and returns it marked with
// result (and cached_at when the write succeeded).
func (c *Cache) store(ctx context.Context, key, rawURL string, doc domain.Document, result string) domain.Document {
	if !Cacheable(doc) {
		return mark(doc, result, time.Time{})
	}
	ttl := c.ttlFor(rawURL)
	if ttl <= 0 {
		return mark(doc, result, time.Time{})
	}
	now := c.now().UTC()
	// WithoutCancel: a caller that hangs up after the upstream answered must
	// not throw the answer away. The backend bounds its own operation time.
	err := c.backend.Set(context.WithoutCancel(ctx), key, Entry{Doc: doc, CachedAt: now}, ttl)
	if err != nil {
		if !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrTooLarge) {
			c.logger.Warn("fetch cache write failed", "err", err)
		}
		return mark(doc, result, time.Time{})
	}
	return mark(doc, result, now)
}

// ttlFor picks the TTL by the engine that handles rawURL. Fallback renders are
// never stored, so the URL-resolved engine is the one that produced the doc.
func (c *Cache) ttlFor(rawURL string) time.Duration {
	if e := c.inner.Resolve(rawURL); e != nil {
		if ttl, ok := c.engineTTL[e.Name()]; ok {
			return ttl
		}
	}
	return c.defaultTTL
}

func (c *Cache) observe(result string) {
	if c.metrics != nil {
		c.metrics.ObserveCache(result)
	}
}

// Cacheable reports whether doc may be stored: a complete, non-empty document
// from the engine that claimed the URL. Fallback renders and partial crawls
// are stand-ins for the real thing; caching them would serve a degraded answer
// for a whole TTL after the upstream recovered.
func Cacheable(doc domain.Document) bool {
	if doc.PageContent == "" {
		return false
	}
	m := doc.Metadata
	return m[domain.PartialKey] != "true" && m[domain.FallbackFromKey] == "" && m["error"] != "true"
}

// Key derives the cache key from the normalized URL and every option that
// reaches the engine. Options are serialized whole (so a field added to
// domain.EngineOptions later is keyed automatically), minus NoCache, which
// changes how the request is served but never what an engine renders.
// ok is false when rawURL does not parse.
//
// max_chars / start_char are deliberately absent: fetch_url applies them to
// the document AFTER the dispatcher, so the cache holds the whole document and
// each character window is cut from it — paging through a long page is one
// upstream fetch, not one per chunk.
func Key(rawURL string, opts domain.EngineOptions) (string, bool) {
	norm, ok := normalizeURL(rawURL)
	if !ok {
		return "", false
	}
	opts.NoCache = false
	o, err := json.Marshal(opts)
	if err != nil {
		return "", false
	}
	h := sha256.New()
	h.Write([]byte(norm))
	h.Write([]byte{'\n'})
	h.Write(o)
	return "v1:" + hex.EncodeToString(h.Sum(nil)), true
}

// normalizeURL folds spellings of one URL that every server treats alike:
// scheme and host case, a default port, the fragment (never sent), and query
// parameter order. The path is kept verbatim — its case and trailing slash can
// matter to the server.
func normalizeURL(rawURL string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return "", false
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	defaultPort := (u.Scheme == "http" && port == "80") || (u.Scheme == "https" && port == "443")
	if port != "" && !defaultPort {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]" // bare IPv6 literal
	}
	u.Host = host
	u.Fragment, u.RawFragment = "", ""
	// Sort parameters only when the query parses cleanly; a query url.ParseQuery
	// rejects (e.g. ';' separators) would lose pairs and merge distinct URLs.
	if q, err := url.ParseQuery(u.RawQuery); err == nil {
		u.RawQuery = q.Encode()
	}
	return u.String(), true
}

// mark returns a copy of doc with the cache metadata set. The input's map is
// never mutated: it may be the one held by the in-process backend.
func mark(doc domain.Document, result string, cachedAt time.Time) domain.Document {
	out := copyDoc(doc)
	out.Metadata[MetaCache] = result
	if cachedAt.IsZero() {
		delete(out.Metadata, MetaCachedAt)
	} else {
		out.Metadata[MetaCachedAt] = cachedAt.UTC().Format(time.RFC3339)
	}
	return out
}

func copyDoc(doc domain.Document) domain.Document {
	meta := make(map[string]string, len(doc.Metadata)+2)
	for k, v := range doc.Metadata {
		meta[k] = v
	}
	doc.Metadata = meta
	return doc
}
