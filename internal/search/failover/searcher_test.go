package failover

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/observability"
)

// fakeSearcher answers every query with a fixed report and error, and counts
// how often it was asked.
type fakeSearcher struct {
	name   string
	report domain.SearchReport
	err    error
	calls  int
}

func (f *fakeSearcher) Name() string { return f.name }

func (f *fakeSearcher) Search(context.Context, string, domain.SearchOptions) (domain.SearchReport, error) {
	f.calls++
	return f.report, f.err
}

var errQuota = &domain.FetchError{Kind: domain.KindQuotaExhausted, Err: errors.New("pacing quota exhausted")}

func newComposite(primary, backup *fakeSearcher, m *observability.Metrics) *Searcher {
	return New(Config{Primary: primary, Backup: backup, Policy: Policy{MinResults: 3}, Metrics: m})
}

func TestSearch_HealthyPrimaryNeverAsksBackup(t *testing.T) {
	primary := &fakeSearcher{name: "searxng", report: domain.SearchReport{Results: hits("https://p/1", "https://p/2", "https://p/3")}}
	backup := &fakeSearcher{name: "tinyfish"}

	report, err := newComposite(primary, backup, nil).Search(context.Background(), "q", domain.SearchOptions{})
	if err != nil || len(report.Results) != 3 {
		t.Fatalf("Search = (%d results, %v), want the primary's 3", len(report.Results), err)
	}
	if backup.calls != 0 {
		t.Fatalf("backup asked %d times for a healthy primary answer", backup.calls)
	}
}

func TestSearch_PrimaryErrorIsServedByBackup(t *testing.T) {
	m := observability.NewMetrics()
	primary := &fakeSearcher{name: "searxng", err: errQuota}
	backup := &fakeSearcher{name: "tinyfish", report: domain.SearchReport{Results: hits("https://b/1")}}

	report, err := newComposite(primary, backup, m).Search(context.Background(), "q", domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v, want the backup's answer", err)
	}
	if got := urls(report.Results); !slices.Equal(got, []string{"https://b/1"}) {
		t.Fatalf("results = %v, want the backup's", got)
	}
	if got := counterValue(t, m.SearchFallbacks.WithLabelValues("searxng", "tinyfish", "error", "served")); got != 1 {
		t.Fatalf("fallbacks{trigger=error,outcome=served} = %v, want 1", got)
	}
}

func TestSearch_BackupFailureKeepsPrimaryError(t *testing.T) {
	m := observability.NewMetrics()
	primary := &fakeSearcher{name: "searxng", err: errQuota}
	backup := &fakeSearcher{name: "tinyfish", err: errors.New("tinyfish returned 402")}

	_, err := newComposite(primary, backup, m).Search(context.Background(), "q", domain.SearchOptions{})
	if !errors.Is(err, errQuota) {
		t.Fatalf("error = %v, want the primary's original error", err)
	}
	if got := observability.Reason(err); got != "quota_exhausted" {
		t.Fatalf("reason = %q, want quota_exhausted preserved", got)
	}
	if got := counterValue(t, m.SearchFallbacks.WithLabelValues("searxng", "tinyfish", "error", "failed")); got != 1 {
		t.Fatalf("fallbacks{outcome=failed} = %v, want 1", got)
	}
}

func TestSearch_DegradedPrimaryIsMergedWithBackup(t *testing.T) {
	primary := &fakeSearcher{name: "searxng", report: domain.SearchReport{
		Results:        hits("https://p/1", "https://p/2", "https://shared/x"),
		DegradedReason: "google cse: too_many_requests",
	}}
	backup := &fakeSearcher{name: "tinyfish", report: domain.SearchReport{Results: hits("https://b/1", "https://shared/x")}}

	report, err := newComposite(primary, backup, nil).Search(context.Background(), "q", domain.SearchOptions{Limit: 10})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	want := []string{"https://p/1", "https://b/1", "https://p/2", "https://shared/x"}
	if got := urls(report.Results); !slices.Equal(got, want) {
		t.Fatalf("results = %v, want %v", got, want)
	}
	if report.Degraded() {
		t.Fatalf("a topped-up answer must not stay degraded, got %q", report.DegradedReason)
	}
}

func TestSearch_DegradedPrimaryStandsWhenBackupFails(t *testing.T) {
	degraded := domain.SearchReport{Results: hits("https://p/1", "https://p/2", "https://p/3"), DegradedReason: "google cse: no results"}
	primary := &fakeSearcher{name: "searxng", report: degraded}
	backup := &fakeSearcher{name: "tinyfish", err: errors.New("timeout")}

	report, err := newComposite(primary, backup, nil).Search(context.Background(), "q", domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v, want the primary's results", err)
	}
	if !slices.Equal(urls(report.Results), urls(degraded.Results)) || !report.Degraded() {
		t.Fatalf("report = %+v, want the primary's degraded answer unchanged", report)
	}
}

func TestSearch_CanceledCallerIsNotFailedOver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	primary := &fakeSearcher{name: "searxng", err: context.Canceled}
	backup := &fakeSearcher{name: "tinyfish"}

	if _, err := newComposite(primary, backup, nil).Search(ctx, "q", domain.SearchOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if backup.calls != 0 {
		t.Fatalf("backup asked %d times for a caller that hung up", backup.calls)
	}
}

func TestName_NestsForLongerChains(t *testing.T) {
	inner := New(Config{Primary: &fakeSearcher{name: "tinyfish"}, Backup: &fakeSearcher{name: "brave"}})
	outer := New(Config{Primary: &fakeSearcher{name: "searxng"}, Backup: inner})
	if got := outer.Name(); got != "searxng+tinyfish+brave" {
		t.Fatalf("Name = %q, want searxng+tinyfish+brave", got)
	}
}

func counterValue(t *testing.T, c prometheus.Counter) float64 {
	t.Helper()
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		t.Fatalf("read counter: %v", err)
	}
	return m.GetCounter().GetValue()
}
