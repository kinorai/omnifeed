package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// Matches must claim issue and pull-request pages only; every other github.com
// URL falls through to the generic crawl4ai fallback.
func TestMatches(t *testing.T) {
	e := &Engine{}

	claim := []string{
		"https://github.com/kinorai/omnifeed/issues/12",
		"https://github.com/kinorai/omnifeed/pull/3",
		"https://github.com/kinorai/omnifeed/issues/12/",                 // trailing slash
		"https://github.com/kinorai/omnifeed/issues/12#issuecomment-999", // fragment
		"https://github.com/kinorai/omnifeed/pull/3?w=1",                 // query
		"https://www.github.com/kinorai/omnifeed/issues/12",              // subdomain
		"https://github.com/unclecode/crawl4ai/pull/2111",
		"https://github.com/some-org/repo.name_x/issues/1", // name charset
	}
	for _, u := range claim {
		if !e.Matches(u) {
			t.Errorf("Matches(%q) = false, want true", u)
		}
	}

	fallThrough := []string{
		"https://github.com/kinorai/omnifeed",                          // repo root
		"https://github.com/some.owner/repo/issues/1",                  // dot in an owner name
		"https://github.com/kinorai",                                   // owner
		"https://github.com/kinorai/omnifeed/issues",                   // issue list
		"https://github.com/kinorai/omnifeed/pulls",                    // PR list
		"https://github.com/kinorai/omnifeed/issues/abc",               // non-numeric
		"https://github.com/kinorai/omnifeed/pull/3/files",             // sub-page
		"https://github.com/kinorai/omnifeed/blob/main/README.md",      // blob
		"https://github.com/kinorai/omnifeed/tree/main/internal",       // tree
		"https://github.com/kinorai/omnifeed/actions",                  // actions
		"https://github.com/kinorai/omnifeed/releases/tag/v0.14.0",     // releases
		"https://github.com/kinorai/omnifeed/discussions/7",            // discussions
		"https://gist.github.com/kinorai/deadbeef",                     // gist
		"https://example.com/kinorai/omnifeed/issues/12",               // not GitHub
		"https://github.com.evil.com/kinorai/omnifeed/issues/12",       // lookalike host
		"https://raw.githubusercontent.com/kinorai/omnifeed/issues/12", // not github.com
	}
	for _, u := range fallThrough {
		if e.Matches(u) {
			t.Errorf("Matches(%q) = true, want false (should fall through)", u)
		}
	}
}

// issueJSON is the /issues/{n} response used by the issue tests.
const issueJSON = `{"title":"Crash on startup","state":"open","created_at":"2026-01-02T03:04:05Z",
	"body":"it crashes","comments":3,"user":{"login":"alice"},"labels":[{"name":"bug"},{"name":"p1"}]}`

