package observability

import (
	"net/http"
	"net/http/pprof"
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
	RedditRounds        prometheus.Histogram
	SearchesTotal       *prometheus.CounterVec   // searcher, status, reason
	SearchSecs          *prometheus.HistogramVec // searcher, status
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
	}
	reg.MustRegister(m.RequestsTotal, m.RequestAttempts, m.RequestSecs, m.UpstreamSecs,
		m.LimiterWaitSecs, m.ResponseChars, m.EngineFallbacks, m.SearxngUnresponsive,
		m.RedditRounds, m.SearchesTotal, m.SearchSecs)
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

// ObserveResponseChars records the character count of the content actually
// returned to the caller (post-truncation) on a successful crawl — the quality
// guard for scrape-option changes.
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

// ObserveSearch records a single search query result. reason classifies WHY a
// search failed (see Reason); it is "ok" on success. SearchSecs stays keyed on
// searcher+status only — adding reason would just inflate histogram cardinality.
func (m *Metrics) ObserveSearch(searcher, status, reason string, duration time.Duration) {
	m.SearchesTotal.WithLabelValues(searcher, status, reason).Inc()
	m.SearchSecs.WithLabelValues(searcher, status).Observe(duration.Seconds())
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
