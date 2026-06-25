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

	"github.com/kinorai/omnifeed/internal/antibot"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// Engine sends URLs to crawl4ai's /crawl endpoint and extracts the best-fit
// markdown body. It is registered as the Registry fallback.
type Engine struct {
	endpoint  string
	client    *httpx.Client
	limiter   *httpx.DomainLimiter
	keepLinks bool
}

// Config configures the crawl4ai Engine.
type Config struct {
	Endpoint string
	Client   *httpx.Client
	Limiter  *httpx.DomainLimiter
	// KeepLinks renders hyperlink anchor text and retains external links in the
	// extracted markdown. When false, both are stripped for leaner output. The
	// default is owned by config (OMNIFEED_CRAWL4AI_KEEP_LINKS).
	KeepLinks bool
}

// New returns a crawl4ai fallback Engine wired with the given config.
func New(cfg Config) *Engine {
	return &Engine{endpoint: cfg.Endpoint, client: cfg.Client, limiter: cfg.Limiter, keepLinks: cfg.KeepLinks}
}

// Name returns the engine identifier ("crawl4ai").
func (*Engine) Name() string { return "crawl4ai" }

// Matches returns false: this engine is the fallback only.
func (*Engine) Matches(string) bool { return false }

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
	RawMarkdown           string `json:"raw_markdown"`
	MarkdownWithCitations string `json:"markdown_with_citations"`
	FitMarkdown           string `json:"fit_markdown"`
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
				"wait_until":                 "domcontentloaded",
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
								"threshold":      0.48,
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
		map[string]string{"Content-Type": "application/json"},
		httpx.RetryConfig{RetryableStatus: retryableCrawlStatus})
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
		if isAntibotBlock(msg) {
			kind = domain.KindBotBlock
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
	return s[:n] + "..."
}

// antibotBlockMarker is the signature crawl4ai 0.8.x stamps into its error body
// when its own anti-bot / structural detector rejects a page (e.g. "Blocked by
// anti-bot protection: Structural: minimal_text on small page"). It is crawl4ai's
// own verdict string, not an HTTP status, so it is matched here in the engine
// rather than in the generic httpx / antibot layers.
const antibotBlockMarker = "blocked by anti-bot protection"

// isAntibotBlock reports whether a crawl4ai error message/body is its anti-bot
// detector firing — a real wall, or a false positive on a legitimately small page
// such as a raw .md / LICENSE.
func isAntibotBlock(s string) bool {
	return strings.Contains(strings.ToLower(s), antibotBlockMarker)
}

// retryableCrawlStatus vetoes DoRetry's retry of a crawl4ai anti-bot block. The
// block surfaces as a 5xx whose body names it (antibotBlockMarker); it is not
// transient, so retrying just re-drives an expensive browser crawl for the same
// failure — the reasoning the Reddit path already applies with MaxAttempts:1. A
// 5xx with any other body stays retryable (a genuine transient upstream fault).
func retryableCrawlStatus(status int, body string) bool {
	return status < 500 || !isAntibotBlock(body)
}

// classifyCrawlError turns a DoRetry transport error into a typed FetchError.
// crawl4ai hard-errors a blocked or too-sparse page as a top-level HTTP 5xx whose
// body names the block (see antibotBlockMarker); that is a content block, not an
// upstream fault, so it is demoted from upstream_error to bot_block. It then drops
// out of the OmnifeedCrawlErrors alert (which excludes bot_block) while staying a
// distinct, visible metric series. A 5xx with any other body stays upstream_error
// and still pages.
func classifyCrawlError(err error) *domain.FetchError {
	fe := httpx.ClassifyClientError(err, domain.KindUpstreamError)
	if fe != nil && fe.Kind == domain.KindUpstreamError {
		var se *httpx.StatusError
		if errors.As(err, &se) && isAntibotBlock(se.Body) {
			fe.Kind = domain.KindBotBlock
		}
	}
	return fe
}
