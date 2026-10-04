package github

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// webBase and rawBase are where rewritten repository links point: pages
// (blob/tree) on github.com, images on raw.githubusercontent.com.
const (
	webBase = "https://github.com"
	rawBase = "https://raw.githubusercontent.com"
)

// repoLinks resolves repository-relative links in a markdown file to absolute
// URLs, as GitHub itself does when it renders the file.
type repoLinks struct {
	owner, repo, ref string
	dir              string // directory of the markdown file, "" at the repo root
}

var (
	htmlCommentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlImgRE     = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	htmlAnchorRE  = regexp.MustCompile(`(?is)<a\b[^>]*?\bhref\s*=\s*"([^"]*)"[^>]*>(.*?)</a>`)
	htmlHeadingRE = regexp.MustCompile(`(?is)<h([1-6])\b[^>]*>(.*?)</h[1-6]>`)
	htmlBreakRE   = regexp.MustCompile(`(?i)<br\s*/?>`)
	// Layout-only wrappers GitHub READMEs use for centering and grouping. Block
	// tags become line breaks, inline ones vanish; tables and lists are left
	// alone (stripping them would scramble their text).
	htmlBlockTagRE  = regexp.MustCompile(`(?i)</?(p|div|center|picture|details|summary)\b[^>]*>`)
	htmlInlineTagRE = regexp.MustCompile(`(?i)</?(span|source|sup|sub|b|strong|em|i|kbd|font)\b[^>]*>`)
	// A line holding nothing but images, each optionally wrapped in a link: the
	// badge rows (CI, coverage, version, chat) atop most READMEs.
	// A linked image whose source is a status badge (CI, report card, license
	// scan), anywhere in a line.
	linkedBadgeRE   = regexp.MustCompile(`(?i)\[!\[[^\]]*\]\([^)]*(badge|shield|badgen)[^)]*\)\]\([^)]*\)`)
	imageOnlyLineRE = regexp.MustCompile(`^\s*((\[\s*)?!\[[^\]]*\]\([^)]*\)(\s*\]\([^)]*\))?\s*)+$`)
	// Link destinations: inline "](dest" and reference definitions "[x]: dest".
	inlineLinkRE = regexp.MustCompile(`\]\(\s*(<[^>]*>|[^()\s]+)`)
	refDefRE     = regexp.MustCompile(`(?m)^( {0,3}\[[^\]]+\]:\s*)(<[^>]*>|\S+)`)
	htmlAttrRE   = regexp.MustCompile(`(?i)\b(href|src)\s*=\s*"([^"]*)"`)
	schemeRE     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	fenceRE      = regexp.MustCompile("^ {0,3}(```+|~~~+)")
	headingRE    = regexp.MustCompile(`^(#{1,6})(\s)`)
	blankRunRE   = regexp.MustCompile(`\n{3,}`)
	spaceRunRE   = regexp.MustCompile(`\s+`)
	imageExtRE   = regexp.MustCompile(`(?i)\.(png|jpe?g|gif|svg|webp|avif|bmp|ico)$`)
)

// cleanMarkdown turns a repository markdown file (README and friends) into
// compact markdown for an LLM: HTML comments, <img> tags, and badge rows go,
// layout HTML is reduced to its text, <a> becomes a markdown link, and
// relative links become absolute. Fenced code blocks are left untouched.
func cleanMarkdown(md string, links repoLinks) string {
	md = strings.ReplaceAll(md, "\r\n", "\n")
	out := mapProse(md, func(s string) string {
		s = htmlCommentRE.ReplaceAllString(s, "")
		s = htmlImgRE.ReplaceAllString(s, "")
		s = htmlAnchorRE.ReplaceAllStringFunc(s, func(m string) string {
			sub := htmlAnchorRE.FindStringSubmatch(m)
			text := strings.TrimSpace(spaceRunRE.ReplaceAllString(sub[2], " "))
			if text == "" {
				return ""
			}
			return "[" + text + "](" + sub[1] + ")"
		})
		s = htmlHeadingRE.ReplaceAllStringFunc(s, func(m string) string {
			sub := htmlHeadingRE.FindStringSubmatch(m)
			text := strings.TrimSpace(spaceRunRE.ReplaceAllString(sub[2], " "))
			if text == "" {
				return ""
			}
			level, _ := strconv.Atoi(sub[1])
			return "\n" + strings.Repeat("#", level) + " " + text + "\n"
		})
		s = htmlBreakRE.ReplaceAllString(s, "\n")
		s = htmlBlockTagRE.ReplaceAllString(s, "\n")
		s = htmlInlineTagRE.ReplaceAllString(s, "")

		s = linkedBadgeRE.ReplaceAllString(s, "")
		lines := strings.Split(s, "\n")
		kept := lines[:0]
		for _, l := range lines {
			if imageOnlyLineRE.MatchString(l) && isBadgeRow(l) {
				continue
			}
			kept = append(kept, strings.TrimRight(l, " \t"))
		}
		s = strings.Join(kept, "\n")
		return links.absolutize(s)
	})
	return strings.TrimSpace(blankRunRE.ReplaceAllString(out, "\n\n"))
}

