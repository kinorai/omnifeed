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
