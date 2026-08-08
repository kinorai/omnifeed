package reddit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/observability"
	"github.com/toon-format/toon-go"
)

// MaxExpansionRounds is the hard cap on /api/morechildren iterations, applied
// even when expand=full is requested. Each round fetches up to 100 child IDs,
// so 40 rounds ≈ 4,000 comments — covers any realistic thread while bounding
// total latency and rate-limit exposure.
const MaxExpansionRounds = 40

// listingLimit caps posts fetched for a subreddit listing. Reddit's own default
// page is 25; a listing carries no comment trees, so this stays small and bounded
// regardless of the (comment-oriented) FetchLimit knob.
const listingLimit = 25

// hostMatcher matches reddit.com and its subdomains (old, new, np, m, amp, ...).
var hostMatcher = httpx.HostMatcher("reddit.com")

// Engine implements domain.Engine for Reddit URLs.
type Engine struct {
	fetcher     *Fetcher
	limiter     *httpx.DomainLimiter
	timeout     time.Duration
	defaultOpts Options
	logger      *slog.Logger
	metrics     *observability.Metrics
}

// Config configures a Reddit Engine.
type Config struct {
	Fetcher     *Fetcher
	Limiter     *httpx.DomainLimiter
	Timeout     time.Duration
	DefaultOpts Options
	Logger      *slog.Logger
	Metrics     *observability.Metrics
}

// New returns a Reddit Engine configured per cfg.
func New(cfg Config) *Engine {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 4 * time.Minute
	}
	return &Engine{
		fetcher:     cfg.Fetcher,
		limiter:     cfg.Limiter,
		timeout:     cfg.Timeout,
		defaultOpts: cfg.DefaultOpts,
		logger:      cfg.Logger,
		metrics:     cfg.Metrics,
	}
}

// Name returns the engine identifier.
func (*Engine) Name() string { return "reddit" }

// Matches claims only the reddit.com URLs this engine can actually render: a
// comments permalink, or a share link (/r/{sub}/s/{code}) that resolves to one.
// Other reddit.com URLs — profiles, wikis, search pages, /dev/api — fall through
// to the generic fallback engine instead of hard-failing in NormalizePermalink.
func (*Engine) Matches(rawURL string) bool {
	if !IsRedditURL(rawURL) {
		return false
	}
	if IsShareURL(rawURL) {
		return true
	}
	if _, err := NormalizePermalink(rawURL); err == nil {
		return true
	}
	_, _, ok := ParseListingURL(rawURL)
	return ok
}

// IsRedditURL is the package-level matcher used by Engine.Matches; exported so
// callers (and tests) can detect Reddit URLs without instantiating an Engine.
func IsRedditURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return hostMatcher.MatchString(u.Hostname())
}