// isBadgeRow reports whether an image-only line is a badge row rather than a
// screenshot: every image in it is linked, or served by a badge service.
func isBadgeRow(line string) bool {
	if strings.Contains(line, "[![") {
		return true
	}
	l := strings.ToLower(line)
	return strings.Contains(l, "shields.io") || strings.Contains(l, "badge")
}

// absolutize rewrites relative link destinations in prose to absolute URLs.
func (rl repoLinks) absolutize(s string) string {
	s = inlineLinkRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := inlineLinkRE.FindStringSubmatch(m)
		return strings.Replace(m, sub[1], rl.resolve(sub[1]), 1)
	})
	s = refDefRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := refDefRE.FindStringSubmatch(m)
		return sub[1] + rl.resolve(sub[2])
	})
	return htmlAttrRE.ReplaceAllStringFunc(s, func(m string) string {
		sub := htmlAttrRE.FindStringSubmatch(m)
		return sub[1] + `="` + rl.resolve(sub[2]) + `"`
	})
}

// resolve maps one link destination to an absolute URL. Absolute URLs,
// protocol-relative URLs, and in-page anchors are returned unchanged. Images
// resolve to raw.githubusercontent.com, everything else to the blob view
// (GitHub redirects it to the tree view for a directory).
func (rl repoLinks) resolve(dest string) string {
	if rl.owner == "" {
		return dest
	}
	d := strings.TrimSuffix(strings.TrimPrefix(dest, "<"), ">")
	if d == "" || strings.HasPrefix(d, "#") || strings.HasPrefix(d, "//") || schemeRE.MatchString(d) {
		return dest
	}
	suffix := ""
	if i := strings.IndexAny(d, "?#"); i >= 0 {
		d, suffix = d[:i], d[i:]
	}
	var p string
	if strings.HasPrefix(d, "/") {
		p = path.Clean(d)
	} else {
		p = path.Clean("/" + path.Join(rl.dir, d))
	}
	p = strings.TrimPrefix(p, "/")
	if imageExtRE.MatchString(p) {
		return rawBase + "/" + rl.owner + "/" + rl.repo + "/" + rl.ref + "/" + p + suffix
	}
	return webBase + "/" + rl.owner + "/" + rl.repo + "/blob/" + rl.ref + "/" + p + suffix
}

// mapProse applies fn to every stretch of md outside fenced code blocks and
// returns the reassembled document; fence lines and code are kept verbatim.
func mapProse(md string, fn func(string) string) string {
	var out, prose strings.Builder
	flush := func() {
		if prose.Len() > 0 {
			out.WriteString(fn(prose.String()))
			prose.Reset()
		}
	}
	fence := "" // the open fence marker, "" outside code
	for _, line := range strings.SplitAfter(md, "\n") {
		m := fenceRE.FindStringSubmatch(line)
		switch {
		case fence == "" && m != nil:
			flush()
			fence = m[1]
			out.WriteString(line)
		case fence != "":
			out.WriteString(line)
			if m != nil && m[1][0] == fence[0] && len(m[1]) >= len(fence) &&
				strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), m[1][:1])) == "" {
				fence = ""
			}
		default:
			prose.WriteString(line)
		}
	}
	flush()
	return out.String()
}

// demoteHeadings pushes every markdown heading down by levels (capped at h6),
// so an embedded document (release notes) nests under the heading that
// introduces it instead of competing with the page title.
func demoteHeadings(md string, levels int) string {
	return mapProse(md, func(s string) string {
		lines := strings.Split(s, "\n")
		for i, l := range lines {
			if m := headingRE.FindStringSubmatch(l); m != nil {
				n := min(len(m[1])+levels, 6)
				lines[i] = strings.Repeat("#", n) + l[len(m[1]):]
			}
		}
		return strings.Join(lines, "\n")
	})
}

