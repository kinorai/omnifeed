package twitter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/observability"
)

const (
	// DefaultFxTwitterURL is the public FxTwitter API. A self-hosted FxEmbed
	// serves the same API (OMNIFEED_TWITTER_FXTWITTER_URL).
	DefaultFxTwitterURL = "https://api.fxtwitter.com"
	// DefaultMaxReplies is how many replies a post renders by default.
	DefaultMaxReplies = 20

	defaultSyndicationURL = "https://cdn.syndication.twimg.com"
	defaultVxTwitterURL   = "https://api.vxtwitter.com"
	// defaultTimeout bounds each upstream API request. The sources are tried
	// one after another, so a hung mirror must not eat the whole budget.
	defaultTimeout = 15 * time.Second
	// cacheTTL keeps a fetched post for repeat reads (an agent re-fetching the
	// same link, several HN threads pointing at one post).
	cacheTTL     = 15 * time.Minute
	cacheEntries = 512
)

// Upstream labels, reported in the document metadata as `upstream`.
const (
	sourceFxTwitter     = "fxtwitter"
	sourceSyndication   = "syndication"
	sourceSyndicationVx = "syndication+vxtwitter"
	sourceVxTwitter     = "vxtwitter"
	sourceCrawl4AI      = "crawl4ai"
)

// Engine implements domain.Engine for X / Twitter post URLs.
type Engine struct {
	fx, synd, vx, tco *httpx.Client
	limiter           httpx.Limiter
	generic           domain.Engine
	fxBase            string
	syndBase          string
	vxBase            string
	maxReplies        int
	timeout           time.Duration
	blockPrivate      bool
	cache             *ttlCache
	logger            *slog.Logger
}

// Config configures a Twitter Engine.
type Config struct {
	Client  *httpx.Client
	Limiter httpx.Limiter
	// Generic is the engine tried last, on the canonical x.com URL (crawl4ai
	// in production). Nil skips that step and lets the registry's fallback
	// handle a failure on the original URL instead.
	Generic        domain.Engine
	FxTwitterURL   string // defaults to DefaultFxTwitterURL
	SyndicationURL string // defaults to the public endpoint; overridden in tests
	VxTwitterURL   string // defaults to the public endpoint; overridden in tests
	MaxReplies     int    // defaults to DefaultMaxReplies
	Timeout        time.Duration
	// BlockPrivateIPs applies the SSRF guard to the destination of a t.co
	// link before it is crawled.
	BlockPrivateIPs bool
	Logger          *slog.Logger
}

