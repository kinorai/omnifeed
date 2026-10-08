package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/observability"
)

var quiet = slog.New(slog.DiscardHandler)

var update = flag.Bool("update", false, "rewrite the golden snapshots in testdata/")

// --- URL matching -------------------------------------------------------------

func TestParseTarget(t *testing.T) {
	const id = "2105105417760391258"
	claim := []string{
		"https://x.com/tuakdotsol/status/" + id,
		"https://twitter.com/tuakdotsol/status/" + id,
		"https://www.twitter.com/tuakdotsol/status/" + id,
		"https://mobile.twitter.com/tuakdotsol/status/" + id,
		"https://www.x.com/tuakdotsol/status/" + id,
		"https://mobile.x.com/tuakdotsol/status/" + id,
		"http://x.com/tuakdotsol/status/" + id,
		"https://X.COM/tuakdotsol/status/" + id,
		"https://x.com/i/status/" + id,
		"https://x.com/i/web/status/" + id,
		"https://twitter.com/i/web/status/" + id,
		"https://x.com/tuakdotsol/status/" + id + "/",
		"https://x.com/tuakdotsol/status/" + id + "/photo/1",
		"https://x.com/tuakdotsol/status/" + id + "/video/2",
		"https://x.com/tuakdotsol/status/" + id + "?s=20&t=abc",
		"https://x.com/tuakdotsol/status/" + id + "/photo/1?s=46",
		"https://x.com/tuakdotsol/status/" + id + "#m",
		"https://twitter.com/tuakdotsol/statuses/" + id,
		"https://fxtwitter.com/tuakdotsol/status/" + id,
		"https://fixupx.com/tuakdotsol/status/" + id,
		"https://vxtwitter.com/tuakdotsol/status/" + id,
		"https://fixvx.com/tuakdotsol/status/" + id,
		"https://www.fxtwitter.com/i/status/" + id,
		"https://fixvx.com/tuakdotsol/status/" + id + "/video/1",
	}
	for _, u := range claim {
		got, ok := parseTarget(u)
		if !ok || got.shortLink || got.id != id {
			t.Errorf("parseTarget(%q) = %+v, %v; want id %s", u, got, ok, id)
		}
	}

	shortLinks := []string{"https://t.co/Ys7KQNsu5w", "http://t.co/abc123", "https://T.CO/abc123/"}
	for _, u := range shortLinks {
		if got, ok := parseTarget(u); !ok || !got.shortLink {
			t.Errorf("parseTarget(%q) = %+v, %v; want a t.co short link", u, got, ok)
		}
	}

	fallThrough := []string{
		"https://x.com/tuakdotsol",                          // profile
		"https://twitter.com/tuakdotsol/with_replies",       // profile tab
		"https://x.com/search?q=go",                         // search
		"https://x.com/i/lists/123",                         // list
		"https://x.com/tuakdotsol/status/",                  // no id
		"https://x.com/tuakdotsol/status/abc",               // non-numeric id
		"https://x.com/tuakdotsol/status/" + id + "/likes",  // engagement page
		"https://x.com/tuakdotsol/status/" + id + "/photo/", // incomplete suffix
		"https://x.com/a/b/status/" + id,                    // extra segment
		"https://nitter.net/tuakdotsol/status/" + id,        // not an X host
		"https://notx.com/tuakdotsol/status/" + id,          // look-alike
		"https://x.com.evil.example/tuakdotsol/status/" + id,
		"https://blog.twitter.com/tuakdotsol/status/" + id, // other subdomain
		"https://t.co/",              // no slug
		"https://t.co/abc/def",       // not a slug
		"ftp://x.com/u/status/" + id, // scheme
		"https://bsky.app/profile/x/post/y",
	}
	for _, u := range fallThrough {
		if got, ok := parseTarget(u); ok {
			t.Errorf("parseTarget(%q) = %+v, true; want it to fall through", u, got)
		}
	}
}

// --- Fakes ----------------------------------------------------------------------

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// upstream is a fake for all three APIs (and t.co): each path maps to a
// status and body. Unlisted paths answer 404 with FxTwitter's null status.
type upstream struct {
	t      *testing.T
	mu     sync.Mutex
	routes map[string]route
	hits   []string
}

type route struct {
	status   int
	body     []byte
	location string
}

