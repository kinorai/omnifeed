package crawl4ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// classifyCrawlError reads StatusError.Body to tell crawl4ai's anti-bot block (a
// content block — bot_block, which OmnifeedCrawlErrors excludes) from a genuine
// upstream 5xx (upstream_error, which still pages). Tested directly so it doesn't
// pay the cost of driving real retries.
func TestClassifyCrawlError(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    domain.FailureKind
		wantErr string // substring the rendered error must contain (verdict enrichment); "" skips
	}{
		{
			"structural content-gate 5xx demotes to thin_content",
			&httpx.StatusError{StatusCode: 500, Body: `{"error":"Blocked by anti-bot protection: Structural: minimal_text on small page (224 bytes, 9 chars visible)"}`},
			domain.KindThinContent, "minimal_text",
		},
		{
			"genuine wall 5xx demotes to bot_block",
			&httpx.StatusError{StatusCode: 500, Body: `{"error":"Blocked by anti-bot protection: Cloudflare JS challenge"}`},
			domain.KindBotBlock, "Cloudflare JS challenge",
		},
		{"generic 5xx stays upstream_error", &httpx.StatusError{StatusCode: 500, Body: `{"error":"internal server error"}`}, domain.KindUpstreamError, ""},
		{"5xx with empty body stays upstream_error", &httpx.StatusError{StatusCode: 503}, domain.KindUpstreamError, ""},
		{"403 is unaffected", &httpx.StatusError{StatusCode: 403}, domain.KindHTTP403, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fe := classifyCrawlError(tc.err)
			if fe == nil {
				t.Fatalf("classifyCrawlError(%v) = nil, want kind %q", tc.err, tc.want)
			}
			if fe.Kind != tc.want {
				t.Fatalf("Kind = %q, want %q", fe.Kind, tc.want)
			}
			if tc.wantErr != "" && !strings.Contains(fe.Error(), tc.wantErr) {
				t.Fatalf("Error() = %q, want it to contain %q", fe.Error(), tc.wantErr)
			}
		})
	}
}

// crawl4ai also reports a block as HTTP 200 with success=false; the anti-bot
// verdict in the body must demote to bot_block there too, while a genuine failure
// stays upstream_error and a real page succeeds.
func TestCrawlClassifiesUnsuccessfulResponse(t *testing.T) {
	cases := []struct {
		name     string
		response map[string]interface{}
		wantErr  bool
		wantKind domain.FailureKind
		wantBody string
	}{
		{
			"success=false structural content-gate to thin_content",
			map[string]interface{}{"success": false, "error": "Blocked by anti-bot protection: Structural: minimal_text on small page"},
			true, domain.KindThinContent, "",
		},
		{
			"success=false genuine wall to bot_block",
			map[string]interface{}{"success": false, "error": "Blocked by anti-bot protection: Cloudflare JS challenge"},
			true, domain.KindBotBlock, "",
		},
		{
			"success=false generic stays upstream_error",
			map[string]interface{}{"success": false, "error": "navigation timeout"},
			true, domain.KindUpstreamError, "",
		},
		{
			"success=true returns content",
			map[string]interface{}{"success": true, "results": []interface{}{map[string]interface{}{
				"success": true, "status_code": 200, "markdown": map[string]interface{}{"raw_markdown": "# Hello"},
			}}},
			false, "", "# Hello",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				b, _ := json.Marshal(tc.response)
				_, _ = w.Write(b)
			}))
			defer srv.Close()

			e := New(Config{
				Endpoint: srv.URL,
				Client:   httpx.New(nil),
				Limiter:  httpx.NewDomainLimiter(2, 0),
			})
			doc, err := e.Crawl(context.Background(), "https://example.com/page", domain.EngineOptions{})

			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Crawl() error = %v, want success", err)
				}
				if !strings.Contains(doc.PageContent, tc.wantBody) {
					t.Fatalf("PageContent = %q, want it to contain %q", doc.PageContent, tc.wantBody)
				}
				return
			}
			var fe *domain.FetchError
			if !errors.As(err, &fe) {
				t.Fatalf("want *domain.FetchError, got %T: %v", err, err)
			}
			if fe.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", fe.Kind, tc.wantKind)
			}
		})
	}
}

