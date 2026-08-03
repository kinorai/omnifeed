package searxng

import (
	"context"
	"errors"
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

// A non-200 must surface as a typed *domain.FetchError so the reason label
// distinguishes a SearXNG config break (403, json format disabled) from an
// upstream outage (5xx) — both used to collapse to reason=error.
func TestSearch_Non200IsTypedError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   domain.FailureKind
	}{
		{"403 json-format-disabled", http.StatusForbidden, domain.KindHTTP403},
		{"5xx upstream", http.StatusBadGateway, domain.KindUpstreamError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			})
			_, err := s.Search(context.Background(), "q", domain.SearchOptions{})
			var fe *domain.FetchError
			if !errors.As(err, &fe) {
				t.Fatalf("want *domain.FetchError, got %T: %v", err, err)
			}
			if fe.Kind != tc.want {
				t.Fatalf("Kind = %q, want %q", fe.Kind, tc.want)
			}
		})
	}
}

// A 200 with no results is ambiguous: SearXNG lists only the engines that
// failed, not how many ran, so "engines suspended" is indistinguishable from an
// honest zero-hit query. All shapes return success — empty results are data,
// not an error (the failure report is logged, not raised).
func TestSearch_ZeroResultsClassification(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantResults int
	}{
		{
			name: "zero results with a failure report is still a success",
			body: `{"results":[],"unresponsive_engines":[["brave","Suspended: too many requests"]]}`,
		},
		{
			name: "honest zero — no failure report",
			body: `{"results":[]}`,
		},
		{
			name: "partial — results despite an unresponsive engine",
			body: `{"results":[{"title":"First","url":"https://example.com/a","engine":"google"}],` +
				`"unresponsive_engines":[["duckduckgo","timeout"]]}`,
			wantResults: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			results, err := s.Search(context.Background(), "q", domain.SearchOptions{})
			if err != nil {
				t.Fatalf("Search: unexpected error %v", err)
			}
			if len(results) != tc.wantResults {
				t.Fatalf("results: got %d, want %d", len(results), tc.wantResults)
			}
		})
	}
}
