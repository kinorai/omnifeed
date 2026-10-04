package github

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kinorai/omnifeed/internal/domain"
)

// crawlGist renders a gist: description, owner, dates, then every file (one
// request; the API inlines file content up to 1MB per file).
func (e *Engine) crawlGist(ctx context.Context, rawURL string, t target) (domain.Document, error) {
	var g apiGist
	if err := e.getJSON(ctx, e.apiBase+"/gists/"+t.gistID, "gist", &g); err != nil {
		return domain.Document{}, err
	}
	names := make([]string, 0, len(g.Files))
	for name := range g.Files {
		names = append(names, name)
	}
	sort.Strings(names) // GitHub lists gist files alphabetically

	title := oneLine(g.Description)
	if title == "" && len(names) > 0 {
		title = names[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Gist: %s\n\n", title)
	if g.Owner != nil && g.Owner.Login != "" {
		fmt.Fprintf(&b, "- Owner: %s\n", g.Owner.Login)
	}
	fmt.Fprintf(&b, "- Created: %s, updated: %s\n", day(g.CreatedAt), day(g.UpdatedAt))
	fmt.Fprintf(&b, "- Files: %d\n", len(names))
	for _, name := range names {
		f := g.Files[name]
		fmt.Fprintf(&b, "\n## %s\n\n", name)
		switch {
		case isMarkdownFile(name), isProseFile(name):
			b.WriteString(strings.TrimSpace(strings.ReplaceAll(f.Content, "\r\n", "\n")) + "\n")
		default:
			lang := codeLang(name)
			if lang == "" {
				lang = strings.ToLower(f.Language)
			}
			b.WriteString(fenced(f.Content, lang) + "\n")
		}
		if f.Truncated {
			fmt.Fprintf(&b, "\n[File cut by GitHub at %s of %s; raw file: %s]\n",
				humanBytes(int64(len(f.Content))), humanBytes(f.Size), f.RawURL)
		}
	}
	return e.markdownDocument(b.String(), rawURL, map[string]string{
		"github_kind": kindNames[t.kind], "files": strconv.Itoa(len(names)),
	}), nil
}
