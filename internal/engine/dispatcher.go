package engine

import (
	"context"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/observability"
)

// Dispatcher is what the transports need from the engine layer: crawl a URL
// through the right engine, and name the engine that would handle it (for
// metrics labels). *Registry implements it; the fetch_url response cache
// (internal/fetchcache) decorates it, so every transport shares one cache.
type Dispatcher interface {
	Crawl(ctx context.Context, rawURL string, opts domain.EngineOptions) (domain.Document, error)
	Resolve(rawURL string) domain.Engine
}

var _ Dispatcher = (*Registry)(nil)

// CacheReporter is implemented by a Dispatcher that serves through a response
// cache (internal/fetchcache). Transports read it only to label metrics.
type CacheReporter interface {
	CacheEnabled() bool
}

// CacheLabel is the omnifeed_requests_total `cache` label for one crawl
// through d: the cache's own verdict on a document (hit|miss|bypass), and for
// an error — which is never served from the cache — bypass when the caller
// asked for no_cache, else miss. off when d has no cache in front of it.
func CacheLabel(d Dispatcher, doc domain.Document, err error, noCache bool) string {
	cr, ok := d.(CacheReporter)
	if !ok || !cr.CacheEnabled() {
		return observability.CacheOff
	}
	if err == nil {
		switch v := doc.Metadata[domain.CacheKey]; v {
		case observability.CacheHit, observability.CacheMiss, observability.CacheBypass:
			return v
		}
	}
	if noCache {
		return observability.CacheBypass
	}
	return observability.CacheMiss
}
