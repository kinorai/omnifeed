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

// classifyCrawlError reads StatusError.Body to tell crawl4ai's anti-bot block (a
// content block — bot_block, which OmnifeedCrawlErrors excludes) from a genuine
// upstream 5xx (upstream_error, which still pages). Tested directly so it doesn't
// pay the cost of driving real retries.
func TestClassifyCrawlError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want domain.FailureKind
	}{
		{
			"anti-bot 5xx demotes to bot_block",
			&httpx.StatusError{StatusCode: 500, Body: `{"error":"Blocked by anti-bot protection: Structural: minimal_text on small page (224 bytes, 9 chars visible)"}`},
			domain.KindBotBlock,
		},
		{"generic 5xx stays upstream_error", &httpx.StatusError{StatusCode: 500, Body: `{"error":"internal server error"}`}, domain.KindUpstreamError},
		{"5xx with empty body stays upstream_error", &httpx.StatusError{StatusCode: 503}, domain.KindUpstreamError},
		{"403 is unaffected", &httpx.StatusError{StatusCode: 403}, domain.KindHTTP403},
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
			"success=false anti-bot to bot_block",
			map[string]interface{}{"success": false, "error": "Blocked by anti-bot protection: Structural: minimal_text on small page"},
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