func newUpstream(t *testing.T, routes map[string]route) (*upstream, *httptest.Server) {
	u := &upstream{t: t, routes: routes}
	srv := httptest.NewServer(u)
	t.Cleanup(srv.Close)
	return u, srv
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Path
	if r.URL.Path == "/tweet-result" {
		key += "?id=" + r.URL.Query().Get("id")
		if r.URL.Query().Get("token") == "" {
			u.t.Errorf("syndication called without a token: %s", r.URL)
		}
	}
	u.mu.Lock()
	u.hits = append(u.hits, r.Method+" "+key)
	rt, ok := u.routes[key]
	u.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":null,"thread":null,"author":null,"code":404}`))
		return
	}
	if rt.location != "" {
		w.Header().Set("Location", rt.location)
	}
	w.WriteHeader(rt.status)
	_, _ = w.Write(rt.body)
}

func (u *upstream) calls() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.hits...)
}

// genericFake stands in for the crawl4ai engine.
type genericFake struct {
	urls []string
	doc  domain.Document
	err  error
}

func (*genericFake) Name() string        { return "crawl4ai" }
func (*genericFake) Matches(string) bool { return false }
func (g *genericFake) Crawl(_ context.Context, rawURL string, _ domain.EngineOptions) (domain.Document, error) {
	g.urls = append(g.urls, rawURL)
	return g.doc, g.err
}

// countingLimiter records every URL the engine paces.
type countingLimiter struct {
	mu   sync.Mutex
	urls []string
}

func (l *countingLimiter) Acquire(_ context.Context, _, rawURL string) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.urls = append(l.urls, rawURL)
	return func() {}, nil
}

func newEngine(srv *httptest.Server, generic domain.Engine) *Engine {
	cfg := Config{
		Logger:         quiet,
		Client:         httpx.New(nil),
		FxTwitterURL:   srv.URL,
		SyndicationURL: srv.URL,
		VxTwitterURL:   srv.URL,
	}
	if generic != nil {
		cfg.Generic = generic
	}
	return New(cfg)
}

func ok(body []byte) route { return route{status: http.StatusOK, body: body} }

var failing = route{status: http.StatusServiceUnavailable, body: []byte("down")}

func mustContain(t *testing.T, doc string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(doc, w) {
			t.Errorf("output missing %q\n---\n%s", w, doc)
		}
	}
}

func mustNotContain(t *testing.T, doc string, nots ...string) {
	t.Helper()
	for _, n := range nots {
		if strings.Contains(doc, n) {
			t.Errorf("output unexpectedly contains %q", n)
		}
	}
}

// --- FxTwitter parsing -------------------------------------------------------------

// A plain post with an image and 35 replies: replies capped at the default
// 20, t.co links expanded, the auto-prepended @mention dropped.
func TestCrawlConversationReplies(t *testing.T) {
	const id = "2103890594137309425"
	up, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id: ok(fixture(t, "fx_conversation_replies.json")),
	})
	e := newEngine(srv, nil)
	doc, err := e.Crawl(context.Background(), "https://twitter.com/AliAbunimah/status/"+id+"/photo/1?s=20", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	mustContain(t, doc.PageContent,
		"@AliAbunimah (Ali Abunimah) · 2026-09-26T16:53:00Z · https://x.com/AliAbunimah/status/"+id,
		"Google Maps has been updated to show Zionist-holocausted Gaza. This is part of Rafah.\n\n[image]",
		"## Replies (20 of ",
		"- @GazeLeeward (1649 likes): They left the school icons up [image]",
		"- @Bratt_world (653 likes): This is what Apple Maps shows me still.\n  This is dystopian [image]",
	)
	mustNotContain(t, doc.PageContent, "https://t.co/", "(1649 likes): @AliAbunimah", "## Thread")
	if got := strings.Count(doc.PageContent, "\n- @"); got != 20 {
		t.Errorf("rendered %d replies, want 20", got)
	}
	m := doc.Metadata
	if m["upstream"] != "fxtwitter" || m["replies"] != "20" || m["text_truncated"] != "false" ||
		m[domain.ContentTypeKey] != domain.ContentTypeMarkdown || m["id"] != id || m["author"] != "AliAbunimah" {
		t.Errorf("metadata = %v", m)
	}
	if calls := up.calls(); len(calls) != 1 || calls[0] != "GET /2/conversation/"+id {
		t.Errorf("upstream calls = %v, want the conversation only (no self-thread)", calls)
	}

	// OMNIFEED_TWITTER_MAX_REPLIES raises the cap up to the first page (35).
	wide := New(Config{Logger: quiet, Client: httpx.New(nil), FxTwitterURL: srv.URL, MaxReplies: 100})
	doc, err = wide.Crawl(context.Background(), "https://x.com/i/status/"+id, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata["replies"] != "35" {
		t.Errorf("replies = %q, want the whole first page (35)", doc.Metadata["replies"])
	}
	mustContain(t, doc.PageContent, "hospital https://en.wikipedia.org/wiki/Tel_al-Sultan_attack")
}

// A long (note) post quoting another: full text, quote as a blockquote with
// its author and media.
func TestCrawlLongPostWithQuote(t *testing.T) {
	const id = "2105105417760391258"
	_, srv := newUpstream(t, map[string]route{"/2/conversation/" + id: ok(fixture(t, "fx_conversation_long_quote.json"))})
	doc, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/tuakdotsol/status/"+id, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	mustContain(t, doc.PageContent,
		"@tuakdotsol (marianne) · 2026-09-30T01:20:17Z · https://x.com/tuakdotsol/status/"+id+" · 6702 likes · 640 reposts · 93 replies · 600628 views",
		"how it works:\n\n1 | your preferences",
		"(or rather, all the tinders and hinges out there) 🍿",
		"Quoting:\n\n> @teo_kai_xiang (Kai) · 2026-09-29T08:15:26Z · https://x.com/teo_kai_xiang/status/2104847505221431730\n>\n> I CALLED IT.",
		"> [image] [image]",
	)
	if doc.Metadata["text_truncated"] != "false" {
		t.Errorf("text_truncated = %q, want false", doc.Metadata["text_truncated"])
	}
}

