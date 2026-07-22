package reddit

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
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// redditOrigin is the reddit.com host we navigate and fetch from. We use
// www (not old.reddit.com): Reddit's edge "network security" wall trips
// per-host on a risk score, and old.reddit gets blocked intermittently while
// www stays clear.
const redditOrigin = "https://www.reddit.com"

// Fetcher retrieves raw JSON from Reddit. Reddit's edge hard-blocks non-browser
// HTTP clients (Go's net/http gets a 403 "network security" wall keyed on the
// TLS/JA3 fingerprint), so we never hit Reddit directly. Instead every fetch is
// routed through crawl4ai's POST /execute_js endpoint: it drives a real headless
// Chromium to a reddit.com page (which clears the bot challenge), then runs a
// same-origin fetch() of the target JSON endpoint from inside that page and
// hands back the raw response text. The browser context passes the wall; the
// in-page fetch inherits it, so the JSON comes back exactly as a logged-out
// browser would see it — no auth, no cookies.
//
// Why /execute_js (not /crawl): crawl4ai 0.9.x rejects caller-supplied js_code on
// /crawl "from an untrusted request". /execute_js is the sanctioned endpoint that
// runs caller JS — it builds the crawler config server-side. It must be enabled
// on the crawl4ai side (CRAWL4AI_EXECUTE_JS_ENABLED=true) and, once crawl4ai has
// a token, requires it (sent via OMNIFEED_CRAWL4AI_TOKEN). There is no session
// reuse on /execute_js, so each call is a fresh navigation.
type Fetcher struct {
	client    *httpx.Client
	execJSURL string
	token     string
}

// NewFetcher constructs a Fetcher that drives crawl4ai's /execute_js endpoint to
// reach Reddit through a browser. crawl4aiURL is OMNIFEED_CRAWL4AI_URL (the
// /crawl URL); the sibling /execute_js URL is derived from it. token, when set,
// is sent as `Authorization: Bearer <token>` (crawl4ai's CRAWL4AI_API_TOKEN).
func NewFetcher(client *httpx.Client, crawl4aiURL, token string) *Fetcher {
	return &Fetcher{client: client, execJSURL: execJSEndpoint(crawl4aiURL), token: token}
}

// execJSEndpoint derives crawl4ai's /execute_js URL from the configured /crawl
// URL (OMNIFEED_CRAWL4AI_URL points at /crawl; /execute_js is its sibling).
func execJSEndpoint(crawlURL string) string {
	if crawlURL == "" {
		return ""
	}
	if i := strings.LastIndex(crawlURL, "/crawl"); i >= 0 {
		return crawlURL[:i] + "/execute_js"
	}
	return strings.TrimRight(crawlURL, "/") + "/execute_js"
}

// FetchThread retrieves a thread via the .json endpoint, fetched from inside a
// real browser on the reddit.com origin. limit/depth/sort map directly onto
// Reddit's comments-endpoint query params (limit = max comments, depth = max
// subtree nesting): https://www.reddit.com/dev/api/#GET_comments_{article}
func (f *Fetcher) FetchThread(ctx context.Context, permalink string, limit, depth int, sort string) ([]byte, error) {
	page := redditOrigin + permalink
	jsonURL := fmt.Sprintf("%s%s.json?limit=%d&depth=%d&sort=%s&raw_json=1",
		redditOrigin, strings.TrimSuffix(permalink, "/"), limit, depth, url.QueryEscape(sort))
	return f.browserFetch(ctx, page, getJS(jsonURL))
}

// FetchListing retrieves a subreddit listing (hot/new/top/…) via its .json
// endpoint, fetched same-origin from inside a real browser on reddit.com — the
// same bot-wall evasion FetchThread uses. limit caps the number of posts.
func (f *Fetcher) FetchListing(ctx context.Context, sub, sort string, limit int) ([]byte, error) {
	page := fmt.Sprintf("%s/r/%s/%s/", redditOrigin, sub, sort)
	jsonURL := fmt.Sprintf("%s/r/%s/%s.json?limit=%d&raw_json=1", redditOrigin, sub, sort, limit)
	return f.browserFetch(ctx, page, getJS(jsonURL))
}

// FetchMoreChildren expands collapsed reply branches via /api/morechildren.
// linkID must include the t3_ prefix; childIDs are bare IDs (no prefix). Each
// call navigates the thread page afresh (crawl4ai's /execute_js has no session
// reuse), then runs the same-origin POST from that page.
func (f *Fetcher) FetchMoreChildren(ctx context.Context, linkID string, childIDs []string, sort string) ([]byte, error) {
	id36 := strings.TrimPrefix(linkID, kindPostPrefix)
	page := redditOrigin + "/comments/" + id36 + "/"

	form := url.Values{}
	form.Set("api_type", "json")
	form.Set("link_id", linkID)
	form.Set("children", strings.Join(childIDs, ","))
	form.Set("limit_children", "false")
	form.Set("sort", sort)
	form.Set("raw_json", "1")
	return f.browserFetch(ctx, page, postJS(redditOrigin+"/api/morechildren", form.Encode()))
}

