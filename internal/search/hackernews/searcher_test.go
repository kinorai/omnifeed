package hackernews

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

const fixture = `{"hits":[
  {"title":"Longhorn – A Kubernetes-Native Filesystem","url":"https://example.com/longhorn",
   "points":59,"num_comments":52,"objectID":"45155731","created_at":"2025-09-07T05:49:29Z"},
  {"title":"Ask HN: how do you monitor disk pressure?","url":null,
   "points":7,"num_comments":3,"objectID":"40131248","created_at":"2024-04-23T12:38:19Z",
   "story_text":"<p>We run &lt;a&gt;Longhorn&lt;/a&gt; and keep hitting disk pressure.</p>"}
]}`

func newTestSearcher(t *testing.T, handler http.HandlerFunc) *Searcher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, Client: httpx.New(srv.Client())})
}

// The two parameters this searcher cannot work without: without
// removeWordsIfNoResults Algolia hard-ANDs every term and a multi-word query
// returns nothing, and numericFilters' `>` must arrive percent-encoded (a raw
// `>` intermittently 400s at Algolia's frontend).
func TestSearchSendsAlgoliaParams(t *testing.T) {
	var rawQuery string
	var params url.Values
	s := newTestSearcher(t, func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		params = r.URL.Query()
		_, _ = io.WriteString(w, fixture)
	})

	before := time.Now().Add(-8 * 24 * time.Hour).Unix()
	if _, err := s.Search(context.Background(), "kubernetes longhorn disk pressure",
		domain.SearchOptions{TimeRange: "week", Limit: 5}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	after := time.Now().Add(-6 * 24 * time.Hour).Unix()

	for key, want := range map[string]string{
		"query":                  "kubernetes longhorn disk pressure",
		"tags":                   "story",
		"hitsPerPage":            "5",
		"removeWordsIfNoResults": "allOptional",
	} {
		if got := params.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if strings.ContainsRune(rawQuery, '>') {
		t.Errorf("raw query carries an unencoded '>': %s", rawQuery)
	}
	if !strings.Contains(rawQuery, "created_at_i%3E") {
		t.Errorf("numericFilters not percent-encoded in %s", rawQuery)
	}
	cutoff, err := strconv.ParseInt(strings.TrimPrefix(params.Get("numericFilters"), "created_at_i>"), 10, 64)
	if err != nil {
		t.Fatalf("numericFilters = %q: %v", params.Get("numericFilters"), err)
	}
	if cutoff < before || cutoff > after {
		t.Errorf("week cutoff = %d, want between %d and %d", cutoff, before, after)
	}
}

func TestSearchMapsHits(t *testing.T) {
	s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, fixture)
	})

	results, err := s.Search(context.Background(), "longhorn", domain.SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results: got %d, want 2", len(results))
	}

	first := results[0]
	if first.URL != "https://example.com/longhorn" || first.Engine != "hackernews" ||
		first.PublishedDate != "2025-09-07T05:49:29Z" {
		t.Errorf("first result mapped wrong: %+v", first)
	}
	// The ranking signal a scraped engine never exposes.
	if first.Snippet != "59 points, 52 comments" {
		t.Errorf("first snippet = %q, want the points/comments line", first.Snippet)
	}
	// A self-post carries no url: it must map to its HN thread page, and its
	// story_text must arrive un-escaped, tag-free, after an em dash.
	second := results[1]
	if second.URL != "https://news.ycombinator.com/item?id=40131248" {
		t.Errorf("null-url hit URL = %q, want the item link", second.URL)
	}
	if second.Snippet != "7 points, 3 comments — We run <a>Longhorn</a> and keep hitting disk pressure." {
		t.Errorf("self-post snippet = %q", second.Snippet)
	}
}

func TestSearchClampsHitsPerPage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		want  string
	}{
		{"unset falls back to the default", 0, "10"},
		{"negative falls back to the default", -3, "10"},
		{"in range is passed through", 12, "12"},
		{"oversized is clamped", 500, "30"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			s := newTestSearcher(t, func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Query().Get("hitsPerPage")
				_, _ = io.WriteString(w, `{"hits":[]}`)
			})
			if _, err := s.Search(context.Background(), "q", domain.SearchOptions{Limit: tc.limit}); err != nil {
				t.Fatalf("Search: %v", err)
			}
			if got != tc.want {
				t.Errorf("hitsPerPage = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSearchEmptyQueryRejected(t *testing.T) {
	s := newTestSearcher(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("server must not be called for an empty query")
	})
	if _, err := s.Search(context.Background(), "  ", domain.SearchOptions{}); err == nil {
		t.Fatal("want error for empty query, got nil")
	}
}

// The limiter is opt-in and must gate the request. With the quota spent and a
// deadline shorter than the window, Search must fail in the limiter rather than
// reach the server.
func TestSearchRespectsTheLimiter(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"hits":[]}`)
	}))
	defer srv.Close()

	s := New(Config{
		BaseURL: srv.URL,
		Client:  httpx.New(srv.Client()),
		Limiter: httpx.NewDomainQuotaLimiter(1, 0, 1, time.Hour),
	})

	if _, err := s.Search(context.Background(), "first", domain.SearchOptions{}); err != nil {
		t.Fatalf("first Search: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Search(ctx, "second", domain.SearchOptions{}); err == nil {
		t.Fatal("second Search: want a limiter deadline error, got nil")
	}
	if calls != 1 {
		t.Fatalf("server calls = %d, want 1 (the second query must not reach it)", calls)
	}
}
