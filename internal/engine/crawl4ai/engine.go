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
	"net/url"
	"regexp"
	"strings"
	"time"
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
// can stay aggressive. The second half drops text that is never content: SVG
// and icon-font <title> labels (Kickstarter's sprite sheet alone is 12k chars
// of "arrow-left icon Fill 1 Copy 5"), chart axis labels, and the root nodes of
// the common consent managers.
const DefaultExcludedSelector = ".sidebar,.toc,#toc,.related,.newsletter,.cookie-banner,[aria-label*='cookie']," +
	"svg title,[class*='icon'] title,.highcharts-axis-labels,#didomi-host,#onetrust-consent-sdk,#usercentrics-root,.truste_box_overlay"

// maxResponseBytes caps the crawl4ai JSON read. The response carries html,
// cleaned_html and links next to the markdown, so a long page (fr.wikipedia
// "Paris": 12 MB, redis.io streams docs: 20 MB) outgrows a 10 MiB cap.
const maxResponseBytes = 32 << 20

// Engine sends URLs to crawl4ai's /crawl endpoint and extracts the best-fit
// markdown body. It is registered as the Registry fallback.
type Engine struct {
	endpoint       string
	token          string
	client         *httpx.Client
	limiter        httpx.Limiter
	keepLinks      bool
	pruneThreshold float64
	waitUntil      string

	excludedSelector string
	targetElements   []string

	scanFullPage    bool
	scrollDelay     float64
	delayBeforeHTML float64
	removeOverlays  bool

	userAgent     string
	stealth       bool
	challengeWait time.Duration
	minProseChars int

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
	Limiter httpx.Limiter
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
	// UserAgent is sent as BrowserConfig.user_agent. crawl4ai's own default is
	// a malformed Chrome/116 string (no "KHTML, like Gecko") that several
	// sites reject as an outdated browser; it should match the Chromium the
	// crawl4ai image bundles (OMNIFEED_CRAWL4AI_USER_AGENT, "" = crawl4ai's).
	UserAgent string
	// Stealth sends BrowserConfig.enable_stealth (playwright-stealth patches)
	// (OMNIFEED_CRAWL4AI_STEALTH).
	Stealth bool
	// ChallengeWait waits up to this long for an interstitial bot challenge
	// ("Just a moment…") to clear by itself before extraction; 0 disables it
	// (OMNIFEED_CRAWL4AI_CHALLENGE_WAIT). Pages without a challenge title pay
	// nothing: the wait_for predicate is true on the first poll.
	ChallengeWait time.Duration
	// MinProseChars rejects a rendered page whose human-readable text (link
	// targets, URLs and markdown syntax removed) is shorter than this, as
	// thin_content: nav-only shells and empty app frames otherwise reach the
	// caller as success. 0 disables the floor (OMNIFEED_CRAWL4AI_MIN_PROSE_CHARS).
	MinProseChars int
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
		userAgent:        cfg.UserAgent,
		stealth:          cfg.Stealth,
		challengeWait:    cfg.ChallengeWait,
		minProseChars:    cfg.MinProseChars,
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
	BrowserConfig map[string]interface{} `json:"browser_config,omitempty"`
	CrawlerConfig map[string]interface{} `json:"crawler_config,omitempty"`
}

// challengeTitle matches the <title> of interstitial bot challenges that clear
// by themselves after a few seconds of JS (Cloudflare "Just a moment...", its
// localized variants, and the older "Attention Required").
const challengeTitle = `/just a moment|un instant|einen moment|attention required|checking your browser|please wait/i`

type crawlResponse struct {
	Success bool          `json:"success"`
	Results []crawlResult `json:"results"`
	Error   string        `json:"error"`
}

