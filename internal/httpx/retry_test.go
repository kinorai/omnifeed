package httpx

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
)

func TestClassifyClientError(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		fallback domain.FailureKind
		want     domain.FailureKind // "" means expect a nil *FetchError
	}{
		{"nil", nil, domain.KindUpstreamError, ""},
		// 429 is unambiguous rate limiting regardless of the caller's fallback.
		{"429 reddit", &StatusError{StatusCode: 429}, domain.KindBotBlock, domain.KindHTTP429},
		{"429 generic", &StatusError{StatusCode: 429}, domain.KindUpstreamError, domain.KindHTTP429},
		// A 5xx after retries is ambiguous: the caller's fallback classifies it.
		// On the Reddit path the anti-bot block surfaces as a crawl4ai 500, so it
		// MUST become bot_block — this is what the OmnifeedRedditBlocked alert keys on.
		{"reddit 500 is a block", &StatusError{StatusCode: 500}, domain.KindBotBlock, domain.KindBotBlock},
		{"reddit 503 is a block", &StatusError{StatusCode: 503}, domain.KindBotBlock, domain.KindBotBlock},
		// The generic crawl path wants 5xx → upstream_error (its fallback).
		{"generic 500 is upstream", &StatusError{StatusCode: 500}, domain.KindUpstreamError, domain.KindUpstreamError},
		// errors.As must see the StatusError through a wrap.
		{"wrapped 502 is a block", fmt.Errorf("crawl4ai request: %w", &StatusError{StatusCode: 502}), domain.KindBotBlock, domain.KindBotBlock},
		{"deadline is timeout", context.DeadlineExceeded, domain.KindBotBlock, domain.KindTimeout},
		{"canceled is timeout", context.Canceled, domain.KindUpstreamError, domain.KindTimeout},
		{"network error falls back", errors.New("dial tcp: connection refused"), domain.KindBotBlock, domain.KindBotBlock},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := ClassifyClientError(tc.err, tc.fallback)
			if tc.want == "" {
				if fe != nil {
					t.Fatalf("ClassifyClientError(nil) = %v, want nil", fe)
				}
				return
			}
			if fe == nil {
				t.Fatalf("ClassifyClientError(%v) = nil, want kind %q", tc.err, tc.want)
			}
			if fe.Kind != tc.want {
				t.Fatalf("Kind = %q, want %q", fe.Kind, tc.want)
			}
		})
	}
}
