// Package crawl4ai implements the fallback engine: dispatches generic URLs
// to an upstream crawl4ai instance and reshapes the response into the
// canonical Document.
package crawl4ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/antibot"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// maxPageTimeoutMS is the highest page_timeout crawl4ai honours from a REST
// body: it clamps untrusted values to 60000 ms without saying so, so sending
// more only misleads whoever reads the payload.
const maxPageTimeoutMS = 60000

// DefaultExcludedSelector is the conservative chrome selector list sent as
// crawl4ai's excluded_selector when the operator hasn't set one. It names only
// chrome-shaped classes/ids (sidebars, tables of contents, related-post and
// newsletter boxes, cookie banners). On the rare page whose main content IS one
// of these (a docs index living in `#toc`, say), the crawl comes back empty —
// Crawl retries once without the selector rather than erroring, so the default
// can stay aggressive.
const DefaultExcludedSelector = ".sidebar,.toc,#toc,.related,.newsletter,.cookie-banner,[aria-label*='cookie']"

// Engine sends URLs to crawl4ai's /crawl endpoint and extracts the best-fit
// markdown body. It is registered as the Registry fallback.
type Engine struct {
	endpoint       string
	token          string
	client         *httpx.Client
	limiter        *httpx.DomainLimiter
	keepLinks      bool
	pruneThreshold float64
	waitUntil      string

	excludedSelector string
	targetElements   []string

	scanFullPage    bool
	scrollDelay     float64
	delayBeforeHTML float64
	removeOverlays  bool

	// direct fetches raw non-HTML text without the browser (see rawtext.go).
	// Its underlying http.Client refuses private/reserved dials post-DNS when
	// the SSRF guard is on. Nil disables the bypass.
	direct *httpx.Client
}

// Config configures the crawl4ai Engine.
type Config struct {
	Endpoint string
	// Token, when set, is sent as `Authorization: Bearer <token>` on every crawl4ai
	// request — required when the upstream runs with CRAWL4AI_API_TOKEN (crawl4ai
	// 0.9.x binds non-loopback only when a token is set). The default is owned by
	// config (OMNIFEED_CRAWL4AI_TOKEN); empty sends no Authorization header.
	Token   string
	Client  *httpx.Client
	Limiter *httpx.DomainLimiter
	// KeepLinks renders hyperlink anchor text and retains external links in the
	// extracted markdown. When false, both are stripped for leaner output. The
	// default is owned by config (OMNIFEED_CRAWL4AI_KEEP_LINKS).
	KeepLinks bool
	// PruneThreshold is the PruningContentFilter score cutoff (0–1): nodes scoring
	// below it are dropped, so a higher value strips more boilerplate/duplicated
	// chrome from noisy pages. The default is owned by config
	// (OMNIFEED_CRAWL4AI_PRUNE_THRESHOLD).
	PruneThreshold float64
	// WaitUntil is crawl4ai's page-ready signal (Playwright wait_until):
	// domcontentloaded (the default) fires before client-side frameworks hydrate,
	// so JS-only SPAs render empty; networkidle waits for them at the cost of
	// latency on every page. The default is owned by config
	// (OMNIFEED_CRAWL4AI_WAIT_UNTIL); empty falls back to domcontentloaded.
	WaitUntil string
	// ExcludedSelector is the CSS selector list crawl4ai drops before extraction
	// (OMNIFEED_CRAWL4AI_EXCLUDED_SELECTOR). Empty = DefaultExcludedSelector; to
	// effectively exclude nothing, set a selector that matches nothing.
	ExcludedSelector string
	// TargetElements is a comma-separated CSS selector list; when non-empty,
	// crawl4ai extracts markdown ONLY from matching containers. Off by default
	// (OMNIFEED_CRAWL4AI_TARGET_ELEMENTS): on pages without a match the crawl
	// yields no content, which the thin-content guard turns into an error.
	TargetElements string
	// ScanFullPage scrolls the page to the bottom (in ScrollDelay steps) before
	// extraction so lazy-loaded content renders — multi-second on long pages.
	// The default is owned by config (OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE).
	ScanFullPage bool
	// ScrollDelay is the pause (seconds) between scroll steps; only sent when
	// ScanFullPage is on (OMNIFEED_CRAWL4AI_SCROLL_DELAY).
	ScrollDelay float64
	// DelayBeforeHTML is the unconditional settle (seconds) after the WaitUntil
	// signal before HTML extraction — paid on every crawl. The default is owned
	// by config (OMNIFEED_CRAWL4AI_DELAY_BEFORE_HTML).
	DelayBeforeHTML float64
	// RemoveOverlays sends crawl4ai's remove_overlay_elements, whose geometry
	// heuristic deletes any large absolute/fixed-position element before
	// extraction. On sites whose main content lives in such containers
	// (Wikipedia Vector-2022, several news fronts) it silently empties the
	// whole page — the default is off (OMNIFEED_CRAWL4AI_REMOVE_OVERLAYS);
	// remove_consent_popups stays on regardless and covers cookie modals.
	RemoveOverlays bool
	// BlockPrivateIPs hardens the raw-text bypass's direct fetches: resolved
	// private/reserved addresses are refused at dial time (mirrors
	// OMNIFEED_BLOCK_PRIVATE_IPS, which the registry enforces pre-dispatch via
	// DNS lookup — the dial-time guard is what a rebinding race can't beat).
	BlockPrivateIPs bool
}

