package bluesky

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

const fixture = `{"posts":[
  {"uri":"at://did:plc:w4lcqitqlgpts3cd4vjldle2/app.bsky.feed.post/3mtcaqd2hrx2o",
   "author":{"handle":"learnkube.com","displayName":"LearnKube"},
   "record":{"text":"kube-gpu-top finds idle GPUs\nby mapping NVIDIA metrics to pods","createdAt":"2026-08-17T17:41:03.032Z"}},
  {"uri":"at://did:plc:abc/app.bsky.feed.post/3lllll",
   "author":{"handle":"someone.bsky.social","displayName":"Someone"},
   "record":{"text":"   ","createdAt":"2026-08-01T00:00:00.000Z"}}
]}`

func newTestSearcher(t *testing.T, handler http.HandlerFunc) *Searcher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, Client: httpx.New(srv.Client())})
}

func TestSearchSendsPathAndParams(t *testing.T) {
	var gotPath string
	var params url.Values
	s := newTestSearcher(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, params = r.URL.Path, r.URL.Query()
		_, _ = io.WriteString(w, fixture)
	})

	before := time.Now().Add(-31 * 24 * time.Hour)
	if _, err := s.Search(context.Background(), "kubernetes gpu",
		domain.SearchOptions{Limit: 3, Language: "en", TimeRange: "month"}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	after := time.Now().Add(-29 * 24 * time.Hour)

	if gotPath != "/xrpc/app.bsky.feed.searchPosts" {
		t.Errorf("path = %q, want the searchPosts XRPC path", gotPath)
	}
	for key, want := range map[string]string{"q": "kubernetes gpu", "limit": "3", "lang": "en"} {
		if got := params.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	since, err := time.Parse(time.RFC3339, params.Get("since"))
	if err != nil {
		t.Fatalf("since = %q: %v", params.Get("since"), err)
	}
	if since.Before(before) || since.After(after) {
		t.Errorf("month cutoff = %s, want between %s and %s", since, before, after)
	}
}

func TestSearchMapsPosts(t *testing.T) {
	s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, fixture)
	})

	results, err := s.Search(context.Background(), "kubernetes gpu", domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results: got %d, want 2", len(results))
	}

	first := results[0]
	// The AT-URI's last segment is the record key the bsky.app permalink needs.
	if first.URL != "https://bsky.app/profile/learnkube.com/post/3mtcaqd2hrx2o" {
		t.Errorf("URL = %q", first.URL)
	}
	if first.Title != "kube-gpu-top finds idle GPUs" {
		t.Errorf("title = %q, want the post's first line", first.Title)
	}
	if first.Engine != "bluesky" || first.PublishedDate != "2026-08-17T17:41:03.032Z" {
		t.Errorf("engine/date mapped wrong: %+v", first)
	}
	if first.Snippet != "kube-gpu-top finds idle GPUs by mapping NVIDIA metrics to pods — @learnkube.com" {
		t.Errorf("snippet = %q", first.Snippet)
	}
	// A post with no usable text has no title of its own; the author stands in.
	if results[1].Title != "@someone.bsky.social" {
		t.Errorf("textless title = %q, want the handle", results[1].Title)
	}
}

func TestSearchClampsLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		want  string
	}{
		{"unset falls back to the default", 0, "10"},
		{"negative falls back to the default", -2, "10"},
		{"in range is passed through", 8, "8"},
		{"oversized is clamped", 400, "25"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			s := newTestSearcher(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Query().Get("limit")
				_, _ = io.WriteString(w, `{"posts":[]}`)
			})
			if _, err := s.Search(context.Background(), "q", domain.SearchOptions{Limit: tc.limit}); err != nil {
				t.Fatalf("Search: %v", err)
			}
			if got != tc.want {
				t.Errorf("limit = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSearchEmptyQueryRejected(t *testing.T) {
	s := newTestSearcher(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("server must not be called for an empty query")
	})
	if _, err := s.Search(context.Background(), " ", domain.SearchOptions{}); err == nil {
		t.Fatal("want error for empty query, got nil")
	}
}
