package failover

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/observability"
)

// fakeSearcher answers every query with a fixed report and error, and counts
// how often it was asked. With hang set it instead waits for its context to
// end, the way a stuck upstream does.
type fakeSearcher struct {
	name   string
	report domain.SearchReport
	err    error
	hang   bool
	calls  int
}

func (f *fakeSearcher) Name() string { return f.name }

func (f *fakeSearcher) Search(ctx context.Context, _ string, _ domain.SearchOptions) (domain.SearchReport, error) {
	f.calls++
	if f.hang {
		<-ctx.Done()
		return domain.SearchReport{}, ctx.Err()
	}
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

// hangUpSearcher is a backup during which the caller hangs up.
type hangUpSearcher struct{ cancel context.CancelFunc }

func (*hangUpSearcher) Name() string { return "tinyfish" }

func (h *hangUpSearcher) Search(ctx context.Context, _ string, _ domain.SearchOptions) (domain.SearchReport, error) {
	h.cancel()
	return domain.SearchReport{}, ctx.Err()
}

// A caller that hangs up while the backup runs is not a backup failure: it
// must not inflate outcome="failed", which an operator reads as TinyFish down.
func TestSearch_CallerHangUpDuringBackupIsNotAFailure(t *testing.T) {
	m := observability.NewMetrics()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	primary := &fakeSearcher{name: "searxng", err: errQuota}
	s := New(Config{Primary: primary, Backup: &hangUpSearcher{cancel: cancel}, Policy: Policy{MinResults: 3}, Metrics: m})

	if _, err := s.Search(ctx, "q", domain.SearchOptions{}); !errors.Is(err, errQuota) {
		t.Fatalf("error = %v, want the primary's original error", err)
	}
	if got := counterValue(t, m.SearchFallbacks.WithLabelValues("searxng", "tinyfish", "error", "canceled")); got != 1 {
		t.Fatalf("fallbacks{outcome=canceled} = %v, want 1", got)
	}
	if got := counterValue(t, m.SearchFallbacks.WithLabelValues("searxng", "tinyfish", "error", "failed")); got != 0 {
		t.Fatalf("fallbacks{outcome=failed} = %v, want 0", got)
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

// A backup that answers a failed primary with nothing must not turn the
// failure into an empty success: "retry in 12s" would read as "nothing exists".
func TestSearch_EmptyBackupKeepsPrimaryError(t *testing.T) {
	m := observability.NewMetrics()
	primary := &fakeSearcher{name: "searxng", err: errQuota}
	backup := &fakeSearcher{name: "tinyfish"}

	_, err := newComposite(primary, backup, m).Search(context.Background(), "q", domain.SearchOptions{})
	if !errors.Is(err, errQuota) {
		t.Fatalf("error = %v, want the primary's quota_exhausted", err)
	}
	if got := counterValue(t, m.SearchFallbacks.WithLabelValues("searxng", "tinyfish", "error", "empty")); got != 1 {
		t.Fatalf("fallbacks{outcome=empty} = %v, want 1", got)
	}
}

// Two empty answers are an honest zero-hit search: success, not an error.
func TestSearch_EmptyPrimaryAndBackupIsEmptySuccess(t *testing.T) {
	primary := &fakeSearcher{name: "searxng"}
	backup := &fakeSearcher{name: "tinyfish"}

	report, err := newComposite(primary, backup, nil).Search(context.Background(), "q", domain.SearchOptions{})
	if err != nil || len(report.Results) != 0 {
		t.Fatalf("Search = (%d results, %v), want an empty success", len(report.Results), err)
	}
}

// A hung primary is cut off at PrimaryBudget and the backup answers, instead
// of the caller waiting out the primary's whole timeout-and-retry budget.
func TestSearch_PrimaryBudgetHandsAHungPrimaryToTheBackup(t *testing.T) {
	primary := &fakeSearcher{name: "searxng", hang: true}
	backup := &fakeSearcher{name: "tinyfish", report: domain.SearchReport{Results: hits("https://b/1")}}
	s := New(Config{Primary: primary, Backup: backup, Policy: Policy{MinResults: 3}, PrimaryBudget: 20 * time.Millisecond})

	start := time.Now()
	report, err := s.Search(context.Background(), "q", domain.SearchOptions{})
	if err != nil || len(report.Results) != 1 {
		t.Fatalf("Search = (%d results, %v), want the backup's 1", len(report.Results), err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("took %s, want about the 20ms budget", elapsed)
	}
}

// A chain nests: the middle searcher answers when the first fails, and the
// last is never asked.
func TestSearch_ThreeLevelChain(t *testing.T) {
	first := &fakeSearcher{name: "searxng", err: errQuota}
	second := &fakeSearcher{name: "tinyfish", report: domain.SearchReport{Results: hits("https://t/1", "https://t/2", "https://t/3")}}
	third := &fakeSearcher{name: "brave"}
	chain := New(Config{Primary: first, Policy: Policy{MinResults: 3},
		Backup: New(Config{Primary: second, Backup: third, Policy: Policy{MinResults: 3}})})

	report, err := chain.Search(context.Background(), "q", domain.SearchOptions{})
	if err != nil || len(report.Results) != 3 {
		t.Fatalf("Search = (%d results, %v), want the middle searcher's 3", len(report.Results), err)
	}
	if third.calls != 0 {
		t.Fatalf("last searcher asked %d times behind a healthy middle one", third.calls)
	}
}