// New returns a crawl4ai fallback Engine wired with the given config.
func New(cfg Config) *Engine {
	waitUntil := cfg.WaitUntil
	if waitUntil == "" {
		waitUntil = "domcontentloaded"
	}
	excludedSelector := cfg.ExcludedSelector
	if excludedSelector == "" {
		excludedSelector = DefaultExcludedSelector
	}
	// The bypass client copies the shared client's metric hooks (WithUpstream)
	// but swaps in its own SSRF-guarded http.Client: direct fetches dial the
	// open internet, which the crawl4ai-bound shared client never does.
	direct := cfg.Client.WithUpstream("direct", "get")
	if direct != nil {
		direct.HTTP = httpx.NewGuardedClient(cfg.BlockPrivateIPs, rawFetchTimeout)
	}
	return &Engine{
		endpoint:         cfg.Endpoint,
		token:            cfg.Token,
		client:           cfg.Client.WithUpstream("crawl4ai", "crawl"),
		limiter:          cfg.Limiter,
		keepLinks:        cfg.KeepLinks,
		pruneThreshold:   cfg.PruneThreshold,
		waitUntil:        waitUntil,
		excludedSelector: excludedSelector,
		targetElements:   splitSelectors(cfg.TargetElements),
		scanFullPage:     cfg.ScanFullPage,
		scrollDelay:      cfg.ScrollDelay,
		delayBeforeHTML:  cfg.DelayBeforeHTML,
		removeOverlays:   cfg.RemoveOverlays,
		direct:           direct,
	}
}

// splitSelectors turns a comma-separated CSS selector list into a trimmed slice,
// dropping empty entries. Commas inside parentheses don't split — functional
// pseudo-classes like :is(h1, h2) or :not(.a, .b) are one selector, not two.
// An empty or all-blank input returns nil.
func splitSelectors(s string) []string {
	var out []string
	depth, start := 0, 0
	emit := func(end int) {
		if p := strings.TrimSpace(s[start:end]); p != "" {
			out = append(out, p)
		}
	}
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				emit(i)
				start = i + 1
			}
		}
	}
	emit(len(s))
	return out
}

// Name returns the engine identifier ("crawl4ai").
func (*Engine) Name() string { return "crawl4ai" }

// Matches returns false: this engine is the fallback only.
func (*Engine) Matches(string) bool { return false }

// headers are the crawl4ai request headers, adding a bearer token when one is
// configured (the upstream's CRAWL4AI_API_TOKEN).
func (e *Engine) headers() map[string]string {
	h := map[string]string{"Content-Type": "application/json"}
	if e.token != "" {
		h["Authorization"] = "Bearer " + e.token
	}
	return h
}

// --- crawl4ai wire types ---

type crawlRequest struct {
	URLs          []string               `json:"urls"`
	CrawlerConfig map[string]interface{} `json:"crawler_config,omitempty"`
}

type crawlResponse struct {
	Success bool          `json:"success"`
	Results []crawlResult `json:"results"`
	Error   string        `json:"error"`
}

