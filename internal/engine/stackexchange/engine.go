package stackexchange

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"maps"
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
	defaultAPIBase = "https://api.stackexchange.com/2.3"
	defaultTimeout = 30 * time.Second // wall-clock budget per Stack Exchange crawl
	maxAnswers     = 100              // one page at the API's maximum page size; caps a megathread for the consumer's context
	userAgent      = "omnifeed"
	bodyLimit      = 20 << 20 // 20MB read cap per response
)

// The API's error_id values this engine tells apart (docs: /docs/error-handling).
// Every error arrives as HTTP 400; error_id is what says which one it is.
const (
	errInternal          = 500 // internal_error
	errThrottleViolation = 502 // throttle_violation: the IP's daily quota or the per-second throttle
	errUnavailable       = 503 // temporarily_unavailable
)

// questionRE claims the question path shapes: /questions/{id}, with an
// optional slug and an optional trailing /{answerId} (the answer permalink
// form), and the share links /q/{id} and /a/{answerId}, each with the optional
// trailing /{userId} the share button appends. Everything else on a Stack
// Exchange host (/questions/tagged/…, /questions/ask, /users/…, /tags, /search)
// falls through. Groups: 1 = question id, 2 = answer id from the permalink,
// 3 = short-link kind (q|a), 4 = its id.
var questionRE = regexp.MustCompile(`^/(?:questions/([0-9]+)(?:/[^/]*(?:/([0-9]+))?)?|(q|a)/([0-9]+)(?:/[0-9]+)?)/?$`)

// throttleWaitRE reads the wait out of a throttle_violation message ("too many
// requests from this IP, more requests available in 79007 seconds").
var throttleWaitRE = regexp.MustCompile(`available in ([0-9]+) seconds?`)

// siteHosts maps the network's own-domain sites to their API site parameter.
// Every other site is {name}.stackexchange.com, whose parameter is {name}.
var siteHosts = map[string]string{
	"stackoverflow.com": "stackoverflow",
	"superuser.com":     "superuser",
	"serverfault.com":   "serverfault",
	"askubuntu.com":     "askubuntu",
	"mathoverflow.net":  "mathoverflow",
}

// nonSiteSubdomains are stackexchange.com hosts that are infrastructure, not a
// Q&A site, so {name}.stackexchange.com must not claim them.
var nonSiteSubdomains = map[string]bool{"api": true, "chat": true, "data": true, "openid": true}

// Engine implements domain.Engine for Stack Exchange question URLs via the
// public Stack Exchange API.
type Engine struct {
	client  *httpx.Client
	limiter httpx.Limiter
	apiBase string
	key     string
	timeout time.Duration
	logger  *slog.Logger
}

// Config configures a Stack Exchange Engine.
type Config struct {
	Client  *httpx.Client
	Limiter httpx.Limiter
	APIBase string        // defaults to the public v2.3 API; overridden in tests
	Key     string        // optional app key; empty = keyless (300 requests/day/IP)
	Timeout time.Duration // wall-clock budget per crawl; defaults to defaultTimeout
	Logger  *slog.Logger
}

// New returns a Stack Exchange Engine configured per cfg.
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
		client:  cfg.Client.WithUpstream("stackexchange", "api"),
		limiter: cfg.Limiter,
		apiBase: strings.TrimRight(cfg.APIBase, "/"),
		key:     cfg.Key,
		timeout: cfg.Timeout,
		logger:  cfg.Logger,
	}
}

// Name returns the engine identifier ("stackexchange").
func (*Engine) Name() string { return "stackexchange" }

// Matches claims question URLs on the Stack Exchange network only (see
// questionRE and siteFor). Every other page on those hosts falls through to the
// generic crawl4ai fallback.
func (*Engine) Matches(rawURL string) bool {
	_, ok := parseTarget(rawURL)
	return ok
}

// target is the resolved fetch plan for a question URL. Exactly one of
// questionID and answerID is set: a short /a/ link names only the answer, and
// its question is looked up first.
type target struct {
	host       string // hostname without www., for the output
	site       string // API site parameter
	questionID string
	answerID   string
}

