package crawl4ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// crawl094Failure is the crawl4ai 0.9.3+/0.9.4 /crawl shape when every URL
// failed (PR #2134): HTTP 200, top-level success:true, and the verdict only in
// results[0].success / error_message — next to the page's own markdown, which
// must never be served.
func crawl094Failure(msg string, status int) map[string]any {
	return map[string]any{
		"success": true,
		"results": []any{map[string]any{
			"url":           "https://example.com/page",
			"success":       false,
			"status_code":   status,
			"error_message": msg,
			"markdown":      map[string]any{"raw_markdown": "# Just a page body that must not be served"},
			"cleaned_html":  "<p>Just a page body that must not be served</p>",
		}},
	}
}

func serveJSON(t *testing.T, status int, body any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		b, _ := json.Marshal(body)
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func crawlFake(t *testing.T, srv *httptest.Server) (domain.Document, *domain.FetchError) {
	t.Helper()
	e := New(Config{Endpoint: srv.URL, Client: httpx.New(nil), Limiter: httpx.NewDomainLimiter(2, 0)})
	doc, err := e.Crawl(context.Background(), deadTargetURL+"/page", domain.EngineOptions{})
	if err == nil {
		t.Fatalf("Crawl() succeeded with %q, want a failure", doc.PageContent)
	}
	var fe *domain.FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("want *domain.FetchError, got %T: %v", err, err)
	}
	return doc, fe
}

// Every error family crawl4ai 0.9.4 reports per result must surface as a
// classified failure — never as the page's markdown — with crawl4ai's own
// message kept in the detail.
func TestCrawl094ResultFailureFamilies(t *testing.T) {
	cases := []struct {
		name       string
		msg        string
		status     int
		want       domain.FailureKind
		wantDetail string
	}{
		{"anti-bot Cloudflare JS challenge", "Blocked by anti-bot protection: Cloudflare JS challenge", 403, domain.KindBotBlock, "Cloudflare JS challenge"},
		{"anti-bot Akamai", "Blocked by anti-bot protection: Akamai block", 0, domain.KindBotBlock, "Akamai"},
		{"captcha", "Blocked by anti-bot protection: DataDome CAPTCHA", 0, domain.KindCaptcha, "CAPTCHA"},
		{"cloudflare just a moment", "Page title is 'Just a moment...' (Cloudflare interstitial)", 0, domain.KindCaptcha, "Just a moment"},
		{"http 403", "Blocked by anti-bot protection: HTTP 403 Forbidden on small page", 403, domain.KindHTTP403, "HTTP 403"},
		{"http 429", "Failed on navigating ACS-GOTO: HTTP 429 Too Many Requests", 429, domain.KindHTTP429, "429"},
		{"timeout", "Failed on navigating ACS-GOTO:\nPage.goto: Timeout 60000ms exceeded.", 0, domain.KindTimeout, "Timeout 60000ms"},
		{"net err timed out", "Failed on navigating ACS-GOTO: net::ERR_TIMED_OUT at https://example.com/page", 0, domain.KindTimeout, "ERR_TIMED_OUT"},
		{"time limit", "Crawl exceeded the time limit", 0, domain.KindTimeout, "time limit"},
		{"structural content-gate", "Blocked by anti-bot protection: Structural: minimal_text on small page (224 bytes)", 200, domain.KindThinContent, "minimal_text"},
		{"http 5xx", "Failed on navigating ACS-GOTO: HTTP 503 Service Unavailable", 503, domain.KindUpstreamError, "HTTP 503"},
		{"net err other", "Failed on navigating ACS-GOTO: net::ERR_NAME_NOT_RESOLVED", 0, domain.KindUpstreamError, "ERR_NAME_NOT_RESOLVED"},
		{"url names a vendor", "Failed on navigating ACS-GOTO: net::ERR_CONNECTION_RESET at https://blog.cloudflare.com/blocked-timeout-captcha", 0, domain.KindUpstreamError, "ERR_CONNECTION_RESET"},
		{"silent 403 page", "Unexpected error", 403, domain.KindHTTP403, "Unexpected error"},
		{"empty message", "", 0, domain.KindUpstreamError, "no error message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, fe := crawlFake(t, serveJSON(t, http.StatusOK, crawl094Failure(tc.msg, tc.status)))
			if fe.Kind != tc.want {
				t.Fatalf("Kind = %q, want %q (err %v)", fe.Kind, tc.want, fe)
			}
			if !strings.Contains(fe.Error(), tc.wantDetail) {
				t.Errorf("Error() = %q, want it to contain %q", fe.Error(), tc.wantDetail)
			}
			if strings.Contains(fe.Error(), "\n") {
				t.Errorf("Error() = %q, want newlines flattened", fe.Error())
			}
			if fe.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", fe.StatusCode, tc.status)
			}
		})
	}
}