func TestCrawlImageAltText(t *testing.T) {
	const id = "1934702782558224447"
	_, srv := newUpstream(t, map[string]route{"/2/conversation/" + id: ok(fixture(t, "fx_conversation_alt.json"))})
	doc, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/NASAUniverse/status/"+id, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	mustContain(t, doc.PageContent,
		"[image: This composite image shows an open star cluster called the Coronet Cluster",
		"## Replies (3 of ",
	)
}

// The author's self-thread: detected from the conversation (the author's own
// replies), read from /2/thread, numbered, and kept out of the replies.
func TestCrawlSelfThread(t *testing.T) {
	const id = "2056793526755840187"
	up, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id: ok(fixture(t, "fx_conversation_thread_root.json")),
		"/2/thread/" + id:       ok(fixture(t, "fx_thread.json")),
	})
	doc, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/altryne/status/"+id, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	mustContain(t, doc.PageContent,
		"## Thread by @altryne (25 posts)",
		"\n1. (the linked post, above)",
		"\n2. 1/ Gemini 3.5 is the main headline.",
		"\n25. 24/ My read:",
		"## Replies (3 of ",
	)
	if doc.Metadata["thread_posts"] != "25" || doc.Metadata["replies"] != "3" {
		t.Errorf("metadata = %v, want 25 thread posts and 3 replies", doc.Metadata)
	}
	if got := up.calls(); len(got) != 2 || got[1] != "GET /2/thread/"+id {
		t.Errorf("upstream calls = %v, want conversation then thread", got)
	}
}

// /2/thread failing is not fatal: the chain visible in the conversation is
// rebuilt instead.
func TestCrawlSelfThreadFallsBackToConversationChain(t *testing.T) {
	const id = "2056793526755840187"
	_, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id: ok(fixture(t, "fx_conversation_thread_root.json")),
		"/2/thread/" + id:       failing,
	})
	doc, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/altryne/status/"+id, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata["thread_posts"] != "25" {
		t.Errorf("thread_posts = %q, want 25 rebuilt from the conversation", doc.Metadata["thread_posts"])
	}
}

// A mid-thread post: its own-author parents and own-author replies make up
// the chain, in order.
func TestThreadFromConversation(t *testing.T) {
	var th fxConversation
	if err := json.Unmarshal(fixture(t, "fx_thread.json"), &th); err != nil {
		t.Fatal(err)
	}
	all := th.Thread
	for _, k := range []int{0, 5, len(all) - 1} {
		root := all[k]
		parents, replies := all[:k], all[k+1:]
		if !isSelfThread(&root, parents, replies) {
			t.Errorf("post %d: isSelfThread = false", k)
		}
		chain := threadFromConversation(&root, parents, replies)
		if len(chain) != len(all) {
			t.Fatalf("post %d: chain has %d posts, want %d", k, len(chain), len(all))
		}
		for i := range chain {
			if chain[i].ID != all[i].ID {
				t.Fatalf("post %d: chain[%d] = %s, want %s", k, i, chain[i].ID, all[i].ID)
			}
		}
	}
	lone := all[0]
	if isSelfThread(&lone, nil, nil) {
		t.Error("a post with no parents or replies is not a thread")
	}
}

