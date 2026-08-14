package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/browser"
	"github.com/kinorai/omnifeed/internal/domain"
)

// --- fake browser backend ---

// fakeSession records Navigate/Eval calls and replies via evalFn, so the Reddit
// Fetcher's own logic (URL/JS shaping, envelope handling, session reuse) is
// tested without a real browser.
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
	opened  int
}

func (b *fakeBrowser) Name() string { return b.name }

func (b *fakeBrowser) Open(context.Context) (browser.Session, error) {
	b.opened++
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

func openSession(t *testing.T, b browser.Browser) *Session {
	t.Helper()
	f := NewFetcher(FetcherConfig{Browser: b})
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
	s := openSession(t, &fakeBrowser{name: "primary", session: sess})

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
	s := openSession(t, &fakeBrowser{name: "primary", session: sess})

	if _, err := s.FetchListing(context.Background(), "golang", "hot", 25, ""); err != nil {
		t.Fatal(err)
	}
	if len(sess.navs) != 1 || !strings.Contains(sess.navs[0], "/r/golang/hot/") {
		t.Errorf("navs = %v", sess.navs)
	}
	if !strings.Contains(sess.evals[0], "/r/golang/hot.json") {
		t.Errorf("listing JS missing the .json URL; js = %s", sess.evals[0])
	}
	if !strings.Contains(sess.evals[0], "limit=25") {
		t.Errorf("listing JS missing limit=25; js = %s", sess.evals[0])
	}
	// An empty time window must not leak a bare `&t=` into the URL: the query
	// ends right after raw_json=1. (The separators are JSON-escaped inside the
	// JS, so match on the query terminating rather than on a literal "&t=".)
	if !strings.Contains(sess.evals[0], `raw_json=1"`) {
		t.Errorf("listing JS should end at raw_json=1 with no t param; js = %s", sess.evals[0])
	}
}

// A listing URL's `?t=` / `?limit=` must reach Reddit's .json endpoint — without
// them, /r/selfhosted/top/?t=week silently returns top-of-day.
func TestFetchListingTimeWindow(t *testing.T) {
	sess := alwaysBody(`{"kind":"Listing","data":{"children":[]}}`)
	s := openSession(t, &fakeBrowser{name: "primary", session: sess})

	if _, err := s.FetchListing(context.Background(), "selfhosted", "top", 50, "week"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/r/selfhosted/top.json", "limit=50", "t=week"} {
		if !strings.Contains(sess.evals[0], want) {
			t.Errorf("listing JS missing %q; js = %s", want, sess.evals[0])
		}
	}
}

// FetchMoreChildren must reuse the exact thread page FetchThread navigated to,
// so the expansion POST runs from the page that cleared the bot wall.
func TestSessionReusesThreadPage(t *testing.T) {
	sess := alwaysBody(validListing)
	s := openSession(t, &fakeBrowser{name: "primary", session: sess})

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

// A whole crawl runs on ONE browser session: re-opening per fetch would leak a
// session per fetch and lose the recorded thread page.
func TestCrawlOpensOneSession(t *testing.T) {
	b := &fakeBrowser{name: "crawl4ai", session: alwaysBody(validListing)}
	s := openSession(t, b)
	if _, err := s.FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FetchMoreChildren(context.Background(), "t3_abc123", []string{"x1"}, "top"); err != nil {
		t.Fatal(err)
	}
	if b.opened != 1 {
		t.Fatalf("browser opened %d times across one crawl, want 1", b.opened)
	}
}

func TestResolveShareURL(t *testing.T) {
	canonical := "https://www.reddit.com/r/news/comments/abc123/title/?utm_source=share"
	sess := &fakeSession{evalFn: func(string) (string, error) { return canonical, nil }}
	s := openSession(t, &fakeBrowser{name: "crawl4ai", session: sess})

	got, err := s.ResolveShareURL(context.Background(), "https://www.reddit.com/r/news/s/abc")
	if err != nil || got != canonical {
		t.Fatalf("ResolveShareURL = %q, %v; want %q", got, err, canonical)
	}

	// A resolution that isn't a thread is an error.
	sess2 := &fakeSession{evalFn: func(string) (string, error) { return "https://www.reddit.com/r/news/", nil }}
	s2 := openSession(t, &fakeBrowser{name: "crawl4ai", session: sess2})
	if _, err := s2.ResolveShareURL(context.Background(), "https://www.reddit.com/r/news/s/abc"); err == nil {
		t.Error("expected error when share link resolves to a non-thread URL")
	}
}

// FetchMoreChildren without a prior FetchThread has no thread page to run the
// same-origin POST from — it must fail loudly, not fabricate a URL.
func TestFetchMoreChildrenRequiresFetchThread(t *testing.T) {
	s := openSession(t, &fakeBrowser{name: "primary", session: alwaysBody(validListing)})
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