// parseTarget classifies a Stack Exchange URL into a fetch plan. ok is false
// for any URL this engine doesn't render (so it falls through).
func parseTarget(rawURL string) (target, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return target{}, false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	site, ok := siteFor(host)
	if !ok {
		return target{}, false
	}
	m := questionRE.FindStringSubmatch(u.Path)
	if m == nil {
		return target{}, false
	}
	t := target{host: host, site: site}
	switch {
	case m[1] != "":
		t.questionID = m[1] // /questions/{id}[/{slug}[/{answerId}]]: the question is known
	case m[3] == "q":
		t.questionID = m[4]
	default:
		t.answerID = m[4]
	}
	return t, true
}

// siteFor maps a host (www. already stripped) to its API site parameter. A
// meta site takes the parameter of its parent prefixed with "meta."
// (meta.stackoverflow.com → meta.stackoverflow, meta.unix.stackexchange.com →
// meta.unix); meta.stackexchange.com is itself the {name} form, "meta".
func siteFor(host string) (string, bool) {
	if s, ok := siteHosts[host]; ok {
		return s, true
	}
	if rest, ok := strings.CutPrefix(host, "meta."); ok {
		if s, ok := siteHosts[rest]; ok {
			return "meta." + s, true
		}
		if name, ok := strings.CutSuffix(rest, ".stackexchange.com"); ok && isLabel(name) && !nonSiteSubdomains[name] {
			return "meta." + name, true
		}
	}
	name, ok := strings.CutSuffix(host, ".stackexchange.com")
	if !ok || !isLabel(name) || nonSiteSubdomains[name] {
		return "", false
	}
	return name, true
}

// isLabel reports whether s is a single DNS label (no dots, non-empty).
func isLabel(s string) bool { return s != "" && !strings.Contains(s, ".") }

// Crawl fetches the question behind rawURL and its answers from the Stack
// Exchange API and returns them as TOON headed by a "# title" line.
func (e *Engine) Crawl(ctx context.Context, rawURL string, _ domain.EngineOptions) (domain.Document, error) {
	t, ok := parseTarget(rawURL)
	if !ok {
		return domain.Document{}, fmt.Errorf("unsupported stackexchange url: %s", rawURL)
	}

	// Bound wall-clock independently of the shared HTTP client timeout (which is
	// the crawl4ai knob); this engine talks to api.stackexchange.com directly.
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()

	// One limiter slot covers the whole crawl (two or three sequential calls).
	// A backoff the API asked for is a hold on this same key (see holdOff), so
	// the next crawl waits it out here.
	if e.limiter != nil {
		release, lerr := e.limiter.Acquire(ctx, e.Name(), e.apiBase)
		if lerr != nil {
			// Classified so a pacing refusal reaches the caller as quota_exhausted
			// with its retry_after_s, not as an opaque "error".
			return domain.Document{}, httpx.ClassifyClientError(lerr, domain.KindError)
		}
		defer release()
	}

	qid := t.questionID
	if qid == "" {
		var err error
		if qid, err = e.questionOfAnswer(ctx, t); err != nil {
			return domain.Document{}, err
		}
	}

	qs, err := get[apiQuestion](ctx, e, t.site, "/questions/"+qid, url.Values{"filter": {"withbody"}})
	if err != nil {
		return domain.Document{}, fmt.Errorf("fetch question: %w", err)
	}
	if len(qs.Items) == 0 {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindNotFound, StatusCode: http.StatusNotFound,
			Err: fmt.Errorf("question %s not found on %s (deleted or never existed)", qid, t.host)}
	}
	q := qs.Items[0]

	var answers []Answer
	quota, more := qs.QuotaRemaining, false
	if q.AnswerCount > 0 {
		as, err := get[apiAnswer](ctx, e, t.site, "/questions/"+qid+"/answers", url.Values{
			"filter":   {"withbody"},
			"sort":     {"votes"},
			"order":    {"desc"},
			"pagesize": {strconv.Itoa(maxAnswers)},
		})
		if err != nil {
			return domain.Document{}, fmt.Errorf("fetch answers: %w", err)
		}
		quota, more = as.QuotaRemaining, as.HasMore
		answers = convertAnswers(as.Items, t.host)
	}

	meta := map[string]string{
		"answers":         strconv.Itoa(len(answers)),
		"quota_remaining": strconv.Itoa(quota),
	}
	th := Thread{Question: convertQuestion(q, t.host), Answers: answers}
	if total := max(q.AnswerCount, len(answers)); more || total > len(answers) {
		meta["truncated_from"] = strconv.Itoa(total)
		th.Note = fmt.Sprintf("answer list truncated: showing the top %d of %d answers by votes", len(answers), total)
	}
	return e.document(th, th.Question.Title, rawURL, meta)
}

