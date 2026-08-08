package lightpanda

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestIntegrationReddit exercises the real chromedp driver against a live
// Lightpanda CDP server. It is skipped unless OMNIFEED_TEST_LIGHTPANDA_CDP_URL is
// set (so `make check` stays hermetic), e.g.:
//
//	lightpanda serve --host 127.0.0.1 --port 9222 &
//	OMNIFEED_TEST_LIGHTPANDA_CDP_URL=ws://127.0.0.1:9222 go test ./internal/browser/lightpanda -run Integration -v
//
// It reproduces the Reddit path: navigate a subreddit (clearing the bot-wall JS
// challenge) then run a same-origin in-page fetch of the .json endpoint.
func TestIntegrationReddit(t *testing.T) {
	wsURL := os.Getenv("OMNIFEED_TEST_LIGHTPANDA_CDP_URL")
	if wsURL == "" {
		t.Skip("set OMNIFEED_TEST_LIGHTPANDA_CDP_URL to run the live Lightpanda test")
	}

	b := New(wsURL)
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := b.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	if err := sess.Navigate(ctx, "https://www.reddit.com/r/golang/"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}

	// Same-origin in-page fetch — the exact shape the Reddit engine uses.
	out, err := sess.Eval(ctx, `const r = await fetch("https://www.reddit.com/r/golang/top.json?limit=2&raw_json=1", {headers: {"Accept": "application/json"}}); return JSON.stringify({s: r.status, b: await r.text()});`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	var env struct {
		S int    `json:"s"`
		B string `json:"b"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode envelope %q: %v", out, err)
	}
	if env.S != 200 {
		t.Fatalf("reddit fetch status = %d, body = %.200s", env.S, env.B)
	}
	if !strings.Contains(env.B, `"Listing"`) {
		t.Fatalf("reddit body is not a Listing: %.200s", env.B)
	}

	// Session reuse: a second Eval without re-navigating must hit the live page.
	navsBefore := time.Now()
	if err := sess.Navigate(ctx, "https://www.reddit.com/r/golang/"); err != nil {
		t.Fatalf("re-Navigate (should be a no-op): %v", err)
	}
	if elapsed := time.Since(navsBefore); elapsed > 2*time.Second {
		t.Errorf("re-Navigate to the same URL took %s — expected a near-instant no-op", elapsed)
	}
}

// A network-level navigation failure must surface as an error. Lightpanda
// replies to Page.navigate with err == nil and only errorText set (verified
// empirically: "OperationTimedout" for unreachable hosts), the page stays on
// about:blank, and a driver that discards errorText reports success against the
// wrong page — the exact bug this test pins.
func TestIntegrationNavigateFailureSurfaces(t *testing.T) {
	wsURL := os.Getenv("OMNIFEED_TEST_LIGHTPANDA_CDP_URL")
	if wsURL == "" {
		t.Skip("set OMNIFEED_TEST_LIGHTPANDA_CDP_URL to run the live Lightpanda test")
	}

	b := New(wsURL)
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sess, err := b.Open(ctx)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sess.Close(context.Background()) }()

	err = sess.Navigate(ctx, "http://nonexistent-domain-3f9d2c.invalid/")
	if err == nil {
		t.Fatal("Navigate to an unresolvable host must fail, got success")
	}
	t.Logf("Navigate failed as expected: %v", err)
}