// The raw message is unscrubbed upstream, so the kept detail is bounded.
func TestCrawl094ResultFailureTruncatesMessage(t *testing.T) {
	long := "Blocked by anti-bot protection: Cloudflare JS challenge " + strings.Repeat("x", 5000)
	_, fe := crawlFake(t, serveJSON(t, http.StatusOK, crawl094Failure(long, 403)))
	if n := len(fe.Error()); n > maxVerdictChars+64 {
		t.Errorf("len(Error()) = %d, want ≤ %d", n, maxVerdictChars+64)
	}
}

// 0.9.2 results carry success:true (or omit it): content is still served.
func TestCrawlResultSuccessCompat(t *testing.T) {
	for name, result := range map[string]map[string]any{
		"explicit true": {"success": true, "status_code": 200, "markdown": map[string]any{"raw_markdown": "# Hello"}},
		"omitted":       {"status_code": 200, "markdown": map[string]any{"raw_markdown": "# Hello"}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := serveJSON(t, http.StatusOK, map[string]any{"success": true, "results": []any{result}})
			e := New(Config{Endpoint: srv.URL, Client: httpx.New(nil), Limiter: httpx.NewDomainLimiter(2, 0)})
			doc, err := e.Crawl(context.Background(), deadTargetURL+"/page", domain.EngineOptions{})
			if err != nil || doc.PageContent != "# Hello" {
				t.Fatalf("Crawl() = %q, %v; want # Hello", doc.PageContent, err)
			}
		})
	}
}

// crawl4ai's wall-clock 504 is a timeout, not an upstream fault.
func TestCrawl504IsTimeout(t *testing.T) {
	_, fe := crawlFake(t, serveJSON(t, http.StatusGatewayTimeout, map[string]any{"detail": "Crawl exceeded the time limit"}))
	if fe.Kind != domain.KindTimeout {
		t.Fatalf("Kind = %q, want timeout (err %v)", fe.Kind, fe)
	}
	if fe.StatusCode != http.StatusGatewayTimeout || !strings.Contains(fe.Error(), "Crawl exceeded the time limit") {
		t.Errorf("err = %v (status %d), want status 504 and the time-limit detail", fe, fe.StatusCode)
	}
}

// A 502/5xx with a FastAPI `detail` (the /md, /llm shape since 0.9.3) is
// classified from the detail; the 0.9.2 scrubbed 500 keeps upstream_rejected.
func TestClassifyCrawlErrorDetail(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want domain.FailureKind
	}{
		{"502 detail 429", &httpx.StatusError{StatusCode: 502, Body: `{"detail":"Failed on navigating ACS-GOTO: HTTP 429 Too Many Requests"}`}, domain.KindHTTP429},
		{"502 detail captcha", &httpx.StatusError{StatusCode: 502, Body: `{"detail":"hCaptcha challenge detected"}`}, domain.KindCaptcha},
		{"502 detail timeout", &httpx.StatusError{StatusCode: 502, Body: `{"detail":"Page.goto: Timeout 60000ms exceeded."}`}, domain.KindTimeout},
		{"502 detail crash", &httpx.StatusError{StatusCode: 502, Body: `{"detail":"browser crashed"}`}, domain.KindUpstreamError},
		{"504 bare", &httpx.StatusError{StatusCode: 504}, domain.KindTimeout},
		{"504 detail", &httpx.StatusError{StatusCode: 504, Body: `{"detail":"Crawl exceeded the time limit"}`}, domain.KindTimeout},
		{"0.9.2 scrubbed 500", &httpx.StatusError{StatusCode: 500, Body: `{"error":"Internal server error","correlation_id":"415768e2265e"}`}, domain.KindUpstreamRejected},
		{"non-string detail ignored", &httpx.StatusError{StatusCode: 502, Body: `{"detail":[{"msg":"x"}]}`}, domain.KindUpstreamError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if fe := classifyCrawlError(tc.err); fe == nil || fe.Kind != tc.want {
				t.Fatalf("classifyCrawlError(%v) = %v, want kind %q", tc.err, fe, tc.want)
			}
		})
	}
}
