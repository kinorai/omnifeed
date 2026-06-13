// Package crawl4ai implements the fallback engine: dispatches generic URLs
// to an upstream crawl4ai instance and reshapes the response into the
// canonical Document.
package crawl4ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/kinorai/omnifeed/internal/antibot"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// Engine sends URLs to crawl4ai's /crawl endpoint and extracts the best-fit
// markdown body. It is registered as the Registry fallback.
type Engine struct {
	endpoint string
	client   *httpx.Client
	limiter  *httpx.DomainLimiter
}

// Config configures the crawl4ai Engine.
type Config struct {
	Endpoint string
	Client   *httpx.Client
	Limiter  *httpx.DomainLimiter
}

// New returns a crawl4ai fallback Engine wired with the given config.
func New(cfg Config) *Engine {
	return &Engine{endpoint: cfg.Endpoint, client: cfg.Client, limiter: cfg.Limiter}
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
				"exclude_external_links":     true,
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
							"ignore_links": true,
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
		map[string]string{"Content-Type": "application/json"}, httpx.RetryConfig{})
	if err != nil {
		return domain.Document{}, httpx.ClassifyClientError(err, domain.KindUpstreamError)
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
		return domain.Document{}, &domain.FetchError{Kind: domain.KindUpstreamError, Err: fmt.Errorf("crawl failed: %s", msg)}
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
