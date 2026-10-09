package crawl4ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// fakeCrawl serves one crawl4ai result per request from fn(attempt).
func fakeCrawl(t *testing.T, fn func(n int32, body map[string]any) map[string]any) (*Engine, *atomic.Int32) {
	return fakeCrawlCfg(t, Config{}, fn)
}

// fakeCrawlCfg is fakeCrawl with engine settings; Endpoint, Client and Limiter
// are filled in.
func fakeCrawlCfg(t *testing.T, cfg Config, fn func(n int32, body map[string]any) map[string]any) (*Engine, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		b, _ := json.Marshal(fn(n.Add(1), req))
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	cfg.Endpoint, cfg.Client, cfg.Limiter = srv.URL, httpx.New(nil), httpx.NewDomainLimiter(2, 0)
	return New(cfg), &n
}

func page(status int, md string) map[string]any {
	return map[string]any{"success": true, "results": []any{map[string]any{
		"success": true, "status_code": status, "markdown": map[string]any{"fit_markdown": md, "raw_markdown": md}}}}
}

const realProse = "This is a real article body with enough words to count as genuine prose content for a reader, well past the floor."

func kindOf(err error) domain.FailureKind {
	var fe *domain.FetchError
	if errors.As(err, &fe) {
		return fe.Kind
	}
	return ""
}

func TestPageStatusPropagates(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   domain.FailureKind
	}{{404, domain.KindNotFound}, {410, domain.KindNotFound}, {405, domain.KindBotBlock}, {401, domain.KindBotBlock}, {400, domain.KindError}, {503, domain.KindUpstreamError}} {
		e, _ := fakeCrawl(t, func(int32, map[string]any) map[string]any { return page(tc.status, "# Page Not Found\n"+realProse) })
		_, err := e.Crawl(context.Background(), deadTargetURL+"/x", domain.EngineOptions{})
		if kindOf(err) != tc.want {
			t.Errorf("status %d: kind = %q, want %q (err %v)", tc.status, kindOf(err), tc.want, err)
		}
	}
}

func TestInterstitialMarkers(t *testing.T) {
	for _, body := range []string{
		"#### Click the button below to continue shopping",
		"# Access Restricted\nWe’re unable to allow access to this page at this time.",
		"Sorry! Please make sure your browser is updated to the latest version.",
		"## Khan Academy does not support this browser.",
		"Temporary error.\nBefore proceeding to your request, you need to solve a puzzle\n# Let’s confirm you are human",
	} {
		e, _ := fakeCrawl(t, func(int32, map[string]any) map[string]any { return page(200, body+"\n"+realProse) })
		_, err := e.Crawl(context.Background(), deadTargetURL+"/x", domain.EngineOptions{})
		if kindOf(err) != domain.KindCaptcha {
			t.Errorf("%q: kind = %q, want captcha", body[:30], kindOf(err))
		}
	}
}

func TestNoRawHTMLFallbackAndProseFloor(t *testing.T) {
	floor := Config{MinProseChars: 100}
	// Empty markdown + cleaned_html must NOT be served as content.
	e, _ := fakeCrawlCfg(t, floor, func(int32, map[string]any) map[string]any {
		return map[string]any{"success": true, "results": []any{map[string]any{"success": true, "status_code": 200,
			"markdown": map[string]any{"fit_markdown": "", "raw_markdown": ""}, "cleaned_html": "<html><head><title>x</title></head><body></body></html>"}}}
	})
	if _, err := e.Crawl(context.Background(), deadTargetURL+"/x", domain.EngineOptions{}); kindOf(err) != domain.KindThinContent {
		t.Errorf("raw-html fallback: kind = %q, want thin_content", kindOf(err))
	}
	// A nav-only shell (Spotify-like) is below the prose floor; links/URLs don't count.
	shell := "[Skip to main content](https://open.spotify.com/x#main-view)\nPremiumSupportDownload[Install App](https://open.spotify.com/download)\nSign upLog in"
	e2, _ := fakeCrawlCfg(t, floor, func(int32, map[string]any) map[string]any { return page(200, shell) })
	if _, err := e2.Crawl(context.Background(), deadTargetURL+"/x", domain.EngineOptions{}); kindOf(err) != domain.KindThinContent {
		t.Errorf("nav shell: kind = %q, want thin_content", kindOf(err))
	}
	e3, _ := fakeCrawlCfg(t, floor, func(int32, map[string]any) map[string]any { return page(200, realProse) })
	if _, err := e3.Crawl(context.Background(), deadTargetURL+"/x", domain.EngineOptions{}); err != nil {
		t.Errorf("real prose rejected: %v", err)
	}
}

