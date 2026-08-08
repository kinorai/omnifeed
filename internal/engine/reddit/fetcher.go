package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/antibot"
	"github.com/kinorai/omnifeed/internal/browser"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/observability"
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
// The Session is backed by a browser.Browser. Two backends exist: crawl4ai
// (default, re-navigates per fetch) and Lightpanda (opt-in, keeps one live page
// so a deep crawl's follow-up fetches skip re-navigation). When a fallback
// browser is configured (Lightpanda primary → crawl4ai fallback), a fetch that
// fails on the primary is retried on the fallback, sticky for the rest of the
// crawl.
type Fetcher struct {
	primary  browser.Browser
	fallback browser.Browser // nil when no fallback is configured
	logger   *slog.Logger
	metrics  *observability.Metrics
}

// FetcherConfig configures a Fetcher. Primary is required; Fallback is optional
// (set only when the primary is a backend worth retrying past, e.g. Lightpanda).
type FetcherConfig struct {
	Primary  browser.Browser
	Fallback browser.Browser
	Logger   *slog.Logger
	Metrics  *observability.Metrics
}

// NewFetcher constructs a Fetcher from cfg.
func NewFetcher(cfg FetcherConfig) *Fetcher {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Fetcher{
		primary:  cfg.Primary,
		fallback: cfg.Fallback,
		logger:   cfg.Logger,
		metrics:  cfg.Metrics,
	}
}

// Open starts a crawl session on the primary browser. The caller owns it and
// must Close it. Fallback happens lazily inside the session on the first fetch
// that fails — not here — so a primary whose process is down (its Open succeeds
// lazily, its first fetch fails) is still handled.
func (f *Fetcher) Open(ctx context.Context) (*Session, error) {
	bs, err := f.primary.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open %s browser: %w", f.primary.Name(), err)
	}
	return &Session{f: f, active: bs}, nil
}

// Session is one crawl's browser session: a live browsing context plus the
// automatic-fallback machinery. All fetches in a crawl share one Session so the
// live-page backend can reuse its page across them. Not safe for concurrent use.
type Session struct {
	f          *Fetcher
	active     browser.Session
	fellBack   bool   // sticky: once we've switched to the fallback, stay there
	threadPage string // the thread page FetchThread navigated to, reused by morechildren
}

// Close releases the active browser session.
func (s *Session) Close(ctx context.Context) error {
	if s.active == nil {
		return nil
	}
	return s.active.Close(ctx)
}

// run executes recipe against the active browser session. On a fallback-worthy
// error — and only if a fallback browser is configured, we haven't already
// switched, and the crawl's context is still live — it opens the fallback
// browser, swaps to it (sticky), and re-runs recipe there. recipe must be
// self-contained (do its own Navigate) so it works on a fresh fallback session.
func (s *Session) run(ctx context.Context, recipe func(bs browser.Session) (string, error)) (string, error) {
	out, err := recipe(s.active)
	if err == nil || s.fellBack || s.f.fallback == nil || ctx.Err() != nil || !shouldFallback(err) {
		return out, err
	}

	fb, oerr := s.f.fallback.Open(ctx)
	if oerr != nil {
		s.f.logger.Warn("browser fallback open failed; keeping primary error",
			"to", s.f.fallback.Name(), "open_err", oerr, "primary_err", err)
		return out, err // surface the original primary error, not the open failure
	}
	s.f.logger.Warn("reddit browser fell back to secondary backend",
		"from", s.f.primary.Name(), "to", s.f.fallback.Name(), "err", err)
	if s.f.metrics != nil {
		s.f.metrics.ObserveBrowserFallback(s.f.primary.Name(), s.f.fallback.Name(), observability.Reason(err))
	}
	_ = s.active.Close(ctx)
	s.active = fb
	s.fellBack = true
	return recipe(s.active)
}

// shouldFallback reports whether err is worth retrying on the fallback backend.
// Anything that means the primary failed to get us content is (a bot wall, a
// nav failure, a dead connection, a bad response). NOT worth it: caller
// cancellation and the crawl's own timeout (the fallback would only exceed the
// same budget), and answers Reddit itself gave through the browser — a 429 is
// keyed on the egress IP both backends share (an immediate replay aggravates
// it) and a 404/5xx/other status is the same answer at twice the cost. Only a
// fingerprint-shaped block (403, CAPTCHA, bot wall) is worth a second browser.
func shouldFallback(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var fe *domain.FetchError
	if errors.As(err, &fe) {
		switch fe.Kind {
		case domain.KindCanceled, domain.KindTimeout,
			domain.KindHTTP429, domain.KindUpstreamError, domain.KindError:
			return false
		}
	}
	return true
}

