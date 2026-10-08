// Package linkfmt renders a hyperlink — visible text plus its real target — as
// plain text an LLM can follow. Engines whose upstream shortens long URLs in
// the visible text (Discourse cooked HTML, Bluesky link facets) use it so the
// output keeps the full target instead of a dead, truncated link.
package linkfmt

import "strings"

// Link renders a link whose visible text is text and whose target is href.
// When the text is just the URL itself — equal to the href, a prefix of it, or
// shortened with a trailing "..."/"…" — the bare href is returned; otherwise
// markdown [text](href) keeps both. Prefix matching also ignores the href's
// http(s):// scheme and a leading "www.", since clients commonly display links
// that way (Bluesky shows "example.com/path…" for https://example.com/path/x).
//
// Both arguments must already be decoded (no HTML entities). An empty href
// returns text unchanged; an empty text returns the bare href.
func Link(text, href string) string {
	if href == "" {
		return text
	}
	text = strings.TrimSpace(text)
	if text == "" || isURLText(text, href) {
		return href
	}
	return "[" + text + "](" + href + ")"
}

// isURLText reports whether text is a (possibly shortened) display of href.
func isURLText(text, href string) bool {
	if strings.HasSuffix(text, "...") || strings.HasSuffix(text, "…") {
		return true
	}
	bare := href
	for _, p := range []string{"https://", "http://"} {
		if strings.HasPrefix(strings.ToLower(bare), p) {
			bare = bare[len(p):]
			break
		}
	}
	return strings.HasPrefix(href, text) ||
		strings.HasPrefix(bare, text) ||
		strings.HasPrefix(strings.TrimPrefix(bare, "www."), text)
}
