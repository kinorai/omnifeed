package bluesky

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// Matches must claim post and profile URLs, and fall through for everything
// else on the host — notably /search, which the AppView refuses without auth
// (see the package doc), so the generic fallback should keep it.
func TestMatches(t *testing.T) {
	e := &Engine{}

	claim := []string{
		"https://bsky.app/profile/bsky.app/post/3msqpusnigc2t",
		"https://bsky.app/profile/did:plc:z72i7hdynmk6r22z27h6tvur/post/3msqpusnigc2t",
		"https://bsky.app/profile/bsky.app",
		"https://bsky.app/profile/bsky.app/", // trailing slash
	}
	for _, u := range claim {
		if !e.Matches(u) {
			t.Errorf("Matches(%q) = false, want true", u)
		}
	}

	fallThrough := []string{
		"https://bsky.app/search?q=kubernetes",            // searchPosts is 403 without auth
		"https://bsky.app/profile/bsky.app/feed/whatshot", // feed generator
		"https://bsky.app/profile/bsky.app/post/",         // no rkey
		"https://bsky.app/profile/",                       // no actor
		"https://bsky.app/",                               // home
		"https://example.com/profile/x/post/y",            // not bluesky
	}
	for _, u := range fallThrough {
		if e.Matches(u) {
			t.Errorf("Matches(%q) = true, want false (should fall through)", u)
		}
	}
}

// Crawl on a post URL must build an at:// URI from the path, flatten the reply
// tree with parent_uri, and skip unreadable nodes while keeping their replies.
func TestCrawlThread(t *testing.T) {
	const thread = `{"thread":{
		"post":{"uri":"at://did:plc:aaa/app.bsky.feed.post/root","author":{"handle":"alice.bsky.social","displayName":"Alice"},
			"record":{"text":"the original post","createdAt":"2026-08-01T10:00:00Z"},"replyCount":3,"repostCount":7,"likeCount":42,
			"embed":{"external":{"uri":"https://example.com/article"}}},
		"replies":[
			{"post":{"uri":"at://did:plc:bbb/app.bsky.feed.post/r1","author":{"handle":"bob.bsky.social"},
				"record":{"text":"first reply","createdAt":"2026-08-01T10:05:00Z"},"likeCount":2},
			 "replies":[
				{"post":{"uri":"at://did:plc:ccc/app.bsky.feed.post/r2","author":{"handle":"carol.bsky.social"},
					"record":{"text":"nested reply","createdAt":"2026-08-01T10:10:00Z"}},"replies":[]}
			 ]},
			{"post":{},"replies":[
				{"post":{"uri":"at://did:plc:ddd/app.bsky.feed.post/r3","author":{"handle":"dave.bsky.social"},
					"record":{"text":"under a blocked parent","createdAt":"2026-08-01T10:15:00Z"}},"replies":[]}
			]}
		]}}`

	var gotPath, gotURI, gotDepth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotURI = r.URL.Query().Get("uri")
		gotDepth = r.URL.Query().Get("depth")
		_, _ = io.WriteString(w, thread)
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	doc, err := e.Crawl(context.Background(),
		"https://bsky.app/profile/alice.bsky.social/post/3abc", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}

	if !strings.HasSuffix(gotPath, "/app.bsky.feed.getPostThread") {
		t.Errorf("path = %q, want .../app.bsky.feed.getPostThread", gotPath)
	}
	// The AppView resolves a handle authority server-side, so the URL's actor
	// goes into the AT-URI as-is — no resolveHandle round trip.
	if want := "at://alice.bsky.social/app.bsky.feed.post/3abc"; gotURI != want {
		t.Errorf("uri = %q, want %q", gotURI, want)
	}
	if gotDepth != "20" {
		t.Errorf("depth = %q, want 20", gotDepth)
	}

	// r1, r2, r3 are readable; the blocked node is skipped but r3 survives.
	if doc.Metadata["replies"] != "3" {
		t.Fatalf("replies = %q, want 3\n%s", doc.Metadata["replies"], doc.PageContent)
	}
	if got := doc.Metadata[domain.ContentTypeKey]; got != domain.ContentTypeTOON {
		t.Fatalf("content_type = %q, want %q", got, domain.ContentTypeTOON)
	}
	for _, want := range []string{
		"the original post", "Alice", "first reply", "nested reply",
		"under a blocked parent", "https://example.com/article",
	} {
		if !strings.Contains(doc.PageContent, want) {
			t.Errorf("output missing %q:\n%s", want, doc.PageContent)
		}
	}
	// r3's parent_uri must be the ROOT, not the skipped blocked node: recursion
	// passes the nearest readable ancestor down.
	if !strings.Contains(doc.PageContent, "at://did:plc:aaa/app.bsky.feed.post/root") {
		t.Errorf("root uri missing from output (parent_uri chain broken):\n%s", doc.PageContent)
	}
}

