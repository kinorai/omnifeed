package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		{"canceled is its own reason", context.Canceled, domain.KindUpstreamError, domain.KindCanceled},
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
				return // unreachable, but staticcheck (SA5011) doesn't model t.Fatalf as terminating
			}
			if fe.Kind != tc.want {
				t.Fatalf("Kind = %q, want %q", fe.Kind, tc.want)
			}
		})
	}
}

// On retry exhaustion the upstream's error body must survive on StatusError.Body
// so callers can classify it (e.g. crawl4ai's anti-bot 5xx → bot_block).
func TestDoRetryCapturesStatusBody(t *testing.T) {
	const errBody = `{"error":"Blocked by anti-bot protection: Structural: minimal_text on small page"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, errBody)
	}))
	defer srv.Close()

	// MaxAttempts: 1 keeps the test fast (no backoff); one 5xx is enough to
	// produce the StatusError whose Body downstream classification reads.
	resp, err := New(nil).DoRetry(context.Background(), http.MethodGet, srv.URL, nil, nil,
		RetryConfig{MaxAttempts: 1})
	if resp != nil { // nil on this 5xx path, but satisfies the bodyclose linter
		_ = resp.Body.Close()
	}

	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("want *StatusError, got %T: %v", err, err)
	}
	if se.StatusCode != http.StatusInternalServerError {
		t.Fatalf("StatusCode = %d, want 500", se.StatusCode)
	}
	if !strings.Contains(se.Body, "anti-bot protection") {
		t.Fatalf("Body = %q, want it to contain the upstream error text", se.Body)
	}
}
