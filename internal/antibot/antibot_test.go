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

// RetryableStatus (and IsBlockResponse) must veto retrying a crawl4ai anti-bot
// 5xx while still retrying genuine transient faults.
func TestRetryableStatus(t *testing.T) {
	const block = `{"error":"Blocked by anti-bot protection: Structural: minimal_text"}`
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{500, block, false},                 // anti-bot 5xx → don't retry
		{503, block, false},                 // any 5xx carrying the marker
		{500, `{"error":"internal"}`, true}, // generic transient 5xx → retry
		{429, "rate limited", true},         // 429 → retry
		{200, block, true},                  // <500 is never vetoed here
	}
	for _, tc := range cases {
		if got := RetryableStatus(tc.status, tc.body); got != tc.want {
			t.Errorf("RetryableStatus(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
		}
	}
}
