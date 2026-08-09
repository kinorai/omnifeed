package antibot

import "testing"

func TestDetectBlocked(t *testing.T) {
	blocked := []string{
		"<title>Just a moment...</title>",
		"You've been blocked by network security",
		"we need you to prove you're a human before continuing",
		"Pardon Our Interruption",
		`<div class="g-recaptcha" data-sitekey="x"></div>`,
		"Our systems have detected unusual traffic from your network",
	}
	for _, b := range blocked {
		if marker, ok := Detect(b); !ok {
			t.Errorf("Detect(%q) = false, want true", b)
		} else if marker == "" {
			t.Errorf("Detect(%q) returned empty marker", b)
		}
	}
}

func TestDetectClean(t *testing.T) {
	clean := "# Garden tomatoes\n\nWater deeply twice a week and stake the plants early. " +
		"This is ordinary article content with no challenge text whatsoever."
	if marker, ok := Detect(clean); ok {
		t.Errorf("Detect(clean) matched %q, want no match", marker)
	}
}

// RetryableStatus must veto retrying every crawl4ai plain 500 — 0.9.2+
// reserves that status for its own deterministic crawl verdicts and scrubs
// the reason from the body, so a generic-looking 500 is NOT transient — while
// still retrying 429 and the pass-through infra statuses (502/503/504) unless
// their body carries an explicit block verdict.
func TestRetryableStatus(t *testing.T) {
	const block = `{"error":"Blocked by anti-bot protection: Structural: minimal_text"}`
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{500, block, false},                  // anti-bot 5xx → don't retry
		{503, block, false},                  // any 5xx carrying the marker
		{500, `{"error":"internal"}`, false}, // scrubbed verdict channel → never retry
		{500, `{"error":"Internal server error","correlation_id":"abc123"}`, false},
		{503, `{"error":"upstream hiccup"}`, true}, // infra pass-through → retry
		{429, "rate limited", true},               // 429 → retry
		{200, block, true},                        // <500 is never vetoed here
	}
	for _, tc := range cases {
		if got := RetryableStatus(tc.status, tc.body); got != tc.want {
			t.Errorf("RetryableStatus(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

// IsStructuralBlock distinguishes crawl4ai's own content-gate verdicts (thin /
// empty / unparseable renders — SPAs, PDFs, near-empty pages) from a genuine
// anti-bot wall carrying the same blockResponseMarker phrase.
func TestIsStructuralBlock(t *testing.T) {
	structural := []string{
		`{"error":"Blocked by anti-bot protection: Structural: minimal_text on small page (224 bytes, 9 chars visible)"}`,
		`{"error":"Blocked by anti-bot protection: Structural: no <body> tag (6300 bytes)"}`,
		`{"error":"Blocked by anti-bot protection: Structural: minimal_text, no_content_elements (237 bytes, 0 chars visible)"}`,
	}
	for _, b := range structural {
		if !IsStructuralBlock(b) {
			t.Errorf("IsStructuralBlock(%q) = false, want true", b)
		}
	}
	walls := []string{
		`{"error":"Blocked by anti-bot protection: Cloudflare JS challenge"}`,
		`{"error":"internal server error"}`,
	}
	for _, b := range walls {
		if IsStructuralBlock(b) {
			t.Errorf("IsStructuralBlock(%q) = true, want false", b)
		}
	}
}

// IsScrubbedServerError recognizes crawl4ai 0.9.2+'s generic 500 body (verdict
// scrubbed server-side under a correlation id) and nothing else.
func TestIsScrubbedServerError(t *testing.T) {
	if !IsScrubbedServerError(`{"error":"Internal server error","correlation_id":"415768e2265e"}`) {
		t.Error("scrubbed body not recognized")
	}
	for _, body := range []string{
		`{"error":"Blocked by anti-bot protection: Structural: minimal_text"}`,
		`{"error":"internal"}`,
		"Internal Server Error", // classic plain 500 page, no correlation id
		"",
	} {
		if IsScrubbedServerError(body) {
			t.Errorf("IsScrubbedServerError(%q) = true, want false", body)
		}
	}
}