// An X Article: Draft.js blocks flattened to markdown.
func TestCrawlArticle(t *testing.T) {
	const id = "2011957172821737574"
	_, srv := newUpstream(t, map[string]route{"/2/conversation/" + id: ok(fixture(t, "fx_conversation_article.json"))})
	doc, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/XCreators/status/"+id, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	mustContain(t, doc.PageContent,
		"## Article: The ultimate guide to Articles on X",
		"You’ve mastered the art of 280-character posting",
		"### Writing a great Article:",
		"- What do I want the reader to think, feel, or do after reading this?\n- ",
		"following link on your post: https://x.com/YOURUSERNAME/creator-subscriptions/subscribe.",
		"[embedded post: https://x.com/i/status/2004900241745883205]",
		"[image]",
	)
	if strings.Count(doc.PageContent, "\n### ") < 3 {
		t.Errorf("expected several article headings:\n%s", doc.PageContent)
	}
}

// Draft.js link offsets count UTF-16 units: an emoji before the link must
// not shift it.
func TestApplyLinksUTF16(t *testing.T) {
	var blk articleBlock
	if err := json.Unmarshal([]byte(`{"type":"unstyled","text":"🚀 see the docs now",
		"entityRanges":[{"key":0,"offset":7,"length":8}]}`), &blk); err != nil {
		t.Fatal(err)
	}
	var en articleEntity
	if err := json.Unmarshal([]byte(`{"key":"0","value":{"type":"LINK","data":{"url":"https://example.com/docs"}}}`), &en); err != nil {
		t.Fatal(err)
	}
	got := applyLinks(blk, map[string]articleEntity{"0": en})
	if want := "🚀 see [the docs](https://example.com/docs) now"; got != want {
		t.Errorf("applyLinks = %q, want %q", got, want)
	}
}

// Poll, video with duration, gif with alt, community note, link card,
// deleted quote (synthetic fixture built on the documented FxTwitter v2
// schema — no recorded post carried all of these).
func TestCrawlPollVideoNoteCard(t *testing.T) {
	const id = "1900000000000000001"
	_, srv := newUpstream(t, map[string]route{"/2/conversation/" + id: ok(fixture(t, "fx_conversation_synthetic.json"))})
	doc, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/i/status/"+id, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	mustContain(t, doc.PageContent,
		"Which telescope should we point next? Details: https://example.com/vote",
		"[video, 1:23] [gif: a spinning galaxy]",
		"> [quoted post unavailable: This post was deleted by the post author.]",
		"**Poll** (1234 votes, Final results):\n- Chandra: 60.1% (742 votes)\n- Webb: 39.9% (492 votes)",
		"**Community note:** The vote is not binding",
		"**Link:** [Vote for the next target](https://example.com/vote)",
	)
	mustNotContain(t, doc.PageContent, "t.co/")
}

// --- Output formats -------------------------------------------------------------

// Markdown output is pinned by a golden snapshot (go test -update rewrites it).
func TestMarkdownSnapshot(t *testing.T) {
	for _, tc := range []struct{ id, fixture, golden string }{
		{"2105105417760391258", "fx_conversation_long_quote.json", "golden_long_quote.md"},
		{"1900000000000000001", "fx_conversation_synthetic.json", "golden_synthetic.md"},
	} {
		_, srv := newUpstream(t, map[string]route{"/2/conversation/" + tc.id: ok(fixture(t, tc.fixture))})
		doc, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/i/status/"+tc.id, domain.EngineOptions{})
		if err != nil {
			t.Fatalf("Crawl: %v", err)
		}
		path := filepath.Join("testdata", tc.golden)
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
			t.Errorf("%s drifted from the snapshot (go test -update to accept):\n%s", tc.golden, doc.PageContent)
		}
	}
}

// format=toon|json returns the structured shape; content_type follows.
func TestStructuredFormats(t *testing.T) {
	const id = "2105105417760391258"
	_, srv := newUpstream(t, map[string]route{"/2/conversation/" + id: ok(fixture(t, "fx_conversation_long_quote.json"))})
	e := newEngine(srv, nil)

	doc, err := e.Crawl(context.Background(), "https://x.com/i/status/"+id, domain.EngineOptions{RedditFormat: "json", FormatExplicit: true})
	if err != nil {
		t.Fatalf("Crawl json: %v", err)
	}
	var out Output
	if err := json.Unmarshal([]byte(doc.PageContent), &out); err != nil {
		t.Fatalf("json output does not decode: %v", err)
	}
	if out.Post.Author != "tuakdotsol" || out.Post.Quote == nil || out.Post.Quote.Author != "teo_kai_xiang" ||
		len(out.Replies) != 3 || out.Source != "fxtwitter" || out.Truncated || doc.Metadata[domain.ContentTypeKey] != "json" {
		t.Errorf("json output = %+v (content_type %q)", out, doc.Metadata[domain.ContentTypeKey])
	}

	doc, err = e.Crawl(context.Background(), "https://x.com/i/status/"+id, domain.EngineOptions{RedditFormat: "toon", FormatExplicit: true})
	if err != nil {
		t.Fatalf("Crawl toon: %v", err)
	}
	if doc.Metadata[domain.ContentTypeKey] != domain.ContentTypeTOON {
		t.Errorf("content_type = %q, want toon", doc.Metadata[domain.ContentTypeKey])
	}
	mustContain(t, doc.PageContent, "post:", "author: tuakdotsol", "source: fxtwitter", "replies[3]")
}

