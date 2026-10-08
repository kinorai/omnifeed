package searxng

import (
	"context"
	"io"
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

// scrape returns the /metrics exposition, read the way Prometheus reads it:
// what is not there has no series, whatever the test helpers would create.
func scrape(t *testing.T, m *observability.Metrics) string {
	t.Helper()
	mux := http.NewServeMux()
	m.RegisterMetrics(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// sample returns the value of the exposition line starting with series, or
// "" when no such series is exposed.
func sample(body, series string) string {
	for _, line := range strings.Split(body, "\n") {
		if rest, ok := strings.CutPrefix(line, series+" "); ok {
			return rest
		}
	}
	return ""
}

// wantEngineSeriesAtZero asserts every per-engine series for engine is exposed
// at 0.
func wantEngineSeriesAtZero(t *testing.T, body, engine string) {
	t.Helper()
	l := `{engine="` + engine + `"}`
	for _, series := range []string{
		"omnifeed_searxng_engine_results_total" + l,
		"omnifeed_searxng_engine_zero_results_total" + l,
		"omnifeed_search_engine_unique_results_total" + l,
		"omnifeed_search_engine_position_rank_count" + l,
	} {
		if got := sample(body, series); got != "0" {
			t.Errorf("%s = %q, want 0", series, got)
		}
	}
	for _, errType := range engineErrorTypes {
		series := `omnifeed_searxng_unresponsive_engines_total{engine="` + engine + `",error="` + errType + `"}`
		if got := sample(body, series); got != "0" {
			t.Errorf("%s = %q, want 0", series, got)
		}
	}
}

func wantNoEngineSeries(t *testing.T, body, engine string) {
	t.Helper()
	if strings.Contains(body, `engine="`+engine+`"`) {
		t.Errorf("engine %q has series, want none", engine)
	}
}

// The error vocabulary minted at startup must cover every label value the
// search path can produce, or a restart still hides the first increment.
func TestEngineErrorTypesCoverNormalizeEngineError(t *testing.T) {
	known := map[string]bool{}
	for _, e := range engineErrorTypes {
		known[e] = true
	}
	for _, msg := range []string{"CAPTCHA required", "Suspended: too many requests", "access denied",
		"timeout", "Suspended: something", "HTTPError", ""} {
		if got := normalizeEngineError(msg); !known[got] {
			t.Errorf("normalizeEngineError(%q) = %q, not in engineErrorTypes", msg, got)
		}
	}
	if !known["unknown"] {
		t.Error(`engineErrorTypes lacks "unknown", countUnresponsive's label for a missing error type`)
	}
}

// The site engines are known from config: their series exist at 0 as soon as
// the searcher is built, before any search and with no /config round-trip.
func TestNewInitializesSiteEngineSeries(t *testing.T) {
	m := observability.NewMetrics()
	New(Config{
		Endpoint: "http://searxng.invalid", Client: httpx.New(nil), Metrics: m,
		SiteEngines: []string{"privacywall", "google cse"},
	})

	body := scrape(t, m)
	wantEngineSeriesAtZero(t, body, "privacywall")
	wantEngineSeriesAtZero(t, body, "google cse")
}

// configSearcher builds a searcher whose SearXNG fake answers /config with
// config and /search with search, and whose retry bounds are test-sized.
func configSearcher(t *testing.T, config http.HandlerFunc, search string) (*Searcher, *capture, *observability.Metrics) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config":
			config(w, r)
		case "/search":
			_, _ = io.WriteString(w, search)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	cap := &capture{}
	m := observability.NewMetrics()
	s := New(Config{Endpoint: srv.URL, Client: httpx.New(srv.Client()), Logger: slog.New(cap), Metrics: m})
	s.initBackoff, s.initMaxBackoff, s.initBudget = time.Millisecond, 4*time.Millisecond, 2*time.Second
	return s, cap, m
}

const configFixture = `{"engines":[
	{"name":"google cse","enabled":true,"categories":["general"]},
	{"name":"privacywall","enabled":true,"categories":["general"]},
	{"name":"bing","enabled":false,"categories":["general"]},
	{"name":"","enabled":true}
]}`

func countLevel(c *capture, level slog.Level) int {
	n := 0
	for _, r := range c.records {
		if r["_level"] == level {
			n++
		}
	}
	return n
}

// Only the engines SearXNG has enabled get series; disabled ones never run,
// and minting them would only add dead series.
func TestInitEngineMetricsMintsEnabledEnginesOnly(t *testing.T) {
	s, cap, m := configSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, configFixture)
	}, `{"results":[]}`)

	s.InitEngineMetrics(context.Background())

	body := scrape(t, m)
	wantEngineSeriesAtZero(t, body, "google cse")
	wantEngineSeriesAtZero(t, body, "privacywall")
	wantNoEngineSeries(t, body, "bing")
	if strings.Contains(body, `engine=""`) {
		t.Error("a nameless engine minted a series")
	}
	if n := countLevel(cap, slog.LevelWarn); n != 0 {
		t.Errorf("WARN lines = %d, want 0", n)
	}
}

