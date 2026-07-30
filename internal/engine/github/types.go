// Package github implements the GitHub engine. It reads the public GitHub REST
// API (api.github.com) and renders an issue URL as issue + comments and a pull
// request URL as PR + conversation comments + reviews + inline review comments +
// changed files with patches (TOON) — mirroring the Hacker News engine's shape.
//
// Like the Hacker News engine (and unlike Reddit and the generic engine), this
// engine fetches its upstream DIRECTLY over HTTP rather than through crawl4ai:
// the REST API is a public JSON API with no bot wall, and a browser render of a
// GitHub issue is both slower and lossy (comments are lazily paginated in the
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

// IssueThread groups an issue with its comments.
type IssueThread struct {
	Issue    Issue     `json:"issue" toon:"issue"`
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
type PullThread struct {
	PR             PullRequest     `json:"pr" toon:"pr"`
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
