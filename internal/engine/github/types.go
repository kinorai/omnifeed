// Package github implements the GitHub engine. It reads the GitHub REST API
// (api.github.com) and renders:
//
//   - issues and pull requests as TOON (issue/PR + comments; a PR adds
//     reviews, inline review comments, and changed files with patches),
//     headed by a "# title" line;
//   - repository roots, files (blob), directories (tree), releases, commits,
//     and gists as compact markdown;
//   - discussions as markdown via the GraphQL API, which needs a token.
//
// Like the Hacker News engine (and unlike Reddit and the generic engine), this
// engine fetches its upstream DIRECTLY over HTTP rather than through crawl4ai:
// the API is public JSON with no bot wall, and a browser render of a GitHub
// page is slower and full of UI chrome (comments are lazily paginated in the
// DOM). It does require omnifeed to have outbound access to api.github.com.
package github

// Issue is the header of a GitHub issue, stripped to LLM-relevant fields.
type Issue struct {
	Title    string   `json:"title" toon:"title"`
	Author   string   `json:"author" toon:"author"`
	State    string   `json:"state" toon:"state"`
	Created  string   `json:"created" toon:"created"`
	Labels   []string `json:"labels,omitempty" toon:"labels,omitempty"`
	Comments int      `json:"comments" toon:"comments"`
	Body     string   `json:"body,omitempty" toon:"body,omitempty"`
}

// Comment is one conversation-tab comment (issue or PR), pruned to the three
// fields an agent actually reads.
type Comment struct {
	Login   string `json:"login" toon:"login"`
	Created string `json:"created" toon:"created"`
	Body    string `json:"body" toon:"body"`
}

// IssueThread groups an issue with its comments. Note, when set, tells the
// reader (the LLM) that a list was truncated — metadata keys land in MCP _meta,
// which models never see, so the signal has to live in the content itself.
type IssueThread struct {
	Issue    Issue     `json:"issue" toon:"issue"`
	Note     string    `json:"note,omitempty" toon:"note,omitempty"`
	Comments []Comment `json:"comments" toon:"comments"`
}

// PullRequest is the header of a pull request. It carries the issue fields (a PR
// *is* an issue) plus the diff stats that only the pulls endpoint reports.
type PullRequest struct {
	Title        string   `json:"title" toon:"title"`
	Author       string   `json:"author" toon:"author"`
	State        string   `json:"state" toon:"state"`
	Draft        bool     `json:"draft" toon:"draft"`
	Merged       bool     `json:"merged" toon:"merged"`
	Created      string   `json:"created" toon:"created"`
	Labels       []string `json:"labels,omitempty" toon:"labels,omitempty"`
	Comments     int      `json:"comments" toon:"comments"`
	Additions    int      `json:"additions" toon:"additions"`
	Deletions    int      `json:"deletions" toon:"deletions"`
	ChangedFiles int      `json:"changed_files" toon:"changed_files"`
	Body         string   `json:"body,omitempty" toon:"body,omitempty"`
}

// Review is one submitted PR review (APPROVED / CHANGES_REQUESTED / COMMENTED).
type Review struct {
	Login     string `json:"login" toon:"login"`
	State     string `json:"state" toon:"state"`
	Submitted string `json:"submitted" toon:"submitted"`
	Body      string `json:"body,omitempty" toon:"body,omitempty"`
}

// InlineComment is a review comment anchored to a line of the diff. ReplyTo
// keeps review threads reconstructable without nesting (same flat+parent shape
// the Reddit and Hacker News engines use).
type InlineComment struct {
	Path    string `json:"path" toon:"path"`
	Line    int    `json:"line" toon:"line"`
	ReplyTo int64  `json:"reply_to,omitempty" toon:"reply_to,omitempty"`
	Login   string `json:"login" toon:"login"`
	Created string `json:"created" toon:"created"`
	Body    string `json:"body" toon:"body"`
}

// File is one changed file. Patch is empty once the diff budget is spent — the
// filename and stats are still listed so the change set stays complete.
type File struct {
	Name      string `json:"name" toon:"name"`
	Status    string `json:"status" toon:"status"`
	Additions int    `json:"additions" toon:"additions"`
	Deletions int    `json:"deletions" toon:"deletions"`
	Patch     string `json:"patch,omitempty" toon:"patch,omitempty"`
}

// PullThread is everything the four PR endpoints contribute, in one document.
// Note carries truncation warnings, as on IssueThread.
type PullThread struct {
	PR             PullRequest     `json:"pr" toon:"pr"`
	Note           string          `json:"note,omitempty" toon:"note,omitempty"`
	Comments       []Comment       `json:"comments" toon:"comments"`
	Reviews        []Review        `json:"reviews" toon:"reviews"`
	InlineComments []InlineComment `json:"inline_comments" toon:"inline_comments"`
	Files          []File          `json:"files" toon:"files"`
}

// --- GitHub REST API wire shapes (api.github.com) ---

// apiUser is the nested user object; only the login is kept.
type apiUser struct {
	Login string `json:"login"`
}

type apiLabel struct {
	Name string `json:"name"`
}

// apiIssue is GET /repos/{o}/{r}/issues/{n}.
type apiIssue struct {
	Title     string     `json:"title"`
	State     string     `json:"state"`
	CreatedAt string     `json:"created_at"`
	Body      string     `json:"body"`
	Comments  int        `json:"comments"`
	User      apiUser    `json:"user"`
	Labels    []apiLabel `json:"labels"`
}