type crawlResult struct {
	URL         string        `json:"url"`
	Markdown    crawlMarkdown `json:"markdown"`
	CleanedHTML string        `json:"cleaned_html"`
	// Success is a pointer so a result that omits the field (older fixtures,
	// pre-0.9.3 shapes) is not read as a failure — only an explicit false is.
	Success       *bool  `json:"success"`
	StatusCode    int    `json:"status_code"`
	ErrorMessage  string `json:"error_message"`
	RedirectedURL string `json:"redirected_url"`
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

	rawURL = consentBypassURL(xcomURL(rawURL))

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

	if looksLikePDF(rawURL) {
		return e.crawlOnce(ctx, rawURL, crawlOpts{pdf: true})
	}

	base := crawlOpts{excludedSelector: e.excludedSelector, scanFullPage: scan}
	doc, err := e.crawlOnce(ctx, rawURL, base)
	// Playwright's "page is navigating" race: a JS challenge or client redirect
	// reloaded the page between load and extraction. One retry that waits for
	// the network to settle is the upstream-recommended remedy.
	if err != nil && isNavigationRace(err) && ctx.Err() == nil {
		settled := base
		settled.waitUntil = "networkidle"
		doc, err = e.crawlOnce(ctx, rawURL, settled)
	}
	// A bot wall on the modern identity: retry once on crawl4ai's default
	// one (see browserParams). Only blocks qualify — a 404 or a thin page
	// would come back the same.
	if err != nil && e.userAgent != "" && isBlock(err) && ctx.Err() == nil {
		alt := base
		alt.altFingerprint = true
		if altDoc, altErr := e.crawlOnce(ctx, rawURL, alt); altErr == nil {
			doc, err = altDoc, nil
		}
	}
	if err != nil && e.excludedSelector != "" && isThinContent(err) && ctx.Err() == nil {
		doc, err = e.crawlOnce(ctx, rawURL, crawlOpts{scanFullPage: scan})
	}
	// Last resort: crawl4ai's content gate rejects a body it cannot render as a
	// page — which is every JSON/plain-text API response served from a path with
	// no file extension, so the pre-crawl bypass above never claimed it. Those
	// arrive as upstream_rejected (the scrubbed 500) or thin_content. Retry them
	// as a direct GET; the Content-Type check still decides, so an HTML page
	// that genuinely failed keeps its original error.
	if err != nil && isRescuable(err) && ctx.Err() == nil {
		if rescued, ok := e.rawTextForce(ctx, rawURL); ok {
			return rescued, nil
		}
	}
	// A PDF behind an extensionless URL fails the browser path: Chromium
	// starts a download ("Download is starting") or crawl4ai's content gate
	// rejects the body. One header-only probe confirms it before the PDF
	// retry; pages that crawled fine, and walls, never pay for it.
	if err != nil && (isRescuable(err) || strings.Contains(err.Error(), "Download is starting")) && ctx.Err() == nil && e.servesPDF(ctx, rawURL) {
		return e.crawlOnce(ctx, rawURL, crawlOpts{pdf: true})
	}
	return doc, err
}

// looksLikePDF reports a URL whose path names a PDF (".pdf", or arXiv's
// /pdf/<id> form), routed to the PDF strategy without probing.
func looksLikePDF(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)
	return strings.HasSuffix(p, ".pdf") || (strings.HasSuffix(u.Hostname(), "arxiv.org") && strings.HasPrefix(p, "/pdf/"))
}

