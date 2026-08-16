package bluesky

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
	defaultAPIBase = "https://public.api.bsky.app/xrpc"
	defaultTimeout = 30 * time.Second // wall-clock budget per Bluesky crawl
	// threadDepth is how many reply levels to request. The lexicon default is 6
	// and the maximum 1000; 20 covers the deep sub-threads that carry the real
	// discussion without asking the AppView to walk a whole megathread.
	threadDepth = 20
	// feedLimit is how many posts an author-feed URL returns (lexicon maximum
	// is 100).
	feedLimit = 50
	// maxThreadReplies caps emitted replies so a viral thread can't blow the
	// consumer's context. Pre-order DFS, so the tail is what gets cut.
	maxThreadReplies = 500
	// postCollection is the AT Protocol record collection a bsky.app /post/
	// URL refers to.
	postCollection = "app.bsky.feed.post"
)

// hostMatcher matches bsky.app and its subdomains.
var hostMatcher = httpx.HostMatcher("bsky.app")

// Engine implements domain.Engine for Bluesky URLs via the public AppView.
type Engine struct {
	client  *httpx.Client
	limiter *httpx.DomainLimiter
	apiBase string
	timeout time.Duration
	logger  *slog.Logger
}

// Config configures a Bluesky Engine.
type Config struct {
	Client  *httpx.Client
	Limiter *httpx.DomainLimiter
	APIBase string        // defaults to the public AppView; overridden in tests
	Timeout time.Duration // wall-clock budget per crawl; defaults to defaultTimeout
	Logger  *slog.Logger
}

// New returns a Bluesky Engine configured per cfg.
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
		client:  cfg.Client.WithUpstream("bluesky", "api"),
		limiter: cfg.Limiter,
		apiBase: strings.TrimRight(cfg.APIBase, "/"),
		timeout: cfg.Timeout,
		logger:  cfg.Logger,
	}
}

// Name returns the engine identifier ("bluesky").
func (*Engine) Name() string { return "bluesky" }

// Matches claims the two bsky.app URL shapes this engine renders:
// /profile/{actor}/post/{rkey} threads and /profile/{actor} author feeds.
// Anything else (/search, feed generators, starter packs) falls through to the
// generic fallback — see the package doc on why /search is not claimed.
func (*Engine) Matches(rawURL string) bool {
	_, ok := parseTarget(rawURL)
	return ok
}

// target is the resolved fetch plan for a Bluesky URL.
type target struct {
	actor string // handle or DID from /profile/{actor}
	rkey  string // post record key; empty means the author feed
}

// parseTarget classifies a Bluesky URL into a fetch plan. ok is false for any
// bsky.app URL this engine doesn't render (so it falls through).
func parseTarget(rawURL string) (target, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || !hostMatcher.MatchString(u.Hostname()) {
		return target{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != "profile" || parts[1] == "" {
		return target{}, false
	}
	actor := parts[1]
	switch len(parts) {
	case 2:
		return target{actor: actor}, true
	case 4:
		if parts[2] != "post" || parts[3] == "" {
			return target{}, false
		}
		return target{actor: actor, rkey: parts[3]}, true
	default:
		return target{}, false
	}
}

// Crawl fetches the Bluesky thread or author feed behind rawURL from the public
// AppView and returns it encoded as TOON.
func (e *Engine) Crawl(ctx context.Context, rawURL string, _ domain.EngineOptions) (domain.Document, error) {
	t, ok := parseTarget(rawURL)
	if !ok {
		return domain.Document{}, fmt.Errorf("unsupported bluesky url: %s", rawURL)
	}

	// Bound wall-clock independently of the shared HTTP client timeout (which is
	// the crawl4ai knob); this engine talks to the AppView directly.
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	if e.limiter != nil {
		release, lerr := e.limiter.Acquire(ctx, e.Name(), e.apiBase)
		if lerr != nil {
			return domain.Document{}, lerr
		}
		defer release()
	}

	if t.rkey == "" {
		return e.crawlFeed(ctx, rawURL, t)
	}
	return e.crawlThread(ctx, rawURL, t)
}

// crawlThread renders a single post and its replies.
func (e *Engine) crawlThread(ctx context.Context, rawURL string, t target) (domain.Document, error) {
	// The AT-URI authority accepts a handle as well as a DID — the AppView
	// resolves it server-side — so no separate resolveHandle round trip.
	q := url.Values{}
	q.Set("uri", "at://"+t.actor+"/"+postCollection+"/"+t.rkey)
	q.Set("depth", strconv.Itoa(threadDepth))

	raw, err := e.get(ctx, e.apiBase+"/app.bsky.feed.getPostThread?"+q.Encode())
	if err != nil {
		return domain.Document{}, fmt.Errorf("fetch thread: %w", err)
	}
	thread, perr := parseThread(raw)
	if perr != nil {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse thread: %w", perr)}
	}

	meta := map[string]string{"actor": t.actor}
	if total := len(thread.Replies); total > maxThreadReplies {
		thread.Replies = thread.Replies[:maxThreadReplies]
		meta["truncated_from"] = strconv.Itoa(total)
	}
	meta["replies"] = strconv.Itoa(len(thread.Replies))
	return e.document(thread, rawURL, meta)
}

// crawlFeed renders an account's recent posts.
func (e *Engine) crawlFeed(ctx context.Context, rawURL string, t target) (domain.Document, error) {
	q := url.Values{}
	q.Set("actor", t.actor)
	q.Set("limit", strconv.Itoa(feedLimit))

	raw, err := e.get(ctx, e.apiBase+"/app.bsky.feed.getAuthorFeed?"+q.Encode())
	if err != nil {
		return domain.Document{}, fmt.Errorf("fetch feed: %w", err)
	}
	feed, perr := parseFeed(raw, t.actor)
	if perr != nil {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse feed: %w", perr)}
	}
	return e.document(feed, rawURL, map[string]string{
		"actor": t.actor,
		"posts": strconv.Itoa(len(feed.Posts)),
	})
}

// get fetches an AppView URL and returns the raw JSON body.
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
			Err:        fmt.Errorf("bluesky appview returned %d", resp.StatusCode),
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
	e.logger.Info("bluesky crawl complete", "source", source, "bytes", len(encoded))
	return domain.Document{PageContent: string(encoded), Metadata: meta}, nil
}
