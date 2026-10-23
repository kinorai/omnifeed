// Package tinyfish implements domain.Searcher against the TinyFish Search API
// (GET https://api.search.tinyfish.ai, X-API-Key header). TinyFish answers
// with live Google-grade results and returns structured filters SearXNG has to
// emulate: a domain allow-list instead of a `site:` operator, a recency window
// in minutes, and a per-result date.
//
// Free tier, as of 2026-10: 30 requests/minute and 12,000/day per key; past the
// daily allowance the API answers 402 until 00:00 UTC.
package tinyfish

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// DefaultEndpoint is the public TinyFish Search API.
const DefaultEndpoint = "https://api.search.tinyfish.ai"

// maxResponseBytes caps how much of a response is read; a result page is a few
// KB, so 1MB is a generous safety net.
const maxResponseBytes = 1 << 20

// defaultMaxWait bounds time spent queued in the pacing limiter when the caller
// does not set Config.MaxWait.
const defaultMaxWait = 5 * time.Second

// recencyMinutes maps domain.ValidTimeRanges onto TinyFish's recency_minutes.
var recencyMinutes = map[string]int{
	"day":   24 * 60,
	"week":  7 * 24 * 60,
	"month": 30 * 24 * 60,
	"year":  365 * 24 * 60,
}

// Config configures the Searcher.
type Config struct {
	Endpoint string // defaults to DefaultEndpoint
	APIKey   string // required; sent as X-API-Key and never logged
	Client   *httpx.Client
	Logger   *slog.Logger
	// Limiter, when non-nil, paces queries to stay under the key's
	// requests-per-minute limit. Nil sends unpaced.
	Limiter httpx.Limiter
	// MaxWait caps how long a query may sit in the Limiter before it gives up.
	// Ignored when Limiter is nil. Defaults to defaultMaxWait.
	MaxWait time.Duration
	// Now is the clock relative dates ("5 hours ago") resolve against.
	// Defaults to time.Now; tests pin it.
	Now func() time.Time
}

// Searcher queries the TinyFish Search API.
type Searcher struct {
	endpoint string
	apiKey   string
	client   *httpx.Client
	logger   *slog.Logger
	limiter  httpx.Limiter
	maxWait  time.Duration
	now      func() time.Time
}

// New returns a Searcher wired with the given config.
func New(cfg Config) *Searcher {
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultEndpoint
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = defaultMaxWait
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Searcher{
		endpoint: strings.TrimRight(cfg.Endpoint, "/"),
		apiKey:   cfg.APIKey,
		client:   withoutRedirects(cfg.Client).WithUpstream("tinyfish", "search"),
		logger:   cfg.Logger,
		limiter:  cfg.Limiter,
		maxWait:  cfg.MaxWait,
		now:      cfg.Now,
	}
}

// Name returns the searcher identifier ("tinyfish").
func (*Searcher) Name() string { return "tinyfish" }

// --- TinyFish wire types ---

type searchResponse struct {
	Results []searchHit `json:"results"`
}

type searchHit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Date    string `json:"date"`
}

// Search runs the query against TinyFish and returns up to opts.Limit results.
// TinyFish is a single source, so its report is never Degraded: when it
// answers, it answered at full strength.
func (s *Searcher) Search(ctx context.Context, query string, opts domain.SearchOptions) (domain.SearchReport, error) {
	if strings.TrimSpace(query) == "" {
		return domain.SearchReport{}, fmt.Errorf("%w: query is empty", domain.ErrInvalidQuery)
	}

	if s.limiter != nil {
		waitCtx, cancel := context.WithTimeout(ctx, s.maxWait)
		release, err := s.limiter.Acquire(waitCtx, s.Name(), s.endpoint)
		cancel()
		if err != nil {
			return domain.SearchReport{}, httpx.ClassifyClientError(err, domain.KindUpstreamError)
		}
		defer release()
	}

	resp, err := s.client.DoRetry(ctx, http.MethodGet, s.endpoint+"/?"+queryParams(query, opts).Encode(),
		nil, map[string]string{"X-API-Key": s.apiKey, "Accept": "application/json"},
		httpx.RetryConfig{RetryableStatus: notRateLimited})
	if err != nil {
		return domain.SearchReport{}, httpx.ClassifyClientError(err, domain.KindUpstreamError)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return domain.SearchReport{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return domain.SearchReport{}, s.statusError(resp.StatusCode, body)
	}

	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return domain.SearchReport{}, fmt.Errorf("decode response: %w", err)
	}
	return domain.SearchReport{Results: s.toResults(sr.Results, opts.Limit)}, nil
}