type crawlResult struct {
	URL          string        `json:"url"`
	Markdown     crawlMarkdown `json:"markdown"`
	CleanedHTML  string        `json:"cleaned_html"`
	Success      bool          `json:"success"`
	StatusCode   int           `json:"status_code"`
	ErrorMessage string        `json:"error_message"`
}

type crawlMarkdown struct {
	RawMarkdown string `json:"raw_markdown"`
	FitMarkdown string `json:"fit_markdown"`
}

// Crawl proxies rawURL to crawl4ai. The configured per-domain limiter applies
// to avoid hammering sites that crawl4ai itself doesn't pace.
//
// A thin-content result with the excluded selector active is retried once
// without it: the selector list names chrome shapes (.sidebar, #toc, …), and on
// the rare page whose main content matches one, the exclusion is what emptied
// the page — not the page itself.
func (e *Engine) Crawl(ctx context.Context, rawURL string, opts domain.EngineOptions) (domain.Document, error) {
	if e.endpoint == "" {
		return domain.Document{}, fmt.Errorf("crawl4ai endpoint not configured (set OMNIFEED_CRAWL4AI_URL)")
	}

	release, err := e.limiter.Acquire(ctx, e.Name(), rawURL)
	if err != nil {
		return domain.Document{}, err
	}
	defer release()

	// Raw non-HTML text (code files, JSON, markdown) needs no browser render —
	// fetch it directly; any uncertainty falls through to the browser path.
	if doc, ok := e.rawText(ctx, rawURL); ok {
		return doc, nil
	}

	// Per-request opt-in/out wins over the deployment default: the scroll only
	// pays off on append-style infinite feeds, which the caller can recognize
	// and this engine can't.
	scan := e.scanFullPage
	if opts.ScanFullPage != nil {
		scan = *opts.ScanFullPage
	}

	doc, err := e.crawlOnce(ctx, rawURL, e.excludedSelector, scan)
	if err != nil && e.excludedSelector != "" && isThinContent(err) && ctx.Err() == nil {
		return e.crawlOnce(ctx, rawURL, "", scan)
	}
	return doc, err
}

// isThinContent reports whether err is the thin-content classification.
func isThinContent(err error) bool {
	var fe *domain.FetchError
	return errors.As(err, &fe) && fe.Kind == domain.KindThinContent
}

