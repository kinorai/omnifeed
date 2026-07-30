package searxng

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/observability"
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

const degradedBody = `{"results":[],"unresponsive_engines":[["brave","Suspended: too many requests"]]}`

// newRetrySearcher builds a Searcher whose degraded retry fires after delay, in
// front of a server that answers with the successive bodies in replies (the last
// one repeats). The returned counter tracks upstream hits.
func newRetrySearcher(t *testing.T, delay time.Duration, replies ...string) (*Searcher, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := int(hits.Add(1)) - 1
		if n >= len(replies) {
			n = len(replies) - 1
		}
		_, _ = w.Write([]byte(replies[n]))
	}))
	t.Cleanup(srv.Close)
	s := New(Config{
		Endpoint:           srv.URL,
		Client:             httpx.New(srv.Client()),
		DegradedRetryDelay: delay,
		Logger:             slog.New(slog.DiscardHandler),
	})
	return s, &hits
}

// A degraded upstream is retried exactly once after the configured delay: the
// engine suspension is per-engine and time-boxed, so the second try recovers.
func TestSearch_RetriesOnceOnDegraded(t *testing.T) {
	const delay = 10 * time.Millisecond
	s, hits := newRetrySearcher(t, delay, degradedBody, fixture)

	start := time.Now()
	results, err := s.Search(context.Background(), "q", domain.SearchOptions{})
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Search: unexpected error %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("results: got %d, want 3", len(results))
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits: got %d, want 2", got)
	}
	if elapsed < delay {
		t.Fatalf("elapsed %v, want >= the retry delay %v", elapsed, delay)
	}
}

// Still degraded on the retry: return the second degraded error, and do not try
// a third time.
func TestSearch_DegradedTwiceReturnsError(t *testing.T) {
	s, hits := newRetrySearcher(t, 10*time.Millisecond, degradedBody)

	_, err := s.Search(context.Background(), "q", domain.SearchOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("want *domain.FetchError, got %T: %v", err, err)
	}
	if fe.Kind != domain.KindDegraded {
		t.Fatalf("Kind = %q, want %q", fe.Kind, domain.KindDegraded)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream hits: got %d, want 2", got)
	}
}

// Delay 0 disables the retry entirely.
func TestSearch_ZeroDelayDisablesRetry(t *testing.T) {
	s, hits := newRetrySearcher(t, 0, degradedBody)

	_, err := s.Search(context.Background(), "q", domain.SearchOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindDegraded {
		t.Fatalf("want degraded *domain.FetchError, got %T: %v", err, err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits: got %d, want 1 (retry disabled)", got)
	}
}

// A cancellation during the retry wait must return promptly with the ctx error
// rather than sleeping out the delay.
func TestSearch_CanceledDuringRetryWait(t *testing.T) {
	s, hits := newRetrySearcher(t, 5*time.Second, degradedBody)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(20*time.Millisecond, cancel)

	start := time.Now()
	_, err := s.Search(ctx, "q", domain.SearchOptions{})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %T: %v", err, err)
	}
	if elapsed > time.Second {
		t.Fatalf("elapsed %v, want a prompt return well under the 5s delay", elapsed)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream hits: got %d, want 1", got)
	}
}

// A 200 with no results is ambiguous: it means "degraded upstream" when SearXNG
// also reports unresponsive engines (suspended after a 429/CAPTCHA, so they were
// never queried), and "no hits" when it doesn't. Only the first is an error.
func TestSearch_ZeroResultsClassification(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		wantErr     bool
		wantResults int
		wantSubstr  []string
	}{
		{
			name:       "degraded — every engine suspended",
			body:       `{"results":[],"unresponsive_engines":[["brave","Suspended: too many requests"]]}`,
			wantErr:    true,
			wantSubstr: []string{"brave", "Suspended"},
		},
		{
			name:        "honest zero — no failure report",
			body:        `{"results":[]}`,
			wantErr:     false,
			wantResults: 0,
		},
		{
			name: "partial — results despite an unresponsive engine",
			body: `{"results":[{"title":"First","url":"https://example.com/a","engine":"google"}],` +
				`"unresponsive_engines":[["duckduckgo","timeout"]]}`,
			wantErr:     false,
			wantResults: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			results, err := s.Search(context.Background(), "q", domain.SearchOptions{})
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Search: unexpected error %v", err)
				}
				if len(results) != tc.wantResults {
					t.Fatalf("results: got %d, want %d", len(results), tc.wantResults)
				}
				return
			}
			var fe *domain.FetchError
			if !errors.As(err, &fe) {
				t.Fatalf("want *domain.FetchError, got %T: %v", err, err)
			}
			if fe.Kind != domain.KindDegraded {
				t.Fatalf("Kind = %q, want %q", fe.Kind, domain.KindDegraded)
			}
			if got := observability.Reason(err); got != "degraded" {
				t.Fatalf("Reason = %q, want %q", got, "degraded")
			}
			for _, want := range tc.wantSubstr {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err.Error(), want)
				}
			}
		})
	}
}