// --- Fallback chain ----------------------------------------------------------------

// FxTwitter down, syndication serves a cut long post, vxTwitter supplies its
// full text.
func TestFallbackSyndicationPlusVxFullText(t *testing.T) {
	const id = "2105105417760391258"
	up, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id:  failing,
		"/tweet-result?id=" + id: ok(fixture(t, "synd_long_quote.json")),
		"/Twitter/status/" + id:  ok(fixture(t, "vx_long_quote.json")),
	})
	doc, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/tuakdotsol/status/"+id, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata["upstream"] != "syndication+vxtwitter" || doc.Metadata["text_truncated"] != "false" {
		t.Errorf("metadata = %v", doc.Metadata)
	}
	mustContain(t, doc.PageContent,
		"(or rather, all the tinders and hinges out there) 🍿",
		"> @teo_kai_xiang (Kai) · 2026-09-29T08:15:26Z",
		"> [image] [image]",
	)
	mustNotContain(t, doc.PageContent, "Text truncated")
	want := []string{
		"GET /2/conversation/" + id, "GET /2/conversation/" + id, // one retry on 503
		"GET /tweet-result?id=" + id, "GET /Twitter/status/" + id,
	}
	if got := up.calls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("call order = %v, want %v", got, want)
	}
}

// vxTwitter down too: the cut syndication text is served, flagged truncated.
func TestFallbackSyndicationTruncated(t *testing.T) {
	const id = "2105105417760391258"
	_, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id:  failing,
		"/tweet-result?id=" + id: ok(fixture(t, "synd_long_quote.json")),
		"/Twitter/status/" + id:  failing,
	})
	doc, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/tuakdotsol/status/"+id, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata["upstream"] != "syndication" || doc.Metadata["text_truncated"] != "true" {
		t.Errorf("metadata = %v", doc.Metadata)
	}
	mustContain(t, doc.PageContent, "**Text truncated:** only the first ~275 characters", "1 | your")

	doc, err = newEngine(srv, nil).Crawl(context.Background(), "https://x.com/i/status/"+id, domain.EngineOptions{RedditFormat: "json", FormatExplicit: true})
	if err != nil {
		t.Fatalf("Crawl json: %v", err)
	}
	mustContain(t, doc.PageContent, `"truncated":true`, `"source":"syndication"`)
}

// Syndication carries alt text (ext_alt_text) for a short post: not truncated.
func TestFallbackSyndicationAltText(t *testing.T) {
	const id = "1934702782558224447"
	up, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id:  {status: http.StatusTooManyRequests, body: []byte("slow down")},
		"/tweet-result?id=" + id: ok(fixture(t, "synd_alt.json")),
	})
	doc, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/NASAUniverse/status/"+id, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata["upstream"] != "syndication" || doc.Metadata["text_truncated"] != "false" {
		t.Errorf("metadata = %v", doc.Metadata)
	}
	mustContain(t, doc.PageContent, "@NASAUniverse", "[image: This composite image shows an open star cluster")
	mustNotContain(t, doc.PageContent, "https://t.co/")
	for _, c := range up.calls() {
		if strings.HasPrefix(c, "GET /Twitter/") {
			t.Errorf("vxTwitter called for a post syndication served whole: %v", up.calls())
		}
	}
}

func TestFallbackVxOnly(t *testing.T) {
	const id = "2105105417760391258"
	_, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id:  failing,
		"/tweet-result?id=" + id: failing,
		"/Twitter/status/" + id:  ok(fixture(t, "vx_long_quote.json")),
	})
	doc, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/i/status/"+id, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata["upstream"] != "vxtwitter" {
		t.Errorf("upstream = %q, want vxtwitter", doc.Metadata["upstream"])
	}
	mustContain(t, doc.PageContent, "hinges out there) 🍿", "> @teo_kai_xiang (Kai)")
}

