package twitter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"github.com/kinorai/omnifeed/internal/fetchcache"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/observability"
)

// testKey is a fake RSSHub access key; no output may ever contain it.
const testKey = "synthetic-access-key-123"

// --- URL matching -------------------------------------------------------------

func TestParseTimelineTarget(t *testing.T) {
	for _, tc := range []struct {
		url    string
		kind   targetKind
		handle string
		query  string
	}{
		{"https://x.com/home", kindHome, "", ""},
		{"https://x.com/home/", kindHome, "", ""},
		{"https://twitter.com/home?f=for_you", kindHome, "", ""},
		{"https://mobile.x.com/home?f=following", kindFollowing, "", ""},
		{"https://www.twitter.com/home?f=following", kindFollowing, "", ""},
		{"https://x.com/example_dev", kindProfile, "example_dev", ""},
		{"https://fxtwitter.com/example_dev/", kindProfile, "example_dev", ""},
		{"https://x.com/Example_Dev?lang=en", kindProfile, "Example_Dev", ""},
		{"https://x.com/search?q=parser", kindSearch, "", "parser"},
		{"https://x.com/search?q=from%3Aexample_dev+since%3A2026-11-01+until%3A2026-11-20+min_faves%3A10&f=live",
			kindSearch, "", "from:example_dev since:2026-11-01 until:2026-11-20 min_faves:10"},
		{"https://vxtwitter.com/search/?q=%22queue+model%22", kindSearch, "", `"queue model"`},
	} {
		got, ok := parseTarget(tc.url)
		if !ok || got.kind != tc.kind || got.handle != tc.handle || got.query != tc.query {
			t.Errorf("parseTarget(%q) = %+v, %v; want kind %s handle %q query %q", tc.url, got, ok, tc.kind, tc.handle, tc.query)
		}
	}

	for _, u := range []string{
		"https://x.com/explore",                        // reserved
		"https://x.com/settings",                       // reserved
		"https://x.com/Notifications",                  // reserved, any case
		"https://x.com/i",                              // reserved
		"https://x.com/example_dev/with_replies",       // profile tab
		"https://x.com/example_dev/media",              // profile tab
		"https://x.com/home/extra",                     // not the home page
		"https://x.com/a_handle_far_too_long_for_x",    // over 15 characters
		"https://x.com/search?q=",                      // blank query
		"https://x.com/search?q=+++",                   // blank query
		"https://x.com/search",                         // no query
		"https://nitter.net/example_dev",               // not an X host
		"https://x.com.evil.example/example_dev",       // look-alike
		"https://x.com/",                               // root
		"https://bsky.app/profile/example.bsky.social", // other site
	} {
		if got, ok := parseTarget(u); ok {
			t.Errorf("parseTarget(%q) = %+v, true; want it to fall through", u, got)
		}
	}
}

// --- Fakes ----------------------------------------------------------------------

// rsshubFake serves one RSSHub route and records every request.
type rsshubFake struct {
	mu     sync.Mutex
	reqs   []*http.Request
	status int
	body   []byte
}

func newRSSHub(t *testing.T, status int, body []byte) (*rsshubFake, *httptest.Server) {
	f := &rsshubFake{status: status, body: body}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *rsshubFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.mu.Unlock()
	w.WriteHeader(f.status)
	_, _ = w.Write(f.body)
}

func (f *rsshubFake) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.reqs...)
}

