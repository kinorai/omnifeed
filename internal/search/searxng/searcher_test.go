package searxng

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

const fixture = `{
  "query": "test",
  "results": [
    {"title": "First", "url": "https://example.com/a", "content": "snippet a", "engine": "google", "publishedDate": "2026-06-01T00:00:00"},
    {"title": "Second", "url": "https://www.reddit.com/r/golang/comments/abc/post/", "content": "snippet b", "engine": "duckduckgo", "publishedDate": null},
    {"title": "Third", "url": "https://example.com/c", "content": "", "engine": "brave"}
  ]
}`

func newTestSearcher(t *testing.T, handler http.HandlerFunc) *Searcher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Config{Endpoint: srv.URL, Client: httpx.New(srv.Client())})
}

func TestSearch_MapsResultsAndSendsParams(t *testing.T) {
	var gotQuery, gotFormat, gotTimeRange, gotLang string
	s := newTestSearcher(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		gotQuery, gotFormat = q.Get("q"), q.Get("format")
		gotTimeRange, gotLang = q.Get("time_range"), q.Get("language")
		_, _ = w.Write([]byte(fixture))
	})

	results, err := s.Search(context.Background(), "golang generics",
		domain.SearchOptions{TimeRange: "month", Language: "en"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if gotQuery != "golang generics" || gotFormat != "json" {
		t.Fatalf("query params: got q=%q format=%q", gotQuery, gotFormat)
	}
	if gotTimeRange != "month" || gotLang != "en" {
		t.Fatalf("optional params: got time_range=%q language=%q", gotTimeRange, gotLang)
	}
	if len(results) != 3 {
		t.Fatalf("results: got %d, want 3", len(results))
	}
	first := results[0]
	if first.Title != "First" || first.URL != "https://example.com/a" ||
		first.Snippet != "snippet a" || first.Engine != "google" ||
		first.PublishedDate != "2026-06-01T00:00:00" {
		t.Fatalf("first result mapped wrong: %+v", first)
	}
	// null publishedDate must not error and must map to "".
	if results[1].PublishedDate != "" {
		t.Fatalf("null publishedDate: got %q, want empty", results[1].PublishedDate)
	}
}

func TestSearch_ClampsToLimit(t *testing.T) {
	s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fixture))
	})

	results, err := s.Search(context.Background(), "q", domain.SearchOptions{Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("results: got %d, want 2 (limit)", len(results))
	}
}

func TestSearch_EmptyQueryRejected(t *testing.T) {
	s := newTestSearcher(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("server must not be called for an empty query")
	})

	if _, err := s.Search(context.Background(), "  ", domain.SearchOptions{}); err == nil {
		t.Fatal("want error for empty query, got nil")
	}
}

func TestSearch_Non200IsError(t *testing.T) {
	s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		// 403 is what SearXNG returns when the json format is not enabled.
		w.WriteHeader(http.StatusForbidden)
	})

	if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err == nil {
		t.Fatal("want error on 403, got nil")
	}
}
