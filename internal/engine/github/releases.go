package github

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/kinorai/omnifeed/internal/domain"
)

const (
	releasesListed    = 10   // releases rendered for /releases
	listNotesExcerpt  = 3000 // chars of notes per release in the list
	maxReleaseAssets  = 50   // assets listed for one release
	releaseHeadingsBy = 2    // notes headings are demoted under the "## tag" heading
)

// crawlReleases renders the latest releases with their notes (one request).
func (e *Engine) crawlReleases(ctx context.Context, rawURL string, t target) (domain.Document, error) {
	var rels []apiRelease
	if err := e.getJSON(ctx, e.repoURL(t, "releases")+"?per_page="+strconv.Itoa(releasesListed), "releases", &rels); err != nil {
		return domain.Document{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s/%s releases\n\n", t.owner, t.repo)
	if len(rels) == 0 {
		b.WriteString("No releases published (tags without a release are not listed).\n")
	} else {
		fmt.Fprintf(&b, "The %s, newest first.\n", plural(len(rels), "latest release", "latest releases"))
	}
	for i := range rels {
		r := &rels[i]
		b.WriteString("\n" + releaseHeading("##", "", r))
		b.WriteString(releaseFlags(r))
		if notes, cut := excerpt(r.Body, listNotesExcerpt); notes != "" {
			b.WriteString("\n" + demoteHeadings(notes, releaseHeadingsBy) + "\n")
			if cut {
				fmt.Fprintf(&b, "\n[Notes cut; full notes: %s]\n", r.HTMLURL)
			}
		}
	}
	return e.markdownDocument(b.String(), rawURL, map[string]string{
		"github_kind": kindNames[t.kind], "releases": strconv.Itoa(len(rels)),
	}), nil
}

// crawlRelease renders one release (by tag, or the latest) with its full
// notes and asset list.
func (e *Engine) crawlRelease(ctx context.Context, rawURL string, t target) (domain.Document, error) {
	endpoint := "releases/latest"
	if t.kind == kindRelease {
		endpoint = "releases/tags/" + url.PathEscape(t.tag)
	}
	var r apiRelease
	if err := e.getJSON(ctx, e.repoURL(t, endpoint), "release", &r); err != nil {
		return domain.Document{}, err
	}
	var b strings.Builder
	b.WriteString(releaseHeading("#", t.owner+"/"+t.repo, &r))
	b.WriteString(releaseFlags(&r))
	if notes := strings.TrimSpace(strings.ReplaceAll(r.Body, "\r\n", "\n")); notes != "" {
		b.WriteString("\n" + demoteHeadings(notes, 1) + "\n")
	}
	if len(r.Assets) > 0 {
		fmt.Fprintf(&b, "\n## Assets (%d)\n\n", len(r.Assets))
		for i, a := range r.Assets {
			if i == maxReleaseAssets {
				fmt.Fprintf(&b, "- … %d more\n", len(r.Assets)-maxReleaseAssets)
				break
			}
			fmt.Fprintf(&b, "- %s (%s, %s)\n", a.Name, humanBytes(a.Size), plural(a.DownloadCount, "download", "downloads"))
		}
	}
	return e.markdownDocument(b.String(), rawURL, map[string]string{"github_kind": kindNames[t.kind]}), nil
}

// releaseFlags renders the author/prerelease/draft line of a release.
func releaseFlags(r *apiRelease) string {
	var parts []string
	if r.Author.Login != "" {
		parts = append(parts, "by "+r.Author.Login)
	}
	if r.Prerelease {
		parts = append(parts, "pre-release")
	}
	if r.Draft {
		parts = append(parts, "draft")
	}
	if len(parts) == 0 {
		return ""
	}
	return "\n" + strings.Join(parts, ", ") + "\n"
}
