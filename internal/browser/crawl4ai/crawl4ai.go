// Package crawl4ai implements the browser.Browser port over crawl4ai's
// /execute_js endpoint: it drives a real headless Chromium to a page and runs
// caller-supplied JavaScript from inside it. This is the Reddit engine's
// browser backend.
//
// crawl4ai has no session reuse on /execute_js, so a Session here does not hold a
// live page: Navigate records the target URL and every Eval re-navigates to it
// before running its script. Semantically each Eval is one fresh browser
// navigation — the same behaviour omnifeed's Reddit engine has always had.
//
// Why /execute_js (not /crawl): crawl4ai 0.9.x rejects caller-supplied js_code on
// /crawl "from an untrusted request". /execute_js is the sanctioned endpoint that
// runs caller JS — it builds the crawler config server-side. It must be enabled
// on the crawl4ai side (CRAWL4AI_EXECUTE_JS_ENABLED=true) and, once crawl4ai has
// a token, requires it (sent as `Authorization: Bearer <token>`).
package crawl4ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/antibot"
	"github.com/kinorai/omnifeed/internal/browser"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// Browser drives crawl4ai's /execute_js endpoint.
type Browser struct {
	client    *httpx.Client
	execJSURL string
	token     string
}

// New constructs a Browser. crawl4aiURL is OMNIFEED_CRAWL4AI_URL (the /crawl
// URL); the sibling /execute_js URL is derived from it. token, when set, is sent
// as `Authorization: Bearer <token>` (crawl4ai's CRAWL4AI_API_TOKEN).
func New(client *httpx.Client, crawl4aiURL, token string) *Browser {
	return &Browser{
		client:    client.WithUpstream("crawl4ai", "execute_js"),
		execJSURL: execJSEndpoint(crawl4aiURL),
		token:     token,
	}
}

// Name identifies the backend.
func (*Browser) Name() string { return "crawl4ai" }

// Open returns a stateless session: it holds only the URL recorded by Navigate,
// since /execute_js re-navigates on every call.
func (b *Browser) Open(context.Context) (browser.Session, error) {
	return &session{b: b}, nil
}

// execJSEndpoint derives crawl4ai's /execute_js URL from the configured /crawl
// URL (OMNIFEED_CRAWL4AI_URL points at /crawl; /execute_js is its sibling).
// The /crawl match is anchored to the URL's path: a bare-host URL like
// http://crawl4ai.svc:11235 must not match the "/crawl" inside its hostname.
func execJSEndpoint(crawlURL string) string {
	if crawlURL == "" {
		return ""
	}
	u, err := url.Parse(crawlURL)
	if err != nil {
		return strings.TrimRight(crawlURL, "/") + "/execute_js"
	}
	if i := strings.LastIndex(u.Path, "/crawl"); i >= 0 {
		u.Path = u.Path[:i] + "/execute_js"
	} else {
		u.Path = strings.TrimRight(u.Path, "/") + "/execute_js"
	}
	return u.String()
}

// session records the last Navigate target and replays it on every Eval.
type session struct {
	b      *Browser
	navURL string
}

// Navigate records the target URL; the actual navigation happens inside Eval,
// which crawl4ai performs atomically with the script run.
func (s *session) Navigate(_ context.Context, rawURL string) error {
	s.navURL = rawURL
	return nil
}

// Close is a no-op: /execute_js holds no server-side session to tear down.
func (*session) Close(context.Context) error { return nil }

// Eval navigates the recorded URL via /execute_js and runs js (an async function
// body), returning the value js returned as a string. crawl4ai wraps each script
// in an async function server-side, so `await` and `return` in js work as-is.
func (s *session) Eval(ctx context.Context, js string) (string, error) {
	if s.b.execJSURL == "" {
		return "", fmt.Errorf("crawl4ai endpoint not configured (set OMNIFEED_CRAWL4AI_URL)")
	}
	if s.navURL == "" {
		return "", fmt.Errorf("crawl4ai: Eval before Navigate")
	}

	reqBody, err := json.Marshal(execJSRequest{URL: s.navURL, Scripts: []string{js}})
	if err != nil {
		return "", fmt.Errorf("marshal crawl4ai request: %w", err)
	}

	// A block surfaces as a crawl4ai 5xx whose body names the anti-bot verdict;
	// don't retry it (re-driving the browser crawl just pays the cost again).
	// antibot.RetryableStatus vetoes exactly that while still retrying a genuine
	// transient crawl4ai 5xx — the same predicate the generic engine uses.
	headers := map[string]string{"Content-Type": "application/json"}
	if s.b.token != "" {
		headers["Authorization"] = "Bearer " + s.b.token
	}
	// MaxAttempts 2 (matching the generic engine): 0.9.2+ scrubs 500 bodies, so
	// one retry covers transient faults while a deterministic block costs at
	// most 2× one navigation.
	resp, err := s.b.client.DoRetry(ctx, http.MethodPost, s.b.execJSURL, reqBody, headers,
		httpx.RetryConfig{MaxAttempts: 2, RetryableStatus: antibot.RetryableStatus})
	if err != nil {
		// A nav that trips crawl4ai's anti-bot detector surfaces here as a 5xx
		// (DoRetry error path); classify unmatched client errors as a block.
		return "", httpx.ClassifyClientError(err, domain.KindBotBlock)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20MB cap
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		// 3xx/4xx from crawl4ai (DoRetry passes these through).
		return "", &domain.FetchError{
			Kind:       domain.KindForStatus(resp.StatusCode),
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("crawl4ai returned %d: %s", resp.StatusCode, truncate(string(body), 200)),
		}
	}

	var cr execJSResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("decode crawl4ai response: %w", err)}
	}
	if !cr.Success {
		return "", &domain.FetchError{Kind: domain.KindBotBlock, Err: fmt.Errorf("crawl4ai fetch failed (reddit may have blocked the nav): %s", truncate(cr.ErrorMessage, 200))}
	}
	if len(cr.JSExecutionResult.Results) == 0 {
		return "", &domain.FetchError{Kind: domain.KindBotBlock, Err: fmt.Errorf("crawl4ai returned no js result (navigation blocked?)")}
	}

	// results[0] is the JSON-encoded string our snippet returned; unwrap it.
	var out string
	if err := json.Unmarshal(cr.JSExecutionResult.Results[0], &out); err != nil {
		return "", &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("unwrap js result: %w", err)}
	}
	return out, nil
}

// --- crawl4ai /execute_js wire types ---

type execJSRequest struct {
	URL     string   `json:"url"`
	Scripts []string `json:"scripts"`
}

// execJSResponse is crawl4ai's /execute_js response: the first CrawlResult, with
// js_execution_result.results[0] holding the string our snippet returned.
type execJSResponse struct {
	Success           bool   `json:"success"`
	StatusCode        int    `json:"status_code"`
	ErrorMessage      string `json:"error_message"`
	JSExecutionResult struct {
		Results []json.RawMessage `json:"results"`
	} `json:"js_execution_result"`
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) { // back up to a rune boundary so we don't split a multibyte char
		n--
	}
	return s[:n] + "..."
}
