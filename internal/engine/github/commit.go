package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/kinorai/omnifeed/internal/domain"
)

// crawlCommit renders a commit: message, author, date, stats, the changed-file
// list, and the diff up to the shared patch budget (one request).
func (e *Engine) crawlCommit(ctx context.Context, rawURL string, t target) (domain.Document, error) {
	var c apiCommit
	if err := e.getJSON(ctx, e.repoURL(t, "commits/"+t.sha), "commit", &c); err != nil {
		return domain.Document{}, err
	}
	msg := strings.TrimSpace(strings.ReplaceAll(c.Commit.Message, "\r\n", "\n"))
	subject, body, _ := strings.Cut(msg, "\n")
	short := c.SHA
	if len(short) > 7 {
		short = short[:7]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# %s/%s@%s: %s\n\n", t.owner, t.repo, short, oneLine(subject))
	author := c.Commit.Author.Name
	if c.Author != nil && c.Author.Login != "" && c.Author.Login != author {
		author += " (" + c.Author.Login + ")"
	}
	fmt.Fprintf(&b, "- Author: %s, %s\n", author, day(c.Commit.Author.Date))
	if cm := c.Commit.Committer; cm.Name != "" && cm.Name != c.Commit.Author.Name && cm.Name != "GitHub" {
		fmt.Fprintf(&b, "- Committer: %s, %s\n", cm.Name, day(cm.Date))
	}
	fmt.Fprintf(&b, "- SHA: %s\n", c.SHA)
	fmt.Fprintf(&b, "- Changes: +%d -%d in %s\n", c.Stats.Additions, c.Stats.Deletions, plural(len(c.Files), "file", "files"))
	if body = strings.TrimSpace(body); body != "" {
		b.WriteString("\n## Message\n\n" + body + "\n")
	}

	files, diffTruncated := budgetFiles(c.Files)
	fmt.Fprintf(&b, "\n## Files (%d)\n\n", len(files))
	for _, f := range files {
		fmt.Fprintf(&b, "- %s (%s, +%d -%d)\n", f.Name, f.Status, f.Additions, f.Deletions)
	}
	var diff strings.Builder
	for _, f := range files {
		if f.Patch != "" {
			fmt.Fprintf(&diff, "--- %s\n%s\n", f.Name, strings.TrimRight(f.Patch, "\n"))
		}
	}
	if diff.Len() > 0 {
		b.WriteString("\n## Diff\n\n" + fenced(diff.String(), "diff") + "\n")
	}
	meta := map[string]string{"github_kind": kindNames[t.kind], "files": strconv.Itoa(len(files))}
	if diffTruncated {
		meta["diff_truncated"] = "true"
		b.WriteString("\n[Some file patches omitted (diff budget spent); names and stats are complete.]\n")
	}
	return e.markdownDocument(b.String(), rawURL, meta), nil
}
