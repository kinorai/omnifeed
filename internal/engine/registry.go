// Package engine defines the dispatch mechanism that picks the right
// per-URL handler. New engines (Hacker News, Stack Overflow, …) plug in by
// implementing domain.Engine and being Registered before the fallback.
package engine

import (
	"context"
	"fmt"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// Registry holds an ordered list of engines and a fallback. Lookup is
// first-match-wins; the fallback handles anything no engine claimed.
type Registry struct {
	engines      []domain.Engine
	fallback     domain.Engine
	blockPrivate bool
}

// New returns an empty Registry. Use Register and Fallback to populate it.
func New() *Registry { return &Registry{} }

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
	e := r.Resolve(rawURL)
	if e == nil {
		return domain.Document{}, fmt.Errorf("no engine available for %s and no fallback configured", rawURL)
	}
	return e.Crawl(ctx, rawURL, opts)
}
