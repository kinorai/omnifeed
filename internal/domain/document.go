// Package domain holds the core types exchanged between transports and engines.
// It has no external dependencies and no I/O.
package domain

import (
	"context"
	"slices"
)

// Document is the canonical shape returned by every engine and re-serialized
// by every transport. The field names match Open WebUI's external-loader
// contract so the OpenWebUI transport can serialize directly.
type Document struct {
	PageContent string            `json:"page_content"`
	Metadata    map[string]string `json:"metadata"`
}

// EngineOptions carries per-request knobs an engine may honor. Unknown fields
// are ignored by engines that don't care.
type EngineOptions struct {
	// Reddit-specific.
	RedditKeepDepth   bool   // include depth field on comments
	RedditKeepCreated bool   // include created field on comments
	RedditMaxRounds   int    // /api/morechildren expansion budget
	RedditFormat      string // "toon" | "json"
	RedditFetchLimit  int    // Reddit `limit`: max comments in initial fetch (0 = engine default)
	RedditDepth       int    // Reddit `depth`: max nesting depth (0 = engine default)
	RedditSort        string // Reddit `sort`: comment sort order ("" = engine default)
	RedditMaxComments int    // hard cap on total comments emitted (0 = unlimited)
	RedditMaxTopLevel int    // hard cap on top-level threads (0 = unlimited)

	// Generic-crawl (crawl4ai fallback) knobs.
	//
	// ScanFullPage scrolls the whole page before extraction so append-style
	// infinite feeds load. Tri-state: nil = the deployment default
	// (OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE); callers opt in per URL — it costs
	// multiple seconds and corrupts virtualized pages, so it's for feed/gallery
	// URLs specifically.
	ScanFullPage *bool
}

// Reddit comment-fetch defaults are defined here so config (env fallback) and
// the reddit engine (zero-Options floor) share one source instead of each
// duplicating the literals.
// https://www.reddit.com/dev/api/#GET_comments_{article}
const (
	DefaultRedditFetchLimit = 500   // Reddit `limit`
	DefaultRedditDepth      = 20    // Reddit `depth`
	DefaultRedditSort       = "top" // Reddit `sort`
)

// ValidRedditSorts are the comment sort orders Reddit's comments endpoint
// accepts; "confidence" is what the Reddit UI labels "best". Kept here so config
// and the reddit engine validate against one list.
// https://www.reddit.com/dev/api/#GET_comments_{article}
var ValidRedditSorts = []string{"confidence", "top", "new", "controversial", "old", "random", "qa", "live"}

// ValidRedditSort reports whether s is an accepted Reddit comment sort order.
func ValidRedditSort(s string) bool { return slices.Contains(ValidRedditSorts, s) }

// ValidTimeRanges are the recency-window filters the web_search front-ends
// accept (forwarded to SearXNG); "" means no filter. Kept here so the MCP and
// REST transports validate against one list.
var ValidTimeRanges = []string{"day", "week", "month", "year"}

// ValidTimeRange reports whether s is an accepted search recency window.
func ValidTimeRange(s string) bool { return slices.Contains(ValidTimeRanges, s) }

// Engine renders a single URL into a Document. Implementations should respect
// the caller-provided ctx for cancellation and deadlines.
type Engine interface {
	Name() string
	Matches(rawURL string) bool
	Crawl(ctx context.Context, rawURL string, opts EngineOptions) (Document, error)
}
