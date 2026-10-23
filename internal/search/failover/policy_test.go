package failover

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
)

func results(n int) []domain.SearchResult {
	out := make([]domain.SearchResult, n)
	for i := range out {
		out[i] = domain.SearchResult{URL: "https://example.com/" + string(rune('a'+i))}
	}
	return out
}

func TestPolicyDecide(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancelExpired()

	cases := []struct {
		name   string
		ctx    context.Context
		min    int
		limit  int
		report domain.SearchReport
		err    error
		want   Decision
	}{
		{
			name:   "healthy answer is kept",
			min:    3,
			report: domain.SearchReport{Results: results(10)},
			want:   Decision{Action: Keep},
		},
		{
			name: "error is replaced",
			min:  3,
			err:  errors.New("quota_exhausted"),
			want: Decision{Action: Replace, Trigger: TriggerError},
		},
		{
			name: "empty success is replaced",
			min:  3,
			want: Decision{Action: Replace, Trigger: TriggerEmpty},
		},
		{
			name:   "thin success is merged",
			min:    3,
			report: domain.SearchReport{Results: results(2)},
			want:   Decision{Action: Merge, Trigger: TriggerThin},
		},
		{
			name:   "thin trigger off at MinResults 0",
			report: domain.SearchReport{Results: results(1)},
			want:   Decision{Action: Keep},
		},
		{
			name:   "degraded success is merged",
			min:    3,
			report: domain.SearchReport{Results: results(10), DegradedReason: "google cse: too_many_requests"},
			want:   Decision{Action: Merge, Trigger: TriggerDegraded},
		},
		{
			name:   "a limit below MinResults is fully answered at the limit",
			min:    3,
			limit:  2,
			report: domain.SearchReport{Results: results(2)},
			want:   Decision{Action: Keep},
		},
		{
			name:   "short of a limit below MinResults is still thin",
			min:    3,
			limit:  2,
			report: domain.SearchReport{Results: results(1)},
			want:   Decision{Action: Merge, Trigger: TriggerThin},
		},
		{
			name:   "a limit above MinResults does not raise the bar",
			min:    3,
			limit:  10,
			report: domain.SearchReport{Results: results(3)},
			want:   Decision{Action: Keep},
		},
		{
			name: "invalid query is never failed over",
			min:  3,
			err:  fmt.Errorf("%w: query is empty", domain.ErrInvalidQuery),
			want: Decision{Action: Keep},
		},
		{
			name: "expired caller deadline is never failed over",
			ctx:  expired,
			min:  3,
			err:  context.DeadlineExceeded,
			want: Decision{Action: Keep},
		},
		{
			name: "canceled caller is never failed over",
			ctx:  canceled,
			min:  3,
			err:  context.Canceled,
			want: Decision{Action: Keep},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			got := Policy{MinResults: tc.min}.Decide(ctx, tc.limit, tc.report, tc.err)
			if got != tc.want {
				t.Fatalf("Decide = %+v, want %+v", got, tc.want)
			}
		})
	}
}
