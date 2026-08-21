package searxng

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
// reach the server (as a fast-fail — see TestSearchFastFailsWhenPacingExceedsMaxWait).
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
// searcher's own MaxWait bounds the wait, or a fan-out queues past the HTTP
// server's write timeout and the caller gets no response body at all.
// The error must also be CLASSIFIED — a local pacing wait reported raw is
// rendered by the REST transport as an upstream SearXNG failure (HTTP 502),
// which sends triage at the wrong component.
//
// MaxWait is a deadline, so a wait longer than it is now refused up front
// rather than held for the full budget: the reason is quota_exhausted, not
// timeout, and it carries the retry-after.
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
	if fe.Kind != domain.KindQuotaExhausted {
		t.Fatalf("Kind = %q, want %q (a pacing wait is our own budget, not an upstream fault)", fe.Kind, domain.KindQuotaExhausted)
	}
	if !strings.Contains(fe.Err.Error(), "retry in ") {
		t.Fatalf("message = %q, want a retry-after the caller can act on", fe.Err.Error())
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

// auditFixture mirrors a real SearXNG response: `engines` and `positions` are
// parallel arrays holding each engine's own rank for the same URL.
const auditFixture = `{"results":[
	{"title":"a","url":"https://example.com/a","engine":"privacywall","score":4.8,
	 "engines":["privacywall","searchtoday"],"positions":[1,4]},
	{"title":"b","url":"https://example.com/b","engine":"searchtoday","score":1.2,
	 "engines":["searchtoday"],"positions":[2]}
]}`

// capture collects slog records so a test can assert on emitted fields.
type capture struct {
	records []map[string]any
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }
func (c *capture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *capture) WithGroup(string) slog.Handler            { return c }
func (c *capture) Handle(_ context.Context, r slog.Record) error {
	rec := map[string]any{"_msg": r.Message, "_level": r.Level}
	r.Attrs(func(a slog.Attr) bool {
		v := a.Value.Any()
		// slog widens every integer to int64; narrow it back so table-driven
		// expectations can be written as plain ints.
		if n, ok := v.(int64); ok {
			v = int(n)
		}
		rec[a.Key] = v
		return true
	})
	c.records = append(c.records, rec)
	return nil
}

func (c *capture) withMsg(msg string) []map[string]any {
	var out []map[string]any
	for _, r := range c.records {
		if r["_msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

func auditSearcher(t *testing.T, mode, body string) (*Searcher, *capture, *observability.Metrics) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	cap := &capture{}
	m := observability.NewMetrics()
	return New(Config{
		Endpoint: srv.URL, Client: httpx.New(srv.Client()),
		Logger: slog.New(cap), Metrics: m, Audit: mode,
	}), cap, m
}

// The audit log is opt-in. "off" (and the zero value) must emit nothing, so an
// operator who never asked for it never has query text in their log store.
func TestSearchAuditOffEmitsNothing(t *testing.T) {
	for _, mode := range []string{"", "off"} {
		s, cap, _ := auditSearcher(t, mode, auditFixture)
		if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err != nil {
			t.Fatalf("Search: %v", err)
		}
		if n := len(cap.withMsg("search audit")) + len(cap.withMsg("search result")); n != 0 {
			t.Errorf("mode %q emitted %d audit lines, want 0", mode, n)
		}
	}
}

// "summary" is one line per search and no position table — the mode for an
// operator who wants engine coverage without logging every result URL.
func TestSearchAuditSummary(t *testing.T) {
	s, cap, _ := auditSearcher(t, "summary", auditFixture)
	opts := domain.SearchOptions{Site: "example.com", TimeRange: "week", Limit: 1}
	if _, err := s.Search(context.Background(), "golang", opts); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if n := len(cap.withMsg("search result")); n != 0 {
		t.Fatalf("summary emitted %d position lines, want 0", n)
	}
	lines := cap.withMsg("search audit")
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1", len(lines))
	}
	got := lines[0]
	// total counts the whole response, not the caller's Limit of 1: the audit
	// answers what the pool returned.
	for k, want := range map[string]any{
		"query": "golang", "site": "example.com", "time_range": "week",
		"total": 2, "site_scoped": true,
		"engine_rows": "privacywall=1 searchtoday=2",
	} {
		if got[k] != want {
			t.Errorf("%s = %v, want %v", k, got[k], want)
		}
	}
	if got["query_id"] == "" || got["query_id"] == nil {
		t.Error("query_id missing — the lines cannot be joined without it")
	}
}

// "full" adds the position table: one line per (engine, result), each carrying
// that engine's OWN rank. This is the shape that survives a log store which
// flattens arrays into strings.
func TestSearchAuditFullEmitsPositionTable(t *testing.T) {
	s, cap, _ := auditSearcher(t, "full", auditFixture)
	if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	rows := cap.withMsg("search result")
	if len(rows) != 3 {
		t.Fatalf("position rows = %d, want 3 (2 engines on hit a, 1 on hit b)", len(rows))
	}
	// Every row shares the summary's query_id, which is what joins them.
	summary := cap.withMsg("search audit")
	if len(summary) != 1 {
		t.Fatalf("audit lines = %d, want 1", len(summary))
	}
	for _, r := range rows {
		if r["query_id"] != summary[0]["query_id"] {
			t.Fatalf("query_id %v does not match the summary %v", r["query_id"], summary[0]["query_id"])
		}
	}
	want := []struct {
		engine   string
		position int
		merged   int
		unique   bool
	}{
		{"privacywall", 1, 1, false},
		{"searchtoday", 4, 1, false},
		{"searchtoday", 2, 2, true},
	}
	for i, w := range want {
		r := rows[i]
		if r["engine"] != w.engine || r["position"] != w.position ||
			r["merged_rank"] != w.merged || r["unique"] != w.unique {
			t.Errorf("row %d = engine=%v position=%v merged=%v unique=%v, want %+v",
				i, r["engine"], r["position"], r["merged_rank"], r["unique"], w)
		}
	}
}

// A short or missing positions array is upstream data. It must not panic and
// must not invent a rank.
func TestSearchAuditToleratesShortPositions(t *testing.T) {
	body := `{"results":[{"title":"a","url":"https://example.com/a",
		"engines":["privacywall","searchtoday"],"positions":[1]}]}`
	s, cap, m := auditSearcher(t, "full", body)
	if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	rows := cap.withMsg("search result")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[1]["position"] != 0 {
		t.Errorf("missing position = %v, want 0 (unknown)", rows[1]["position"])
	}
	// An unknown rank must not enter the histogram, or it drags the mean down.
	var dm dto.Metric
	if err := m.SearchEnginePos.WithLabelValues("searchtoday").(prometheus.Metric).Write(&dm); err != nil {
		t.Fatal(err)
	}
	if got := dm.GetHistogram().GetSampleCount(); got != 0 {
		t.Errorf("searchtoday samples = %d, want 0 (rank was unknown)", got)
	}
}

// The rank metrics carry no query text and no URLs, so they are NOT gated by
// the audit setting — they must work with the audit log off.
func TestSearchRankMetricsIgnoreTheAuditSetting(t *testing.T) {
	s, _, m := auditSearcher(t, "off", auditFixture)
	if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	var dm dto.Metric
	if err := m.SearchEnginePos.WithLabelValues("privacywall").(prometheus.Metric).Write(&dm); err != nil {
		t.Fatal(err)
	}
	if got := dm.GetHistogram().GetSampleCount(); got != 1 {
		t.Fatalf("privacywall rank samples = %d, want 1", got)
	}
	// Only hit b was returned by a single engine.
	var unique dto.Metric
	if err := m.SearchEngineUnique.WithLabelValues("searchtoday").Write(&unique); err != nil {
		t.Fatal(err)
	}
	if got := unique.GetCounter().GetValue(); got != 1 {
		t.Fatalf("unique{searchtoday} = %v, want 1", got)
	}
}

// Unresponsive engines get one line each with engine and reason as separate
// fields — the joined WARN string cannot be aggregated.
func TestSearchAuditUnresponsiveIsOneLinePerEngine(t *testing.T) {
	body := `{"results":[],"unresponsive_engines":[
		["braveapi","Suspended: access denied"],["google cse","Suspended: too many requests"]]}`
	s, cap, _ := auditSearcher(t, "summary", body)
	if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	rows := cap.withMsg("search engine unresponsive")
	if len(rows) != 2 {
		t.Fatalf("unresponsive lines = %d, want 2", len(rows))
	}
	if rows[0]["engine"] != "braveapi" || rows[0]["reason_class"] != "access_denied" {
		t.Errorf("row 0 = %v", rows[0])
	}
	if rows[1]["engine"] != "google cse" || rows[1]["reason_class"] != "too_many_requests" {
		t.Errorf("row 1 = %v", rows[1])
	}
}

// Regression, code review of #35: an unknown rank must not swallow the unique
// count. Gating both on the rank under-counts exactly the engines whose
// positions SearXNG omits, and unique contribution is the number that decides
// whether an engine earns its slot.
func TestSearchUniqueCountedWithoutAKnownRank(t *testing.T) {
	body := `{"results":[{"title":"a","url":"https://example.com/a","engines":["privacywall"]}]}`
	s, _, m := auditSearcher(t, "off", body)
	if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	var unique dto.Metric
	if err := m.SearchEngineUnique.WithLabelValues("privacywall").Write(&unique); err != nil {
		t.Fatal(err)
	}
	if got := unique.GetCounter().GetValue(); got != 1 {
		t.Errorf("unique{privacywall} = %v, want 1 even with no positions array", got)
	}
	// ...but the histogram must still reject the unknown rank.
	var dm dto.Metric
	if err := m.SearchEnginePos.WithLabelValues("privacywall").(prometheus.Metric).Write(&dm); err != nil {
		t.Fatal(err)
	}
	if got := dm.GetHistogram().GetSampleCount(); got != 0 {
		t.Errorf("rank samples = %d, want 0 (rank unknown)", got)
	}
}

// Regression, code review of #35: a response carrying only the legacy `engine`
// field must still produce audit rows and metrics, or the audit contradicts
// omnifeed_searxng_engine_hits for the same search.
func TestSearchAuditFallsBackToLegacyEngineField(t *testing.T) {
	body := `{"results":[{"title":"a","url":"https://example.com/a","engine":"privacywall"}]}`
	s, cap, m := auditSearcher(t, "full", body)
	if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	lines := cap.withMsg("search audit")
	if len(lines) != 1 || lines[0]["engine_rows"] != "privacywall=1" {
		t.Fatalf("engine_rows = %v, want privacywall=1", lines[0]["engine_rows"])
	}
	rows := cap.withMsg("search result")
	if len(rows) != 1 || rows[0]["engine"] != "privacywall" || rows[0]["unique"] != true {
		t.Fatalf("position rows = %+v, want one unique privacywall row", rows)
	}
	var unique dto.Metric
	if err := m.SearchEngineUnique.WithLabelValues("privacywall").Write(&unique); err != nil {
		t.Fatal(err)
	}
	if got := unique.GetCounter().GetValue(); got != 1 {
		t.Errorf("unique{privacywall} = %v, want 1", got)
	}
}

// Regression, code review of #35: duration_ms must measure the upstream call,
// not time spent queued in the local pacing limiter. Otherwise an operator
// comparing pool latency is reading their own backpressure.
func TestSearchAuditDurationExcludesLimiterWait(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()
	cap := &capture{}
	// A delay far larger than the (empty) upstream call, so a duration that
	// included the queue would be unmistakable.
	limiter := httpx.NewDomainLimiter(1, 300*time.Millisecond)
	s := New(Config{
		Endpoint: srv.URL, Client: httpx.New(srv.Client()),
		Logger: slog.New(cap), Audit: "summary", Limiter: limiter,
	})
	for i := 0; i < 2; i++ { // the second query pays the pacing delay
		if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err != nil {
			t.Fatalf("Search: %v", err)
		}
	}
	lines := cap.withMsg("search audit")
	if len(lines) != 2 {
		t.Fatalf("audit lines = %d, want 2", len(lines))
	}
	if got := lines[1]["duration_ms"].(int); got >= 300 {
		t.Errorf("duration_ms = %d, want well under the 300ms pacing delay", got)
	}
}

// budgetLimiter is a limiter that always refuses with the pacing verdict, so a
// test can pin how a fast-fail reaches the caller without spending a real
// window.
type budgetLimiter struct {
	retryAfter time.Duration
	calls      int
}

func (b *budgetLimiter) Acquire(_ context.Context, _, _ string) (func(), error) {
	b.calls++
	return nil, &httpx.WaitBudgetError{RetryAfter: b.retryAfter}
}

// A refused pacing wait must reach the caller as reason quota_exhausted with the
// retry-after in the message — an AI agent reads that string and decides when to
// come back. It must NOT look like an upstream SearXNG failure, and the query
// must never be sent.
func TestSearchFastFailsWhenPacingExceedsMaxWait(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	defer srv.Close()

	limiter := &budgetLimiter{retryAfter: 12 * time.Second}
	s := New(Config{
		Endpoint: srv.URL,
		Client:   httpx.New(srv.Client()),
		Limiter:  limiter,
		MaxWait:  15 * time.Second,
	})

	start := time.Now()
	_, err := s.Search(context.Background(), "q", domain.SearchOptions{})
	if err == nil {
		t.Fatal("Search: want a fast-fail, got nil")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Search blocked for %v, want an immediate refusal", elapsed)
	}
	var fe *domain.FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("want a classified *domain.FetchError, got %T: %v", err, err)
	}
	if fe.Kind != domain.KindQuotaExhausted {
		t.Fatalf("Kind = %q, want %q", fe.Kind, domain.KindQuotaExhausted)
	}
	if got := observability.Reason(err); got != "quota_exhausted" {
		t.Fatalf("Reason = %q, want quota_exhausted", got)
	}
	if got := fe.Err.Error(); got != "pacing quota exhausted; retry in 12s" {
		t.Fatalf("message = %q, want the retry-after in seconds", got)
	}
	if limiter.calls != 1 {
		t.Fatalf("limiter calls = %d, want 1", limiter.calls)
	}
	if calls != 0 {
		t.Fatalf("server calls = %d, want 0 (nothing was admitted)", calls)
	}
}
