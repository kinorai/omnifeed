// Package httpx provides HTTP utilities shared across engines: a retrying
// HTTP client wrapper, per-domain rate limiter, and SSRF guards.
package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
)

// RetryConfig controls per-request retry behavior. Zero values use defaults.
type RetryConfig struct {
	MaxAttempts int           // total attempts including the first try
	BaseDelay   time.Duration // first backoff interval
	MaxDelay    time.Duration // cap on any single backoff
}

func (c *RetryConfig) defaults() {
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 3
	}
	if c.BaseDelay == 0 {
		c.BaseDelay = 500 * time.Millisecond
	}
	if c.MaxDelay == 0 {
		c.MaxDelay = 4 * time.Second
	}
}

// Client wraps http.Client with retry-on-429/5xx and Retry-After honoring.
type Client struct {
	HTTP *http.Client
}

// New returns a Client wrapping the given http.Client. If nil is passed, a
// 90s-timeout client is used.
func New(c *http.Client) *Client {
	if c == nil {
		c = &http.Client{Timeout: 90 * time.Second}
	}
	return &Client{HTTP: c}
}

// DoRetry sends an HTTP request with exponential-backoff-with-jitter retries
// on transient failures. It retries on network errors, 429, and 5xx. 4xx
// other than 429 and context cancellation are not retried.
//
// Body is passed as a byte slice (or nil) so the helper can rebuild the
// request on each attempt — http.Request bodies are single-use streams.
//
// Honors Retry-After when the server provides it (capped at MaxDelay).
// Caller is responsible for closing the returned response body.
func (c *Client) DoRetry(
	ctx context.Context,
	method, url string,
	body []byte,
	headers map[string]string,
	cfg RetryConfig,
) (*http.Response, error) {
	cfg.defaults()

	delay := cfg.BaseDelay
	var lastErr error

	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if attempt > 0 {
			// Full jitter: random delay in [0, current backoff).
			// Better than fixed-multiplier on burst load (no thundering herd).
			jittered := time.Duration(rand.Int63n(int64(delay) + 1))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(jittered):
			}
		}

		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			delay = nextBackoff(delay, cfg.MaxDelay)
			continue
		}

		// Non-retryable status — return immediately.
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		// Retryable: drain body so the connection can be reused, capture
		// Retry-After, schedule the next attempt.
		retryAfter := resp.Header.Get("Retry-After")
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		lastErr = &StatusError{StatusCode: resp.StatusCode}

		if secs, ok := parseRetryAfter(retryAfter); ok {
			d := time.Duration(secs) * time.Second
			if d > cfg.MaxDelay {
				d = cfg.MaxDelay
			}
			delay = d
		} else {
			delay = nextBackoff(delay, cfg.MaxDelay)
		}
	}

	return nil, lastErr
}

func nextBackoff(d, max time.Duration) time.Duration {
	d *= 2
	if d > max {
		return max
	}
	return d
}

// parseRetryAfter parses the integer-seconds form of Retry-After.
// HTTP-date form is intentionally not supported (Reddit and crawl4ai both use
// seconds, and parsing dates here would invite clock-skew bugs).
func parseRetryAfter(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	secs, err := strconv.Atoi(s)
	if err != nil || secs < 0 {
		return 0, false
	}
	return secs, true
}

// StatusError reports a non-2xx HTTP status returned by an upstream after
// retries were exhausted (429 / 5xx). It lets callers classify the failure by
// code via errors.As instead of parsing error text.
type StatusError struct {
	StatusCode int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream returned %d", e.StatusCode)
}

// ClassifyClientError translates an error returned by DoRetry into a typed
// domain.FetchError. A StatusError carries the upstream status after retries:
// a 429 is unambiguous and becomes KindHTTP429, but a 5xx is ambiguous — it can
// be a genuine upstream fault OR, on the browser/anti-bot paths, the block
// itself surfacing as a crawl4ai 5xx (see reddit.browserExec) — so the caller's
// fallback decides (KindUpstreamError for a generic crawl, KindBotBlock for a
// Reddit navigation). A context deadline/cancellation becomes KindTimeout, and
// anything else becomes the fallback. Returns nil when err is nil.
func ClassifyClientError(err error, fallback domain.FailureKind) *domain.FetchError {
	if err == nil {
		return nil
	}
	var se *StatusError
	switch {
	case errors.As(err, &se):
		// A 5xx after retry exhaustion is ambiguous (infra fault vs. an anti-bot
		// block served as a 5xx), so let the caller's fallback classify it; a 429
		// is unambiguous rate limiting.
		kind := domain.KindForStatus(se.StatusCode)
		if se.StatusCode >= 500 {
			kind = fallback
		}
		return &domain.FetchError{Kind: kind, StatusCode: se.StatusCode, Err: err}
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return &domain.FetchError{Kind: domain.KindTimeout, Err: err}
	default:
		return &domain.FetchError{Kind: fallback, Err: err}
	}
}
