package bluesky

import "encoding/json"

// toPost flattens one AppView post view into the emitted shape.
func toPost(p postView, parentURI string) Post {
	return Post{
		URI:       p.URI,
		ParentURI: parentURI,
		Author: Author{
			Handle: p.Author.Handle,
			Name:   p.Author.DisplayName,
		},
		Text:      p.Record.Text,
		CreatedAt: p.Record.CreatedAt,
		Replies:   p.ReplyCount,
		Reposts:   p.RepostCount,
		Likes:     p.LikeCount,
		Quotes:    p.QuoteCount,
		Link:      p.Embed.External.URI,
	}
}

// flattenReplies walks the reply tree in pre-order and appends each readable
// reply with its parent_uri — the same flat+parent shape the Reddit and Hacker
// News engines emit. Unreadable nodes (deleted, blocked, or by an account that
// blocks the viewer) arrive with an empty URI: they are skipped, but recursion
// still descends through them so live replies underneath survive.
func flattenReplies(nodes []threadViewPost, parentURI string, out *[]Post) {
	for i := range nodes {
		n := &nodes[i]
		next := parentURI
		if n.Post.URI != "" {
			*out = append(*out, toPost(n.Post, parentURI))
			next = n.Post.URI
		}
		if len(n.Replies) > 0 {
			flattenReplies(n.Replies, next, out)
		}
	}
}

// collectAncestors walks the parent chain up from node and returns the posts it
// replies to, ROOT FIRST — the order they read in. Unreadable links (deleted or
// blocked, so no URI) are skipped, and the walk continues past them: a broken
// link partway up must not hide the rest of the conversation.
func collectAncestors(node *threadViewPost) []Post {
	var reversed []Post
	for p := node.Parent; p != nil; p = p.Parent {
		if p.Post.URI == "" {
			continue
		}
		parentURI := ""
		if p.Parent != nil {
			parentURI = p.Parent.Post.URI
		}
		reversed = append(reversed, toPost(p.Post, parentURI))
	}
	// Walking up yields nearest-first; the reader wants oldest-first.
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed
}

// parseThread decodes getPostThread into a Thread with its ancestor chain and a
// flattened reply tree. An anchor post with no URI means the AppView answered
// with notFoundPost or blockedPost — a 200 that carries no post — which the
// caller must not render as an empty success.
func parseThread(raw []byte) (Thread, error) {
	var tr threadResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return Thread{}, err
	}
	anchorParent := ""
	if tr.Thread.Parent != nil {
		anchorParent = tr.Thread.Parent.Post.URI
	}
	var replies []Post
	flattenReplies(tr.Thread.Replies, tr.Thread.Post.URI, &replies)
	return Thread{
		Ancestors: collectAncestors(&tr.Thread),
		Post:      toPost(tr.Thread.Post, anchorParent),
		Replies:   replies,
	}, nil
}

// parseFeed decodes getAuthorFeed into an account's recent posts. Reposts
// appear in the feed as the original post and are emitted as-is: what the
// account chose to amplify is as informative as what it wrote.
func parseFeed(raw []byte, actor string) (Feed, error) {
	var fr feedResponse
	if err := json.Unmarshal(raw, &fr); err != nil {
		return Feed{}, err
	}
	posts := make([]Post, 0, len(fr.Feed))
	for _, item := range fr.Feed {
		if item.Post.URI == "" {
			continue
		}
		posts = append(posts, toPost(item.Post, ""))
	}
	return Feed{Actor: actor, Posts: posts}, nil
}
