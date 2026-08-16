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

	"github.com/kinorai/omnifeed/internal/antibot"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// The generic engine renders through a headless browser, but non-HTML text
// (raw code files, JSON, markdown, plain text) has nothing for a browser to
// render — Chromium's page-idle machinery just spins on it (raw
// .githubusercontent.com files measured at 30–39s vs ~200ms for a plain GET).
// For URLs whose path extension looks raw (see rawTextExts), rawText issues a
// direct GET and lets the response's Content-Type decide: non-HTML text is
// returned as-is (truncated at maxRawBodyBytes if huge); anything uncertain —
// fetch failure, non-200, HTML, binary, restricted egress — closes the body
// and falls back to the browser path silently, so the bypass can never make a
// fetch fail that would have succeeded before.

const (
	// rawFetchTimeout bounds the direct GET including the body read.
	rawFetchTimeout = 30 * time.Second
	// maxRawBodyBytes mirrors the crawl4ai response cap.
	maxRawBodyBytes = 10 << 20
	// rawUserAgent matches the identity the direct-API engines send.
	rawUserAgent = "omnifeed"
)

// rawTextExts are the path extensions that justify attempting the direct GET.
// The response's content-type verdict still decides — a .md URL served as
// text/html takes the browser path — but ordinary pages (no extension, .html,
// …) skip the attempt entirely, so the bypass adds zero latency to them. The
// cost of the gate: an extensionless raw URL (a JSON API path, a bare LICENSE
// file) keeps the browser path it has today.
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
	if !hasRawTextExt(rawURL) {
		return domain.Document{}, false
	}
	return e.rawTextForce(ctx, rawURL)
}

// rawTextForce is rawText without the path-extension gate. The gate exists to
// keep the bypass off the latency path of ordinary pages; it also excludes
// every extensionless API endpoint (itunes.apple.com/search?term=…,
// public.api.bsky.app/xrpc/…), which is exactly the shape crawl4ai's content
// gate rejects — a browser has nothing to render in a JSON body. Callers use
// this only AFTER the browser path has already failed, where there is no
// latency left to protect and the response's Content-Type is still the judge.
func (e *Engine) rawTextForce(ctx context.Context, rawURL string) (domain.Document, bool) {
	if e.direct == nil {
		return domain.Document{}, false
	}
	headers := map[string]string{"User-Agent": rawUserAgent}

	fetchCtx, cancel := context.WithTimeout(ctx, rawFetchTimeout)
	defer cancel()
	// MaxAttempts 1: a retry here would only delay the browser fallback, which
	// handles flaky hosts better anyway. The Content-Type check runs on the
	// response HEADERS, before any body read, so an HTML page costs one
	// aborted GET — no separate HEAD probe (HEAD-hostile hosts would forfeit
	// the bypass, and the GET's verdict is the one that covers the bytes).
	resp, err := e.direct.DoRetry(fetchCtx, http.MethodGet, rawURL, nil, headers, httpx.RetryConfig{MaxAttempts: 1})
	if err != nil {
		return domain.Document{}, false
	}
	// Close without draining on every exit: a mismatched or oversized body may
	// be arbitrarily large, and abandoning the connection is cheaper than
	// pulling gigabytes just to keep it reusable.
	defer func() { _ = resp.Body.Close() }()
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
	// A wall can answer 200 with a non-HTML body (a JSON or text/plain
	// challenge payload), which the status and Content-Type checks above both
	// wave through. crawlOnce screens every body it returns this way; a body
	// that skipped the browser must not skip the screening. Declining here is
	// the right answer for both callers: the pre-crawl bypass falls through to
	// the browser, and the post-failure rescue keeps the original error rather
	// than reporting a wall as a successful crawl.
	if _, blocked := antibot.Detect(content); blocked {
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