// statusError classifies a non-200 answer. 402 is the daily free allowance
// running out: an expected quota state that lifts at 00:00 UTC, not a fault,
// so it reads quota_exhausted with that wait rather than as a generic error.
func (s *Searcher) statusError(status int, body []byte) *domain.FetchError {
	fe := &domain.FetchError{
		Kind:       domain.KindForStatus(status),
		StatusCode: status,
		Err:        fmt.Errorf("tinyfish returned %d: %s", status, errorCode(body)),
	}
	if status == http.StatusPaymentRequired {
		now := s.now().UTC()
		fe.Kind = domain.KindQuotaExhausted
		fe.RetryAfter = now.Truncate(24 * time.Hour).Add(24 * time.Hour).Sub(now)
	}
	return fe
}

// notRateLimited vetoes retrying a 429. TinyFish limits requests per minute,
// so the retry loop's seconds of backoff cannot outwait it: a retry would only
// spend another request of the same minute. Its Retry-After still reaches the
// limiter through the client's OnRetryAfter hook.
func notRateLimited(status int, _ string) bool { return status != http.StatusTooManyRequests }

// withoutRedirects returns a copy of c that never follows a redirect. The API
// key rides in X-API-Key, which net/http — unlike Authorization — copies onto
// a redirect to ANOTHER host, so a 3xx from the endpoint (or anything between)
// would hand the key to whoever it points at. The API has no reason to
// redirect; a 3xx surfaces as an error status instead.
func withoutRedirects(c *httpx.Client) *httpx.Client {
	if c == nil {
		return nil
	}
	cp := *c
	hc := http.Client{}
	if c.HTTP != nil {
		hc = *c.HTTP
	}
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	cp.HTTP = &hc
	return &cp
}

// queryParams builds the request. The site filter goes in include_domains,
// TinyFish's own allow-list, rather than a `site:` operator in the query text,
// which TinyFish documents as deprecated for domain filtering.
func queryParams(query string, opts domain.SearchOptions) url.Values {
	params := url.Values{}
	params.Set("query", query)
	if opts.Site != "" && domain.ValidSiteFilter(opts.Site) {
		params.Set("include_domains", opts.Site)
	}
	if minutes, ok := recencyMinutes[opts.TimeRange]; ok {
		params.Set("recency_minutes", strconv.Itoa(minutes))
	}
	if opts.Language != "" {
		params.Set("language", opts.Language)
	}
	return params
}

func (s *Searcher) toResults(hits []searchHit, limit int) []domain.SearchResult {
	now := s.now()
	results := make([]domain.SearchResult, 0, len(hits))
	for _, hit := range hits {
		results = append(results, domain.SearchResult{
			Title:         hit.Title,
			URL:           hit.URL,
			Snippet:       hit.Snippet,
			Engine:        s.Name(),
			PublishedDate: parseDate(hit.Date, now),
		})
		if limit > 0 && len(results) >= limit {
			break
		}
	}
	return results
}

// errorCode extracts TinyFish's machine-readable error code from an error body
// ({"error":{"code":"INSUFFICIENT_CREDITS",...}}), so a 402 says WHICH quota
// ran out. The message is left out: it may echo request details.
func errorCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error.Code == "" {
		return "no error code"
	}
	return e.Error.Code
}
