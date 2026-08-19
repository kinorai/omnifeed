package observability

import (
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds Prometheus collectors emitted by the proxy.
type Metrics struct {
	registry *prometheus.Registry

	RequestsTotal       *prometheus.CounterVec   // engine, tenant, status, reason
	RequestAttempts     *prometheus.CounterVec   // upstream, attempt (first|retry)
	RequestSecs         *prometheus.HistogramVec // engine, status, reason
	UpstreamSecs        *prometheus.HistogramVec // upstream, op, status
	LimiterWaitSecs     *prometheus.HistogramVec // engine
	ResponseChars       *prometheus.HistogramVec // engine
	EngineFallbacks     *prometheus.CounterVec   // from_engine, reason
	SearxngUnresponsive *prometheus.CounterVec   // engine, error
	SearxngEngineHits   *prometheus.CounterVec   // engine
	SearxngEmpty        *prometheus.CounterVec   // scoped
	RedditRounds        prometheus.Histogram
	SearchesTotal       *prometheus.CounterVec   // searcher, status, reason
	SearchSecs          *prometheus.HistogramVec // searcher, status
	SearchRoutes        *prometheus.CounterVec   // vertical, outcome
	SearchEnginePos     *prometheus.HistogramVec // engine
	SearchEngineUnique  *prometheus.CounterVec   // engine
}

// NewMetrics builds and registers all collectors.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omnifeed_requests_total",
			Help: "Total /crawl requests by engine, tenant, status, and failure reason.",
		}, []string{"engine", "tenant", "status", "reason"}),
		RequestAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omnifeed_request_attempts_total",
			Help: `HTTP attempts by the retrying client, by upstream, split into the first try and retries. attempt="retry" is retry volume; it drops when a non-transient block stops being re-driven.`,
		}, []string{"upstream", "attempt"}),
		RequestSecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "omnifeed_request_seconds",
			Help: "Crawl latency by engine, status, and failure reason (reason=\"ok\" on success).",
			// 14 buckets → top ~409.6s: end-to-end can stack limiter wait +
			// 3 retried 90s attempts (or a 4m Reddit crawl); a 102.4s ceiling
			// would push exactly the pathological tail into +Inf.
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 14),
		}, []string{"engine", "status", "reason"}),
		UpstreamSecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "omnifeed_upstream_seconds",
			Help:    "Upstream HTTP round-trip latency per attempt (request start until the response body is fully read, or transport error). One observation per attempt, so a 3-try DoRetry records 3.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
		}, []string{"upstream", "op", "status"}),
		LimiterWaitSecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "omnifeed_domain_limiter_wait_seconds",
			Help: "Time spent blocked in per-domain limiter acquisition (semaphore wait + politeness delay); ~0 when uncontended. outcome=\"canceled\" is a wait that died in the queue (caller gave up) — the worst waits, which never acquire.",
			// 14 buckets → top ~81.9s: queued-behind-a-slow-domain waits run
			// to the caller's deadline (90s crawl timeouts), well past 20s.
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 14),
		}, []string{"engine", "outcome"}),
		ResponseChars: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "omnifeed_response_chars",
			Help:    "Extracted content length in characters on successful crawls, pre-truncation (the engine's output, not what a transport delivered after max_chars) — comparable across transports, the quality guard for scrape-option changes.",
			Buckets: prometheus.ExponentialBuckets(100, 4, 10),
		}, []string{"engine"}),
		EngineFallbacks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omnifeed_engine_fallbacks_total",
			Help: "Re-crawls via the generic fallback engine after a dedicated engine failed, by failing engine and classified failure reason.",
		}, []string{"from_engine", "reason"}),
		SearxngUnresponsive: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omnifeed_searxng_unresponsive_engines_total",
			Help: "SearXNG engines reported unresponsive per search (the unresponsive_engines field), by engine and error type.",
		}, []string{"engine", "error"}),
		SearxngEngineHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omnifeed_searxng_engine_results_total",
			Help: "Result rows returned per SearXNG engine. The silent-block detector: an engine blocked by its upstream keeps answering HTTP 200 with an empty result set and no unresponsive_engines entry, so it goes flat here while the rest of the pool keeps moving. Alert on the divergence, not on the absolute rate. CAVEAT: a series exists only once that engine has been named in a response (a returned row, or an unresponsive_engines entry). An engine that is ALREADY blocked when the process starts, and blocked silently, mints no series at all — so an alert written as rate(...) == 0 matches nothing for it. Pair the rule with absent_over_time() over the engines you expect in the pool.",
		}, []string{"engine"}),
		SearxngEmpty: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omnifeed_searxng_empty_searches_total",
			Help: "Searches that returned zero results AND no unresponsive_engines report. SearXNG cannot distinguish these from an honest zero-hit query, so a rising rate — especially with scoped=\"true\" — is the signature of engines that block silently.",
		}, []string{"scoped"}),
		RedditRounds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "omnifeed_reddit_expansion_rounds",
			Help:    "Number of /api/morechildren rounds per Reddit crawl.",
			Buckets: prometheus.LinearBuckets(0, 5, 9),
		}),
		SearchesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omnifeed_search_requests_total",
			Help: "Total search queries by searcher, status, and failure reason.",
		}, []string{"searcher", "status", "reason"}),
		SearchSecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "omnifeed_search_request_seconds",
			Help:    "Search latency by searcher and status.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 10),
		}, []string{"searcher", "status"}),
		// Where in its own ranking an engine placed the rows it contributed.
		// Buckets are ranks, not seconds: 1-3 is a result a caller actually
		// reads, 20+ is filler. An engine whose distribution sits deep is
		// contributing volume, not answers.
		SearchEnginePos: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "omnifeed_search_engine_position_rank",
			Help:    "Per-engine rank of each result an engine returned.",
			Buckets: []float64{1, 2, 3, 5, 10, 20, 50},
		}, []string{"engine"}),
		// Results NO other engine in the pool returned. This is the metric that
		// decides whether an engine earns its slot: an engine with high volume
		// and near-zero unique contribution is duplicating the pool and can be
		// dropped without losing anything.
		SearchEngineUnique: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omnifeed_search_engine_unique_results_total",
			Help: "Results contributed by exactly one engine, by that engine.",
		}, []string{"engine"}),
		SearchRoutes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "omnifeed_search_routes_total",
			Help: `Router dispatches to a native vertical searcher, by vertical and outcome. outcome="served" means the vertical's own results were returned; every other outcome — "empty" (it found nothing), "declined" (it does not serve this query shape) and "error" (it failed) — also produced a SearXNG fallback query, so those searches cost two upstream calls.`,
		}, []string{"vertical", "outcome"}),
	}
	reg.MustRegister(m.RequestsTotal, m.RequestAttempts, m.RequestSecs, m.UpstreamSecs,
		m.LimiterWaitSecs, m.ResponseChars, m.EngineFallbacks, m.SearxngUnresponsive,
		m.SearxngEngineHits, m.SearxngEmpty,
		m.RedditRounds, m.SearchesTotal, m.SearchSecs, m.SearchRoutes,
		m.SearchEnginePos, m.SearchEngineUnique)
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Observe records a single crawl result. reason is a bounded classification of
// WHY a request failed (see Reason); it is "ok" on success.
func (m *Metrics) Observe(engine, tenant, status, reason string, duration time.Duration) {
	m.RequestsTotal.WithLabelValues(engine, tenant, status, reason).Inc()
	m.RequestSecs.WithLabelValues(engine, status, reason).Observe(duration.Seconds())
}

