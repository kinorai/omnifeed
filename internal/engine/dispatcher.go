package engine

import (
	"context"

	"github.com/kinorai/omnifeed/internal/domain"
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
