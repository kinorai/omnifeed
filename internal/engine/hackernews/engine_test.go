package hackernews

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// Matches must claim /item?id=N threads and the supported feed paths, and fall
// through for anything else on the host (so the generic fallback handles it).
func TestMatches(t *testing.T) {
	e := &Engine{}

	claim := []string{
		"https://news.ycombinator.com/",
		"https://news.ycombinator.com/news",
		"https://news.ycombinator.com/newest",
		"https://news.ycombinator.com/ask",
		"https://news.ycombinator.com/show",
		"https://news.ycombinator.com/item?id=43307229",
		"https://news.ycombinator.com/item/?id=43307229", // trailing slash
		"https://news.ycombinator.com/news/",             // trailing slash
	}
	for _, u := range claim {
		if !e.Matches(u) {
			t.Errorf("Matches(%q) = false, want true", u)
		}
	}

	fallThrough := []string{
		"https://news.ycombinator.com/item",        // no id
		"https://news.ycombinator.com/item?id=abc", // non-numeric id
		"https://news.ycombinator.com/user?id=pg",  // profile
		"https://news.ycombinator.com/jobs",        // unsupported feed
		"https://example.com/news",                 // not Hacker News
	}
	for _, u := range fallThrough {
		if e.Matches(u) {
			t.Errorf("Matches(%q) = true, want false (should fall through)", u)
		}
	}
}

// Crawl on an /item URL must fetch /items/{id}, flatten the comment tree with
// parent_id, unescape HTML, skip deleted nodes but keep their live replies.
func TestCrawlItem(t *testing.T) {
	const item = `{"id":1,"created_at_i":1700000000,"type":"story","author":"pg","title":"Hello","url":"https://example.com","points":100,"children":[
		{"id":2,"author":"alice","text":"<p>first &amp; best","created_at_i":1700000100,"parent_id":1,"children":[
			{"id":3,"author":"bob","text":"reply","created_at_i":1700000200,"parent_id":2,"children":[]}
		]},
		{"id":4,"author":null,"text":null,"created_at_i":1700000300,"parent_id":1,"children":[
			{"id":5,"author":"carol","text":"under a deleted parent","created_at_i":1700000400,"parent_id":4,"children":[]}
		]}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/items/1") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, item)
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	doc, err := e.Crawl(context.Background(), "https://news.ycombinator.com/item?id=1", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	// Comments 2, 3, 5 are live; deleted #4 is skipped but its child #5 survives.
	if doc.Metadata["comments"] != "3" {
		t.Fatalf("comments = %q, want 3\n%s", doc.Metadata["comments"], doc.PageContent)
	}
	// TOON carries length markers, so transports must not char-truncate it —
	// they decide from content_type.
	if got := doc.Metadata[domain.ContentTypeKey]; got != domain.ContentTypeTOON {
		t.Fatalf("content_type = %q, want %q", got, domain.ContentTypeTOON)
	}
	for _, want := range []string{"Hello", "alice", "first & best", "bob", "carol"} {
		if !strings.Contains(doc.PageContent, want) {
			t.Errorf("output missing %q:\n%s", want, doc.PageContent)
		}
	}
}

// Crawl on a feed path must query the right Algolia tag and rank the hits.
func TestCrawlFrontPage(t *testing.T) {
	const search = `{"hits":[
		{"objectID":"10","title":"Story A","url":"https://a.com","author":"alice","points":50,"num_comments":12,"created_at_i":1700000000},
		{"objectID":"20","title":"Story B","url":"https://b.com","author":"bob","points":30,"num_comments":4,"created_at_i":1700000100}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("tags"); got != "front_page" {
			t.Errorf("tags = %q, want front_page", got)
		}
		_, _ = io.WriteString(w, search)
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	doc, err := e.Crawl(context.Background(), "https://news.ycombinator.com/news", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata["stories"] != "2" {
		t.Fatalf("stories = %q, want 2", doc.Metadata["stories"])
	}
	for _, want := range []string{"Story A", "Story B", "alice"} {
		if !strings.Contains(doc.PageContent, want) {
			t.Errorf("output missing %q:\n%s", want, doc.PageContent)
		}
	}
}

// /ask and /show must hit search_by_date (recency), not search (all-time top).
func TestCrawlFeedUsesSearchByDate(t *testing.T) {
	for _, feed := range []struct{ path, tag string }{
		{"/ask", "ask_hn"},
		{"/show", "show_hn"},
		{"/newest", "story"},
	} {
		t.Run(feed.path, func(t *testing.T) {
			var gotPath, gotTags string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotTags = r.URL.Path, r.URL.Query().Get("tags")
				_, _ = io.WriteString(w, `{"hits":[]}`)
			}))
			defer srv.Close()

			e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
			if _, err := e.Crawl(context.Background(), "https://news.ycombinator.com"+feed.path, domain.EngineOptions{}); err != nil {
				t.Fatalf("Crawl: %v", err)
			}
			if !strings.HasSuffix(gotPath, "/search_by_date") {
				t.Errorf("%s hit %q, want .../search_by_date", feed.path, gotPath)
			}
			if gotTags != feed.tag {
				t.Errorf("%s tags=%q, want %q", feed.path, gotTags, feed.tag)
			}
		})
	}
}

