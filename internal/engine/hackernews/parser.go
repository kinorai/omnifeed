package hackernews

import (
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"
)

var (
	hnTagRE      = regexp.MustCompile(`<[^>]+>`)
	hnBlankRunRE = regexp.MustCompile(`\n{3,}`)
)

// cleanHTML turns HN's HTML comment text into readable plain text: paragraph
// tags become blank lines, remaining tags are stripped, HTML entities are
// unescaped, and runs of blank lines are clamped. HN renders link anchors with
// the visible URL as their text, so stripping the <a> tag keeps the URL.
func cleanHTML(s string) string {
	if s == "" {
		return s
	}
	// Fence code blocks BEFORE stripping tags so HN's <pre><code> survives as a
	// Markdown fence — the in-comment code fidelity this engine exists to provide.
	// (Tag-strip then unescape order means entity-encoded code chars like &lt;
	// survive the strip and are decoded inside the fence.)
	s = strings.ReplaceAll(s, "<pre><code>", "\n```\n")
	s = strings.ReplaceAll(s, "</code></pre>", "\n```\n")
	s = strings.ReplaceAll(s, "<pre>", "\n```\n")
	s = strings.ReplaceAll(s, "</pre>", "\n```\n")
	s = strings.ReplaceAll(s, "<p>", "\n\n")
	s = strings.ReplaceAll(s, "</p>", "")
	s = hnTagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = hnBlankRunRE.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// flattenComments walks the Algolia children tree in pre-order and appends each
// live comment with its parent_id — the same flat+parent_id shape the Reddit
// engine emits. Deleted nodes (empty author and text) are skipped, but recursion
// still descends through them so live replies under a dead parent survive (the
// dead node's id stays the parent_id, preserving structure).
func flattenComments(children []algoliaItem, parentID int, out *[]Comment) {
	for i := range children {
		c := &children[i]
		if c.Author != "" || c.Text != "" {
			*out = append(*out, Comment{
				ID:       c.ID,
				ParentID: parentID,
				Author:   c.Author,
				Body:     cleanHTML(c.Text),
				Created:  c.CreatedI,
			})
		}
		if len(c.Children) > 0 {
			flattenComments(c.Children, c.ID, out)
		}
	}
}

// parseThread decodes GET /items/{id} into a Thread with a flattened comment tree.
func parseThread(raw []byte) (Thread, error) {
	var it algoliaItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return Thread{}, err
	}
	var comments []Comment

	// A comment permalink (/item?id=<commentID>) resolves to a type:"comment" item
	// with a null title — don't render it as a blank-titled story. Emit the comment
	// itself (then its replies), with a header that points at the enclosing story.
	if it.Type == "comment" {
		comments = append(comments, Comment{
			ID:       it.ID,
			ParentID: it.ParentID,
			Author:   it.Author,
			Body:     cleanHTML(it.Text),
			Created:  it.CreatedI,
		})
		flattenComments(it.Children, it.ID, &comments)
		return Thread{Story: Item{ID: it.StoryID}, Comments: comments}, nil
	}

	flattenComments(it.Children, it.ID, &comments)
	return Thread{
		Story: Item{
			ID:      it.ID,
			Title:   it.Title,
			Author:  it.Author,
			URL:     it.URL,
			Points:  it.Points,
			Created: it.CreatedI,
			Text:    cleanHTML(it.Text),
		},
		Comments: comments,
	}, nil
}

// parseFrontPage decodes GET /search hits into a ranked story list.
func parseFrontPage(raw []byte, feed string) (FrontPage, error) {
	var s algoliaSearch
	if err := json.Unmarshal(raw, &s); err != nil {
		return FrontPage{}, err
	}
	stories := make([]Story, 0, len(s.Hits))
	for _, h := range s.Hits {
		id, _ := strconv.Atoi(h.ObjectID)
		stories = append(stories, Story{
			ID:          id,
			Title:       h.Title,
			URL:         h.URL,
			Author:      h.Author,
			Points:      h.Points,
			NumComments: h.NumComments,
			Created:     h.CreatedI,
		})
	}
	return FrontPage{Feed: feed, Stories: stories}, nil
}