// Every API down: the generic engine renders the canonical x.com URL — never
// twitter.com or the mirror the caller pasted.
func TestFallbackGenericOnCanonicalURL(t *testing.T) {
	const id = "2105105417760391258"
	_, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id:  failing,
		"/tweet-result?id=" + id: failing,
		"/Twitter/status/" + id:  failing,
	})
	gen := &genericFake{doc: domain.Document{
		PageContent: "marianne on X: \"Singapore's govt dating app runs on a Nobel Prize-winning game theory algorithm\" …",
		Metadata:    map[string]string{"source": "https://x.com/i/status/" + id},
	}}
	doc, err := newEngine(srv, gen).Crawl(context.Background(), "https://fxtwitter.com/tuakdotsol/status/"+id, domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if len(gen.urls) != 1 || gen.urls[0] != "https://x.com/i/status/"+id {
		t.Errorf("generic crawled %v, want the canonical x.com URL", gen.urls)
	}
	if doc.Metadata["upstream"] != "crawl4ai" {
		t.Errorf("upstream = %q, want crawl4ai", doc.Metadata["upstream"])
	}
}

// FxTwitter and syndication both say 404: a classified not-found error,
// final (no browser render), never a near-empty document.
func TestNotFound(t *testing.T) {
	const id = "1999999999999999999"
	_, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id: {status: http.StatusNotFound, body: fixture(t, "fx_missing.json")},
	})
	gen := &genericFake{}
	_, err := newEngine(srv, gen).Crawl(context.Background(), "https://x.com/someone/status/"+id, domain.EngineOptions{})
	if err == nil {
		t.Fatal("Crawl = nil error, want not found")
	}
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.StatusCode != http.StatusNotFound || !domain.IsNoFallback(err) {
		t.Fatalf("err = %v, want a final 404 FetchError", err)
	}
	if !strings.Contains(err.Error(), "tweet unavailable: not found") {
		t.Errorf("message = %q", err.Error())
	}
	if len(gen.urls) != 0 {
		t.Errorf("generic crawled %v for a post two sources call missing", gen.urls)
	}
	if got := observability.Explain(err); !strings.Contains(got, "HTTP 404") || !strings.Contains(got, "not found") {
		t.Errorf("Explain = %q", got)
	}
}

// One source's 404 alone is not trusted: FxTwitter 404 + syndication 5xx
// still tries the remaining sources.
func TestSingleNotFoundIsNotFinal(t *testing.T) {
	const id = "2105105417760391258"
	_, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id:  {status: http.StatusNotFound, body: fixture(t, "fx_missing.json")},
		"/tweet-result?id=" + id: failing,
		"/Twitter/status/" + id:  ok(fixture(t, "vx_long_quote.json")),
	})
	doc, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/i/status/"+id, domain.EngineOptions{})
	if err != nil || doc.Metadata["upstream"] != "vxtwitter" {
		t.Fatalf("Crawl = %v, %v; want vxtwitter to serve it", err, doc.Metadata)
	}
}

// Every source blocked or rate-limited: the error says so, with the 429 kind.
func TestBlockedEverywhere(t *testing.T) {
	const id = "2105105417760391258"
	limited := route{status: http.StatusTooManyRequests, body: []byte("rate limited")}
	_, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id:  limited,
		"/tweet-result?id=" + id: {status: http.StatusForbidden, body: []byte("no")},
		"/Twitter/status/" + id:  limited,
	})
	gen := &genericFake{err: &domain.FetchError{Kind: domain.KindCaptcha, Marker: "Something went wrong"}}
	_, err := newEngine(srv, gen).Crawl(context.Background(), "https://x.com/i/status/"+id, domain.EngineOptions{})
	if err == nil {
		t.Fatal("Crawl = nil error")
	}
	if got := observability.Reason(err); got != string(domain.KindHTTP429) {
		t.Errorf("Reason = %q, want http_429", got)
	}
	if !domain.IsNoFallback(err) || !strings.Contains(err.Error(), "tweet unavailable: blocked or rate-limited by every source") {
		t.Errorf("err = %v", err)
	}
	mustContain(t, err.Error(), "fxtwitter: http_429", "syndication: http_403", "vxtwitter: http_429", "crawl4ai: captcha")
}

