package searxng

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

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

// Every unresponsive_engines entry must increment
// omnifeed_searxng_unresponsive_engines_total under {engine, error} so engine
// cooldowns are visible per engine. Entries are [engine, error_type] pairs;
// the error string is upstream free text and is normalized to a closed
// vocabulary before becoming a label (cardinality guard), a missing error type
// counts as "unknown", malformed entries are skipped silently, and the field's
// absence increments nothing.
func TestSearch_CountsUnresponsiveEngines(t *testing.T) {
	cases := []struct {
		name string
		body string
		want map[[2]string]float64 // {engine, error} → count
	}{
		{
			name: "well-formed pairs, errors normalized",
			body: `{"results":[],"unresponsive_engines":[` +
				`["brave","Suspended: too many requests"],` +
				`["duckduckgo","timeout"],` +
				`["brave","Suspended: too many requests"]]}`,
			want: map[[2]string]float64{
				{"brave", "too_many_requests"}: 2,
				{"duckduckgo", "timeout"}:      1,
			},
		},
		{
			name: "1-element pair counts as unknown, malformed entries skipped",
			body: `{"results":[],"unresponsive_engines":[["startpage"],[],["",""],["qwant","CAPTCHA required"]]}`,
			want: map[[2]string]float64{
				{"startpage", "unknown"}: 1,
				{"qwant", "captcha"}:     1,
			},
		},
		{
			name: "novel upstream message collapses to error",
			body: `{"results":[],"unresponsive_engines":[["mojeek","SearxEngineAPIException: unexpected shape v2"]]}`,
			want: map[[2]string]float64{
				{"mojeek", "error"}: 1,
			},
		},
		{
			name: "no report increments nothing",
			body: `{"results":[]}`,
			want: map[[2]string]float64{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := observability.NewMetrics()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)
			s := New(Config{Endpoint: srv.URL, Client: httpx.New(srv.Client()), Metrics: m})

			if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err != nil {
				t.Fatalf("Search: %v", err)
			}

			for labels, want := range tc.want {
				var dm dto.Metric
				if err := m.SearxngUnresponsive.WithLabelValues(labels[0], labels[1]).Write(&dm); err != nil {
					t.Fatal(err)
				}
				if got := dm.GetCounter().GetValue(); got != want {
					t.Fatalf("counter{%s,%s} = %v, want %v", labels[0], labels[1], got, want)
				}
			}
			// No stray series beyond the expected ones (malformed entries skipped).
			ch := make(chan prometheus.Metric, 16)
			m.SearxngUnresponsive.Collect(ch)
			close(ch)
			if got := len(ch); got != len(tc.want) {
				t.Fatalf("series count = %d, want %d", got, len(tc.want))
			}
		})
	}
}

// A `site` option must reach the engines as a `site:` operator, because naming
// the site inside the query text does not restrict anything.
func TestSearchSiteFilter(t *testing.T) {
	for _, tc := range []struct {
		name, site, wantQ string
	}{
		{"scopes to the host", "reddit.com", "site:reddit.com kubernetes plex"},
		{"empty site is untouched", "", "kubernetes plex"},
		// A value that could carry another operator is dropped, not forwarded:
		// the filter is a narrowing hint, so a bad one must not break the search.
		{"injection attempt dropped", "evil.com foo:bar", "kubernetes plex"},
		{"quote injection dropped", `x" OR "y`, "kubernetes plex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotQ string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQ = r.URL.Query().Get("q")
				_, _ = io.WriteString(w, `{"results":[]}`)
			}))
			defer srv.Close()

			s := New(Config{Endpoint: srv.URL, Client: httpx.New(nil)})
			if _, err := s.Search(context.Background(), "kubernetes plex",
				domain.SearchOptions{Site: tc.site}); err != nil {
				t.Fatalf("Search: %v", err)
			}
			if gotQ != tc.wantQ {
				t.Errorf("q = %q, want %q", gotQ, tc.wantQ)
			}
		})
	}
}

