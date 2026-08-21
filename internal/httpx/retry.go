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
	"sync"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
)

// RetryConfig controls per-request retry behavior. Zero values use defaults.
type RetryConfig struct {
	MaxAttempts int           // total attempts including the first try
	BaseDelay   time.Duration // first backoff interval
	MaxDelay    time.Duration // cap on any single backoff
	// RetryableStatus, when non-nil, is consulted for a retryable status (429 or
	// 5xx) before another attempt is scheduled. Returning false stops retries and
	// surfaces the StatusError immediately — used to avoid re-driving an expensive
	// crawl for a non-transient block that an upstream reports as a 5xx.
	RetryableStatus func(status int, body string) bool
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
	// OnAttempt, when non-nil, is called once per HTTP attempt DoRetry makes,
	// with the upstream this client is labeled for (see WithUpstream). retry is
	// false for the first try and true for every retry, so a caller can count
	// retry volume — the wasted work #2's RetryableStatus veto cuts. Set on the
	// shared crawl client; nil elsewhere.
	OnAttempt func(upstream string, retry bool)
	// OnUpstream, when non-nil, is called once per HTTP attempt with the
	// round-trip duration — request start until the response body is fully read
	// (or closed), or until the transport error. status is "ok" for 2xx and
	// "error" otherwise (non-2xx status, transport error, timeout).
	OnUpstream func(upstream, op, status string, duration time.Duration)
	// OnRetryAfter, when non-nil, is called once per response that pairs a 429
	// or 503 with a parseable Retry-After, with the request URL and the delay
	// the upstream asked for. It fires whatever the retry loop then does —
	// including when a RetryableStatus veto or exhausted attempts end the
	// request — because the point is to keep the upstream's answer AFTER this
	// request dies: wired to a limiter, the next caller waits instead of walking
	// into the same wall. This client reports the fact; the cap and the policy
	// live at the wiring site. The retry loop's own capped backoff is unchanged.
	OnRetryAfter func(upstream, rawURL string, wait time.Duration)

	// upstream/op label every attempt this client makes in metrics. Set once per
	// adapter via WithUpstream so labels never derive from URLs.
	upstream, op string
}

// New returns a Client wrapping the given http.Client. If nil is passed, a
// 90s-timeout client is used.
func New(c *http.Client) *Client {
	if c == nil {
		c = &http.Client{Timeout: 90 * time.Second}
	}
	return &Client{HTTP: c}
}

// WithUpstream returns a shallow copy of c whose attempts are labeled with the
// given upstream/op pair (e.g. "crawl4ai"/"crawl"). The copy shares the
// underlying http.Client and hooks, so adapters declare their identity once at
// construction. Returns nil when c is nil (engines built without a client).
func (c *Client) WithUpstream(upstream, op string) *Client {
	if c == nil {
		return nil
	}
	cp := *c
	cp.upstream, cp.op = upstream, op
	return &cp
}

