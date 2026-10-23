package failover

import (
	"context"
	"errors"
	"testing"

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

	cases := []struct {
		name   string
		ctx    context.Context
		min    int
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
			got := Policy{MinResults: tc.min}.Decide(ctx, tc.report, tc.err)
			if got != tc.want {
				t.Fatalf("Decide = %+v, want %+v", got, tc.want)
			}
		})
	}
}
