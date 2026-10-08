package twitter

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// articleMarkdown flattens an X Article — a Draft.js document — to markdown:
// headings, lists, quotes and code blocks by block type; links from LINK
// entity ranges; atomic blocks as image / embedded-post / markdown
// placeholders. Inline bold/italic is dropped: it costs tokens and carries
// little for a reader that is an LLM.
func articleMarkdown(a *fxArticle) string {
	entities := make(map[string]articleEntity, len(a.Content.EntityMap))
	for _, en := range a.Content.EntityMap {
		entities[en.Key] = en
	}
	mediaByID := make(map[string]articleMedia, len(a.MediaEntities))
	for _, m := range a.MediaEntities {
		mediaByID[m.MediaID] = m
	}

	var out strings.Builder
	prevList := false
	for _, blk := range a.Content.Blocks {
		line, isList := renderBlock(blk, entities, mediaByID)
		if line == "" {
			continue
		}
		if out.Len() > 0 {
			if isList && prevList {
				out.WriteString("\n")
			} else {
				out.WriteString("\n\n")
			}
		}
		out.WriteString(line)
		prevList = isList
	}
	return out.String()
}

// renderBlock renders one Draft.js block; isList reports a list item, which
// joins its neighbours with a single newline.
func renderBlock(blk articleBlock, entities map[string]articleEntity, mediaByID map[string]articleMedia) (line string, isList bool) {
	if blk.Type == "atomic" {
		return renderAtomic(blk, entities, mediaByID), false
	}
	text := strings.TrimSpace(applyLinks(blk, entities))
	if text == "" {
		return "", false
	}
	switch blk.Type {
	case "header-one":
		return "## " + text, false
	case "header-two":
		return "### " + text, false
	case "header-three", "header-four", "header-five", "header-six":
		return "#### " + text, false
	case "unordered-list-item":
		return "- " + text, true
	case "ordered-list-item":
		return "1. " + text, true
	case "blockquote":
		return "> " + strings.ReplaceAll(text, "\n", "\n> "), false
	case "code-block":
		return "```\n" + blk.Text + "\n```", false
	default:
		return text, false
	}
}

// renderAtomic renders a block that stands for an embedded object.
func renderAtomic(blk articleBlock, entities map[string]articleEntity, mediaByID map[string]articleMedia) string {
	if len(blk.EntityRanges) == 0 {
		return ""
	}
	en, ok := entities[strconv.Itoa(blk.EntityRanges[0].Key)]
	if !ok {
		return ""
	}
	d := en.Value.Data
	switch en.Value.Type {
	case "MEDIA":
		var parts []string
		for _, it := range d.MediaItems {
			m := mediaByID[it.MediaID]
			kind := "image"
			switch m.MediaInfo.Typename {
			case "ApiVideo":
				kind = "video"
			case "ApiGif":
				kind = "gif"
			}
			parts = append(parts, mediaMarker(media{Kind: kind, Alt: m.MediaInfo.AltText}))
		}
		return strings.Join(parts, " ")
	case "TWEET":
		if d.TweetID == "" {
			return ""
		}
		return "[embedded post: " + canonicalURL(d.TweetID) + "]"
	case "MARKDOWN":
		return strings.TrimSpace(d.Markdown)
	default:
		return ""
	}
}

// applyLinks turns the block's LINK entity ranges into markdown links.
// Draft.js offsets count UTF-16 code units, so the text is sliced in UTF-16.
func applyLinks(blk articleBlock, entities map[string]articleEntity) string {
	type link struct {
		off, end int
		url      string
	}
	units := utf16.Encode([]rune(blk.Text))
	var links []link
	for _, r := range blk.EntityRanges {
		en, ok := entities[strconv.Itoa(r.Key)]
		if !ok || en.Value.Type != "LINK" || en.Value.Data.URL == "" {
			continue
		}
		if r.Offset < 0 || r.Length <= 0 || r.Offset+r.Length > len(units) {
			continue
		}
		links = append(links, link{r.Offset, r.Offset + r.Length, en.Value.Data.URL})
	}
	if len(links) == 0 {
		return blk.Text
	}
	sort.Slice(links, func(i, j int) bool { return links[i].off < links[j].off })

	var out strings.Builder
	pos := 0
	for _, l := range links {
		if l.off < pos { // overlapping range: keep the first
			continue
		}
		out.WriteString(string(utf16.Decode(units[pos:l.off])))
		anchor := string(utf16.Decode(units[l.off:l.end]))
		if strings.TrimSpace(anchor) == l.url {
			out.WriteString(l.url)
		} else {
			out.WriteString("[" + anchor + "](" + l.url + ")")
		}
		pos = l.end
	}
	out.WriteString(string(utf16.Decode(units[pos:])))
	return out.String()
}
