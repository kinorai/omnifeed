// Package reddit implements domain.Searcher against Reddit's own in-site
// search, fetched through the browser port because Reddit 403-blocks
// non-browser HTTP clients (the same wall the Reddit engine clears — see
// internal/engine/reddit).
//
// A query that names an `r/<sub>` is scoped to that community; any other query
// runs against Reddit's sitewide search. Measured 2026-08-17: subreddit-scoped
// in-site search beat a `site:reddit.com` web search on every test query, while
// sitewide in-site search ranked worse than that web search — Reddit's
// relevance ranking is good inside a community and weak across all of them. The
// sitewide mode is served anyway, so `site=reddit.com` always reaches Reddit's
// own index and returns its ranking signals (score, comments, community).
package reddit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/browser"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

const (
	// redditOrigin is the host both the search and the fetch engine use; www
	// stays clear of Reddit's edge wall where old.reddit gets blocked.
	redditOrigin = "https://www.reddit.com"
	// defaultLimit / maxLimit bound the `limit` query param.
	defaultLimit = 10
	maxLimit     = 25
	// defaultTimeWindow is the `t` used when the caller asks for no time range.
	// Measured 2026-08-17: relevance-sorted results restricted to the last year
	// beat the unfiltered ranking, which surfaces decade-old threads.
	defaultTimeWindow = "year"
	// snippetChars caps the selftext excerpt appended to the snippet.
	snippetChars = 200
)

// subredditPattern matches the `r/<name>` token that scopes the search.
// Reddit names are 2–21 characters of letters, digits and underscores.
var subredditPattern = regexp.MustCompile(`\br/([A-Za-z0-9_]{2,21})\b`)

// Config configures the Searcher.
type Config struct {
	// Browser fetches the search JSON from inside a real reddit.com page.
	Browser browser.Browser
	// Limiter, when non-nil, paces queries to Reddit. It is keyed by hostname
	// internally, so passing the process-wide limiter makes searches share the
	// www.reddit.com pacing slot with the Reddit fetch engine.
	Limiter *httpx.DomainLimiter
	Logger  *slog.Logger
}

// Searcher queries a subreddit's own search and reshapes posts into the
// canonical domain.SearchResult.
type Searcher struct {
	browser browser.Browser
	limiter *httpx.DomainLimiter
	logger  *slog.Logger
}

// New returns a Searcher wired with the given config.
func New(cfg Config) *Searcher {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Searcher{
		browser: cfg.Browser,
		limiter: cfg.Limiter,
		logger:  cfg.Logger,
	}
}

// Name returns the searcher identifier ("reddit").
func (*Searcher) Name() string { return "reddit" }

// --- Reddit wire types ---