// Crawl fetches a Reddit thread, expands gaps up to the configured budget,
// and returns it encoded as TOON or JSON.
func (e *Engine) Crawl(ctx context.Context, rawURL string, eo domain.EngineOptions) (domain.Document, error) {
	opts := e.resolveOptions(eo)

	// Bound wall-clock so a slow expand=full can't outlive the handler.
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	// A bare subreddit listing (/r/{sub}/, /r/{sub}/top, …) has no comment tree —
	// fetch the post list instead. Checked before the comments path because
	// ParseListingURL only claims listing-shaped paths (never /comments/).
	if sub, sort, ok := ParseListingURL(rawURL); ok {
		return e.crawlListing(ctx, rawURL, sub, sort, opts)
	}

	// The per-domain limiter spans the whole crawl including the share-link
	// resolve and all expansion rounds — concurrent crawls of different reddit
	// URLs serialize here. Fine for single-tenant deployments. Acquired BEFORE
	// the browser session opens (matching crawlListing): with Lightpanda each
	// Open dials its own CDP connection and the share resolve is a full browser
	// navigation, so acquiring later would let concurrent crawls stampede Reddit
	// un-throttled. Key the limiter on the same host the fetch actually hits
	// (redditOrigin = www.reddit.com) so thread and listing crawls share one
	// per-domain slot — DomainLimiter buckets by Hostname(), and "reddit.com"
	// != "www.reddit.com".
	release, lerr := e.limiter.Acquire(ctx, redditOrigin+"/")
	if lerr != nil {
		return domain.Document{}, lerr
	}
	defer release()

	// One browser session for the whole crawl (share resolve + thread fetch + all
	// expansion rounds), so the live-page backend reuses its page across them.
	sess, err := e.fetcher.Open(ctx)
	if err != nil {
		return domain.Document{}, fmt.Errorf("open browser session: %w", err)
	}
	defer func() { _ = sess.Close(ctx) }()

	// Reddit share links (/r/{sub}/s/{code}) 301-redirect to the canonical
	// /comments/ permalink; resolve them in the browser before normalizing.
	if IsShareURL(rawURL) {
		resolved, rerr := sess.ResolveShareURL(ctx, rawURL)
		if rerr != nil {
			return domain.Document{}, fmt.Errorf("resolve share url: %w", rerr)
		}
		rawURL = resolved
	}

	permalink, err := NormalizePermalink(rawURL)
	if err != nil {
		return domain.Document{}, fmt.Errorf("normalize url: %w", err)
	}

	threadJSON, err := sess.FetchThread(ctx, permalink, opts.FetchLimit, opts.Depth, opts.Sort)
	if err != nil {
		return domain.Document{}, fmt.Errorf("fetch thread: %w", err)
	}

	thread, err := ParseThread(threadJSON, opts)
	if err != nil {
		return domain.Document{}, fmt.Errorf("parse thread: %w", err)
	}

	rounds := e.expandGaps(ctx, sess, &thread, opts)
	if e.metrics != nil {
		e.metrics.RedditRounds.Observe(float64(rounds))
	}

	// Post-fetch size caps: applied after expansion so they bound the final
	// output regardless of how much expansion produced. Top-level cap first
	// (structural — keeps whole threads), then the absolute comment ceiling.
	if opts.MaxTopLevel > 0 {
		capTopLevel(&thread, opts.MaxTopLevel)
	}
	if opts.MaxComments > 0 {
		capComments(&thread, opts.MaxComments)
	}

	// Strip the per-gap child-ID lists from the output — they were only
	// needed internally for /api/morechildren expansion.
	for i := range thread.Gaps {
		if thread.Gaps[i].Type == "more" {
			thread.Gaps[i].Children = nil
		}
	}

	encoded, err := encode(&thread, opts.Format)
	if err != nil {
		return domain.Document{}, fmt.Errorf("encode: %w", err)
	}

	e.logger.Info("reddit crawl complete",
		"post", thread.Post.ID,
		"rounds", rounds,
		"comments", len(thread.Comments),
		"gaps_left", len(thread.Gaps),
		"format", opts.Format,
		"bytes", len(encoded),
	)

	return domain.Document{
		PageContent: string(encoded),
		Metadata: map[string]string{
			"source":              "https://www.reddit.com" + thread.Post.Permalink,
			"status_code":         "200",
			"format":              opts.Format,
			domain.ContentTypeKey: opts.Format,
			"comments":            strconv.Itoa(len(thread.Comments)),
			"gaps":                strconv.Itoa(len(thread.Gaps)),
			"total_comments":      strconv.Itoa(thread.Post.NumComments),
		},
	}, nil
}

// resolveOptions merges per-request options on top of the engine's defaults.
func (e *Engine) resolveOptions(eo domain.EngineOptions) Options {
	opts := e.defaultOpts
	if eo.RedditFormat == "toon" || eo.RedditFormat == "json" {
		opts.Format = eo.RedditFormat
	}
	if eo.RedditKeepDepth {
		opts.KeepDepth = true
	}
	if eo.RedditKeepCreated {
		opts.KeepCreated = true
	}
	if eo.RedditMaxRounds > 0 {
		opts.MaxRounds = min(eo.RedditMaxRounds, MaxExpansionRounds)
	}
	if opts.MaxRounds == 0 {
		opts.MaxRounds = 3
	}
	if opts.Format == "" {
		opts.Format = "toon"
	}

	// Size knobs: a positive per-request value overrides the engine default.
	if eo.RedditFetchLimit > 0 {
		opts.FetchLimit = eo.RedditFetchLimit
	}
	if eo.RedditDepth > 0 {
		opts.Depth = eo.RedditDepth
	}
	if eo.RedditSort != "" && domain.ValidRedditSort(eo.RedditSort) {
		opts.Sort = eo.RedditSort
	}
	if eo.RedditMaxComments > 0 {
		opts.MaxComments = eo.RedditMaxComments
	}
	if eo.RedditMaxTopLevel > 0 {
		opts.MaxTopLevel = eo.RedditMaxTopLevel
	}
	// Hard fallbacks for the Reddit-side params, which must always be valid.
	if opts.FetchLimit <= 0 {
		opts.FetchLimit = domain.DefaultRedditFetchLimit
	}
	if opts.Depth <= 0 {
		opts.Depth = domain.DefaultRedditDepth
	}
	if opts.Sort == "" {
		opts.Sort = domain.DefaultRedditSort
	}
	return opts
}