// DoRetry sends an HTTP request with exponential-backoff-with-jitter retries
// on transient failures. It retries on network errors, 429, and 5xx. 4xx
// other than 429 and context cancellation are not retried. A non-nil
// RetryConfig.RetryableStatus can additionally veto retrying a specific 429/5xx
// (e.g. a non-transient block), returning the StatusError immediately.
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

	// Metric labels for this client's attempts. "unknown" keeps the label set
	// bounded when a caller forgot WithUpstream (never derived from URLs).
	upstream, op := c.upstream, c.op
	if upstream == "" {
		upstream = "unknown"
	}
	if op == "" {
		op = "unknown"
	}

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

		if c.OnAttempt != nil {
			c.OnAttempt(upstream, attempt > 0)
		}
		start := time.Now()
		resp, err := c.HTTP.Do(req)
		if err != nil {
			if c.OnUpstream != nil {
				c.OnUpstream(upstream, op, "error", time.Since(start))
			}
			lastErr = err
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			delay = min(delay*2, cfg.MaxDelay)
			continue
		}
		if c.OnUpstream != nil {
			// The round-trip isn't over at header receipt: wrap the body so the
			// observation fires once it is fully read (or closed) — by the caller on
			// the return paths below, or by the drain in the retryable path.
			status := "error"
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				status = "ok"
			}
			observe := func() { c.OnUpstream(upstream, op, status, time.Since(start)) }
			resp.Body = &observedBody{ReadCloser: resp.Body, observe: observe}
		}

		// Non-retryable status — return immediately.
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return resp, nil
		}

		// Retryable: capture a bounded snippet of the body for later
		// classification, drain the rest so the connection can be reused, then
		// capture Retry-After and schedule the next attempt.
		retryAfter := resp.Header.Get("Retry-After")
		if secs, ok := parseRetryAfter(retryAfter); ok && c.OnRetryAfter != nil &&
			(resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable) {
			// Reported before the veto below, so a non-transient block still
			// hands its Retry-After on.
			c.OnRetryAfter(upstream, url, time.Duration(secs)*time.Second)
		}
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, statusBodySnippetLimit))
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		errBody := string(snippet)
		lastErr = &StatusError{StatusCode: resp.StatusCode, Body: errBody}

		// Let the caller veto a retry for a status it knows is non-transient (e.g.
		// an anti-bot block served as a 5xx). Without this the block is re-driven
		// MaxAttempts times at full crawl cost.
		if cfg.RetryableStatus != nil && !cfg.RetryableStatus(resp.StatusCode, errBody) {
			return nil, lastErr
		}

		if secs, ok := parseRetryAfter(retryAfter); ok {
			delay = min(time.Duration(secs)*time.Second, cfg.MaxDelay)
		} else {
			delay = min(delay*2, cfg.MaxDelay)
		}
	}

	return nil, lastErr
}

// observedBody fires observe exactly once, when the response body has been
// fully read (EOF) or is closed — whichever comes first — so the upstream
// round-trip metric covers body transfer, not just header receipt.
type observedBody struct {
	io.ReadCloser
	observe func()
	once    sync.Once
}

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err == io.EOF {
		b.once.Do(b.observe)
	}
	return n, err
}

// Close observes the round-trip (if EOF hasn't already) and closes the
// underlying body.
func (b *observedBody) Close() error {
	b.once.Do(b.observe)
	return b.ReadCloser.Close()
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

// statusBodySnippetLimit bounds how much of a retryable error-response body is
// captured into StatusError.Body for later classification (e.g. crawl4ai's
// anti-bot verdict). The marker sits at the top of a small JSON error, so 2KiB
// is ample and keeps the read off the hot path for large bodies.
const statusBodySnippetLimit = 2 << 10

// StatusError reports a non-2xx HTTP status returned by an upstream after
// retries were exhausted (429 / 5xx). It lets callers classify the failure by
// code via errors.As instead of parsing error text.
type StatusError struct {
	StatusCode int
	// Body is a bounded snippet of the upstream's error-response body, captured
	// when retries are exhausted. It lets a caller tell an anti-bot block served
	// as a 5xx (crawl4ai's detector) from a genuine upstream fault without
	// re-reading the response. Empty when the body was absent or unreadable.
	Body string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("upstream returned %d", e.StatusCode)
}

// ClassifyClientError translates an error returned by DoRetry into a typed
// domain.FetchError. A StatusError carries the upstream status after retries:
// a 429 is unambiguous and becomes KindHTTP429, but a 5xx is ambiguous — it can
// be a genuine upstream fault OR, on the browser/anti-bot paths, the block
// itself surfacing as a crawl4ai 5xx (see browser/crawl4ai) — so the caller's
// fallback decides (KindUpstreamError for a generic crawl, KindBotBlock for a
// Reddit navigation). A context deadline becomes KindTimeout; a caller
// cancellation (client abort) becomes KindCanceled; anything else becomes the
// fallback. Returns nil when err is nil.
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
	case errors.Is(err, context.Canceled):
		// The caller (e.g. Open WebUI) hung up before the fetch finished — a client
		// abort, not an omnifeed fault. Give it its own reason so alerts can exclude
		// it while still firing on the genuine deadline case below.
		return &domain.FetchError{Kind: domain.KindCanceled, Err: err}
	case errors.Is(err, context.DeadlineExceeded):
		return &domain.FetchError{Kind: domain.KindTimeout, Err: err}
	default:
		return &domain.FetchError{Kind: fallback, Err: err}
	}
}
