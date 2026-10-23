package searxng

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

func TestDegradation(t *testing.T) {
	cases := []struct {
		name         string
		critical     []string
		rows         map[string]int
		unresponsive [][]string
		want         string
	}{
		{name: "no critical engines never degrades", rows: map[string]int{}, want: ""},
		{
			name:     "critical engine answered",
			critical: []string{"google cse"},
			rows:     map[string]int{"google cse": 4, "yandex": 6},
			want:     "",
		},
		{
			name:         "critical engine rate limited",
			critical:     []string{"google cse"},
			rows:         map[string]int{"privacywall": 9},
			unresponsive: [][]string{{"google cse", "Suspended: too many requests"}},
			want:         "google cse: too_many_requests",
		},
		{
			name:     "critical engine silently empty",
			critical: []string{"google cse"},
			rows:     map[string]int{"privacywall": 9},
			want:     "google cse: no results",
		},
		{
			name:         "unresponsive pair without a message",
			critical:     []string{"google cse"},
			unresponsive: [][]string{{"google cse"}},
			want:         "google cse: error",
		},
		{
			name:     "first missing critical engine is named",
			critical: []string{"google cse", "braveapi"},
			rows:     map[string]int{"google cse": 3},
			want:     "braveapi: no results",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := degradation(tc.critical, tc.rows, tc.unresponsive); got != tc.want {
				t.Fatalf("degradation = %q, want %q", got, tc.want)
			}
		})
	}
}

// A critical engine outside SiteEngines is never asked on a site-scoped
// search, so its absence there must not report the search degraded.
func TestSearch_DegradedReportRespectsSiteEngines(t *testing.T) {
	const body = `{"results": [{"title": "T", "url": "https://www.reddit.com/r/x/", "engine": "privacywall"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	cases := []struct {
		name        string
		siteEngines []string
		site        string
		want        string
	}{
		{name: "unscoped search asks the critical engine", want: "google cse: no results"},
		{name: "scoped to engines that exclude it", siteEngines: []string{"privacywall"}, site: "reddit.com", want: ""},
		{name: "scoped to engines that include it", siteEngines: []string{"privacywall", "google cse"}, site: "reddit.com", want: "google cse: no results"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{
				Endpoint:        srv.URL,
				Client:          httpx.New(srv.Client()),
				SiteEngines:     tc.siteEngines,
				CriticalEngines: []string{"google cse"},
			})
			report, err := s.Search(context.Background(), "q", domain.SearchOptions{Site: tc.site})
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if report.DegradedReason != tc.want {
				t.Fatalf("DegradedReason = %q, want %q", report.DegradedReason, tc.want)
			}
		})
	}
}
