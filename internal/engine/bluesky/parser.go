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

// parseThread decodes getPostThread into a Thread with a flattened reply tree.
func parseThread(raw []byte) (Thread, error) {
	var tr threadResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return Thread{}, err
	}
	var replies []Post
	flattenReplies(tr.Thread.Replies, tr.Thread.Post.URI, &replies)
	return Thread{Post: toPost(tr.Thread.Post, ""), Replies: replies}, nil
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