// crawlOnce performs one crawl4ai request with the given excluded selector
// ("" omits the field — exclude nothing) and full-page-scan setting.
func (e *Engine) crawlOnce(ctx context.Context, rawURL, excludedSelector string, scanFullPage bool) (domain.Document, error) {
	// Dropping links silently loses the primary content on link-dense pages —
	// e.g. every story title on a Hacker News front page is an external link.
	// keepLinks renders anchor text (ignore_links=false) and retains external
	// anchors (exclude_external_links=false).
	ignoreLinks := !e.keepLinks
	excludeExternalLinks := !e.keepLinks

	params := map[string]interface{}{
		"word_count_threshold":     10,
		"wait_until":               e.waitUntil,
		"delay_before_return_html": e.delayBeforeHTML,
		// crawl4ai silently clamps page_timeout from an untrusted REST body to
		// 60000 ms, so anything higher is a lie we'd tell ourselves in logs.
		// (The client-side HTTP timeout is a separate knob and is unaffected.)
		"page_timeout": maxPageTimeoutMS,
		// max_retries stays at crawl4ai's default (0): its internal re-renders
		// are dominated by content-gate failures that never succeed on re-drive,
		// happen invisibly inside one HTTP call, and stack multiplicatively with
		// our own client-side retry (which covers genuine transient 500s, at
		// MaxAttempts 2, visibly in metrics).
		// script/style/noscript carry no prose but do reach the markdown on pages
		// that inline them, so they go out with the structural chrome.
		"excluded_tags":              []string{"nav", "footer", "header", "form", "aside", "script", "style", "noscript"},
		"remove_consent_popups":      true,
		"exclude_external_links":     excludeExternalLinks,
		"exclude_social_media_links": true,
		"exclude_external_images":    true,
		"markdown_generator": map[string]interface{}{
			"type": "DefaultMarkdownGenerator",
			"params": map[string]interface{}{
				"content_filter": map[string]interface{}{
					"type": "PruningContentFilter",
					"params": map[string]interface{}{
						"threshold":      e.pruneThreshold,
						"threshold_type": "fixed",
						// Syntax highlighters wrap code tokens in <span>s that the
						// pruning filter scores below the threshold and drops,
						// corrupting the code it keeps. preserve_tags/_classes make
						// the filter skip those subtrees whole (available since
						// crawl4ai 0.9.1; upstream bug unclecode/crawl4ai#2110).
						// "table" stays OUT: it would re-admit chrome tables.
						"preserve_tags":    []string{"pre", "code"},
						"preserve_classes": []string{"highlight", "chroma", "highlighter-rouge", "codehilite"},
					},
				},
				"options": map[string]interface{}{
					"ignore_links": ignoreLinks,
				},
			},
		},
	}
	// Full-page scan is opt-in: scroll_delay only means anything while scanning,
	// so both keys stay out of the payload when the scan is off (crawl4ai's
	// default is no scan).
	if scanFullPage {
		params["scan_full_page"] = true
		params["scroll_delay"] = e.scrollDelay
	}
	// Overlay removal is opt-in: its geometry heuristic silently empties pages
	// whose content sits in large fixed/absolute containers (see Config).
	if e.removeOverlays {
		params["remove_overlay_elements"] = true
	}
	// Selector-level chrome removal; omitted on the thin-content retry.
	if excludedSelector != "" {
		params["excluded_selector"] = excludedSelector
	}
	// Opt-in and off by default: target_elements narrows extraction to the
	// matching containers, which returns nothing at all on pages that have none.
	if len(e.targetElements) > 0 {
		params["target_elements"] = e.targetElements
	}

	req := crawlRequest{
		URLs:          []string{rawURL},
		CrawlerConfig: map[string]interface{}{"type": "CrawlerRunConfig", "params": params},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return domain.Document{}, fmt.Errorf("marshal request: %w", err)
	}

	// MaxAttempts 2: crawl4ai 0.9.2+ scrubs 500 bodies, so a deterministic
	// verdict (block/content-gate) and a transient fault (pool churn, worker
	// OOM) are indistinguishable — one retry rescues the transients while a
	// deterministic page costs at most 2× one crawl, not 3×.
	resp, err := e.client.DoRetry(ctx, http.MethodPost, e.endpoint, body,
		e.headers(),
		httpx.RetryConfig{MaxAttempts: 2, RetryableStatus: antibot.RetryableStatus})
	if err != nil {
		return domain.Document{}, classifyCrawlError(err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return domain.Document{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return domain.Document{}, &domain.FetchError{
			Kind:       domain.KindForStatus(resp.StatusCode),
			StatusCode: resp.StatusCode,
			Err:        fmt.Errorf("crawl4ai returned %d: %s", resp.StatusCode, truncate(string(respBody), 200)),
		}
	}

	var cr crawlResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("decode response: %w", err)}
	}
	if !cr.Success {
		msg := cr.Error
		if msg == "" && len(cr.Results) > 0 {
			msg = cr.Results[0].ErrorMessage
		}
		kind := domain.KindUpstreamError
		if antibot.IsBlockResponse(msg) {
			kind = blockKind(msg)
		}
		return domain.Document{}, &domain.FetchError{Kind: kind, Err: fmt.Errorf("crawl failed: %s", msg)}
	}
	if len(cr.Results) == 0 {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("crawl returned no results")}
	}

	result := cr.Results[0]
	// Whitespace-only counts as empty here: a fit_markdown of "\n" must fall
	// back to raw_markdown, not win the pick and defeat the fallback chain.
	content := result.Markdown.FitMarkdown
	if strings.TrimSpace(content) == "" {
		content = result.Markdown.RawMarkdown
	}
	if strings.TrimSpace(content) == "" {
		content = result.CleanedHTML
	}

	// A bot wall often arrives as a "successful" HTTP 200 whose body is a
	// challenge page, so a crawl that succeeded can still be a block. Reclassify
	// it as a failure instead of handing the challenge page to the caller (the
	// LLM) — this is what surfaces Cloudflare/CAPTCHA blocks in metrics and logs.
	scan := content
	if result.CleanedHTML != "" && result.CleanedHTML != content {
		scan = result.CleanedHTML + "\n" + content
	}
	if marker, blocked := antibot.Detect(scan); blocked {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindCaptcha, StatusCode: result.StatusCode, Marker: marker}
	}
	if result.StatusCode == http.StatusForbidden || result.StatusCode == http.StatusTooManyRequests {
		return domain.Document{}, &domain.FetchError{
			Kind:       domain.KindForStatus(result.StatusCode),
			StatusCode: result.StatusCode,
			Err:        fmt.Errorf("crawl4ai page returned %d (blocked)", result.StatusCode),
		}
	}

	// crawl4ai has no "extracted nothing" flag: a page it failed to render comes
	// back as success with every content field empty. Returning that as a
	// successful empty Document hands the caller (the LLM) silence it can't tell
	// from a genuinely blank page — classify it as thin_content instead. Checked
	// after the block detection above so a whitespace challenge page still
	// reports captcha.
	if strings.TrimSpace(content) == "" {
		return domain.Document{}, &domain.FetchError{
			Kind:       domain.KindThinContent,
			StatusCode: result.StatusCode,
			Err: fmt.Errorf("crawl4ai extracted 0 chars (raw_md=%d fit_md=%d cleaned_html=%d)",
				len(result.Markdown.RawMarkdown), len(result.Markdown.FitMarkdown), len(result.CleanedHTML)),
		}
	}

	return domain.Document{
		PageContent: content,
		Metadata: map[string]string{
			"source":              rawURL,
			"status_code":         fmt.Sprintf("%d", result.StatusCode),
			domain.ContentTypeKey: domain.ContentTypeMarkdown,
		},
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) { // back up to a rune boundary so we don't split a multibyte char
		n--
	}
	return s[:n] + "..."
}

