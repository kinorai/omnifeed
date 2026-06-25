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
	for i, h := range s.Hits {
		id, _ := strconv.Atoi(h.ObjectID)
		stories = append(stories, Story{
			Rank:        i + 1,
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
