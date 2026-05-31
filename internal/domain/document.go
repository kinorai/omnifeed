// Package domain holds the core types exchanged between transports and engines.
// It has no external dependencies and no I/O.
package domain

import "context"

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
}

// Engine renders a single URL into a Document. Implementations should respect
// the caller-provided ctx for cancellation and deadlines.
type Engine interface {
	Name() string
	Matches(rawURL string) bool
	Crawl(ctx context.Context, rawURL string, opts EngineOptions) (Document, error)
}
