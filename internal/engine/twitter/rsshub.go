package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/linkfmt"
	"github.com/kinorai/omnifeed/internal/observability"
)

// X timelines (home, following, profiles, search) come from RSSHub alone.
// Measured from one IP, 15 calls each: FxTwitter served a profile 5-9 times
// and a search never; an RSSHub with an X auth_token served all four routes
// 15 times. RSSHub items carry no engagement counts, so each post is then
// loaded through the post chain (fetchBundle), which has them.
const (
	sourceRSSHub = "rsshub"
	// rsshubTimeout bounds one RSSHub request: it calls X itself and answers
	// in seconds, slower than the JSON mirrors.
	rsshubTimeout = 30 * time.Second
	// hydrateWorkers is how many posts of a timeline load at once. The post
	// chain is paced by the engine's limiter (OMNIFEED_TWITTER_CONCURRENCY,
	// default 4, per host) anyway; this bounds the burst.
	hydrateWorkers = 4
)

// errNoRSSHub is the answer to a timeline URL when no RSSHub is configured.
// Final: crawl4ai only gets X's logged-out shell for these pages.
var errNoRSSHub = errors.New("X timelines need an RSSHub instance: set OMNIFEED_TWITTER_RSSHUB_URL") //nolint:staticcheck // "X" is the product name

// rsshubFeed is the JSON Feed 1.1 RSSHub returns for ?format=json, reduced to
// what a timeline needs.
type rsshubFeed struct {
	Items []rsshubItem `json:"items"`
}

