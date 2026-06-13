// Package antibot recognizes bot-wall / CAPTCHA / challenge pages that
// upstreams (Cloudflare, Reddit's "network security" wall, PerimeterX, …)
// return in place of real content — frequently with an HTTP 200, so a crawl
// that "succeeded" can still be a block. Detect lets engines reclassify those
// as failures (a domain.FetchError of kind captcha) instead of handing a
// challenge page back to the caller, and surfaces the matched marker for
// metrics/logs.
package antibot

import "strings"

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
