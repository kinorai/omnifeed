package domain

import "context"

// SearchResult is a single hit returned by a Searcher.
type SearchResult struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Snippet       string `json:"snippet,omitempty"`
	Engine        string `json:"engine,omitempty"`
	PublishedDate string `json:"published_date,omitempty"`
}

// SearchOptions carries per-query knobs a Searcher may honor.
type SearchOptions struct {
	Limit     int    // max results to return; <= 0 means no clamp
	TimeRange string // "", "day", "week", "month", "year"
	Language  string // e.g. "en", "fr"; empty = upstream default
}

// Searcher turns a query into ranked result URLs. It is the discovery
// counterpart of Engine: a Searcher finds URLs (query → results), Engine
// implementations then render them (URL → content).
type Searcher interface {
	Name() string
	Search(ctx context.Context, query string, opts SearchOptions) ([]SearchResult, error)
}
