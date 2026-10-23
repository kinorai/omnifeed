package domain

import (
	"context"
	"errors"
)

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
	// Site restricts results to one hostname. Naming the site inside the query
	// text instead ("reddit kubernetes plex") does not work: the engines read
	// the site name as a topic word and return the site's own homepage and its
	// Wikipedia article. Must satisfy ValidSiteFilter; empty = no restriction.
	Site string
}

// ErrInvalidQuery marks a search refused for its input (an empty query, ...)
// before any upstream was asked. No other searcher would answer it either, so
// it is never failed over.
var ErrInvalidQuery = errors.New("invalid search query")

// SearchReport is a Searcher's answer to one query: the ranked results, and
// whether the upstream answered at full strength.
type SearchReport struct {
	Results []SearchResult
	// DegradedReason is set when the search succeeded but a source the
	// operator relies on did not contribute — e.g. an engine that was rate
	// limited or silently blocked — so the results are weaker than usual. It
	// names the source and why, for logs and metrics. Empty means the search
	// ran at full strength.
	DegradedReason string
}

// Degraded reports whether the search ran without a source it relies on.
func (r SearchReport) Degraded() bool { return r.DegradedReason != "" }

// Searcher turns a query into ranked result URLs. It is the discovery
// counterpart of Engine: a Searcher finds URLs (query → results), Engine
// implementations then render them (URL → content).
type Searcher interface {
	Name() string
	Search(ctx context.Context, query string, opts SearchOptions) (SearchReport, error)
}
