package router

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/observability"
)

// stubSearcher is a Searcher with a scripted answer, recording what it was asked.
type stubSearcher struct {
	name    string
	results []domain.SearchResult
	err     error
	calls   int
	gotQ    string
	gotOpts domain.SearchOptions
}

func (s *stubSearcher) Name() string { return s.name }

func (s *stubSearcher) Search(_ context.Context, query string, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	s.calls++
	s.gotQ, s.gotOpts = query, opts
	return s.results, s.err
}

func hit(title string) []domain.SearchResult {
	return []domain.SearchResult{{Title: title, URL: "https://example.com/" + title}}
}

// The router dispatches on the Site filter and falls back for every outcome but
// a non-empty success. The fallback always sees the caller's original query and
// options — a vertical must never narrow what the fallback is asked.
func TestSearchRoutesAndFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name         string
		site         string
		vertResults  []domain.SearchResult
		vertErr      error
		wantVertical int
		wantFallback int
		wantTitle    string
		wantOutcome  string
		wantWarn     bool
	}{
		{
			name: "vertical serves its results", site: "news.ycombinator.com",
			vertResults:  hit("vertical"),
			wantVertical: 1, wantFallback: 0, wantTitle: "vertical", wantOutcome: "served",
		},
		{
			name: "www. prefix still routes", site: "www.news.ycombinator.com",
			vertResults:  hit("vertical"),
			wantVertical: 1, wantFallback: 0, wantTitle: "vertical", wantOutcome: "served",
		},
		{
			name: "zero results fall back", site: "news.ycombinator.com",
			wantVertical: 1, wantFallback: 1, wantTitle: "fallback", wantOutcome: "empty",
		},
		{
			name: "a declining vertical falls back quietly", site: "news.ycombinator.com",
			vertErr:      domain.ErrSearchUnsupported,
			wantVertical: 1, wantFallback: 1, wantTitle: "fallback", wantOutcome: "declined",
		},
		{
			name: "a failing vertical falls back and warns", site: "news.ycombinator.com",
			vertErr:      errors.New("upstream exploded"),
			wantVertical: 1, wantFallback: 1, wantTitle: "fallback", wantOutcome: "error", wantWarn: true,
		},
		{
			name: "an unrouted site goes straight to the fallback", site: "example.com",
			wantVertical: 0, wantFallback: 1, wantTitle: "fallback",
		},
		{
			name: "no site goes straight to the fallback", site: "",
			wantVertical: 0, wantFallback: 1, wantTitle: "fallback",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vertical := &stubSearcher{name: "hackernews", results: tc.vertResults, err: tc.vertErr}
			fallback := &stubSearcher{name: "searxng", results: hit("fallback")}
			m := observability.NewMetrics()
			var logs bytes.Buffer
			r := New(Config{
				Verticals: map[string]domain.Searcher{"news.ycombinator.com": vertical},
				Fallback:  fallback,
				Logger:    slog.New(slog.NewTextHandler(&logs, nil)),
				Metrics:   m,
			})

			opts := domain.SearchOptions{Site: tc.site, Limit: 7, TimeRange: "week"}
			results, err := r.Search(context.Background(), "kubernetes plex", opts)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}

			if vertical.calls != tc.wantVertical || fallback.calls != tc.wantFallback {
				t.Fatalf("calls: vertical=%d fallback=%d, want %d/%d",
					vertical.calls, fallback.calls, tc.wantVertical, tc.wantFallback)
			}
			if len(results) != 1 || results[0].Title != tc.wantTitle {
				t.Fatalf("results = %+v, want the %q hit", results, tc.wantTitle)
			}
			if tc.wantFallback > 0 && (fallback.gotQ != "kubernetes plex" || fallback.gotOpts != opts) {
				t.Fatalf("fallback got q=%q opts=%+v, want the caller's query and options unchanged",
					fallback.gotQ, fallback.gotOpts)
			}
			if warned := bytes.Contains(logs.Bytes(), []byte("level=WARN")); warned != tc.wantWarn {
				t.Fatalf("warn logged = %v, want %v (log: %s)", warned, tc.wantWarn, logs.String())
			}
			assertRouteCounts(t, m, tc.wantOutcome)
		})
	}
}

// assertRouteCounts checks that exactly the wanted outcome was counted once for
// the hackernews vertical, and nothing else was counted at all. wantOutcome ""
// means no route metric may exist: an unrouted search is not a dispatch.
func assertRouteCounts(t *testing.T, m *observability.Metrics, wantOutcome string) {
	t.Helper()
	for _, outcome := range []string{"served", "empty", "declined", "error"} {
		var dm dto.Metric
		if err := m.SearchRoutes.WithLabelValues("hackernews", outcome).Write(&dm); err != nil {
			t.Fatal(err)
		}
		want := 0.0
		if outcome == wantOutcome {
			want = 1
		}
		if got := dm.GetCounter().GetValue(); got != want {
			t.Fatalf("routes{hackernews,%s} = %v, want %v", outcome, got, want)
		}
	}
}

func TestName(t *testing.T) {
	if got := New(Config{}).Name(); got != "router" {
		t.Fatalf("Name() = %q, want %q", got, "router")
	}
}
