package reddit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/antibot"
	"github.com/kinorai/omnifeed/internal/browser"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// redditOrigin is the reddit.com host we navigate and fetch from. We use
// www (not old.reddit.com): Reddit's edge "network security" wall trips
// per-host on a risk score, and old.reddit gets blocked intermittently while
// www stays clear.
const redditOrigin = "https://www.reddit.com"

// Fetcher retrieves Reddit data through a real headless browser. Reddit's edge
// hard-blocks non-browser HTTP clients (Go's net/http gets a 403 "network
// security" wall keyed on the TLS/JA3 fingerprint), so we never hit Reddit
// directly. Instead a crawl opens a browser Session, navigates to a reddit.com
// page (which clears the bot challenge), and runs a same-origin fetch() of the
// target JSON endpoint from inside that page — the browser context passes the
// wall and the in-page fetch inherits it, so the JSON comes back exactly as a
// logged-out browser would see it (no auth, no cookies).
//
// The Session is backed by a browser.Browser (crawl4ai's /execute_js).
type Fetcher struct {
	browser  browser.Browser
	quota    httpx.Limiter
	penalize func(rawURL string, d time.Duration)
}

// FetcherConfig configures a Fetcher.
type FetcherConfig struct {
	Browser browser.Browser

	// Quota, when non-nil, admits every request that reaches Reddit — the
	// thread fetch, each /api/morechildren round, a listing, a share-link
	// resolve — not the crawl as a whole. Reddit counts requests, and one
	// expand=full crawl can be 40 of them. Nil disables it
	// (OMNIFEED_REDDIT_QUOTA=0, the default).
	Quota httpx.Limiter

	// Penalize, when non-nil, is told how long Reddit asked us to stay away
	// (Retry-After / X-Ratelimit-Reset on a 429, or when X-Ratelimit-Remaining
	// hits 0), keyed on the Reddit origin. main.go points it at the per-domain
	// limiter, so the next crawl waits out the block instead of extending it.
	Penalize func(rawURL string, d time.Duration)
}

// NewFetcher constructs a Fetcher from cfg.
func NewFetcher(cfg FetcherConfig) *Fetcher {
	return &Fetcher{browser: cfg.Browser, quota: cfg.Quota, penalize: cfg.Penalize}
}

// Open starts a crawl session. The caller owns it and must Close it.
func (f *Fetcher) Open(ctx context.Context) (*Session, error) {
	bs, err := f.browser.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open %s browser: %w", f.browser.Name(), err)
	}
	return &Session{active: bs, quota: f.quota, penalize: f.penalize}, nil
}

// Session is one crawl's browser session. All fetches in a crawl share one
// Session so state like the recorded thread page carries across them. Not safe
// for concurrent use.
type Session struct {
	active     browser.Session
	threadPage string // the thread page FetchThread navigated to, reused by morechildren
	quota      httpx.Limiter
	penalize   func(rawURL string, d time.Duration)
}

// admit takes one slot of the Reddit request quota and returns its release.
// The slot is keyed on redditOrigin whatever URL the request is for — a share
// link may be on bare reddit.com, and the limiter buckets by hostname, so
// keying on the request URL would split one IP's budget across two counters. A refused or canceled wait comes back
// classified (quota_exhausted with its retry-after, timeout, canceled), so the
// caller sees the same error shape every other pacing refusal has.
func (s *Session) admit(ctx context.Context) (func(), error) {
	if s.quota == nil {
		return func() {}, nil
	}
	release, err := s.quota.Acquire(ctx, "reddit", redditOrigin+"/")
	if err != nil {
		return nil, httpx.ClassifyClientError(err, domain.KindError)
	}
	return release, nil
}

// Close releases the browser session.
func (s *Session) Close(ctx context.Context) error {
	if s.active == nil {
		return nil
	}
	return s.active.Close(ctx)
}

