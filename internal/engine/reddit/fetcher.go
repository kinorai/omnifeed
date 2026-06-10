package reddit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kinorai/search-crawl-reddit-proxy/internal/httpx"
)

// redditOrigin is the reddit.com host we navigate and fetch from. We use
// www (not old.reddit.com): Reddit's edge "network security" wall trips
// per-host on a risk score, and old.reddit gets blocked intermittently while
// www stays clear — and crawl4ai 0.8.7's anti-bot detector hard-errors the
// whole crawl when a navigation lands on the 403 block page.
const redditOrigin = "https://www.reddit.com"

// browserPageTimeoutMS bounds crawl4ai's page navigation. It is kept comfortably
// BELOW the shared HTTP client's timeout (SCRM_CRAWL4AI_TIMEOUT, default 90s) so
// the Go client doesn't cancel a still-running crawl and lose crawl4ai's precise
// "navigation blocked" diagnostics. If you raise this, raise SCRM_CRAWL4AI_TIMEOUT too.
const browserPageTimeoutMS = 60000

// Fetcher retrieves raw JSON from Reddit. Reddit's edge now hard-blocks
// non-browser HTTP clients (Go's net/http gets a 403 "network security" wall
// keyed on TLS/JA3 fingerprint), so we no longer hit Reddit directly. Instead
// every fetch is routed through the upstream crawl4ai instance: it drives a
// real headless Chromium to a reddit.com page (which clears the bot
// challenge), then runs a same-origin fetch() of the target JSON endpoint from
// inside that page and hands back the raw response text. The browser context
// is what passes the wall; the in-page fetch inherits it, so the JSON comes
// back exactly as a logged-out browser would see it — no auth, no cookies.
type Fetcher struct {
	client      *httpx.Client
	crawl4aiURL string
}

// NewFetcher constructs a Fetcher that drives crawl4ai's /crawl endpoint
// (crawl4aiURL == SCRM_CRAWL4AI_URL) to reach Reddit through a browser.
func NewFetcher(client *httpx.Client, crawl4aiURL string) *Fetcher {
	return &Fetcher{client: client, crawl4aiURL: crawl4aiURL}
}

// FetchThread retrieves a thread via the .json endpoint with a generous limit
// and depth, fetched from inside a real browser on the reddit.com origin. This
// call navigates the page, creating/warming the per-thread crawl4ai session
// that subsequent FetchMoreChildren calls reuse.
func (f *Fetcher) FetchThread(ctx context.Context, permalink string) ([]byte, error) {
	page := redditOrigin + permalink
	jsonURL := redditOrigin + strings.TrimSuffix(permalink, "/") + ".json?limit=500&depth=20&sort=top&raw_json=1"
	return f.browserFetch(ctx, page, getJS(jsonURL), threadSession(permalink), false)
}

// FetchMoreChildren expands collapsed reply branches via /api/morechildren.
// linkID must include the t3_ prefix; childIDs are bare IDs (no prefix). It
// reuses the thread's warmed browser session (js_only: no re-navigation).
func (f *Fetcher) FetchMoreChildren(ctx context.Context, linkID string, childIDs []string) ([]byte, error) {
	id36 := strings.TrimPrefix(linkID, kindPostPrefix)
	page := redditOrigin + "/comments/" + id36 + "/"

	form := url.Values{}
	form.Set("api_type", "json")
	form.Set("link_id", linkID)
	form.Set("children", strings.Join(childIDs, ","))
	form.Set("limit_children", "false")
	form.Set("sort", "top")
	form.Set("raw_json", "1")
	return f.browserFetch(ctx, page, postJS(redditOrigin+"/api/morechildren", form.Encode()), "carp-reddit-"+id36, true)
}

// ResolveShareURL resolves a Reddit share link (/r/{sub}/s/{code}) to its
// canonical /comments/ permalink: the browser follows the 301 redirect and we
// read the resulting location. Returns the full canonical URL (tracking query
// params and all — NormalizePermalink only looks at the path).
func (f *Fetcher) ResolveShareURL(ctx context.Context, shareURL string) (string, error) {
	resolved, err := f.browserExec(ctx, shareURL, "return location.href;", "", false)
	if err != nil {
		return "", err
	}
	if !strings.Contains(resolved, "/comments/") {
		return "", fmt.Errorf("share link did not resolve to a thread (got %q)", redactQuery(resolved))
	}
	return resolved, nil
}

