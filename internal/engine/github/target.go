package github

import (
	"net/url"
	"regexp"
	"strings"
)

// kind is the GitHub page type a URL resolves to; each kind has its own
// renderer.
type kind int

const (
	kindIssue         kind = iota // /{o}/{r}/issues/{n}
	kindPull                      // /{o}/{r}/pull/{n}
	kindRepo                      // /{o}/{r}
	kindBlob                      // /{o}/{r}/blob/{ref}/{path}
	kindTree                      // /{o}/{r}/tree/{ref}[/{path}]
	kindReleases                  // /{o}/{r}/releases
	kindRelease                   // /{o}/{r}/releases/tag/{tag}
	kindLatestRelease             // /{o}/{r}/releases/latest
	kindDiscussion                // /{o}/{r}/discussions/{n}
	kindCommit                    // /{o}/{r}/commit/{sha}, /{o}/{r}/pull/{n}/commits/{sha}
	kindGist                      // gist.github.com/[{user}/]{id}
)

// kindNames label each kind in the document metadata (github_kind).
var kindNames = map[kind]string{
	kindIssue: "issue", kindPull: "pull", kindRepo: "repo", kindBlob: "blob", kindTree: "tree",
	kindReleases: "releases", kindRelease: "release", kindLatestRelease: "release",
	kindDiscussion: "discussion", kindCommit: "commit", kindGist: "gist",
}

// target is the resolved fetch plan for a GitHub URL.
type target struct {
	kind   kind
	owner  string
	repo   string
	number string // issue, pull request, or discussion number
	pull   bool   // /pull/{n} rather than /issues/{n} (kindIssue/kindPull)
	rest   string // blob/tree: "{ref}/{path}", split later (refs may contain "/")
	tag    string // kindRelease
	sha    string // kindCommit
	gistID string // kindGist
}

// Owner names are [A-Za-z0-9-]; repository names additionally allow "." and "_".
const (
	ownerPat = `([A-Za-z0-9-]+)`
	repoPat  = `([A-Za-z0-9._-]+)`
)

// Each pattern claims exactly one page kind. Anything not listed (actions,
// settings, wiki, compare, issue lists, …) falls through to the generic engine.
var (
	issuePullRE  = regexp.MustCompile(`^/` + ownerPat + `/` + repoPat + `/(issues|pull)/([0-9]+)$`)
	repoRE       = regexp.MustCompile(`^/` + ownerPat + `/` + repoPat + `$`)
	blobTreeRE   = regexp.MustCompile(`^/` + ownerPat + `/` + repoPat + `/(blob|tree)/(.+)$`)
	releasesRE   = regexp.MustCompile(`^/` + ownerPat + `/` + repoPat + `/releases$`)
	latestRE     = regexp.MustCompile(`^/` + ownerPat + `/` + repoPat + `/releases/latest$`)
	releaseTagRE = regexp.MustCompile(`^/` + ownerPat + `/` + repoPat + `/releases/tag/(.+)$`)
	discussionRE = regexp.MustCompile(`^/` + ownerPat + `/` + repoPat + `/discussions/([0-9]+)$`)
	commitRE     = regexp.MustCompile(`^/` + ownerPat + `/` + repoPat + `/(?:commit|pull/[0-9]+/commits)/([0-9a-fA-F]{7,40})$`)
	// A gist is /{user}/{id}, or /{id} alone; bare ids are 20+ hex chars, so a
	// short single segment (a user page) never matches. Legacy ids are decimal.
	gistRE = regexp.MustCompile(`^/(?:[A-Za-z0-9-]+/([0-9a-fA-F]+)|([0-9a-fA-F]{20,}))$`)
)