// servesPDF probes rawURL's Content-Type with a GET whose body is never read.
func (e *Engine) servesPDF(ctx context.Context, rawURL string) bool {
	if e.direct == nil {
		return false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := e.direct.DoRetry(probeCtx, http.MethodGet, rawURL, nil,
		map[string]string{"User-Agent": rawUserAgent}, httpx.RetryConfig{MaxAttempts: 1})
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK && rawContentType(resp) == "application/pdf"
}

// isBlock reports a bot wall: a challenge, a block page or a 403. A 429 is
// left out on purpose — it asks us to slow down, not to come back as someone
// else.
func isBlock(err error) bool {
	var fe *domain.FetchError
	if !errors.As(err, &fe) {
		return false
	}
	switch fe.Kind {
	case domain.KindCaptcha, domain.KindBotBlock, domain.KindHTTP403:
		return true
	}
	return false
}

// isNavigationRace reports whether err is Playwright's "page is navigating
// and changing the content" failure.
func isNavigationRace(err error) bool {
	return err != nil && strings.Contains(err.Error(), "page is navigating")
}

var (
	mdLinkTarget = regexp.MustCompile(`\]\([^)]*\)`)
	bareURL      = regexp.MustCompile(`https?://\S+`)
	mdSyntax     = regexp.MustCompile("[#*_\\[\\]|`>\u200b-\u200f\u2060\ufeff-]+")
	spaces       = regexp.MustCompile(`\s+`)
)

// proseText reduces markdown to human-readable text: link targets, bare URLs,
// markdown syntax and zero-width runes are dropped.
func proseText(s string) string {
	s = mdLinkTarget.ReplaceAllString(s, "]")
	s = bareURL.ReplaceAllString(s, "")
	s = mdSyntax.ReplaceAllString(s, " ")
	return strings.TrimSpace(spaces.ReplaceAllString(s, " "))
}

func proseChars(s string) int { return len([]rune(proseText(s))) }

// softNotFound matches a "not found" page served with a 2xx/3xx status (soft
// 404). Only the page's opening prose is checked, so an article that merely
// discusses 404s further down is not caught; bare "404 Not Found" is left out
// because it is also the title of HTTP reference pages.
var softNotFound = regexp.MustCompile(`(?i)\bpage not found\b|\bpage (you|you're|you are) (were |are )?looking for\b|\bthis page (doesn't|does not|no longer) exists?\b|\bthis page is not available\b|\bpage introuvable\b|\bseite nicht gefunden\b`)

// silentFailure classifies a 2xx page whose content is not the requested
// content: a soft 404, or a client-rendered skeleton captured before hydration
// (a wall of "Loading" placeholders).
func silentFailure(content, scan string) (domain.FailureKind, string) {
	// Akamai Bot Manager's interstitial: the whole page is its logo line.
	// The markdown keeps only "Powered and protected by" (the Akamai logo is
	// an external image, stripped), so the vendor is read from scan (cleaned
	// HTML + markdown). A real page may credit its CDN in a footer, so only a
	// near-empty page counts.
	if strings.Contains(strings.ToLower(content), "powered and protected by") && strings.Contains(strings.ToLower(scan), "akamai") && proseChars(content) < 300 {
		return domain.KindBotBlock, "akamai bot manager interstitial"
	}
	// Soft 404: the phrase must stand as its own short line (a heading or
	// banner) BEFORE the page's first real paragraph. An error page leads
	// with it; an article that has a "Page not found" section (MDN's 404
	// reference) reaches it only after prose, so scanning stops there.
	seen := 0
	for _, l := range strings.Split(content, "\n") {
		l = proseText(l)
		if l == "" {
			continue
		}
		if seen++; seen > 12 || len([]rune(l)) >= 120 {
			break
		}
		if len([]rune(l)) <= 60 {
			if m := softNotFound.FindString(l); m != "" {
				return domain.KindNotFound, "soft 404: " + m
			}
		}
	}
	// Geo country picker served in place of the page (Best Buy from an EU
	// egress): short, and leads with the picker.
	if head := strings.ToLower(firstLines(content, 10)); proseChars(content) < 1500 && (strings.Contains(head, "choose a country") || strings.Contains(head, "select your country")) {
		return domain.KindBotBlock, "country picker interstitial"
	}
	lines, loading := 0, 0
	for _, l := range strings.Split(content, "\n") {
		l = strings.ToLower(strings.Trim(strings.TrimSpace(l), ".…#* "))
		if l == "" {
			continue
		}
		lines++
		if l == "loading" {
			loading++
		}
	}
	if loading >= 10 && loading*10 > lines*3 {
		return domain.KindThinContent, fmt.Sprintf("skeleton: %d/%d lines are Loading placeholders", loading, lines)
	}
	return "", ""
}

// isThinContent reports whether err is the thin-content classification.
func isThinContent(err error) bool {
	var fe *domain.FetchError
	return errors.As(err, &fe) && fe.Kind == domain.KindThinContent
}

// isRescuable reports whether err is a crawl4ai verdict about the CONTENT
// (it rendered nothing usable) rather than a transport fault or a bot wall.
// Only those are worth a direct-GET retry: a 429 or a CAPTCHA would meet the
// same wall without a browser, and a timeout would just spend the budget twice.
func isRescuable(err error) bool {
	var fe *domain.FetchError
	if !errors.As(err, &fe) {
		return false
	}
	return fe.Kind == domain.KindThinContent || fe.Kind == domain.KindUpstreamRejected
}

// browserParams builds the BrowserConfig overrides. altFingerprint keeps
// crawl4ai's own default user agent: Cloudflare-fronted sites reject that
// default while Akamai-fronted ones reject the modern UA (measured
// 2026-10-08: lequipe, boulanger, lesechos), so a block on one identity is
// retried once on the other.
func (e *Engine) browserParams(altFingerprint bool) map[string]interface{} {
	bp := map[string]interface{}{}
	if e.userAgent != "" && !altFingerprint {
		bp["user_agent"] = e.userAgent
	}
	if e.stealth {
		bp["enable_stealth"] = true
	}
	return bp
}

// crawlOpts are the per-attempt variations of one crawl4ai request.
type crawlOpts struct {
	excludedSelector string // "" omits the field: exclude nothing
	scanFullPage     bool
	waitUntil        string // "" = the engine's configured wait_until
	pdf              bool   // browserless PDF strategy instead of a page render
	altFingerprint   bool   // crawl4ai's default user agent (see browserParams)
}

// crawlOnce performs one crawl4ai request.
func (e *Engine) crawlOnce(ctx context.Context, rawURL string, o crawlOpts) (domain.Document, error) {
	excludedSelector, scanFullPage, pdf := o.excludedSelector, o.scanFullPage, o.pdf
	waitUntil := e.waitUntil
	if o.waitUntil != "" {
		waitUntil = o.waitUntil
	}
	// Dropping links silently loses the primary content on link-dense pages —
	// e.g. every story title on a Hacker News front page is an external link.
	// keepLinks renders anchor text (ignore_links=false) and retains external
	// anchors (exclude_external_links=false).
	ignoreLinks := !e.keepLinks
	excludeExternalLinks := !e.keepLinks

	params := map[string]interface{}{
		"word_count_threshold":     10,
		"wait_until":               waitUntil,
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
						// ul/ol go IN: link-only list items (package deps, index
						// pages) score below the threshold and vanish. Benchmark
						// 2026-10-08: recovers Arch deps / gov.uk / OCW, prose pages
						// grow 0-13%.
						"preserve_tags":    []string{"pre", "code", "ul", "ol"},
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

	// PDFs: crawl4ai switches to its browserless PDFCrawlerStrategy (pypdf)
	// when the scraping strategy is PDFContentScrapingStrategy. The pruning
	// filter and the browser-only knobs mean nothing there, and pruning drops
	// most of a paper's paragraphs, so only the strategy and timeout go out.
	if pdf {
		params = map[string]interface{}{
			"page_timeout":      maxPageTimeoutMS,
			"scraping_strategy": map[string]interface{}{"type": "PDFContentScrapingStrategy", "params": map[string]interface{}{}},
		}
	}
	if e.challengeWait > 0 && !pdf {
		params["wait_for"] = "js:() => !" + challengeTitle + ".test(document.title)"
		params["wait_for_timeout"] = int(e.challengeWait / time.Millisecond)
	}

	req := crawlRequest{
		URLs:          []string{rawURL},
		CrawlerConfig: map[string]interface{}{"type": "CrawlerRunConfig", "params": params},
	}
	// Both keys are on crawl4ai's untrusted-body allowlist (cookies/headers
	// are not — they 400). An identical browser_config on every call keeps
	// crawl4ai's pool on one warm browser.
	if bp := e.browserParams(o.altFingerprint); len(bp) > 0 && !pdf {
		req.BrowserConfig = map[string]interface{}{"type": "BrowserConfig", "params": bp}
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

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return domain.Document{}, fmt.Errorf("read response: %w", err)
	}
	// A body cut at the cap fails json.Unmarshal as "unexpected end of JSON
	// input", which hides the real cause; say so explicitly instead.
	if len(respBody) > maxResponseBytes {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindBadResponse,
			Err: fmt.Errorf("crawl4ai response exceeds %d MiB", maxResponseBytes>>20)}
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
		status := 0
		if msg == "" && len(cr.Results) > 0 {
			msg = cr.Results[0].ErrorMessage
			status = cr.Results[0].StatusCode
		}
		return domain.Document{}, verdictError(msg, status)
	}
	if len(cr.Results) == 0 {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindBadResponse, Err: fmt.Errorf("crawl returned no results")}
	}

	result := cr.Results[0]
	// crawl4ai 0.9.3+ (PR #2134) answers HTTP 200 with a top-level
	// success:true even when every URL failed: the verdict lives in the
	// result's own success/error_message. Without this check a blocked page's
	// own markdown would be served as content.
	if result.Success != nil && !*result.Success {
		return domain.Document{}, verdictError(result.ErrorMessage, result.StatusCode)
	}
	// Whitespace-only counts as empty here: a fit_markdown of "\n" must fall
	// back to raw_markdown, not win the pick and defeat the fallback chain.
	content := result.Markdown.FitMarkdown
	if strings.TrimSpace(content) == "" {
		content = result.Markdown.RawMarkdown
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
	// Google's EU consent interstitial is served from consent.<site> after a
	// redirect: the page is a language picker, never the requested content.
	if u, err := url.Parse(result.RedirectedURL); err == nil && strings.HasPrefix(u.Hostname(), "consent.") {
		return domain.Document{}, &domain.FetchError{Kind: domain.KindCaptcha, StatusCode: result.StatusCode, Marker: "consent redirect " + u.Hostname()}
	}
	if result.StatusCode == http.StatusForbidden || result.StatusCode == http.StatusTooManyRequests {
		return domain.Document{}, &domain.FetchError{
			Kind:       domain.KindForStatus(result.StatusCode),
			StatusCode: result.StatusCode,
			Err:        fmt.Errorf("crawl4ai page returned %d (blocked)", result.StatusCode),
		}
	}
	// Any other error status means the page itself is an error page (404 body,
	// 451 legal block, 5xx). Serving its markdown as success is a silent
	// failure the caller can't detect — and so can't fall back from.
	if result.StatusCode >= 400 {
		kind := domain.KindForStatus(result.StatusCode)
		// A public page answering 401/402/405/… to a plain GET is refusing
		// the visitor (AWS WAF challenges answer 405, Cloudflare-fronted
		// logins 401), not reporting a fault: classify it as a block so the
		// caller switches source and the alternate fingerprint gets a try.
		if kind == domain.KindError && result.StatusCode != http.StatusBadRequest {
			kind = domain.KindBotBlock
		}
		return domain.Document{}, &domain.FetchError{
			Kind:       kind,
			StatusCode: result.StatusCode,
			Err:        fmt.Errorf("crawl4ai page returned %d", result.StatusCode),
		}
	}

	if kind, why := silentFailure(content, scan); kind != "" {
		return domain.Document{}, &domain.FetchError{Kind: kind, StatusCode: result.StatusCode, Err: errors.New(why)}
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

	if n := proseChars(content); e.minProseChars > 0 && n < e.minProseChars {
		return domain.Document{}, &domain.FetchError{
			Kind:       domain.KindThinContent,
			StatusCode: result.StatusCode,
			Err:        fmt.Errorf("crawl4ai extracted %d prose chars (< %d)", n, e.minProseChars),
		}
	}

	meta := map[string]string{
		"source":              rawURL,
		"status_code":         fmt.Sprintf("%d", result.StatusCode),
		domain.ContentTypeKey: domain.ContentTypeMarkdown,
	}
	// A redirect to a different page (a hotel URL landing on a city search, a
	// retired article landing on the section front) still renders fine, so
	// nothing above fails it — but the caller only sees the content, not
	// _meta. Say it in-band, once, so the agent can tell it got another page.
	if final := result.RedirectedURL; final != "" && redirectedElsewhere(rawURL, final) {
		// A redirect INTO a verification page (Temu's /bgn_verification.html,
		// Google's /sorry/) is a wall even when its body passes every check.
		if u, err := url.Parse(final); err == nil && verificationPath.MatchString(u.Path) {
			return domain.Document{}, &domain.FetchError{Kind: domain.KindCaptcha, StatusCode: result.StatusCode, Marker: "redirect to " + u.Path}
		}
		// Redirected into a sign-in page the caller did not ask for (Temu,
		// LinkedIn's /authwall): the content is a login form.
		if u, err := url.Parse(final); err == nil && loginPath.MatchString(u.Path) && !loginPath.MatchString(rawPath(rawURL)) {
			return domain.Document{}, &domain.FetchError{Kind: domain.KindBotBlock, StatusCode: result.StatusCode, Err: fmt.Errorf("redirected to sign-in page %s", u.Path)}
		}
		meta["final_url"] = final
		content = "> Redirected from " + rawURL + " to " + final + "\n\n" + content
	}
	return domain.Document{PageContent: content, Metadata: meta}, nil
}

func rawPath(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil {
		return u.Path
	}
	return ""
}

// firstLines returns the first n non-empty lines of s.
func firstLines(s string, n int) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if out = append(out, l); len(out) == n {
			break
		}
	}
	return strings.Join(out, "\n")
}