type listing struct {
	Data struct {
		Children []struct {
			Data listingPost `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type listingPost struct {
	Title       string  `json:"title"`
	Permalink   string  `json:"permalink"`
	Subreddit   string  `json:"subreddit_name_prefixed"`
	Score       int     `json:"score"`
	NumComments int     `json:"num_comments"`
	CreatedUTC  float64 `json:"created_utc"`
	Selftext    string  `json:"selftext"`
}

// Search runs the query against the subreddit named in the query text, or
// against Reddit's sitewide search when the query names none.
func (s *Searcher) Search(ctx context.Context, query string, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	sub, terms := splitSubreddit(query)
	if strings.TrimSpace(terms) == "" {
		return nil, fmt.Errorf("query is empty apart from the subreddit")
	}

	params := url.Values{}
	params.Set("q", terms)
	params.Set("sort", "relevance")
	params.Set("t", timeWindow(opts.TimeRange))
	params.Set("limit", strconv.Itoa(clampLimit(opts.Limit)))

	page := redditOrigin + "/"
	if sub != "" {
		// restrict_sr only means anything under /r/<sub>/, and Reddit ignores
		// it sitewide, so the two travel together.
		params.Set("restrict_sr", "1")
		page = redditOrigin + "/r/" + sub + "/"
	}
	searchURL := page + "search.json?" + params.Encode()

	if s.limiter != nil {
		release, lerr := s.limiter.Acquire(ctx, s.Name(), searchURL)
		if lerr != nil {
			return nil, httpx.ClassifyClientError(lerr, domain.KindUpstreamError)
		}
		defer release()
	}

	body, err := s.fetch(ctx, page, searchURL)
	if err != nil {
		return nil, err
	}

	var l listing
	if err := json.Unmarshal(body, &l); err != nil {
		return nil, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("decode listing: %w", err)}
	}

	results := make([]domain.SearchResult, 0, len(l.Data.Children))
	for _, child := range l.Data.Children {
		results = append(results, mapPost(child.Data))
	}
	return results, nil
}

// fetch opens a browser session on the subreddit page and runs a same-origin
// fetch of the search JSON from inside it — the browser context clears Reddit's
// bot wall and the in-page fetch inherits it.
func (s *Searcher) fetch(ctx context.Context, page, searchURL string) ([]byte, error) {
	session, err := s.browser.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open %s browser: %w", s.browser.Name(), err)
	}
	defer func() {
		if cerr := session.Close(ctx); cerr != nil {
			s.logger.Warn("closing reddit search session", "err", cerr)
		}
	}()

	if err := session.Navigate(ctx, page); err != nil {
		return nil, err
	}
	out, err := session.Eval(ctx, getJS(searchURL))
	if err != nil {
		return nil, err
	}
	return unwrapEnvelope(out)
}

// --- in-page fetch ---
//
// The snippet and its {s,b} envelope mirror internal/engine/reddit's fetcher.
// They are kept local rather than shared: the searcher is a discovery adapter
// and must not depend on an engine (the dependency rule points inward, and
// adapters never import each other).

// getJS returns an async snippet that GETs u and returns {s:status, b:body}.
// The URL is embedded as a JSON string literal, which is also a valid JS string
// literal — that is the injection guard for anything smuggled through a query.
func getJS(u string) string {
	lit, _ := json.Marshal(u)
	return `const r = await fetch(` + string(lit) + `, {headers: {"Accept": "application/json"}}); ` +
		`return JSON.stringify({s: r.status, b: await r.text()});`
}

// fetchEnvelope is what the in-page snippet returns: the HTTP status of the
// Reddit fetch and its raw body, so a Reddit-side block is distinguishable from
// a browser/navigation failure.
type fetchEnvelope struct {
	S int    `json:"s"`
	B string `json:"b"`
}

// unwrapEnvelope decodes the envelope and yields the Reddit body.
func unwrapEnvelope(envStr string) ([]byte, error) {
	var env fetchEnvelope
	if err := json.Unmarshal([]byte(envStr), &env); err != nil {
		return nil, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("decode fetch envelope: %w", err)}
	}
	if env.S != http.StatusOK {
		return nil, &domain.FetchError{
			Kind:       domain.KindForStatus(env.S),
			StatusCode: env.S,
			Err:        fmt.Errorf("reddit search returned %d via browser", env.S),
		}
	}
	if !json.Valid([]byte(env.B)) {
		return nil, &domain.FetchError{Kind: domain.KindBotBlock, Err: fmt.Errorf("reddit search response is not JSON (likely bot-blocked)")}
	}
	return []byte(env.B), nil
}

// mapPost reshapes one listing post. The snippet leads with the ranking signals
// a web engine cannot give — score, comment count and the community.
func mapPost(p listingPost) domain.SearchResult {
	snippet := fmt.Sprintf("%d points, %d comments in %s", p.Score, p.NumComments, p.Subreddit)
	if text := excerpt(p.Selftext); text != "" {
		snippet += " — " + text
	}
	r := domain.SearchResult{
		Title:   p.Title,
		URL:     redditOrigin + p.Permalink,
		Snippet: snippet,
		Engine:  "reddit",
	}
	if p.CreatedUTC > 0 {
		r.PublishedDate = time.Unix(int64(p.CreatedUTC), 0).UTC().Format(time.RFC3339)
	}
	return r
}

// splitSubreddit pulls the first `r/<name>` token out of the query and returns
// it with the remaining query text; an empty sub means the query named none and
// the search runs sitewide. The token is removed because Reddit's in-site
// search would otherwise match it as a literal term.
func splitSubreddit(query string) (sub, terms string) {
	m := subredditPattern.FindStringSubmatchIndex(query)
	if m == nil {
		return "", query
	}
	sub = query[m[2]:m[3]]
	terms = strings.Join(strings.Fields(query[:m[0]]+" "+query[m[1]:]), " ")
	return sub, terms
}

// timeWindow maps a SearchOptions.TimeRange onto Reddit's `t` parameter, which
// uses the same vocabulary. No range asked for means defaultTimeWindow.
func timeWindow(timeRange string) string {
	switch timeRange {
	case "day", "week", "month", "year":
		return timeRange
	default:
		return defaultTimeWindow
	}
}

// clampLimit bounds the caller's Limit to what this searcher asks Reddit for.
func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultLimit
	case limit > maxLimit:
		return maxLimit
	default:
		return limit
	}
}

// excerpt renders a short single-line preview of a self-post's body.
func excerpt(selftext string) string {
	text := strings.Join(strings.Fields(selftext), " ")
	if len(text) <= snippetChars {
		return text
	}
	n := snippetChars
	for n > 0 && !utf8.RuneStart(text[n]) { // back up so we don't split a multibyte char
		n--
	}
	return strings.TrimSpace(text[:n]) + "..."
}