// ResolveShareURL resolves a Reddit share link (/r/{sub}/s/{code}) to its
// canonical /comments/ permalink: the browser follows the 301 redirect and we
// read the resulting location. Returns the full canonical URL (tracking query
// params and all — NormalizePermalink only looks at the path).
func (f *Fetcher) ResolveShareURL(ctx context.Context, shareURL string) (string, error) {
	resolved, err := f.browserExec(ctx, shareURL, "return location.href;")
	if err != nil {
		return "", err
	}
	if !strings.Contains(resolved, "/comments/") {
		return "", fmt.Errorf("share link did not resolve to a thread (got %q)", redactQuery(resolved))
	}
	return resolved, nil
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

// fetchEnvelope is what the in-page snippet returns: the HTTP status of the
// Reddit fetch and its raw body — so we can tell a Reddit-side block (403)
// apart from a crawl4ai/navigation failure.
type fetchEnvelope struct {
	S int    `json:"s"`
	B string `json:"b"`
}

// browserExec navigates navURL via crawl4ai's /execute_js and runs js, returning
// the first JS return value as a string. Shared by browserFetch (which expects a
// {s,b} envelope) and ResolveShareURL (which expects a URL).
//
// crawl4ai builds the browser/crawler config server-side for /execute_js, so we
// can't pass request-level evasions (enable_stealth, override_navigator) the way
// the old /crawl path did — the server's default browser config clears Reddit's
// wall. If Reddit starts challenging this path, enable stealth in the crawl4ai
// server config rather than here.
func (f *Fetcher) browserExec(ctx context.Context, navURL, js string) (string, error) {
	if f.execJSURL == "" {
		return "", fmt.Errorf("crawl4ai endpoint not configured (set OMNIFEED_CRAWL4AI_URL)")
	}

	reqBody, err := json.Marshal(execJSRequest{URL: navURL, Scripts: []string{js}})
	if err != nil {
		return "", fmt.Errorf("marshal crawl4ai request: %w", err)
	}

	// A Reddit block surfaces as a crawl4ai 5xx whose body names the anti-bot
	// verdict; don't retry it (re-driving the browser crawl just pays the cost
	// again). antibot.RetryableStatus vetoes exactly that while still retrying a
	// genuine transient crawl4ai 5xx — the same predicate the generic engine uses.
	headers := map[string]string{"Content-Type": "application/json"}
	if f.token != "" {
		headers["Authorization"] = "Bearer " + f.token
	}
	resp, err := f.client.DoRetry(ctx, http.MethodPost, f.execJSURL, reqBody, headers,
		httpx.RetryConfig{RetryableStatus: antibot.RetryableStatus})
	if err != nil {
		// A Reddit nav that trips crawl4ai's anti-bot detector surfaces here as a
		// 5xx (DoRetry error path); classify unmatched client errors as a block.
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

// browserFetch runs js (a getJS/postJS snippet that returns a {s,b} envelope)
// and returns the fetched Reddit body, distinguishing a Reddit-side block (the
// envelope's non-200 status) from a crawl4ai/navigation failure.
func (f *Fetcher) browserFetch(ctx context.Context, navURL, js string) ([]byte, error) {
	envStr, err := f.browserExec(ctx, navURL, js)
	if err != nil {
		return nil, err
	}
	var env fetchEnvelope
	if err := json.Unmarshal([]byte(envStr), &env); err != nil {
		return nil, fmt.Errorf("decode fetch envelope: %w", err)
	}
	if env.S != http.StatusOK {
		return nil, &domain.FetchError{
			Kind:       domain.KindForStatus(env.S),
			StatusCode: env.S,
			Err:        fmt.Errorf("reddit returned %d via browser: %s", env.S, truncate(env.B, 200)),
		}
	}
	if !json.Valid([]byte(env.B)) {
		if marker, blocked := antibot.Detect(env.B); blocked {
			return nil, &domain.FetchError{Kind: domain.KindCaptcha, StatusCode: env.S, Marker: marker}
		}
		return nil, &domain.FetchError{Kind: domain.KindBotBlock, Err: fmt.Errorf("reddit response not JSON (likely bot-blocked): %s", truncate(env.B, 200))}
	}
	return []byte(env.B), nil
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

// redactQuery strips the query string from a URL before logging it: Reddit's
// share-link redirect appends a transient anti-bot token (js_challenge/token)
// we don't want in error logs. Scheme+host+path are enough for diagnosis.
func redactQuery(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		u = u[:i]
	}
	return truncate(u, 200)
}
