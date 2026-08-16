package crawl4ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// newRawTestEngine returns an engine whose crawl4ai endpoint records whether
// the browser path was hit, so tests can assert bypass-vs-fallback decisions.
func newRawTestEngine(t *testing.T) (*Engine, *bool) {
	t.Helper()
	browserHit := false
	c4a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		browserHit = true
		_, _ = w.Write([]byte(`{"success":true,"results":[{"success":true,"status_code":200,"markdown":{"raw_markdown":"# browser"}}]}`))
	}))
	t.Cleanup(c4a.Close)
	e := New(Config{
		Endpoint: c4a.URL,
		Client:   httpx.New(nil),
		Limiter:  httpx.NewDomainLimiter(2, 0),
	})
	return e, &browserHit
}

// A raw-extension URL served with a raw content type must be fetched directly
// — no crawl4ai (browser) round-trip at all.
func TestRawTextBypassesBrowser(t *testing.T) {
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("package main\n\nfunc main() {}\n"))
	}))
	defer raw.Close()

	e, browserHit := newRawTestEngine(t)
	doc, err := e.Crawl(context.Background(), raw.URL+"/main.go", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl() error = %v", err)
	}
	if *browserHit {
		t.Error("crawl4ai endpoint was hit, want direct fetch only")
	}
	if !strings.Contains(doc.PageContent, "func main()") {
		t.Errorf("PageContent = %q, want the raw file body", doc.PageContent)
	}
	if doc.Metadata[domain.ContentTypeKey] != domain.ContentTypeMarkdown {
		t.Errorf("content type = %q, want %q (truncatable)", doc.Metadata[domain.ContentTypeKey], domain.ContentTypeMarkdown)
	}
}

// The content-type verdict overrides the extension: a .md URL served as
// text/html is a rendered page and must take the browser path.
func TestRawTextHTMLContentTypeFallsBack(t *testing.T) {
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body>rendered</body></html>"))
	}))
	defer raw.Close()

	e, browserHit := newRawTestEngine(t)
	doc, err := e.Crawl(context.Background(), raw.URL+"/post.md", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl() error = %v", err)
	}
	if !*browserHit {
		t.Error("crawl4ai endpoint not hit, want browser fallback for text/html")
	}
	if doc.PageContent != "# browser" {
		t.Errorf("PageContent = %q, want the browser-path document", doc.PageContent)
	}
}

// Direct-fetch failures must fall back silently — a deployment whose egress
// policy blocks direct fetches keeps working exactly as before the bypass
// existed.
func TestRawTextFetchFailureFallsBack(t *testing.T) {
	e, browserHit := newRawTestEngine(t)
	// Closed port: the direct GET gets connection refused.
	dead := httptest.NewServer(http.HandlerFunc(nil))
	deadURL := dead.URL
	dead.Close()

	doc, err := e.Crawl(context.Background(), deadURL+"/notes.txt", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl() error = %v", err)
	}
	if !*browserHit {
		t.Error("crawl4ai endpoint not hit, want browser fallback on fetch failure")
	}
	if doc.PageContent != "# browser" {
		t.Errorf("PageContent = %q, want the browser-path document", doc.PageContent)
	}
}

// URLs without a raw-looking extension must not even be attempted directly:
// ordinary pages pay zero extra latency for the bypass.
func TestRawTextNoExtensionSkipsDirectFetch(t *testing.T) {
	fetched := false
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetched = true
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("raw"))
	}))
	defer raw.Close()

	e, browserHit := newRawTestEngine(t)
	if _, err := e.Crawl(context.Background(), raw.URL+"/article", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl() error = %v", err)
	}
	if fetched {
		t.Error("extensionless URL was fetched directly, want no direct request at all")
	}
	if !*browserHit {
		t.Error("crawl4ai endpoint not hit, want browser path")
	}
}

