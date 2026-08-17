// Package router implements domain.Searcher as a dispatcher: a site-scoped
// query goes to that site's native search API when one is wired, and everything
// else — plus every vertical that declines, finds nothing, or fails — goes to
// the fallback searcher (SearXNG).
//
// A native vertical answers with the site's own ranking signals (points,
// comments, score), which scraped web engines never expose. The fallback is
// what keeps that an optimisation rather than a risk: a vertical can never make
// a search worse than it was, only slower by one hop.
package router

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/observability"
)

// Route outcomes, recorded as the `outcome` label of
// omnifeed_search_routes_total. Only "served" skips the fallback query.
const (
	outcomeServed   = "served"   // the vertical returned results, and they were returned to the caller
	outcomeEmpty    = "empty"    // the vertical succeeded with zero results
	outcomeDeclined = "declined" // the vertical returned domain.ErrSearchUnsupported
	outcomeError    = "error"    // the vertical failed
)

// Config configures the Router.
type Config struct {
	// Verticals maps a bare hostname ("news.ycombinator.com") to the searcher
	// that queries that site's own search API.
	Verticals map[string]domain.Searcher
	// Fallback answers every query no vertical serves. Required: without it a
	// declining vertical would turn into a failed search.
	Fallback domain.Searcher
	Logger   *slog.Logger
	// Metrics, when non-nil, receives per-dispatch route outcomes
	// (omnifeed_search_routes_total).
	Metrics *observability.Metrics
}

// Router dispatches site-scoped searches to native vertical searchers and
// falls back to a general web searcher for everything else.
type Router struct {
	verticals map[string]domain.Searcher
	fallback  domain.Searcher
	logger    *slog.Logger
	metrics   *observability.Metrics
}

// New returns a Router wired with the given config.
func New(cfg Config) *Router {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Router{
		verticals: cfg.Verticals,
		fallback:  cfg.Fallback,
		logger:    cfg.Logger,
		metrics:   cfg.Metrics,
	}
}

// Name returns the searcher identifier ("router").
func (*Router) Name() string { return "router" }

// Search routes the query by opts.Site and returns the chosen searcher's results.
func (r *Router) Search(ctx context.Context, query string, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	host := normalizeSite(opts.Site)
	vertical, ok := r.verticals[host]
	if !ok {
		return r.fallback.Search(ctx, query, opts)
	}

	// The metric label is the searcher's own name, not the host: it names the
	// implementation whose behaviour the outcome describes.
	name := vertical.Name()
	results, err := vertical.Search(ctx, query, opts)
	switch {
	case err == nil && len(results) > 0:
		r.observe(name, outcomeServed)
		return results, nil
	case err == nil:
		r.observe(name, outcomeEmpty)
	case errors.Is(err, domain.ErrSearchUnsupported):
		// Declining is a design decision of the vertical, not a fault: it is how
		// a searcher says the fallback will do better on this query shape.
		r.observe(name, outcomeDeclined)
	default:
		r.logger.Warn("vertical searcher failed, falling back",
			"vertical", name, "site", host, "err", err)
		r.observe(name, outcomeError)
	}
	return r.fallback.Search(ctx, query, opts)
}

func (r *Router) observe(vertical, outcome string) {
	if r.metrics != nil {
		r.metrics.ObserveSearchRoute(vertical, outcome)
	}
}

// normalizeSite reduces a Site filter to the key the vertical map is written
// with. Callers pass "reddit.com" and "www.reddit.com" interchangeably, and
// both name the same site search.
func normalizeSite(site string) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(site)), "www.")
}
