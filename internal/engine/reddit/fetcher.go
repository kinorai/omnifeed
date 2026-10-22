package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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

// Fetcher retrieves Reddit data. Reddit closed logged-out .json in 2026, so
// every method needs something the logged-out web lacks:
//   - client_id and session call Reddit's JSON endpoints over plain HTTP with
//     a credential (see directClient). Logged in, Go's net/http passes.
//   - browser reads the rendered thread page through a browser.Browser
//     (crawl4ai's /execute_js), the only logged-out path left.
//
// OpenChain tries them in the configured order (see Chain). Open uses one
// backend only: the first credential, else the browser.
type Fetcher struct {
	browser browser.Browser
	quota   httpx.Limiter

	// The chain (see Chain): configured methods in order, how long a failed
	// one is skipped, and the hooks that report outcomes and state changes.
	methods   []*method
	cooldown  time.Duration
	onOutcome func(method, outcome string)
	onState   func(method string, up bool, why string, hold time.Duration)
	onBudget  func(method string, remaining float64)
	onInPage  func(kind string, n int)

	// The holds shared with other replicas, when configured. After a failure
	// the chain uses only its own memory until sharedDownUntil.
	shared          SharedState
	onSharedError   func(op string, err error)
	onSharedDown    func(down bool)
	sharedMu        sync.Mutex
	sharedDownUntil time.Time

	now func() time.Time // the clock, nil meaning time.Now (tests set it)
}

// FetcherConfig configures a Fetcher.
type FetcherConfig struct {
	Browser browser.Browser

	// Quota, when non-nil, admits every request that reaches Reddit — the
	// thread fetch, each /api/morechildren round, a listing, a share-link
	// resolve — not the crawl as a whole. Reddit counts requests, and one
	// expand=full crawl can be 40 of them. The requests a browser page sends
	// itself (scroll batches, "more replies") are charged once the page
	// answers. Nil disables it (OMNIFEED_REDDIT_QUOTA=0, the default).
	Quota httpx.Limiter

	// ClientID and SessionCookie, when set, enable the client_id and session
	// methods: Reddit still serves its .json endpoints to authenticated
	// clients, plain HTTP ones included (see directClient). ClientID is an
	// OAuth app's client ID, used with the installed-client grant. HTTP is
	// the client for both; it must not follow redirects (share links resolve
	// from the Location header).
	ClientID      string
	SessionCookie string
	UserAgent     string
	HTTP          *http.Client

	// Methods is the chain order (MethodClientID, MethodSession,
	// MethodBrowser); a method without its credential is left out. Empty
	// means DefaultMethods. Cooldown is how long a method that failed in a way
	// the next one can get around is skipped; 0 means DefaultMethodCooldown.
	Methods  []string
	Cooldown time.Duration
	// OnOutcome is told each call's result per method: "ok", "incomplete"
	// (a browser answer holding less than the page claims), "not_found",
	// "forbidden" (a subreddit refused to everyone), "layout" (an unreadable
	// page), "error", or the failure kind that moved the call on. Method
	// MethodChain with "all_held" is a call that found every method on hold.
	// OnState is told when a method starts or stops being skipped: why is
	// "failed", "backoff" (Reddit answered but asked for one) or "held"
	// (found on hold, maybe by another replica), and hold how long for; both
	// are empty when up. Both hooks are for metrics and logs and can be nil.
	OnOutcome func(method, outcome string)
	OnState   func(method string, up bool, why string, hold time.Duration)
	// OnBudget is told the X-Ratelimit-Remaining of each direct answer that
	// carries it: the requests left in Reddit's window. It can be nil.
	OnBudget func(method string, remaining float64)
	// OnInPage is told how many requests a browser page sent itself, by kind
	// ("listing" or "thread"): the scroll batches and "more replies" that come
	// on top of the page load. It can be nil.
	OnInPage func(kind string, n int)

	// Shared, when non-nil, shares the methods' holds between replicas
	// (main.go sets it when OMNIFEED_REDIS_URL is set). It fails open: on an
	// error the chain uses only this pod's memory for 30s. OnSharedError is
	// told each failed operation, and OnSharedDown each change between shared
	// and per-pod state. Both can be nil.
	Shared        SharedState
	OnSharedError func(op string, err error)
	OnSharedDown  func(down bool)
}