// New returns a Twitter Engine configured per cfg.
func New(cfg Config) *Engine {
	if cfg.FxTwitterURL == "" {
		cfg.FxTwitterURL = DefaultFxTwitterURL
	}
	if cfg.SyndicationURL == "" {
		cfg.SyndicationURL = defaultSyndicationURL
	}
	if cfg.VxTwitterURL == "" {
		cfg.VxTwitterURL = defaultVxTwitterURL
	}
	if cfg.MaxReplies <= 0 {
		cfg.MaxReplies = DefaultMaxReplies
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Client == nil {
		cfg.Client = httpx.New(nil)
	}
	// t.co is resolved by reading Location from its redirect, never by
	// following it: the destination is unvetted until ValidateURL has seen it.
	noRedirect := *cfg.Client.HTTP
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	tco := *cfg.Client
	tco.HTTP = &noRedirect

	return &Engine{
		fx:           cfg.Client.WithUpstream("twitter", "fxtwitter"),
		synd:         cfg.Client.WithUpstream("twitter", "syndication"),
		vx:           cfg.Client.WithUpstream("twitter", "vxtwitter"),
		tco:          tco.WithUpstream("twitter", "tco"),
		limiter:      cfg.Limiter,
		generic:      cfg.Generic,
		fxBase:       strings.TrimRight(cfg.FxTwitterURL, "/"),
		syndBase:     strings.TrimRight(cfg.SyndicationURL, "/"),
		vxBase:       strings.TrimRight(cfg.VxTwitterURL, "/"),
		maxReplies:   cfg.MaxReplies,
		timeout:      cfg.Timeout,
		blockPrivate: cfg.BlockPrivateIPs,
		cache:        newTTLCache(cacheTTL, cacheEntries),
		logger:       cfg.Logger,
	}
}

// Name returns the engine identifier ("twitter").
func (*Engine) Name() string { return "twitter" }

// Matches claims post URLs on x.com, twitter.com and the fixer mirrors, and
// t.co short links. Profiles, search, lists and the rest fall through to the
// generic engine (which rewrites twitter.com to x.com).
func (*Engine) Matches(rawURL string) bool {
	_, ok := parseTarget(rawURL)
	return ok
}

// Crawl renders the post behind rawURL.
func (e *Engine) Crawl(ctx context.Context, rawURL string, opts domain.EngineOptions) (domain.Document, error) {
	t, ok := parseTarget(rawURL)
	if !ok {
		return domain.Document{}, fmt.Errorf("unsupported twitter url: %s", rawURL)
	}
	if !t.shortLink {
		return e.crawlPost(ctx, rawURL, t.id, opts)
	}

	dest, err := e.resolveShortLink(ctx, rawURL)
	if err != nil {
		return domain.Document{}, err
	}
	if dt, ok := parseTarget(dest); ok && !dt.shortLink {
		return e.crawlPost(ctx, rawURL, dt.id, opts)
	}
	// Not a post: hand the destination to the generic path. The registry
	// validated only the t.co URL, so the destination gets its own check.
	if e.generic == nil {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindError,
			Err: fmt.Errorf("t.co link leads to %s, which is not an X post and no generic engine is configured", dest)}
	}
	doc, err := e.generic.Crawl(ctx, dest, opts)
	return doc, domain.NoFallback(err)
}

// crawlPost fetches post id through the source chain and renders it.
func (e *Engine) crawlPost(ctx context.Context, rawURL, id string, opts domain.EngineOptions) (domain.Document, error) {
	b, err := e.fetchBundle(ctx, id)
	if err == nil {
		return e.document(b, rawURL, opts)
	}
	if ctx.Err() != nil {
		return domain.Document{}, err
	}
	var failed *chainError
	if !errors.As(err, &failed) {
		return domain.Document{}, err
	}
	if failed.notFound() {
		// Two independent sources say the post does not exist: a browser
		// render would only return X's "this page doesn't exist" shell.
		return domain.Document{}, domain.NoFallback(failed.classify())
	}
	if e.generic == nil {
		return domain.Document{}, failed.classify()
	}

	canonical := canonicalURL(id)
	doc, gerr := e.generic.Crawl(ctx, canonical, opts)
	if gerr == nil {
		gerr = thinXPage(doc)
	}
	if gerr != nil {
		failed.add(sourceCrawl4AI, gerr)
		return domain.Document{}, domain.NoFallback(failed.classify())
	}
	e.logger.Warn("twitter APIs failed, served the x.com page through the generic engine",
		"id", id, "err", failed.summary())
	if doc.Metadata == nil {
		doc.Metadata = map[string]string{}
	}
	doc.Metadata["upstream"] = sourceCrawl4AI
	doc.Metadata["id"] = id
	return doc, nil
}

// fetchBundle tries the cache, then each API source in order.
func (e *Engine) fetchBundle(ctx context.Context, id string) (bundle, error) {
	if b, ok := e.cache.get(id); ok {
		return b, nil
	}
	chain := &chainError{}
	for _, src := range []struct {
		name  string
		fetch func(context.Context, string) (bundle, error)
	}{
		{sourceFxTwitter, e.fetchFxTwitter},
		{sourceSyndication, e.fetchSyndication},
		{sourceVxTwitter, e.fetchVxBundle},
	} {
		b, err := src.fetch(ctx, id)
		if err == nil {
			e.cache.put(id, b)
			if len(chain.attempts) > 0 {
				e.logger.Warn("twitter source fallback", "id", id, "served_by", b.Source, "failed", chain.summary())
			}
			return b, nil
		}
		if ctx.Err() != nil {
			return bundle{}, err
		}
		chain.add(src.name, err)
	}
	return bundle{}, chain
}