// A comment permalink (/item?id=<commentID>) resolves to a type:"comment" item;
// it must be emitted as a comment (not a blank-titled story) with the enclosing
// story id in the header.
func TestCrawlCommentPermalink(t *testing.T) {
	const comment = `{"id":99,"type":"comment","author":"bob","text":"<p>my point","parent_id":50,"story_id":42,"created_at_i":1700000000,"children":[
		{"id":100,"type":"comment","author":"carol","text":"a reply","parent_id":99,"children":[]}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, comment)
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	doc, err := e.Crawl(context.Background(), "https://news.ycombinator.com/item?id=99", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	// Root comment (#99) + its reply (#100) both emitted.
	if doc.Metadata["comments"] != "2" {
		t.Fatalf("comments = %q, want 2\n%s", doc.Metadata["comments"], doc.PageContent)
	}
	for _, want := range []string{"my point", "bob", "a reply", "carol"} {
		if !strings.Contains(doc.PageContent, want) {
			t.Errorf("output missing %q:\n%s", want, doc.PageContent)
		}
	}
}

// A non-200 from Algolia must surface as a typed FetchError, not a fake success.
func TestCrawlHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	_, err := e.Crawl(context.Background(), "https://news.ycombinator.com/item?id=1", domain.EngineOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.StatusCode != http.StatusNotFound {
		t.Fatalf("want *FetchError with 404, got %v", err)
	}
}

// A 200 with an unparseable body must classify as KindBadResponse.
func TestCrawlParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	_, err := e.Crawl(context.Background(), "https://news.ycombinator.com/item?id=1", domain.EngineOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindBadResponse {
		t.Fatalf("want KindBadResponse, got %v", err)
	}
}

// Crawl on an /item URL must honor the HN size caps in EngineOptions — the
// params used to be silently ignored, which billed the caller for the whole
// megathread while it asked for a slice.
func TestCrawlItemHonorsCaps(t *testing.T) {
	full := megaThread(megaThreadSizes) // 228 comments over 8 top-level threads
	tree := make([]algoliaItem, 0, len(megaThreadSizes))
	nextID := 2
	for _, size := range megaThreadSizes {
		tree = append(tree, buildBranch(&nextID, size).item())
	}
	body, err := json.Marshal(algoliaItem{ID: 1, Type: "story", Title: "megathread", Author: "pg", Children: tree})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})

	cases := []struct {
		name string
		opts domain.EngineOptions
		want int
	}{
		{"uncapped", domain.EngineOptions{}, len(full.Comments)},
		{"per_subtree_12", domain.EngineOptions{HNMaxPerSubtree: 12}, 87},
		{"top_level_3", domain.EngineOptions{HNMaxTopLevel: 3}, 176},
		{"max_comments_40", domain.EngineOptions{HNMaxComments: 40}, 40},
		{"combined", domain.EngineOptions{HNMaxTopLevel: 3, HNMaxPerSubtree: 12, HNMaxComments: 30}, 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, cerr := e.Crawl(context.Background(), "https://news.ycombinator.com/item?id=1", tc.opts)
			if cerr != nil {
				t.Fatalf("Crawl: %v", cerr)
			}
			if doc.Metadata["comments"] != strconv.Itoa(tc.want) {
				t.Fatalf("comments = %q, want %d", doc.Metadata["comments"], tc.want)
			}
			wantTruncated := tc.want < len(full.Comments)
			if got, has := doc.Metadata["truncated_from"]; has != wantTruncated ||
				(has && got != strconv.Itoa(len(full.Comments))) {
				t.Errorf("truncated_from = %q (present %v), want present=%v value %d",
					got, has, wantTruncated, len(full.Comments))
			}
		})
	}
}

// depth is Reddit-only: passing it on an HN URL must not shrink the tree (HN's
// best comments are often deep, so this engine caps breadth, never depth).
func TestCrawlItemIgnoresRedditDepth(t *testing.T) {
	tree := []algoliaItem{buildBranch(new(int), 20).item()}
	body, err := json.Marshal(algoliaItem{ID: 1, Type: "story", Title: "t", Children: tree})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	doc, err := e.Crawl(context.Background(), "https://news.ycombinator.com/item?id=1",
		domain.EngineOptions{RedditDepth: 1, RedditMaxComments: 5})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata["comments"] != "20" {
		t.Fatalf("comments = %q, want 20 (Reddit params must not affect HN)", doc.Metadata["comments"])
	}
}
