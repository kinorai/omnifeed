package discourse

import (
	"context"
	"encoding/json"
	"errors"
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

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/toon-format/toon-go"
)

const (
	defaultTimeout  = 30 * time.Second // wall-clock budget per Discourse crawl
	maxPosts        = 500              // cap emitted posts so a megathread can't blow the consumer's context
	postIDsPerBatch = 40               // post_ids[] per posts.json request on the fallback path
	userAgent       = "omnifeed"
	bodyLimit       = 20 << 20 // 20MB read cap per response
)

// targetRE claims exactly the topic path shapes: /t/{slug}/{id},
// /t/{slug}/{id}/{post_number}, and the slugless /t/{id}. Everything else on a
// listed host (front page, /c/ categories, /u/ users, /search, …) falls through.
var targetRE = regexp.MustCompile(`^/t/(?:([^/]+)/([0-9]+)(?:/[0-9]+)?|([0-9]+))$`)

// Engine implements domain.Engine for Discourse topic URLs via the public topic
// JSON API.
type Engine struct {
	client  *httpx.Client
	limiter *httpx.DomainLimiter
	hosts   map[string]struct{}
	timeout time.Duration
	logger  *slog.Logger
	// scheme is the URL scheme used for API requests. Always "https" in
	// production (Discourse forums are TLS-only); the tests point it at an
	// httptest server by overriding it to "http".
	scheme string
}

// Config configures a Discourse Engine.
type Config struct {
	Client  *httpx.Client
	Limiter *httpx.DomainLimiter
	// Hosts is the exact hostname allowlist. Empty means the engine claims
	// nothing — it can still be registered, Matches just always returns false.
	Hosts   []string
	Timeout time.Duration // wall-clock budget per crawl; defaults to defaultTimeout
	Logger  *slog.Logger
}

// New returns a Discourse Engine configured per cfg.
func New(cfg Config) *Engine {
	if cfg.Timeout == 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	hosts := make(map[string]struct{}, len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		if h = strings.ToLower(strings.TrimSpace(h)); h != "" {
			hosts[h] = struct{}{}
		}
	}
	return &Engine{
		client:  cfg.Client,
		limiter: cfg.Limiter,
		hosts:   hosts,
		timeout: cfg.Timeout,
		logger:  cfg.Logger,
		scheme:  "https",
	}
}

// Name returns the engine identifier ("discourse").
func (*Engine) Name() string { return "discourse" }

// Matches claims topic URLs on the configured hosts only. The host must equal a
// configured hostname exactly (case-insensitively) — no subdomain wildcarding,
// because a Discourse install at forum.example.com says nothing about
// example.com. Non-topic pages on a listed host, and every page on an unlisted
// host, fall through to the generic crawl4ai fallback.
func (e *Engine) Matches(rawURL string) bool {
	_, ok := e.parseTarget(rawURL)
	return ok
}

// target is the resolved fetch plan for a Discourse URL.
type target struct {
	host      string // hostname, for the allowlist check and the output metadata
	authority string // host[:port] as given, for building request URLs
	id        string // numeric topic id
}

// parseTarget classifies a Discourse URL into a fetch plan. ok is false for any
// URL this engine doesn't render (so it falls through).
func (e *Engine) parseTarget(rawURL string) (target, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return target{}, false
	}
	host := strings.ToLower(u.Hostname())
	if _, listed := e.hosts[host]; !listed {
		return target{}, false
	}
	// url.Parse already split off the query and fragment; normalize a trailing
	// slash so /t/slug/12/ matches like /t/slug/12.
	p := strings.TrimSuffix(u.Path, "/")
	m := targetRE.FindStringSubmatch(p)
	if m == nil {
		return target{}, false
	}
	id := m[2] // /t/{slug}/{id}[/{post_number}]
	if id == "" {
		id = m[3] // /t/{id}
	}
	return target{host: host, authority: strings.ToLower(u.Host), id: id}, true
}