// fetchViaBrowser navigates navURL, runs the in-page fetch snippet js, and
// unwraps the {s,b} envelope into the Reddit body.
func (s *Session) fetchViaBrowser(ctx context.Context, navURL, js string) ([]byte, error) {
	release, err := s.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := s.active.Navigate(ctx, navURL); err != nil {
		return nil, err
	}
	envStr, err := s.active.Eval(ctx, js)
	if err != nil {
		return nil, err
	}
	out, wait, err := unwrapEnvelope(envStr)
	// Reddit's own back-off answer outlives this request: hand it to the
	// limiter so the NEXT crawl waits it out, instead of walking into the same
	// wall and extending the block. Applied on success too — a 200 with
	// X-Ratelimit-Remaining 0 means the next request will be refused.
	if wait > 0 && s.penalize != nil {
		s.penalize(redditOrigin+"/", wait)
	}
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// defaultRateLimitBackoff is the hold applied on a Reddit 429 that names no
// back-off of its own (neither Retry-After nor X-Ratelimit-Reset). Reddit's
// unauthenticated budget refills per minute, so a minute is the smallest wait
// that is not a guess at hammering it again.
const defaultRateLimitBackoff = time.Minute

// unwrapEnvelope decodes the envelope the in-page snippet returns and yields
// the Reddit body, distinguishing a Reddit-side block (non-200 envelope
// status, or a non-JSON body carrying an anti-bot marker) from a clean
// response. wait is how long Reddit asked us to stay away (0 when it did not);
// a 429 error carries it as RetryAfter.
func unwrapEnvelope(envStr string) (body string, wait time.Duration, err error) {
	var env fetchEnvelope
	if err := json.Unmarshal([]byte(envStr), &env); err != nil {
		return "", 0, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("decode fetch envelope: %w", err)}
	}
	wait = env.backoff()
	if env.S == http.StatusTooManyRequests {
		return "", wait, &domain.FetchError{
			Kind:       domain.KindHTTP429,
			StatusCode: env.S,
			RetryAfter: wait,
			Err: fmt.Errorf("reddit rate limited this IP; retry in %ds: %s",
				int((wait+time.Second-1)/time.Second), truncate(env.B, 200)),
		}
	}
	if env.S != http.StatusOK {
		return "", wait, &domain.FetchError{
			Kind:       domain.KindForStatus(env.S),
			StatusCode: env.S,
			RetryAfter: wait,
			Err:        fmt.Errorf("reddit returned %d via browser: %s", env.S, truncate(env.B, 200)),
		}
	}
	if !json.Valid([]byte(env.B)) {
		if marker, blocked := antibot.Detect(env.B); blocked {
			return "", wait, &domain.FetchError{Kind: domain.KindCaptcha, StatusCode: env.S, Marker: marker}
		}
		return "", wait, &domain.FetchError{Kind: domain.KindBotBlock, Err: fmt.Errorf("reddit response not JSON (likely bot-blocked): %s", truncate(env.B, 200))}
	}
	return env.B, wait, nil
}

// FetchThread retrieves a thread via the .json endpoint, fetched from inside a
// real browser on the reddit.com origin. limit/depth/sort map directly onto
// Reddit's comments-endpoint query params (limit = max comments, depth = max
// subtree nesting): https://www.reddit.com/dev/api/#GET_comments_{article}
func (s *Session) FetchThread(ctx context.Context, permalink string, limit, depth int, sort string) ([]byte, error) {
	page := redditOrigin + permalink
	jsonURL := fmt.Sprintf("%s%s.json?limit=%d&depth=%d&sort=%s&raw_json=1",
		redditOrigin, strings.TrimSuffix(permalink, "/"), limit, depth, url.QueryEscape(sort))
	// Record the thread page so morechildren re-navigates to the exact same URL —
	// on the live-page backend that makes its Navigate a no-op and the page is
	// reused across all expansion rounds.
	s.threadPage = page
	return s.fetchViaBrowser(ctx, page, getJS(jsonURL))
}

// FetchListing retrieves a subreddit listing (hot/new/top/…) via its .json
// endpoint, fetched same-origin from inside a real browser on reddit.com — the
// same bot-wall evasion FetchThread uses. limit caps the number of posts, and t
// is Reddit's time window (hour|day|week|month|year|all), appended only when set.
// Whether a window is meaningful for the sort is ParseListingURL's call — it
// only sets t for top/controversial — so this appends whatever it is handed.
func (s *Session) FetchListing(ctx context.Context, sub, sort string, limit int, t string) ([]byte, error) {
	page := fmt.Sprintf("%s/r/%s/%s/", redditOrigin, sub, sort)
	jsonURL := fmt.Sprintf("%s/r/%s/%s.json?limit=%d&raw_json=1", redditOrigin, sub, sort, limit)
	if t != "" {
		jsonURL += "&t=" + url.QueryEscape(t)
	}
	return s.fetchViaBrowser(ctx, page, getJS(jsonURL))
}