// Crawl on a profile URL must hit getAuthorFeed and emit the account's posts.
func TestCrawlAuthorFeed(t *testing.T) {
	const feed = `{"feed":[
		{"post":{"uri":"at://did:plc:aaa/app.bsky.feed.post/p1","author":{"handle":"alice.bsky.social","displayName":"Alice"},
			"record":{"text":"post one","createdAt":"2026-08-01T10:00:00Z"},"likeCount":5}},
		{"post":{"uri":"at://did:plc:aaa/app.bsky.feed.post/p2","author":{"handle":"alice.bsky.social"},
			"record":{"text":"post two","createdAt":"2026-08-02T10:00:00Z"}}},
		{"post":{}}
	]}`

	var gotPath, gotActor string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotActor = r.URL.Query().Get("actor")
		_, _ = io.WriteString(w, feed)
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	doc, err := e.Crawl(context.Background(),
		"https://bsky.app/profile/alice.bsky.social", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}

	if !strings.HasSuffix(gotPath, "/app.bsky.feed.getAuthorFeed") {
		t.Errorf("path = %q, want .../app.bsky.feed.getAuthorFeed", gotPath)
	}
	if gotActor != "alice.bsky.social" {
		t.Errorf("actor = %q, want alice.bsky.social", gotActor)
	}
	// The unreadable third entry is dropped.
	if doc.Metadata["posts"] != "2" {
		t.Fatalf("posts = %q, want 2\n%s", doc.Metadata["posts"], doc.PageContent)
	}
	for _, want := range []string{"post one", "post two", "Alice"} {
		if !strings.Contains(doc.PageContent, want) {
			t.Errorf("output missing %q:\n%s", want, doc.PageContent)
		}
	}
}

// A non-200 from the AppView must classify by status, so the registry can fall
// back to the browser rather than hand the caller an error page.
func TestCrawlUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"Forbidden"}`)
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	_, err := e.Crawl(context.Background(),
		"https://bsky.app/profile/alice.bsky.social/post/3abc", domain.EngineOptions{})
	if err == nil {
		t.Fatal("Crawl: want error on 403, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, want it to name the status", err)
	}
}

// A viral thread must be capped, with the pre-cap total reported.
func TestCrawlThreadTruncates(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"thread":{"post":{"uri":"at://did:plc:aaa/app.bsky.feed.post/root",` +
		`"author":{"handle":"alice.bsky.social"},"record":{"text":"root"}},"replies":[`)
	for i := 0; i < maxThreadReplies+10; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"post":{"uri":"at://did:plc:bbb/app.bsky.feed.post/r","author":{"handle":"bob.bsky.social"},` +
			`"record":{"text":"reply"}},"replies":[]}`)
	}
	b.WriteString(`]}}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, b.String())
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	doc, err := e.Crawl(context.Background(),
		"https://bsky.app/profile/alice.bsky.social/post/3abc", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata["replies"] != "500" {
		t.Errorf("replies = %q, want 500", doc.Metadata["replies"])
	}
	if doc.Metadata["truncated_from"] != "510" {
		t.Errorf("truncated_from = %q, want 510", doc.Metadata["truncated_from"])
	}
}
