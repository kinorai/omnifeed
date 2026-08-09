package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/toon-format/toon-go"
)

const (
	defaultAPIBase = "https://api.github.com"
	defaultTimeout = 30 * time.Second // wall-clock budget per GitHub crawl
	maxComments    = 500              // cap emitted comments so a megathread can't blow the consumer's context
	maxDiffBytes   = 30000            // cumulative patch budget; files past it keep stats, lose the patch
	perPage        = 100              // GitHub's maximum page size
	userAgent      = "omnifeed"       // GitHub rejects requests without a User-Agent
)

// hostMatcher matches github.com and its subdomains.
var hostMatcher = httpx.HostMatcher("github.com")

// targetRE claims exactly the two page kinds this engine renders. Owner names are
// [A-Za-z0-9-]; repository names additionally allow "." and "_".
var targetRE = regexp.MustCompile(`^/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)/(issues|pull)/([0-9]+)$`)

// linkNextRE extracts the rel="next" URL from a Link response header.
var linkNextRE = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="next"`)

// Engine implements domain.Engine for GitHub issue and pull-request URLs via the
// REST API.
type Engine struct {
	client  *httpx.Client
	limiter *httpx.DomainLimiter
	apiBase string
	token   string
	timeout time.Duration
	logger  *slog.Logger
}

// Config configures a GitHub Engine.
type Config struct {
	Client  *httpx.Client
	Limiter *httpx.DomainLimiter
	APIBase string        // defaults to the public REST API; overridden in tests
	Token   string        // optional PAT; empty = anonymous (60 req/h/IP)
	Timeout time.Duration // wall-clock budget per crawl; defaults to defaultTimeout
	Logger  *slog.Logger
}

// New returns a GitHub Engine configured per cfg.
func New(cfg Config) *Engine {
	if cfg.APIBase == "" {
		cfg.APIBase = defaultAPIBase
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Engine{
		client:  cfg.Client.WithUpstream("github", "api"),
		limiter: cfg.Limiter,
		apiBase: strings.TrimRight(cfg.APIBase, "/"),
		token:   cfg.Token,
		timeout: cfg.Timeout,
		logger:  cfg.Logger,
	}
}

// Name returns the engine identifier ("github").
func (*Engine) Name() string { return "github" }

// Matches claims only the github.com URLs this engine renders: issue and pull
// request pages. Everything else on the host (blob/tree/actions/releases/
// discussions, repo roots, …) falls through to the generic fallback, which
// renders the page through crawl4ai.
func (*Engine) Matches(rawURL string) bool {
	_, ok := parseTarget(rawURL)
	return ok
}

// target is the resolved fetch plan for a GitHub URL.
type target struct {
	owner  string
	repo   string
	number string
	pull   bool // /pull/{n} rather than /issues/{n}
}

// parseTarget classifies a GitHub URL into a fetch plan. ok is false for any
// github.com URL this engine doesn't render (so it falls through).
func parseTarget(rawURL string) (target, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || !hostMatcher.MatchString(u.Hostname()) {
		return target{}, false
	}
	// url.Parse already split off the query and fragment; normalize a trailing
	// slash so /issues/12/ matches like /issues/12.
	p := strings.TrimSuffix(u.Path, "/")
	m := targetRE.FindStringSubmatch(p)
	if m == nil {
		return target{}, false
	}
	return target{owner: m[1], repo: m[2], number: m[4], pull: m[3] == "pull"}, true
}

// Crawl fetches the issue or pull request behind rawURL from the GitHub REST API
// and returns it encoded as TOON.
func (e *Engine) Crawl(ctx context.Context, rawURL string, _ domain.EngineOptions) (domain.Document, error) {
	t, ok := parseTarget(rawURL)
	if !ok {
		return domain.Document{}, fmt.Errorf("unsupported github url: %s", rawURL)
	}

	// Bound wall-clock independently of the shared HTTP client timeout (which is
	// the crawl4ai knob); this engine talks to api.github.com directly.
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	if e.limiter != nil {
		release, lerr := e.limiter.Acquire(ctx, e.Name(), e.apiBase)
		if lerr != nil {
			return domain.Document{}, lerr
		}
		defer release()
	}

	if t.pull {
		return e.crawlPull(ctx, rawURL, t)
	}
	return e.crawlIssue(ctx, rawURL, t)
}

// crawlIssue fetches the issue plus its (paginated) conversation comments.
func (e *Engine) crawlIssue(ctx context.Context, rawURL string, t target) (domain.Document, error) {
	raw, _, err := e.get(ctx, e.repoURL(t, "issues/"+t.number))
	if err != nil {
		return domain.Document{}, fmt.Errorf("fetch issue: %w", err)
	}
	var ai apiIssue
	if jerr := json.Unmarshal(raw, &ai); jerr != nil {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse issue: %w", jerr)}
	}

	comments, collected, more, err := e.comments(ctx, t)
	if err != nil {
		return domain.Document{}, err
	}

	meta := map[string]string{"comments": strconv.Itoa(len(comments))}
	th := IssueThread{
		Issue: Issue{
			Title:    ai.Title,
			Author:   ai.User.Login,
			State:    ai.State,
			Created:  ai.CreatedAt,
			Labels:   labelNames(ai.Labels),
			Comments: ai.Comments,
			Body:     ai.Body,
		},
		Comments: comments,
	}
	if from := truncatedFrom(len(comments), collected, ai.Comments, more); from > 0 {
		meta["truncated_from"] = strconv.Itoa(from)
		th.Note = fmt.Sprintf("comment list truncated: showing %d of %d comments", len(comments), from)
	}
	return e.document(th, rawURL, meta)
}

// crawlPull fetches the PR plus its conversation comments, reviews, inline review
// comments, and changed files. A PR is also an issue, so the conversation-tab
// comments come from the ISSUES endpoint — /pulls/{n}/comments returns the inline
// review comments instead.
func (e *Engine) crawlPull(ctx context.Context, rawURL string, t target) (domain.Document, error) {
	// The five REST reads are independent, so they run concurrently: a PR
	// crawl costs one round-trip (plus comment pagination), not five in a row.
	var (
		ap                              apiPull
		comments                        []Comment
		collected                       int
		more                            bool
		inline                          []InlineComment
		reviews                         []Review
		af                              []apiFile
		inlineHdr, reviewsHdr, filesHdr http.Header
	)
	err := runAll(
		func() error {
			raw, _, err := e.get(ctx, e.repoURL(t, "pulls/"+t.number))
			if err != nil {
				return fmt.Errorf("fetch pull request: %w", err)
			}
			if jerr := json.Unmarshal(raw, &ap); jerr != nil {
				return &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse pull request: %w", jerr)}
			}
			return nil
		},
		func() (err error) {
			comments, collected, more, err = e.comments(ctx, t)
			return err
		},
		func() error {
			raw, hdr, err := e.get(ctx, e.repoURL(t, "pulls/"+t.number+"/comments")+"?per_page="+strconv.Itoa(perPage))
			if err != nil {
				return fmt.Errorf("fetch review comments: %w", err)
			}
			var arc []apiReviewComment
			if jerr := json.Unmarshal(raw, &arc); jerr != nil {
				return &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse review comments: %w", jerr)}
			}
			inlineHdr = hdr
			inline = make([]InlineComment, 0, len(arc))
			for _, c := range arc {
				inline = append(inline, InlineComment{
					Path: c.Path, Line: c.Line, ReplyTo: c.InReplyTo,
					Login: c.User.Login, Created: c.CreatedAt, Body: c.Body,
				})
			}
			return nil
		},
		func() error {
			raw, hdr, err := e.get(ctx, e.repoURL(t, "pulls/"+t.number+"/reviews")+"?per_page="+strconv.Itoa(perPage))
			if err != nil {
				return fmt.Errorf("fetch reviews: %w", err)
			}
			var ar []apiReview
			if jerr := json.Unmarshal(raw, &ar); jerr != nil {
				return &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse reviews: %w", jerr)}
			}
			reviewsHdr = hdr
			reviews = make([]Review, 0, len(ar))
			for _, r := range ar {
				reviews = append(reviews, Review{Login: r.User.Login, State: r.State, Submitted: r.SubmittedAt, Body: r.Body})
			}
			return nil
		},
		func() error {
			raw, hdr, err := e.get(ctx, e.repoURL(t, "pulls/"+t.number+"/files")+"?per_page="+strconv.Itoa(perPage))
			if err != nil {
				return fmt.Errorf("fetch files: %w", err)
			}
			if jerr := json.Unmarshal(raw, &af); jerr != nil {
				return &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse files: %w", jerr)}
			}
			filesHdr = hdr
			return nil
		},
	)
	if err != nil {
		return domain.Document{}, err
	}
	files, diffTruncated := budgetFiles(af)

	meta := map[string]string{"comments": strconv.Itoa(len(comments))}
	var notes []string
	if from := truncatedFrom(len(comments), collected, ap.Comments, more); from > 0 {
		meta["truncated_from"] = strconv.Itoa(from)
		notes = append(notes, fmt.Sprintf("comment list truncated: showing %d of %d comments", len(comments), from))
	}
	// The inline-comment, review, and file lists are deliberately first-page-only
	// (a second page means a 100+-entry list — pathological for LLM consumption),
	// but the reader must be able to tell "complete" from "cut at 100": a further
	// page is flagged in metadata AND in the document itself (metadata lands in
	// MCP _meta, which models never see).
	for _, l := range []struct {
		name string
		hdr  http.Header
	}{{"inline_comments", inlineHdr}, {"reviews", reviewsHdr}, {"files", filesHdr}} {
		if nextLink(l.hdr) != "" {
			meta[l.name+"_truncated"] = "true"
			notes = append(notes, l.name+" list truncated at "+strconv.Itoa(perPage))
		}
	}
	if diffTruncated {
		meta["diff_truncated"] = "true"
		notes = append(notes, "some file patches omitted (diff budget spent); names and stats are complete")
	}
	return e.document(PullThread{
		Note: strings.Join(notes, "; "),
		PR: PullRequest{
			Title:        ap.Title,
			Author:       ap.User.Login,
			State:        ap.State,
			Draft:        ap.Draft,
			Merged:       ap.Merged,
			Created:      ap.CreatedAt,
			Labels:       labelNames(ap.Labels),
			Comments:     ap.Comments,
			Additions:    ap.Additions,
			Deletions:    ap.Deletions,
			ChangedFiles: ap.ChangedFiles,
			Body:         ap.Body,
		},
		Comments:       comments,
		Reviews:        reviews,
		InlineComments: inline,
		Files:          files,
	}, rawURL, meta)
}

// runAll runs the given functions concurrently and waits for all of them; the
// first (by argument order) non-nil error is returned, so failure reporting is
// deterministic. No early cancellation: the losers are bounded by the crawl's
// context timeout anyway, and cancel plumbing isn't worth it for five calls.
func runAll(fns ...func() error) error {
	errs := make([]error, len(fns))
	var wg sync.WaitGroup
	for i, fn := range fns {
		wg.Add(1)
		go func(i int, fn func() error) {
			defer wg.Done()
			errs[i] = fn()
		}(i, fn)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// comments fetches the conversation-tab comments of an issue or PR, following the
// Link rel="next" header until GitHub runs out of pages or maxComments is
// reached. collected is how many comments were seen before the cap was applied
// and more reports that unfetched pages remain — together they tell the caller
// whether (and from what total) the list was truncated.
func (e *Engine) comments(ctx context.Context, t target) (out []Comment, collected int, more bool, err error) {
	next := e.repoURL(t, "issues/"+t.number+"/comments") + "?per_page=" + strconv.Itoa(perPage)
	// maxComments/perPage well-formed pages reach the cap; the margin tolerates
	// short pages while still bounding a misbehaving upstream.
	for pages := 0; next != "" && pages < 2*(maxComments/perPage); pages++ {
		raw, hdr, gerr := e.get(ctx, next)
		if gerr != nil {
			return nil, 0, false, fmt.Errorf("fetch comments: %w", gerr)
		}
		var page []apiComment
		if jerr := json.Unmarshal(raw, &page); jerr != nil {
			return nil, 0, false, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse comments: %w", jerr)}
		}
		for _, c := range page {
			out = append(out, Comment{Login: c.User.Login, Created: c.CreatedAt, Body: c.Body})
		}
		next = nextLink(hdr)
		// Follow only URLs under our own API base: get() attaches the bearer
		// token to every request, and the Link header is upstream-controlled
		// input — never send the token wherever a response points.
		if next != "" && !strings.HasPrefix(next, e.apiBase+"/") {
			next = ""
		}
		if len(out) >= maxComments {
			// Stop paginating: anything past the cap is dropped anyway.
			return out[:maxComments], len(out), next != "", nil
		}
	}
	return out, len(out), false, nil
}

// truncatedFrom returns the total to report in the truncated_from metadata key,
// or 0 when nothing was dropped. apiTotal is the comment count GitHub reports on
// the issue/PR itself — authoritative when pagination stopped early. (apiTotal
// alone can't be the signal: GitHub's count and the paginated list disagree on
// e.g. deleted comments, and that mismatch is not a truncation.)
func truncatedFrom(kept, collected, apiTotal int, more bool) int {
	if !more && collected <= kept {
		return 0
	}
	return max(apiTotal, collected)
}

// budgetFiles converts the changed-file list, dropping the patch text of any
// file that no longer fits the cumulative patch budget. Later smaller patches
// still use whatever budget remains — one oversized file must not strip every
// file after it. Filenames and stats are always kept.
func budgetFiles(af []apiFile) ([]File, bool) {
	files := make([]File, 0, len(af))
	used, truncated := 0, false
	for _, f := range af {
		out := File{Name: f.Filename, Status: f.Status, Additions: f.Additions, Deletions: f.Deletions}
		if used+len(f.Patch) > maxDiffBytes {
			truncated = true
		} else {
			out.Patch = f.Patch
			used += len(f.Patch)
		}
		files = append(files, out)
	}
	return files, truncated
}

// repoURL builds an API URL under /repos/{owner}/{repo}/.
func (e *Engine) repoURL(t target, suffix string) string {
	return e.apiBase + "/repos/" + t.owner + "/" + t.repo + "/" + suffix
}

// get fetches a GitHub API URL and returns the raw JSON body and the response
// headers (the caller needs Link for pagination).
func (e *Engine) get(ctx context.Context, apiURL string) ([]byte, http.Header, error) {
	headers := map[string]string{
		"Accept":     "application/vnd.github+json",
		"User-Agent": userAgent,
	}
	if e.token != "" {
		headers["Authorization"] = "Bearer " + e.token
	}
	resp, err := e.client.DoRetry(ctx, http.MethodGet, apiURL, nil, headers, httpx.RetryConfig{})
	if err != nil {
		return nil, nil, httpx.ClassifyClientError(err, domain.KindUpstreamError)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20MB cap
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, &domain.FetchError{
			Kind:       domain.KindForStatus(resp.StatusCode),
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("github api returned %d", resp.StatusCode),
		}
	}
	return body, resp.Header, nil
}

// nextLink returns the rel="next" URL of a Link response header, or "".
func nextLink(h http.Header) string {
	m := linkNextRE.FindStringSubmatch(h.Get("Link"))
	if m == nil {
		return ""
	}
	return m[1]
}

// labelNames flattens the label objects to their names.
func labelNames(labels []apiLabel) []string {
	if len(labels) == 0 {
		return nil
	}
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.Name)
	}
	return out
}

// document encodes v as TOON and wraps it in a Document with standard metadata.
func (e *Engine) document(v any, source string, extra map[string]string) (domain.Document, error) {
	encoded, err := toon.Marshal(v, toon.WithLengthMarkers(true))
	if err != nil {
		return domain.Document{}, fmt.Errorf("encode: %w", err)
	}
	meta := map[string]string{
		"source":              source,
		"engine":              "github",
		"status_code":         "200",
		domain.ContentTypeKey: domain.ContentTypeTOON,
	}
	for k, val := range extra {
		meta[k] = val
	}
	e.logger.Info("github crawl complete", "source", source, "bytes", len(encoded))
	return domain.Document{PageContent: string(encoded), Metadata: meta}, nil
}
