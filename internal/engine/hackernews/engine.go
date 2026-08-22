package hackernews

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/toon-format/toon-go"
)

const (
	defaultAPIBase = "https://hn.algolia.com/api/v1"
	frontPageSize  = 30
	defaultTimeout = 30 * time.Second // wall-clock budget per HN crawl
)

// MaxThreadComments is the ceiling on comments emitted for one thread, so a
// megathread can't blow the consumer's context. A caller's max_comments can only
// lower it. Exported because the fetch_url tool schema states the number.
const MaxThreadComments = 500

// hostMatcher matches news.ycombinator.com and its subdomains.
var hostMatcher = httpx.HostMatcher("news.ycombinator.com")

// pathFeed maps a front-page-style path to a feed label.
var pathFeed = map[string]string{
	"/": "front_page", "/news": "front_page", "/front": "front_page",
	"/newest": "newest", "/ask": "ask", "/show": "show",
}

// feedQuery maps a feed label to the Algolia endpoint + tag that serves it.
// /ask and /show use search_by_date: plain /search returns all-time top by points,
// not the current listing those pages represent. front_page keeps /search (the
// front_page tag is curated; Story carries no positional rank — see types.go).
var feedQuery = map[string]struct{ endpoint, tag string }{
	"front_page": {"search", "front_page"},
	"newest":     {"search_by_date", "story"},
	"ask":        {"search_by_date", "ask_hn"},
	"show":       {"search_by_date", "show_hn"},
}

// Engine implements domain.Engine for Hacker News URLs via the Algolia API.
type Engine struct {
	client  *httpx.Client
	limiter httpx.Limiter
	apiBase string
	timeout time.Duration
	logger  *slog.Logger
}

// Config configures a Hacker News Engine.
type Config struct {
	Client  *httpx.Client
	Limiter httpx.Limiter
	APIBase string        // defaults to the public Algolia API; overridden in tests
	Timeout time.Duration // wall-clock budget per crawl; defaults to defaultTimeout
	Logger  *slog.Logger
}

// New returns a Hacker News Engine configured per cfg.
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
		client:  cfg.Client.WithUpstream("hackernews", "api"),
		limiter: cfg.Limiter,
		apiBase: strings.TrimRight(cfg.APIBase, "/"),
		timeout: cfg.Timeout,
		logger:  cfg.Logger,
	}
}

// Name returns the engine identifier ("hackernews").
func (*Engine) Name() string { return "hackernews" }

// Matches claims the news.ycombinator.com URLs this engine renders: /item?id=N
// threads and the front-page-style feeds (/, /news, /newest, /ask, /show).
// Anything else (user profiles, /jobs, …) falls through to the generic fallback.
func (*Engine) Matches(rawURL string) bool {
	_, ok := parseTarget(rawURL)
	return ok
}

// target is the resolved fetch plan for a Hacker News URL.
type target struct {
	item bool   // an /item?id= thread
	id   string // numeric item id (item only)
	feed string // feed label (front_page/newest/ask/show) — list only
}

// parseTarget classifies a Hacker News URL into a fetch plan. ok is false for any
// news.ycombinator.com URL this engine doesn't render (so it falls through).
func parseTarget(rawURL string) (target, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || !hostMatcher.MatchString(u.Hostname()) {
		return target{}, false
	}
	// Normalize a trailing slash once so /item/ and /news/ match like /item and /news.
	p := strings.TrimSuffix(u.Path, "/")
	if p == "" {
		p = "/"
	}
	if p == "/item" {
		id := u.Query().Get("id")
		if !isDigits(id) {
			return target{}, false
		}
		return target{item: true, id: id}, true
	}
	if feed, ok := pathFeed[p]; ok {
		return target{feed: feed}, true
	}
	return target{}, false
}