// NewFetcher constructs a Fetcher from cfg.
func NewFetcher(cfg FetcherConfig) *Fetcher {
	f := &Fetcher{browser: cfg.Browser, quota: cfg.Quota,
		cooldown: cfg.Cooldown, onOutcome: cfg.OnOutcome, onState: cfg.OnState, onBudget: cfg.OnBudget, onInPage: cfg.OnInPage,
		shared: cfg.Shared, onSharedError: cfg.OnSharedError, onSharedDown: cfg.OnSharedDown}
	if f.cooldown <= 0 {
		f.cooldown = DefaultMethodCooldown
	}
	var token, cookie *directClient
	if cfg.ClientID != "" {
		token = newTokenClient(cfg.HTTP, cfg.UserAgent, cfg.ClientID)
	}
	if cfg.SessionCookie != "" {
		cookie = newCookieClient(cfg.HTTP, cfg.UserAgent, cfg.SessionCookie)
	}
	order := cfg.Methods
	if len(order) == 0 {
		order = DefaultMethods
	}
	for _, name := range order {
		switch {
		case name == MethodClientID && token != nil:
			f.methods = append(f.methods, f.directMethod(name, token))
		case name == MethodSession && cookie != nil:
			f.methods = append(f.methods, f.directMethod(name, cookie))
		case name == MethodBrowser && cfg.Browser != nil:
			f.methods = append(f.methods, f.browserMethod())
		}
	}
	return f
}

// directMethod is a chain method over one credential. It does not penalize
// the www.reddit.com crawl limiter: each credential has its own budget, so a
// 429 on one is handled by skipping that method, not by holding the others.
func (f *Fetcher) directMethod(name string, c *directClient) *method {
	m := &method{name: name}
	m.open = func(context.Context) (*Session, error) {
		s := &Session{direct: c, quota: f.quota}
		s.penalize = f.penalizeMethod(m, s)
		s.budget = func(remaining float64) {
			if f.onBudget != nil {
				f.onBudget(name, remaining)
			}
		}
		return s, nil
	}
	return m
}

// browserMethod is the chain method over the browser. Like the direct ones,
// Reddit's back-off holds this method only: on the crawl limiter it would
// stall every method behind www.reddit.com, a working credential included.
func (f *Fetcher) browserMethod() *method {
	m := &method{name: MethodBrowser}
	m.open = func(ctx context.Context) (*Session, error) {
		s, err := f.openBrowser(ctx)
		if err == nil {
			s.penalize = f.penalizeMethod(m, s)
		}
		return s, err
	}
	return m
}

// penalizeMethod turns a session's penalize call into a hold on its method.
func (f *Fetcher) penalizeMethod(m *method, s *Session) func(string, time.Duration) {
	return func(_ string, d time.Duration) {
		s.held = true
		f.holdMethod(context.Background(), m, d, whyBackoff)
	}
}

// holdMethod skips m for at least d, on this pod and, when shared, on every
// replica.
func (f *Fetcher) holdMethod(ctx context.Context, m *method, d time.Duration, why string) {
	m.hold(f.clock(), d)
	f.setDown(m, true, why, d)
	if f.sharedReady() {
		f.sharedDone("hold", f.shared.Hold(context.WithoutCancel(ctx), m.name, d))
	}
}

// markUp records a success through m, for a call that started at start. If
// m was down, it ends the hold on every replica too.
func (f *Fetcher) markUp(ctx context.Context, m *method, start time.Time) {
	if !m.clear(start) {
		return
	}
	if f.setDown(m, false, "", 0) && f.sharedReady() {
		f.sharedDone("clear", f.shared.Clear(context.WithoutCancel(ctx), m.name))
	}
}

// setDown reports m's state through OnState when it changes, and reports
// whether it changed. why and hold say what put it down (see OnState).
func (f *Fetcher) setDown(m *method, down bool, why string, hold time.Duration) bool {
	if !m.report(down) {
		return false
	}
	f.state(m.name, !down, why, hold)
	return true
}

// LoadSharedHolds reads the holds other replicas recorded and reports the
// held methods down, so a pod that starts during a hold does not report every
// method up until its first call. A no-op without shared state.
func (f *Fetcher) LoadSharedHolds(ctx context.Context) {
	holds := f.sharedHolds(ctx)
	now := f.clock()
	for _, m := range f.methods {
		if left, ok := holds[m.name]; ok && left > 0 {
			m.hold(now, left)
			f.setDown(m, true, whyHeld, left)
		}
	}
}