// questionOfAnswer resolves a short /a/{id} link to its question id through
// GET /answers/{id} (default filter: no body, just the ids).
func (e *Engine) questionOfAnswer(ctx context.Context, t target) (string, error) {
	as, err := get[apiAnswer](ctx, e, t.site, "/answers/"+t.answerID, nil)
	if err != nil {
		return "", fmt.Errorf("fetch answer: %w", err)
	}
	if len(as.Items) == 0 || as.Items[0].QuestionID == 0 {
		return "", &domain.FetchError{Kind: domain.KindNotFound, StatusCode: http.StatusNotFound,
			Err: fmt.Errorf("answer %s not found on %s (deleted or never existed)", t.answerID, t.host)}
	}
	return strconv.FormatInt(as.Items[0].QuestionID, 10), nil
}

// convertQuestion maps the wire question to the output header. Titles and
// display names arrive HTML-encoded; the body is HTML.
func convertQuestion(q apiQuestion, host string) Question {
	return Question{
		Title:          html.UnescapeString(q.Title),
		Site:           host,
		Score:          q.Score,
		Tags:           q.Tags,
		Author:         html.UnescapeString(q.Owner.DisplayName),
		Asked:          isoTime(q.CreationDate),
		Answers:        q.AnswerCount,
		AcceptedAnswer: q.AcceptedAnswerID,
		Closed:         q.ClosedReason,
		Body:           htmlToMarkdown(q.Body, host),
	}
}

// convertAnswers maps wire answers to output answers, keeping the API's vote
// order.
func convertAnswers(in []apiAnswer, host string) []Answer {
	out := make([]Answer, 0, len(in))
	for _, a := range in {
		out = append(out, Answer{
			ID:       a.AnswerID,
			Score:    a.Score,
			Accepted: a.IsAccepted,
			Author:   html.UnescapeString(a.Owner.DisplayName),
			Created:  isoTime(a.CreationDate),
			Body:     htmlToMarkdown(a.Body, host),
		})
	}
	return out
}

// isoTime renders an API unix timestamp as RFC 3339 UTC; "" for none.
func isoTime(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).UTC().Format(time.RFC3339)
}

// get calls one API method on site and decodes its response wrapper. A backoff in the response, error or not, is
// handed to the limiter (holdOff) before anything else, so the next crawl
// honours it. An API error is classified by its error_id (classifyAPIError).
func get[T any](ctx context.Context, e *Engine, site, path string, params url.Values) (apiWrapper[T], error) {
	q := url.Values{}
	maps.Copy(q, params)
	q.Set("site", site)
	if e.key != "" {
		q.Set("key", e.key)
	}
	apiURL := e.apiBase + path + "?" + q.Encode()

	// No Accept-Encoding header: net/http then asks for gzip itself and
	// decodes a response that declares it. The API compresses every response,
	// so decode below covers one that arrives gzipped without saying so.
	headers := map[string]string{
		"Accept":     "application/json",
		"User-Agent": userAgent,
	}
	resp, err := e.client.DoRetry(ctx, http.MethodGet, apiURL, nil, headers, httpx.RetryConfig{})
	if err != nil {
		return apiWrapper[T]{}, httpx.ClassifyClientError(err, domain.KindUpstreamError)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, bodyLimit))
	if err != nil {
		return apiWrapper[T]{}, httpx.ClassifyClientError(err, domain.KindUpstreamError)
	}
	raw, err = decode(raw)
	if err != nil {
		return apiWrapper[T]{}, &domain.FetchError{Kind: domain.KindBadResponse, StatusCode: resp.StatusCode, Err: fmt.Errorf("decompress: %w", err)}
	}
	var w apiWrapper[T]
	if jerr := json.Unmarshal(raw, &w); jerr != nil {
		if resp.StatusCode != http.StatusOK {
			// Not the API's own error wrapper: something in front of it (a CDN
			// page, a load balancer) answered.
			return apiWrapper[T]{}, &domain.FetchError{
				Kind:       domain.KindForStatus(resp.StatusCode),
				StatusCode: resp.StatusCode,
				Err:        fmt.Errorf("stackexchange api returned %d", resp.StatusCode),
			}
		}
		return apiWrapper[T]{}, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("parse response: %w", jerr)}
	}
	if w.Backoff > 0 {
		e.holdOff(ctx, apiURL, time.Duration(w.Backoff)*time.Second)
	}
	if w.ErrorID != 0 || resp.StatusCode != http.StatusOK {
		ferr := classifyAPIError(resp.StatusCode, w.ErrorID, w.ErrorName, w.ErrorMessage)
		if ferr.RetryAfter > 0 {
			e.holdOff(ctx, apiURL, ferr.RetryAfter)
		}
		return apiWrapper[T]{}, ferr
	}
	return w, nil
}