// subtreeIndex records, for every emitted comment, the top-level comment its
// branch hangs under (root) and its depth below that root (the root is 0).
type subtreeIndex struct {
	root  map[int]int
	depth map[int]int
}

// indexSubtrees builds the subtreeIndex for a flattened comment list. A comment
// whose parent is not itself an emitted comment is its own root: that covers the
// story's direct replies (their parent is the story) and live replies under a
// skipped deleted parent, so no comment is ever left without a root.
// flattenComments emits pre-order, so a single forward pass always sees a
// parent before its children.
func indexSubtrees(comments []Comment) subtreeIndex {
	idx := subtreeIndex{
		root:  make(map[int]int, len(comments)),
		depth: make(map[int]int, len(comments)),
	}
	for _, c := range comments {
		if r, ok := idx.root[c.ParentID]; ok {
			idx.root[c.ID] = r
			idx.depth[c.ID] = idx.depth[c.ParentID] + 1
			continue
		}
		idx.root[c.ID] = c.ID
		idx.depth[c.ID] = 0
	}
	return idx
}

// keepOnly drops every comment whose id is not in kept, preserving the original
// document order of the survivors.
func keepOnly(t *Thread, kept map[int]bool) {
	filtered := t.Comments[:0]
	for _, c := range t.Comments {
		if kept[c.ID] {
			filtered = append(filtered, c)
		}
	}
	t.Comments = filtered
}

// capTopLevel keeps only the first n top-level threads (in the order HN returns
// them) and every reply beneath them, dropping the rest (0 = unlimited).
func capTopLevel(t *Thread, n int) {
	if n <= 0 {
		return
	}
	idx := indexSubtrees(t.Comments)
	keptRoots := make(map[int]bool, n)
	roots := 0
	for _, c := range t.Comments {
		if idx.root[c.ID] != c.ID {
			continue
		}
		if roots < n {
			keptRoots[c.ID] = true
		}
		roots++
	}
	if roots <= n {
		return // fewer top-level threads than the cap — nothing to drop
	}
	kept := make(map[int]bool, len(t.Comments))
	for _, c := range t.Comments {
		if keptRoots[idx.root[c.ID]] {
			kept[c.ID] = true
		}
	}
	keepOnly(t, kept)
}

// capPerSubtree keeps at most n comments inside each top-level thread, counting
// the thread's root comment, and drops the rest (0 = unlimited). This is the cap
// that fits HN's mega-thread shape: subtree sizes are wildly skewed (one measured
// thread ran 105, 55, 16, 13, … over 62 top-level threads), so a flat total cap
// spends nearly the whole budget on the first branch, while a per-subtree cap
// buys breadth across the discussion.
//
// Selection inside a subtree is breadth-first — the root and its shallow replies
// before the deep tail — because depth-first would again pour the budget into one
// branch. Depth carries no signal on HN (the most-cited comment in both measured
// threads sat at depth 7), so this engine caps breadth and never depth.
// Breadth-first also means a kept reply always has its ancestors kept, so the cut
// cannot orphan a comment from its parent_id. Output order is untouched: the
// survivors stay in the order HN returned them.
func capPerSubtree(t *Thread, n int) {
	if n <= 0 {
		return
	}
	idx := indexSubtrees(t.Comments)
	maxDepth := 0
	for _, d := range idx.depth {
		if d > maxDepth {
			maxDepth = d
		}
	}
	kept := make(map[int]bool, len(t.Comments))
	perRoot := make(map[int]int, len(t.Comments))
	for d := 0; d <= maxDepth; d++ {
		for _, c := range t.Comments {
			if idx.depth[c.ID] != d {
				continue
			}
			r := idx.root[c.ID]
			if perRoot[r] >= n {
				continue
			}
			perRoot[r]++
			kept[c.ID] = true
		}
	}
	keepOnly(t, kept)
}

// capComments truncates the flat comment list to at most n entries, preserving
// order. The list is pre-order, so ancestors precede their descendants and a
// prefix cut never orphans a kept reply from a dropped parent.
func capComments(t *Thread, n int) {
	if n > 0 && len(t.Comments) > n {
		t.Comments = t.Comments[:n]
	}
}