// blockKind splits a crawl4ai block verdict (one that has already matched
// antibot.IsBlockResponse) into its two meanings: crawl4ai's own structural
// content-gate — a thin / empty / unparseable render (SPA shell, PDF, near-empty
// page) — is thin_content; anything else is a genuine anti-bot wall (bot_block).
func blockKind(body string) domain.FailureKind {
	if antibot.IsStructuralBlock(body) {
		return domain.KindThinContent
	}
	return domain.KindBotBlock
}

// classifyCrawlError turns a DoRetry transport error into a typed FetchError.
// crawl4ai hard-errors a blocked or too-sparse page as a top-level HTTP 5xx whose
// body names the block (antibot.IsBlockResponse); that is a content block, not an
// upstream fault, so it is demoted out of upstream_error (which still pages) via
// blockKind — crawl4ai's content-gate (minimal_text / no <body>: a JS-only SPA, a
// PDF, a near-empty page) becomes thin_content, while a genuine wall becomes
// bot_block. Both drop out of the OmnifeedCrawlErrors alert while staying
// distinct, visible metric series. A 5xx with any other body stays upstream_error.
func classifyCrawlError(err error) *domain.FetchError {
	fe := httpx.ClassifyClientError(err, domain.KindUpstreamError)
	if fe != nil && fe.Kind == domain.KindUpstreamError {
		var se *httpx.StatusError
		switch {
		case errors.As(err, &se) && antibot.IsBlockResponse(se.Body):
			fe.Kind = blockKind(se.Body)
			// Surface crawl4ai's verdict (the StatusError body) so the log/metric
			// says WHY — minimal_text / no <body> / which wall — instead of the
			// bare "upstream returned 500".
			fe.Err = fmt.Errorf("crawl4ai %d: %s", se.StatusCode, truncate(se.Body, 200))
		case errors.As(err, &se) && se.StatusCode == http.StatusInternalServerError && antibot.IsScrubbedServerError(se.Body):
			// crawl4ai 0.9.2+ scrubs its crawl verdicts (blocks, content-gates,
			// crashes) out of the 500 body — the reason lives in ITS log under a
			// correlation id. Deterministic per page and dominated by non-faults,
			// so it must not read as an upstream outage.
			fe.Kind = domain.KindUpstreamRejected
			fe.Err = fmt.Errorf("crawl4ai rejected the page (verdict scrubbed server-side; see crawl4ai logs): %s", truncate(se.Body, 120))
		}
	}
	return fe
}
