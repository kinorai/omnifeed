package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// The browser path rebuilds Reddit's JSON from the rendered page, so it is
// where a markup change, a missing thread or a refused expansion shows up.
// These tests pin how each is told apart: which ones move the chain on and
// hold the method, which ones come back to the caller, and which succeed
// marked partial.

const browserThread = `[{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"abc123","title":"t","author":"a","subreddit":"news","num_comments":1,"permalink":"/r/news/comments/abc123/t/"}}]}},` +
	`{"kind":"Listing","data":{"children":[{"kind":"t1","data":{"id":"c1","parent_id":"t3_abc123","link_id":"t3_abc123","author":"b","score":3,"body":"hi","depth":0,"replies":""}}]}}]`

const browserListingBody = `{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"p1","title":"t","permalink":"/r/news/comments/p1/t/"}}]}}`

// browserPages answers the thread snippet with a thread and the listing
// snippet with a listing.
func browserPages() *fakeSession {
	return pageSession(func(js string) string {
		if strings.Contains(js, "shreddit-comment") {
			return envStr(200, browserThread)
		}
		return envStr(200, browserListingBody)
	})
}

func pageSession(fn func(js string) string) *fakeSession {
	return &fakeSession{evalFn: func(js string) (string, error) { return fn(js), nil }}
}

type browserFixture struct {
	outcomes []string
	states   []string
}

func newBrowserFetcher(fx *browserFixture, sess *fakeSession) *Fetcher {
	return NewFetcher(FetcherConfig{
		Browser:   &fakeBrowser{name: "crawl4ai", session: sess},
		Cooldown:  time.Hour,
		OnOutcome: func(m, o string) { fx.outcomes = append(fx.outcomes, m+"="+o) },
		OnState: func(m string, up bool, why string, hold time.Duration) {
			fx.states = append(fx.states, fmt.Sprintf("%s:%v:%s:%s", m, up, why, hold))
		},
	})
}

func chainThread(t *testing.T, f *Fetcher) (*Chain, error) {
	t.Helper()
	c, err := f.OpenChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	_, err = c.FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top")
	return c, err
}

func missingEnv(status int, text string) string {
	b, _ := json.Marshal(fetchEnvelope{S: status, B: text, NF: 1})
	return string(b)
}

// A page with no post used to read as bot_block, which held the browser for
// its cooldown on every replica: one bad URL blocked Reddit for everyone. Now
// only a real wall holds; a gone or refused thread is final, and an unknown
// page is a layout error.
func TestBrowserPageWithoutPost(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		kind     domain.FailureKind
		outcome  string
		final    bool
		heldDown bool
	}{
		{"navigation 404", missingEnv(404, "Some page"), domain.KindNotFound, "not_found", true, false},
		{"not found text", missingEnv(200, "Sorry, nobody on Reddit goes by that name."), domain.KindNotFound, "not_found", true, false},
		{"private community", missingEnv(200, "r/foo is a private community"), domain.KindHTTP403, "forbidden", true, false},
		{"banned community", missingEnv(200, "This community has been banned"), domain.KindHTTP403, "forbidden", true, false},
		{"network security wall", missingEnv(403, "You've been blocked by network security."), domain.KindCaptcha, "captcha", false, true},
		{"unknown page", missingEnv(200, "Welcome to the new Reddit!"), domain.KindLayout, "layout", false, false},
		{"rate-limit page", missingEnv(429, "Too many requests"), domain.KindHTTP429, "http_429", false, true},
		{"server error page", missingEnv(503, "Our CDN was unable to reach our servers"), domain.KindUpstreamError, "upstream_error", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := &browserFixture{}
			f := newBrowserFetcher(fx, pageSession(func(string) string { return tc.env }))
			_, err := chainThread(t, f)
			var fe *domain.FetchError
			if !errors.As(err, &fe) || fe.Kind != tc.kind {
				t.Fatalf("err = %v, want kind %s", err, tc.kind)
			}
			if domain.IsNoFallback(err) != tc.final {
				t.Errorf("NoFallback = %v, want %v", domain.IsNoFallback(err), tc.final)
			}
			if got := fmt.Sprint(fx.outcomes); got != fmt.Sprintf("[browser=%s]", tc.outcome) {
				t.Errorf("outcomes = %s", got)
			}
			if held := len(fx.states) > 0; held != tc.heldDown {
				t.Errorf("states = %v, want held %v", fx.states, tc.heldDown)
			}
		})
	}
}