// expandGaps walks the gap list and calls /api/morechildren up to
// opts.MaxRounds times. Returns the number of rounds actually performed.
func (e *Engine) expandGaps(ctx context.Context, sess *Session, thread *Thread, opts Options) int {
	linkFullID := kindPostPrefix + thread.Post.ID
	rounds := 0
	for round := 0; round < opts.MaxRounds; round++ {
		var batchIDs []string
		var usedGapIdx []int
		const maxBatch = 100
		for i, g := range thread.Gaps {
			if g.Type != "more" || len(g.Children) == 0 {
				continue
			}
			for _, c := range g.Children {
				if len(batchIDs) >= maxBatch {
					break
				}
				batchIDs = append(batchIDs, c)
			}
			usedGapIdx = append(usedGapIdx, i)
			if len(batchIDs) >= maxBatch {
				break
			}
		}
		if len(batchIDs) == 0 {
			break
		}
		moreJSON, err := sess.FetchMoreChildren(ctx, linkFullID, batchIDs, opts.Sort)
		if err != nil {
			e.logger.Warn("morechildren round failed", "round", round, "err", err)
			break
		}
		newComments, newGaps, perr := ParseMoreChildren(moreJSON, opts)
		if perr != nil {
			e.logger.Warn("morechildren parse failed", "round", round, "err", perr)
			break
		}
		MergeExpanded(thread, newComments, newGaps, batchIDs, usedGapIdx)
		rounds = round + 1
		if len(newComments) == 0 && len(newGaps) == 0 {
			break
		}
	}
	return rounds
}

// encode serializes v as TOON (default) or JSON. Generic over the output shape
// so threads and subreddit listings share one serialization contract.
func encode[T any](v T, format string) ([]byte, error) {
	if format == "json" {
		return json.Marshal(v)
	}
	return toon.Marshal(v, toon.WithLengthMarkers(true))
}

// crawlListing renders a bare subreddit page (/r/{sub}/{sort}) as its post list.
// It reuses the same browser-fetch path as threads (so it clears Reddit's wall),
// but parses the flat Listing shape instead of a comment tree.
func (e *Engine) crawlListing(ctx context.Context, rawURL, sub, sort string, opts Options) (domain.Document, error) {
	release, lerr := e.limiter.Acquire(ctx, redditOrigin+"/r/"+sub+"/"+sort)
	if lerr != nil {
		return domain.Document{}, lerr
	}
	defer release()

	sess, err := e.fetcher.Open(ctx)
	if err != nil {
		return domain.Document{}, fmt.Errorf("open browser session: %w", err)
	}
	defer func() { _ = sess.Close(ctx) }()

	raw, err := sess.FetchListing(ctx, sub, sort, listingLimit)
	if err != nil {
		return domain.Document{}, fmt.Errorf("fetch listing: %w", err)
	}
	posts, err := ParseSubredditListing(raw)
	if err != nil {
		return domain.Document{}, fmt.Errorf("parse listing: %w", err)
	}

	listing := SubredditListing{Subreddit: sub, Sort: sort, Posts: posts}
	encoded, err := encode(&listing, opts.Format)
	if err != nil {
		return domain.Document{}, fmt.Errorf("encode: %w", err)
	}

	e.logger.Info("reddit listing complete",
		"subreddit", sub,
		"sort", sort,
		"posts", len(posts),
		"format", opts.Format,
		"bytes", len(encoded),
	)

	return domain.Document{
		PageContent: string(encoded),
		Metadata: map[string]string{
			"source":              rawURL, // echo the caller's URL, not a fabricated /sort/ path
			"status_code":         "200",
			"format":              opts.Format,
			domain.ContentTypeKey: opts.Format,
			"posts":               strconv.Itoa(len(posts)),
		},
	}, nil
}