// excerpt cuts md to about maxChars at a line boundary, closing a code fence
// left open by the cut. cut reports whether anything was dropped.
func excerpt(md string, maxChars int) (out string, cut bool) {
	md = strings.TrimSpace(strings.ReplaceAll(md, "\r\n", "\n"))
	if len(md) <= maxChars {
		return md, false
	}
	cutAt := maxChars
	for cutAt > 0 && !utf8.RuneStart(md[cutAt]) {
		cutAt-- // never split a multi-byte character
	}
	head := md[:cutAt]
	if i := strings.LastIndexByte(head, '\n'); i > maxChars/2 {
		head = head[:i]
	}
	head = strings.TrimSpace(head)
	open := ""
	for _, l := range strings.Split(head, "\n") {
		if m := fenceRE.FindStringSubmatch(l); m != nil {
			if open == "" {
				open = m[1]
			} else if m[1][0] == open[0] {
				open = ""
			}
		}
	}
	if open != "" {
		head += "\n" + open
	}
	return head, true
}

// fenced wraps content in a code fence tagged with lang, using a fence longer
// than any backtick run inside the content so it can't be closed early.
func fenced(content, lang string) string {
	longest, run := 0, 0
	for _, r := range content {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + lang + "\n" + strings.TrimRight(content, "\n") + "\n" + fence
}

// langByExt maps file extensions to code-fence languages where the extension
// itself isn't the conventional tag.
var langByExt = map[string]string{
	"py": "python", "rb": "ruby", "rs": "rust", "kt": "kotlin", "kts": "kotlin", "ts": "typescript",
	"mts": "typescript", "cts": "typescript", "js": "javascript", "mjs": "javascript", "cjs": "javascript",
	"sh": "bash", "bash": "bash", "zsh": "bash", "yml": "yaml", "tf": "hcl", "tfvars": "hcl",
	"h": "c", "hpp": "cpp", "cc": "cpp", "cxx": "cpp", "cs": "csharp", "md": "markdown",
	"ps1": "powershell", "pl": "perl", "ex": "elixir", "exs": "elixir", "hs": "haskell", "jl": "julia",
	"m": "objectivec", "fs": "fsharp", "ml": "ocaml", "vue": "vue", "svelte": "svelte",
}

// langByName maps extensionless file names to code-fence languages.
var langByName = map[string]string{
	"dockerfile": "dockerfile", "containerfile": "dockerfile", "makefile": "makefile",
	"gnumakefile": "makefile", "jenkinsfile": "groovy", "vagrantfile": "ruby", "gemfile": "ruby",
	"rakefile": "ruby", "justfile": "just", "go.mod": "go", "go.sum": "text",
}

// codeLang returns the code-fence language for a file name ("" if unknown).
func codeLang(name string) string {
	base := strings.ToLower(path.Base(name))
	if l, ok := langByName[base]; ok {
		return l
	}
	if strings.HasPrefix(base, "dockerfile.") || strings.HasSuffix(base, ".dockerfile") {
		return "dockerfile"
	}
	ext := strings.TrimPrefix(path.Ext(base), ".")
	if l, ok := langByExt[ext]; ok {
		return l
	}
	return ext
}

// isMarkdownFile reports whether a file renders as markdown on GitHub.
func isMarkdownFile(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".markdown", ".mdown", ".mkd", ".mdx":
		return true
	}
	return false
}

// isProseFile reports whether a non-markdown file is plain prose best shown as
// is rather than fenced (LICENSE, NOTICE, .txt).
func isProseFile(name string) bool {
	ext := strings.ToLower(path.Ext(name))
	return ext == ".txt" || (ext == "" && codeLang(name) == "")
}

// day trims an ISO-8601 timestamp to its date.
func day(ts string) string {
	if len(ts) >= 10 {
		return ts[:10]
	}
	return ts
}

// oneLine collapses whitespace so a value fits on a single markdown line.
func oneLine(s string) string {
	return strings.TrimSpace(spaceRunRE.ReplaceAllString(s, " "))
}

// plural formats n with a singular or plural noun.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