// fxConv is a minimal FxTwitter /2/conversation answer for one post.
func fxConv(t *testing.T, id, handle, name, text string, likes, views int, replyTo string) []byte {
	t.Helper()
	status := map[string]any{
		"type": "status", "id": id, "url": "https://x.com/" + handle + "/status/" + id, "text": text,
		"author":     map[string]any{"screen_name": handle, "name": name},
		"created_at": "Fri Nov 20 10:00:00 +0000 2026", "likes": likes, "reposts": 2, "replies": 1, "views": views,
	}
	if replyTo != "" {
		status["replying_to"] = map[string]any{"screen_name": replyTo, "status": "1910000000000000002"}
	}
	b, err := json.Marshal(map[string]any{"code": 200, "status": status, "thread": []any{}, "replies": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// homePosts are the FxTwitter answers for the posts in rsshub_home.json.
func homePosts(t *testing.T) map[string]route {
	return map[string]route{
		"/2/conversation/1910000000000000001": ok(fxConv(t, "1910000000000000001", "example_dev", "Example Dev",
			"Shipping the new parser today.\n\nNotes: https://example.com/notes", 42, 1200, "")),
		"/2/conversation/1910000000000000002": ok(fxConv(t, "1910000000000000002", "demo_lab", "Demo Lab",
			"Queue models for telescope time & why FIFO loses", 310, 9800, "")),
		"/2/conversation/1910000000000000003": ok(fxConv(t, "1910000000000000003", "example_dev", "Example Dev",
			"@demo_lab Agreed, the queue model fits.", 5, 0, "demo_lab")),
	}
}

func newTimelineEngine(t *testing.T, posts, rsshub *httptest.Server, key string, logger *slog.Logger) *Engine {
	t.Helper()
	if logger == nil {
		logger = quiet
	}
	cfg := Config{Logger: logger, Client: httpx.New(nil), RSSHubKey: key}
	if posts != nil {
		cfg.FxTwitterURL, cfg.SyndicationURL, cfg.VxTwitterURL = posts.URL, posts.URL, posts.URL
	}
	if rsshub != nil {
		cfg.RSSHubURL = rsshub.URL + "/"
	}
	return New(cfg)
}

// --- Routes -------------------------------------------------------------------

// Each timeline URL asks its RSSHub route for JSON Feed, with the access key.
func TestTimelineRSSHubRoutes(t *testing.T) {
	_, posts := newUpstream(t, homePosts(t))
	for _, tc := range []struct{ url, path, kind string }{
		{"https://x.com/home", "/twitter/home", "home"},
		{"https://x.com/home?f=following", "/twitter/home_latest", "following"},
		{"https://twitter.com/example_dev", "/twitter/user/example_dev", "profile"},
		{"https://x.com/search?q=from%3Aexample_dev+min_faves%3A10", "/twitter/keyword/from:example_dev%20min_faves:10", "search"},
	} {
		fake, rss := newRSSHub(t, http.StatusOK, fixture(t, "rsshub_home.json"))
		doc, err := newTimelineEngine(t, posts, rss, testKey, nil).Crawl(context.Background(), tc.url, domain.EngineOptions{})
		if err != nil {
			t.Fatalf("%s: Crawl: %v", tc.url, err)
		}
		reqs := fake.requests()
		if len(reqs) != 1 {
			t.Fatalf("%s: rsshub requests = %d, want 1", tc.url, len(reqs))
		}
		r := reqs[0]
		if r.URL.EscapedPath() != tc.path || r.URL.Query().Get("format") != "json" || r.URL.Query().Get("key") != testKey {
			t.Errorf("%s: rsshub got %s, want path %s with format=json and the key", tc.url, r.URL.EscapedPath()+"?"+r.URL.RawQuery, tc.path)
		}
		if doc.Metadata["kind"] != tc.kind || doc.Metadata["upstream"] != "rsshub" || doc.Metadata["posts"] != "3" {
			t.Errorf("%s: meta = %v", tc.url, doc.Metadata)
		}
		mustNotContain(t, doc.PageContent+strings.Join(mapValues(doc.Metadata), " "), testKey)
	}
}

// No key configured: no key parameter.
func TestTimelineWithoutKey(t *testing.T) {
	_, posts := newUpstream(t, homePosts(t))
	fake, rss := newRSSHub(t, http.StatusOK, fixture(t, "rsshub_home.json"))
	if _, err := newTimelineEngine(t, posts, rss, "", nil).Crawl(context.Background(), "https://x.com/example_dev", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if q := fake.requests()[0].URL.Query(); q.Has("key") {
		t.Errorf("rsshub query = %v, want no key", q)
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// --- Hydration ----------------------------------------------------------------

// Posts keep RSSHub's order even when a later one loads first, a repost shows
// the original post, and a post reposted twice shows once.
func TestTimelineHydrationOrderAndReposts(t *testing.T) {
	routes := homePosts(t)
	slow := routes["/2/conversation/1910000000000000001"]
	slow.delay = 100 * time.Millisecond
	routes["/2/conversation/1910000000000000001"] = slow
	_, posts := newUpstream(t, routes)
	_, rss := newRSSHub(t, http.StatusOK, fixture(t, "rsshub_home.json"))
	doc, err := newTimelineEngine(t, posts, rss, "", nil).Crawl(context.Background(), "https://x.com/home",
		domain.EngineOptions{RedditFormat: "json", FormatExplicit: true})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	var tl Timeline
	if err := json.Unmarshal([]byte(doc.PageContent), &tl); err != nil {
		t.Fatalf("json: %v", err)
	}
	var got []string
	for _, p := range tl.Posts {
		got = append(got, p.Author+"/"+p.ID)
	}
	want := "example_dev/1910000000000000001,demo_lab/1910000000000000002,example_dev/1910000000000000003"
	if strings.Join(got, ",") != want {
		t.Errorf("posts = %v, want %s", got, want)
	}
	if tl.Posts[1].Likes != 310 || tl.Posts[1].Views != 9800 || tl.Posts[2].ReplyTo != "1910000000000000002" {
		t.Errorf("hydrated posts = %+v", tl.Posts)
	}
	if doc.Metadata[domain.PartialKey] != "" {
		t.Errorf("partial = %q on a fully loaded timeline", doc.Metadata[domain.PartialKey])
	}
}

// A post no source can load keeps the RSSHub item's text, author and date,
// with zero counts, and the document is marked partial.
func TestTimelineHydrationFailureKeepsMinimalPost(t *testing.T) {
	routes := homePosts(t)
	delete(routes, "/2/conversation/1910000000000000001")
	_, posts := newUpstream(t, routes)
	_, rss := newRSSHub(t, http.StatusOK, fixture(t, "rsshub_home.json"))
	e := newTimelineEngine(t, posts, rss, "", nil)

	doc, err := e.Crawl(context.Background(), "https://x.com/home", domain.EngineOptions{RedditFormat: "json", FormatExplicit: true})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata[domain.PartialKey] != "true" || doc.Metadata[domain.PartialReasonKey] == "" || doc.Metadata["unhydrated_posts"] != "1" {
		t.Errorf("meta = %v, want partial with a reason and 1 unhydrated post", doc.Metadata)
	}
	var tl Timeline
	if err := json.Unmarshal([]byte(doc.PageContent), &tl); err != nil {
		t.Fatalf("json: %v", err)
	}
	p := tl.Posts[0]
	want := OutPost{ID: "1910000000000000001", URL: "https://x.com/example_dev/status/1910000000000000001",
		Author: "example_dev", Name: "Example Dev", CreatedAt: "2026-11-20T10:00:00Z",
		Text: "Shipping the new parser today.\n\nNotes: example.com/notes"}
	if p.ID != want.ID || p.URL != want.URL || p.Author != want.Author || p.Name != want.Name ||
		p.CreatedAt != want.CreatedAt || p.Text != want.Text || p.Likes != 0 || p.Views != 0 {
		t.Errorf("minimal post = %+v, want %+v", p, want)
	}
	if len(tl.Posts) != 3 || tl.Posts[1].Likes != 310 {
		t.Errorf("other posts = %+v", tl.Posts)
	}

	// Markdown shows no counts for the post it could not load.
	md, err := e.Crawl(context.Background(), "https://x.com/home", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl markdown: %v", err)
	}
	mustContain(t, md.PageContent, "@example_dev (Example Dev) · 2026-11-20T10:00:00Z · https://x.com/example_dev/status/1910000000000000001\n")
}

// --- Errors -------------------------------------------------------------------

// Without RSSHub a timeline is a final error: crawl4ai would only get X's
// logged-out shell.
func TestTimelineWithoutRSSHubIsFinal(t *testing.T) {
	gen := &genericFake{}
	e := New(Config{Logger: quiet, Client: httpx.New(nil), Generic: gen})
	reg := engine.New().Register(e).Fallback(gen)
	for _, u := range []string{"https://x.com/home", "https://x.com/home?f=following", "https://x.com/example_dev", "https://x.com/search?q=parser"} {
		_, err := reg.Crawl(context.Background(), u, domain.EngineOptions{})
		if err == nil || !strings.Contains(err.Error(), "set OMNIFEED_TWITTER_RSSHUB_URL") {
			t.Errorf("%s: err = %v, want the RSSHub setup error", u, err)
		}
		if _, direct := e.Crawl(context.Background(), u, domain.EngineOptions{}); !domain.IsNoFallback(direct) {
			t.Errorf("%s: engine error %v is not final", u, direct)
		}
	}
	if len(gen.urls) != 0 {
		t.Errorf("fallback crawled %v", gen.urls)
	}
}

// RSSHub errors keep their kind, are final, are asked once, and never quote
// the access key.
func TestTimelineRSSHubErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		kind   domain.FailureKind
	}{
		{http.StatusServiceUnavailable, domain.KindUpstreamError},
		{http.StatusInternalServerError, domain.KindUpstreamError},
		{http.StatusForbidden, domain.KindHTTP403},
		{http.StatusNotFound, domain.KindNotFound},
		{http.StatusTooManyRequests, domain.KindHTTP429},
	} {
		var logs bytes.Buffer
		fake, rss := newRSSHub(t, tc.status, []byte("Authentication failed. Access denied."))
		e := newTimelineEngine(t, nil, rss, testKey, slog.New(slog.NewTextHandler(&logs, nil)))
		_, err := e.Crawl(context.Background(), "https://x.com/example_dev", domain.EngineOptions{})
		if !domain.IsNoFallback(err) {
			t.Errorf("%d: err %v is not final", tc.status, err)
		}
		fe, ok := errors.AsType[*domain.FetchError](err)
		if !ok || fe.Kind != tc.kind || fe.StatusCode != tc.status {
			t.Errorf("%d: err = %#v, want kind %s", tc.status, err, tc.kind)
		}
		if n := len(fake.requests()); n != 1 {
			t.Errorf("%d: rsshub asked %d times, want 1", tc.status, n)
		}
		mustNotContain(t, err.Error()+logs.String(), testKey)
	}
}

// A transport error quotes the request URL; the key is cut from it.
func TestTimelineTransportErrorRedactsKey(t *testing.T) {
	_, rss := newRSSHub(t, http.StatusOK, nil)
	rss.Close() // connection refused
	var logs bytes.Buffer
	e := newTimelineEngine(t, nil, rss, testKey, slog.New(slog.NewTextHandler(&logs, nil)))
	_, err := e.Crawl(context.Background(), "https://x.com/search?q=parser", domain.EngineOptions{})
	if err == nil || !domain.IsNoFallback(err) {
		t.Fatalf("err = %v, want a final error", err)
	}
	if fe, ok := errors.AsType[*domain.FetchError](err); !ok || fe.Kind != domain.KindUpstreamError {
		t.Errorf("err = %#v, want upstream_error", err)
	}
	mustContain(t, err.Error(), "/twitter/keyword/parser")
	mustNotContain(t, err.Error()+observability.Explain(err)+logs.String(), testKey, "key=")
}

func TestTimelineBadFeed(t *testing.T) {
	_, rss := newRSSHub(t, http.StatusOK, []byte("<rss>not json</rss>"))
	_, err := newTimelineEngine(t, nil, rss, "", nil).Crawl(context.Background(), "https://x.com/home", domain.EngineOptions{})
	if fe, ok := errors.AsType[*domain.FetchError](err); !ok || fe.Kind != domain.KindBadResponse || !domain.IsNoFallback(err) {
		t.Errorf("err = %v, want a final bad_response", err)
	}
}

// The RSSHub request goes through the per-domain limiter, without the key.
func TestTimelineLimiter(t *testing.T) {
	_, posts := newUpstream(t, homePosts(t))
	_, rss := newRSSHub(t, http.StatusOK, fixture(t, "rsshub_home.json"))
	lim := &countingLimiter{}
	e := New(Config{Logger: quiet, Client: httpx.New(nil), Limiter: lim, FxTwitterURL: posts.URL,
		SyndicationURL: posts.URL, VxTwitterURL: posts.URL, RSSHubURL: rss.URL, RSSHubKey: testKey})
	if _, err := e.Crawl(context.Background(), "https://x.com/home", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if len(lim.urls) != 4 || lim.urls[0] != rss.URL+"/twitter/home" {
		t.Errorf("limiter saw %v, want the RSSHub route then 3 posts", lim.urls)
	}
}

// --- Response cache -------------------------------------------------------------

// The home and following timelines are the RSSHub owner's feed: never in the
// shared response cache. Profiles and search are cached like posts.
func TestTimelinePrivacyInResponseCache(t *testing.T) {
	_, posts := newUpstream(t, homePosts(t))
	for _, tc := range []struct {
		url    string
		cached bool
	}{
		{"https://x.com/home", false},
		{"https://x.com/home?f=following", false},
		{"https://x.com/example_dev", true},
		{"https://x.com/search?q=parser", true},
	} {
		fake, rss := newRSSHub(t, http.StatusOK, fixture(t, "rsshub_home.json"))
		e := newTimelineEngine(t, posts, rss, "", nil)
		cache := fetchcache.New(fetchcache.Config{
			Inner:     engine.New().Register(e),
			Backend:   fetchcache.NewMemory(fetchcache.MemoryConfig{MaxBytes: 1 << 20}),
			EngineTTL: map[string]time.Duration{"twitter": time.Hour},
			Logger:    quiet,
		})
		var doc domain.Document
		for range 2 {
			var err error
			if doc, err = cache.Crawl(context.Background(), tc.url, domain.EngineOptions{}); err != nil {
				t.Fatalf("%s: Crawl: %v", tc.url, err)
			}
		}
		wantCalls, wantResult := 2, fetchcache.ResultMiss
		if tc.cached {
			wantCalls, wantResult = 1, fetchcache.ResultHit
		}
		if n := len(fake.requests()); n != wantCalls || doc.Metadata[domain.CacheKey] != wantResult {
			t.Errorf("%s: rsshub calls = %d, cache = %q; want %d, %q", tc.url, n, doc.Metadata[domain.CacheKey], wantCalls, wantResult)
		}
		if got := doc.Metadata[domain.PrivateKey] == "true"; got == tc.cached {
			t.Errorf("%s: private = %v", tc.url, got)
		}
	}
}

// --- Output -------------------------------------------------------------------

// The home timeline's markdown is pinned by a golden snapshot (go test
// -update rewrites it).
func TestTimelineMarkdownSnapshot(t *testing.T) {
	_, posts := newUpstream(t, homePosts(t))
	_, rss := newRSSHub(t, http.StatusOK, fixture(t, "rsshub_home.json"))
	doc, err := newTimelineEngine(t, posts, rss, "", nil).Crawl(context.Background(), "https://x.com/home", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata[domain.ContentTypeKey] != domain.ContentTypeMarkdown {
		t.Errorf("content_type = %q", doc.Metadata[domain.ContentTypeKey])
	}
	path := filepath.Join("testdata", "golden_home.md")
	if *update {
		if err := os.WriteFile(path, []byte(doc.PageContent), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run with -update to create): %v", err)
	}
	if doc.PageContent != string(want) {
		t.Errorf("golden_home.md drifted from the snapshot (go test -update to accept):\n%s", doc.PageContent)
	}
}

// The JSON shape another client parses: kind, posts with their counts; no
// handle or query on a home timeline.
func TestTimelineJSONShape(t *testing.T) {
	_, posts := newUpstream(t, homePosts(t))
	_, rss := newRSSHub(t, http.StatusOK, fixture(t, "rsshub_home.json"))
	doc, err := newTimelineEngine(t, posts, rss, "", nil).Crawl(context.Background(), "https://x.com/home?f=following",
		domain.EngineOptions{RedditFormat: "json", FormatExplicit: true})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata[domain.ContentTypeKey] != "json" {
		t.Errorf("content_type = %q", doc.Metadata[domain.ContentTypeKey])
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc.PageContent), &raw); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(raw) != 2 || string(raw["kind"]) != `"following"` || raw["posts"] == nil {
		t.Errorf("top-level keys = %s, want only kind=following and posts", doc.PageContent)
	}
	var list []map[string]any
	if err := json.Unmarshal(raw["posts"], &list); err != nil || len(list) != 3 {
		t.Fatalf("posts = %s (%v)", raw["posts"], err)
	}
	for _, k := range []string{"id", "url", "author", "name", "created_at", "text", "likes", "reposts", "replies", "views"} {
		if _, ok := list[1][k]; !ok {
			t.Errorf("post is missing %q: %v", k, list[1])
		}
	}

	toon, err := newTimelineEngine(t, posts, rss, "", nil).Crawl(context.Background(), "https://x.com/example_dev",
		domain.EngineOptions{RedditFormat: "toon", FormatExplicit: true})
	if err != nil {
		t.Fatalf("Crawl toon: %v", err)
	}
	mustContain(t, toon.PageContent, "kind: profile", "handle: example_dev", "posts[3]")
}
