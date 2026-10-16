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

// openFirst opens the session of the fetcher's first chain method.
func openFirst(t *testing.T, f *Fetcher) *Session {
	t.Helper()
	s, err := f.methods[0].open(context.Background())
	if err != nil {
		t.Fatalf("open %s: %v", f.methods[0].name, err)
	}
	return s
}

func openSession(t *testing.T, b browser.Browser) *Session {
	t.Helper()
	return openFirst(t, NewFetcher(FetcherConfig{Browser: b}))
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
			got, _, err := unwrapEnvelope(tc.env)
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
	if len(sess.navs) != 1 || sess.navs[0] != "https://www.reddit.com/r/news/comments/abc123/some_title/?sort=new" {
		t.Errorf("FetchThread must navigate the sorted thread page; navs = %v", sess.navs)
	}
	// Reddit 403s logged-out .json, so the thread is read from the page itself.
	if strings.Contains(sess.evals[0], ".json") {
		t.Errorf("thread fetch must not request .json; js = %s", sess.evals[0])
	}
	if !strings.Contains(sess.evals[0], "LIMIT = 123,") {
		t.Errorf("thread fetch JS missing the comment limit; js = %s", sess.evals[0])
	}
}

func TestFetchListingShape(t *testing.T) {
	sess := alwaysBody(`{"kind":"Listing","data":{"children":[]}}`)
	s := openSession(t, &fakeBrowser{name: "primary", session: sess})

	if _, err := s.FetchListing(context.Background(), "golang", "hot", 25, ""); err != nil {
		t.Fatal(err)
	}
	// An empty time window must not leak a bare `?t=` into the page URL.
	if len(sess.navs) != 1 || sess.navs[0] != "https://www.reddit.com/r/golang/hot/?feedViewType=compactView" {
		t.Errorf("navs = %v", sess.navs)
	}
	if strings.Contains(sess.evals[0], ".json") {
		t.Errorf("listing fetch must not request .json; js = %s", sess.evals[0])
	}
	if !strings.Contains(sess.evals[0], "const LIMIT = 25;") {
		t.Errorf("listing JS missing the 25-post cap; js = %s", sess.evals[0])
	}
}

// A listing URL's `?t=` / `?limit=` must reach the page Reddit renders — without
// them, /r/selfhosted/top/?t=week silently returns top-of-day.
func TestFetchListingTimeWindow(t *testing.T) {
	sess := alwaysBody(`{"kind":"Listing","data":{"children":[]}}`)
	s := openSession(t, &fakeBrowser{name: "primary", session: sess})

	if _, err := s.FetchListing(context.Background(), "selfhosted", "top", 50, "week"); err != nil {
		t.Fatal(err)
	}
	if len(sess.navs) != 1 || sess.navs[0] != "https://www.reddit.com/r/selfhosted/top/?feedViewType=compactView&t=week" {
		t.Errorf("navs = %v", sess.navs)
	}
	if !strings.Contains(sess.evals[0], "const LIMIT = 50;") {
		t.Errorf("listing JS missing the 50-post cap; js = %s", sess.evals[0])
	}
}

// A whole crawl runs on ONE browser session: re-opening per fetch would leak a
// session per fetch.
func TestCrawlOpensOneSession(t *testing.T) {
	b := &fakeBrowser{name: "crawl4ai", session: browserPages()}
	c, err := NewFetcher(FetcherConfig{Browser: b}).OpenChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	if _, err := c.FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.FetchListing(context.Background(), "news", "hot", 25, ""); err != nil {
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

// A browser session refuses an expansion: Reddit refuses the logged-out call,
// so it must fail without sending anything.
func TestBrowserFetchMoreChildrenRefused(t *testing.T) {
	sess := alwaysBody(validListing)
	s := openSession(t, &fakeBrowser{name: "primary", session: sess})
	if _, err := s.FetchMoreChildren(context.Background(), "t3_abc", []string{"c1"}, "top"); err == nil {
		t.Fatal("expected an error from a browser expansion")
	}
	if len(sess.evals) != 0 {
		t.Errorf("browser ran %d snippets, want 0", len(sess.evals))
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