// Crawl fetches the topic behind rawURL from the forum's topic JSON API and
// returns it encoded as TOON.
func (e *Engine) Crawl(ctx context.Context, rawURL string, _ domain.EngineOptions) (domain.Document, error) {
	t, ok := e.parseTarget(rawURL)
	if !ok {
		return domain.Document{}, fmt.Errorf("unsupported discourse url: %s", rawURL)
	}

	// Bound wall-clock independently of the shared HTTP client timeout (which is
	// the crawl4ai knob); this engine talks to the forum directly.
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	base := e.scheme + "://" + t.authority
	if e.limiter != nil {
		release := e.limiter.Acquire(base)
		defer release()
	}

	topic, posts, total, err := e.fetchTopic(ctx, base, t.id)
	if err != nil {
		return domain.Document{}, err
	}

	meta := map[string]string{"posts": strconv.Itoa(len(posts))}
	if total > len(posts) {
		meta["truncated_from"] = strconv.Itoa(total)
	}
	return e.document(Thread{
		Topic: Topic{
			Title:      topic.Title,
			PostsCount: topic.PostsCount,
			Created:    topic.CreatedAt,
			Host:       t.host,
		},
		Posts: posts,
	}, rawURL, meta)
}

// fetchTopic resolves a topic to its header and posts. total is how many posts
// the topic has upstream — greater than len(posts) exactly when the budget cut
// the list.
//
// The print view returns the whole topic (up to ~1000 posts) in a single
// response, so it is tried first. It is rate-limited more aggressively than the
// normal topic endpoint though, so a 429 there falls back to the paged route:
// the plain topic JSON (first chunk of posts + post_stream.stream = every post id
// in display order) plus batched posts.json requests for the remainder.
func (e *Engine) fetchTopic(ctx context.Context, base, id string) (apiTopic, []Post, int, error) {
	raw, err := e.get(ctx, base+"/t/"+id+".json?print=true&include_raw=1")
	if err == nil {
		var at apiTopic
		if jerr := json.Unmarshal(raw, &at); jerr != nil {
			return apiTopic{}, nil, 0, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse topic: %w", jerr)}
		}
		total := max(at.PostsCount, len(at.PostStream.Posts))
		return at, convert(capPosts(at.PostStream.Posts)), total, nil
	}
	// Discourse rate-limits the print view with either 429 or — observed live on
	// discuss.python.org — 422 ("You've performed this action too many times").
	if !isStatus(err, http.StatusTooManyRequests) && !isStatus(err, http.StatusUnprocessableEntity) {
		return apiTopic{}, nil, 0, fmt.Errorf("fetch topic: %w", err)
	}
	e.logger.Info("discourse print view rate limited, falling back to batched posts", "topic", id, "host", base)
	return e.fetchTopicBatched(ctx, base, id)
}

// fetchTopicBatched implements the fallback route described on fetchTopic.
func (e *Engine) fetchTopicBatched(ctx context.Context, base, id string) (apiTopic, []Post, int, error) {
	raw, err := e.get(ctx, base+"/t/"+id+".json?include_raw=1")
	if err != nil {
		return apiTopic{}, nil, 0, fmt.Errorf("fetch topic: %w", err)
	}
	var at apiTopic
	if jerr := json.Unmarshal(raw, &at); jerr != nil {
		return apiTopic{}, nil, 0, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse topic: %w", jerr)}
	}

	byID := make(map[int64]apiPost, len(at.PostStream.Posts))
	for _, p := range at.PostStream.Posts {
		byID[p.ID] = p
	}

	stream := at.PostStream.Stream
	if len(stream) == 0 {
		// No index to page through (shouldn't happen on a real forum): emit what
		// the first chunk gave us rather than failing.
		total := max(at.PostsCount, len(at.PostStream.Posts))
		return at, convert(capPosts(at.PostStream.Posts)), total, nil
	}
	total := max(at.PostsCount, len(stream))

	wanted := stream
	if len(wanted) > maxPosts {
		wanted = wanted[:maxPosts]
	}
	var missing []int64
	for _, pid := range wanted {
		if _, ok := byID[pid]; !ok {
			missing = append(missing, pid)
		}
	}
	for start := 0; start < len(missing); start += postIDsPerBatch {
		batch := missing[start:min(start+postIDsPerBatch, len(missing))]
		q := url.Values{}
		for _, pid := range batch {
			q.Add("post_ids[]", strconv.FormatInt(pid, 10))
		}
		q.Set("include_raw", "1")
		braw, berr := e.get(ctx, base+"/t/"+id+"/posts.json?"+q.Encode())
		if berr != nil {
			return apiTopic{}, nil, 0, fmt.Errorf("fetch posts batch: %w", berr)
		}
		var bt apiTopic
		if jerr := json.Unmarshal(braw, &bt); jerr != nil {
			return apiTopic{}, nil, 0, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse posts batch: %w", jerr)}
		}
		for _, p := range bt.PostStream.Posts {
			byID[p.ID] = p
		}
	}

	// Reassemble in stream order — the batches come back grouped, not interleaved.
	ordered := make([]apiPost, 0, len(wanted))
	for _, pid := range wanted {
		if p, ok := byID[pid]; ok {
			ordered = append(ordered, p)
		}
	}
	return at, convert(ordered), total, nil
}