// Crawl fetches the HN item or feed behind rawURL from the Algolia API and
// returns it encoded as TOON.
func (e *Engine) Crawl(ctx context.Context, rawURL string, eo domain.EngineOptions) (domain.Document, error) {
	t, ok := parseTarget(rawURL)
	if !ok {
		return domain.Document{}, fmt.Errorf("unsupported hacker news url: %s", rawURL)
	}

	// Bound wall-clock independently of the shared HTTP client timeout (which is
	// the crawl4ai knob); the HN engine talks to Algolia directly, not crawl4ai.
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	if e.limiter != nil {
		release, lerr := e.limiter.Acquire(ctx, e.Name(), e.apiBase)
		if lerr != nil {
			return domain.Document{}, lerr
		}
		defer release()
	}

	if t.item {
		raw, err := e.get(ctx, e.apiBase+"/items/"+t.id)
		if err != nil {
			return domain.Document{}, fmt.Errorf("fetch item: %w", err)
		}
		thread, perr := parseThread(raw)
		if perr != nil {
			return domain.Document{}, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse item: %w", perr)}
		}
		meta := map[string]string{"id": t.id}
		total := len(thread.Comments)
		// Caps in structural order: whole threads first, then breadth inside each
		// kept thread, then the absolute ceiling on the flat list.
		capTopLevel(&thread, eo.HNMaxTopLevel)
		capPerSubtree(&thread, eo.HNMaxPerSubtree)
		capComments(&thread, commentCeiling(eo.HNMaxComments))
		if len(thread.Comments) < total {
			meta["truncated_from"] = strconv.Itoa(total)
		}
		meta["comments"] = strconv.Itoa(len(thread.Comments))
		return e.document(thread, rawURL, meta)
	}

	fq := feedQuery[t.feed]
	q := url.Values{}
	q.Set("tags", fq.tag)
	q.Set("hitsPerPage", strconv.Itoa(frontPageSize))
	raw, err := e.get(ctx, e.apiBase+"/"+fq.endpoint+"?"+q.Encode())
	if err != nil {
		return domain.Document{}, fmt.Errorf("fetch feed: %w", err)
	}
	fp, perr := parseFrontPage(raw, t.feed)
	if perr != nil {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse feed: %w", perr)}
	}
	return e.document(fp, rawURL, map[string]string{
		"feed":    t.feed,
		"stories": strconv.Itoa(len(fp.Stories)),
	})
}

// commentCeiling resolves the absolute cap on emitted comments: the caller's
// max_comments when it asks for fewer, otherwise MaxThreadComments. The engine
// ceiling always wins over a larger caller value — it exists so a megathread
// can't blow the consumer's context no matter what was asked for.
func commentCeiling(requested int) int {
	if requested > 0 {
		return min(requested, MaxThreadComments)
	}
	return MaxThreadComments
}

// get fetches an Algolia API URL and returns the raw JSON body.
func (e *Engine) get(ctx context.Context, apiURL string) ([]byte, error) {
	resp, err := e.client.DoRetry(ctx, http.MethodGet, apiURL, nil,
		map[string]string{"Accept": "application/json"}, httpx.RetryConfig{})
	if err != nil {
		return nil, httpx.ClassifyClientError(err, domain.KindUpstreamError)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20MB cap
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &domain.FetchError{
			Kind:       domain.KindForStatus(resp.StatusCode),
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("hn algolia returned %d", resp.StatusCode),
		}
	}
	return body, nil
}

// document encodes v as TOON and wraps it in a Document with standard metadata.
func (e *Engine) document(v any, source string, extra map[string]string) (domain.Document, error) {
	encoded, err := toon.Marshal(v, toon.WithLengthMarkers(true))
	if err != nil {
		return domain.Document{}, fmt.Errorf("encode: %w", err)
	}
	meta := map[string]string{
		"source":              source,
		"status_code":         "200",
		domain.ContentTypeKey: domain.ContentTypeTOON,
	}
	for k, val := range extra {
		meta[k] = val
	}
	e.logger.Info("hackernews crawl complete", "source", source, "bytes", len(encoded))
	return domain.Document{PageContent: string(encoded), Metadata: meta}, nil
}

// isDigits reports whether s is a non-empty run of ASCII digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
