// Package bluesky implements the Bluesky engine. It reads the public AT
// Protocol AppView (public.api.bsky.app) and renders a bsky.app post URL as a
// flattened reply tree and a profile URL as that account's recent posts (TOON)
// — mirroring the Reddit and Hacker News engines' output shape, so nesting is
// reconstructable from parent_uri.
//
// Like the Hacker News and GitHub engines it fetches its upstream DIRECTLY over
// HTTP rather than through crawl4ai: the AppView is a public, keyless JSON API,
// while bsky.app itself is a client-side SPA that a headless browser renders as
// a near-empty shell. It does require omnifeed to have outbound access to
// public.api.bsky.app.
//
// SCOPE: post threads and author feeds only. app.bsky.feed.searchPosts —
// keyword search across the network — is NOT used: the AppView answers it with
// HTTP 403 to unauthenticated callers (its own lexicon warns the endpoint "may
// require authentication (eg, not public)"), while getPostThread,
// getAuthorFeed and getProfile on the very same host stay open. Topic discovery
// on Bluesky therefore still needs a signed-in client or web search; this
// engine covers reading a post or an account you already have a URL for.
package bluesky

// Author is the account that wrote a post, stripped to LLM-relevant fields.
type Author struct {
	Handle string `json:"handle" toon:"handle"`
	Name   string `json:"name,omitempty" toon:"name,omitempty"`
}

// Post is one Bluesky post rendered for an LLM. URI is the AT-URI, which is
// also the join key: a reply's ParentURI names the post it answers.
type Post struct {
	URI       string `json:"uri" toon:"uri"`
	ParentURI string `json:"parent_uri,omitempty" toon:"parent_uri,omitempty"`
	Author    Author `json:"author" toon:"author"`
	Text      string `json:"text" toon:"text"`
	CreatedAt string `json:"created_at" toon:"created_at"`
	Replies   int    `json:"replies" toon:"replies"`
	Reposts   int    `json:"reposts" toon:"reposts"`
	Likes     int    `json:"likes" toon:"likes"`
	Quotes    int    `json:"quotes,omitempty" toon:"quotes,omitempty"`
	// Link is the URL of an external page the post embeds, when it has one.
	// Bluesky posts are short and often exist only to point somewhere else, so
	// dropping the embed would lose the post's whole payload.
	Link string `json:"link,omitempty" toon:"link,omitempty"`
}

// Thread groups a Bluesky post with its ancestors and its flattened reply tree.
type Thread struct {
	// Ancestors are the posts this one replies to, root first. A bsky.app URL
	// for a reply is indistinguishable from one for a root post, so without
	// these a linked reply would arrive with no sight of what it answers.
	Ancestors []Post `json:"ancestors,omitempty" toon:"ancestors,omitempty"`
	Post      Post   `json:"post" toon:"post"`
	Replies   []Post `json:"replies" toon:"replies"`
}

// Feed is an account's recent posts.
type Feed struct {
	Actor string `json:"actor" toon:"actor"`
	Posts []Post `json:"posts" toon:"posts"`
}

// --- AppView wire shapes (public.api.bsky.app/xrpc) ---

// threadResponse is GET app.bsky.feed.getPostThread.
type threadResponse struct {
	Thread threadViewPost `json:"thread"`
}

// threadViewPost is the recursive thread node. A notFoundPost or blockedPost
// arrives as the same object with no Post field, which decodes to the zero
// value and is skipped by URI emptiness.
//
// Parent walks UP the reply chain (Replies walks down). It is a pointer
// because the chain has to terminate: the root post has no parent.
type threadViewPost struct {
	Post    postView         `json:"post"`
	Parent  *threadViewPost  `json:"parent"`
	Replies []threadViewPost `json:"replies"`
}

// feedResponse is GET app.bsky.feed.getAuthorFeed.
type feedResponse struct {
	Feed []feedItem `json:"feed"`
}

type feedItem struct {
	Post postView `json:"post"`
}

type postView struct {
	URI    string     `json:"uri"`
	Author authorView `json:"author"`
	Record record     `json:"record"`
	Embed  embed      `json:"embed"`

	ReplyCount  int `json:"replyCount"`
	RepostCount int `json:"repostCount"`
	LikeCount   int `json:"likeCount"`
	QuoteCount  int `json:"quoteCount"`
}

type authorView struct {
	Handle      string `json:"handle"`
	DisplayName string `json:"displayName"`
}

// record is the post record itself: the text and its creation time live here,
// not on the view.
type record struct {
	Text      string `json:"text"`
	CreatedAt string `json:"createdAt"`
}

// embed carries an external link when the post has one. Only the external
// variant is decoded; image and quote-post embeds have no URL to surface.
type embed struct {
	External struct {
		URI string `json:"uri"`
	} `json:"external"`
}
