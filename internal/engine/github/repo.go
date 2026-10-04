package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/domain"
)

const (
	repoReleaseExcerpt = 1500   // chars of latest-release notes shown on a repo root
	maxFileBytes       = 400000 // file content past this is cut (the API serves up to 100MB)
	maxTreeEntries     = 500    // directory entries listed (the contents API stops at 1000)
)

// crawlRepo renders a repository root: metadata, latest release, and README.
// Three requests, run concurrently; the README and release are optional.
func (e *Engine) crawlRepo(ctx context.Context, rawURL string, t target) (domain.Document, error) {
	var (
		repo    apiRepo
		readme  *apiContent
		release *apiRelease
	)
	err := runAll(
		func() error { return e.getJSON(ctx, e.repoURL(t, ""), "repository", &repo) },
		func() error {
			var c apiContent
			err := e.getJSON(ctx, e.repoURL(t, "readme"), "readme", &c)
			if isNotFound(err) {
				return nil
			}
			readme = &c
			return err
		},
		func() error {
			var r apiRelease
			err := e.getJSON(ctx, e.repoURL(t, "releases/latest"), "latest release", &r)
			if isNotFound(err) {
				return nil
			}
			release = &r
			return err
		},
	)
	if err != nil {
		return domain.Document{}, err
	}

	var b strings.Builder
	name := t.owner + "/" + t.repo
	if repo.FullName != "" {
		name = repo.FullName
	}
	fmt.Fprintf(&b, "# %s\n\n", name)
	if d := oneLine(repo.Description); d != "" {
		b.WriteString(d + "\n\n")
	}
	field := func(label, value string) {
		if value != "" {
			fmt.Fprintf(&b, "- %s: %s\n", label, value)
		}
	}
	field("Topics", strings.Join(repo.Topics, ", "))
	fmt.Fprintf(&b, "- Stars: %d, forks: %d, open issues and PRs: %d\n", repo.Stars, repo.Forks, repo.OpenIssues)
	field("Language", repo.Language)
	if repo.License != nil && repo.License.SPDX != "" && repo.License.SPDX != "NOASSERTION" {
		field("License", repo.License.SPDX)
	} else if repo.License != nil {
		field("License", repo.License.Name)
	}
	field("Homepage", repo.Homepage)
	field("Default branch", repo.DefaultBranch)
	field("Last push", day(repo.PushedAt))
	if repo.Parent != nil {
		field("Fork of", repo.Parent.FullName)
	}
	if repo.Archived {
		field("Archived", "yes (read-only)")
	}
	if release != nil {
		b.WriteString("\n" + releaseHeading("##", "Latest release", release))
		if notes, cut := excerpt(release.Body, repoReleaseExcerpt); notes != "" {
			b.WriteString("\n" + demoteHeadings(notes, 3) + "\n")
			if cut {
				fmt.Fprintf(&b, "\n[Release notes cut; full notes: %s]\n", release.HTMLURL)
			}
		}
	}
	if readme != nil {
		text, derr := readme.decode()
		if derr != nil {
			return domain.Document{}, derr
		}
		links := repoLinks{owner: t.owner, repo: t.repo, ref: repo.DefaultBranch, dir: path.Dir(readme.Path)}
		fmt.Fprintf(&b, "\n## README (%s)\n\n%s\n", readme.Path, cleanMarkdown(text, links))
	}
	return e.markdownDocument(b.String(), rawURL, map[string]string{"github_kind": kindNames[t.kind]}), nil
}

// releaseHeading renders "<hashes> <label>: <tag>[ — name] (date)".
func releaseHeading(hashes, label string, r *apiRelease) string {
	h := hashes + " "
	if label != "" {
		h += label + ": "
	}
	h += r.TagName
	if n := oneLine(r.Name); n != "" && n != r.TagName {
		h += ", " + n
	}
	if r.PublishedAt != "" {
		h += " (" + day(r.PublishedAt) + ")"
	}
	return h + "\n"
}

// crawlBlob renders one file. The ref may contain "/", so each plausible
// ref/path split is tried in turn until one isn't a 404.
func (e *Engine) crawlBlob(ctx context.Context, rawURL string, t target) (domain.Document, error) {
	var lastErr error = &domain.FetchError{Kind: domain.KindError, Err: errors.New("blob url has no file path")}
	for _, rp := range refSplits(t.rest, true) {
		body, err := e.getRaw(ctx, e.contentsURL(t, rp))
		if isNotFound(err) {
			lastErr = err
			continue
		}
		if err != nil {
			return domain.Document{}, fmt.Errorf("fetch file: %w", err)
		}
		return e.markdownDocument(renderFile(t, rp, body), rawURL, map[string]string{
			"github_kind": kindNames[t.kind], "ref": rp.ref, "path": rp.path,
		}), nil
	}
	return domain.Document{}, fmt.Errorf("fetch file: %w", lastErr)
}