// SearXNG may come up after omnifeed: /config failing at first must be
// retried, and the series minted once it answers.
func TestInitEngineMetricsRetriesUntilConfigAnswers(t *testing.T) {
	var calls atomic.Int32
	s, cap, m := configSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= 2 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, configFixture)
	}, `{"results":[]}`)

	s.InitEngineMetrics(context.Background())

	if got := calls.Load(); got != 3 {
		t.Fatalf("/config calls = %d, want 3 (two failures, one success)", got)
	}
	wantEngineSeriesAtZero(t, scrape(t, m), "google cse")
	if n := countLevel(cap, slog.LevelWarn); n != 0 {
		t.Errorf("WARN lines = %d, want 0 (a retry that succeeds is not a warning)", n)
	}
}

// /config never answering (or answering something unreadable) must not crash
// or hang: one WARN, then the lazy path is all there is.
func TestInitEngineMetricsGivesUpWithOneWarn(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"unreachable": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "down", http.StatusBadGateway)
		},
		"unexpected shape": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"brand":{}}`)
		},
		"not json": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `<html>`)
		},
		"not found": http.NotFound,
	} {
		t.Run(name, func(t *testing.T) {
			s, cap, m := configSearcher(t, handler, `{"results":[]}`)
			s.initBudget = 50 * time.Millisecond

			done := make(chan struct{})
			go func() { s.InitEngineMetrics(context.Background()); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("InitEngineMetrics did not return")
			}

			if n := countLevel(cap, slog.LevelWarn); n != 1 {
				t.Fatalf("WARN lines = %d, want exactly 1", n)
			}
			wantNoEngineSeries(t, scrape(t, m), "google cse")
		})
	}
}

// A connection that is refused outright is the same as an error answer.
func TestInitEngineMetricsToleratesARefusedConnection(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	endpoint := srv.URL
	srv.Close() // nothing listens there any more

	cap := &capture{}
	s := New(Config{Endpoint: endpoint, Client: httpx.New(nil), Logger: slog.New(cap),
		Metrics: observability.NewMetrics()})
	s.initBackoff, s.initMaxBackoff, s.initBudget = time.Millisecond, 4*time.Millisecond, 50*time.Millisecond

	s.InitEngineMetrics(context.Background())

	if n := countLevel(cap, slog.LevelWarn); n != 1 {
		t.Fatalf("WARN lines = %d, want exactly 1", n)
	}
}

// Shutdown must stop the retry loop at once, without a WARN.
func TestInitEngineMetricsStopsOnCancel(t *testing.T) {
	s, cap, _ := configSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}, `{"results":[]}`)
	s.initBackoff, s.initMaxBackoff, s.initBudget = time.Hour, time.Hour, 2*time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.InitEngineMetrics(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("InitEngineMetrics ignored cancellation")
	}
	if n := countLevel(cap, slog.LevelWarn); n != 0 {
		t.Errorf("WARN lines = %d, want 0", n)
	}
}

// The point of the fix: the series already sits at 0 when the first search
// lands, so the first increment is a delta increase() can see — not the
// first sample of a new series, which it cannot. Engines nobody named at
// startup still get their series lazily.
func TestFirstSearchIncrementsFromZero(t *testing.T) {
	s, _, m := configSearcher(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, configFixture)
	}, `{"results":[
		{"title":"a","url":"https://example.com/a","engine":"google cse","engines":["google cse"],"positions":[1]},
		{"title":"b","url":"https://example.com/b","engine":"google cse","engines":["google cse"],"positions":[2]},
		{"title":"c","url":"https://example.com/c","engine":"newcomer","engines":["newcomer"],"positions":[1]}
	]}`)

	s.InitEngineMetrics(context.Background())
	before := scrape(t, m)
	if got := sample(before, `omnifeed_searxng_engine_results_total{engine="google cse"}`); got != "0" {
		t.Fatalf("before the first search: results{google cse} = %q, want 0", got)
	}
	wantNoEngineSeries(t, before, "newcomer")

	if _, err := s.Search(context.Background(), "q", domain.SearchOptions{}); err != nil {
		t.Fatalf("Search: %v", err)
	}

	after := scrape(t, m)
	for series, want := range map[string]string{
		`omnifeed_searxng_engine_results_total{engine="google cse"}`:       "2",
		`omnifeed_search_engine_unique_results_total{engine="google cse"}`: "2",
		`omnifeed_search_engine_position_rank_count{engine="google cse"}`:  "2",
		`omnifeed_searxng_engine_results_total{engine="newcomer"}`:         "1",
	} {
		if got := sample(after, series); got != want {
			t.Errorf("after the first search: %s = %q, want %s", series, got, want)
		}
	}
}