// sharedHolds reads the holds other replicas recorded. It returns nil when
// nothing is shared or the shared state is failing.
func (f *Fetcher) sharedHolds(ctx context.Context) map[string]time.Duration {
	if !f.sharedReady() {
		return nil
	}
	holds, err := f.shared.Holds(ctx, f.Methods())
	if ctx.Err() != nil {
		// The caller's own deadline, not the backend's health.
		return nil
	}
	f.sharedDone("read", err)
	return holds
}

// sharedReady reports whether the shared state is configured and not in its
// failure cooldown.
func (f *Fetcher) sharedReady() bool {
	if f.shared == nil {
		return false
	}
	f.sharedMu.Lock()
	defer f.sharedMu.Unlock()
	return !f.clock().Before(f.sharedDownUntil)
}

// sharedDone records the result of one shared operation. An error starts the
// failure cooldown, and a success after one ends it.
func (f *Fetcher) sharedDone(op string, err error) {
	f.sharedMu.Lock()
	was := !f.sharedDownUntil.IsZero()
	if err != nil {
		f.sharedDownUntil = f.clock().Add(sharedCooldown)
	} else {
		f.sharedDownUntil = time.Time{}
	}
	f.sharedMu.Unlock()
	if err != nil && f.onSharedError != nil {
		f.onSharedError(op, err)
	}
	if now := err != nil; now != was && f.onSharedDown != nil {
		f.onSharedDown(now)
	}
}

func (f *Fetcher) clock() time.Time {
	if f.now != nil {
		return f.now()
	}
	return time.Now()
}

// Methods returns the configured chain methods, in order.
func (f *Fetcher) Methods() []string {
	names := make([]string, len(f.methods))
	for i, m := range f.methods {
		names[i] = m.name
	}
	return names
}

func (f *Fetcher) outcome(method, outcome string) {
	if f.onOutcome != nil {
		f.onOutcome(method, outcome)
	}
}

func (f *Fetcher) state(method string, up bool, why string, hold time.Duration) {
	if f.onState != nil {
		f.onState(method, up, why, hold)
	}
}

func (f *Fetcher) openBrowser(ctx context.Context) (*Session, error) {
	bs, err := f.browser.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open %s browser: %w", f.browser.Name(), err)
	}
	return &Session{active: bs, quota: f.quota, onInPage: f.onInPage}, nil
}

// Session is one chain method's connection for one crawl: a browser session,
// or a direct client. Not safe for concurrent use.
type Session struct {
	active browser.Session
	direct *directClient // set for a direct method; active is nil then
	held   bool          // Reddit asked for back-off on the last answer
	// cut is why the browser's in-page expansion stopped early on the last
	// answer ("" when it did not), and incomplete why that answer holds less
	// than the page claims. The chain reads both into the crawl's
	// partial_reason (see Chain.Partial).
	cut        string
	incomplete string
	quota      httpx.Limiter
	penalize   func(rawURL string, d time.Duration) // holds this session's method
	onInPage   func(kind string, n int)             // reports a page's own requests
	budget     func(remaining float64)              // reports a direct answer's X-Ratelimit-Remaining
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
// unwraps the {s,b} envelope into the Reddit body. kind ("listing" or
// "thread") labels the requests the page sent itself, which are charged to the
// quota once it answers.
func (s *Session) fetchViaBrowser(ctx context.Context, kind, navURL, js string) ([]byte, error) {
	release, err := s.admit(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.active.Navigate(ctx, navURL); err != nil {
		release()
		return nil, err
	}
	envStr, err := s.active.Eval(ctx, js)
	release()
	if err != nil {
		s.holdAfterFailedPage(err)
		return nil, err
	}
	var env fetchEnvelope
	if err := json.Unmarshal([]byte(envStr), &env); err != nil {
		return nil, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("decode fetch envelope: %w", err)}
	}
	if env.N > 0 {
		if s.onInPage != nil {
			s.onInPage(kind, env.N)
		}
		s.charge(env.N)
	}
	s.cut = env.cut()
	return s.unwrap(env.unwrap())
}

// chargeTimeout bounds how long Session.charge waits for quota slots. It only
// has to outlast a full window or two; a charge still waiting past it was for
// a quota far smaller than the page's requests.
var chargeTimeout = 10 * time.Minute

// charge books n requests in the quota: the ones a page sent itself (scroll
// batches, "more replies"), which the limiter could not admit beforehand. It
// runs after the answer is back, so this crawl doesn't wait for it; the next
// one waits as it would behind any n requests. Each slot is released at once:
// these requests are done, they only have to count in the window.
func (s *Session) charge(n int) {
	if s.quota == nil || n <= 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), chargeTimeout)
		defer cancel()
		for range n {
			release, err := s.quota.Acquire(ctx, "reddit", redditOrigin+"/")
			if err != nil {
				return
			}
			release()
		}
	}()
}