// renderFile renders a file's bytes: markdown as markdown, prose as is, code
// fenced with its language, binaries as a one-line note.
func renderFile(t target, rp refPath, body []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s/%s: %s\n\n", t.owner, t.repo, rp.path)
	fmt.Fprintf(&b, "- Ref: %s\n- Size: %d bytes\n\n", rp.ref, len(body))
	if bytes.IndexByte(body[:min(len(body), 8000)], 0) >= 0 || !utf8.Valid(body[:min(len(body), 8000)]) {
		b.WriteString("Binary file, not shown.\n")
		return b.String()
	}
	note := ""
	if len(body) > maxFileBytes {
		cut := body[:maxFileBytes]
		if i := bytes.LastIndexByte(cut, '\n'); i > 0 {
			cut = cut[:i]
		}
		note = fmt.Sprintf("\n\n[File cut at %d of %d bytes.]", len(cut), len(body))
		body = cut
	}
	text := strings.ToValidUTF8(string(body), "")
	switch {
	case isMarkdownFile(rp.path):
		b.WriteString(cleanMarkdown(text, repoLinks{owner: t.owner, repo: t.repo, ref: rp.ref, dir: path.Dir(rp.path)}))
	case isProseFile(rp.path):
		b.WriteString(strings.TrimSpace(text))
	default:
		b.WriteString(fenced(text, codeLang(rp.path)))
	}
	b.WriteString(note)
	return b.String()
}

// crawlTree renders a directory listing plus the directory's README, if any.
func (e *Engine) crawlTree(ctx context.Context, rawURL string, t target) (domain.Document, error) {
	var lastErr error
	for _, rp := range refSplits(t.rest, false) {
		var (
			entries []apiDirEntry
			readme  *apiContent
		)
		err := runAll(
			func() error { return e.getJSON(ctx, e.contentsURL(t, rp), "directory", &entries) },
			func() error {
				var c apiContent
				u := e.repoURL(t, "readme")
				if rp.path != "" {
					u += "/" + escapePath(rp.path)
				}
				err := e.getJSON(ctx, u+"?ref="+url.QueryEscape(rp.ref), "readme", &c)
				if isNotFound(err) {
					return nil
				}
				readme = &c
				return err
			},
		)
		if isNotFound(err) {
			lastErr = err
			continue
		}
		if err != nil {
			return domain.Document{}, err
		}
		doc, rerr := renderTree(t, rp, entries, readme)
		if rerr != nil {
			return domain.Document{}, rerr
		}
		return e.markdownDocument(doc, rawURL, map[string]string{
			"github_kind": kindNames[t.kind], "ref": rp.ref, "path": rp.path,
		}), nil
	}
	return domain.Document{}, fmt.Errorf("fetch directory: %w", lastErr)
}

func renderTree(t target, rp refPath, entries []apiDirEntry, readme *apiContent) (string, error) {
	var b strings.Builder
	dir := rp.path
	if dir == "" {
		dir = "/"
	}
	fmt.Fprintf(&b, "# %s/%s: %s\n\n- Ref: %s\n\n", t.owner, t.repo, dir, rp.ref)
	// Directories first, then files, each alphabetical (GitHub's own order).
	sort.SliceStable(entries, func(i, j int) bool {
		di, dj := entries[i].Type == "dir", entries[j].Type == "dir"
		if di != dj {
			return di
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	fmt.Fprintf(&b, "## Entries (%d)\n\n", len(entries))
	for i, en := range entries {
		if i == maxTreeEntries {
			fmt.Fprintf(&b, "- … %d more\n", len(entries)-maxTreeEntries)
			break
		}
		switch en.Type {
		case "dir":
			fmt.Fprintf(&b, "- %s/\n", en.Name)
		case "file":
			fmt.Fprintf(&b, "- %s (%s)\n", en.Name, humanBytes(en.Size))
		default: // symlink, submodule
			fmt.Fprintf(&b, "- %s (%s)\n", en.Name, en.Type)
		}
	}
	if readme != nil {
		text, err := readme.decode()
		if err != nil {
			return "", err
		}
		links := repoLinks{owner: t.owner, repo: t.repo, ref: rp.ref, dir: path.Dir(readme.Path)}
		fmt.Fprintf(&b, "\n## README (%s)\n\n%s\n", readme.Path, cleanMarkdown(text, links))
	}
	return b.String(), nil
}

// humanBytes formats a byte count compactly (812 B, 4.2 KB, 1.3 MB).
func humanBytes(n int64) string {
	switch {
	case n < 1024:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1024*1024:
		return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KB"
	default:
		return strconv.FormatFloat(float64(n)/(1024*1024), 'f', 1, 64) + " MB"
	}
}

// contentsURL builds the contents-API URL of a path at a ref.
func (e *Engine) contentsURL(t target, rp refPath) string {
	u := e.repoURL(t, "contents")
	if rp.path != "" {
		u += "/" + escapePath(rp.path)
	}
	return u + "?ref=" + url.QueryEscape(rp.ref)
}

// getJSON fetches an API URL and decodes its JSON body into out; what names the
// resource in error messages.
func (e *Engine) getJSON(ctx context.Context, apiURL, what string, out any) error {
	raw, _, err := e.get(ctx, apiURL)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", what, err)
	}
	if jerr := json.Unmarshal(raw, out); jerr != nil {
		return &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse %s: %w", what, jerr)}
	}
	return nil
}

// decode returns the file text of a contents-API object (base64 in the JSON).
func (c *apiContent) decode() (string, error) {
	if c.Encoding != "base64" {
		return c.Content, nil
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(c.Content, "\n", ""))
	if err != nil {
		return "", &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("decode %s: %w", c.Path, err)}
	}
	return strings.ToValidUTF8(string(raw), ""), nil
}
