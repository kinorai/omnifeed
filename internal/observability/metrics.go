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

	RequestsTotal   *prometheus.CounterVec   // engine, tenant, status, reason
	RequestAttempts *prometheus.CounterVec   // attempt (first|retry)
	RequestSecs     *prometheus.HistogramVec // engine, status
	RedditRounds    prometheus.Histogram
	SearchesTotal   *prometheus.CounterVec   // searcher, status, reason
	SearchSecs      *prometheus.HistogramVec // searcher, status
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
			Help: `HTTP attempts by the retrying crawl client, split into the first try and retries. attempt="retry" is retry volume; it drops when a non-transient block stops being re-driven.`,
		}, []string{"attempt"}),
		RequestSecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "omnifeed_request_seconds",
			Help:    "Crawl latency by engine and status.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
		}, []string{"engine", "status"}),
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
	reg.MustRegister(m.RequestsTotal, m.RequestAttempts, m.RequestSecs, m.RedditRounds, m.SearchesTotal, m.SearchSecs)
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
	m.RequestSecs.WithLabelValues(engine, status).Observe(duration.Seconds())
}

// ObserveAttempt records one HTTP attempt from the retrying crawl client. retry
// is false for the first try and true for each retry, so attempt="retry" counts
// the re-drives that #2's RetryableStatus veto removes for non-transient blocks.
func (m *Metrics) ObserveAttempt(retry bool) {
	attempt := "first"
	if retry {
		attempt = "retry"
	}
	m.RequestAttempts.WithLabelValues(attempt).Inc()
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
