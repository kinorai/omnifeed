package github

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// The rendering tests serve responses recorded from api.github.com (trimmed;
// see testdata/) through an httptest fake of the API.

// reply is one canned API response: a testdata fixture file or an inline body.
type reply struct {
	status      int    // 0 = 200
	fixture     string // file under testdata/
	body        string
	contentType string // "" = sniffed by net/http
}

// apiFake serves routes keyed by "METHOD /path" (query ignored) and records
// every request; unknown routes are 404s, as on the real API.
type apiFake struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
}

func (f *apiFake) start(t *testing.T, routes map[string]reply) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, r)
		f.bodies = append(f.bodies, string(body))
		f.mu.Unlock()
		rp, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if rp.contentType != "" {
			w.Header().Set("Content-Type", rp.contentType)
		}
		if rp.status != 0 {
			w.WriteHeader(rp.status)
		}
		if rp.fixture != "" {
			b, err := os.ReadFile(filepath.Join("testdata", rp.fixture))
			if err != nil {
				t.Errorf("fixture: %v", err)
			}
			_, _ = w.Write(b)
			return
		}
		_, _ = io.WriteString(w, rp.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// paths returns "METHOD /path?query" for every request, in arrival order.
func (f *apiFake) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.requests))
	for _, r := range f.requests {
		out = append(out, r.Method+" "+r.URL.RequestURI())
	}
	return out
}

// crawl runs one crawl against a fake API serving routes.
func crawl(t *testing.T, rawURL, token string, routes map[string]reply) (domain.Document, *apiFake, error) {
	t.Helper()
	f := &apiFake{}
	srv := f.start(t, routes)
	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL, Token: token})
	doc, err := e.Crawl(context.Background(), rawURL, domain.EngineOptions{})
	return doc, f, err
}

// assertMarkdown checks the common shape of every markdown page: content type,
// kind label, and the H1 title as the very first line.
func assertMarkdown(t *testing.T, doc domain.Document, kind, h1 string) {
	t.Helper()
	if got := doc.Metadata[domain.ContentTypeKey]; got != domain.ContentTypeMarkdown {
		t.Errorf("content_type = %q, want markdown", got)
	}
	if got := doc.Metadata["github_kind"]; got != kind {
		t.Errorf("github_kind = %q, want %q", got, kind)
	}
	if first, _, _ := strings.Cut(doc.PageContent, "\n"); first != "# "+h1 {
		t.Errorf("first line = %q, want %q", first, "# "+h1)
	}
}

func assertContains(t *testing.T, content string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(content, w) {
			t.Errorf("output missing %q:\n%s", w, content)
		}
	}
}

func assertNotContains(t *testing.T, content string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(content, w) {
			t.Errorf("output unexpectedly contains %q:\n%s", w, content)
		}
	}
}