// A well-formed but empty rebuild is a markup change, not a thread: it must
// not reach the caller (or the cache) as a success.
func TestBrowserThreadChecks(t *testing.T) {
	blankID := strings.Replace(browserThread, `"id":"abc123"`, `"id":""`, 1)
	otherID := strings.Replace(browserThread, `"id":"abc123"`, `"id":"zzz999"`, 1)
	for name, body := range map[string]string{"blank id": blankID, "other thread": otherID} {
		t.Run(name, func(t *testing.T) {
			fx := &browserFixture{}
			f := newBrowserFetcher(fx, pageSession(func(string) string { return envStr(200, body) }))
			_, err := chainThread(t, f)
			var fe *domain.FetchError
			if !errors.As(err, &fe) || fe.Kind != domain.KindLayout {
				t.Fatalf("err = %v, want layout", err)
			}
			if len(fx.states) != 0 {
				t.Errorf("a layout error must not hold the method: states = %v", fx.states)
			}
		})
	}
}

// A post that counts comments the page did not render succeeds, but says so:
// outcome incomplete, and partial_reason browser_incomplete.
func TestBrowserThreadWithoutComments(t *testing.T) {
	body := strings.Replace(browserThread, `"num_comments":1`, `"num_comments":300`, 1)
	head, _, _ := strings.Cut(body, `{"kind":"t1"`)
	body = head + `]}}]`
	fx := &browserFixture{}
	f := newBrowserFetcher(fx, pageSession(func(string) string { return envStr(200, body) }))
	c, err := chainThread(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Partial() != browserIncomplete || c.Served() != MethodBrowser {
		t.Errorf("partial = %q served = %q", c.Partial(), c.Served())
	}
	if fmt.Sprint(fx.outcomes) != "[browser=incomplete]" {
		t.Errorf("outcomes = %v", fx.outcomes)
	}
}

// An in-page expansion Reddit refused with a bare 429 used to set no hold
// and no partial_reason: the next crawl walked back into the 429, and the
// caller could not see the thread was cut.
func TestBrowserExpansionRefused(t *testing.T) {
	env, _ := json.Marshal(fetchEnvelope{S: 200, B: browserThread, XS: 429})
	fx := &browserFixture{}
	f := newBrowserFetcher(fx, pageSession(func(string) string { return string(env) }))
	c, err := chainThread(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if c.Partial() != "http_429" {
		t.Errorf("partial = %q, want http_429", c.Partial())
	}
	want := fmt.Sprintf("[browser:false:backoff:%s]", defaultRateLimitBackoff)
	if fmt.Sprint(fx.states) != want {
		t.Errorf("states = %v, want %s (a back-off hold, not a failure)", fx.states, want)
	}
	if fmt.Sprint(fx.outcomes) != "[browser=ok]" {
		t.Errorf("outcomes = %v", fx.outcomes)
	}
}

// A crawl4ai fault on the last method left is no verdict on Reddit: holding
// it would turn a restart into a cooldown-long outage.
func TestChainDoesNotHoldLastMethodForUpstreamFault(t *testing.T) {
	sess := &fakeSession{evalFn: func(string) (string, error) {
		return "", &domain.FetchError{Kind: domain.KindUpstreamError, Err: errors.New("crawl4ai unreachable")}
	}}
	fx := &browserFixture{}
	f := newBrowserFetcher(fx, sess)
	for range 2 {
		if _, err := chainThread(t, f); err == nil {
			t.Fatal("want the upstream error")
		}
	}
	if len(sess.evals) != 2 || len(fx.states) != 0 {
		t.Errorf("evals = %d states = %v, want both calls tried and no hold", len(sess.evals), fx.states)
	}
}

// When every method is on hold, the call is counted under the chain, apart
// from any method: it is the "Reddit is out of reach" signal.
func TestChainCountsAllHeld(t *testing.T) {
	fx := &browserFixture{}
	f := newBrowserFetcher(fx, pageSession(func(string) string { return missingEnv(403, "You've been blocked by network security.") }))
	_, _ = chainThread(t, f)
	_, err := chainThread(t, f)
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindQuotaExhausted {
		t.Fatalf("err = %v, want quota_exhausted", err)
	}
	if got := fmt.Sprint(fx.outcomes); got != "[browser=captcha chain=all_held]" {
		t.Errorf("outcomes = %s", got)
	}
}

// A direct-only call (more replies) that finds no direct method leaves the
// browser out by design: it is not "every method held".
func TestChainDirectOnlyDoesNotCountAllHeld(t *testing.T) {
	fx := &browserFixture{}
	f := newBrowserFetcher(fx, pageSession(func(string) string { return missingEnv(200, "unused") }))
	c, err := f.OpenChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	if _, err := c.FetchMoreChildren(context.Background(), "t3_abc123", []string{"x"}, "top"); err == nil {
		t.Fatal("want an error with no direct method")
	}
	if len(fx.outcomes) != 0 {
		t.Errorf("outcomes = %v, want none", fx.outcomes)
	}
}

// A browser listing with some posts missing their id keeps the others and is
// reported incomplete; with none, it is a layout error.
func TestBrowserListingChecks(t *testing.T) {
	two := `{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":"p1"}},{"kind":"t3","data":{"id":""}}]}}`
	none := `{"kind":"Listing","data":{"children":[{"kind":"t3","data":{"id":""}}]}}`
	if why, err := checkBrowserListing([]byte(two)); err != nil || why != browserIncomplete {
		t.Errorf("some blank: why=%q err=%v", why, err)
	}
	if _, err := checkBrowserListing([]byte(none)); err == nil {
		t.Error("all blank: want a layout error")
	}
}

// A dropped connection on a direct method used to come back raw, which
// stopped the chain: the next method was never tried.
func TestDirectTransportErrorFailsOver(t *testing.T) {
	fx := &chainFixture{status: map[string]int{}, hits: map[string]int{}}
	f := directFetcher(fx, roundTrip(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/v1/access_token" {
			return reply(r, 200, `{"access_token":"tok","expires_in":86400}`, nil)
		}
		if r.URL.Host == "oauth.reddit.com" {
			return nil // http.Client turns a nil response into an error
		}
		fx.hits[r.URL.Host]++
		return reply(r, 200, validListing, nil)
	}))
	if err := fetchThread(t, f); err != nil {
		t.Fatal(err)
	}
	if fx.hits["www.reddit.com"] != 1 || !strings.HasPrefix(fmt.Sprint(fx.outcomes), "[client_id=upstream_error") {
		t.Errorf("hits = %v outcomes = %v, want a failover to session", fx.hits, fx.outcomes)
	}
}

