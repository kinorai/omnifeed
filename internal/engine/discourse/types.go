// Package discourse implements the Discourse engine. It reads a forum's public
// topic JSON API and renders a topic URL as topic header + the full post list
// (TOON) — mirroring the Hacker News and GitHub engines' shape.
//
// Like those two (and unlike Reddit and the generic engine), this engine fetches
// its upstream DIRECTLY over HTTP rather than through crawl4ai: the topic JSON is
// public and not bot-walled, and a browser render is both slower and lossy (posts
// are lazily paginated in the DOM — the benchmark's browser fallback returned the
// last 2 of 6 posts, buried in ~40% navigation chrome).
//
// Discourse is self-hosted software running on arbitrary domains, so there is no
// host pattern to match on. domain.Engine.Matches is a pure predicate — it cannot
// probe a host to find out whether it runs Discourse — so the set of hosts this
// engine claims is an explicit operator-supplied allowlist
// (OMNIFEED_DISCOURSE_HOSTS). Unlisted forums fall through to the generic
// browser fallback, which still renders them, just less completely.
package discourse

// Topic is the header of a Discourse topic, stripped to LLM-relevant fields.
type Topic struct {
	Title      string `json:"title" toon:"title"`
	PostsCount int    `json:"posts_count" toon:"posts_count"`
	Created    string `json:"created" toon:"created"`
	Host       string `json:"host" toon:"host"`
}

// Post is one post in the topic. ReplyTo keeps the reply structure
// reconstructable without nesting (the same flat+parent shape the Reddit, Hacker
// News, and GitHub engines use).
type Post struct {
	Number  int    `json:"number" toon:"number"`
	Login   string `json:"login" toon:"login"`
	Created string `json:"created" toon:"created"`
	ReplyTo int    `json:"reply_to,omitempty" toon:"reply_to,omitempty"`
	Body    string `json:"body" toon:"body"`
}

// Thread groups a topic with its posts, in post-stream order.
type Thread struct {
	Topic Topic  `json:"topic" toon:"topic"`
	Posts []Post `json:"posts" toon:"posts"`
}

// --- Discourse topic-JSON wire shapes ---

// apiTopic is GET /t/{id}.json (with or without print=true) and, for the batched
// fallback, GET /t/{id}/posts.json — which returns only the post_stream.posts
// half of the same envelope.
type apiTopic struct {
	Title      string        `json:"title"`
	PostsCount int           `json:"posts_count"`
	CreatedAt  string        `json:"created_at"`
	PostStream apiPostStream `json:"post_stream"`
}

// apiPostStream carries the posts of this response plus, on the non-print topic
// endpoint, Stream: the ids of EVERY post in the topic, in display order. It is
// the index the batched fallback pages through.
type apiPostStream struct {
	Posts  []apiPost `json:"posts"`
	Stream []int64   `json:"stream"`
}

// apiPost is one entry of post_stream.posts. Raw is the author's original
// markdown, present only with include_raw=1; Cooked is the rendered HTML that
// is always present.
type apiPost struct {
	ID                int64  `json:"id"`
	PostNumber        int    `json:"post_number"`
	Username          string `json:"username"`
	CreatedAt         string `json:"created_at"`
	ReplyToPostNumber int    `json:"reply_to_post_number"`
	Raw               string `json:"raw"`
	Cooked            string `json:"cooked"`
}
