package reddit

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	// wsNewlineRunRE matches a run of one or more newlines plus surrounding
	// horizontal whitespace. We use ReplaceAllStringFunc to clamp newline
	// count to [1, 2]: single \n stays \n, 2+ collapse to \n\n. This
	// preserves paragraph breaks and code fence boundaries while killing
	// blank-line spam.
	wsNewlineRunRE = regexp.MustCompile(`[ \t\f\v]*(?:\n[ \t\f\v]*)+`)
	wsHorizontalRE = regexp.MustCompile(`[ \t\f\v]{2,}`)

	// permalinkRE extracts the canonical /r/{sub}/comments/{id}[/{slug}] form.
	// [^/?#]+ excludes URL-control chars so we can't be tricked into
	// injecting query params via the slug.
	permalinkRE = regexp.MustCompile(`(/r/[^/]+/comments/[a-z0-9]+(?:/[^/?#]+)?)/?`)
)

// ParseThread decodes the 2-element listing array from .json and produces a
// Thread with all initial comments and gap markers.
func ParseThread(raw []byte, opts Options) (Thread, error) {
	var listings []rawListingWrap
	if err := json.Unmarshal(raw, &listings); err != nil {
		return Thread{}, err
	}
	if len(listings) < 2 {
		return Thread{}, fmt.Errorf("expected 2-element listing, got %d", len(listings))
	}
	if len(listings[0].Data.Children) == 0 {
		return Thread{}, fmt.Errorf("no post in first listing")
	}

	var post rawPost
	if err := json.Unmarshal(listings[0].Data.Children[0], &post); err != nil {
		return Thread{}, err
	}

	thread := Thread{
		Post: Post{
			ID:          post.Data.ID,
			Title:       post.Data.Title,
			Author:      post.Data.Author,
			Subreddit:   post.Data.Subreddit,
			Score:       post.Data.Score,
			UpvoteRatio: post.Data.UpvoteRatio,
			NumComments: post.Data.NumComments,
			Created:     int64(post.Data.CreatedUTC),
			URL:         post.Data.URL,
			Selftext:    cleanBody(post.Data.Selftext),
			Permalink:   post.Data.Permalink,
		},
	}

	comments, gaps := walkChildren(listings[1].Data.Children, opts)
	thread.Comments = comments
	thread.Gaps = gaps
	return thread, nil
}

