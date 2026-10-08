package crawl4ai

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/kinorai/omnifeed/internal/antibot"
	"github.com/kinorai/omnifeed/internal/domain"
)

// maxVerdictChars bounds how much of crawl4ai's raw error_message is kept in
// the error detail. The message is unscrubbed since crawl4ai 0.9.3 (it can
// carry a stack-trace tail or page text), so it is cut short and flattened.
const maxVerdictChars = 300

// Stable patterns in crawl4ai's per-result error_message (0.9.3+, PR #2134) and
// in the `detail` of its 502/504 bodies. crawl4ai's wording is free text, so
// each family is matched on the phrases its detector and Playwright have kept
// across releases, not on whole sentences.
var (
	// HTTP status named inside the message: "HTTP 403", "status 429",
	// "status code: 503", "403 Forbidden".
	status403Pattern = regexp.MustCompile(`(?i)\b(?:http|status(?: code)?)\s*:?\s*403\b|\b403 forbidden\b`)
	status429Pattern = regexp.MustCompile(`(?i)\b(?:http|status(?: code)?)\s*:?\s*429\b|\btoo many requests\b|\brate[- ]limit`)
	timeoutPattern   = regexp.MustCompile(`(?i)exceeded the time limit|\btime(?:d)?[ -]?out\b|net::err_timed_out|net::err_connection_timed_out`)
	captchaPattern   = regexp.MustCompile(`(?i)captcha|just a moment|turnstile|verify(?:ing)? (?:you are|you're) (?:a )?human`)
	botBlockPattern  = regexp.MustCompile(`(?i)anti-?bot|\bblocked\b|cloudflare|akamai|perimeterx|datadome|incapsula|imperva|access denied`)
	// urlPattern strips the URLs Playwright embeds in its messages ("… at
	// https://blog.cloudflare.com/…") so a host or path never reads as a verdict.
	urlPattern = regexp.MustCompile(`(?i)\bhttps?://\S+`)
)

// classifyVerdict maps a crawl4ai failure message (a result's error_message, a
// top-level `error`, or a 502/504 `detail`) onto the FailureKind taxonomy.
// Precedence runs from the most specific verdict to the least:
//
//  1. structural content-gate (minimal_text, no <body>, …) → thin_content —
//     checked first because crawl4ai stamps it "Blocked by anti-bot
//     protection: Structural: …", which would otherwise read as a wall;
//  2. time limit / timeout / net::ERR_TIMED_OUT → timeout;
//  3. CAPTCHA / Cloudflare "just a moment" / Turnstile → captcha (a bare
//     "Cloudflare JS challenge" stays bot_block, as on the 5xx path);
//  4. HTTP 429 / too many requests → http_429;
//  5. HTTP 403 / forbidden → http_403;
//  6. anti-bot / blocked / named WAF vendor → bot_block;
//  7. anything else (HTTP 5xx, net::ERR_*, crashes) → upstream_error.
//
// pageStatus is the result's own status_code (0 when unknown): it breaks the
// tie when the message names nothing but the page itself answered 403/429.
func classifyVerdict(msg string, pageStatus int) domain.FailureKind {
	msg = urlPattern.ReplaceAllString(msg, " ")
	switch {
	case antibot.IsStructuralBlock(msg):
		return domain.KindThinContent
	case timeoutPattern.MatchString(msg):
		return domain.KindTimeout
	case captchaPattern.MatchString(msg):
		return domain.KindCaptcha
	case status429Pattern.MatchString(msg):
		return domain.KindHTTP429
	case status403Pattern.MatchString(msg):
		return domain.KindHTTP403
	case botBlockPattern.MatchString(msg):
		return domain.KindBotBlock
	}
	if _, blocked := antibot.Detect(msg); blocked {
		return domain.KindCaptcha
	}
	if pageStatus == http.StatusForbidden || pageStatus == http.StatusTooManyRequests {
		return domain.KindForStatus(pageStatus)
	}
	return domain.KindUpstreamError
}

// sanitizeVerdict flattens a raw crawl4ai message for an error detail: control
// characters and newlines become spaces, runs of whitespace collapse, and the
// result is cut to maxVerdictChars on a rune boundary.
func sanitizeVerdict(msg string) string {
	msg = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, msg)
	return truncate(strings.Join(strings.Fields(msg), " "), maxVerdictChars)
}

// errorDetail extracts FastAPI's `detail` string from a crawl4ai error body —
// the shape of its 502 (/md, /llm: the result's error_message) and 504 ("Crawl
// exceeded the time limit") responses. "" when the body is not that shape.
func errorDetail(body string) string {
	var v struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal([]byte(body), &v) != nil || len(v.Detail) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(v.Detail, &s) != nil {
		return ""
	}
	return s
}