// SiteEngines narrows the pool for site-scoped searches only. Engines that do
// not implement `site:` answer such a query with unrelated pages or with
// nothing at all, so they must not be asked; every other search still runs on
// the whole pool.
func TestSearchSiteEngines(t *testing.T) {
	for _, tc := range []struct {
		name, site  string
		siteEngines []string
		wantEngines string
	}{
		{"site-scoped narrows the pool", "reddit.com", []string{"privacywall", "google cse"}, "privacywall,google cse"},
		{"unscoped search keeps every engine", "", []string{"privacywall"}, ""},
		// siteScoped drops an invalid site, so the query is no longer scoped and
		// narrowing it would lose recall for nothing.
		{"dropped site filter does not narrow", "evil.com foo:bar", []string{"privacywall"}, ""},
		{"unset config queries the whole pool", "reddit.com", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotEngines string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotEngines = r.URL.Query().Get("engines")
				_, _ = io.WriteString(w, `{"results":[]}`)
			}))
			defer srv.Close()

			s := New(Config{Endpoint: srv.URL, Client: httpx.New(nil), SiteEngines: tc.siteEngines})
			if _, err := s.Search(context.Background(), "kubernetes plex",
				domain.SearchOptions{Site: tc.site}); err != nil {
				t.Fatalf("Search: %v", err)
			}
			if gotEngines != tc.wantEngines {
				t.Errorf("engines = %q, want %q", gotEngines, tc.wantEngines)
			}
		})
	}
}