// ParseMoreChildren parses the /api/morechildren response: { json: { data: { things: [...] } } }
func ParseMoreChildren(raw []byte, opts Options) ([]Comment, []Gap, error) {
	var wrap struct {
		JSON struct {
			Errors []json.RawMessage `json:"errors"`
			Data   struct {
				Things []json.RawMessage `json:"things"`
			} `json:"data"`
		} `json:"json"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, nil, err
	}
	c, g := walkChildren(wrap.JSON.Data.Things, opts)
	return c, g, nil
}

func walkChildren(children []json.RawMessage, opts Options) ([]Comment, []Gap) {
	var comments []Comment
	var gaps []Gap
	for _, c := range children {
		var k rawChildKind
		if err := json.Unmarshal(c, &k); err != nil {
			continue
		}
		switch k.Kind {
		case "t1":
			var ct rawComment
			if err := json.Unmarshal(c, &ct); err != nil {
				continue
			}
			// Skip deleted/removed — they're pure noise to LLM consumers.
			// Still recurse into replies in case the deleted parent has
			// live children worth keeping.
			if !isRemovedComment(ct.Data) {
				comments = append(comments, makeComment(ct.Data, opts))
			}
			if hasRepliesListing(ct.Data.Replies) {
				var sub rawListingWrap
				if err := json.Unmarshal(ct.Data.Replies, &sub); err == nil {
					subC, subG := walkChildren(sub.Data.Children, opts)
					comments = append(comments, subC...)
					gaps = append(gaps, subG...)
				}
			}
		case "more":
			var mr rawMore
			if err := json.Unmarshal(c, &mr); err != nil {
				continue
			}
			gaps = append(gaps, makeGap(mr.Data))
		}
	}
	return comments, gaps
}

// hasRepliesListing returns true when the raw `replies` field is a non-empty
// JSON object (Listing). Reddit emits `""` for comments with no replies.
func hasRepliesListing(raw json.RawMessage) bool {
	if len(raw) < 2 {
		return false
	}
	return raw[0] == '{'
}

func makeComment(d rawCommentData, opts Options) Comment {
	c := Comment{
		ID:       d.ID,
		ParentID: stripFullnamePrefix(d.ParentID),
		Author:   d.Author,
		Score:    d.Score,
		Body:     cleanBody(d.Body),
	}
	if opts.KeepDepth {
		depth := d.Depth
		c.Depth = &depth
	}
	if opts.KeepCreated {
		t := int64(d.CreatedUTC)
		c.Created = &t
	}
	return c
}

func makeGap(d rawMoreData) Gap {
	g := Gap{
		ParentID: stripFullnamePrefix(d.ParentID),
		Depth:    d.Depth,
	}
	if d.Count == 0 {
		g.Type = "continue"
	} else {
		g.Type = "more"
		g.Count = d.Count
		g.Children = d.Children
	}
	return g
}

// MergeExpanded folds newly-fetched comments/gaps into the existing thread,
// dedupes by ID, removes fulfilled child IDs from the gaps that triggered the
// fetch, and drops gaps that no longer carry any children.
func MergeExpanded(thread *Thread, newC []Comment, newG []Gap, requestedIDs []string, usedGapIdx []int) {
	seen := make(map[string]bool, len(thread.Comments)+len(newC))
	for _, c := range thread.Comments {
		seen[c.ID] = true
	}
	for _, c := range newC {
		if seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		thread.Comments = append(thread.Comments, c)
	}

	fulfilled := make(map[string]bool, len(requestedIDs))
	for _, id := range requestedIDs {
		fulfilled[id] = true
	}
	for _, idx := range usedGapIdx {
		if idx >= len(thread.Gaps) {
			continue
		}
		remaining := thread.Gaps[idx].Children[:0]
		for _, child := range thread.Gaps[idx].Children {
			if !fulfilled[child] {
				remaining = append(remaining, child)
			}
		}
		thread.Gaps[idx].Children = remaining
		thread.Gaps[idx].Count = len(remaining)
	}

	keep := thread.Gaps[:0]
	for _, g := range thread.Gaps {
		if g.Type == "more" && len(g.Children) == 0 {
			continue
		}
		keep = append(keep, g)
	}
	thread.Gaps = keep

	thread.Gaps = append(thread.Gaps, newG...)
}

// capComments truncates the comment list to at most n entries, preserving order
// (0 = unlimited). The list is depth-first pre-order for the initial fetch, so
// ancestors precede their descendants and a prefix cut never orphans a kept
// reply from a dropped parent; expansion appends deeper comments at the tail, so
// those — the least central — are dropped first.
func capComments(t *Thread, n int) {
	if n > 0 && len(t.Comments) > n {
		t.Comments = t.Comments[:n]
	}
}

// capTopLevel keeps only the first n top-level comment threads (in Reddit's sort
// order) and every reply beneath them, dropping the rest (0 = unlimited). A
// comment is top-level when its parent is the post itself. Comments whose parent
// chain can't be resolved to a top-level root (e.g. a deleted parent was skipped
// during parsing) are kept rather than risk dropping live content.
func capTopLevel(t *Thread, n int) {
	if n <= 0 {
		return
	}
	postID := t.Post.ID
	parent := make(map[string]string, len(t.Comments))
	for _, c := range t.Comments {
		parent[c.ID] = c.ParentID
	}

	kept := make(map[string]bool)
	roots := 0
	for _, c := range t.Comments {
		if c.ParentID == postID {
			if roots < n {
				kept[c.ID] = true
			}
			roots++
		}
	}
	if roots <= n {
		return // fewer top-level threads than the cap — nothing to drop
	}

	// rootKept walks up to the top-level ancestor and reports whether it
	// survived the cut. A broken chain (parent absent) or a cycle keeps the
	// comment.
	rootKept := func(id string) bool {
		cur := id
		for hops := 0; hops <= len(t.Comments); hops++ {
			p, ok := parent[cur]
			if !ok {
				return true
			}
			if p == postID {
				return kept[cur]
			}
			cur = p
		}
		return true
	}

	filtered := t.Comments[:0]
	for _, c := range t.Comments {
		if rootKept(c.ID) {
			filtered = append(filtered, c)
		}
	}
	t.Comments = filtered
}

func stripFullnamePrefix(s string) string {
	if strings.HasPrefix(s, kindCommentPrefix) || strings.HasPrefix(s, kindPostPrefix) {
		return s[3:]
	}
	return s
}

// isRemovedComment returns true for placeholder rows where the author was
// scrubbed and the body is one of Reddit's standard removal markers. These
// rows have no content for an LLM and burn ~30 tokens each.
func isRemovedComment(d rawCommentData) bool {
	if d.Author != "[deleted]" && d.Author != "" {
		return false
	}
	switch d.Body {
	case "", "[removed]", "[deleted]", "[ Removed by Reddit ]":
		return true
	}
	return false
}

// cleanBody normalizes Reddit body whitespace for token efficiency without
// destroying markdown structure: collapses horizontal whitespace, clamps
// newline runs to 1 (single source) or 2 (paragraph break), and trims.
func cleanBody(s string) string {
	if s == "" {
		return s
	}
	s = wsHorizontalRE.ReplaceAllString(s, " ")
	s = wsNewlineRunRE.ReplaceAllStringFunc(s, func(match string) string {
		if strings.Count(match, "\n") >= 2 {
			return "\n\n"
		}
		return "\n"
	})
	return strings.TrimSpace(s)
}

// NormalizePermalink converts any reddit.com URL into the canonical
// /r/{sub}/comments/{id}[/{slug}] permalink fragment (no trailing slash, no
// .json suffix).
func NormalizePermalink(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	path := strings.TrimSuffix(u.Path, "/")
	path = strings.TrimSuffix(path, ".json")
	m := permalinkRE.FindStringSubmatch(path)
	if len(m) < 2 {
		return "", fmt.Errorf("not a comments URL: %s", rawURL)
	}
	return m[1], nil
}

// listingRE matches a subreddit listing path — /r/{sub} optionally followed by a
// sort — and nothing deeper. It deliberately does NOT match /r/{sub}/comments/…
// (a thread), /r/{sub}/s/… (a share link), /r/{sub}/wiki/…, or /user/…; those
// keep falling through to the comments path or the generic fallback.
// listingRE matches a subreddit listing path after the trailing slash and any
// `.json` suffix are stripped. The sub capture excludes `/?#` (like permalinkRE)
// so an injected query/fragment can't slip into the fetch URL, and the sort
// alternation lists only the real subreddit sorts — `best` is a site-wide feed,
// not a subreddit sort, so /r/sub/best falls through to the generic engine.
var listingRE = regexp.MustCompile(`(?i)^/r/([^/?#]+)(?:/(hot|new|top|rising|controversial))?$`)

// ParseListingURL extracts the subreddit and sort from a subreddit listing URL.
// Sort defaults to "hot" (Reddit's default view) when the path omits it. ok is
// false for any reddit.com URL that isn't a bare subreddit listing.
func ParseListingURL(rawURL string) (sub, sort string, ok bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", false
	}
	// Strip a trailing slash and an optional `.json` suffix — people paste both
	// `/r/go` and `/r/go.json` — mirroring NormalizePermalink's handling so the
	// listing path doesn't capture `.json` into the subreddit name.
	path := strings.TrimSuffix(u.Path, "/")
	path = strings.TrimSuffix(path, ".json")
	m := listingRE.FindStringSubmatch(path)
	if m == nil {
		return "", "", false
	}
	sort = strings.ToLower(m[2])
	if sort == "" {
		sort = "hot"
	}
	return m[1], sort, true
}

// ParseSubredditListing decodes a subreddit listing (.json) — a single Listing
// whose children are t3 posts — into a slice of Posts (no comment trees).
func ParseSubredditListing(raw []byte) ([]Post, error) {
	var wrap rawListingWrap
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, err
	}
	posts := make([]Post, 0, len(wrap.Data.Children))
	for _, c := range wrap.Data.Children {
		var p rawPost
		if err := json.Unmarshal(c, &p); err != nil || p.Kind != "t3" {
			continue
		}
		posts = append(posts, Post{
			ID:          p.Data.ID,
			Title:       p.Data.Title,
			Author:      p.Data.Author,
			Subreddit:   p.Data.Subreddit,
			Score:       p.Data.Score,
			UpvoteRatio: p.Data.UpvoteRatio,
			NumComments: p.Data.NumComments,
			Created:     int64(p.Data.CreatedUTC),
			URL:         p.Data.URL,
			Selftext:    cleanBody(p.Data.Selftext),
			Permalink:   p.Data.Permalink,
		})
	}
	return posts, nil
}

// shareRE matches Reddit share links: /r/{sub}/s/{code}. They 301-redirect to
// the canonical /comments/ permalink and must be resolved before fetching.
var shareRE = regexp.MustCompile(`(?i)^/r/[^/]+/s/[A-Za-z0-9_-]+/?$`)

// IsShareURL reports whether rawURL is a Reddit share link needing resolution.
func IsShareURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return shareRE.MatchString(u.Path)
}
