package observability

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
)

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

// ObserveAttempt must record under the attempt label so retry volume is visible
// (and so #2's drop in re-drives is measurable, not just reconstructed from logs).
func TestObserveAttemptCounts(t *testing.T) {
	m := NewMetrics()
	m.ObserveAttempt(false)
	m.ObserveAttempt(true)
	m.ObserveAttempt(true)

	var dm dto.Metric
	if err := m.RequestAttempts.WithLabelValues("retry").Write(&dm); err != nil {
		t.Fatal(err)
	}
	if got := dm.GetCounter().GetValue(); got != 2 {
		t.Fatalf("attempt=\"retry\" counter = %v, want 2", got)
	}
}
