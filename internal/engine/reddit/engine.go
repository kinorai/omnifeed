package reddit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/kinorai/search-crawl-reddit-proxy/internal/domain"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/httpx"
	"github.com/toon-format/toon-go"
)

// MaxExpansionRounds is the hard cap on /api/morechildren iterations, applied
// even when expand=full is requested. Each round fetches up to 100 child IDs,
// so 40 rounds ≈ 4,000 comments — covers any realistic thread while bounding
// total latency and rate-limit exposure.
const MaxExpansionRounds = 40

// HostMatcher matches reddit.com and its subdomains (old, new, np, m, amp, ...).
var hostMatcher = regexp.MustCompile(`(?i)(^|\.)reddit\.com$`)

// Engine implements domain.Engine for Reddit URLs.
type Engine struct {
	fetcher     *Fetcher
	limiter     *httpx.DomainLimiter
	timeout     time.Duration
	defaultOpts Options
	logger      *slog.Logger
}

// Config configures a Reddit Engine.
type Config struct {
	Fetcher     *Fetcher
	Limiter     *httpx.DomainLimiter
	Timeout     time.Duration
	DefaultOpts Options
	Logger      *slog.Logger
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
	}
}

// Name returns the engine identifier.
func (*Engine) Name() string { return "reddit" }

// Matches returns true for reddit.com URLs (any subdomain).
func (*Engine) Matches(rawURL string) bool { return IsRedditURL(rawURL) }

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

	// Reddit share links (/r/{sub}/s/{code}) 301-redirect to the canonical
	// /comments/ permalink; resolve them in the browser before normalizing.
	if IsShareURL(rawURL) {
		resolved, rerr := e.fetcher.ResolveShareURL(ctx, rawURL)
		if rerr != nil {
			return domain.Document{}, fmt.Errorf("resolve share url: %w", rerr)
		}
		rawURL = resolved
	}

	permalink, err := NormalizePermalink(rawURL)
	if err != nil {
		return domain.Document{}, fmt.Errorf("normalize url: %w", err)
	}

	// The per-domain limiter spans the whole crawl including all expansion
	// rounds — concurrent crawls of different reddit URLs serialize here.
	// Fine for single-tenant deployments.
	release := e.limiter.Acquire("https://reddit.com" + permalink)
	defer release()

	threadJSON, err := e.fetcher.FetchThread(ctx, permalink)
	if err != nil {
		return domain.Document{}, fmt.Errorf("fetch thread: %w", err)
	}

	thread, err := ParseThread(threadJSON, opts)
	if err != nil {
		return domain.Document{}, fmt.Errorf("parse thread: %w", err)
	}

	rounds := e.expandGaps(ctx, &thread, opts)

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
			"source":         "https://www.reddit.com" + thread.Post.Permalink,
			"status_code":    "200",
			"format":         opts.Format,
			"comments":       strconv.Itoa(len(thread.Comments)),
			"gaps":           strconv.Itoa(len(thread.Gaps)),
			"total_comments": strconv.Itoa(thread.Post.NumComments),
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
		if eo.RedditMaxRounds > MaxExpansionRounds {
			opts.MaxRounds = MaxExpansionRounds
		} else {
			opts.MaxRounds = eo.RedditMaxRounds
		}
	}
	if opts.MaxRounds == 0 {
		opts.MaxRounds = 3
	}
	if opts.Format == "" {
		opts.Format = "toon"
	}
	return opts
}

// expandGaps walks the gap list and calls /api/morechildren up to
// opts.MaxRounds times. Returns the number of rounds actually performed.
func (e *Engine) expandGaps(ctx context.Context, thread *Thread, opts Options) int {
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
		moreJSON, err := e.fetcher.FetchMoreChildren(ctx, linkFullID, batchIDs)
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

// encode serializes thread as TOON or JSON.
func encode(thread *Thread, format string) ([]byte, error) {
	if format == "json" {
		return json.Marshal(thread)
	}
	return toon.Marshal(thread, toon.WithLengthMarkers(true))
}

// OptionsFromQuery parses ?format=, ?depth=, ?nocreated=, ?expand= from a
// query string. Unknown values fall back to opts unchanged.
func OptionsFromQuery(q url.Values, opts Options) Options {
	if f := q.Get("format"); f == "json" || f == "toon" {
		opts.Format = f
	}
	if q.Get("depth") == "1" {
		opts.KeepDepth = true
	}
	if q.Get("nocreated") == "1" {
		opts.KeepCreated = false
	}
	if ex := q.Get("expand"); ex != "" {
		switch ex {
		case "full", "all", "max":
			opts.MaxRounds = MaxExpansionRounds
		default:
			if n, err := strconv.Atoi(ex); err == nil && n >= 0 {
				if n > MaxExpansionRounds {
					n = MaxExpansionRounds
				}
				opts.MaxRounds = n
			}
		}
	}
	return opts
}
