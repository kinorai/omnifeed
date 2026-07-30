// Package searxng implements domain.Searcher against a SearXNG instance's
// JSON API (GET /search?format=json). The instance must list `json` under
// `search.formats` in its settings.yml or every query returns 403.
package searxng

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// maxResponseBytes caps how much of the SearXNG response is read; a JSON
// result page is a few hundred KB at most, so 10MB is a generous safety net.
const maxResponseBytes = 10 << 20

// Config configures the Searcher.
type Config struct {
	Endpoint string // base URL of the SearXNG instance, e.g. http://searxng:8080
	Client   *httpx.Client
	// Limiter paces outbound queries to the SearXNG host. Upstream SearXNG has
	// no outbound rate limiting of its own (rejected twice: PRs #998, #1273), so
	// bursts trip its engines' bot detection and get them suspended — pacing can
	// only live here. Optional; nil disables pacing.
	Limiter *httpx.DomainLimiter
	// DegradedRetryDelay is how long to wait before the single retry of a
	// degraded search (see Search). 0 disables the retry.
	DegradedRetryDelay time.Duration
	Logger             *slog.Logger
}

// Searcher queries a SearXNG instance and reshapes results into the canonical
// domain.SearchResult.
type Searcher struct {
	searchURL          string
	client             *httpx.Client
	limiter            *httpx.DomainLimiter
	degradedRetryDelay time.Duration
	logger             *slog.Logger
}

// New returns a Searcher wired with the given config.
func New(cfg Config) *Searcher {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Searcher{
		searchURL:          strings.TrimRight(cfg.Endpoint, "/") + "/search",
		client:             cfg.Client,
		limiter:            cfg.Limiter,
		degradedRetryDelay: cfg.DegradedRetryDelay,
		logger:             cfg.Logger,
	}
}

// Name returns the searcher identifier ("searxng").
func (*Searcher) Name() string { return "searxng" }

// --- SearXNG wire types ---

type searchResponse struct {
	Results []searchHit `json:"results"`
	// UnresponsiveEngines is SearXNG's per-query failure report, emitted on the
	// HTTP 200 path as [[engine_name, message], …] — message is e.g.
	// "Suspended: too many requests" or "timeout". A suspended engine is skipped
	// without a network call, so an all-suspended query returns 200 + no results
	// near-instantly; without this field that is indistinguishable from an
	// honest zero-hit search.
	UnresponsiveEngines [][]string `json:"unresponsive_engines"`
}

type searchHit struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Content       string `json:"content"`
	Engine        string `json:"engine"`
	PublishedDate string `json:"publishedDate"`
}

// Search runs the query against SearXNG and returns up to opts.Limit results.
//
// A degraded upstream (engines suspended after a 429/CAPTCHA, so the query
// returned 200 + zero results) is retried exactly once after
// DegradedRetryDelay: the suspension is per-engine and time-boxed, so a delayed
// retry genuinely recovers results. Whatever the retry yields — results or a
// second degraded error — is returned as-is. Metrics stay caller-side
// (metrics.ObserveSearch wraps this call), so the retry is folded into one
// logical search rather than counted twice; the first degraded attempt is
// visible in the logs.
func (s *Searcher) Search(ctx context.Context, query string, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	results, err := s.query(ctx, query, opts)
	if err == nil || s.degradedRetryDelay <= 0 || !isDegraded(err) {
		return results, err
	}

	s.logger.Warn("searxng search degraded, retrying once",
		"delay", s.degradedRetryDelay.String(), "error", err)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(s.degradedRetryDelay):
	}
	return s.query(ctx, query, opts)
}

// isDegraded reports whether err is the degraded-upstream signal.
func isDegraded(err error) bool {
	var fe *domain.FetchError
	return errors.As(err, &fe) && fe.Kind == domain.KindDegraded
}

// query performs one SearXNG search attempt.
func (s *Searcher) query(ctx context.Context, query string, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is empty")
	}

	if s.limiter != nil {
		release := s.limiter.Acquire(s.searchURL)
		defer release()
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	if opts.TimeRange != "" {
		params.Set("time_range", opts.TimeRange)
	}
	if opts.Language != "" {
		params.Set("language", opts.Language)
	}

	resp, err := s.client.DoRetry(ctx, http.MethodGet, s.searchURL+"?"+params.Encode(),
		nil, nil, httpx.RetryConfig{})
	if err != nil {
		return nil, httpx.ClassifyClientError(err, domain.KindUpstreamError)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &domain.FetchError{
			Kind:       domain.KindForStatus(resp.StatusCode),
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("searxng returned %d (is the json format enabled in settings.yml?)", resp.StatusCode),
		}
	}

	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// Zero results with a failure report means the engines never ran (suspended
	// after a 429/CAPTCHA, or timed out) — a degraded upstream, not "nothing
	// matched". Return it as an error so callers retry instead of concluding the
	// query has no answers; Reason() reads KindDegraded off the FetchError.
	if len(sr.Results) == 0 && len(sr.UnresponsiveEngines) > 0 {
		return nil, &domain.FetchError{
			Kind: domain.KindDegraded,
			Err: fmt.Errorf("search degraded: %d engines unavailable (%s) — retry shortly",
				len(sr.UnresponsiveEngines), formatUnresponsive(sr.UnresponsiveEngines)),
		}
	}
	if len(sr.UnresponsiveEngines) > 0 {
		s.logger.Warn("searxng returned partial results",
			"unresponsive_engines", formatUnresponsive(sr.UnresponsiveEngines),
			"results", len(sr.Results))
	}

	results := make([]domain.SearchResult, 0, len(sr.Results))
	for _, hit := range sr.Results {
		results = append(results, domain.SearchResult{
			Title:         hit.Title,
			URL:           hit.URL,
			Snippet:       hit.Content,
			Engine:        hit.Engine,
			PublishedDate: hit.PublishedDate,
		})
		if opts.Limit > 0 && len(results) >= opts.Limit {
			break
		}
	}
	return results, nil
}

// formatUnresponsive renders SearXNG's [[engine, message], …] pairs as
// "brave: Suspended: too many requests; duckduckgo: timeout". Entries are
// tolerated with a missing message (SearXNG has emitted 1-element pairs).
func formatUnresponsive(pairs [][]string) string {
	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		switch len(pair) {
		case 0:
			continue
		case 1:
			parts = append(parts, pair[0])
		default:
			parts = append(parts, pair[0]+": "+pair[1])
		}
	}
	return strings.Join(parts, "; ")
}