// A private subreddit is refused to every method: no retry, no failover, no
// hold, and the caller gets the reason.
func TestDirectPrivateSubredditIsFinal(t *testing.T) {
	fx := &chainFixture{status: map[string]int{}, hits: map[string]int{}}
	f := directFetcher(fx, roundTrip(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/v1/access_token" {
			return reply(r, 200, `{"access_token":"tok","expires_in":86400}`, nil)
		}
		fx.hits[r.URL.Host]++
		return reply(r, 403, `{"reason":"private","message":"Forbidden","error":403}`, nil)
	}))
	err := fetchThread(t, f)
	if err == nil || !strings.Contains(err.Error(), "private") || !domain.IsNoFallback(err) {
		t.Fatalf("err = %v, want a final refusal naming private", err)
	}
	if fx.hits["oauth.reddit.com"] != 1 || fx.hits["www.reddit.com"] != 0 || len(fx.states) != 0 {
		t.Errorf("hits = %v states = %v, want one call and no hold", fx.hits, fx.states)
	}
	if fmt.Sprint(fx.outcomes) != "[client_id=forbidden]" {
		t.Errorf("outcomes = %v", fx.outcomes)
	}
}

// A pod that starts while another replica holds a method reports it down at
// once, not at its first call.
func TestLoadSharedHolds(t *testing.T) {
	fx := &browserFixture{}
	f := newBrowserFetcher(fx, browserPages())
	f.shared = &fakeShared{holds: map[string]time.Duration{MethodBrowser: 5 * time.Minute}}
	f.LoadSharedHolds(context.Background())
	if len(fx.states) != 1 || !strings.HasPrefix(fx.states[0], "browser:false:held:") {
		t.Errorf("states = %v", fx.states)
	}
}

