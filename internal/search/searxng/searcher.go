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
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/observability"
)

// maxResponseBytes caps how much of the SearXNG response is read; a JSON
// result page is a few hundred KB at most, so 10MB is a generous safety net.
const maxResponseBytes = 10 << 20

// defaultMaxWait bounds time spent queued in the pacing limiter when the caller
// does not set Config.MaxWait. Generous enough to absorb a few queries at a
// realistic delay, short enough that a caller learns it is being throttled
// instead of hanging.
const defaultMaxWait = 15 * time.Second

// Config configures the Searcher.
type Config struct {
	Endpoint string // base URL of the SearXNG instance, e.g. http://searxng:8080
	Client   *httpx.Client
	Logger   *slog.Logger
	// Metrics, when non-nil, receives per-search unresponsive-engine counts
	// (omnifeed_searxng_unresponsive_engines_total).
	Metrics *observability.Metrics
	// SiteEngines restricts which SearXNG engines run when the caller passes a
	// Site filter. SearXNG forwards `site:` to the engines rather than applying
	// it itself, and engines differ wildly in what they do with it: some honour
	// it, some ignore the operator and answer with unrelated pages, and some
	// return nothing at all for any `site:` query. On a pool with the last kind
	// in it, a site-scoped search is answered by whichever engines are left,
	// with the ignorers' noise ranking high because nothing corroborates it.
	// Listing the engines that implement the operator keeps the filter
	// meaningful. Empty (the default) queries the whole pool, as before.
	SiteEngines []string
	// Limiter, when non-nil, paces queries to the SearXNG instance. It exists
	// for the ENGINES behind SearXNG, not for SearXNG itself: SearXNG fans one
	// query out to every enabled engine, so each engine sees exactly this
	// searcher's query rate, and an engine that decides the rate is bot-like
	// suspends itself (or gets CAPTCHA'd) for the whole pool's benefit. Nil
	// keeps the previous unpaced behaviour.
	Limiter *httpx.DomainLimiter
	// MaxWait caps how long a query may sit in the Limiter before it gives up.
	// Ignored when Limiter is nil. Defaults to defaultMaxWait.
	MaxWait time.Duration
	// Audit selects the per-search audit log: "off" (default), "summary" or
	// "full". See config.Config.SearchAudit for why this is its own setting
	// rather than a log level.
	Audit string
}

// Searcher queries a SearXNG instance and reshapes results into the canonical
// domain.SearchResult.
type Searcher struct {
	searchURL   string
	client      *httpx.Client
	logger      *slog.Logger
	metrics     *observability.Metrics
	siteEngines string // pre-joined for the `engines` param; empty = all engines
	limiter     *httpx.DomainLimiter
	maxWait     time.Duration
	audit       string
}