// FetchMoreChildren expands collapsed reply branches via /api/morechildren.
// linkID must include the t3_ prefix; childIDs are bare IDs (no prefix). It
// re-navigates the thread page FetchThread recorded and runs the same-origin
// POST from it.
func (s *Session) FetchMoreChildren(ctx context.Context, linkID string, childIDs []string, sort string) ([]byte, error) {
	page := s.threadPage
	if page == "" {
		// Deriving a page from the link id here would navigate a URL that never
		// matches the one FetchThread recorded, silently defeating the live-page
		// no-op — fail loudly instead.
		return nil, fmt.Errorf("FetchMoreChildren before FetchThread: no thread page recorded")
	}

	form := url.Values{}
	form.Set("api_type", "json")
	form.Set("link_id", linkID)
	form.Set("children", strings.Join(childIDs, ","))
	form.Set("limit_children", "false")
	form.Set("sort", sort)
	form.Set("raw_json", "1")
	return s.fetchViaBrowser(ctx, page, postJS(redditOrigin+"/api/morechildren", form.Encode()))
}

// ResolveShareURL resolves a Reddit share link (/r/{sub}/s/{code}) to its
// canonical /comments/ permalink: the browser follows the 301 redirect and we
// read the resulting location. Returns the full canonical URL (tracking query
// params and all — NormalizePermalink only looks at the path).
func (s *Session) ResolveShareURL(ctx context.Context, shareURL string) (string, error) {
	release, err := s.admit(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if err := s.active.Navigate(ctx, shareURL); err != nil {
		return "", err
	}
	resolved, err := s.active.Eval(ctx, "return location.href;")
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

// envelopeReturn is the tail every in-page snippet ends with: the status, the
// body, and the rate-limit headers Reddit sends. The fetch is same-origin, so
// every response header is readable (CORS exposure rules only bind
// cross-origin reads); a missing header comes back as null.
const envelopeReturn = `return JSON.stringify({s: r.status, b: await r.text(), ` +
	`ra: r.headers.get("retry-after"), rs: r.headers.get("x-ratelimit-reset"), ` +
	`rm: r.headers.get("x-ratelimit-remaining")});`

// getJS returns an async snippet that GETs u and returns the envelope.
func getJS(u string) string {
	return `const r = await fetch(` + jsLit(u) + `, {headers: {"Accept": "application/json"}}); ` +
		envelopeReturn
}

// postJS returns an async snippet that form-POSTs body to u and returns the
// envelope.
func postJS(u, body string) string {
	return `const r = await fetch(` + jsLit(u) + `, {method: "POST", ` +
		`headers: {"Accept": "application/json", "Content-Type": "application/x-www-form-urlencoded"}, ` +
		`body: ` + jsLit(body) + `}); ` +
		envelopeReturn
}

// fetchEnvelope is what the in-page snippet returns: the HTTP status of the
// Reddit fetch and its raw body — so we can tell a Reddit-side block (403)
// apart from a browser/navigation failure — plus Reddit's rate-limit headers,
// which are the only signal of how long a block will last.
type fetchEnvelope struct {
	S  int    `json:"s"`
	B  string `json:"b"`
	RA string `json:"ra,omitempty"` // Retry-After (seconds)
	RS string `json:"rs,omitempty"` // X-Ratelimit-Reset (seconds until the window resets)
	RM string `json:"rm,omitempty"` // X-Ratelimit-Remaining (requests left; Reddit sends a float, "0.0")
}

// backoff returns how long Reddit asked us to stay away, or 0 when it did not.
// Only a spent budget asks: a 429, or any response whose X-Ratelimit-Remaining
// is below 1. The hold is Retry-After when present, else X-Ratelimit-Reset,
// else — for a 429 that names nothing — defaultRateLimitBackoff.
func (e fetchEnvelope) backoff() time.Duration {
	limited := e.S == http.StatusTooManyRequests
	if rm, ok := headerNumber(e.RM); ok && rm < 1 {
		limited = true
	}
	if !limited {
		return 0
	}
	if ra, ok := headerNumber(e.RA); ok && ra > 0 {
		return seconds(ra)
	}
	if rs, ok := headerNumber(e.RS); ok && rs > 0 {
		return seconds(rs)
	}
	if e.S == http.StatusTooManyRequests {
		return defaultRateLimitBackoff
	}
	return 0
}

// headerNumber parses a non-negative numeric header value. Reddit sends
// integers for Retry-After and X-Ratelimit-Reset but floats ("0.0", "99.0")
// for X-Ratelimit-Remaining, so both forms are accepted. The HTTP-date form of
// Retry-After is not (see httpx.parseRetryAfter for why).
func headerNumber(v string) (float64, bool) {
	if v == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return f, true
}

func seconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

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