// threadSession derives a stable crawl4ai session key for a thread from its
// permalink (/r/{sub}/comments/{id36}/...). FetchThread and all its
// FetchMoreChildren rounds share this key so they reuse ONE warmed browser
// context: persistent cookies + a single consistent fingerprint look far less
// bot-like to Reddit's wall than N cold sessions, and morechildren rounds skip
// re-navigating the (heavy) thread page. Returns "" if the id can't be found,
// disabling reuse for that call.
func threadSession(permalink string) string {
	parts := strings.Split(strings.Trim(permalink, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "comments" {
			return "carp-reddit-" + parts[i+1]
		}
	}
	return ""
}

// --- in-page fetch snippets ---

// jsLit encodes s as a JS string literal (a JSON string is a valid JS string).
// This is the injection guard: any quote/backslash smuggled through a permalink
// or child ID is escaped, so it can't break out of the literal in the JS we send.
func jsLit(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// getJS returns an async snippet that GETs u and returns {s:status, b:body}.
func getJS(u string) string {
	return `const r = await fetch(` + jsLit(u) + `, {headers: {"Accept": "application/json"}}); ` +
		`return JSON.stringify({s: r.status, b: await r.text()});`
}

// postJS returns an async snippet that form-POSTs body to u and returns {s,b}.
func postJS(u, body string) string {
	return `const r = await fetch(` + jsLit(u) + `, {method: "POST", ` +
		`headers: {"Accept": "application/json", "Content-Type": "application/x-www-form-urlencoded"}, ` +
		`body: ` + jsLit(body) + `}); ` +
		`return JSON.stringify({s: r.status, b: await r.text()});`
}

// --- crawl4ai /crawl wire types (browser-fetch path) ---

type c4aTypedConfig struct {
	Type   string                 `json:"type"`
	Params map[string]interface{} `json:"params"`
}

type c4aRequest struct {
	URLs          []string       `json:"urls"`
	BrowserConfig c4aTypedConfig `json:"browser_config"`
	CrawlerConfig c4aTypedConfig `json:"crawler_config"`
}

type c4aResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Results []struct {
		Success           bool   `json:"success"`
		StatusCode        int    `json:"status_code"`
		ErrorMessage      string `json:"error_message"`
		JSExecutionResult struct {
			Results []json.RawMessage `json:"results"`
		} `json:"js_execution_result"`
	} `json:"results"`
}

// fetchEnvelope is what the in-page snippet returns: the HTTP status of the
// Reddit fetch and its raw body — so we can tell a Reddit-side block (403)
// apart from a crawl4ai/navigation failure.
type fetchEnvelope struct {
	S int    `json:"s"`
	B string `json:"b"`
}

// browserExec navigates navURL via crawl4ai (or reuses a warmed session when
// jsOnly) and runs js, returning the first JS return value as a string. Shared
// by browserFetch (which expects a {s,b} envelope) and ResolveShareURL (which
// expects a URL).
//
// Which crawl4ai knobs, and why:
//   - enable_stealth (BrowserConfig) + override_navigator (CrawlerRunConfig)
//     are fingerprint-level evasions evaluated at page LOAD — this is what
//     clears Reddit's wall, and both are kept.
//   - We deliberately OMIT `magic` and `simulate_user`: they mostly drive
//     post-load behavioral simulation (scroll/click) that an in-page fetch()
//     never needs — pure latency here. If Reddit's wall starts challenging
//     this path, re-add them (cheap insurance); they were verified working.
//
// sessionID (when non-empty) reuses one warmed browser context; jsOnly skips
// re-navigation and runs js on that context.
func (f *Fetcher) browserExec(ctx context.Context, navURL, js, sessionID string, jsOnly bool) (string, error) {
	if f.crawl4aiURL == "" {
		return "", fmt.Errorf("crawl4ai endpoint not configured (set SCRM_CRAWL4AI_URL)")
	}

	crawler := map[string]interface{}{
		"cache_mode":         "BYPASS",
		"override_navigator": true,
		"page_timeout":       browserPageTimeoutMS,
		"js_code":            []string{js},
	}
	if sessionID != "" {
		crawler["session_id"] = sessionID
	}
	if jsOnly {
		crawler["js_only"] = true
	}

	reqBody, err := json.Marshal(c4aRequest{
		URLs: []string{navURL},
		BrowserConfig: c4aTypedConfig{Type: "BrowserConfig", Params: map[string]interface{}{
			"headless":       true,
			"enable_stealth": true,
		}},
		CrawlerConfig: c4aTypedConfig{Type: "CrawlerRunConfig", Params: crawler},
	})
	if err != nil {
		return "", fmt.Errorf("marshal crawl4ai request: %w", err)
	}

	// MaxAttempts: 1 — no retry. A Reddit block surfaces as a crawl4ai 500
	// (anti-bot detector) which is NOT transient, so retrying just re-drives an
	// expensive browser crawl for the same failure; genuine transient crawl4ai
	// 5xx are rare and the caller/engine degrades gracefully.
	resp, err := f.client.DoRetry(ctx, http.MethodPost, f.crawl4aiURL, reqBody,
		map[string]string{"Content-Type": "application/json"}, httpx.RetryConfig{MaxAttempts: 1})
	if err != nil {
		return "", fmt.Errorf("crawl4ai request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20MB cap
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		// 3xx/4xx from crawl4ai (DoRetry passes these through). 5xx/429 surface
		// above via the DoRetry error path — the anti-bot block on a 403 nav
		// manifests there as a 500.
		return "", fmt.Errorf("crawl4ai returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var cr c4aResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("decode crawl4ai response: %w", err)
	}
	if !cr.Success || len(cr.Results) == 0 {
		msg := cr.Error
		if msg == "" && len(cr.Results) > 0 {
			msg = cr.Results[0].ErrorMessage
		}
		return "", fmt.Errorf("crawl4ai fetch failed (reddit may have blocked the nav): %s", truncate(msg, 200))
	}
	res0 := cr.Results[0]
	if !res0.Success {
		msg := res0.ErrorMessage
		if msg == "" {
			msg = cr.Error
		}
		return "", fmt.Errorf("crawl4ai result failed (reddit may have blocked the nav): %s", truncate(msg, 200))
	}
	jsResults := res0.JSExecutionResult.Results
	if len(jsResults) == 0 {
		return "", fmt.Errorf("crawl4ai returned no js result (navigation blocked?)")
	}

	// jsResults[0] is the JSON-encoded string our snippet returned; unwrap it.
	var out string
	if err := json.Unmarshal(jsResults[0], &out); err != nil {
		return "", fmt.Errorf("unwrap js result: %w", err)
	}
	return out, nil
}

// browserFetch runs js (a getJS/postJS snippet that returns a {s,b} envelope)
// and returns the fetched Reddit body, distinguishing a Reddit-side block (the
// envelope's non-200 status) from a crawl4ai/navigation failure.
func (f *Fetcher) browserFetch(ctx context.Context, navURL, js, sessionID string, jsOnly bool) ([]byte, error) {
	envStr, err := f.browserExec(ctx, navURL, js, sessionID, jsOnly)
	if err != nil {
		return nil, err
	}
	var env fetchEnvelope
	if err := json.Unmarshal([]byte(envStr), &env); err != nil {
		return nil, fmt.Errorf("decode fetch envelope: %w", err)
	}
	if env.S != http.StatusOK {
		return nil, fmt.Errorf("reddit returned %d via browser: %s", env.S, truncate(env.B, 200))
	}
	if !json.Valid([]byte(env.B)) {
		return nil, fmt.Errorf("reddit response not JSON (likely bot-blocked): %s", truncate(env.B, 200))
	}
	return []byte(env.B), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// redactQuery strips the query string from a URL before logging it: Reddit's
// share-link redirect appends a transient anti-bot token (js_challenge/token)
// we don't want in error logs. Scheme+host+path are enough for diagnosis.
func redactQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	return truncate(u, 200)
}