// A generic render that is X's error shell is a failure, not a document.
func TestGenericThinPageRejected(t *testing.T) {
	const id = "2105105417760391258"
	_, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id:  failing,
		"/tweet-result?id=" + id: failing,
		"/Twitter/status/" + id:  failing,
	})
	gen := &genericFake{doc: domain.Document{PageContent: "Hmm...this page doesn’t exist. Try searching for something else."}}
	_, err := newEngine(srv, gen).Crawl(context.Background(), "https://x.com/i/status/"+id, domain.EngineOptions{})
	if err == nil || !domain.IsNoFallback(err) {
		t.Fatalf("err = %v, want a final error", err)
	}
	if got := observability.Reason(err); got != string(domain.KindThinContent) {
		t.Errorf("Reason = %q, want thin_content (the last source's verdict)", got)
	}
}

// Without a generic engine the error is NOT final, so the registry's own
// fallback still gets its turn.
func TestNoGenericLeavesFallbackToRegistry(t *testing.T) {
	const id = "2105105417760391258"
	_, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id:  failing,
		"/tweet-result?id=" + id: failing,
		"/Twitter/status/" + id:  failing,
	})
	_, err := newEngine(srv, nil).Crawl(context.Background(), "https://x.com/i/status/"+id, domain.EngineOptions{})
	if err == nil || domain.IsNoFallback(err) {
		t.Fatalf("err = %v, want a non-final error", err)
	}
	if got := observability.Reason(err); got != string(domain.KindUpstreamError) {
		t.Errorf("Reason = %q, want upstream_error", got)
	}
}

// Wired into the registry, a not-found post does not reach the browser
// fallback.
func TestRegistryDoesNotRerenderMissingPost(t *testing.T) {
	const id = "1999999999999999999"
	_, srv := newUpstream(t, map[string]route{})
	gen := &genericFake{}
	reg := engine.New().Register(newEngine(srv, gen)).Fallback(gen)
	if _, err := reg.Crawl(context.Background(), "https://x.com/someone/status/"+id, domain.EngineOptions{}); err == nil {
		t.Fatal("registry Crawl = nil error")
	}
	if len(gen.urls) != 0 {
		t.Errorf("fallback crawled %v", gen.urls)
	}
}

// --- Cache, limiter ------------------------------------------------------------------

func TestCacheServesRepeatReads(t *testing.T) {
	const id = "2103890594137309425"
	up, srv := newUpstream(t, map[string]route{"/2/conversation/" + id: ok(fixture(t, "fx_conversation_replies.json"))})
	e := newEngine(srv, nil)
	for _, f := range []string{"", "toon", ""} {
		if _, err := e.Crawl(context.Background(), "https://x.com/i/status/"+id, domain.EngineOptions{RedditFormat: f, FormatExplicit: f != ""}); err != nil {
			t.Fatalf("Crawl: %v", err)
		}
	}
	if n := len(up.calls()); n != 1 {
		t.Errorf("upstream called %d times for three reads, want 1", n)
	}
}

func TestCacheExpires(t *testing.T) {
	c := newTTLCache(cacheTTL, 2)
	now := c.now()
	c.now = func() time.Time { return now }
	c.put("a", bundle{Source: "x"})
	if _, ok := c.get("a"); !ok {
		t.Fatal("fresh entry missing")
	}
	c.now = func() time.Time { return now.Add(cacheTTL + time.Second) }
	if _, ok := c.get("a"); ok {
		t.Fatal("expired entry served")
	}
	c.now = func() time.Time { return now }
	c.put("a", bundle{})
	c.put("b", bundle{})
	c.put("c", bundle{})
	if len(c.entries) > 2 {
		t.Fatalf("cache grew to %d entries, cap 2", len(c.entries))
	}
}