type rsshubItem struct {
	URL           string `json:"url"`
	ContentHTML   string `json:"content_html"`
	DatePublished string `json:"date_published"`
	Authors       []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"authors"`
	// Extra links the post a repost, quote or reply points at. For a repost
	// the item's own url is the repost; the "repost" link is the original.
	Extra *struct {
		Links []struct {
			URL  string `json:"url"`
			Type string `json:"type"`
		} `json:"links"`
	} `json:"_extra"`
}

// rsshubPath is the RSSHub route for a timeline target.
func rsshubPath(t target) string {
	switch t.kind {
	case kindHome:
		return "/twitter/home"
	case kindFollowing:
		return "/twitter/home_latest"
	case kindProfile:
		return "/twitter/user/" + url.PathEscape(t.handle)
	default: // kindSearch: X operators (from:, since:, min_faves:) pass verbatim
		return "/twitter/keyword/" + url.PathEscape(t.query)
	}
}

// crawlTimeline reads a timeline from RSSHub, loads every post through the
// post chain for its counts and media, and renders the list.
func (e *Engine) crawlTimeline(ctx context.Context, rawURL string, t target, opts domain.EngineOptions) (domain.Document, error) {
	if e.rsshubBase == "" {
		return domain.Document{}, domain.NoFallback(&domain.FetchError{Kind: domain.KindError, Err: errNoRSSHub})
	}
	items, err := e.fetchRSSHub(ctx, rsshubPath(t))
	if err != nil {
		return domain.Document{}, domain.NoFallback(err)
	}
	posts, failed, firstErr := e.hydrate(ctx, items)
	if ctx.Err() != nil {
		return domain.Document{}, httpx.ClassifyClientError(ctx.Err(), domain.KindUpstreamError)
	}
	return e.timelineDocument(t, posts, failed, firstErr, rawURL, opts)
}

// fetchRSSHub GETs one RSSHub route as JSON Feed, under the per-domain
// limiter. The access key travels as the key parameter; it never reaches an
// error, a log line or the limiter.
func (e *Engine) fetchRSSHub(ctx context.Context, path string) ([]rsshubItem, error) {
	ctx, cancel := context.WithTimeout(ctx, rsshubTimeout)
	defer cancel()
	base := e.rsshubBase + path
	q := url.Values{"format": {"json"}}
	if e.rsshubKey != "" {
		q.Set("key", e.rsshubKey)
	}
	if e.limiter != nil {
		release, err := e.limiter.Acquire(ctx, e.Name(), base)
		if err != nil {
			return nil, httpx.ClassifyClientError(err, domain.KindUpstreamError)
		}
		defer release()
	}
	// One attempt: RSSHub calls X with its owner's token, and its errors
	// (an X rate limit, a missing credential) do not clear in a second.
	resp, err := e.rsshub.DoRetry(ctx, http.MethodGet, base+"?"+q.Encode(), nil,
		map[string]string{"Accept": "application/feed+json, application/json"}, httpx.RetryConfig{MaxAttempts: 1})
	if se, ok := errors.AsType[*httpx.StatusError](err); ok {
		// DoRetry hands a 429 or 5xx back as an error, with no response.
		return nil, rsshubStatusError(se.StatusCode)
	}
	if err != nil {
		return nil, httpx.ClassifyClientError(redactURL(err, base), domain.KindUpstreamError)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20MB cap
	if err != nil {
		return nil, httpx.ClassifyClientError(redactURL(err, base), domain.KindUpstreamError)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, rsshubStatusError(resp.StatusCode)
	}
	var feed rsshubFeed
	if err := json.Unmarshal(body, &feed); err != nil {
		return nil, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("rsshub: decode JSON Feed: %w", err)}
	}
	return feed.Items, nil
}

// rsshubStatusError classifies an RSSHub error status. RSSHub answers 503
// for a route that failed upstream (X refused, a credential is missing), 403
// for a wrong access key and 404 for an unknown account or route.
func rsshubStatusError(status int) error {
	kind := domain.KindForStatus(status)
	msg := fmt.Sprintf("rsshub returned %d", status)
	switch status {
	case http.StatusForbidden:
		msg += ": access denied, check OMNIFEED_TWITTER_RSSHUB_KEY"
	case http.StatusNotFound:
		kind = domain.KindNotFound
		msg += ": no such account or route"
	case http.StatusServiceUnavailable:
		msg += ": the route failed on X's side (a rate limit, an expired TWITTER_AUTH_TOKEN, or X changed " +
			"its request signing and RSSHub needs an update), see the RSSHub log"
	}
	return &domain.FetchError{Kind: kind, StatusCode: status, Err: errors.New(msg)}
}

// redactURL rewrites the request URL inside a transport error to base (the
// URL without its query), so the access key never reaches a message.
func redactURL(err error, base string) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Kept classifiable; the inner error carries no URL.
		return ue.Err
	}
	return &url.Error{Op: ue.Op, URL: base, Err: ue.Err}
}

// timelinePost is one timeline entry: the post, and whether the post chain
// loaded it (with its counts) or it is the bare RSSHub item.
type timelinePost struct {
	post
	hydrated bool
}

// hydrate turns RSSHub items into posts, in RSSHub's order, loading each one
// through the post chain with a few workers. A post two sources call gone
// (deleted, protected, withheld) is dropped. A post the chain cannot load for
// any other reason (a rate limit, an outage) keeps what the item has (text,
// author, date) without counts; failed counts those, firstErr is the first
// one's error.
func (e *Engine) hydrate(ctx context.Context, items []rsshubItem) (posts []timelinePost, failed int, firstErr error) {
	seen := map[string]bool{}
	for _, it := range items {
		p, ok := itemPost(it)
		if !ok || seen[p.ID] {
			continue // not a post URL, or a second repost of the same post
		}
		seen[p.ID] = true
		posts = append(posts, timelinePost{post: p})
	}

	errs := make([]error, len(posts))
	gone := make([]bool, len(posts))
	sem := make(chan struct{}, hydrateWorkers)
	var wg sync.WaitGroup
	for i := range posts {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			b, err := e.fetchBundle(ctx, posts[i].ID)
			if c, ok := errors.AsType[*chainError](err); ok && c.notFound() {
				gone[i] = true
				return
			}
			if err != nil {
				errs[i] = err
				return
			}
			posts[i] = timelinePost{post: b.Post, hydrated: true}
		})
	}
	wg.Wait()
	kept := posts[:0]
	for i, p := range posts {
		if gone[i] {
			continue
		}
		kept = append(kept, p)
		if errs[i] == nil {
			continue
		}
		failed++
		if firstErr == nil {
			firstErr = errs[i]
		}
	}
	return kept, failed, firstErr
}

// itemPost is the bare post an RSSHub item describes. A repost names the
// original post (its "repost" link), which is what the timeline shows; a
// reply names the post it answers (its "reply" link).
func itemPost(it rsshubItem) (post, bool) {
	link, repost := it.URL, false
	var replyTo string
	if it.Extra != nil {
		for _, l := range it.Extra.Links {
			switch {
			case l.URL == "":
			case l.Type == "repost":
				link, repost = l.URL, true
			case l.Type == "reply":
				replyTo = l.URL
			}
		}
	}
	p, ok := statusRef(link)
	if !ok {
		return post{}, false
	}
	if r, ok := statusRef(replyTo); ok && !repost {
		p.ReplyToID, p.ReplyTo = r.ID, r.Handle
	}
	p.Text = htmlText(it.ContentHTML, p.ReplyTo != "")
	// On a repost the item's author and date are the reposter's, so only an
	// original post takes them.
	if !repost {
		if len(it.Authors) > 0 {
			p.Name = it.Authors[0].Name
		}
		if ts, err := time.Parse(time.RFC3339, it.DatePublished); err == nil {
			p.Created = ts.UTC().Format(time.RFC3339)
		}
	}
	return p, true
}

// statusRef is the id and author handle of an x.com post URL.
func statusRef(link string) (post, bool) {
	u, err := url.Parse(link)
	if err != nil || link == "" {
		return post{}, false
	}
	m := statusPath.FindStringSubmatch(u.Path)
	if m == nil {
		return post{}, false
	}
	p := post{ID: m[1]}
	if h, _, ok := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/"); ok && h != "i" {
		p.Handle = h
	}
	return p, true
}

var (
	htmlAnchor = regexp.MustCompile(`(?is)<a\s[^>]*?href="([^"]*)"[^>]*>(.*?)</a>`)
	htmlBreak  = regexp.MustCompile(`(?i)<br\s*/?>`)
	htmlTag    = regexp.MustCompile(`<[^>]*>`)
	manyBlank  = regexp.MustCompile(`\n{3,}`)
)

