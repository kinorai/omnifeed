package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/kinorai/omnifeed/internal/browser"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/observability"
)

// --- fake browser backend ---

// fakeSession records Navigate/Eval calls and replies via evalFn, so the Reddit
// Fetcher's own logic (URL/JS shaping, envelope handling, fallback, session
// reuse) is tested without a real browser.
type fakeSession struct {
	navs   []string
	evals  []string
	navErr error
	evalFn func(js string) (string, error)
	closed bool
}

func (s *fakeSession) Navigate(_ context.Context, url string) error {
	s.navs = append(s.navs, url)
	return s.navErr
}

func (s *fakeSession) Eval(_ context.Context, js string) (string, error) {
	s.evals = append(s.evals, js)
	if s.evalFn != nil {
		return s.evalFn(js)
	}
	return "", nil
}

func (s *fakeSession) Close(context.Context) error { s.closed = true; return nil }

type fakeBrowser struct {
	name    string
	session *fakeSession
	openErr error
	opened  int
}

func (b *fakeBrowser) Name() string { return b.name }

func (b *fakeBrowser) Open(context.Context) (browser.Session, error) {
	b.opened++
	if b.openErr != nil {
		return nil, b.openErr
	}
	return b.session, nil
}

// envStr builds the {s,b} envelope string an in-page snippet returns.
func envStr(status int, body string) string {
	b, _ := json.Marshal(fetchEnvelope{S: status, B: body})
	return string(b)
}

// alwaysBody makes a session whose Eval always returns a 200 envelope wrapping
// the given Reddit body.
func alwaysBody(body string) *fakeSession {
	return &fakeSession{evalFn: func(string) (string, error) { return envStr(200, body), nil }}
}

const validListing = `[{"kind":"Listing","data":{"children":[]}},{"kind":"Listing","data":{"children":[]}}]`

func openSession(t *testing.T, primary, fallback browser.Browser) *Session {
	t.Helper()
	f := NewFetcher(FetcherConfig{Primary: primary, Fallback: fallback, Metrics: observability.NewMetrics()})
	s, err := f.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// --- envelope classification ---

func TestUnwrapEnvelope(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		wantBody string
		wantErr  string
		wantKind domain.FailureKind
	}{
		{"200 valid json", envStr(200, validListing), validListing, "", ""},
		{"reddit 403", envStr(403, "<html>You've been blocked</html>"), "", "reddit returned 403", domain.KindHTTP403},
		{"network-security wall", envStr(200, "<html>You've been blocked by network security. Please prove you're a human.</html>"), "", "", domain.KindCaptcha},
		{"non-json body", envStr(200, "<html>not json</html>"), "", "not JSON", domain.KindBotBlock},
		{"garbage envelope", "not-an-envelope", "", "decode fetch envelope", domain.KindBadResponse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := unwrapEnvelope(tc.env)
			if tc.wantErr == "" && tc.wantKind == "" {
				if err != nil || got != tc.wantBody {
					t.Fatalf("got (%q, %v), want (%q, nil)", got, err, tc.wantBody)
				}
				return
			}
			if err == nil {
				t.Fatalf("want error, got body %q", got)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q missing %q", err, tc.wantErr)
			}
			var fe *domain.FetchError
			if !errors.As(err, &fe) || fe.Kind != tc.wantKind {
				t.Fatalf("want Kind %q, got %v", tc.wantKind, err)
			}
		})
	}
}

// --- request shaping ---

func TestFetchThreadShape(t *testing.T) {
	sess := alwaysBody(validListing)
	s := openSession(t, &fakeBrowser{name: "primary", session: sess}, nil)

	got, err := s.FetchThread(context.Background(), "/r/news/comments/abc123/some_title/", 123, 4, "new")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != validListing {
		t.Fatalf("body = %q", got)
	}
	if len(sess.navs) != 1 || !strings.Contains(sess.navs[0], "/r/news/comments/abc123/") {
		t.Errorf("FetchThread must navigate the thread page; navs = %v", sess.navs)
	}
	for _, want := range []string{"limit=123", "depth=4", "sort=new"} {
		if !strings.Contains(sess.evals[0], want) {
			t.Errorf("thread fetch JS missing %q; js = %s", want, sess.evals[0])
		}
	}
}

