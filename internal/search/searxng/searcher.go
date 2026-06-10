// Package searxng implements domain.Searcher against a SearXNG instance's
// JSON API (GET /search?format=json). The instance must list `json` under
// `search.formats` in its settings.yml or every query returns 403.
package searxng

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kinorai/search-crawl-reddit-proxy/internal/domain"
	"github.com/kinorai/search-crawl-reddit-proxy/internal/httpx"
)

// maxResponseBytes caps how much of the SearXNG response is read; a JSON
// result page is a few hundred KB at most, so 10MB is a generous safety net.
const maxResponseBytes = 10 << 20

// Config configures the Searcher.
type Config struct {
	Endpoint string // base URL of the SearXNG instance, e.g. http://searxng:8080
	Client   *httpx.Client
}

// Searcher queries a SearXNG instance and reshapes results into the canonical
// domain.SearchResult.
type Searcher struct {
	searchURL string
	client    *httpx.Client
}

// New returns a Searcher wired with the given config.
func New(cfg Config) *Searcher {
	return &Searcher{
		searchURL: strings.TrimRight(cfg.Endpoint, "/") + "/search",
		client:    cfg.Client,
	}
}

// Name returns the searcher identifier ("searxng").
func (*Searcher) Name() string { return "searxng" }

// --- SearXNG wire types ---

type searchResponse struct {
	Results []searchHit `json:"results"`
}

type searchHit struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Content       string `json:"content"`
	Engine        string `json:"engine"`
	PublishedDate string `json:"publishedDate"`
}

// Search runs the query against SearXNG and returns up to opts.Limit results.
func (s *Searcher) Search(ctx context.Context, query string, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("query is empty")
	}

	params := url.Values{}
	params.Set("q", query)
	params.Set("format", "json")
	if opts.TimeRange != "" {
		params.Set("time_range", opts.TimeRange)
	}
	if opts.Language != "" {
		params.Set("language", opts.Language)
	}

	resp, err := s.client.DoRetry(ctx, http.MethodGet, s.searchURL+"?"+params.Encode(),
		nil, nil, httpx.RetryConfig{})
	if err != nil {
		return nil, fmt.Errorf("searxng request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("searxng returned %d (is the json format enabled in settings.yml?)", resp.StatusCode)
	}

	var sr searchResponse
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	results := make([]domain.SearchResult, 0, len(sr.Results))
	for _, hit := range sr.Results {
		results = append(results, domain.SearchResult{
			Title:         hit.Title,
			URL:           hit.URL,
			Snippet:       hit.Content,
			Engine:        hit.Engine,
			PublishedDate: hit.PublishedDate,
		})
		if opts.Limit > 0 && len(results) >= opts.Limit {
			break
		}
	}
	return results, nil
}
