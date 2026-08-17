// Package bluesky implements domain.Searcher against the AT Protocol
// app.bsky.feed.searchPosts endpoint. It is the vertical the router picks for a
// bsky.app site filter.
//
// HOST: this searcher calls api.bsky.app, NOT the cached public.api.bsky.app
// the Bluesky fetch ENGINE uses. searchPosts answers keyless with HTTP 200 on
// api.bsky.app and with HTTP 403 on public.api.bsky.app (verified live
// 2026-08-17). Bluesky asks public-web use to stay on the cached host, so
// thread and profile fetching deliberately stays there — only search moves.
package bluesky

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

const (
	defaultBaseURL = "https://api.bsky.app"
	// profileURL is the bsky.app permalink prefix a post's AT-URI maps back to.
	profileURL = "https://bsky.app/profile/"
	// maxResponseBytes caps how much of the response is read.
	maxResponseBytes = 10 << 20
	// defaultLimit / maxLimit bound the `limit` param.
	defaultLimit = 10
	maxLimit     = 25
	// titleChars / snippetChars cap the derived title and snippet. A post has
	// no title of its own, so its first line stands in for one.
	titleChars   = 100
	snippetChars = 500
)

// Config configures the Searcher.
type Config struct {
	Client *httpx.Client
	// Limiter, when non-nil, paces queries to the AppView. It is keyed by
	// hostname internally, so it paces api.bsky.app independently of the fetch
	// engine's public.api.bsky.app.
	Limiter *httpx.DomainLimiter
	BaseURL string // defaults to the keyless search host; overridden in tests
	Logger  *slog.Logger
}

// Searcher queries searchPosts and reshapes posts into the canonical
// domain.SearchResult.
type Searcher struct {
	baseURL string
	client  *httpx.Client
	limiter *httpx.DomainLimiter
	logger  *slog.Logger
}

// New returns a Searcher wired with the given config.
func New(cfg Config) *Searcher {
	if cfg.BaseURL == "" {
		cfg.BaseURL = defaultBaseURL
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Searcher{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		client:  cfg.Client.WithUpstream("bluesky", "search"),
		limiter: cfg.Limiter,
		logger:  cfg.Logger,
	}
}

// Name returns the searcher identifier ("bluesky").
func (*Searcher) Name() string { return "bluesky" }

// --- AppView wire types ---

type searchResponse struct {
	Posts []post `json:"posts"`
}

type post struct {
	URI    string `json:"uri"` // at://<did>/app.bsky.feed.post/<rkey>
	Author struct {
		Handle      string `json:"handle"`
		DisplayName string `json:"displayName"`
	} `json:"author"`
	Record struct {
		Text      string `json:"text"`
		CreatedAt string `json:"createdAt"`
	} `json:"record"`
}

// Search runs the query against searchPosts.
func (s *Searcher) Search(ctx context.Context, query string, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is empty")
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("limit", strconv.Itoa(clampLimit(opts.Limit)))
	if opts.Language != "" {
		params.Set("lang", opts.Language)
	}
	if cutoff, ok := timeCutoff(opts.TimeRange, time.Now()); ok {
		params.Set("since", cutoff.UTC().Format(time.RFC3339))
	}

	searchURL := s.baseURL + "/xrpc/app.bsky.feed.searchPosts?" + params.Encode()
	if s.limiter != nil {
		release, lerr := s.limiter.Acquire(ctx, s.Name(), searchURL)
		if lerr != nil {
			return nil, httpx.ClassifyClientError(lerr, domain.KindUpstreamError)
		}
		defer release()
	}

	resp, err := s.client.DoRetry(ctx, http.MethodGet, searchURL, nil,
		map[string]string{"Accept": "application/json"}, httpx.RetryConfig{})
	if err != nil {
		return nil, httpx.ClassifyClientError(err, domain.KindUpstreamError)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &domain.FetchError{
			Kind:       domain.KindForStatus(resp.StatusCode),
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("bluesky appview returned %d", resp.StatusCode),
		}
	}

	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("decode response: %w", err)}
	}

	results := make([]domain.SearchResult, 0, len(sr.Posts))
	for _, p := range sr.Posts {
		results = append(results, domain.SearchResult{
			Title:         title(p),
			URL:           postURL(p),
			Snippet:       truncate(p.Record.Text, snippetChars) + " — @" + p.Author.Handle,
			Engine:        "bluesky",
			PublishedDate: p.Record.CreatedAt,
		})
	}
	return results, nil
}

// postURL rebuilds the bsky.app permalink from the post's AT-URI, whose last
// path segment is the record key.
func postURL(p post) string {
	rkey := p.URI
	if i := strings.LastIndex(rkey, "/"); i >= 0 {
		rkey = rkey[i+1:]
	}
	return profileURL + p.Author.Handle + "/post/" + rkey
}

// title stands in for the title a post does not have: its first line, cut
// short. An image-only post has no text at all, so the author answers for it.
func title(p post) string {
	line := p.Record.Text
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if line = strings.TrimSpace(line); line == "" {
		return "@" + p.Author.Handle
	}
	return truncate(line, titleChars)
}

// clampLimit bounds the caller's Limit to what this searcher asks for.
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

// timeCutoff maps a SearchOptions.TimeRange to the instant a post must be newer
// than. ok is false when no range was asked for (or it is unknown).
func timeCutoff(timeRange string, now time.Time) (time.Time, bool) {
	var d time.Duration
	switch timeRange {
	case "day":
		d = 24 * time.Hour
	case "week":
		d = 7 * 24 * time.Hour
	case "month":
		d = 30 * 24 * time.Hour
	case "year":
		d = 365 * 24 * time.Hour
	default:
		return time.Time{}, false
	}
	return now.Add(-d), true
}

// truncate cuts s to at most n bytes without splitting a rune, appending an
// ellipsis when anything was dropped.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimSpace(s[:n]) + "..."
}