// New returns a Searcher wired with the given config.
func New(cfg Config) *Searcher {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxWait <= 0 {
		cfg.MaxWait = defaultMaxWait
	}
	return &Searcher{
		searchURL:   strings.TrimRight(cfg.Endpoint, "/") + "/search",
		client:      cfg.Client.WithUpstream("searxng", "search"),
		logger:      cfg.Logger,
		metrics:     cfg.Metrics,
		siteEngines: strings.Join(cfg.SiteEngines, ","),
		limiter:     cfg.Limiter,
		maxWait:     cfg.MaxWait,
		audit:       cfg.Audit,
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
	// Engines and Positions are parallel arrays: SearXNG merges every engine's
	// ranking into one list and records, per result, which engines returned it
	// and at what rank in each engine's OWN list. Together they reconstruct
	// each engine's raw ranking from a single response — no per-engine fan-out
	// needed. Score is SearXNG's merged rank weight (see searx/results.py).
	Engines   []string `json:"engines"`
	Positions []int    `json:"positions"`
	Score     float64  `json:"score"`
}

// Search runs the query against SearXNG and returns up to opts.Limit results.
func (s *Searcher) Search(ctx context.Context, query string, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is empty")
	}

	queryID := newQueryID()
	params := url.Values{}
	scoped := siteScoped(query, opts.Site)
	params.Set("q", scoped)
	params.Set("format", "json")
	// Only when the operator actually made it into the query: an invalid Site is
	// dropped by siteScoped, and narrowing the engine pool for a search that is
	// no longer site-scoped would cost recall for nothing.
	if s.siteEngines != "" && scoped != query {
		params.Set("engines", s.siteEngines)
	}
	if opts.TimeRange != "" {
		params.Set("time_range", opts.TimeRange)
	}
	if opts.Language != "" {
		params.Set("language", opts.Language)
	}

	// Pacing is bounded by maxWait, not left to run to the caller's deadline. At
	// a quota of 14/90s a fan-out of parallel searches serializes behind one
	// slot, and an unbounded wait would let the last caller queue for minutes —
	// past the HTTP server's write timeout, which drops the connection with no
	// response body at all. Failing fast with a timeout tells the caller to back
	// off, and the limiter's outcome="canceled" series makes the pressure
	// visible. The error is classified rather than returned raw: a local pacing
	// wait must not be reported to the caller as an upstream SearXNG failure.
	if s.limiter != nil {
		waitCtx, cancel := context.WithTimeout(ctx, s.maxWait)
		release, lerr := s.limiter.Acquire(waitCtx, s.Name(), s.searchURL)
		cancel()
		if lerr != nil {
			return nil, httpx.ClassifyClientError(lerr, domain.KindUpstreamError)
		}
		defer release()
	}

	// Timed from here on purpose: the limiter above can hold a query for up to
	// maxWait, and folding that into duration_ms would report local
	// backpressure as upstream latency — the opposite of what an operator
	// comparing engine pools needs. Queue time has its own metric
	// (omnifeed_domain_limiter_wait_seconds).
	started := time.Now()
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
		// The line above joins every failure into one string, which reads fine
		// and cannot be aggregated. These carry engine and reason as their own
		// fields so a log store can group by them.
		s.logUnresponsive(queryID, sr.UnresponsiveEngines)
	}

	// Zero results with NO failure report is the shape a silently blocked pool
	// produces — several engines answer a block with HTTP 200 and an empty
	// result set, which SearXNG cannot tell apart from an honest zero-hit
	// query. Neither can this code, so it does not fail the search. It records
	// the two signals that let the difference be seen over time: which engines
	// are still contributing rows, and how often searches come back empty.
	s.countEngineResults(sr.Results)
	if len(sr.Results) == 0 && len(sr.UnresponsiveEngines) == 0 {
		s.logger.Warn("searxng returned no results and no failure report",
			"query", scoped, "site_scoped", scoped != query)
		if s.metrics != nil {
			s.metrics.ObserveEmptySearch(scoped != query)
		}
	}

	s.observeRanks(sr.Results)
	s.logAudit(queryID, query, scoped, opts, sr, time.Since(started))

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
		// Mint the engine's results series at zero. A counter that never
		// existed cannot go flat, so an engine already failing when the process
		// started would be invisible to a rate()-based alert; naming it here
		// creates the series the moment SearXNG first mentions the engine.
		s.metrics.ObserveEngineResults(pair[0], 0)
	}
}

