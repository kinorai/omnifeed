package tinyfish

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

const fixture = `{
  "query": "q",
  "results": [
    {"position": 1, "site_name": "a.example", "title": "A", "url": "https://a.example/1", "snippet": "first", "date": "5 hours ago"},
    {"position": 2, "site_name": "b.example", "title": "B", "url": "https://b.example/2", "snippet": "second", "date": "Sep 25, 2026"},
    {"position": 3, "site_name": "c.example", "title": "C", "url": "https://c.example/3", "snippet": "third"}
  ],
  "total_results": 3,
  "page": 0
}`

var pinnedNow = time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)

func newTestSearcher(t *testing.T, handler http.HandlerFunc) *Searcher {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Config{
		Endpoint: srv.URL,
		APIKey:   "test-key",
		Client:   httpx.New(srv.Client()),
		Now:      func() time.Time { return pinnedNow },
	})
}

func TestSearch_SendsKeyAndMapsOptions(t *testing.T) {
	var got url.Values
	var gotKey string
	s := newTestSearcher(t, func(w http.ResponseWriter, r *http.Request) {
		got, gotKey = r.URL.Query(), r.Header.Get("X-API-Key")
		_, _ = w.Write([]byte(fixture))
	})

	_, err := s.Search(context.Background(), "kubernetes gateway",
		domain.SearchOptions{Site: "reddit.com", TimeRange: "week", Language: "fr"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotKey != "test-key" {
		t.Fatalf("X-API-Key = %q, want test-key", gotKey)
	}
	want := map[string]string{
		"query":           "kubernetes gateway",
		"include_domains": "reddit.com",
		"recency_minutes": "10080",
		"language":        "fr",
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("param %s = %q, want %q", k, got.Get(k), v)
		}
	}
}

func TestSearch_OmitsUnsetOptions(t *testing.T) {
	var got url.Values
	s := newTestSearcher(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_, _ = w.Write([]byte(fixture))
	})
	if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, k := range []string{"include_domains", "recency_minutes", "language"} {
		if got.Has(k) {
			t.Errorf("param %s sent for an unset option", k)
		}
	}
}

func TestSearch_MapsResultsAndClampsToLimit(t *testing.T) {
	s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(fixture))
	})

	report, err := s.Search(context.Background(), "q", domain.SearchOptions{Limit: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if report.Degraded() {
		t.Fatalf("a single-source searcher must never report Degraded, got %q", report.DegradedReason)
	}
	if len(report.Results) != 2 {
		t.Fatalf("results = %d, want 2 (limit)", len(report.Results))
	}
	first := report.Results[0]
	if first.Title != "A" || first.URL != "https://a.example/1" || first.Snippet != "first" ||
		first.Engine != "tinyfish" || first.PublishedDate != "2026-10-23T07:00:00Z" {
		t.Fatalf("first result mapped wrong: %+v", first)
	}
	if got := report.Results[1].PublishedDate; got != "2026-09-25T00:00:00Z" {
		t.Fatalf("absolute date = %q, want 2026-09-25T00:00:00Z", got)
	}
}

func TestSearch_ErrorStatusCarriesKindAndCode(t *testing.T) {
	s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"code":"INSUFFICIENT_CREDITS","message":"wallet empty for query secret-text"}}`))
	})

	_, err := s.Search(context.Background(), "secret-text", domain.SearchOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("error = %v, want *domain.FetchError", err)
	}
	if fe.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("StatusCode = %d, want 402", fe.StatusCode)
	}
	if !strings.Contains(err.Error(), "INSUFFICIENT_CREDITS") {
		t.Fatalf("error %q should name the quota that ran out", err)
	}
	if strings.Contains(err.Error(), "secret-text") {
		t.Fatalf("error %q must not echo the upstream message", err)
	}
}

func TestSearch_EmptyQueryRejected(t *testing.T) {
	s := newTestSearcher(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("server must not be called for an empty query")
	})
	if _, err := s.Search(context.Background(), "  ", domain.SearchOptions{}); err == nil {
		t.Fatal("expected an error for an empty query")
	}
}

func TestParseDate(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"", ""},
		{"55 minutes ago", "2026-10-23T11:05:00Z"},
		{"1 hour ago", "2026-10-23T11:00:00Z"},
		{"2 days ago", "2026-10-21T12:00:00Z"},
		{"1 week ago", "2026-10-16T12:00:00Z"},
		{"Oct 7, 2026", "2026-10-07T00:00:00Z"},
		{"2026-06-01", "2026-06-01T00:00:00Z"},
		{"yesterday", ""},
		{"5 fortnights ago", ""},
	}
	for _, tc := range cases {
		if got := parseDate(tc.raw, pinnedNow); got != tc.want {
			t.Errorf("parseDate(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}