// get fetches an upstream API URL under the per-domain limiter and returns
// the body of a 200 response. A 404 becomes the not-found FetchError.
func (e *Engine) get(ctx context.Context, c *httpx.Client, apiURL, source string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	if e.limiter != nil {
		release, err := e.limiter.Acquire(ctx, e.Name(), apiURL)
		if err != nil {
			return nil, httpx.ClassifyClientError(err, domain.KindUpstreamError)
		}
		defer release()
	}
	resp, err := c.DoRetry(ctx, http.MethodGet, apiURL, nil,
		map[string]string{"Accept": "application/json"}, httpx.RetryConfig{MaxAttempts: 2})
	if err != nil {
		return nil, httpx.ClassifyClientError(err, domain.KindUpstreamError)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20MB cap
	if err != nil {
		return nil, httpx.ClassifyClientError(err, domain.KindUpstreamError)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return body, nil
	case http.StatusNotFound:
		return nil, notFoundError(source)
	default:
		return nil, &domain.FetchError{
			Kind:       domain.KindForStatus(resp.StatusCode),
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("%s returned %d", source, resp.StatusCode),
		}
	}
}

// resolveShortLink reads the destination of a t.co link from its redirect
// and runs it through the SSRF guard.
func (e *Engine) resolveShortLink(ctx context.Context, rawURL string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	if e.limiter != nil {
		release, err := e.limiter.Acquire(ctx, e.Name(), rawURL)
		if err != nil {
			return "", httpx.ClassifyClientError(err, domain.KindUpstreamError)
		}
		defer release()
	}
	resp, err := e.tco.DoRetry(ctx, http.MethodHead, rawURL, nil, nil, httpx.RetryConfig{MaxAttempts: 2})
	if err != nil {
		return "", httpx.ClassifyClientError(err, domain.KindUpstreamError)
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", &domain.FetchError{Kind: domain.KindForStatus(resp.StatusCode), StatusCode: resp.StatusCode,
			Err: fmt.Errorf("t.co answered %d instead of a redirect", resp.StatusCode)}
	}
	loc, err := resp.Request.URL.Parse(resp.Header.Get("Location"))
	if err != nil || resp.Header.Get("Location") == "" {
		return "", &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("t.co redirect has no usable Location")}
	}
	dest := loc.String()
	if err := httpx.ValidateURL(dest, e.blockPrivate); err != nil {
		return "", &domain.FetchError{Kind: domain.KindError, Err: fmt.Errorf("t.co destination rejected: %w", err)}
	}
	return dest, nil
}

// --- Failure classification ---------------------------------------------------

// attempt is one source's failure.
type attempt struct {
	source string
	err    error
}

// chainError collects every source's failure for one post.
type chainError struct{ attempts []attempt }

func (c *chainError) add(source string, err error) {
	c.attempts = append(c.attempts, attempt{source, err})
}

func (c *chainError) Error() string { return "every twitter source failed: " + c.summary() }

// summary is "source: reason; …" for logs and the error message.
func (c *chainError) summary() string {
	parts := make([]string, 0, len(c.attempts))
	for _, a := range c.attempts {
		parts = append(parts, a.source+": "+observability.Explain(a.err))
	}
	return strings.Join(parts, "; ")
}

// notFound reports whether FxTwitter and at least one other source both say
// the post does not exist (or is withheld). One source alone is not trusted.
func (c *chainError) notFound() bool {
	fx, other := false, false
	for _, a := range c.attempts {
		if !isNotFound(a.err) {
			continue
		}
		if a.source == sourceFxTwitter {
			fx = true
		} else {
			other = true
		}
	}
	return fx && other
}