// holdAfterFailedPage holds every Reddit request in the quota for
// defaultRateLimitBackoff after a page load crawl4ai answered with its scrubbed
// 500. crawl4ai (0.9.2+) hides why behind a generic body, and its log shows the
// usual cause: Reddit refused the navigation with a 429. Without the hold, the
// next fetch walks straight into the same wall and keeps the window full; with
// it, a crash costs one minute. A 500 that names its verdict (the content gate,
// a block page) and any other status are not this, and nothing is held. With
// no quota (OMNIFEED_REDDIT_QUOTA=0) there is nothing to hold.
func (s *Session) holdAfterFailedPage(err error) {
	var se *httpx.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusInternalServerError || !antibot.IsScrubbedServerError(se.Body) {
		return
	}
	if p, ok := s.quota.(interface {
		Penalize(rawURL string, d time.Duration)
	}); ok {
		p.Penalize(redditOrigin+"/", defaultRateLimitBackoff)
	}
}

// fetchDirect GETs a Reddit JSON endpoint with the logged-in session and
// unwraps the response the same way as a browser fetch.
func (s *Session) fetchDirect(ctx context.Context, rawURL string) ([]byte, error) {
	release, err := s.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	env, err := s.direct.get(ctx, rawURL)
	if err == nil && env.S == http.StatusForbidden {
		if reason := refusalReason(env.B); reason != "" {
			// A private, quarantined or banned subreddit: every method gets
			// this answer, so neither the retry nor the next method can help.
			return nil, domain.NoFallback(&domain.FetchError{Kind: domain.KindHTTP403, StatusCode: env.S,
				Err: fmt.Errorf("reddit refuses this subreddit to everyone: it is %s", reason)})
		}
		// Reddit's edge refuses a valid session now and then: in a 14-thread
		// run one 403 came between successes and the same URL answered 200
		// seconds later. One spaced retry absorbs that; an expired session
		// fails both.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(directRetryDelay):
		}
		env, err = s.direct.get(ctx, rawURL)
	}
	if err != nil {
		return nil, err
	}
	if rm, ok := headerNumber(env.RM); ok && s.budget != nil {
		s.budget(rm)
	}
	out, err := s.unwrap(env.unwrap())
	if err != nil && env.S == http.StatusForbidden {
		return nil, fmt.Errorf("reddit refused this request twice with the configured credential (if every Reddit fetch fails this way, check OMNIFEED_REDDIT_CLIENT_ID, or refresh OMNIFEED_REDDIT_SESSION): %w", err)
	}
	return out, err
}

// directRetryDelay spaces the one retry after a 403 in logged-in mode.
var directRetryDelay = 2 * time.Second

// unwrap hands Reddit's back-off from an unwrapped envelope to the method.
func (s *Session) unwrap(out string, wait time.Duration, err error) ([]byte, error) {
	// Reddit's own back-off answer outlives this request: hold the method so
	// the NEXT crawl skips it, instead of walking into the same wall and
	// extending the block. Applied on success too — a 200 with
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
	return env.unwrap()
}