// ObserveAttempt records one HTTP attempt from the retrying client against the
// named upstream. retry is false for the first try and true for each retry, so
// attempt="retry" counts the re-drives that #2's RetryableStatus veto removes
// for non-transient blocks.
func (m *Metrics) ObserveAttempt(upstream string, retry bool) {
	attempt := "first"
	if retry {
		attempt = "retry"
	}
	m.RequestAttempts.WithLabelValues(upstream, attempt).Inc()
}

// ObserveUpstream records one upstream HTTP round-trip attempt (request start
// until the response body is fully read, or transport error). status is "ok"
// for 2xx and "error" otherwise. Wired as the httpx client's OnUpstream hook.
func (m *Metrics) ObserveUpstream(upstream, op, status string, duration time.Duration) {
	m.UpstreamSecs.WithLabelValues(upstream, op, status).Observe(duration.Seconds())
}

// ObserveLimiterWait records the time an engine spent blocked acquiring the
// per-domain limiter. Wired as the DomainLimiter's OnWait hook. outcome is
// "acquired" or "canceled" (the wait died in the queue).
func (m *Metrics) ObserveLimiterWait(engine, outcome string, duration time.Duration) {
	m.LimiterWaitSecs.WithLabelValues(engine, outcome).Observe(duration.Seconds())
}

