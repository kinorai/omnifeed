// Package hackernews implements the Hacker News engine. It reads the public
// Algolia HN API (hn.algolia.com) and renders a front-page URL as a ranked story
// list and an /item?id= URL as a flattened comment tree (TOON) — mirroring the
// Reddit engine's output shape so nesting is reconstructable from parent_id.
//
// Unlike the Reddit and generic engines, this engine fetches its upstream
// DIRECTLY over HTTP rather than through crawl4ai: the Algolia HN API is a
// public, CORS-open JSON API with no bot wall, so a headless browser would only
// add latency. It does require omnifeed to have outbound access to hn.algolia.com.
package hackernews

// Story is one Hacker News front-page entry, stripped to LLM-relevant fields.
// No rank field: the Algolia feed is ordered by points, not HN's live gravity
// rank, so a positional rank would misrepresent the front page.
type Story struct {
	ID          int    `json:"id" toon:"id"`
	Title       string `json:"title" toon:"title"`
	URL         string `json:"url,omitempty" toon:"url,omitempty"`
	Author      string `json:"author" toon:"author"`
	Points      int    `json:"points" toon:"points"`
	NumComments int    `json:"num_comments" toon:"num_comments"`
	Created     int64  `json:"created" toon:"created"`
}

// FrontPage is a Hacker News listing (front page / newest / ask / show).
type FrontPage struct {
	Feed    string  `json:"feed" toon:"feed"`
	Stories []Story `json:"stories" toon:"stories"`
}

// Item is the story/post header of a thread.
type Item struct {
	ID      int    `json:"id" toon:"id"`
	Title   string `json:"title" toon:"title"`
	Author  string `json:"author" toon:"author"`
	URL     string `json:"url,omitempty" toon:"url,omitempty"`
	Points  int    `json:"points" toon:"points"`
	Created int64  `json:"created" toon:"created"`
	Text    string `json:"text,omitempty" toon:"text,omitempty"`
}

// Comment is a Hacker News comment, flattened with parent_id (like the Reddit
// engine) so the full nesting is reconstructable without deep indentation.
type Comment struct {
	ID       int    `json:"id" toon:"id"`
	ParentID int    `json:"parent_id" toon:"parent_id"`
	Author   string `json:"author" toon:"author"`
	Body     string `json:"body" toon:"body"`
	Created  int64  `json:"created" toon:"created"`
}

// Thread groups an HN story with its flattened comment tree.
type Thread struct {
	Story    Item      `json:"story" toon:"story"`
	Comments []Comment `json:"comments" toon:"comments"`
}

// --- Algolia HN API wire shapes (hn.algolia.com/api/v1) ---

// algoliaItem is the recursive shape of GET /items/{id}: a story (or comment)
// with a nested children tree. Deleted nodes arrive with empty author/text.
type algoliaItem struct {
	ID       int           `json:"id"`
	CreatedI int64         `json:"created_at_i"`
	Type     string        `json:"type"` // "story" | "comment" | "poll" | …
	Author   string        `json:"author"`
	Title    string        `json:"title"`
	URL      string        `json:"url"`
	Text     string        `json:"text"`
	Points   int           `json:"points"`
	ParentID int           `json:"parent_id"` // set on comments
	StoryID  int           `json:"story_id"`  // the enclosing story, set on comments
	Children []algoliaItem `json:"children"`
}

// algoliaSearch is GET /search (and /search_by_date): a flat list of hits.
type algoliaSearch struct {
	Hits []algoliaHit `json:"hits"`
}

type algoliaHit struct {
	ObjectID    string `json:"objectID"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Author      string `json:"author"`
	Points      int    `json:"points"`
	NumComments int    `json:"num_comments"`
	CreatedI    int64  `json:"created_at_i"`
}
