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
	"time"

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

// RetryableStatus lets a caller veto retrying a non-transient 5xx (e.g. an
// anti-bot block crawl4ai serves as a 500). A vetoed status is attempted once;
// any other 5xx still retries to MaxAttempts.
func TestDoRetryRespectsRetryableStatus(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantAttempts int
	}{
		{"anti-bot block is not retried", `{"error":"Blocked by anti-bot protection"}`, 1},
		{"generic 5xx still retries", `{"error":"internal server error"}`, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var attempts int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			resp, err := New(nil).DoRetry(context.Background(), http.MethodGet, srv.URL, nil, nil,
				RetryConfig{
					MaxAttempts: 3,
					BaseDelay:   time.Microsecond, // keep backoff negligible
					RetryableStatus: func(status int, body string) bool {
						return status < 500 || !strings.Contains(body, "anti-bot protection")
					},
				})
			if resp != nil { // nil on this 5xx path, but satisfies the bodyclose linter
				_ = resp.Body.Close()
			}

			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("want *StatusError, got %T: %v", err, err)
			}
			if attempts != tc.wantAttempts {
				t.Fatalf("attempts = %d, want %d", attempts, tc.wantAttempts)
			}
		})
	}
}

// OnAttempt fires once per HTTP attempt — the first try plus every retry — so the
// metrics layer can count retry volume. A 200 is one attempt; a retried 5xx
// reports the first try and each retry.
func TestDoRetryReportsAttempts(t *testing.T) {
	t.Run("single attempt on success", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		var firsts, retries int
		c := New(nil)
		c.OnAttempt = func(_ string, retry bool) {
			if retry {
				retries++
			} else {
				firsts++
			}
		}
		resp, err := c.DoRetry(context.Background(), http.MethodGet, srv.URL, nil, nil, RetryConfig{})
		if err != nil {
			t.Fatalf("DoRetry() error = %v", err)
		}
		_ = resp.Body.Close()
		if firsts != 1 || retries != 0 {
			t.Fatalf("attempts: first=%d retry=%d, want 1/0", firsts, retries)
		}
	})

	t.Run("counts retries on 5xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		var firsts, retries int
		c := New(nil)
		c.OnAttempt = func(_ string, retry bool) {
			if retry {
				retries++
			} else {
				firsts++
			}
		}
		resp, err := c.DoRetry(context.Background(), http.MethodGet, srv.URL, nil, nil,
			RetryConfig{MaxAttempts: 3, BaseDelay: time.Microsecond})
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatal("DoRetry() error = nil, want a 5xx failure")
		}
		if firsts != 1 || retries != 2 {
			t.Fatalf("attempts: first=%d retry=%d, want 1/2", firsts, retries)
		}
	})
}

// OnUpstream fires once per HTTP attempt with the client's WithUpstream labels:
// a 200 records one "ok" observation once the caller drains/closes the body; a
// retried 5xx records one "error" observation per attempt (drained internally).
func TestDoRetryReportsUpstreamRoundTrips(t *testing.T) {
	type obs struct{ upstream, op, status string }

	t.Run("2xx observed ok after body read", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("body"))
		}))
		defer srv.Close()

		var got []obs
		c := New(nil).WithUpstream("crawl4ai", "crawl")
		c.OnUpstream = func(upstream, op, status string, _ time.Duration) {
			got = append(got, obs{upstream, op, status})
		}
		resp, err := c.DoRetry(context.Background(), http.MethodGet, srv.URL, nil, nil, RetryConfig{})
		if err != nil {
			t.Fatalf("DoRetry() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("observed before the body was read: %+v", got)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close() // Close after EOF must not double-observe
		if len(got) != 1 || got[0] != (obs{"crawl4ai", "crawl", "ok"}) {
			t.Fatalf("observations = %+v, want one {crawl4ai crawl ok}", got)
		}
	})

	t.Run("retried 5xx observed error per attempt", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		var got []obs
		c := New(nil).WithUpstream("searxng", "search")
		c.OnUpstream = func(upstream, op, status string, _ time.Duration) {
			got = append(got, obs{upstream, op, status})
		}
		resp, err := c.DoRetry(context.Background(), http.MethodGet, srv.URL, nil, nil,
			RetryConfig{MaxAttempts: 3, BaseDelay: time.Microsecond})
		if resp != nil {
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatal("DoRetry() error = nil, want a 5xx failure")
		}
		if len(got) != 3 {
			t.Fatalf("observations = %d, want 3 (one per attempt)", len(got))
		}
		for _, o := range got {
			if o != (obs{"searxng", "search", "error"}) {
				t.Fatalf("observation = %+v, want {searxng search error}", o)
			}
		}
	})

	t.Run("transport error observed error", func(t *testing.T) {
		var got []obs
		c := New(&http.Client{Timeout: 50 * time.Millisecond}).WithUpstream("github", "api")
		c.OnUpstream = func(upstream, op, status string, _ time.Duration) {
			got = append(got, obs{upstream, op, status})
		}
		// Closed server → connection refused on every attempt.
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()
		resp, err := c.DoRetry(context.Background(), http.MethodGet, srv.URL, nil, nil,
			RetryConfig{MaxAttempts: 2, BaseDelay: time.Microsecond})
		if resp != nil { // nil on this transport-error path, but satisfies the bodyclose linter
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatal("DoRetry() error = nil, want transport failure")
		}
		if len(got) != 2 {
			t.Fatalf("observations = %d, want 2", len(got))
		}
		for _, o := range got {
			if o != (obs{"github", "api", "error"}) {
				t.Fatalf("observation = %+v, want {github api error}", o)
			}
		}
	})

	t.Run("client without WithUpstream reports unknown labels", func(t *testing.T) {
		var got []obs
		c := New(nil)
		c.OnUpstream = func(upstream, op, status string, _ time.Duration) {
			got = append(got, obs{upstream, op, status})
		}
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		srv.Close()
		resp, err := c.DoRetry(context.Background(), http.MethodGet, srv.URL, nil, nil,
			RetryConfig{MaxAttempts: 1})
		if resp != nil { // nil on this transport-error path, but satisfies the bodyclose linter
			_ = resp.Body.Close()
		}
		if err == nil {
			t.Fatal("DoRetry() error = nil, want transport failure")
		}
		if len(got) != 1 || got[0] != (obs{"unknown", "unknown", "error"}) {
			t.Fatalf("observations = %+v, want one {unknown unknown error}", got)
		}
	})
}