// A binary body behind a raw content type (mislabeled upstream) must fall back
// rather than hand the caller undecodable bytes.
func TestRawTextBinaryBodyFallsBack(t *testing.T) {
	raw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte{0xff, 0xfe, 0x00, 0x01, 0x02})
	}))
	defer raw.Close()

	e, browserHit := newRawTestEngine(t)
	doc, err := e.Crawl(context.Background(), raw.URL+"/data.txt", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl() error = %v", err)
	}
	if !*browserHit {
		t.Error("crawl4ai endpoint not hit, want browser fallback for invalid UTF-8")
	}
	if doc.PageContent != "# browser" {
		t.Errorf("PageContent = %q, want the browser-path document", doc.PageContent)
	}
}

// isRawTextType: the browser types are excluded, text/* and the structured
// families are raw.
func TestIsRawTextType(t *testing.T) {
	for mt, want := range map[string]bool{
		"text/html":             false,
		"application/xhtml+xml": false,
		"":                      false,
		"image/png":             false,
		"application/pdf":       false,
		"text/plain":            true,
		"text/markdown":         true,
		"text/csv":              true,
		"application/json":      true,
		"application/ld+json":   true,
		"application/atom+xml":  true,
		"application/yaml":      true,
	} {
		if got := isRawTextType(mt); got != want {
			t.Errorf("isRawTextType(%q) = %v, want %v", mt, got, want)
		}
	}
}

// newRejectingEngine returns an engine whose crawl4ai endpoint always answers
// with the scrubbed 500 that means "I could not render this page" — the
// verdict every JSON API response gets, since a browser has nothing to render.
func newRejectingEngine(t *testing.T) (*Engine, *int) {
	t.Helper()
	browserHits := 0
	c4a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		browserHits++
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"Internal server error","correlation_id":"be95648aa6d5"}`))
	}))
	t.Cleanup(c4a.Close)
	e := New(Config{
		Endpoint: c4a.URL,
		Client:   httpx.New(nil),
		Limiter:  httpx.NewDomainLimiter(2, 0),
	})
	return e, &browserHits
}

// An extensionless JSON API path is not claimed by the pre-crawl bypass, so it
// takes the browser path and crawl4ai rejects it. The rescue must then fetch it
// directly and return the body instead of the error.
func TestRejectedJSONAPIIsRescued(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resultCount":1,"results":[{"trackName":"Octal"}]}`))
	}))
	defer api.Close()

	e, browserHits := newRejectingEngine(t)
	doc, err := e.Crawl(context.Background(), api.URL+"/search?term=hacker+news", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl() error = %v, want the direct fetch to rescue it", err)
	}
	if *browserHits == 0 {
		t.Error("browser path was skipped, want it tried first (the rescue is a fallback)")
	}
	if !strings.Contains(doc.PageContent, "Octal") {
		t.Errorf("PageContent = %q, want the JSON body", doc.PageContent)
	}
}

// The rescue must not turn a genuine page failure into a success: an HTML body
// is what the browser path exists for, so the original error stands.
func TestRejectedHTMLKeepsOriginalError(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body>a real page</body></html>`))
	}))
	defer page.Close()

	e, _ := newRejectingEngine(t)
	_, err := e.Crawl(context.Background(), page.URL+"/article", domain.EngineOptions{})

	var fe *domain.FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("want *domain.FetchError, got %T: %v", err, err)
	}
	if fe.Kind != domain.KindUpstreamRejected {
		t.Errorf("Kind = %q, want %q preserved", fe.Kind, domain.KindUpstreamRejected)
	}
}

// A bot wall must NOT be retried directly: the same wall would answer a plain
// GET, so the retry only spends latency.
func TestBlockedPageIsNotRescued(t *testing.T) {
	direct := 0
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		direct++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer page.Close()

	c4a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"results":[{"success":true,"status_code":429,"markdown":{"raw_markdown":"rate limited"}}]}`))
	}))
	defer c4a.Close()

	e := New(Config{Endpoint: c4a.URL, Client: httpx.New(nil), Limiter: httpx.NewDomainLimiter(2, 0)})
	if _, err := e.Crawl(context.Background(), page.URL+"/api/thing", domain.EngineOptions{}); err == nil {
		t.Fatal("Crawl() = nil error, want the 429 to stand")
	}
	if direct != 0 {
		t.Errorf("direct fetches = %d, want 0 (a wall is not rescuable)", direct)
	}
}