// loginPath matches a sign-in wall a site redirects anonymous visitors to.
var loginPath = regexp.MustCompile(`(?i)/(login|signin|sign-in|sign_in|authwall)(/|$|\.|\?)`)

// verificationPath matches the path of a bot-check page a site redirects to.
var verificationPath = regexp.MustCompile(`(?i)captcha|_verification\b|/(verify|challenge)(/|$|\.)|/sorry/`)

// redirectedElsewhere reports a redirect that changed the page itself: another
// host (www. ignored) or another path (trailing slash and case ignored).
// Query and fragment changes are not reported: tracking parameters and
// consent bypasses rewrite them on every hop.
func redirectedElsewhere(from, to string) bool {
	a, errA := url.Parse(from)
	b, errB := url.Parse(to)
	if errA != nil || errB != nil {
		return false
	}
	host := func(u *url.URL) string { return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") }
	path := func(u *url.URL) string { return strings.TrimSuffix(strings.ToLower(u.EscapedPath()), "/") }
	return host(a) != host(b) || path(a) != path(b)
}

// verdictError turns a crawl4ai failure message into a classified FetchError
// (see classifyVerdict), keeping the raw message — flattened and truncated —
// as the detail so the log and the caller see crawl4ai's own words.
func verdictError(msg string, pageStatus int) *domain.FetchError {
	kind := classifyVerdict(msg, pageStatus)
	detail := sanitizeVerdict(msg)
	if detail == "" {
		detail = "no error message"
	}
	return &domain.FetchError{Kind: kind, StatusCode: pageStatus, Err: fmt.Errorf("crawl failed: %s", detail)}
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
// distinct, visible metric series. A 5xx carrying a FastAPI `detail` is
// classified from that message (classifyVerdict); a scrubbed 0.9.2+ 500 is
// upstream_rejected; a 504 is a timeout; any other 5xx stays upstream_error.
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
		case errors.As(err, &se) && errorDetail(se.Body) != "":
			// A FastAPI `detail` carries crawl4ai's own verdict (the 502 shape of
			// /md and /llm since 0.9.3, PR #2117): classify it like a result's
			// error_message instead of reading every 5xx as an outage.
			detail := errorDetail(se.Body)
			fe.Kind = classifyVerdict(detail, 0)
			fe.Err = fmt.Errorf("crawl4ai %d: %s", se.StatusCode, sanitizeVerdict(detail))
		case errors.As(err, &se) && se.StatusCode == http.StatusInternalServerError && antibot.IsScrubbedServerError(se.Body):
			// crawl4ai 0.9.2+ scrubs its crawl verdicts (blocks, content-gates,
			// crashes) out of the 500 body — the reason lives in ITS log under a
			// correlation id. Deterministic per page and dominated by non-faults,
			// so it must not read as an upstream outage.
			fe.Kind = domain.KindUpstreamRejected
			fe.Err = fmt.Errorf("crawl4ai rejected the page (verdict scrubbed server-side; see crawl4ai logs): %s", truncate(se.Body, 120))
		}
	}
	// crawl4ai's wall-clock 504 is already a timeout (ClassifyClientError);
	// keep its `detail` ("Crawl exceeded the time limit") as the cause.
	var se *httpx.StatusError
	if fe != nil && fe.Kind == domain.KindTimeout && errors.As(err, &se) {
		if detail := errorDetail(se.Body); detail != "" {
			fe.Err = fmt.Errorf("crawl4ai %d: %s", se.StatusCode, sanitizeVerdict(detail))
		}
	}
	return fe
}