// OnRetryAfter reports what an upstream asked for, once per response that
// actually asked: 429 and 503 with a parseable Retry-After, nothing else. The
// retry loop's own capped backoff is a separate thing and stays unchanged.
func TestDoRetryReportsRetryAfter(t *testing.T) {
	type call struct {
		upstream, rawURL string
		wait             time.Duration
	}

	tests := []struct {
		name        string
		status      int
		retryAfter  string
		maxAttempts int
		veto        func(status int, body string) bool
		wantCalls   int
		wantWait    time.Duration
	}{
		{name: "429 with Retry-After", status: http.StatusTooManyRequests, retryAfter: "120",
			maxAttempts: 1, wantCalls: 1, wantWait: 2 * time.Minute},
		{name: "503 with Retry-After", status: http.StatusServiceUnavailable, retryAfter: "30",
			maxAttempts: 1, wantCalls: 1, wantWait: 30 * time.Second},
		{name: "429 without the header", status: http.StatusTooManyRequests,
			maxAttempts: 1, wantCalls: 0},
		{name: "429 with an unparseable header", status: http.StatusTooManyRequests,
			retryAfter: "Wed, 21 Oct 2026 07:28:00 GMT", maxAttempts: 1, wantCalls: 0},
		{name: "500 with Retry-After is not a rate limit", status: http.StatusInternalServerError,
			retryAfter: "30", maxAttempts: 1, wantCalls: 0},
		// One call per RESPONSE, so a retried 429 reports each of them — and a
		// vetoed retry still reports the one it saw.
		{name: "once per response across retries", status: http.StatusTooManyRequests, retryAfter: "10",
			maxAttempts: 3, wantCalls: 3, wantWait: 10 * time.Second},
		{name: "reported even when the retry is vetoed", status: http.StatusTooManyRequests, retryAfter: "10",
			maxAttempts: 3, veto: func(int, string) bool { return false }, wantCalls: 1, wantWait: 10 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tt.retryAfter != "" {
					w.Header().Set("Retry-After", tt.retryAfter)
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			var calls []call
			c := New(nil).WithUpstream("hackernews", "api")
			c.OnRetryAfter = func(upstream, rawURL string, wait time.Duration) {
				calls = append(calls, call{upstream, rawURL, wait})
			}
			resp, err := c.DoRetry(context.Background(), http.MethodGet, srv.URL, nil, nil,
				RetryConfig{MaxAttempts: tt.maxAttempts, BaseDelay: time.Microsecond,
					RetryableStatus: tt.veto})
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				t.Fatalf("DoRetry() error = nil, want a %d failure", tt.status)
			}

			if len(calls) != tt.wantCalls {
				t.Fatalf("OnRetryAfter fired %d times, want %d (%v)", len(calls), tt.wantCalls, calls)
			}
			for _, got := range calls {
				if got.wait != tt.wantWait {
					t.Fatalf("wait = %v, want %v", got.wait, tt.wantWait)
				}
				if got.upstream != "hackernews" {
					t.Fatalf("upstream = %q, want hackernews", got.upstream)
				}
				if got.rawURL != srv.URL {
					t.Fatalf("rawURL = %q, want %q", got.rawURL, srv.URL)
				}
			}
		})
	}
}
