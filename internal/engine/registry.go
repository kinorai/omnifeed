// Package engine defines the dispatch mechanism that picks the right
// per-URL handler. New engines (Hacker News, Stack Overflow, …) plug in by
// implementing domain.Engine and being Registered before the fallback.
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/observability"
)

// Registry holds an ordered list of engines and a fallback. Lookup is
// first-match-wins; the fallback handles anything no engine claimed.
type Registry struct {
	engines      []domain.Engine
	fallback     domain.Engine
	blockPrivate bool
	logger       *slog.Logger
	metrics      *observability.Metrics
}

// New returns an empty Registry. Use Register and Fallback to populate it.
func New() *Registry { return &Registry{logger: slog.Default()} }

// Logger sets the logger used to report engine→fallback handoffs.
func (r *Registry) Logger(l *slog.Logger) *Registry {
	if l != nil {
		r.logger = l
	}
	return r
}

// Metrics sets the collectors used to count engine→fallback handoffs
// (omnifeed_engine_fallbacks_total). Nil disables the counter.
func (r *Registry) Metrics(m *observability.Metrics) *Registry {
	r.metrics = m
	return r
}

// Register appends an engine to the dispatch chain. Order matters — earlier
// engines get first crack at each URL.
func (r *Registry) Register(e domain.Engine) *Registry {
	r.engines = append(r.engines, e)
	return r
}

// Fallback sets the engine used when no Registered engine claims a URL.
func (r *Registry) Fallback(e domain.Engine) *Registry {
	r.fallback = e
	return r
}

// BlockPrivateIPs configures the SSRF choke point. Crawl validates every URL
// before dispatch, so no transport (HTTP loader, MCP HTTP, MCP stdio) can
// forget the check. The http(s)-scheme and non-empty-host checks always run;
// the private/reserved-IP rejection is gated on block.
func (r *Registry) BlockPrivateIPs(block bool) *Registry {
	r.blockPrivate = block
	return r
}

// Resolve returns the engine that should handle rawURL.
func (r *Registry) Resolve(rawURL string) domain.Engine {
	for _, e := range r.engines {
		if e.Matches(rawURL) {
			return e
		}
	}
	return r.fallback
}

// Crawl dispatches rawURL to the resolved engine, after validating it at the
// SSRF choke point (see BlockPrivateIPs). Validating here — rather than in each
// transport — guarantees every inbound path is covered.
func (r *Registry) Crawl(ctx context.Context, rawURL string, opts domain.EngineOptions) (domain.Document, error) {
	if err := httpx.ValidateURL(rawURL, r.blockPrivate); err != nil {
		return domain.Document{}, fmt.Errorf("url rejected: %w", err)
	}
	for _, e := range r.engines {
		if !e.Matches(rawURL) {
			continue
		}
		doc, err := e.Crawl(ctx, rawURL, opts)
		// A dedicated engine failing (rate limit, API change, upstream hiccup)
		// must not hard-fail a URL the generic browser fallback can still
		// render — before dedicated engines existed, these URLs worked. Skipped
		// when the caller is already gone: the fallback would only burn a
		// browser render on a dead request.
		if err != nil && r.fallback != nil && ctx.Err() == nil {
			r.logger.Warn("engine failed, falling back to generic crawl",
				"engine", e.Name(), "url", rawURL, "err", err)
			if r.metrics != nil {
				r.metrics.ObserveFallback(e.Name(), observability.Reason(err))
			}
			doc, err = r.fallback.Crawl(ctx, rawURL, opts)
			r.observeChars(r.fallback, doc, err)
			return doc, err
		}
		r.observeChars(e, doc, err)
		return doc, err
	}
	if r.fallback == nil {
		return domain.Document{}, fmt.Errorf("no engine available for %s and no fallback configured", rawURL)
	}
	doc, err := r.fallback.Crawl(ctx, rawURL, opts)
	r.observeChars(r.fallback, doc, err)
	return doc, err
}

// observeChars records the extracted content length of a successful crawl
// under the engine that actually PRODUCED the document. This lives at the
// dispatch choke point — not in the transports — because only the registry
// knows which engine served a fallback crawl; labeling by URL-resolved engine
// would attribute the generic engine's output to the engine that failed.
func (r *Registry) observeChars(e domain.Engine, doc domain.Document, err error) {
	if r.metrics == nil || err != nil {
		return
	}
	r.metrics.ObserveResponseChars(e.Name(), utf8.RuneCountInString(doc.PageContent))
}