// classify turns the collected failures into the one error the caller sees:
// not found (HTTP 404), blocked (the block kind, 429 first), or the last
// source's own classification.
func (c *chainError) classify() error {
	if c.notFound() {
		return &domain.FetchError{Kind: domain.KindError, StatusCode: http.StatusNotFound,
			Err: fmt.Errorf("tweet unavailable: not found — deleted, protected, or its account suspended (%s)", c.summary())}
	}
	blocked := len(c.attempts) > 0
	var blockKind domain.FailureKind
	for _, a := range c.attempts {
		k := kindOf(a.err)
		switch k {
		case domain.KindHTTP429:
			blockKind = k
		case domain.KindHTTP403, domain.KindCaptcha, domain.KindBotBlock, domain.KindQuotaExhausted:
			if blockKind == "" {
				blockKind = k
			}
		default:
			blocked = false
		}
	}
	if blocked {
		return &domain.FetchError{Kind: blockKind,
			Err: fmt.Errorf("tweet unavailable: blocked or rate-limited by every source (%s)", c.summary())}
	}
	last := domain.KindUpstreamError
	if n := len(c.attempts); n > 0 {
		if k := kindOf(c.attempts[n-1].err); k != domain.KindError {
			last = k
		}
	}
	return &domain.FetchError{Kind: last,
		Err: fmt.Errorf("tweet unavailable: every source failed (%s)", c.summary())}
}

func kindOf(err error) domain.FailureKind {
	var fe *domain.FetchError
	if errors.As(err, &fe) {
		return fe.Kind
	}
	return domain.KindError
}

func isNotFound(err error) bool {
	var fe *domain.FetchError
	return errors.As(err, &fe) && fe.StatusCode == http.StatusNotFound
}

// notFoundError is a source's "no such post".
func notFoundError(source string) error {
	return &domain.FetchError{Kind: domain.KindError, StatusCode: http.StatusNotFound,
		Err: fmt.Errorf("%s: post not found", source)}
}

// thinXPageMarkers are the x.com error shells a logged-out render can return
// in place of a post.
var thinXPageMarkers = []string{
	"this page doesn’t exist", "this page doesn't exist",
	"something went wrong. try reloading",
	"this post is unavailable", "this post was deleted",
}

// thinXPage rejects a generic render of x.com that holds no post: X's error
// shells, or text too short to be one.
func thinXPage(doc domain.Document) error {
	lower := strings.ToLower(doc.PageContent)
	for _, m := range thinXPageMarkers {
		if strings.Contains(lower, m) {
			return &domain.FetchError{Kind: domain.KindThinContent, Err: fmt.Errorf("x.com served an error page (%q)", m)}
		}
	}
	if utf8.RuneCountInString(strings.TrimSpace(doc.PageContent)) < 80 {
		return &domain.FetchError{Kind: domain.KindThinContent, Err: fmt.Errorf("x.com page rendered near-empty")}
	}
	return nil
}

// --- Small helpers -------------------------------------------------------------

// leadingMentions is the run of @handles X prepends to a reply's text.
var leadingMentions = regexp.MustCompile(`^(?:@[A-Za-z0-9_]{1,15}\s+)+`)

// onlyMentions matches a text that is nothing but @handles (or nothing): a
// reply like that keeps its mentions, they are its whole content.
var onlyMentions = regexp.MustCompile(`^\s*(?:@[A-Za-z0-9_]{1,15}\s*)*$`)

// cleanText trims a post's text (and each line's trailing blanks) and, for a reply, drops the leading @handles
// X adds automatically (the reply context says who it answers).
func cleanText(text string, reply bool) string {
	text = strings.TrimSpace(text)
	if reply {
		if stripped := leadingMentions.ReplaceAllString(text, ""); !onlyMentions.MatchString(stripped) {
			text = stripped
		}
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.Join(lines, "\n")
}

// unixTime renders a post time as RFC 3339 UTC, from the epoch when known,
// else from Twitter's "Wed Sep 30 01:20:17 +0000 2026" layout.
func unixTime(epoch int64, s string) string {
	if epoch > 0 {
		return time.Unix(epoch, 0).UTC().Format(time.RFC3339)
	}
	if t, err := time.Parse(time.RubyDate, s); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return ""
}

func nonEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func sameHandle(a, b string) bool { return a != "" && strings.EqualFold(a, b) }