// A repo root costs three concurrent requests (repo, README, latest release)
// and renders the metadata, a release excerpt, then the cleaned README.
func TestCrawlRepoRoot(t *testing.T) {
	doc, f, err := crawl(t, "https://github.com/longhorn/longhorn", "", map[string]reply{
		"GET /repos/longhorn/longhorn":                 {fixture: "repo.json"},
		"GET /repos/longhorn/longhorn/readme":          {fixture: "readme.json"},
		"GET /repos/longhorn/longhorn/releases/latest": {fixture: "release_latest.json"},
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if n := len(f.paths()); n != 3 {
		t.Errorf("made %d requests (%v), want 3", n, f.paths())
	}
	assertMarkdown(t, doc, "repo", "longhorn/longhorn")
	c := doc.PageContent
	assertContains(t, c,
		"Cloud-Native distributed storage built on and for Kubernetes",
		"- Topics: cncf, distributed-systems, high-availability",
		"- Stars: 8017, forks: 739, open issues and PRs: 1929",
		"- Language: Shell", "- License: Apache-2.0", "- Homepage: https://longhorn.io",
		"- Default branch: master", "- Last push: 2026-10-04",
		"## Latest release: v1.13.0, Longhorn v1.13.0 (2026-09-29)",
		"#### Longhorn v1.13.0 Release Notes", // demoted under the release heading
		"[Release notes cut; full notes: https://github.com/longhorn/longhorn/releases/tag/v1.13.0]",
		"## README (README.md)",
		"Visit [longhorn.io](https://longhorn.io/) for docs.",
		"[install guide](https://github.com/longhorn/longhorn/blob/master/docs/install.md)",
		"[chart](https://github.com/longhorn/longhorn/blob/master/chart)",
		"![Architecture](https://raw.githubusercontent.com/longhorn/longhorn/master/docs/arch.png)",
		"[contrib]: https://github.com/longhorn/longhorn/blob/master/CONTRIBUTING.md",
		"<p align=\"center\"><img src=\"keep-me.png\"></p>\n[not a link](relative.md)", // code block verbatim
	)
	assertNotContains(t, c, "logo block", "logo.png", "badge.svg", "goreportcard", "Archived", "Fork of")
	if i, j := strings.Index(c, "## Latest release"), strings.Index(c, "## README"); i < 0 || i > j {
		t.Errorf("release section must precede the README")
	}
}

// A repository with no README and no release still renders: both are
// optional, so their 404s are not errors.
func TestCrawlRepoRootWithoutReadmeOrRelease(t *testing.T) {
	doc, _, err := crawl(t, "https://github.com/o/r", "", map[string]reply{
		"GET /repos/o/r": {body: `{"full_name":"o/r","description":"","default_branch":"main","archived":true,
			"parent":{"full_name":"up/r"},"stargazers_count":1}`},
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	assertMarkdown(t, doc, "repo", "o/r")
	assertContains(t, doc.PageContent, "- Fork of: up/r", "- Archived: yes")
	assertNotContains(t, doc.PageContent, "README", "Latest release")
}

// The repository itself failing (404, rate limit) is an error, so the
// registry falls back to the generic engine.
func TestCrawlRepoRootNotFound(t *testing.T) {
	_, _, err := crawl(t, "https://github.com/o/r", "", nil)
	if !isNotFound(err) {
		t.Fatalf("want a 404 FetchError, got %v", err)
	}
}

// A code file is fetched raw from the contents API and fenced with its
// language; a ref containing "/" is found by retrying the ref/path split.
func TestCrawlBlobCode(t *testing.T) {
	doc, f, err := crawl(t, "https://github.com/o/r/blob/feature/x/cmd/main.go", "", map[string]reply{
		// "feature" alone is not a ref: /contents/x/cmd/main.go?ref=feature 404s.
		"GET /repos/o/r/contents/cmd/main.go": {body: "package main\n\nfunc main() {}\n"},
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	want := []string{
		"GET /repos/o/r/contents/x/cmd/main.go?ref=feature",
		"GET /repos/o/r/contents/cmd/main.go?ref=feature%2Fx",
	}
	if got := f.paths(); !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
	if got := f.requests[1].Header.Get("Accept"); got != "application/vnd.github.raw+json" {
		t.Errorf("Accept = %q, want the raw media type", got)
	}
	assertMarkdown(t, doc, "blob", "o/r: cmd/main.go")
	assertContains(t, doc.PageContent, "- Ref: feature/x", "```go\npackage main\n\nfunc main() {}\n```")
	if doc.Metadata["ref"] != "feature/x" || doc.Metadata["path"] != "cmd/main.go" {
		t.Errorf("ref/path metadata = %q/%q", doc.Metadata["ref"], doc.Metadata["path"])
	}
}

// A markdown file is returned as markdown with its relative links resolved
// against its own directory.
func TestCrawlBlobMarkdown(t *testing.T) {
	doc, _, err := crawl(t, "https://github.com/o/r/blob/main/docs/guide.md", "", map[string]reply{
		"GET /repos/o/r/contents/docs/guide.md": {body: "# Guide\n\nSee [setup](setup.md) and ![d](img/d.png).\n"},
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	assertMarkdown(t, doc, "blob", "o/r: docs/guide.md")
	assertContains(t, doc.PageContent, "# Guide\n\nSee [setup](https://github.com/o/r/blob/main/docs/setup.md)",
		"![d](https://raw.githubusercontent.com/o/r/main/docs/img/d.png)")
	assertNotContains(t, doc.PageContent, "```")
}

// Binary content is not dumped into the document.
func TestCrawlBlobBinary(t *testing.T) {
	doc, _, err := crawl(t, "https://github.com/o/r/blob/main/logo.png", "", map[string]reply{
		"GET /repos/o/r/contents/logo.png": {body: "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"},
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	assertContains(t, doc.PageContent, "Binary file, not shown.")
	assertNotContains(t, doc.PageContent, "PNG")
}

// A blob URL naming a directory (GitHub redirects it to the tree view) gets
// the JSON entry list despite the raw media type, and renders as a tree.
func TestCrawlBlobDirectory(t *testing.T) {
	doc, _, err := crawl(t, "https://github.com/longhorn/longhorn/blob/master/chart", "", map[string]reply{
		"GET /repos/longhorn/longhorn/contents/chart": {fixture: "dir_chart.json", contentType: "application/json; charset=utf-8"},
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	assertMarkdown(t, doc, "tree", "longhorn/longhorn: chart")
	assertContains(t, doc.PageContent, "## Entries (9)", "- templates/\n")
}

// A file that exists under no ref/path split is an error (a 404 the registry
// turns into a generic-engine fallback).
func TestCrawlBlobNotFound(t *testing.T) {
	_, f, err := crawl(t, "https://github.com/o/r/blob/a/b/c/d/e.go", "", nil)
	if !isNotFound(err) {
		t.Fatalf("want a 404 FetchError, got %v", err)
	}
	if n := len(f.paths()); n != maxRefSegments {
		t.Errorf("made %d requests, want %d (one per ref guess)", n, maxRefSegments)
	}
}

// A directory renders its entries (directories first) and its README.
func TestCrawlTree(t *testing.T) {
	doc, f, err := crawl(t, "https://github.com/longhorn/longhorn/tree/master/chart", "", map[string]reply{
		"GET /repos/longhorn/longhorn/contents/chart": {fixture: "dir_chart.json"},
		"GET /repos/longhorn/longhorn/readme/chart": {body: `{"path":"chart/README.md","encoding":"base64",` +
			`"content":"IyBMb25naG9ybiBDaGFydAoKU2VlIFt2YWx1ZXNdKHZhbHVlcy55YW1sKS4K"}`}, // "# Longhorn Chart\n\nSee [values](values.yaml).\n"
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if !slices.Contains(f.paths(), "GET /repos/longhorn/longhorn/readme/chart?ref=master") {
		t.Errorf("directory README not requested at the ref: %v", f.paths())
	}
	assertMarkdown(t, doc, "tree", "longhorn/longhorn: chart")
	c := doc.PageContent
	assertContains(t, c, "- Ref: master", "## Entries (9)", "- templates/\n", "- values.yaml (57.0 KB)",
		"## README (chart/README.md)", "# Longhorn Chart",
		"[values](https://github.com/longhorn/longhorn/blob/master/chart/values.yaml)")
	if strings.Index(c, "- templates/") > strings.Index(c, "- .helmignore") {
		t.Errorf("directories must be listed before files:\n%s", c)
	}
}

// The release list renders the latest releases with their notes.
func TestCrawlReleases(t *testing.T) {
	doc, f, err := crawl(t, "https://github.com/kinorai/omnifeed/releases", "", map[string]reply{
		"GET /repos/kinorai/omnifeed/releases": {fixture: "releases.json"},
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if got := f.paths(); !slices.Equal(got, []string{"GET /repos/kinorai/omnifeed/releases?per_page=10"}) {
		t.Errorf("requests = %v", got)
	}
	assertMarkdown(t, doc, "releases", "kinorai/omnifeed releases")
	assertContains(t, doc.PageContent, "The 2 latest releases, newest first.",
		"## v0.29.0 (2026-08-27)", "## v0.28.0 (2026-08-26)", "by github-actions[bot]",
		"feat(httpx): honour the search concurrency cap without Redis")
	if doc.Metadata["releases"] != "2" {
		t.Errorf("releases = %q, want 2", doc.Metadata["releases"])
	}
}

// One release, by tag or the latest, renders full notes and assets.
func TestCrawlRelease(t *testing.T) {
	routes := map[string]reply{
		"GET /repos/longhorn/longhorn/releases/tags/v1.13.0": {fixture: "release_latest.json"},
		"GET /repos/longhorn/longhorn/releases/latest":       {fixture: "release_latest.json"},
	}
	for _, u := range []string{
		"https://github.com/longhorn/longhorn/releases/tag/v1.13.0",
		"https://github.com/longhorn/longhorn/releases/latest",
	} {
		doc, _, err := crawl(t, u, "", routes)
		if err != nil {
			t.Fatalf("Crawl(%s): %v", u, err)
		}
		assertMarkdown(t, doc, "release", "longhorn/longhorn: v1.13.0, Longhorn v1.13.0 (2026-09-29)")
		assertContains(t, doc.PageContent, "## Longhorn v1.13.0 Release Notes", "## Assets (2)", "downloads)")
		assertNotContains(t, doc.PageContent, "Release notes cut")
	}
}

// A commit renders message, author, stats, the file list, and the diff.
func TestCrawlCommit(t *testing.T) {
	doc, f, err := crawl(t, "https://github.com/kinorai/omnifeed/pull/5/commits/06c08d2", "", map[string]reply{
		"GET /repos/kinorai/omnifeed/commits/06c08d2": {fixture: "commit.json"},
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if n := len(f.paths()); n != 1 {
		t.Errorf("made %d requests, want 1", n)
	}
	assertMarkdown(t, doc, "commit", "kinorai/omnifeed@06c08d2: docs: restore README animations, tagline and bold emphasis")
	assertContains(t, doc.PageContent, "- Author: kinorai, 2026-09-25",
		"- SHA: 06c08d2f65a43b5d4146446ed86bb35ac4d0d467", "- Changes: +74 -74 in 2 files",
		"## Message\n\nCo-Authored-By:", "- README.md (modified, +29 -29)", "- SECURITY.md (modified, +1 -1)",
		"## Diff", "--- README.md\n@@ -4,7 +4,7 @@")
	assertNotContains(t, doc.PageContent, "Committer") // GitHub's web-flow committer is noise
}

// A gist renders its description and every file.
func TestCrawlGist(t *testing.T) {
	doc, f, err := crawl(t, "https://gist.github.com/schacon/1", "", map[string]reply{
		"GET /gists/1": {fixture: "gist.json"},
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if got := f.paths(); !slices.Equal(got, []string{"GET /gists/1"}) {
		t.Errorf("requests = %v", got)
	}
	assertMarkdown(t, doc, "gist", "Gist: the meaning of gist")
	assertContains(t, doc.PageContent, "- Owner: schacon", "## gistfile1.txt", "This is gist.")
}

// Gist code files are fenced with the language of their extension.
func TestCrawlGistCode(t *testing.T) {
	doc, _, err := crawl(t, "https://gist.github.com/u/abc123", "", map[string]reply{
		"GET /gists/abc123": {body: `{"description":"","files":{"b.py":{"language":"Python","content":"print(1)"},
			"a.md":{"language":"Markdown","content":"# Notes"}}}`},
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	assertMarkdown(t, doc, "gist", "Gist: a.md") // no description: the first file names it
	assertContains(t, doc.PageContent, "## a.md\n\n# Notes", "## b.py\n\n```python\nprint(1)\n```")
}

// A discussion goes through one authenticated GraphQL POST and renders the
// body, the accepted answer first, then comments with their replies.
func TestCrawlDiscussion(t *testing.T) {
	doc, f, err := crawl(t, "https://github.com/longhorn/longhorn/discussions/2189", "tok", map[string]reply{
		"POST /graphql": {fixture: "discussion.json"},
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if n := len(f.requests); n != 1 {
		t.Fatalf("made %d requests, want 1", n)
	}
	if got := f.requests[0].Header.Get("Authorization"); got != "Bearer tok" {
		t.Errorf("Authorization = %q", got)
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	if jerr := json.Unmarshal([]byte(f.bodies[0]), &req); jerr != nil {
		t.Fatalf("request body: %v", jerr)
	}
	if req.Variables["owner"] != "longhorn" || req.Variables["name"] != "longhorn" || req.Variables["number"] != float64(2189) {
		t.Errorf("variables = %v", req.Variables)
	}
	assertMarkdown(t, doc, "discussion", "Recreate existing cluster")
	c := doc.PageContent
	assertContains(t, c, "- Discussion: longhorn/longhorn#2189, category: General",
		"- Author: viceice, 2021-01-17, 1 upvote", "- Answered: yes", "- Comments: 3",
		"Whats the best way to recreate the longhorn cluster",
		"## Accepted answer, by yasker (2021-01-27)", "### joshimoo (2021-01-17, 1 upvote)",
		"#### Reply by viceice (2021-01-22)", "### yasker (2021-01-27, 1 upvote): accepted answer, shown above")
	if strings.Count(c, "We have an issue tracking the effort") != 1 {
		t.Errorf("the accepted answer must be rendered exactly once:\n%s", c)
	}
	if doc.Metadata["comments"] != "3" {
		t.Errorf("comments = %q, want 3", doc.Metadata["comments"])
	}
}

// GraphQL reports a missing discussion as a 200 carrying an errors array; it
// must surface as a 404 FetchError, not as an empty document.
func TestCrawlDiscussionNotFound(t *testing.T) {
	_, _, err := crawl(t, "https://github.com/o/r/discussions/9", "tok", map[string]reply{
		"POST /graphql": {body: `{"data":{"repository":{"discussion":null}},"errors":[{"type":"NOT_FOUND",` +
			`"message":"Could not resolve to a Discussion with the number of 9."}]}`},
	})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.StatusCode != http.StatusNotFound {
		t.Fatalf("want a 404 FetchError, got %v", err)
	}
}
