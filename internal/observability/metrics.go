package observability

import (
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds Prometheus collectors emitted by the proxy.
type Metrics struct {
	registry *prometheus.Registry

	RequestsTotal *prometheus.CounterVec   // engine, tenant, status
	RequestSecs   *prometheus.HistogramVec // engine, status
	RedditRounds  prometheus.Histogram
}

// NewMetrics builds and registers all collectors.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "carp_requests_total",
			Help: "Total /crawl requests by engine, tenant, and status.",
		}, []string{"engine", "tenant", "status"}),
		RequestSecs: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "carp_request_seconds",
			Help:    "Crawl latency by engine and status.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
		}, []string{"engine", "status"}),
		RedditRounds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "carp_reddit_expansion_rounds",
			Help:    "Number of /api/morechildren rounds per Reddit crawl.",
			Buckets: prometheus.LinearBuckets(0, 5, 9),
		}),
	}
	reg.MustRegister(m.RequestsTotal, m.RequestSecs, m.RedditRounds)
	reg.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
	)
	return m
}

// Observe records a single crawl result.
func (m *Metrics) Observe(engine, tenant, status string, duration time.Duration) {
	m.RequestsTotal.WithLabelValues(engine, tenant, status).Inc()
	m.RequestSecs.WithLabelValues(engine, status).Observe(duration.Seconds())
}

// RegisterMetrics attaches /metrics to mux.
func (m *Metrics) RegisterMetrics(mux *http.ServeMux) {
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
}

// RegisterPprof attaches /debug/pprof/* to mux. Opt-in via CARP_ENABLE_PPROF.
func RegisterPprof(mux *http.ServeMux) {
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
}