// countEngineResults tallies how many rows each engine contributed, counting
// the full response rather than the caller's limited slice — the metric answers
// "is this engine still returning anything", which a caller-side limit would
// distort. Rows with no engine name are skipped: the label must stay bounded by
// the instance's own engine list.
func (s *Searcher) countEngineResults(hits []searchHit) {
	if s.metrics == nil || len(hits) == 0 {
		return
	}
	rows := make(map[string]int, len(hits))
	for _, hit := range hits {
		if hit.Engine != "" {
			rows[hit.Engine]++
		}
	}
	for engine, n := range rows {
		s.metrics.ObserveEngineResults(engine, n)
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

// --- audit log ---
//
// The per-search record that makes engine quality measurable. It is a data
// feed, not diagnostics, so it is gated by its own setting and emitted at INFO
// (see config.Config.SearchAudit).
//
// SHAPE: one flat line per fact, never a nested table. Log stores that flatten
// JSON — VictoriaLogs among them — turn arrays into opaque strings, so a line
// carrying `positions: [1,1,4,7]` stores the literal text "[1, 1, 4, 7]" and
// can never be grouped or averaged. One line per (engine, result) keeps every
// value in a field of its own, which is what makes `stats by (engine)` work.
// Lines are joined by query_id.

const (
	auditOff     = "off"
	auditSummary = "summary"
	auditFull    = "full"
)

// newQueryID returns the correlation id shared by every line of one search.
// Non-cryptographic: it only has to be unique among the searches a log store
// holds at once, and it is never a secret or a capability.
// #nosec G404 -- log correlation id, not a security token
func newQueryID() string {
	return strconv.FormatUint(rand.Uint64(), 36) //nolint:gosec // see above
}

// logAudit emits the search-level line, plus the per-result position table in
// "full" mode.
func (s *Searcher) logAudit(queryID, query, scoped string, opts domain.SearchOptions, sr searchResponse, took time.Duration) {
	if s.auditDisabled() {
		return
	}
	rows := make(map[string]int, len(sr.Results))
	for _, hit := range sr.Results {
		for _, engine := range hitEngines(hit) {
			rows[engine]++
		}
	}
	// engine_rows is rendered as "engine=n" pairs rather than a map or an
	// array, for the same flattening reason as above: it stays greppable in
	// every store, and the authoritative per-engine numbers are the
	// omnifeed_search_engine_* metrics anyway.
	s.logger.Info("search audit",
		"query_id", queryID,
		"query", query,
		"site", opts.Site,
		"time_range", opts.TimeRange,
		"site_scoped", scoped != query,
		"total", len(sr.Results),
		"limit", opts.Limit,
		"duration_ms", took.Milliseconds(),
		"engine_rows", formatRows(rows),
	)
	if s.audit != auditFull {
		return
	}
	for mergedRank, hit := range sr.Results {
		engines := hitEngines(hit)
		unique := len(engines) == 1
		for i, engine := range engines {
			// rank 0 records "this engine returned it, position unknown".
			rank := rankOf(hit, i)
			s.logger.Info("search result",
				"query_id", queryID,
				"engine", engine,
				"position", rank,
				"merged_rank", mergedRank+1,
				"score", hit.Score,
				"url", hit.URL,
				"unique", unique,
			)
		}
	}
}

// logUnresponsive emits one line per failed engine, with engine and reason as
// separate fields.
func (s *Searcher) logUnresponsive(queryID string, pairs [][]string) {
	if s.auditDisabled() {
		return
	}
	for _, pair := range pairs {
		if len(pair) == 0 || pair[0] == "" {
			continue
		}
		reason := ""
		if len(pair) > 1 {
			reason = pair[1]
		}
		s.logger.Info("search engine unresponsive",
			"query_id", queryID,
			"engine", pair[0],
			"reason", reason,
			"reason_class", normalizeEngineError(reason),
		)
	}
}

// hitEngines returns the engines that produced a result. SearXNG sends the
// `engines` array; a response carrying only the legacy `engine` field falls
// back to it, so the audit log and the metrics agree with
// omnifeed_searxng_engine_hits (which reads `engine`) instead of reporting an
// empty pool for the same search.
func hitEngines(hit searchHit) []string {
	if len(hit.Engines) > 0 {
		return hit.Engines
	}
	if hit.Engine != "" {
		return []string{hit.Engine}
	}
	return nil
}

// rankOf returns the engine's own position for the i-th engine of a hit, or 0
// when SearXNG did not report one. Positions is parallel to Engines, but it is
// upstream data: a short or absent array must not panic.
func rankOf(hit searchHit, i int) int {
	if i < len(hit.Positions) {
		return hit.Positions[i]
	}
	return 0
}

// observeRanks feeds the two aggregate metrics. It runs whatever the audit
// setting is: these carry no query text and no URLs, so there is nothing to
// gate.
func (s *Searcher) observeRanks(hits []searchHit) {
	if s.metrics == nil {
		return
	}
	for _, hit := range hits {
		engines := hitEngines(hit)
		unique := len(engines) == 1
		for i, engine := range engines {
			if engine == "" {
				continue
			}
			// An unknown rank is passed through as 0 rather than skipped: the
			// metric drops it from the histogram but still counts a unique
			// contribution. Skipping here would lose that.
			s.metrics.ObserveEngineRank(engine, rankOf(hit, i), unique)
		}
	}
}

// auditDisabled reports whether the audit log is disabled. The empty string counts
// as off so a zero-valued Config keeps the previous behaviour.
func (s *Searcher) auditDisabled() bool {
	return s.audit == "" || s.audit == auditOff
}

// formatRows renders per-engine row counts as a stable, sorted "engine=n"
// string.
func formatRows(rows map[string]int) string {
	if len(rows) == 0 {
		return ""
	}
	names := make([]string, 0, len(rows))
	for engine := range rows {
		names = append(names, engine)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, engine := range names {
		parts = append(parts, engine+"="+strconv.Itoa(rows[engine]))
	}
	return strings.Join(parts, " ")
}
