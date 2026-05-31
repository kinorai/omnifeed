// Package reddit implements the Reddit-specific engine: fetches threads via
// the public JSON API, expands collapsed reply branches with /api/morechildren,
// strips fields, drops deleted comments, and emits TOON or JSON.
package reddit

import "encoding/json"

// Reddit "thing" type prefixes. We only emit posts (t3) and comments (t1).
const (
	kindCommentPrefix = "t1_"
	kindPostPrefix    = "t3_"
)

// --- Output shape returned to callers ---

type Post struct {
	ID          string  `json:"id" toon:"id"`
	Title       string  `json:"title" toon:"title"`
	Author      string  `json:"author" toon:"author"`
	Subreddit   string  `json:"subreddit" toon:"subreddit"`
	Score       int     `json:"score" toon:"score"`
	UpvoteRatio float64 `json:"upvote_ratio" toon:"upvote_ratio"`
	NumComments int     `json:"num_comments" toon:"num_comments"`
	Created     int64   `json:"created" toon:"created"`
	URL         string  `json:"url" toon:"url"`
	Selftext    string  `json:"selftext,omitempty" toon:"selftext,omitempty"`
	Permalink   string  `json:"permalink" toon:"permalink"`
}

type Comment struct {
	ID       string `json:"id" toon:"id"`
	ParentID string `json:"parent_id" toon:"parent_id"`
	Author   string `json:"author" toon:"author"`
	Score    int    `json:"score" toon:"score"`
	Body     string `json:"body" toon:"body"`
	Depth    *int   `json:"depth,omitempty" toon:"depth,omitempty"`
	Created  *int64 `json:"created,omitempty" toon:"created,omitempty"`
}

type Gap struct {
	Type     string   `json:"type" toon:"type"`
	ParentID string   `json:"parent_id" toon:"parent_id"`
	Depth    int      `json:"depth" toon:"depth"`
	Count    int      `json:"count,omitempty" toon:"count,omitempty"`
	Children []string `json:"children,omitempty" toon:"children,omitempty"`
}

type Thread struct {
	Post     Post      `json:"post" toon:"post"`
	Comments []Comment `json:"comments" toon:"comments"`
	Gaps     []Gap     `json:"gaps,omitempty" toon:"gaps,omitempty"`
}

// --- Raw Reddit wire shapes ---

type rawListingWrap struct {
	Kind string         `json:"kind"`
	Data rawListingData `json:"data"`
}

type rawListingData struct {
	Children []json.RawMessage `json:"children"`
}

type rawChildKind struct {
	Kind string `json:"kind"`
}

type rawPost struct {
	Kind string      `json:"kind"`
	Data rawPostData `json:"data"`
}

type rawPostData struct {
	ID          string  `json:"id"`
	Title       string  `json:"title"`
	Author      string  `json:"author"`
	Subreddit   string  `json:"subreddit"`
	Score       int     `json:"score"`
	UpvoteRatio float64 `json:"upvote_ratio"`
	NumComments int     `json:"num_comments"`
	CreatedUTC  float64 `json:"created_utc"`
	URL         string  `json:"url"`
	Selftext    string  `json:"selftext"`
	Permalink   string  `json:"permalink"`
}

type rawComment struct {
	Kind string         `json:"kind"`
	Data rawCommentData `json:"data"`
}

type rawCommentData struct {
	ID         string          `json:"id"`
	ParentID   string          `json:"parent_id"`
	LinkID     string          `json:"link_id"`
	Author     string          `json:"author"`
	Score      int             `json:"score"`
	Body       string          `json:"body"`
	Depth      int             `json:"depth"`
	CreatedUTC float64         `json:"created_utc"`
	Replies    json.RawMessage `json:"replies"`
}

type rawMore struct {
	Kind string      `json:"kind"`
	Data rawMoreData `json:"data"`
}

type rawMoreData struct {
	Count    int      `json:"count"`
	ParentID string   `json:"parent_id"`
	Depth    int      `json:"depth"`
	Children []string `json:"children"`
}

// Options carries Reddit-specific per-request knobs derived from query strings
// or env-var defaults.
type Options struct {
	KeepDepth   bool   // include depth field on each comment
	KeepCreated bool   // include created field on each comment
	MaxRounds   int    // hard cap on /api/morechildren expansion rounds
	Format      string // "toon" or "json"
}