// fetchViaBrowser navigates navURL, runs the in-page fetch snippet js, and
// unwraps the {s,b} envelope into the Reddit body — the whole recipe wrapped in
// the fallback machinery, so a block or failure on the primary retries the exact
// same fetch on the fallback.
func (s *Session) fetchViaBrowser(ctx context.Context, navURL, js string) ([]byte, error) {
	out, err := s.run(ctx, func(bs browser.Session) (string, error) {
		if err := bs.Navigate(ctx, navURL); err != nil {
			return "", err
		}
		envStr, err := bs.Eval(ctx, js)
		if err != nil {
			return "", err
		}
		return unwrapEnvelope(envStr)
	})
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// unwrapEnvelope decodes the {s,b} envelope the in-page snippet returns and
// yields the Reddit body, distinguishing a Reddit-side block (non-200 envelope
// status, or a non-JSON body carrying an anti-bot marker) from a clean response.
func unwrapEnvelope(envStr string) (string, error) {
	var env fetchEnvelope
	if err := json.Unmarshal([]byte(envStr), &env); err != nil {
		return "", &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("decode fetch envelope: %w", err)}
	}
	if env.S != http.StatusOK {
		return "", &domain.FetchError{
			Kind:       domain.KindForStatus(env.S),
			StatusCode: env.S,
			Err:        fmt.Errorf("reddit returned %d via browser: %s", env.S, truncate(env.B, 200)),
		}
	}
	if !json.Valid([]byte(env.B)) {
		if marker, blocked := antibot.Detect(env.B); blocked {
			return "", &domain.FetchError{Kind: domain.KindCaptcha, StatusCode: env.S, Marker: marker}
		}
		return "", &domain.FetchError{Kind: domain.KindBotBlock, Err: fmt.Errorf("reddit response not JSON (likely bot-blocked): %s", truncate(env.B, 200))}
	}
	return env.B, nil
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
// same bot-wall evasion FetchThread uses. limit caps the number of posts.
func (s *Session) FetchListing(ctx context.Context, sub, sort string, limit int) ([]byte, error) {
	page := fmt.Sprintf("%s/r/%s/%s/", redditOrigin, sub, sort)
	jsonURL := fmt.Sprintf("%s/r/%s/%s.json?limit=%d&raw_json=1", redditOrigin, sub, sort, limit)
	return s.fetchViaBrowser(ctx, page, getJS(jsonURL))
}

// FetchMoreChildren expands collapsed reply branches via /api/morechildren.
// linkID must include the t3_ prefix; childIDs are bare IDs (no prefix). It
// re-navigates the thread page FetchThread recorded (reused live on the
// Lightpanda backend, re-navigated on crawl4ai) and runs the same-origin POST.
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
	return s.run(ctx, func(bs browser.Session) (string, error) {
		if err := bs.Navigate(ctx, shareURL); err != nil {
			return "", err
		}
		resolved, err := bs.Eval(ctx, "return location.href;")
		if err != nil {
			return "", err
		}
		if !strings.Contains(resolved, "/comments/") {
			if resolved != shareURL {
				// Reddit answered with a real redirect to a non-thread page — the
				// fallback would resolve the same way, so don't retry there.
				return "", &domain.FetchError{Kind: domain.KindError,
					Err: fmt.Errorf("share link did not resolve to a thread (got %q)", redactQuery(resolved))}
			}
			// No redirect happened at all: indistinguishable from an un-cleared bot
			// wall, so leave the error fallback-worthy.
			return "", fmt.Errorf("share link did not redirect (still on %q)", redactQuery(resolved))
		}
		return resolved, nil
	})
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

// fetchEnvelope is what the in-page snippet returns: the HTTP status of the
// Reddit fetch and its raw body — so we can tell a Reddit-side block (403)
// apart from a browser/navigation failure.
type fetchEnvelope struct {
	S int    `json:"s"`
	B string `json:"b"`
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
