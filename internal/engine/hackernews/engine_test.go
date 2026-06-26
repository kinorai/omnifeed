package hackernews

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
