// Package antibot recognizes bot-wall / CAPTCHA / challenge pages that
// upstreams (Cloudflare, Reddit's "network security" wall, PerimeterX, …)
// return in place of real content — frequently with an HTTP 200, so a crawl
// that "succeeded" can still be a block. Detect lets engines reclassify those
// as failures (a domain.FetchError of kind captcha) instead of handing a
// challenge page back to the caller, and surfaces the matched marker for
// metrics/logs.
package antibot

import (
	"net/http"
	"strings"
)

// scanLimit bounds how much of a body Detect inspects. Challenge pages are
// tiny and their tell-tale markers appear near the top, so scanning the first
// 64KiB catches every real wall while keeping the lower-casing cost off the
// hot path for large legitimate pages (and avoiding deep-in-content false hits).
const scanLimit = 64 << 10

// markers are lower-case substrings that, when present in a fetched body,
// indicate a bot wall / human-verification challenge rather than real content.
// Keep this list conservative: every entry is matched against
// attacker-influenced page text, so over-broad phrases ("error", "denied")
// would misclassify real pages. Visible-text markers survive crawl4ai's
// markdown filtering; the markup/script markers only survive on raw HTML (the
// Reddit in-page fetch path). Add new walls here as they are observed.
var markers = []string{
	// Reddit network-security wall (Anubis-style) — the current reddit.com block.
	"you've been blocked by network security",
	"prove you're a human",
	"whoa there, pardner", // legacy reddit rate-limit interstitial
	// Cloudflare (interstitial / Turnstile / managed challenge).
	"just a moment...",
	"attention required! | cloudflare",
	"checking if the site connection is secure",
	"verify you are human",
	"verifying you are human",
	"/cdn-cgi/challenge-platform/",
	// PerimeterX/HUMAN, DataDome, Imperva/Incapsula.
	"pardon our interruption",
	"please verify you are a human",
	"request unsuccessful. incapsula incident id",
	// Generic CAPTCHA widgets (raw-HTML path).
	"g-recaptcha",
	"recaptcha/api.js",
	"hcaptcha.com/captcha",
	// Google / YouTube "unusual traffic" interstitial.
	"our systems have detected unusual traffic",
	"sign in to confirm you're not a bot",
}

// blockResponseMarker is the verdict crawl4ai stamps into its error RESPONSE
// body when its own anti-bot / structural detector rejects a page (e.g. "Blocked
// by anti-bot protection: Structural: minimal_text on small page"). Unlike the
// markers above (which appear in page CONTENT), this appears in crawl4ai's 5xx
// error body.
const blockResponseMarker = "blocked by anti-bot protection"

// IsBlockResponse reports whether an upstream error body is crawl4ai's anti-bot
// block verdict — a non-transient content block (served as a 5xx), not a fault.
func IsBlockResponse(body string) bool {
	return strings.Contains(strings.ToLower(body), blockResponseMarker)
}

// structuralMarkers identify crawl4ai's own content-gate verdicts: it rejected
// the page not because of a real wall but because it rendered too little usable
// content — a JS-only SPA shell ("no <body> tag"), a PDF/binary the headless
// browser can't extract, or a near-empty page ("minimal_text"). crawl4ai labels
// these "Structural: <reason>" and ships them carrying the same
// blockResponseMarker phrase as genuine walls, so IsBlockResponse alone can't
// tell them apart.
var structuralMarkers = []string{
	"structural:",
	"minimal_text",
	"no_content_elements",
	"no <body",
}

// IsStructuralBlock reports whether a crawl4ai block verdict is its own
// content-gate (a thin / empty / unparseable render) rather than a genuine
// anti-bot wall. Only meaningful when IsBlockResponse(body) is already true.
func IsStructuralBlock(body string) bool {
	lower := strings.ToLower(body)
	for _, m := range structuralMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// RetryableStatus is an httpx.RetryConfig predicate shared by the Reddit and
// generic crawl paths: retry genuine transient 429/5xx, but never a crawl4ai
// application-level 500. crawl4ai 0.9.2+ reserves plain 500 for its own crawl
// verdicts (blocks, content-gates, crashes) and scrubs the reason from the
// body (server.py: "Deliberate operational statuses (502/503/504 …) pass
// through") — so a 500 is deterministic per page and re-driving it just pays
// the full crawl cost again. 502/503/504 (infra hops) stay retryable unless
// the body carries an explicit block verdict (older crawl4ai versions).
func RetryableStatus(status int, body string) bool {
	if status == http.StatusInternalServerError {
		return false
	}
	return status < 500 || !IsBlockResponse(body)
}

// IsScrubbedServerError reports whether body is the generic 500 crawl4ai
// 0.9.2+ returns for its application-level failures ({"error":"Internal
// server error","correlation_id":"…"}) after scrubbing the real verdict —
// block wall, content-gate, or crash — into its own server log.
func IsScrubbedServerError(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "internal server error") && strings.Contains(lower, "correlation_id")
}

// Detect reports whether body looks like a bot wall / challenge page and, if
// so, the marker that matched (for logging/metrics). Matching is
// case-insensitive and bounded to the first scanLimit bytes.
func Detect(body string) (marker string, blocked bool) {
	if len(body) > scanLimit {
		body = body[:scanLimit]
	}
	lower := strings.ToLower(body)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return m, true
		}
	}
	return "", false
}