// capPosts trims a post list to the emission budget.
func capPosts(posts []apiPost) []apiPost {
	if len(posts) > maxPosts {
		return posts[:maxPosts]
	}
	return posts
}

// convert maps wire posts to output posts, preferring the author's original
// markdown (raw) over the rendered HTML (cooked).
func convert(posts []apiPost) []Post {
	out := make([]Post, 0, len(posts))
	for _, p := range posts {
		body := p.Raw
		if body == "" {
			body = stripHTML(p.Cooked)
		}
		out = append(out, Post{
			Number:  p.PostNumber,
			Login:   p.Username,
			Created: p.CreatedAt,
			ReplyTo: p.ReplyToPostNumber,
			Body:    body,
		})
	}
	return out
}

// HTML-stripping patterns for the cooked fallback. This is deliberately a
// minimal tag stripper, not an HTML→markdown converter: `raw` is present on
// every response this engine makes (include_raw=1), so cooked is only reached
// for the rare post where the forum withholds it, and a new module dependency
// for that path isn't worth it.
var (
	scriptStyleRE = regexp.MustCompile(`(?is)<(script|style)\b[^>]*>.*?</(script|style)\s*>`)
	brRE          = regexp.MustCompile(`(?i)<br\s*/?>`)
	blockCloseRE  = regexp.MustCompile(`(?i)</(p|div|li|ul|ol|h[1-6]|blockquote|pre|tr|table)\s*>`)
	anyTagRE      = regexp.MustCompile(`(?s)<[^>]*>`)
	blankLinesRE  = regexp.MustCompile(`\n{3,}`)
)

// stripHTML reduces a cooked post body to plain text: script/style content is
// dropped, <br> and block-element ends become line breaks, remaining tags are
// removed, and entities are unescaped.
func stripHTML(s string) string {
	s = scriptStyleRE.ReplaceAllString(s, "")
	s = brRE.ReplaceAllString(s, "\n")
	s = blockCloseRE.ReplaceAllString(s, "\n\n")
	s = anyTagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = blankLinesRE.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// isStatus reports whether err is a FetchError carrying the given HTTP status.
func isStatus(err error, status int) bool {
	var fe *domain.FetchError
	return errors.As(err, &fe) && fe.StatusCode == status
}

// get fetches a Discourse JSON URL and returns the raw body. A 429 is never
// retried: on the print view it is the fallback signal, and elsewhere Discourse's
// anonymous rate limits are invisible to us, so an honest error beats hammering.
func (e *Engine) get(ctx context.Context, apiURL string) ([]byte, error) {
	headers := map[string]string{
		"Accept":     "application/json",
		"User-Agent": userAgent,
	}
	resp, err := e.client.DoRetry(ctx, http.MethodGet, apiURL, nil, headers, httpx.RetryConfig{
		RetryableStatus: func(status int, _ string) bool { return status != http.StatusTooManyRequests },
	})
	if err != nil {
		return nil, httpx.ClassifyClientError(err, domain.KindUpstreamError)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &domain.FetchError{
			Kind:       domain.KindForStatus(resp.StatusCode),
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("discourse api returned %d", resp.StatusCode),
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
		"engine":              "discourse",
		"status_code":         "200",
		domain.ContentTypeKey: domain.ContentTypeTOON,
	}
	for k, val := range extra {
		meta[k] = val
	}
	e.logger.Info("discourse crawl complete", "source", source, "bytes", len(encoded))
	return domain.Document{PageContent: string(encoded), Metadata: meta}, nil
}