// twitterHosts are the legacy hosts this engine must never load: twitter.com
// only 301s to x.com, yet a headless browser opening it lands on X's anti-bot
// page (a 39-byte crawl), while x.com serves logged-out readers a rendered page.
var twitterHosts = map[string]bool{"twitter.com": true, "www.twitter.com": true, "mobile.twitter.com": true}

// xcomURL points a twitter.com URL at x.com (profiles, lists, … — the
// URLs the Twitter engine does not claim) and returns any other URL unchanged.
func xcomURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || !twitterHosts[strings.ToLower(u.Hostname())] {
		return rawURL
	}
	u.Host = "x.com"
	return u.String()
}

// consentBypassURL adds ucbcb=1 to Google and YouTube URLs. From an EU egress
// those hosts 302 every cookieless visitor to consent.<host>, a language
// picker; ucbcb=1 serves the page itself. crawl4ai refuses request-supplied
// cookies (the SOCS consent cookie is not an option), so the URL is the lever.
func consentBypassURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	h = strings.TrimPrefix(h, "m.")
	if h != "youtube.com" && h != "google.com" && h != "news.google.com" {
		return rawURL
	}
	q := u.Query()
	if q.Has("ucbcb") {
		return rawURL
	}
	q.Set("ucbcb", "1")
	u.RawQuery = q.Encode()
	return u.String()
}
