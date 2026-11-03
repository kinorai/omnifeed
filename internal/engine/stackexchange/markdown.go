package stackexchange

import (
	"html"
	"regexp"
	"strconv"
	"strings"

	"github.com/kinorai/omnifeed/internal/linkfmt"
)

// HTML-to-markdown patterns for question and answer bodies. Like the Hacker
// News engine's cleanHTML and the Discourse engine's stripHTML this is a small
// regex converter, not a general one: Stack Exchange bodies are rendered from
// the site's own markdown dialect, so they use a short, regular set of tags
// (p, pre/code, code, a, img, em/strong, lists, tables, headings, blockquote,
// hr, br).
// What it cares about most is code fidelity — the code blocks are the answer.
var (
	preRE        = regexp.MustCompile(`(?is)<pre\b([^>]*)>(.*?)</pre\s*>`)
	langRE       = regexp.MustCompile(`\blang-([A-Za-z0-9_+#-]+)`)
	inlineCodeRE = regexp.MustCompile(`(?is)<code\b[^>]*>(.*?)</code\s*>`)
	// anchorRE captures an anchor's double-quoted href (group 1) and its inner
	// HTML (group 2). The href attribute must follow whitespace so that
	// data-href and similar never pass for it.
	anchorRE     = regexp.MustCompile(`(?is)<a\s(?:[^>]*?\s)?href\s*=\s*"([^"]*)"[^>]*>(.*?)</a\s*>`)
	imgRE        = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	imgSrcRE     = regexp.MustCompile(`(?is)\ssrc\s*=\s*"([^"]*)"`)
	imgAltRE     = regexp.MustCompile(`(?is)\salt\s*=\s*"([^"]*)"`)
	headingRE    = regexp.MustCompile(`(?is)<h([1-6])\b[^>]*>(.*?)</h[1-6]\s*>`)
	blockquoteRE = regexp.MustCompile(`(?is)<blockquote\b[^>]*>(.*?)</blockquote\s*>`)
	olRE         = regexp.MustCompile(`(?is)<ol\b[^>]*>(.*?)</ol\s*>`)
	liOpenRE     = regexp.MustCompile(`(?i)\s*<li\b[^>]*>`)
	liCloseRE    = regexp.MustCompile(`(?i)</li\s*>\s*`)
	tableRE      = regexp.MustCompile(`(?is)<table\b[^>]*>(.*?)</table\s*>`)
	rowRE        = regexp.MustCompile(`(?is)<tr\b[^>]*>(.*?)</tr\s*>`)
	cellRE       = regexp.MustCompile(`(?is)<t[hd]\b[^>]*>(.*?)</t[hd]\s*>`)
	strongRE     = regexp.MustCompile(`(?i)</?(strong|b)\b[^>]*>`)
	emRE         = regexp.MustCompile(`(?i)</?(em|i)\b[^>]*>`)
	strikeRE     = regexp.MustCompile(`(?i)</?(del|s|strike)\b[^>]*>`)
	hrRE         = regexp.MustCompile(`(?i)<hr\b[^>]*>`)
	brRE         = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockCloseRE = regexp.MustCompile(`(?i)</(p|div|ul|ol)\s*>`)
	spaceRunRE   = regexp.MustCompile(`\s+`)
	anyTagRE     = regexp.MustCompile(`(?s)<[^>]*>`)
	trailingWSRE = regexp.MustCompile(`[ \t]+\n`)
	blankLinesRE = regexp.MustCompile(`\n{3,}`)
)

// htmlToMarkdown converts a body's HTML to markdown. host is the site the body
// came from: its relative and protocol-relative links ("/questions/1",
// "//stackoverflow.com/a/2") are made absolute, since they mean nothing
// outside the page.
//
// Code blocks are cut out first and put back last, so nothing below touches
// their content (indentation, "<" in code, blank lines). Everything else stays
// HTML-encoded until the single unescape at the end; the parts converted on
// the way (link targets) are re-escaped so they are decoded exactly once.
func htmlToMarkdown(s, host string) string {
	if s == "" {
		return ""
	}
	var blocks []string
	s = preRE.ReplaceAllStringFunc(s, func(m string) string {
		g := preRE.FindStringSubmatch(m)
		blocks = append(blocks, fence(g[1], g[2]))
		return "\n\n\x00" + strconv.Itoa(len(blocks)-1) + "\x00\n\n"
	})
	s = inlineCodeRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := inlineCodeRE.FindStringSubmatch(m)[1]
		tick := "`"
		if strings.Contains(inner, "`") {
			tick = "``"
		}
		return tick + anyTagRE.ReplaceAllString(inner, "") + tick
	})
	s = anchorRE.ReplaceAllStringFunc(s, func(m string) string { return rewriteAnchor(m, host) })
	s = imgRE.ReplaceAllStringFunc(s, func(m string) string { return rewriteImg(m, host) })
	s = headingRE.ReplaceAllStringFunc(s, func(m string) string {
		g := headingRE.FindStringSubmatch(m)
		level, _ := strconv.Atoi(g[1])
		return "\n\n" + strings.Repeat("#", level) + " " + strings.TrimSpace(g[2]) + "\n\n"
	})
	s = liCloseRE.ReplaceAllString(s, "")
	s = olRE.ReplaceAllStringFunc(s, func(m string) string {
		return liOpenRE.ReplaceAllString(m, "\n1. ")
	})
	s = liOpenRE.ReplaceAllString(s, "\n- ")
	s = strongRE.ReplaceAllString(s, "**")
	s = emRE.ReplaceAllString(s, "*")
	s = strikeRE.ReplaceAllString(s, "~~")
	s = hrRE.ReplaceAllString(s, "\n\n---\n\n")
	s = brRE.ReplaceAllString(s, "\n")
	s = tableRE.ReplaceAllStringFunc(s, table)
	s = blockquoteRE.ReplaceAllStringFunc(s, func(m string) string {
		inner := blockquoteRE.FindStringSubmatch(m)[1]
		inner = strings.TrimSpace(blankLinesRE.ReplaceAllString(blockCloseRE.ReplaceAllString(inner, "\n\n"), "\n\n"))
		return "\n\n> " + strings.ReplaceAll(inner, "\n", "\n> ") + "\n\n"
	})
	s = blockCloseRE.ReplaceAllString(s, "\n\n")
	s = anyTagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = trailingWSRE.ReplaceAllString(s, "\n")
	s = blankLinesRE.ReplaceAllString(s, "\n\n")
	for i, b := range blocks {
		s = strings.Replace(s, "\x00"+strconv.Itoa(i)+"\x00", b, 1)
	}
	return strings.TrimSpace(s)
}