// directFetcher is newChainFetcher over a transport of the test's own.
func directFetcher(fx *chainFixture, rt http.RoundTripper) *Fetcher {
	return NewFetcher(FetcherConfig{
		ClientID: "app", SessionCookie: "sess", UserAgent: "omnifeed/test",
		HTTP: &http.Client{Transport: rt}, Cooldown: time.Hour,
		OnOutcome: func(m, o string) { fx.outcomes = append(fx.outcomes, m+"="+o) },
		OnState: func(m string, up bool, _ string, _ time.Duration) {
			fx.states = append(fx.states, fmt.Sprintf("%s:%v", m, up))
		},
	})
}

// The caller sees how a Reddit document was served and whether it is whole:
// reddit_method always, partial and partial_reason when the browser page held
// less than it counts. The body says so too, for agents that read only text.
func TestCrawlReportsMethodAndBrowserPartial(t *testing.T) {
	body := strings.Replace(browserThread, `"num_comments":1`, `"num_comments":300`, 1)
	e := New(Config{
		Fetcher:     NewFetcher(FetcherConfig{Browser: &fakeBrowser{name: "crawl4ai", session: pageSession(func(string) string { return envStr(200, body) })}}),
		Limiter:     httpx.NewDomainLimiter(2, 0),
		DefaultOpts: Options{Format: "json"},
	})
	doc, err := e.Crawl(context.Background(), "https://www.reddit.com/r/news/comments/abc123/t/", domain.EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Metadata[RedditMethodKey] != MethodBrowser {
		t.Errorf("reddit_method = %q", doc.Metadata[RedditMethodKey])
	}
	// One comment rendered out of 300 is not incomplete by the check (it only
	// flags none at all), so this document is whole.
	if doc.Metadata[domain.PartialKey] != "" {
		t.Errorf("partial = %q, want unset", doc.Metadata[domain.PartialKey])
	}

	head, _, _ := strings.Cut(body, `{"kind":"t1"`)
	empty := head + `]}}]`
	e = New(Config{
		Fetcher:     NewFetcher(FetcherConfig{Browser: &fakeBrowser{name: "crawl4ai", session: pageSession(func(string) string { return envStr(200, empty) })}}),
		Limiter:     httpx.NewDomainLimiter(2, 0),
		DefaultOpts: Options{Format: "json"},
	})
	doc, err = e.Crawl(context.Background(), "https://www.reddit.com/r/news/comments/abc123/t/", domain.EngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Metadata[domain.PartialKey] != "true" || doc.Metadata[domain.PartialReasonKey] != browserIncomplete {
		t.Errorf("meta = %v, want partial browser_incomplete", doc.Metadata)
	}
	if !strings.Contains(doc.PageContent, "only 0 of 300 comments could be loaded (browser_incomplete)") {
		t.Errorf("body lacks the note: %.200s", doc.PageContent)
	}
}
