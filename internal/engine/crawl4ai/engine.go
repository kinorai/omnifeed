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
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/antibot"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

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
}

// New returns a crawl4ai fallback Engine wired with the given config.
func New(cfg Config) *Engine {
	waitUntil := cfg.WaitUntil
	if waitUntil == "" {
		waitUntil = "domcontentloaded"
	}
	return &Engine{endpoint: cfg.Endpoint, token: cfg.Token, client: cfg.Client, limiter: cfg.Limiter, keepLinks: cfg.KeepLinks, pruneThreshold: cfg.PruneThreshold, waitUntil: waitUntil}
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
func (e *Engine) Crawl(ctx context.Context, rawURL string, _ domain.EngineOptions) (domain.Document, error) {
	if e.endpoint == "" {
		return domain.Document{}, fmt.Errorf("crawl4ai endpoint not configured (set OMNIFEED_CRAWL4AI_URL)")
	}

	release := e.limiter.Acquire(rawURL)
	defer release()

	// Dropping links silently loses the primary content on link-dense pages —
	// e.g. every story title on a Hacker News front page is an external link.
	// keepLinks renders anchor text (ignore_links=false) and retains external
	// anchors (exclude_external_links=false).
	ignoreLinks := !e.keepLinks
	excludeExternalLinks := !e.keepLinks

	req := crawlRequest{
		URLs: []string{rawURL},
		CrawlerConfig: map[string]interface{}{
			"type": "CrawlerRunConfig",
			"params": map[string]interface{}{
				"word_count_threshold":       10,
				"wait_until":                 e.waitUntil,
				"delay_before_return_html":   1.0,
				"page_timeout":               90000,
				"scan_full_page":             true,
				"scroll_delay":               0.5,
				"max_retries":                2,
				"excluded_tags":              []string{"nav", "footer", "header", "form", "aside"},
				"remove_overlay_elements":    true,
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
							},
						},
						"options": map[string]interface{}{
							"ignore_links": ignoreLinks,
						},
					},
				},
			},
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return domain.Document{}, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := e.client.DoRetry(ctx, http.MethodPost, e.endpoint, body,
		e.headers(),
		httpx.RetryConfig{RetryableStatus: antibot.RetryableStatus})
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
	content := result.Markdown.FitMarkdown
	if content == "" {
		content = result.Markdown.RawMarkdown
	}
	if content == "" {
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

	return domain.Document{
		PageContent: content,
		Metadata: map[string]string{
			"source":      rawURL,
			"status_code": fmt.Sprintf("%d", result.StatusCode),
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
		if errors.As(err, &se) && antibot.IsBlockResponse(se.Body) {
			fe.Kind = blockKind(se.Body)
			// Surface crawl4ai's verdict (the StatusError body) so the log/metric
			// says WHY — minimal_text / no <body> / which wall — instead of the
			// bare "upstream returned 500".
			fe.Err = fmt.Errorf("crawl4ai %d: %s", se.StatusCode, truncate(se.Body, 200))
		}
	}
	return fe
}