// apiPull is GET /repos/{o}/{r}/pulls/{n}.
type apiPull struct {
	Title        string     `json:"title"`
	State        string     `json:"state"`
	Draft        bool       `json:"draft"`
	Merged       bool       `json:"merged"`
	CreatedAt    string     `json:"created_at"`
	Body         string     `json:"body"`
	Comments     int        `json:"comments"`
	Additions    int        `json:"additions"`
	Deletions    int        `json:"deletions"`
	ChangedFiles int        `json:"changed_files"`
	User         apiUser    `json:"user"`
	Labels       []apiLabel `json:"labels"`
}

// apiComment is one entry of GET /issues/{n}/comments.
type apiComment struct {
	User      apiUser `json:"user"`
	CreatedAt string  `json:"created_at"`
	Body      string  `json:"body"`
}

// apiReviewComment is one entry of GET /pulls/{n}/comments.
type apiReviewComment struct {
	Path      string  `json:"path"`
	Line      int     `json:"line"`
	InReplyTo int64   `json:"in_reply_to_id"`
	User      apiUser `json:"user"`
	CreatedAt string  `json:"created_at"`
	Body      string  `json:"body"`
}

// apiReview is one entry of GET /pulls/{n}/reviews.
type apiReview struct {
	User        apiUser `json:"user"`
	State       string  `json:"state"`
	SubmittedAt string  `json:"submitted_at"`
	Body        string  `json:"body"`
}

// apiFile is one entry of GET /pulls/{n}/files.
type apiFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch"`
}

// apiRepo is GET /repos/{o}/{r}.
type apiRepo struct {
	FullName      string      `json:"full_name"`
	Description   string      `json:"description"`
	Topics        []string    `json:"topics"`
	Stars         int         `json:"stargazers_count"`
	Forks         int         `json:"forks_count"`
	OpenIssues    int         `json:"open_issues_count"` // issues + pull requests
	Language      string      `json:"language"`
	License       *apiLicense `json:"license"`
	Homepage      string      `json:"homepage"`
	DefaultBranch string      `json:"default_branch"`
	PushedAt      string      `json:"pushed_at"`
	Archived      bool        `json:"archived"`
	Parent        *struct {
		FullName string `json:"full_name"`
	} `json:"parent"` // set on forks
}

type apiLicense struct {
	SPDX string `json:"spdx_id"`
	Name string `json:"name"`
}

// apiContent is a contents-API file object, e.g. GET /repos/{o}/{r}/readme.
type apiContent struct {
	Path     string `json:"path"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// apiDirEntry is one entry of GET /repos/{o}/{r}/contents/{dir}.
type apiDirEntry struct {
	Name string `json:"name"`
	Type string `json:"type"` // file, dir, symlink, submodule
	Size int64  `json:"size"`
}

// apiRelease is GET /repos/{o}/{r}/releases/{latest,tags/{tag}} and one entry
// of GET /repos/{o}/{r}/releases.
type apiRelease struct {
	TagName     string     `json:"tag_name"`
	Name        string     `json:"name"`
	Body        string     `json:"body"`
	HTMLURL     string     `json:"html_url"`
	PublishedAt string     `json:"published_at"`
	Prerelease  bool       `json:"prerelease"`
	Draft       bool       `json:"draft"`
	Author      apiUser    `json:"author"`
	Assets      []apiAsset `json:"assets"`
}

type apiAsset struct {
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	DownloadCount int    `json:"download_count"`
}

// apiCommit is GET /repos/{o}/{r}/commits/{sha}.
type apiCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message   string        `json:"message"`
		Author    apiCommitUser `json:"author"`
		Committer apiCommitUser `json:"committer"`
	} `json:"commit"`
	Author *apiUser `json:"author"` // the linked GitHub account; null for unknown emails
	Stats  struct {
		Additions int `json:"additions"`
		Deletions int `json:"deletions"`
	} `json:"stats"`
	Files []apiFile `json:"files"`
}

type apiCommitUser struct {
	Name string `json:"name"`
	Date string `json:"date"`
}

// apiGist is GET /gists/{id}.
type apiGist struct {
	Description string                 `json:"description"`
	CreatedAt   string                 `json:"created_at"`
	UpdatedAt   string                 `json:"updated_at"`
	Owner       *apiUser               `json:"owner"`
	Files       map[string]apiGistFile `json:"files"`
}

type apiGistFile struct {
	Language  string `json:"language"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Content   string `json:"content"`
	RawURL    string `json:"raw_url"`
}

// --- GitHub GraphQL shapes (discussions) ---

// gqlActor is a GraphQL author; nil when the account was deleted.
type gqlActor struct {
	Login string `json:"login"`
}

func (a *gqlActor) name() string {
	if a == nil || a.Login == "" {
		return "ghost" // GitHub's own name for a deleted account
	}
	return a.Login
}

type gqlDiscussion struct {
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	CreatedAt   string    `json:"createdAt"`
	UpvoteCount int       `json:"upvoteCount"`
	Author      *gqlActor `json:"author"`
	Category    struct {
		Name string `json:"name"`
	} `json:"category"`
	Answer *struct {
		ID string `json:"id"`
	} `json:"answer"`
	Comments struct {
		TotalCount int                    `json:"totalCount"`
		Nodes      []gqlDiscussionComment `json:"nodes"`
	} `json:"comments"`
}

type gqlDiscussionComment struct {
	ID          string    `json:"id"`
	Body        string    `json:"body"`
	CreatedAt   string    `json:"createdAt"`
	UpvoteCount int       `json:"upvoteCount"`
	Author      *gqlActor `json:"author"`
	Replies     struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Body      string    `json:"body"`
			CreatedAt string    `json:"createdAt"`
			Author    *gqlActor `json:"author"`
		} `json:"nodes"`
	} `json:"replies"`
}