// unwrap yields the Reddit body of a decoded envelope, as unwrapEnvelope does.
func (env fetchEnvelope) unwrap() (body string, wait time.Duration, err error) {
	wait = env.backoff()
	if env.NF != 0 {
		return "", wait, env.missing()
	}
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

// FetchThread retrieves a thread from its rendered HTML page, read from inside
// a real browser on the reddit.com origin (see html.go for why not .json). It
// returns the [post, comments] listing pair the .json endpoint used to, so
// ParseThread reads it unchanged. limit is the comment count at which the
// in-page expansion stops, and expand the most "more replies" requests it may
// send (capped at maxExpandRequests): the browser's counterpart of the
// /api/morechildren rounds a direct method makes. depth is not applied: the
// page renders its own nesting. A direct method ignores expand; the engine
// expands its gaps afterwards.
func (s *Session) FetchThread(ctx context.Context, permalink string, limit, depth int, sort string, expand int) ([]byte, error) {
	if s.direct != nil {
		return s.fetchDirect(ctx, fmt.Sprintf("%s%s.json?limit=%d&depth=%d&sort=%s&raw_json=1",
			s.direct.origin, strings.TrimSuffix(permalink, "/"), limit, depth, url.QueryEscape(sort)))
	}
	return s.fetchViaBrowser(ctx, "thread", threadPageURL(permalink, sort),
		threadHTMLJS(limit, min(max(expand, 0), maxExpandRequests)))
}

// FetchListing retrieves a subreddit listing (hot/new/top/…) from its rendered
// HTML page, read from inside a real browser on reddit.com — the same path
// FetchThread uses. limit caps the number of posts, and t is Reddit's time
// window (hour|day|week|month|year|all), appended only when set. Whether a
// window is meaningful for the sort is ParseListingURL's call — it only sets t
// for top/controversial — so this appends whatever it is handed.
func (s *Session) FetchListing(ctx context.Context, sub, sort string, limit int, t string) ([]byte, error) {
	if s.direct != nil {
		u := fmt.Sprintf("%s/r/%s/%s.json?limit=%d&raw_json=1", s.direct.origin, sub, sort, limit)
		if t != "" {
			u += "&t=" + url.QueryEscape(t)
		}
		return s.fetchDirect(ctx, u)
	}
	// The compact view renders the feed's posts as one dense list: far more
	// of them reach the page than in the default card view (see
	// listingHTMLJS).
	q := url.Values{"feedViewType": {"compactView"}}
	if t != "" {
		q.Set("t", t)
	}
	page := fmt.Sprintf("%s/r/%s/%s/?%s", redditOrigin, sub, sort, q.Encode())
	return s.fetchViaBrowser(ctx, "listing", page, listingHTMLJS(limit))
}

// FetchMoreChildren expands collapsed reply branches via /api/morechildren.
// linkID must include the t3_ prefix; childIDs are bare IDs (no prefix). Only
// a direct method can: Reddit refuses the logged-out call with 403, so the
// browser session has no path to it (the chain never asks it).
func (s *Session) FetchMoreChildren(ctx context.Context, linkID string, childIDs []string, sort string) ([]byte, error) {
	form := url.Values{}
	form.Set("api_type", "json")
	form.Set("link_id", linkID)
	form.Set("children", strings.Join(childIDs, ","))
	form.Set("limit_children", "false")
	form.Set("sort", sort)
	form.Set("raw_json", "1")
	if s.direct == nil {
		return nil, errors.New("morechildren needs a direct method: Reddit refuses it logged out")
	}
	return s.fetchDirect(ctx, s.direct.origin+"/api/morechildren.json?"+form.Encode())
}

// ResolveShareURL resolves a Reddit share link (/r/{sub}/s/{code}) to its
// canonical /comments/ permalink: the browser follows the 301 redirect and we
// read the resulting location. Returns the full canonical URL (tracking query
// params and all — NormalizePermalink only looks at the path).
func (s *Session) ResolveShareURL(ctx context.Context, shareURL string) (string, error) {
	shareURL, err := shareRequestURL(shareURL)
	if err != nil {
		return "", err
	}
	release, err := s.admit(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	var resolved string
	if s.direct != nil {
		resolved, err = s.direct.location(ctx, shareURL)
	} else {
		if err := s.active.Navigate(ctx, shareURL); err != nil {
			return "", err
		}
		resolved, err = s.active.Eval(ctx, "return location.href;")
	}
	if err != nil {
		return "", err
	}
	if !strings.Contains(resolved, "/comments/") {
		return "", fmt.Errorf("share link did not resolve to a thread (got %q)", redactQuery(resolved))
	}
	return resolved, nil
}

// shareRequestURL rebuilds a share link on redditOrigin, keeping only its
// path. The caller chose the scheme, host and port of the URL it passed in;
// the cookie method sends reddit_session with this request, so none of them
// may come from the caller.
func shareRequestURL(shareURL string) (string, error) {
	u, err := url.Parse(shareURL)
	if err != nil || !shareRE.MatchString(u.Path) {
		return "", fmt.Errorf("not a Reddit share link: %q", redactQuery(shareURL))
	}
	return redditOrigin + u.EscapedPath(), nil
}

// --- fetch envelope ---

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
	// The browser snippets only. NF is set when the rendered page holds no
	// post (S is then the navigation's own status, B the page text). XS is the
	// status of the in-page expansion request Reddit refused, -1 when it never
	// answered, 0 when none was refused.
	NF int `json:"nf,omitempty"`
	XS int `json:"xs,omitempty"`
	// N is how many requests the page sent itself: scroll batches for a
	// listing, "more replies" for a thread.
	N int `json:"n,omitempty"`
}

// Page texts Reddit renders, logged out, instead of a thread or a listing.
var (
	gonePhrases = []string{"page not found", "community not found", "there's nothing here",
		"nobody on reddit goes by that name", "this community doesn't exist"}
	refusedPhrases = map[string]string{"is a private community": "private",
		"private community": "private", "quarantined": "quarantined", "has been banned": "banned",
		"was banned": "banned"}
)

// missing classifies a rendered page that holds no post (see html.go). A wall
// moves the chain on, like any block. A thread or subreddit that is gone, or
// that Reddit refuses to everyone, fails the same on every method, so it is
// final. Anything else is a page the snippet can no longer read: a layout
// error, which no other method's hold would fix either.
func (env fetchEnvelope) missing() error {
	if marker, blocked := antibot.Detect(env.B); blocked {
		return &domain.FetchError{Kind: domain.KindCaptcha, StatusCode: env.S, Marker: marker}
	}
	text := strings.NewReplacer("\u2019", "'").Replace(strings.ToLower(env.B))
	for phrase, why := range refusedPhrases {
		if strings.Contains(text, phrase) {
			return domain.NoFallback(&domain.FetchError{Kind: domain.KindHTTP403, StatusCode: http.StatusForbidden,
				Err: fmt.Errorf("reddit refuses this subreddit to everyone: it is %s", why)})
		}
	}
	gone := env.S == http.StatusNotFound || env.S == http.StatusGone
	for _, phrase := range gonePhrases {
		gone = gone || strings.Contains(text, phrase)
	}
	if gone {
		return domain.NoFallback(&domain.FetchError{Kind: domain.KindNotFound, StatusCode: http.StatusNotFound,
			Err: fmt.Errorf("reddit has no such thread or subreddit: %s", truncate(env.B, 200))})
	}
	if env.S == http.StatusForbidden {
		return &domain.FetchError{Kind: domain.KindHTTP403, StatusCode: env.S,
			Err: fmt.Errorf("reddit returned 403 via browser: %s", truncate(env.B, 200))}
	}
	// A rate-limit or server-error page is Reddit's state, not a markup
	// change: it must not read as layout (and fire OmnifeedRedditLayoutChanged).
	if env.S == http.StatusTooManyRequests {
		return &domain.FetchError{Kind: domain.KindHTTP429, StatusCode: env.S, RetryAfter: env.backoff(),
			Err: fmt.Errorf("reddit returned 429 via browser: %s", truncate(env.B, 200))}
	}
	if env.S >= 500 {
		return &domain.FetchError{Kind: domain.KindUpstreamError, StatusCode: env.S,
			Err: fmt.Errorf("reddit returned %d via browser: %s", env.S, truncate(env.B, 200))}
	}
	return &domain.FetchError{Kind: domain.KindLayout, StatusCode: env.S,
		Err: fmt.Errorf("the rendered reddit page has no post to read (markup changed?): %s", truncate(env.B, 200))}
}

// cut is why the in-page expansion stopped early, as a partial_reason, or ""
// when it was not refused.
func (env fetchEnvelope) cut() string {
	switch {
	case env.XS == 0:
		return ""
	case env.XS < 0:
		return "expansion_failed"
	}
	return string(domain.KindForStatus(env.XS))
}

// backoff returns how long Reddit asked us to stay away, or 0 when it did not.
// Only a spent budget asks: a 429, or any response whose X-Ratelimit-Remaining
// is below 1. The hold is Retry-After when present, else X-Ratelimit-Reset,
// else — for a 429 that names nothing — defaultRateLimitBackoff.
func (e fetchEnvelope) backoff() time.Duration {
	rateLimited := e.S == http.StatusTooManyRequests || e.XS == http.StatusTooManyRequests
	limited := rateLimited
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
	if rateLimited {
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
