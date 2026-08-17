// Package hackernews implements domain.Searcher against the Algolia Hacker
// News search API (hn.algolia.com). It is the vertical the router picks for a
// news.ycombinator.com site filter.
//
// The reason to prefer it over a scraped web search of the same site: every hit
// carries HN's own ranking signals — points and comment count — which no web
// engine exposes. An agent picking which thread to read gets the discussion
// with 300 comments instead of the one that happened to rank on keywords.
package hackernews

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

const (
	defaultBaseURL = "https://hn.algolia.com/api/v1"
	// itemURL is the HN thread page for an item id; also the result URL for
	// Ask/Show self-posts, which carry no external url.
	itemURL = "https://news.ycombinator.com/item?id="
	// maxResponseBytes caps how much of the Algolia response is read; a search
	// page is a few hundred KB at most.
	maxResponseBytes = 10 << 20
	// defaultHits / maxHits bound hitsPerPage. Algolia allows far more, but a
	// search result set is read by an agent, not paged through.
	defaultHits = 10
	maxHits     = 30
	// snippetChars caps the story-text excerpt appended to the snippet.
	snippetChars = 200
)

// tagStripper removes the HTML tags Algolia leaves in story_text (<p>, <a>, …).
var tagStripper = regexp.MustCompile(`<[^>]*>`)

// Config configures the Searcher.
type Config struct {
	Client *httpx.Client
	// Limiter, when non-nil, paces queries to Algolia. It is keyed by hostname
	// internally, so passing the process-wide limiter makes searches share the
	// hn.algolia.com pacing slot with the Hacker News FETCH engine — the two
	// talk to the same host and must not be polite in isolation.
	Limiter *httpx.DomainLimiter
	BaseURL string // defaults to the public Algolia API; overridden in tests
	Logger  *slog.Logger
}

// Searcher queries the Algolia Hacker News index and reshapes stories into the
// canonical domain.SearchResult.
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
		client:  cfg.Client.WithUpstream("hackernews", "search"),
		limiter: cfg.Limiter,
		logger:  cfg.Logger,
	}
}

// Name returns the searcher identifier ("hackernews").
func (*Searcher) Name() string { return "hackernews" }

// --- Algolia wire types ---

type searchResponse struct {
	Hits []searchHit `json:"hits"`
}

type searchHit struct {
	Title       string `json:"title"`
	URL         string `json:"url"` // null for Ask/Show self-posts
	Points      int    `json:"points"`
	NumComments int    `json:"num_comments"`
	ObjectID    string `json:"objectID"`
	CreatedAt   string `json:"created_at"`
	StoryText   string `json:"story_text"` // HTML-entity-encoded; absent on link posts
}

// Search runs the query against the Algolia story index.
func (s *Searcher) Search(ctx context.Context, query string, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is empty")
	}

	params := url.Values{}
	params.Set("query", query)
	params.Set("tags", "story")
	params.Set("hitsPerPage", strconv.Itoa(clampHits(opts.Limit)))
	// MUST stay. Algolia hard-ANDs every term by default, so a natural-language
	// query returns nothing at all: measured live 2026-08-17, "kubernetes
	// longhorn disk pressure" gave 0 hits without this parameter and 3076 with
	// it. Removing it silently turns this searcher into a zero-result path.
	params.Set("removeWordsIfNoResults", "allOptional")
	if cutoff, ok := timeCutoff(opts.TimeRange, time.Now()); ok {
		// Built through url.Values so `>` is percent-encoded. A hand-built query
		// string with a raw `>` intermittently 400s at Algolia's frontend
		// (observed live 2026-07 and 2026-08).
		params.Set("numericFilters", "created_at_i>"+strconv.FormatInt(cutoff, 10))
	}

	searchURL := s.baseURL + "/search?" + params.Encode()
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
			Err:        fmt.Errorf("hn algolia returned %d", resp.StatusCode),
		}
	}

	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("decode response: %w", err)}
	}

	results := make([]domain.SearchResult, 0, len(sr.Hits))
	for _, hit := range sr.Hits {
		results = append(results, domain.SearchResult{
			Title:         hit.Title,
			URL:           resultURL(hit),
			Snippet:       snippet(hit),
			Engine:        "hackernews",
			PublishedDate: hit.CreatedAt,
		})
	}
	return results, nil
}

// resultURL is the story's own link, or its HN thread page when it has none
// (Ask HN / Show HN self-posts, where the discussion IS the story).
func resultURL(hit searchHit) string {
	if hit.URL != "" {
		return hit.URL
	}
	return itemURL + hit.ObjectID
}

// snippet leads with the ranking signals a web engine cannot give — points and
// comment count — then adds an excerpt of the self-post text when there is one.
func snippet(hit searchHit) string {
	s := fmt.Sprintf("%d points, %d comments", hit.Points, hit.NumComments)
	if text := excerpt(hit.StoryText); text != "" {
		s += " — " + text
	}
	return s
}

// excerpt turns Algolia's HTML-entity-encoded story_text into a short plain
// line: tags out, entities decoded, whitespace collapsed, truncated.
func excerpt(storyText string) string {
	if storyText == "" {
		return ""
	}
	text := html.UnescapeString(tagStripper.ReplaceAllString(storyText, " "))
	text = strings.Join(strings.Fields(text), " ")
	if len(text) <= snippetChars {
		return text
	}
	n := snippetChars
	for n > 0 && !utf8.RuneStart(text[n]) { // back up so we don't split a multibyte char
		n--
	}
	return strings.TrimSpace(text[:n]) + "..."
}

// clampHits bounds the caller's Limit to what this searcher asks Algolia for.
func clampHits(limit int) int {
	switch {
	case limit <= 0:
		return defaultHits
	case limit > maxHits:
		return maxHits
	default:
		return limit
	}
}

// timeCutoff maps a SearchOptions.TimeRange to the Unix instant a story must be
// newer than. ok is false when no range was asked for (or it is unknown).
func timeCutoff(timeRange string, now time.Time) (int64, bool) {
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
		return 0, false
	}
	return now.Add(-d).Unix(), true
}
