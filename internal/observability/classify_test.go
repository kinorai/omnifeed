package observability

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
)

func TestReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "ok"},
		{"captcha", &domain.FetchError{Kind: domain.KindCaptcha, Marker: "just a moment..."}, "captcha"},
		{"http_403", &domain.FetchError{Kind: domain.KindHTTP403, StatusCode: 403}, "http_403"},
		{"http_429", &domain.FetchError{Kind: domain.KindHTTP429, StatusCode: 429}, "http_429"},
		{"bot_block", &domain.FetchError{Kind: domain.KindBotBlock}, "bot_block"},
		{"upstream", &domain.FetchError{Kind: domain.KindUpstreamError, StatusCode: 500}, "upstream_error"},
		{"bad_response", &domain.FetchError{Kind: domain.KindBadResponse}, "bad_response"},
		// errors.As must see through wrapping done by the reddit engine.
		{"wrapped", fmt.Errorf("fetch thread: %w", &domain.FetchError{Kind: domain.KindHTTP403}), "http_403"},
		// Context deadlines that never became a FetchError still classify as timeout.
		{"timeout_sentinel", fmt.Errorf("crawl4ai request: %w", context.DeadlineExceeded), "timeout"},
		{"plain", errors.New("normalize url: bad permalink"), "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Reason(c.err); got != c.want {
				t.Fatalf("Reason(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}
