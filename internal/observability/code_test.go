package observability

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
)

// Every FailureKind has an explicit row in codeFor: a new kind must be mapped
// (and documented in docs/errors.md) deliberately, not inherit the default.
func TestClassify_EveryKindMapped(t *testing.T) {
	kinds := []domain.FailureKind{
		domain.KindCaptcha, domain.KindHTTP403, domain.KindHTTP429, domain.KindBotBlock,
		domain.KindThinContent, domain.KindTimeout, domain.KindCanceled, domain.KindUpstreamError,
		domain.KindUpstreamRejected, domain.KindQuotaExhausted, domain.KindBadResponse, domain.KindError,
	}
	for _, k := range kinds {
		if _, ok := codeFor[k]; !ok {
			t.Errorf("FailureKind %q has no error code", k)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Failure
	}{
		{"nil", nil, Failure{}},
		{"429 with retry-after", &domain.FetchError{Kind: domain.KindHTTP429, StatusCode: 429, RetryAfter: 1500 * time.Millisecond},
			Failure{Code: CodeRateLimited, Retryable: true, RetryAfter: 1500 * time.Millisecond, UpstreamStatus: 429}},
		{"captcha", &domain.FetchError{Kind: domain.KindCaptcha, StatusCode: 403}, Failure{Code: CodeCaptcha, UpstreamStatus: 403}},
		{"bare deadline", context.DeadlineExceeded, Failure{Code: CodeTimeout, Retryable: true}},
		{"plain error", errors.New("boom"), Failure{Code: CodeUpstreamError}},
		{"invalid request wins over kind", fmt.Errorf("url rejected: %w", &domain.InvalidRequestError{Err: errors.New("empty hostname")}),
			Failure{Code: CodeInvalidRequest}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestFailureFields(t *testing.T) {
	f := Failure{Code: CodeRateLimited, Retryable: true, RetryAfter: 1500 * time.Millisecond, UpstreamStatus: 429}
	got := f.Fields()
	if got["code"] != "rate_limited" || got["retryable"] != true || got["retry_after_s"] != 2 || got["upstream_status"] != 429 {
		t.Errorf("Fields() = %v", got)
	}
	bare := Failure{Code: CodeBlocked}.Fields()
	if _, has := bare["retry_after_s"]; has {
		t.Errorf("unknown retry-after must be omitted: %v", bare)
	}
	if _, has := bare["upstream_status"]; has {
		t.Errorf("unknown status must be omitted: %v", bare)
	}
}