// table renders one <table> as a markdown table: one line per row, the first
// row taken as the header. Cell content is flattened to one line, and a "|"
// inside it is escaped so it does not split the cell.
func table(m string) string {
	var lines []string
	for _, row := range rowRE.FindAllStringSubmatch(tableRE.FindStringSubmatch(m)[1], -1) {
		cells := cellRE.FindAllStringSubmatch(row[1], -1)
		if len(cells) == 0 {
			continue
		}
		out := make([]string, len(cells))
		for i, c := range cells {
			text := strings.TrimSpace(spaceRunRE.ReplaceAllString(anyTagRE.ReplaceAllString(c[1], " "), " "))
			out[i] = strings.ReplaceAll(text, "|", `\|`)
		}
		lines = append(lines, "| "+strings.Join(out, " | ")+" |")
		if len(lines) == 1 {
			lines = append(lines, "|"+strings.Repeat(" --- |", len(cells)))
		}
	}
	return "\n\n" + strings.Join(lines, "\n") + "\n\n"
}

// fence renders one <pre> block as a fenced code block. attrs are the <pre>
// tag's attributes: the site's syntax-highlighting hint (class="lang-cpp")
// becomes the fence's language, except lang-none. The fence grows past any
// backtick run in the code, so code that itself contains ``` stays intact.
func fence(attrs, inner string) string {
	code := html.UnescapeString(anyTagRE.ReplaceAllString(inner, ""))
	code = strings.TrimRight(code, "\n")
	lang := ""
	if m := langRE.FindStringSubmatch(attrs); m != nil && m[1] != "none" {
		lang = m[1]
	}
	ticks := "```"
	for strings.Contains(code, ticks) {
		ticks += "`"
	}
	return ticks + lang + "\n" + code + "\n" + ticks
}

// rewriteAnchor replaces one <a href="…">text</a> with its link target,
// rendered by linkfmt.Link (bare URL when the text is the URL or a shortened
// form of it, markdown [text](href) otherwise). Non-http(s) targets (mailto:,
// fragments) keep just their text. The result is re-escaped: htmlToMarkdown
// unescapes the whole string afterwards.
func rewriteAnchor(a, host string) string {
	m := anchorRE.FindStringSubmatch(a)
	href, ok := absolute(strings.TrimSpace(html.UnescapeString(m[1])), host)
	if !ok {
		return m[2]
	}
	text := html.UnescapeString(anyTagRE.ReplaceAllString(m[2], ""))
	return html.EscapeString(linkfmt.Link(text, href))
}

// rewriteImg renders an <img> as markdown ![alt](src); an image without a
// usable src is dropped.
func rewriteImg(tag, host string) string {
	m := imgSrcRE.FindStringSubmatch(tag)
	if m == nil {
		return ""
	}
	src, ok := absolute(strings.TrimSpace(html.UnescapeString(m[1])), host)
	if !ok {
		return ""
	}
	alt := ""
	if a := imgAltRE.FindStringSubmatch(tag); a != nil {
		alt = html.UnescapeString(a[1])
	}
	return html.EscapeString("![" + alt + "](" + src + ")")
}

// absolute resolves a link target against the site: protocol-relative
// "//host/x" and root-relative "/x" become https URLs; absolute http(s) URLs
// pass through. ok is false for anything else.
func absolute(href, host string) (string, bool) {
	lower := strings.ToLower(href)
	switch {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"):
		return href, true
	case strings.HasPrefix(href, "//"):
		return "https:" + href, true
	case strings.HasPrefix(href, "/"):
		return "https://" + host + href, true
	}
	return "", false
}
