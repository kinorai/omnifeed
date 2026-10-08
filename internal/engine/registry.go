// Package engine defines the dispatch mechanism that picks the right
// per-URL handler. New engines (Hacker News, Stack Overflow, …) plug in by
// implementing domain.Engine and being Registered before the fallback.
package engine

import (
	"context"
	"errors"
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
		return domain.Document{}, fmt.Errorf("url rejected: %w", &domain.InvalidRequestError{Err: err})
	}
	for _, e := range r.engines {
		if !e.Matches(rawURL) {
			continue
		}
		doc, err := e.Crawl(ctx, rawURL, opts)
		err = classifyPacing(err)
		// A dedicated engine failing (rate limit, API change, upstream hiccup)
		// must not hard-fail a URL the generic browser fallback can still
		// render — before dedicated engines existed, these URLs worked. The
		// exceptions are in fallbackRefused. Skipped when the caller is already
		// gone: the fallback would only burn a browser render on a dead request.
		if err != nil && r.fallback != nil && ctx.Err() == nil {
			reason := observability.Reason(err)
			if why := fallbackRefused(e, opts, err, reason); why != "" {
				r.logger.Warn("engine failed, not falling back to generic crawl",
					"engine", e.Name(), "url", rawURL, "reason", reason, "why", why, "err", err)
				return doc, err
			}
			r.logger.Warn("engine failed, falling back to generic crawl",
				"engine", e.Name(), "url", rawURL, "err", err)
			if r.metrics != nil {
				r.metrics.ObserveFallback(e.Name(), reason)
			}
			doc, err = r.fallback.Crawl(ctx, rawURL, opts)
			err = classifyPacing(err)
			r.observeChars(r.fallback, doc, err)
			if err == nil {
				doc = markFallback(doc, e.Name(), reason)
			}
			return doc, err
		}
		r.observeChars(e, doc, err)
		return doc, err
	}
	if r.fallback == nil {
		return domain.Document{}, fmt.Errorf("no engine available for %s and no fallback configured", rawURL)
	}
	doc, err := r.fallback.Crawl(ctx, rawURL, opts)
	err = classifyPacing(err)
	r.observeChars(r.fallback, doc, err)
	return doc, err
}

// fallbackRefused reports why a failed dedicated engine must NOT hand over to
// the generic fallback, or "" when it may.
//
// Two callers read the reply. An engine client (format=json|toon) parses it:
// the fallback's markdown under the same success shape is a parse error at
// best, so an explicit structured format always gets the engine's own error.
// An AI agent reads text, so a page render still beats nothing — except after
// a block or rate verdict (429, 403, CAPTCHA, bot wall, our own spent quota)
// from an engine that fetches the page's own host (domain.SameHostEngine): the
// browser would hit the very host that just refused us and prolong the block.
// Observed on Reddit 2026-10-08, where the fallback returned
// "[ Skip to main content ](…)" to a JSON caller and kept the IP blocked. A
// separate-host engine's block is about its API host, not the page host, so
// the render still follows it.
//
// An engine that already ran the generic render itself (on a URL it rewrote),
// or knows the content is gone, marks its error final (domain.NoFallback): a
// second render of the original URL would only repeat the work.
func fallbackRefused(e domain.Engine, opts domain.EngineOptions, err error, reason string) string {
	if domain.IsNoFallback(err) {
		return "engine marked its error final"
	}
	if opts.FormatExplicit {
		return "explicit structured format requested"
	}
	if domain.IsBlockKind(domain.FailureKind(reason)) && sameHostAsPage(e) {
		return "block or rate verdict from the page's own host"
	}
	return ""
}

// sameHostAsPage reads the optional domain.SameHostEngine capability; an
// engine without it is separate-host.
func sameHostAsPage(e domain.Engine) bool {
	sh, ok := e.(domain.SameHostEngine)
	return ok && sh.SameHostAsPage()
}

// FallbackNotice is the first line of a document the generic fallback
// rendered for a URL a dedicated engine claimed, so a reader of the text alone
// (an AI agent over MCP) knows it is not looking at the engine's output.
const FallbackNotice = "> Note: the dedicated %s engine failed (%s); this is the generic page render instead.\n\n"

// markFallback labels a fallback-rendered document: _meta fallback_from /
// fallback_reason for clients that read metadata, and FallbackNotice on top of
// the body for those that only read text. fallback_from also keeps the
// response cache from storing a stand-in render under the dedicated engine's
// URL (fetchcache refuses any document carrying domain.FallbackFromKey). The
// metadata map is copied, never mutated in place — the fallback engine owns it.
func markFallback(doc domain.Document, from, reason string) domain.Document {
	meta := make(map[string]string, len(doc.Metadata)+2)
	for k, v := range doc.Metadata {
		meta[k] = v
	}
	meta[domain.FallbackFromKey] = from
	meta["fallback_reason"] = reason
	doc.Metadata = meta
	doc.PageContent = fmt.Sprintf(FallbackNotice, from, reason) + doc.PageContent
	return doc
}

// classifyPacing types a limiter's raw fast-fail (*httpx.WaitBudgetError) as
// KindQuotaExhausted with its retry-after. Engines return the limiter's
// Acquire error as-is, so without this a refused pacing wait would reach the
// transports as an untyped error: reason "error", code upstream_error, not
// retryable, no retry_after_s.
func classifyPacing(err error) error {
	var wbe *httpx.WaitBudgetError
	var fe *domain.FetchError
	if errors.As(err, &wbe) && !errors.As(err, &fe) {
		return httpx.ClassifyClientError(err, domain.KindError)
	}
	return err
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