// holdOff tells the limiter that the API asked for a pause. The API puts its
// backoff in the JSON body instead of a Retry-After header, so the retry
// client cannot see it; this reports it through the same client hook
// (httpx.Client.OnRetryAfter), which main.go wires to the domain limiter's
// Penalize — the next crawl then waits for the api.stackexchange.com slot
// (or fails fast as quota_exhausted with its retry_after_s) instead of calling
// the method again too soon. Backoff is per method upstream; the hold covers
// the whole host, which is stricter, never looser.
func (e *Engine) holdOff(ctx context.Context, apiURL string, d time.Duration) {
	e.logger.InfoContext(ctx, "stackexchange api asked for a backoff", "backoff_s", int(d/time.Second))
	if e.client != nil && e.client.OnRetryAfter != nil {
		e.client.OnRetryAfter("stackexchange", apiURL, d)
	}
}

// gzipMagic is the first two bytes of every gzip stream.
var gzipMagic = []byte{0x1f, 0x8b}

// decode returns raw as plain bytes. The API compresses every response, and
// tells clients to assume gzip when Content-Encoding is missing; net/http has
// already decoded a response that declared it.
func decode(raw []byte) ([]byte, error) {
	if !bytes.HasPrefix(raw, gzipMagic) {
		return raw, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(io.LimitReader(zr, bodyLimit))
}

// classifyAPIError maps an API error onto a FetchError. The HTTP status is
// 400 for every API error, so error_id decides: a throttle violation is rate
// limiting (with the wait the message names, when it names one), an internal
// or temporary failure is an upstream fault, and anything else — a bad
// parameter, an unknown method — is not something a retry fixes.
func classifyAPIError(status, id int, name, message string) *domain.FetchError {
	err := fmt.Errorf("stackexchange api error %d %s: %s", id, name, html.UnescapeString(message))
	switch id {
	case errThrottleViolation:
		fe := &domain.FetchError{Kind: domain.KindHTTP429, StatusCode: status, Err: err}
		if m := throttleWaitRE.FindStringSubmatch(message); m != nil {
			if secs, perr := strconv.Atoi(m[1]); perr == nil {
				fe.RetryAfter = time.Duration(secs) * time.Second
			}
		}
		return fe
	case errInternal, errUnavailable:
		return &domain.FetchError{Kind: domain.KindUpstreamError, StatusCode: status, Err: err}
	case 0:
		// A non-200 that still parsed as a wrapper but carries no error_id.
		return &domain.FetchError{Kind: domain.KindForStatus(status), StatusCode: status,
			Err: fmt.Errorf("stackexchange api returned %d", status)}
	default:
		return &domain.FetchError{Kind: domain.KindError, StatusCode: status, Err: err}
	}
}

// document encodes v as TOON, heads it with a "# title" markdown line (so a
// reader, human or program, gets the page title without parsing the TOON), and
// wraps it in a Document with standard metadata.
func (e *Engine) document(v any, title, source string, extra map[string]string) (domain.Document, error) {
	encoded, err := toon.Marshal(v)
	if err != nil {
		return domain.Document{}, fmt.Errorf("encode: %w", err)
	}
	content := string(encoded)
	if title = strings.Join(strings.Fields(title), " "); title != "" {
		content = "# " + title + "\n\n" + content
	}
	meta := map[string]string{
		"source":              source,
		"engine":              "stackexchange",
		"status_code":         "200",
		domain.ContentTypeKey: domain.ContentTypeTOON,
	}
	maps.Copy(meta, extra)
	e.logger.Info("stackexchange crawl complete", "source", source, "bytes", len(content), "quota_remaining", extra["quota_remaining"])
	return domain.Document{PageContent: content, Metadata: meta}, nil
}