// An issue crawl must fetch the issue plus every comment page the Link header
// advertises, keep page order, and report the comment count.
func TestCrawlIssuePaginatesComments(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch {
		case r.URL.Path == "/repos/o/r/issues/7":
			_, _ = io.WriteString(w, issueJSON)
		case r.URL.Path == "/repos/o/r/issues/7/comments" && r.URL.Query().Get("page") == "":
			base := "http://" + r.Host
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues/7/comments?per_page=100&page=2>; rel="next", `+
				`<%s/repos/o/r/issues/7/comments?per_page=100&page=2>; rel="last"`, base, base))
			_, _ = io.WriteString(w, `[{"user":{"login":"bob"},"created_at":"2026-01-03T00:00:00Z","body":"page one comment"}]`)
		case r.URL.Path == "/repos/o/r/issues/7/comments":
			_, _ = io.WriteString(w, `[{"user":{"login":"carol"},"created_at":"2026-01-04T00:00:00Z","body":"page two comment"}]`)
		default:
			t.Errorf("unexpected path %q", r.URL.RequestURI())
		}
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	doc, err := e.Crawl(context.Background(), "https://github.com/o/r/issues/7", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("fetched %d urls (%v), want 3 (issue + 2 comment pages)", len(paths), paths)
	}
	if doc.Metadata["comments"] != "2" {
		t.Fatalf("comments = %q, want 2\n%s", doc.Metadata["comments"], doc.PageContent)
	}
	if got := doc.Metadata[domain.ContentTypeKey]; got != domain.ContentTypeTOON {
		t.Fatalf("content_type = %q, want %q", got, domain.ContentTypeTOON)
	}
	if _, ok := doc.Metadata["truncated_from"]; ok {
		t.Errorf("unexpected truncated_from = %q", doc.Metadata["truncated_from"])
	}
	for _, want := range []string{"Crash on startup", "alice", "bug", "bob", "page one comment", "carol", "page two comment"} {
		if !strings.Contains(doc.PageContent, want) {
			t.Errorf("output missing %q:\n%s", want, doc.PageContent)
		}
	}
	// Page order must survive.
	if i, j := strings.Index(doc.PageContent, "page one comment"), strings.Index(doc.PageContent, "page two comment"); i > j {
		t.Errorf("page two sorted before page one:\n%s", doc.PageContent)
	}
}

// More comments than the cap must be truncated, with truncated_from reporting the
// upstream total.
func TestCrawlIssueCapsComments(t *testing.T) {
	var page strings.Builder
	page.WriteString("[")
	for i := 0; i < maxComments+10; i++ {
		if i > 0 {
			page.WriteString(",")
		}
		fmt.Fprintf(&page, `{"user":{"login":"u%d"},"created_at":"2026-01-03T00:00:00Z","body":"c%d"}`, i, i)
	}
	page.WriteString("]")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			_, _ = io.WriteString(w, page.String())
			return
		}
		_, _ = io.WriteString(w, issueJSON)
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	doc, err := e.Crawl(context.Background(), "https://github.com/o/r/issues/7", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if got, want := doc.Metadata["comments"], fmt.Sprint(maxComments); got != want {
		t.Fatalf("comments = %q, want %q", got, want)
	}
	if got, want := doc.Metadata["truncated_from"], fmt.Sprint(maxComments+10); got != want {
		t.Fatalf("truncated_from = %q, want %q", got, want)
	}
}

// pullHandler serves the five PR endpoints from the given bodies, recording the
// paths it was asked for.
func pullHandler(t *testing.T, paths *[]string, files string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		*paths = append(*paths, r.URL.Path)
		switch r.URL.Path {
		case "/repos/o/r/pulls/9":
			_, _ = io.WriteString(w, `{"title":"Add engine","state":"open","draft":false,"merged":false,
				"created_at":"2026-02-01T00:00:00Z","body":"pr body","comments":1,"additions":10,"deletions":2,
				"changed_files":2,"user":{"login":"alice"},"labels":[{"name":"feat"}]}`)
		case "/repos/o/r/issues/9/comments":
			_, _ = io.WriteString(w, `[{"user":{"login":"bob"},"created_at":"2026-02-02T00:00:00Z","body":"conversation comment"}]`)
		case "/repos/o/r/pulls/9/comments":
			_, _ = io.WriteString(w, `[{"path":"internal/x.go","line":42,"in_reply_to_id":555,
				"user":{"login":"carol"},"created_at":"2026-02-03T00:00:00Z","body":"inline nit"}]`)
		case "/repos/o/r/pulls/9/reviews":
			_, _ = io.WriteString(w, `[{"user":{"login":"dave"},"state":"APPROVED","submitted_at":"2026-02-04T00:00:00Z","body":"lgtm"}]`)
		case "/repos/o/r/pulls/9/files":
			_, _ = io.WriteString(w, files)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}
}

// A PR crawl must hit all five endpoints and emit conversation comments, inline
// review comments, reviews, and files with patches.
func TestCrawlPullRequest(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(pullHandler(t, &paths, `[{"filename":"internal/x.go","status":"modified",
		"additions":10,"deletions":2,"patch":"@@ -1 +1 @@\n-old\n+new"}]`))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	doc, err := e.Crawl(context.Background(), "https://github.com/o/r/pull/9", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	for _, want := range []string{
		"/repos/o/r/pulls/9", "/repos/o/r/issues/9/comments",
		"/repos/o/r/pulls/9/comments", "/repos/o/r/pulls/9/reviews", "/repos/o/r/pulls/9/files",
	} {
		if !slices.Contains(paths, want) {
			t.Errorf("endpoint %q not fetched (got %v)", want, paths)
		}
	}
	if got := doc.Metadata[domain.ContentTypeKey]; got != domain.ContentTypeTOON {
		t.Fatalf("content_type = %q, want %q", got, domain.ContentTypeTOON)
	}
	if doc.Metadata["comments"] != "1" {
		t.Errorf("comments = %q, want 1", doc.Metadata["comments"])
	}
	for _, want := range []string{
		"Add engine", "alice", "feat", // pr header
		"conversation comment", "bob", // issues/{n}/comments
		"inline nit", "carol", "internal/x.go", "42", // pulls/{n}/comments
		"lgtm", "dave", "APPROVED", // reviews
		"modified", "+new", // files + patch
	} {
		if !strings.Contains(doc.PageContent, want) {
			t.Errorf("output missing %q:\n%s", want, doc.PageContent)
		}
	}
	if _, ok := doc.Metadata["diff_truncated"]; ok {
		t.Errorf("unexpected diff_truncated")
	}
}

// Past the patch budget, files keep their name and stats but lose the patch, and
// the document is flagged diff_truncated.
func TestCrawlPullRequestDiffBudget(t *testing.T) {
	big := strings.Repeat("x", maxDiffBytes-10)
	files := fmt.Sprintf(`[{"filename":"first.go","status":"modified","additions":1,"deletions":0,"patch":%q},
		{"filename":"second.go","status":"added","additions":3,"deletions":0,"patch":"@@ dropped patch @@"}]`, big)

	var paths []string
	srv := httptest.NewServer(pullHandler(t, &paths, files))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	doc, err := e.Crawl(context.Background(), "https://github.com/o/r/pull/9", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if doc.Metadata["diff_truncated"] != "true" {
		t.Fatalf("diff_truncated = %q, want true", doc.Metadata["diff_truncated"])
	}
	// The second file is still listed with its stats, but without patch text.
	if !strings.Contains(doc.PageContent, "second.go") {
		t.Errorf("output missing second.go:\n%s", doc.PageContent)
	}
	if strings.Contains(doc.PageContent, "dropped patch") {
		t.Errorf("patch past the budget was emitted")
	}
}

// A quota refusal must surface as a typed FetchError naming the reset and the
// token knob — never a silent fall-through to the browser.
func TestCrawlRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-ratelimit-remaining", "0")
		w.Header().Set("x-ratelimit-reset", "1800000000")
		http.Error(w, "API rate limit exceeded", http.StatusForbidden)
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	_, err := e.Crawl(context.Background(), "https://github.com/o/r/issues/7", domain.EngineOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.StatusCode != http.StatusForbidden {
		t.Fatalf("want *FetchError with 403, got %v", err)
	}
	for _, want := range []string{"rate limited until 1800000000", "OMNIFEED_GITHUB_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// A non-200 without a spent quota must not claim rate limiting.
func TestCrawlNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	_, err := e.Crawl(context.Background(), "https://github.com/o/r/issues/7", domain.EngineOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.StatusCode != http.StatusNotFound {
		t.Fatalf("want *FetchError with 404, got %v", err)
	}
	if strings.Contains(err.Error(), "rate limited") {
		t.Errorf("404 wrongly reported as rate limited: %v", err)
	}
}

// A 200 with an unparseable body must classify as KindBadResponse.
func TestCrawlParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}))
	defer srv.Close()

	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	_, err := e.Crawl(context.Background(), "https://github.com/o/r/issues/7", domain.EngineOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindBadResponse {
		t.Fatalf("want KindBadResponse, got %v", err)
	}
}

// A configured token must be sent as a bearer credential on every request; the
// Accept and User-Agent headers GitHub requires must always be present.
func TestCrawlSendsHeaders(t *testing.T) {
	for _, tc := range []struct{ name, token, wantAuth string }{
		{"token set", "ghp_secret", "Bearer ghp_secret"},
		{"anonymous", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth, gotAccept, gotUA string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth, gotAccept, gotUA = r.Header.Get("Authorization"), r.Header.Get("Accept"), r.Header.Get("User-Agent")
				if strings.HasSuffix(r.URL.Path, "/comments") {
					_, _ = io.WriteString(w, `[]`)
					return
				}
				_, _ = io.WriteString(w, issueJSON)
			}))
			defer srv.Close()

			e := New(Config{Client: httpx.New(nil), APIBase: srv.URL, Token: tc.token})
			if _, err := e.Crawl(context.Background(), "https://github.com/o/r/issues/7", domain.EngineOptions{}); err != nil {
				t.Fatalf("Crawl: %v", err)
			}
			if gotAuth != tc.wantAuth {
				t.Errorf("Authorization = %q, want %q", gotAuth, tc.wantAuth)
			}
			if gotAccept != "application/vnd.github+json" {
				t.Errorf("Accept = %q, want application/vnd.github+json", gotAccept)
			}
			if gotUA != userAgent {
				t.Errorf("User-Agent = %q, want %q", gotUA, userAgent)
			}
		})
	}
}