// Every upstream request goes through the per-domain limiter, keyed by its URL.
func TestLimiterPacesEveryRequest(t *testing.T) {
	const id = "2056793526755840187"
	_, srv := newUpstream(t, map[string]route{
		"/2/conversation/" + id: ok(fixture(t, "fx_conversation_thread_root.json")),
		"/2/thread/" + id:       ok(fixture(t, "fx_thread.json")),
	})
	lim := &countingLimiter{}
	e := New(Config{Logger: quiet, Client: httpx.New(nil), Limiter: lim, FxTwitterURL: srv.URL, SyndicationURL: srv.URL, VxTwitterURL: srv.URL})
	if _, err := e.Crawl(context.Background(), "https://x.com/i/status/"+id, domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	want := []string{srv.URL + "/2/conversation/" + id, srv.URL + "/2/thread/" + id}
	if strings.Join(lim.urls, ",") != strings.Join(want, ",") {
		t.Errorf("limiter saw %v, want %v", lim.urls, want)
	}
}

// --- t.co --------------------------------------------------------------------------

// redirectTransport sends every request to the fake, whatever its host, so
// t.co itself can be faked.
type redirectTransport struct{ target *url.URL }

func (rt redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host = rt.target.Scheme, rt.target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

func newShortLinkEngine(t *testing.T, routes map[string]route, gen domain.Engine, blockPrivate bool) (*upstream, *Engine) {
	up, srv := newUpstream(t, routes)
	target, _ := url.Parse(srv.URL)
	client := httpx.New(&http.Client{Transport: redirectTransport{target}})
	return up, New(Config{
		Logger: quiet, Client: client, Generic: gen, BlockPrivateIPs: blockPrivate,
		FxTwitterURL: srv.URL, SyndicationURL: srv.URL, VxTwitterURL: srv.URL,
	})
}

// A t.co link to a post is resolved with one HEAD (redirect not followed) and
// rendered as that post.
func TestShortLinkToPost(t *testing.T) {
	const id = "2103890594137309425"
	up, e := newShortLinkEngine(t, map[string]route{
		"/Ys7KQNsu5w":           {status: http.StatusMovedPermanently, location: "https://x.com/AliAbunimah/status/" + id + "/photo/1"},
		"/2/conversation/" + id: ok(fixture(t, "fx_conversation_replies.json")),
	}, nil, false)
	doc, err := e.Crawl(context.Background(), "https://t.co/Ys7KQNsu5w", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata["id"] != id || doc.Metadata["source"] != "https://t.co/Ys7KQNsu5w" {
		t.Errorf("metadata = %v", doc.Metadata)
	}
	want := []string{"HEAD /Ys7KQNsu5w", "GET /2/conversation/" + id}
	if got := up.calls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v, want %v (HEAD, no redirect followed)", got, want)
	}
}

// A t.co link elsewhere goes to the generic engine, final either way.
func TestShortLinkElsewhereGoesGeneric(t *testing.T) {
	gen := &genericFake{doc: domain.Document{PageContent: "an article"}}
	_, e := newShortLinkEngine(t, map[string]route{
		"/abc123": {status: http.StatusMovedPermanently, location: "https://example.com/article?ref=x"},
	}, gen, false)
	doc, err := e.Crawl(context.Background(), "https://t.co/abc123", domain.EngineOptions{})
	if err != nil || doc.PageContent != "an article" {
		t.Fatalf("Crawl = %q, %v", doc.PageContent, err)
	}
	if len(gen.urls) != 1 || gen.urls[0] != "https://example.com/article?ref=x" {
		t.Errorf("generic crawled %v", gen.urls)
	}

	gen.err = &domain.FetchError{Kind: domain.KindBotBlock}
	if _, err := e.Crawl(context.Background(), "https://t.co/abc123", domain.EngineOptions{}); !domain.IsNoFallback(err) {
		t.Errorf("err = %v, want final (the generic engine already ran)", err)
	}
}

// The SSRF guard vets the t.co destination before anything fetches it.
func TestShortLinkToPrivateAddressRejected(t *testing.T) {
	gen := &genericFake{}
	_, e := newShortLinkEngine(t, map[string]route{
		"/evil1": {status: http.StatusFound, location: "http://169.254.169.254/latest/meta-data"},
	}, gen, true)
	_, err := e.Crawl(context.Background(), "https://t.co/evil1", domain.EngineOptions{})
	if err == nil || !strings.Contains(err.Error(), "t.co destination rejected") {
		t.Fatalf("err = %v, want the destination rejected", err)
	}
	if len(gen.urls) != 0 {
		t.Errorf("generic crawled %v", gen.urls)
	}
}

func TestShortLinkWithoutRedirect(t *testing.T) {
	_, e := newShortLinkEngine(t, map[string]route{"/nothing": ok([]byte("<html>"))}, nil, false)
	if _, err := e.Crawl(context.Background(), "https://t.co/nothing", domain.EngineOptions{}); err == nil {
		t.Fatal("Crawl = nil error for a t.co answer with no redirect")
	}
}

// --- Small helpers -----------------------------------------------------------------

func TestCleanTextAndDuration(t *testing.T) {
	if got := cleanText("@a @b_c  hello @d", true); got != "hello @d" {
		t.Errorf("cleanText reply = %q", got)
	}
	if got := cleanText("@a hello", false); got != "@a hello" {
		t.Errorf("cleanText non-reply = %q", got)
	}
	if got := cleanText("@a @b", true); got != "@a @b" {
		t.Errorf("cleanText mention-only reply = %q, want kept", got)
	}
	for sec, want := range map[float64]string{0.4: "0:00", 83.4: "1:23", 600: "10:00", 3725: "1:02:05"} {
		if got := duration(sec); got != want {
			t.Errorf("duration(%v) = %q, want %q", sec, got, want)
		}
	}
}