// ObserveResponseChars records the character count of a successful crawl's
// extracted content, PRE-truncation (the engine's output, before any transport
// max_chars clipping) — the quality guard for scrape-option changes. Recorded
// at the registry choke point so the label names the engine that actually
// produced the document, fallbacks included.
func (m *Metrics) ObserveResponseChars(engine string, chars int) {
	m.ResponseChars.WithLabelValues(engine).Observe(float64(chars))
}

// ObserveFallback counts one engine→generic-fallback handoff: fromEngine is the
// dedicated engine that failed, reason its classified failure (see Reason).
func (m *Metrics) ObserveFallback(fromEngine, reason string) {
	m.EngineFallbacks.WithLabelValues(fromEngine, reason).Inc()
}

// ObserveUnresponsiveEngine counts one engine SearXNG reported unresponsive on
// a search response, by engine name and error type.
func (m *Metrics) ObserveUnresponsiveEngine(engine, errType string) {
	m.SearxngUnresponsive.WithLabelValues(engine, errType).Inc()
}

// ObserveEngineResults counts the rows one SearXNG engine contributed to a
// search response. Engines that block silently — HTTP 200, no error, empty
// results — are invisible in every other metric, and this is what makes them
// visible: their series stops advancing while the rest of the pool continues.
func (m *Metrics) ObserveEngineResults(engine string, rows int) {
	m.SearxngEngineHits.WithLabelValues(engine).Add(float64(rows))
}

// ObserveEmptySearch counts a search that came back with no results and no
// failure report — the shape a silently blocked pool produces, and the shape an
// honest zero-hit query produces. scoped says whether a `site:` filter was
// applied, because the site-scoped path is where silent blocks concentrate.
func (m *Metrics) ObserveEmptySearch(scoped bool) {
	m.SearxngEmpty.WithLabelValues(strconv.FormatBool(scoped)).Inc()
}

// ObserveSearch records a single search query result. reason classifies WHY a
// search failed (see Reason); it is "ok" on success. SearchSecs stays keyed on
// searcher+status only — adding reason would just inflate histogram cardinality.
func (m *Metrics) ObserveSearch(searcher, status, reason string, duration time.Duration) {
	m.SearchesTotal.WithLabelValues(searcher, status, reason).Inc()
	m.SearchSecs.WithLabelValues(searcher, status).Observe(duration.Seconds())
}

// ObserveEngineRank records the rank an engine gave one of its own results,
// and whether that result was unique to it. Both are aggregates only — the URL
// and the query stay in the audit log, never in a label.
func (m *Metrics) ObserveEngineRank(engine string, rank int, unique bool) {
	m.SearchEnginePos.WithLabelValues(engine).Observe(float64(rank))
	if unique {
		m.SearchEngineUnique.WithLabelValues(engine).Inc()
	}
}

// ObserveSearchRoute counts one router dispatch to a native vertical searcher.
// outcome is "served", "empty", "declined" or "error"; everything but "served"
// was followed by a SearXNG fallback query.
func (m *Metrics) ObserveSearchRoute(vertical, outcome string) {
	m.SearchRoutes.WithLabelValues(vertical, outcome).Inc()
}

// RegisterMetrics attaches /metrics to mux.
func (m *Metrics) RegisterMetrics(mux *http.ServeMux) {
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
}

// RegisterPprof attaches /debug/pprof/* to mux. Opt-in via OMNIFEED_ENABLE_PPROF.
func RegisterPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}