// reservedOwners are first path segments of github.com that are site pages, not
// accounts, so /{segment}/{x} is never a repository (e.g. /topics/go,
// /orgs/kinorai). Claiming them would cost an API 404 and a fallback.
var reservedOwners = map[string]bool{
	"about": true, "account": true, "advisories": true, "apps": true, "codespaces": true,
	"collections": true, "contact": true, "copilot": true, "customer-stories": true,
	"dashboard": true, "enterprise": true, "enterprises": true, "events": true, "explore": true,
	"features": true, "github-copilot": true, "issues": true, "login": true, "marketplace": true,
	"models": true, "new": true, "notifications": true, "organizations": true, "orgs": true,
	"pricing": true, "pulls": true, "readme": true, "resources": true, "search": true,
	"security": true, "settings": true, "site": true, "solutions": true, "sponsors": true,
	"stars": true, "team": true, "topics": true, "trending": true, "users": true,
}

// parseTarget classifies a GitHub URL into a fetch plan. ok is false for any URL
// this engine doesn't render (so it falls through to the generic engine).
func parseTarget(rawURL string) (target, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return target{}, false
	}
	// url.Parse already split off the query and fragment; normalize a trailing
	// slash so /issues/12/ matches like /issues/12.
	p := strings.TrimSuffix(u.Path, "/")
	switch strings.ToLower(u.Hostname()) {
	case "gist.github.com":
		if m := gistRE.FindStringSubmatch(p); m != nil {
			return target{kind: kindGist, gistID: m[1] + m[2]}, true
		}
		return target{}, false
	case "github.com", "www.github.com":
	default:
		// Other subdomains (docs., api., skills., …) are not repository hosts.
		return target{}, false
	}

	if m := issuePullRE.FindStringSubmatch(p); m != nil {
		t := target{kind: kindIssue, owner: m[1], repo: m[2], number: m[4], pull: m[3] == "pull"}
		if t.pull {
			t.kind = kindPull
		}
		return t, true
	}
	if m := repoRE.FindStringSubmatch(p); m != nil {
		if reservedOwners[strings.ToLower(m[1])] {
			return target{}, false
		}
		return target{kind: kindRepo, owner: m[1], repo: m[2]}, true
	}
	if m := blobTreeRE.FindStringSubmatch(p); m != nil {
		k := kindBlob
		if m[3] == "tree" {
			k = kindTree
		} else if !strings.Contains(m[4], "/") {
			return target{}, false // a blob URL needs a file path after the ref
		}
		return target{kind: k, owner: m[1], repo: m[2], rest: m[4]}, true
	}
	if m := releasesRE.FindStringSubmatch(p); m != nil {
		return target{kind: kindReleases, owner: m[1], repo: m[2]}, true
	}
	if m := latestRE.FindStringSubmatch(p); m != nil {
		return target{kind: kindLatestRelease, owner: m[1], repo: m[2]}, true
	}
	if m := releaseTagRE.FindStringSubmatch(p); m != nil {
		return target{kind: kindRelease, owner: m[1], repo: m[2], tag: m[3]}, true
	}
	if m := discussionRE.FindStringSubmatch(p); m != nil {
		return target{kind: kindDiscussion, owner: m[1], repo: m[2], number: m[3]}, true
	}
	if m := commitRE.FindStringSubmatch(p); m != nil {
		return target{kind: kindCommit, owner: m[1], repo: m[2], sha: m[3]}, true
	}
	return target{}, false
}

// maxRefSegments bounds how many leading path segments of a blob/tree URL are
// tried as the ref. A ref may contain "/" (feature/x), and the URL alone can't
// tell where the ref ends and the path begins; each extra guess costs a request.
const maxRefSegments = 3

// refPath is one way to split "{ref}/{path}".
type refPath struct {
	ref  string
	path string
}

// refSplits returns the candidate ref/path splits of rest, shortest ref first.
// When needPath is set (blobs), the path must be non-empty.
func refSplits(rest string, needPath bool) []refPath {
	segs := strings.Split(rest, "/")
	var out []refPath
	for i := 1; i <= len(segs) && i <= maxRefSegments; i++ {
		rp := refPath{ref: strings.Join(segs[:i], "/"), path: strings.Join(segs[i:], "/")}
		if needPath && rp.path == "" {
			break
		}
		out = append(out, rp)
	}
	return out
}

// escapePath escapes each segment of a repository path for an API URL.
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}