// htmlText reduces an RSSHub item body to plain text: line breaks kept, a
// link's target kept (see keepHref), other tags (images, the quote box)
// dropped, entities decoded. reply drops the @handles X prepends to a reply.
func htmlText(s string, reply bool) string {
	s = htmlAnchor.ReplaceAllStringFunc(s, keepHref)
	s = htmlBreak.ReplaceAllString(s, "\n")
	s = htmlTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	return cleanText(manyBlank.ReplaceAllString(s, "\n\n"), reply)
}

// keepHref renders one <a> as its text and target (linkfmt), so a link X
// shows shortened keeps its full URL. @mentions and #hashtags keep their text
// alone: their targets are x.com pages, and the text already names them. The
// result is re-escaped, since htmlText unescapes the whole body once after.
func keepHref(a string) string {
	m := htmlAnchor.FindStringSubmatch(a)
	href := strings.TrimSpace(html.UnescapeString(m[1]))
	text := strings.TrimSpace(html.UnescapeString(htmlTag.ReplaceAllString(m[2], "")))
	if strings.HasPrefix(text, "@") || strings.HasPrefix(text, "#") || strings.HasPrefix(text, "$") {
		return html.EscapeString(text)
	}
	return html.EscapeString(linkfmt.Link(text, href))
}

// timelineDocument renders a timeline and attaches its metadata. The home
// and following timelines are the RSSHub owner's own feed: marked private, so
// the shared response cache never stores them.
func (e *Engine) timelineDocument(t target, posts []timelinePost, failed int, firstErr error, rawURL string, opts domain.EngineOptions) (domain.Document, error) {
	tl := Timeline{Kind: t.kind.String(), Handle: t.handle, Query: t.query}
	content, contentType, err := encode(opts,
		func() any {
			tl.Posts = make([]OutPost, 0, len(posts))
			for _, p := range posts {
				o := outPost(p.post)
				o.CountsMissing = !p.hydrated
				tl.Posts = append(tl.Posts, o)
			}
			return tl
		},
		func() string { return renderTimeline(tl, posts) })
	if err != nil {
		return domain.Document{}, err
	}

	meta := map[string]string{
		"source":              rawURL,
		"status_code":         "200",
		domain.ContentTypeKey: contentType,
		"kind":                tl.Kind,
		"upstream":            sourceRSSHub,
		"posts":               strconv.Itoa(len(posts)),
	}
	if tl.Handle != "" {
		meta["author"] = tl.Handle
	}
	if tl.Query != "" {
		meta["query"] = tl.Query
	}
	if t.kind == kindHome || t.kind == kindFollowing {
		meta[domain.PrivateKey] = "true"
	}
	if failed > 0 {
		meta[domain.PartialKey] = "true"
		meta[domain.PartialReasonKey] = observability.Reason(classifyHydration(firstErr))
		meta["unhydrated_posts"] = strconv.Itoa(failed)
		e.logger.Warn("twitter timeline posts kept without counts", "kind", tl.Kind, "failed", failed,
			"posts", len(posts), "err", observability.Explain(firstErr))
	}
	e.logger.Info("twitter crawl complete", "source", rawURL, "upstream", sourceRSSHub, "kind", tl.Kind,
		"posts", len(posts), "bytes", len(content))
	return domain.Document{PageContent: content, Metadata: meta}, nil
}

// classifyHydration turns a post chain failure into its classified error.
func classifyHydration(err error) error {
	if c, ok := errors.AsType[*chainError](err); ok {
		return c.classify()
	}
	return err
}

// renderTimeline lays a timeline out for an LLM reader: a title line, then
// each post as its header line (counts only when the post chain loaded
// them), a reply marker when it answers someone else, and its body.
func renderTimeline(tl Timeline, posts []timelinePost) string {
	var w strings.Builder
	n := plural(len(posts), "post")
	switch tl.Kind {
	case kindHome.String():
		fmt.Fprintf(&w, "# X home timeline: For you (%s)\n", n)
	case kindFollowing.String():
		fmt.Fprintf(&w, "# X home timeline: Following (%s)\n", n)
	case kindSearch.String():
		fmt.Fprintf(&w, "# X search: %s (%s)\n", tl.Query, n)
	default:
		fmt.Fprintf(&w, "# X posts by @%s (%s)\n", tl.Handle, n)
	}
	if len(posts) == 0 {
		w.WriteString("\nNo posts.\n")
	}
	for _, p := range posts {
		w.WriteString("\n---\n\n" + header(p.post, p.hydrated) + "\n")
		if p.ReplyTo != "" && !sameHandle(p.ReplyTo, p.Handle) {
			w.WriteString("Replying to @" + p.ReplyTo + "\n")
		}
		if body := postBody(p.post); body != "" {
			w.WriteString("\n" + body + "\n")
		}
	}
	return w.String()
}
