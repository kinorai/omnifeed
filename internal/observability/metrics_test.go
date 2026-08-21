package observability

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// counterValue reads a CounterVec child's current value.
func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var dm dto.Metric
	if err := c.Write(&dm); err != nil {
		t.Fatal(err)
	}
	return dm.GetCounter().GetValue()
}

// histogramCount reads a HistogramVec child's sample count. WithLabelValues
// returns a prometheus.Observer; the concrete child also implements
// prometheus.Metric, which carries Write.
func histogramCount(t *testing.T, o prometheus.Observer) uint64 {
	t.Helper()
	m, ok := o.(prometheus.Metric)
	if !ok {
		t.Fatalf("observer %T does not implement prometheus.Metric", o)
	}
	var dm dto.Metric
	if err := m.Write(&dm); err != nil {
		t.Fatal(err)
	}
	return dm.GetHistogram().GetSampleCount()
}

// NewMetrics must register cleanly (no duplicate collectors), and RedditRounds
// must actually record. The histogram was previously registered but never
// observed anywhere, so its dashboard panel sat empty — this guards the wiring.
func TestRedditRoundsObserved(t *testing.T) {
	m := NewMetrics() // panics on duplicate registration
	m.RedditRounds.Observe(3)

	var dm dto.Metric
	if err := m.RedditRounds.Write(&dm); err != nil {
		t.Fatal(err)
	}
	if got := dm.GetHistogram().GetSampleCount(); got != 1 {
		t.Fatalf("RedditRounds sample count = %d, want 1", got)
	}
}

// ObserveAttempt must record under the upstream+attempt labels so retry volume
// is visible per upstream (and so #2's drop in re-drives is measurable, not
// just reconstructed from logs).
func TestObserveAttemptCounts(t *testing.T) {
	m := NewMetrics()
	m.ObserveAttempt("crawl4ai", false)
	m.ObserveAttempt("crawl4ai", true)
	m.ObserveAttempt("crawl4ai", true)
	m.ObserveAttempt("searxng", false)

	if got := counterValue(t, m.RequestAttempts.WithLabelValues("crawl4ai", "retry")); got != 2 {
		t.Fatalf(`crawl4ai attempt="retry" counter = %v, want 2`, got)
	}
	if got := counterValue(t, m.RequestAttempts.WithLabelValues("searxng", "first")); got != 1 {
		t.Fatalf(`searxng attempt="first" counter = %v, want 1`, got)
	}
}

// Observe must key the latency histogram on engine+status+reason, with
// reason="ok" on success, so slow-failure modes are separable from slow
// successes.
func TestObserveRecordsReasonOnHistogram(t *testing.T) {
	m := NewMetrics()
	m.Observe("crawl4ai", "mcp", "ok", "ok", time.Second)
	m.Observe("crawl4ai", "mcp", "error", "timeout", 2*time.Second)

	if got := histogramCount(t, m.RequestSecs.WithLabelValues("crawl4ai", "ok", "ok")); got != 1 {
		t.Fatalf("RequestSecs{ok,ok} count = %d, want 1", got)
	}
	if got := histogramCount(t, m.RequestSecs.WithLabelValues("crawl4ai", "error", "timeout")); got != 1 {
		t.Fatalf("RequestSecs{error,timeout} count = %d, want 1", got)
	}
}

// The new latency/quality collectors must register cleanly and record under
// their fixed label sets — infra recording rules reference these names.
func TestNewCollectorsObservable(t *testing.T) {
	m := NewMetrics()

	m.ObserveUpstream("crawl4ai", "crawl", "ok", 300*time.Millisecond)
	if got := histogramCount(t, m.UpstreamSecs.WithLabelValues("crawl4ai", "crawl", "ok")); got != 1 {
		t.Fatalf("UpstreamSecs count = %d, want 1", got)
	}

	m.ObserveLimiterWait("reddit", "acquired", 10*time.Millisecond)
	if got := histogramCount(t, m.LimiterWaitSecs.WithLabelValues("reddit", "acquired")); got != 1 {
		t.Fatalf("LimiterWaitSecs count = %d, want 1", got)
	}

	m.ObserveResponseChars("crawl4ai", 12345)
	if got := histogramCount(t, m.ResponseChars.WithLabelValues("crawl4ai")); got != 1 {
		t.Fatalf("ResponseChars count = %d, want 1", got)
	}

	m.ObserveFallback("github", "http_429")
	if got := counterValue(t, m.EngineFallbacks.WithLabelValues("github", "http_429")); got != 1 {
		t.Fatalf("EngineFallbacks count = %v, want 1", got)
	}

	m.ObserveUnresponsiveEngine("brave", "timeout")
	if got := counterValue(t, m.SearxngUnresponsive.WithLabelValues("brave", "timeout")); got != 1 {
		t.Fatalf("SearxngUnresponsive count = %v, want 1", got)
	}
}

// The degraded gauge is per scope: the two FallbackLimiters degrade and recover
// independently, so one coming back must not zero the other's signal.
func TestSetRatelimitDegradedIsPerScope(t *testing.T) {
	m := NewMetrics()
	m.SetRatelimitDegraded("domain", true)
	m.SetRatelimitDegraded("searxng", true)
	m.SetRatelimitDegraded("searxng", false)

	if got := gaugeValue(t, m.RatelimitDegraded.WithLabelValues("domain")); got != 1 {
		t.Fatalf("domain gauge = %v, want 1", got)
	}
	if got := gaugeValue(t, m.RatelimitDegraded.WithLabelValues("searxng")); got != 0 {
		t.Fatalf("searxng gauge = %v, want 0", got)
	}
}

// gaugeValue reads a GaugeVec child's current value.
func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var dm dto.Metric
	if err := g.Write(&dm); err != nil {
		t.Fatal(err)
	}
	return dm.GetGauge().GetValue()
}
