package main

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/kinorai/omnifeed/internal/config"
	"github.com/kinorai/omnifeed/internal/observability"
)

// OMNIFEED_SEARCHERS is an order, and a listed searcher without its URL or key
// is skipped: the chain is what is configured, in the order given.
func TestBuildSearcherChain(t *testing.T) {
	const searxngURL, tinyfishKey = "http://searxng:8080", "key"
	tests := []struct {
		name        string
		searchers   []string
		searxng     string
		tinyfish    string
		wantName    string // "" = search disabled
		wantSearXNG bool
	}{
		{name: "nothing configured disables search", searchers: []string{"searxng", "tinyfish"}},
		{name: "searxng alone, as before", searchers: []string{"searxng", "tinyfish"}, searxng: searxngURL,
			wantName: "searxng", wantSearXNG: true},
		{name: "a key adds the backup", searchers: []string{"searxng", "tinyfish"}, searxng: searxngURL, tinyfish: tinyfishKey,
			wantName: "searxng+tinyfish", wantSearXNG: true},
		{name: "order is honoured", searchers: []string{"tinyfish", "searxng"}, searxng: searxngURL, tinyfish: tinyfishKey,
			wantName: "tinyfish+searxng", wantSearXNG: true},
		{name: "tinyfish alone", searchers: []string{"searxng", "tinyfish"}, tinyfish: tinyfishKey,
			wantName: "tinyfish"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			searcher, sx := buildSearcher(searchDeps{
				cfg: config.Config{
					Searchers:      tc.searchers,
					SearXNGURL:     tc.searxng,
					TinyFishAPIKey: tc.tinyfish,
					TinyFishURL:    "https://api.search.tinyfish.ai",
					SearchAudit:    "off",
				},
				metrics:   observability.NewMetrics(),
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
				transport: http.DefaultTransport,
			})
			gotName := ""
			if searcher != nil {
				gotName = searcher.Name()
			}
			if gotName != tc.wantName {
				t.Fatalf("chain = %q, want %q", gotName, tc.wantName)
			}
			if (sx != nil) != tc.wantSearXNG {
				t.Fatalf("searxng searcher returned = %v, want %v", sx != nil, tc.wantSearXNG)
			}
		})
	}
}
