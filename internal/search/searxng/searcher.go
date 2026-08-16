// Package searxng implements domain.Searcher against a SearXNG instance's
// JSON API (GET /search?format=json). The instance must list `json` under
// `search.formats` in its settings.yml or every query returns 403.
package searxng

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/observability"
)

// maxResponseBytes caps how much of the SearXNG response is read; a JSON
// result page is a few hundred KB at most, so 10MB is a generous safety net.
const maxResponseBytes = 10 << 20

// Config configures the Searcher.
type Config struct {
	Endpoint string // base URL of the SearXNG instance, e.g. http://searxng:8080
	Client   *httpx.Client
	Logger   *slog.Logger
	// Metrics, when non-nil, receives per-search unresponsive-engine counts
	// (omnifeed_searxng_unresponsive_engines_total).
	Metrics *observability.Metrics
}

// Searcher queries a SearXNG instance and reshapes results into the canonical
// domain.SearchResult.
type Searcher struct {
	searchURL string
	client    *httpx.Client
	logger    *slog.Logger
	metrics   *observability.Metrics
}

// New returns a Searcher wired with the given config.
func New(cfg Config) *Searcher {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Searcher{
		searchURL: strings.TrimRight(cfg.Endpoint, "/") + "/search",
		client:    cfg.Client.WithUpstream("searxng", "search"),
		logger:    cfg.Logger,
		metrics:   cfg.Metrics,
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
func (s *Searcher) Search(ctx context.Context, query string, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is empty")
	}

	params := url.Values{}
	params.Set("q", siteScoped(query, opts.Site))
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

	// Zero results with a failure report is ambiguous: SearXNG lists only the
	// engines that failed, not how many ran, so "all engines suspended" (a real
	// outage) is indistinguishable from "some engines were in cooldown and the
	// rest honestly found nothing". Engine cooldowns are near-constant with a
	// wide engine pool, so treating this as an error turns honest zero-hit
	// queries into failures (and pages). Return what we have and log the report.
	if len(sr.UnresponsiveEngines) > 0 {
		s.logger.Warn("searxng returned partial results",
			"unresponsive_engines", formatUnresponsive(sr.UnresponsiveEngines),
			"results", len(sr.Results))
		s.countUnresponsive(sr.UnresponsiveEngines)
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

// siteScoped prefixes the query with a `site:` operator when the caller asked
// for one. SearXNG passes the operator through to the engines rather than
// filtering itself, so this is the same restriction a human types into Google
// or Brave. An invalid site value is dropped rather than rejected: it is a
// narrowing hint, and a failed search is worse for the caller than a wide one.
func siteScoped(query, site string) string {
	if site == "" || !domain.ValidSiteFilter(site) {
		return query
	}
	return "site:" + site + " " + query
}

// countUnresponsive increments the unresponsive-engine counter for each
// [engine, error_type] pair SearXNG reported. The engine name is a bounded set
// on a self-hosted instance (its own engine list); the error string is
// upstream-controlled free text, so it is normalized to a fixed vocabulary
// before becoming a label — a SearXNG upgrade that embeds detail in its
// messages must not mint unbounded permanent series. Malformed entries (empty
// pair or blank engine name) are skipped silently; a missing error type counts
// as "unknown" — the engine still failed.
func (s *Searcher) countUnresponsive(pairs [][]string) {
	if s.metrics == nil {
		return
	}
	for _, pair := range pairs {
		if len(pair) == 0 || pair[0] == "" {
			continue
		}
		errType := "unknown"
		if len(pair) > 1 && pair[1] != "" {
			errType = normalizeEngineError(pair[1])
		}
		s.metrics.ObserveUnresponsiveEngine(pair[0], errType)
	}
}

// normalizeEngineError maps SearXNG's free-text unresponsive reasons onto a
// closed label vocabulary. Substring checks, most specific first: SearXNG
// composes messages like "Suspended: too many requests" or "CAPTCHA required",
// and exception class names leak through as-is.
func normalizeEngineError(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "captcha"):
		return "captcha"
	case strings.Contains(m, "too many request"):
		return "too_many_requests"
	case strings.Contains(m, "access denied"):
		return "access_denied"
	case strings.Contains(m, "timeout"):
		return "timeout"
	case strings.Contains(m, "suspended"):
		return "suspended"
	default:
		return "error"
	}
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
