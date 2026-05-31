package reddit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kinorai/crawl4ai-reddit-proxy/internal/httpx"
)

// Fetcher fetches raw bytes from Reddit's public JSON API. It uses the shared
// httpx.Client (retry + Retry-After) and tags every request with the
// configured User-Agent.
type Fetcher struct {
	client    *httpx.Client
	userAgent string
}

func NewFetcher(client *httpx.Client, userAgent string) *Fetcher {
	return &Fetcher{client: client, userAgent: userAgent}
}

func (f *Fetcher) headers() map[string]string {
	return map[string]string{
		"User-Agent": f.userAgent,
		"Accept":     "application/json",
	}
}

// FetchThread retrieves a thread via old.reddit.com's .json endpoint with a
// generous limit and depth. Returns the raw response body or an error.
func (f *Fetcher) FetchThread(ctx context.Context, permalink string) ([]byte, error) {
	u := "https://old.reddit.com" + strings.TrimSuffix(permalink, "/") + ".json?limit=500&depth=20&sort=top&raw_json=1"
	resp, err := f.client.DoRetry(ctx, http.MethodGet, u, nil, f.headers(), httpx.RetryConfig{})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20)) // 20MB cap
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("old.reddit.com returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("response not JSON (likely bot-blocked): %s", truncate(string(body), 200))
	}
	return body, nil
}

// FetchMoreChildren expands collapsed reply branches via /api/morechildren.
// linkID must include the t3_ prefix; childIDs are bare IDs (no prefix).
func (f *Fetcher) FetchMoreChildren(ctx context.Context, linkID string, childIDs []string) ([]byte, error) {
	q := url.Values{}
	q.Set("api_type", "json")
	q.Set("link_id", linkID)
	q.Set("children", strings.Join(childIDs, ","))
	q.Set("limit_children", "false")
	q.Set("sort", "top")
	q.Set("raw_json", "1")
	u := "https://api.reddit.com/api/morechildren?" + q.Encode()

	resp, err := f.client.DoRetry(ctx, http.MethodGet, u, nil, f.headers(), httpx.RetryConfig{})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("api.reddit.com returned %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("response not JSON: %s", truncate(string(body), 200))
	}
	return body, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