// A search that comes back with no results and no failure report is the shape a
// silently blocked pool produces. It stays a success (it is indistinguishable
// from an honest zero-hit query), but it must be counted, split by whether the
// caller scoped the search — that is where silent blocks concentrate.
func TestSearchCountsEmptySearches(t *testing.T) {
	for _, tc := range []struct {
		name       string
		body       string
		site       string
		wantScoped string // label value; "" means nothing must be counted
	}{
		{
			name:       "unscoped empty, no failure report",
			body:       `{"results":[]}`,
			wantScoped: "false",
		},
		{
			name:       "site-scoped empty, no failure report",
			body:       `{"results":[]}`,
			site:       "reddit.com",
			wantScoped: "true",
		},
		{
			// An engine said it failed, so the emptiness is explained. That path
			// already has its own counter and must not double-count here.
			name: "empty WITH a failure report is not a silent zero",
			body: `{"results":[],"unresponsive_engines":[["brave","Suspended: too many requests"]]}`,
		},
		{
			name: "results present",
			body: `{"results":[{"title":"a","url":"https://example.com/a","engine":"privacywall"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := observability.NewMetrics()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			s := New(Config{Endpoint: srv.URL, Client: httpx.New(srv.Client()), Metrics: m})
			if _, err := s.Search(context.Background(), "q", domain.SearchOptions{Site: tc.site}); err != nil {
				t.Fatalf("Search: %v", err)
			}

			for _, label := range []string{"true", "false"} {
				var dm dto.Metric
				if err := m.SearxngEmpty.WithLabelValues(label).Write(&dm); err != nil {
					t.Fatal(err)
				}
				want := 0.0
				if label == tc.wantScoped {
					want = 1
				}
				if got := dm.GetCounter().GetValue(); got != want {
					t.Fatalf("empty_searches{scoped=%q} = %v, want %v", label, got, want)
				}
			}
		})
	}
}

// Per-engine row counts are what make a silent block visible: the blocked
// engine's series stops advancing while the rest of the pool keeps moving.
// The count must reflect the whole response, not the caller's limit.
func TestSearchCountsResultsPerEngine(t *testing.T) {
	body := `{"results":[
		{"title":"a","url":"https://example.com/a","engine":"privacywall"},
		{"title":"b","url":"https://example.com/b","engine":"privacywall"},
		{"title":"c","url":"https://example.com/c","engine":"searchtoday"},
		{"title":"d","url":"https://example.com/d","engine":""}
	]}`
	m := observability.NewMetrics()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	s := New(Config{Endpoint: srv.URL, Client: httpx.New(srv.Client()), Metrics: m})
	// Limit 1: the caller sees one result, but the metric must still see all
	// four rows — it answers "is this engine alive", not "what did we return".
	if _, err := s.Search(context.Background(), "q", domain.SearchOptions{Limit: 1}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	for engine, want := range map[string]float64{"privacywall": 2, "searchtoday": 1} {
		var dm dto.Metric
		if err := m.SearxngEngineHits.WithLabelValues(engine).Write(&dm); err != nil {
			t.Fatal(err)
		}
		if got := dm.GetCounter().GetValue(); got != want {
			t.Fatalf("engine_results{engine=%q} = %v, want %v", engine, got, want)
		}
	}
	// The unnamed engine must not mint a series: the label stays bounded by the
	// instance's own engine list.
	ch := make(chan prometheus.Metric, 8)
	m.SearxngEngineHits.Collect(ch)
	close(ch)
	if got := len(ch); got != 2 {
		t.Fatalf("engine series = %d, want 2 (blank engine skipped)", got)
	}
}

// The limiter is opt-in and must gate the request. With the quota spent and a
// deadline shorter than the window, Search must fail in the limiter rather than
// reach the server.
func TestSearchRespectsTheLimiter(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()

	s := New(Config{
		Endpoint: srv.URL,
		Client:   httpx.New(srv.Client()),
		Limiter:  httpx.NewDomainQuotaLimiter(1, 0, 1, time.Hour),
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

// A query must not sit in the limiter indefinitely: with no caller deadline the
// searcher's own MaxWait has to end the wait, or a fan-out queues past the HTTP
// server's write timeout and the caller gets no response body at all.
// The error must also be CLASSIFIED — a local pacing wait reported raw is
// rendered by the REST transport as an upstream SearXNG failure (HTTP 502),
// which sends triage at the wrong component.
func TestSearchBoundsTheLimiterWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()

	s := New(Config{
		Endpoint: srv.URL,
		Client:   httpx.New(srv.Client()),
		Limiter:  httpx.NewDomainQuotaLimiter(1, 0, 1, time.Hour),
		MaxWait:  20 * time.Millisecond,
	})

	if _, err := s.Search(context.Background(), "first", domain.SearchOptions{}); err != nil {
		t.Fatalf("first Search: %v", err)
	}

	// context.Background() never expires: only MaxWait can end this.
	start := time.Now()
	_, err := s.Search(context.Background(), "second", domain.SearchOptions{})
	if err == nil {
		t.Fatal("second Search: want a MaxWait timeout, got nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("second Search blocked for %v, want ~MaxWait (20ms)", elapsed)
	}

	var fe *domain.FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("want a classified *domain.FetchError, got %T: %v", err, err)
	}
	if fe.Kind != domain.KindTimeout {
		t.Fatalf("Kind = %q, want %q (a pacing wait is our own budget, not an upstream fault)", fe.Kind, domain.KindTimeout)
	}
}

// An engine that SearXNG reports as unresponsive must mint its results series
// at zero. A counter that never existed cannot go flat, so without this an
// engine already failing at process start is invisible to a rate()-based alert.
func TestSearchMintsSeriesForUnresponsiveEngines(t *testing.T) {
	m := observability.NewMetrics()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"title":"a","url":"https://example.com/a","engine":"searchtoday"}],`+
			`"unresponsive_engines":[["privacywall","Suspended: too many requests"]]}`)
	}))
	defer srv.Close()

	s := New(Config{Endpoint: srv.URL, Client: httpx.New(srv.Client()), Metrics: m})
	if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	for engine, want := range map[string]float64{"searchtoday": 1, "privacywall": 0} {
		var dm dto.Metric
		if err := m.SearxngEngineHits.WithLabelValues(engine).Write(&dm); err != nil {
			t.Fatal(err)
		}
		if got := dm.GetCounter().GetValue(); got != want {
			t.Fatalf("engine_results{engine=%q} = %v, want %v", engine, got, want)
		}
	}
	// Both series must exist — the blocked engine's presence is the point.
	ch := make(chan prometheus.Metric, 8)
	m.SearxngEngineHits.Collect(ch)
	close(ch)
	if got := len(ch); got != 2 {
		t.Fatalf("engine series = %d, want 2 (the unresponsive engine must be minted)", got)
	}
}