func TestFetchListingShape(t *testing.T) {
	sess := alwaysBody(`{"kind":"Listing","data":{"children":[]}}`)
	s := openSession(t, &fakeBrowser{name: "primary", session: sess}, nil)

	if _, err := s.FetchListing(context.Background(), "golang", "hot", 25); err != nil {
		t.Fatal(err)
	}
	if len(sess.navs) != 1 || !strings.Contains(sess.navs[0], "/r/golang/hot/") {
		t.Errorf("navs = %v", sess.navs)
	}
	if !strings.Contains(sess.evals[0], "/r/golang/hot.json") {
		t.Errorf("listing JS missing the .json URL; js = %s", sess.evals[0])
	}
}

// FetchMoreChildren must reuse the exact thread page FetchThread navigated to —
// this is what makes the live-page backend's second Navigate a no-op.
func TestSessionReusesThreadPage(t *testing.T) {
	sess := alwaysBody(validListing)
	s := openSession(t, &fakeBrowser{name: "primary", session: sess}, nil)

	if _, err := s.FetchThread(context.Background(), "/r/news/comments/abc123/some_title/", 500, 20, "top"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FetchMoreChildren(context.Background(), "t3_abc123", []string{"x1", "x2"}, "top"); err != nil {
		t.Fatal(err)
	}
	if len(sess.navs) != 2 || sess.navs[0] != sess.navs[1] {
		t.Fatalf("morechildren must re-navigate the same thread page; navs = %v", sess.navs)
	}
	if !strings.Contains(sess.evals[1], "morechildren") {
		t.Errorf("morechildren JS must POST /api/morechildren; js = %s", sess.evals[1])
	}
}

// A whole crawl runs on ONE browser session: re-opening per fetch would destroy
// live-page reuse and leak a CDP target per fetch.
func TestCrawlOpensOneSession(t *testing.T) {
	primary := &fakeBrowser{name: "primary", session: alwaysBody(validListing)}
	s := openSession(t, primary, nil)
	if _, err := s.FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FetchMoreChildren(context.Background(), "t3_abc123", []string{"x1"}, "top"); err != nil {
		t.Fatal(err)
	}
	if primary.opened != 1 {
		t.Fatalf("primary browser opened %d times across one crawl, want 1", primary.opened)
	}
}

func TestResolveShareURL(t *testing.T) {
	canonical := "https://www.reddit.com/r/news/comments/abc123/title/?utm_source=share"
	sess := &fakeSession{evalFn: func(string) (string, error) { return canonical, nil }}
	s := openSession(t, &fakeBrowser{name: "primary", session: sess}, nil)

	got, err := s.ResolveShareURL(context.Background(), "https://www.reddit.com/r/news/s/abc")
	if err != nil || got != canonical {
		t.Fatalf("ResolveShareURL = %q, %v; want %q", got, err, canonical)
	}

	// A resolution that isn't a thread is an error.
	sess2 := &fakeSession{evalFn: func(string) (string, error) { return "https://www.reddit.com/r/news/", nil }}
	s2 := openSession(t, &fakeBrowser{name: "primary", session: sess2}, nil)
	if _, err := s2.ResolveShareURL(context.Background(), "https://www.reddit.com/r/news/s/abc"); err == nil {
		t.Error("expected error when share link resolves to a non-thread URL")
	}
}

// --- fallback ---

// A block on the primary retries the same fetch on the fallback and returns its
// result, opening the fallback exactly once and recording the fallback in the
// metric.
func TestFetchFallsBackOnBlock(t *testing.T) {
	primarySess := &fakeSession{evalFn: func(string) (string, error) {
		return envStr(200, "<html>You've been blocked by network security</html>"), nil // → captcha
	}}
	fallbackSess := alwaysBody(validListing)
	primary := &fakeBrowser{name: "lightpanda", session: primarySess}
	fallback := &fakeBrowser{name: "crawl4ai", session: fallbackSess}
	metrics := observability.NewMetrics()
	f := NewFetcher(FetcherConfig{Primary: primary, Fallback: fallback, Metrics: metrics})
	s, err := f.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	got, err := s.FetchThread(context.Background(), "/r/news/comments/abc/t/", 500, 20, "top")
	if err != nil {
		t.Fatalf("expected fallback success, got %v", err)
	}
	if string(got) != validListing {
		t.Fatalf("fallback body = %q", got)
	}
	if fallback.opened != 1 {
		t.Errorf("fallback browser opened %d times, want 1", fallback.opened)
	}
	if !primarySess.closed {
		t.Error("primary session must be closed after falling back")
	}
	// A second fetch stays on the fallback (sticky) — the primary isn't retried.
	if _, err := s.FetchMoreChildren(context.Background(), "t3_abc", []string{"c1"}, "top"); err != nil {
		t.Fatal(err)
	}
	if fallback.opened != 1 {
		t.Errorf("fallback must be sticky (opened once), got %d", fallback.opened)
	}
	if len(primarySess.evals) != 1 {
		t.Errorf("primary must not be retried after fallback; primary evals = %d", len(primarySess.evals))
	}
	// The fallback is recorded in the metric, tagged with the primary's failure
	// reason — the operator-facing signal the README documents.
	fbCount := testutil.ToFloat64(metrics.BrowserFallbacks.WithLabelValues("lightpanda", "crawl4ai", "captcha"))
	if fbCount != 1 {
		t.Errorf("omnifeed_browser_fallback_total{lightpanda,crawl4ai,captcha} = %v, want 1", fbCount)
	}
}

// With no fallback configured, the primary's error surfaces unchanged.
func TestNoFallbackConfigured(t *testing.T) {
	primarySess := &fakeSession{evalFn: func(string) (string, error) {
		return envStr(403, "blocked"), nil
	}}
	s := openSession(t, &fakeBrowser{name: "crawl4ai", session: primarySess}, nil)

	_, err := s.FetchThread(context.Background(), "/r/news/comments/abc/t/", 500, 20, "top")
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindHTTP403 {
		t.Fatalf("want http_403 error, got %v", err)
	}
}

// Caller cancellation / the crawl's own timeout must NOT trigger fallback — the
// fallback would only exceed the same budget.
func TestNoFallbackOnCancel(t *testing.T) {
	primarySess := &fakeSession{evalFn: func(string) (string, error) {
		return "", &domain.FetchError{Kind: domain.KindCanceled, Err: context.Canceled}
	}}
	primary := &fakeBrowser{name: "lightpanda", session: primarySess}
	fallback := &fakeBrowser{name: "crawl4ai", session: alwaysBody(validListing)}
	s := openSession(t, primary, fallback)

	if _, err := s.FetchThread(context.Background(), "/r/news/comments/abc/t/", 500, 20, "top"); err == nil {
		t.Fatal("expected the canceled error to surface")
	}
	if fallback.opened != 0 {
		t.Errorf("cancellation must not fall back; fallback opened %d times", fallback.opened)
	}
}

func TestShouldFallback(t *testing.T) {
	yes := []error{
		// Fingerprint-shaped blocks and browser faults: a second browser can help.
		&domain.FetchError{Kind: domain.KindCaptcha},
		&domain.FetchError{Kind: domain.KindBotBlock},
		&domain.FetchError{Kind: domain.KindHTTP403},
		&domain.FetchError{Kind: domain.KindBadResponse},
		errors.New("dead websocket"),
	}
	for _, e := range yes {
		if !shouldFallback(e) {
			t.Errorf("shouldFallback(%v) = false, want true", e)
		}
	}
	no := []error{
		nil,
		// Caller cancellation / the crawl's own budget.
		context.Canceled,
		context.DeadlineExceeded,
		&domain.FetchError{Kind: domain.KindCanceled},
		&domain.FetchError{Kind: domain.KindTimeout},
		// Reddit's own answers through the browser: deterministic (404/5xx) or
		// IP-keyed (429) — a different browser gets the same answer.
		&domain.FetchError{Kind: domain.KindError, StatusCode: 404},
		&domain.FetchError{Kind: domain.KindHTTP429, StatusCode: 429},
		&domain.FetchError{Kind: domain.KindUpstreamError, StatusCode: 503},
	}
	for _, e := range no {
		if shouldFallback(e) {
			t.Errorf("shouldFallback(%v) = true, want false", e)
		}
	}
}

// Deterministic Reddit-side outcomes (a deleted thread's 404, a 429 rate limit)
// must surface as-is instead of being replayed through the fallback — the
// fallback shares the egress IP and can't change what Reddit said.
func TestNoFallbackOnRedditAnswer(t *testing.T) {
	for _, status := range []int{404, 429, 503} {
		primarySess := &fakeSession{evalFn: func(string) (string, error) {
			return envStr(status, "reddit says no"), nil
		}}
		primary := &fakeBrowser{name: "lightpanda", session: primarySess}
		fallback := &fakeBrowser{name: "crawl4ai", session: alwaysBody(validListing)}
		s := openSession(t, primary, fallback)

		_, err := s.FetchThread(context.Background(), "/r/news/comments/abc/t/", 500, 20, "top")
		var fe *domain.FetchError
		if !errors.As(err, &fe) || fe.StatusCode != status {
			t.Fatalf("status %d: want the envelope error to surface, got %v", status, err)
		}
		if fallback.opened != 0 {
			t.Errorf("status %d: reddit's own answer must not fall back; fallback opened %d times", status, fallback.opened)
		}
	}
}

// When the fallback browser itself fails to open, the PRIMARY's error must
// surface (log-and-keep contract) — not the open failure — and the session must
// not switch.
func TestFallbackOpenFailureKeepsPrimaryError(t *testing.T) {
	primarySess := &fakeSession{evalFn: func(string) (string, error) {
		return envStr(403, "blocked"), nil
	}}
	primary := &fakeBrowser{name: "lightpanda", session: primarySess}
	fallback := &fakeBrowser{name: "crawl4ai", openErr: errors.New("crawl4ai down")}
	s := openSession(t, primary, fallback)

	_, err := s.FetchThread(context.Background(), "/r/news/comments/abc/t/", 500, 20, "top")
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindHTTP403 {
		t.Fatalf("want the primary's http_403 to surface, got %v", err)
	}
	if strings.Contains(err.Error(), "crawl4ai down") {
		t.Errorf("open failure must not replace the primary error: %v", err)
	}
	if s.fellBack {
		t.Error("a failed fallback open must not mark the session as fallen back")
	}
	// The next fetch retries the fallback open (fellBack never latched).
	if _, err := s.FetchThread(context.Background(), "/r/news/comments/abc/t/", 500, 20, "top"); err == nil {
		t.Fatal("expected error")
	}
	if fallback.opened != 2 {
		t.Errorf("fallback open must be attempted per failing fetch; opened %d times", fallback.opened)
	}
}

// FetchMoreChildren without a prior FetchThread has no thread page to run the
// same-origin POST from — it must fail loudly, not fabricate a URL.
func TestFetchMoreChildrenRequiresFetchThread(t *testing.T) {
	s := openSession(t, &fakeBrowser{name: "primary", session: alwaysBody(validListing)}, nil)
	if _, err := s.FetchMoreChildren(context.Background(), "t3_abc", []string{"c1"}, "top"); err == nil {
		t.Fatal("expected error when FetchMoreChildren precedes FetchThread")
	}
}

// --- pure snippet helpers ---

func TestJSLit(t *testing.T) {
	for _, in := range []string{
		`https://www.reddit.com/r/x/comments/abc/t.json?a=1`,
		`evil"; fetch("http://attacker"); //`,
		`back\slash`,
		"tab\tand\nnewline",
		`</script><b>`,
	} {
		out := jsLit(in)
		var back string
		if err := json.Unmarshal([]byte(out), &back); err != nil || back != in {
			t.Errorf("jsLit(%q)=%q is not a round-tripping literal (err=%v back=%q)", in, out, err, back)
		}
	}
}

func TestGetPostJS(t *testing.T) {
	u := "https://www.reddit.com/r/x/comments/abc/t.json?raw_json=1"
	g := getJS(u)
	if !strings.Contains(g, "fetch("+jsLit(u)) {
		t.Errorf("getJS missing escaped url: %s", g)
	}
	if !strings.Contains(g, "JSON.stringify({s: r.status, b: await r.text()})") {
		t.Errorf("getJS missing envelope return: %s", g)
	}

	body := "api_type=json&children=a%2Cb&link_id=t3_abc"
	p := postJS("https://www.reddit.com/api/morechildren", body)
	if !strings.Contains(p, `"POST"`) {
		t.Errorf("postJS not a POST: %s", p)
	}
	if !strings.Contains(p, "body: "+jsLit(body)) {
		t.Errorf("postJS missing escaped form body: %s", p)
	}
}

func TestIsShareURL(t *testing.T) {
	share := []string{
		"https://www.reddit.com/r/news/s/abc123",
		"https://www.reddit.com/r/OpenWebUI/s/ibnxYbmeOE",
	}
	for _, u := range share {
		if !IsShareURL(u) {
			t.Errorf("IsShareURL(%q) = false, want true", u)
		}
	}
	notShare := []string{
		"https://www.reddit.com/r/news/comments/abc/t/",
		"https://www.reddit.com/r/news/",
	}
	for _, u := range notShare {
		if IsShareURL(u) {
			t.Errorf("IsShareURL(%q) = true, want false", u)
		}
	}
}