func TestNavigationRaceRetriesWithNetworkIdle(t *testing.T) {
	var waits []string
	e, n := fakeCrawl(t, func(i int32, req map[string]any) map[string]any {
		params := req["crawler_config"].(map[string]any)["params"].(map[string]any)
		waits = append(waits, params["wait_until"].(string))
		if i == 1 {
			return map[string]any{"success": true, "results": []any{map[string]any{"success": false, "status_code": 200,
				"error_message": "Page.content: Unable to retrieve content because the page is navigating and changing the content."}}}
		}
		return page(200, realProse)
	})
	doc, err := e.Crawl(context.Background(), deadTargetURL+"/x", domain.EngineOptions{})
	if err != nil || !strings.Contains(doc.PageContent, "real article") {
		t.Fatalf("Crawl() = %v, want success after retry", err)
	}
	if n.Load() != 2 || waits[1] != "networkidle" {
		t.Errorf("attempts=%d waits=%v, want 2 attempts and networkidle on the retry", n.Load(), waits)
	}
}

func TestOversizedResponseIsExplicit(t *testing.T) {
	big := strings.Repeat("a", maxResponseBytes+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"x":"` + big + `"}`))
	}))
	t.Cleanup(srv.Close)
	e := New(Config{Endpoint: srv.URL, Client: httpx.New(nil), Limiter: httpx.NewDomainLimiter(2, 0)})
	_, err := e.Crawl(context.Background(), deadTargetURL+"/x", domain.EngineOptions{})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("err = %v, want explicit size error", err)
	}
}

func TestConsentRedirectIsBlocked(t *testing.T) {
	e, _ := fakeCrawl(t, func(int32, map[string]any) map[string]any {
		p := page(200, "  * Français\n  * Deutsch\n"+realProse)
		p["results"].([]any)[0].(map[string]any)["redirected_url"] = "https://consent.youtube.com/m?continue=x"
		return p
	})
	if _, err := e.Crawl(context.Background(), deadTargetURL+"/x", domain.EngineOptions{}); kindOf(err) != domain.KindCaptcha {
		t.Errorf("consent redirect: kind = %q, want captcha", kindOf(err))
	}
}

func TestPDFUsesPDFStrategy(t *testing.T) {
	var strategy any
	e, _ := fakeCrawl(t, func(_ int32, req map[string]any) map[string]any {
		params := req["crawler_config"].(map[string]any)["params"].(map[string]any)
		strategy = params["scraping_strategy"]
		if _, ok := req["browser_config"]; ok {
			t.Error("PDF crawl must not send browser_config")
		}
		return page(200, "# Attention Is All You Need\n"+realProse)
	})
	if _, err := e.Crawl(context.Background(), deadTargetURL+"/paper.pdf", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl() = %v", err)
	}
	if m, _ := strategy.(map[string]any); m["type"] != "PDFContentScrapingStrategy" {
		t.Errorf("scraping_strategy = %v, want PDFContentScrapingStrategy", strategy)
	}
	for u, want := range map[string]bool{"https://arxiv.org/pdf/1706.03762": true, "https://x.org/a.PDF?dl=1": true, "https://x.org/pdf-guide": false, "https://arxiv.org/abs/1706.03762": false} {
		if got := looksLikePDF(u); got != want {
			t.Errorf("looksLikePDF(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestSilentFailureRules(t *testing.T) {
	for _, tc := range []struct {
		body string
		want domain.FailureKind
	}{
		{"Something went wrong.\nSign In\nPage Not Found\nWe searched everywhere but couldn't find the page you were looking for.\n" + strings.Repeat(realProse+"\n", 50), domain.KindNotFound},
		{"# 404 Not Found\nThe HTTP 404 Not Found client error response status code indicates that the server cannot find the requested resource. " + realProse, ""},
		// MDN's 404 reference: a "Page not found" section heading after prose.
		{"# 404 Not Found\nThe HTTP **`404 Not Found`** client error response status code indicates that the server cannot find the requested resource, and links to it are broken.\n## Status\n## Examples\n### [Page not found](https://x/#page_not_found)\n" + realProse, ""},
		{"  * English\n  * Français\nHello!\n# Choose a country.\n#### [Canada](https://www.bestbuy.ca/)\n#### [United States](https://www.bestbuy.com/)", domain.KindBotBlock},
		{realProse + "\n" + strings.Repeat("Page not found errors are discussed below. ", 20), ""},
		{"## Filter Results\n" + strings.Repeat("Loading\n", 40) + realProse, domain.KindThinContent},
		{realProse + "\nLoading\n" + realProse, ""},
		{"Powered and protected by\n![Akamai](https://www.akamai.com/site/ko/images/logo/akamai-logo1.svg)\n[Privacy](https://www.akamai.com/privacy)\n", domain.KindBotBlock},
		{strings.Repeat(realProse+"\n", 5) + "Powered and protected by Akamai", ""},
	} {
		if got, _ := silentFailure(tc.body, tc.body); got != tc.want {
			t.Errorf("silentFailure(%.50q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestConsentBypassURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://www.youtube.com/@veritasium/videos":  "https://www.youtube.com/@veritasium/videos?ucbcb=1",
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ": "https://www.youtube.com/watch?ucbcb=1&v=dQw4w9WgXcQ",
		"https://news.google.com/home?hl=en-US":       "https://news.google.com/home?hl=en-US&ucbcb=1",
		"https://example.com/youtube.com":             "https://example.com/youtube.com",
		"https://www.youtube.com/watch?v=x&ucbcb=1":   "https://www.youtube.com/watch?v=x&ucbcb=1",
		"https://notyoutube.com/x":                    "https://notyoutube.com/x",
	} {
		if got := consentBypassURL(in); got != want {
			t.Errorf("consentBypassURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedirectIsSurfaced(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		want     bool
	}{
		{"https://www.booking.com/hotel/fr/ritz-paris.html", "https://www.booking.com/city/fr/paris.fr.html", true},
		{"https://x.org/a", "https://www.x.org/a/", false},
		{"https://x.org/a?utm=1", "https://x.org/a?ucbcb=1#top", false},
		{"https://nature.com/", "https://idp.nature.com/authorize", true},
	} {
		if got := redirectedElsewhere(tc.from, tc.to); got != tc.want {
			t.Errorf("redirectedElsewhere(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
	e, _ := fakeCrawl(t, func(int32, map[string]any) map[string]any {
		p := page(200, "# Recherchez des hôtels à Paris\n"+realProse)
		p["results"].([]any)[0].(map[string]any)["redirected_url"] = deadTargetURL + "/city/paris"
		return p
	})
	doc, err := e.Crawl(context.Background(), deadTargetURL+"/hotel/ritz", domain.EngineOptions{})
	if err != nil || !strings.HasPrefix(doc.PageContent, "> Redirected from ") || doc.Metadata["final_url"] == "" {
		t.Errorf("redirect not surfaced: err=%v content=%.60q", err, doc.PageContent)
	}
}

func TestBlockRetriesOnDefaultFingerprint(t *testing.T) {
	var uas []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		var ua any
		if bc, ok := req["browser_config"].(map[string]any); ok {
			ua = bc["params"].(map[string]any)["user_agent"]
		}
		uas = append(uas, ua)
		body := page(200, realProse)
		if ua != nil { // the modern identity gets the Akamai interstitial
			body = page(200, "Powered and protected by\n![Akamai](https://www.akamai.com/logo.svg)")
		}
		b, _ := json.Marshal(body)
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	e := New(Config{Endpoint: srv.URL, Client: httpx.New(nil), Limiter: httpx.NewDomainLimiter(2, 0), UserAgent: "Mozilla/5.0 Chrome/153", Stealth: true})
	doc, err := e.Crawl(context.Background(), deadTargetURL+"/x", domain.EngineOptions{})
	if err != nil || !strings.Contains(doc.PageContent, "real article") {
		t.Fatalf("Crawl() = %v, want the default-fingerprint retry to succeed", err)
	}
	if len(uas) != 2 || uas[0] == nil || uas[1] != nil {
		t.Errorf("user agents sent = %v, want [modern, default]", uas)
	}
}

func TestAkamaiVendorReadFromHTML(t *testing.T) {
	// The markdown lost the logo; only the cleaned HTML names Akamai.
	if got, _ := silentFailure("Powered and protected by\n[Privacy](https://www.akamai.com/privacy)", `<img alt="Akamai" src="https://www.akamai.com/logo.svg">`); got != domain.KindBotBlock {
		t.Errorf("kind = %q, want bot_block", got)
	}
	if got, _ := silentFailure("Powered and protected by", "<p>Powered and protected by</p>"); got != "" {
		t.Errorf("no vendor named: kind = %q, want none", got)
	}
}

func TestRedirectToVerificationIsCaptcha(t *testing.T) {
	e, _ := fakeCrawl(t, func(int32, map[string]any) map[string]any {
		p := page(200, realProse)
		p["results"].([]any)[0].(map[string]any)["redirected_url"] = deadTargetURL + "/bgn_verification.html?verifyCode=x"
		return p
	})
	if _, err := e.Crawl(context.Background(), deadTargetURL+"/search_result.html", domain.EngineOptions{}); kindOf(err) != domain.KindCaptcha {
		t.Errorf("kind = %q, want captcha", kindOf(err))
	}
	for _, ok := range []string{"/questions/1/how-to-verify-gpg-signatures", "/docs/challenges-of-scale", "/verifying-builds", "/verify-email-guide"} {
		if verificationPath.MatchString(ok) {
			t.Errorf("verificationPath matched article path %q", ok)
		}
	}
	for _, wall := range []string{"/bgn_verification.html", "/sorry/index", "/captcha", "/verify", "/verify/", "/challenge"} {
		if !verificationPath.MatchString(wall) {
			t.Errorf("verificationPath missed wall path %q", wall)
		}
	}
}

func TestLoginRedirectIsBlock(t *testing.T) {
	e, _ := fakeCrawl(t, func(int32, map[string]any) map[string]any {
		p := page(200, realProse)
		p["results"].([]any)[0].(map[string]any)["redirected_url"] = deadTargetURL + "/login.html?from=x"
		return p
	})
	if _, err := e.Crawl(context.Background(), deadTargetURL+"/search_result.html", domain.EngineOptions{}); kindOf(err) != domain.KindBotBlock {
		t.Errorf("kind = %q, want bot_block", kindOf(err))
	}
	for p, want := range map[string]bool{"/login.html": true, "/authwall": true, "/signin": true, "/blog/login-best-practices": false, "/loginradius-review": false} {
		if loginPath.MatchString(p) != want {
			t.Errorf("loginPath(%q) = %v, want %v", p, !want, want)
		}
	}
}