// KeepLinks must drive the crawl4ai markdown options: with it on, anchor text is
// rendered (ignore_links=false) and external links are retained
// (exclude_external_links=false). Dropping them loses the primary content on
// link-dense pages (e.g. every Hacker News story title is an external link).
func TestCrawlRequestLinkOptions(t *testing.T) {
	cases := []struct {
		name                       string
		keepLinks                  bool
		wantIgnore, wantExcludeExt bool
	}{
		{"keep links renders anchors", true, false, false},
		{"strip links for lean output", false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotIgnore, gotExcludeExt bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var req map[string]any
				if err := json.Unmarshal(raw, &req); err != nil {
					t.Errorf("decode request: %v", err)
				}
				params := req["crawler_config"].(map[string]any)["params"].(map[string]any)
				gotExcludeExt = params["exclude_external_links"].(bool)
				mg := params["markdown_generator"].(map[string]any)["params"].(map[string]any)
				gotIgnore = mg["options"].(map[string]any)["ignore_links"].(bool)

				resp := map[string]any{"success": true, "results": []any{map[string]any{
					"success": true, "status_code": 200, "markdown": map[string]any{"raw_markdown": "# ok"},
				}}}
				b, _ := json.Marshal(resp)
				_, _ = w.Write(b)
			}))
			defer srv.Close()

			e := New(Config{
				Endpoint:  srv.URL,
				Client:    httpx.New(nil),
				Limiter:   httpx.NewDomainLimiter(2, 0),
				KeepLinks: tc.keepLinks,
			})
			if _, err := e.Crawl(context.Background(), "https://example.com/page", domain.EngineOptions{}); err != nil {
				t.Fatalf("Crawl() error = %v", err)
			}
			if gotIgnore != tc.wantIgnore {
				t.Errorf("ignore_links = %v, want %v", gotIgnore, tc.wantIgnore)
			}
			if gotExcludeExt != tc.wantExcludeExt {
				t.Errorf("exclude_external_links = %v, want %v", gotExcludeExt, tc.wantExcludeExt)
			}
		})
	}
}

// PruneThreshold must drive the PruningContentFilter cutoff sent to crawl4ai so
// operators can tune how aggressively boilerplate/duplicated chrome is stripped
// without a rebuild (OMNIFEED_CRAWL4AI_PRUNE_THRESHOLD).
func TestCrawlRequestPruneThreshold(t *testing.T) {
	var got float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		params := req["crawler_config"].(map[string]any)["params"].(map[string]any)
		filter := params["markdown_generator"].(map[string]any)["params"].(map[string]any)["content_filter"].(map[string]any)["params"].(map[string]any)
		got = filter["threshold"].(float64)

		resp := map[string]any{"success": true, "results": []any{map[string]any{
			"success": true, "status_code": 200, "markdown": map[string]any{"raw_markdown": "# ok"},
		}}}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	e := New(Config{
		Endpoint:       srv.URL,
		Client:         httpx.New(nil),
		Limiter:        httpx.NewDomainLimiter(2, 0),
		PruneThreshold: 0.72,
	})
	if _, err := e.Crawl(context.Background(), "https://example.com/page", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl() error = %v", err)
	}
	if got != 0.72 {
		t.Errorf("threshold = %v, want 0.72", got)
	}
}

// Token, when set, must be sent as `Authorization: Bearer <token>` on every
// crawl4ai call — required for crawl4ai 0.9.x, which binds non-loopback only when
// CRAWL4AI_API_TOKEN is set (OMNIFEED_CRAWL4AI_TOKEN). Empty token → no header.
func TestCrawlSendsBearerToken(t *testing.T) {
	for _, tc := range []struct {
		name, token, want string
	}{
		{"token sends bearer header", "sekret", "Bearer sekret"},
		{"empty token sends no header", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				resp := map[string]any{"success": true, "results": []any{map[string]any{
					"success": true, "status_code": 200, "markdown": map[string]any{"raw_markdown": "# ok"},
				}}}
				b, _ := json.Marshal(resp)
				_, _ = w.Write(b)
			}))
			defer srv.Close()

			e := New(Config{
				Endpoint: srv.URL,
				Token:    tc.token,
				Client:   httpx.New(nil),
				Limiter:  httpx.NewDomainLimiter(2, 0),
			})
			if _, err := e.Crawl(context.Background(), "https://example.com/page", domain.EngineOptions{}); err != nil {
				t.Fatalf("Crawl() error = %v", err)
			}
			if gotAuth != tc.want {
				t.Fatalf("Authorization = %q, want %q", gotAuth, tc.want)
			}
		})
	}
}

// WaitUntil must drive crawl4ai's page-ready signal so operators can switch the
// generic engine to networkidle for JS-SPAs (OMNIFEED_CRAWL4AI_WAIT_UNTIL)
// without a rebuild.
func TestCrawlRequestWaitUntil(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		params := req["crawler_config"].(map[string]any)["params"].(map[string]any)
		got = params["wait_until"].(string)

		resp := map[string]any{"success": true, "results": []any{map[string]any{
			"success": true, "status_code": 200, "markdown": map[string]any{"raw_markdown": "# ok"},
		}}}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	e := New(Config{
		Endpoint:  srv.URL,
		Client:    httpx.New(nil),
		Limiter:   httpx.NewDomainLimiter(2, 0),
		WaitUntil: "networkidle",
	})
	if _, err := e.Crawl(context.Background(), "https://example.com/page", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl() error = %v", err)
	}
	if got != "networkidle" {
		t.Errorf("wait_until = %q, want networkidle", got)
	}
}
