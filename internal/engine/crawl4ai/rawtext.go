package crawl4ai

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// The generic engine renders through a headless browser, but non-HTML text
// (raw code files, JSON, markdown, plain text) has nothing for a browser to
// render — Chromium's page-idle machinery just spins on it (raw
// .githubusercontent.com files measured at 30–39s vs ~200ms for a plain GET).
// rawText sniffs the content type with a cheap HEAD probe (spent only on URLs
// whose path extension looks raw — see rawTextExts) and fetches such URLs
// directly. Anything uncertain — probe failure, non-200, HTML, binary,
// oversized, restricted egress — falls back to the browser path silently, so
// the bypass can never make a fetch fail that would have succeeded before.

const (
	// rawProbeTimeout bounds the HEAD probe; a site slower than this gets the
	// browser path, which is no worse than before the bypass existed.
	rawProbeTimeout = 10 * time.Second
	// rawFetchTimeout bounds the direct GET including the body read.
	rawFetchTimeout = 30 * time.Second
	// maxRawBodyBytes mirrors the crawl4ai response cap.
	maxRawBodyBytes = 10 << 20
	// rawUserAgent matches the identity the direct-API engines send.
	rawUserAgent = "omnifeed"
)

// rawTextExts are the path extensions that justify spending a HEAD probe. The
// probe's content-type verdict still decides — a .md URL served as text/html
// takes the browser path — but ordinary pages (no extension, .html, …) skip
// the probe entirely, so the bypass adds zero latency to them. The cost of the
// gate: an extensionless raw URL (a JSON API path, a bare LICENSE file) keeps
// the browser path it has today.
var rawTextExts = map[string]bool{
	".md": true, ".markdown": true, ".txt": true, ".text": true, ".rst": true, ".adoc": true,
	".json": true, ".ndjson": true, ".jsonl": true, ".xml": true, ".yaml": true, ".yml": true,
	".toml": true, ".ini": true, ".cfg": true, ".conf": true, ".csv": true, ".tsv": true,
	".log": true, ".patch": true, ".diff": true, ".proto": true, ".graphql": true, ".sql": true,
	".go": true, ".py": true, ".rb": true, ".rs": true, ".c": true, ".h": true, ".cpp": true,
	".hpp": true, ".cc": true, ".java": true, ".kt": true, ".swift": true, ".ts": true,
	".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".sh": true, ".bash": true,
	".zsh": true, ".ps1": true, ".lua": true, ".pl": true, ".ex": true, ".exs": true,
	".zig": true, ".tf": true, ".hcl": true, ".dockerfile": true, ".mod": true, ".sum": true,
	".lock": true, ".env": true, ".service": true, ".css": true,
}

// hasRawTextExt reports whether the URL's path extension is in rawTextExts.
func hasRawTextExt(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return rawTextExts[strings.ToLower(path.Ext(u.Path))]
}

// isRawTextType reports whether a media type is plain text an LLM can consume
// as-is, with no browser rendering step in between. text/html and its XHTML
// twin are exactly what the browser path is for; everything else under text/,
// the common structured-text application types, and the +json/+xml suffix
// families are raw.
func isRawTextType(mediaType string) bool {
	switch mediaType {
	case "text/html", "application/xhtml+xml":
		return false
	case "application/json", "application/xml", "application/x-ndjson",
		"application/javascript", "application/yaml", "application/x-yaml",
		"application/toml":
		return true
	}
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	return strings.HasSuffix(mediaType, "+json") || strings.HasSuffix(mediaType, "+xml")
}

// rawContentType extracts and normalizes the media type of a response, or ""
// when the header is absent/unparseable (unparseable ⇒ browser path).
func rawContentType(resp *http.Response) string {
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	return strings.ToLower(mediaType)
}

// rawText attempts the direct fetch. ok is false whenever the URL should take
// the browser path instead — never an error: the browser path is the fallback
// for every uncertainty here.
func (e *Engine) rawText(ctx context.Context, rawURL string) (domain.Document, bool) {
	if e.direct == nil || !hasRawTextExt(rawURL) {
		return domain.Document{}, false
	}
	headers := map[string]string{"User-Agent": rawUserAgent}

	probeCtx, cancel := context.WithTimeout(ctx, rawProbeTimeout)
	defer cancel()
	// MaxAttempts 1: a retry here would delay the browser fallback, which
	// handles flaky hosts better anyway.
	probe, err := e.direct.DoRetry(probeCtx, http.MethodHead, rawURL, nil, headers, httpx.RetryConfig{MaxAttempts: 1})
	if err != nil {
		return domain.Document{}, false
	}
	_, _ = io.Copy(io.Discard, probe.Body)
	_ = probe.Body.Close()
	if probe.StatusCode != http.StatusOK || !isRawTextType(rawContentType(probe)) {
		return domain.Document{}, false
	}

	fetchCtx, cancelFetch := context.WithTimeout(ctx, rawFetchTimeout)
	defer cancelFetch()
	resp, err := e.direct.DoRetry(fetchCtx, http.MethodGet, rawURL, nil, headers, httpx.RetryConfig{MaxAttempts: 1})
	if err != nil {
		return domain.Document{}, false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	// Re-check on the GET: servers may answer HEAD and GET differently, and
	// only the GET's verdict covers the bytes we're about to return.
	if resp.StatusCode != http.StatusOK || !isRawTextType(rawContentType(resp)) {
		return domain.Document{}, false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRawBodyBytes))
	if err != nil {
		return domain.Document{}, false
	}
	content := string(body)
	// Binary masquerading as text, or nothing at all: let the browser try.
	if !utf8.ValidString(content) || strings.TrimSpace(content) == "" {
		return domain.Document{}, false
	}

	return domain.Document{
		PageContent: content,
		Metadata: map[string]string{
			"source":      rawURL,
			"status_code": "200",
			// Reported as markdown: raw text shares its property that matters
			// downstream — generic char truncation is safe (domain.Truncatable-
			// ContentType keys on it).
			domain.ContentTypeKey: domain.ContentTypeMarkdown,
		},
	}, true
}
