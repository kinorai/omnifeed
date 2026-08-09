package crawl4ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/observability"
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
				// Transports key size control off content_type: markdown is the
				// only shape that is safe to cut mid-document.
				if got := doc.Metadata[domain.ContentTypeKey]; got != domain.ContentTypeMarkdown {
					t.Fatalf("content_type = %q, want %q", got, domain.ContentTypeMarkdown)
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

// The PruningContentFilter must be told to skip code subtrees, otherwise the
// whitespace/operator spans a syntax highlighter emits score below the threshold
// and get pruned, corrupting the code (upstream unclecode/crawl4ai#2110). "table"
// must stay out of preserve_tags — it would re-admit chrome tables. page_timeout
// must not exceed 60000 ms: crawl4ai silently clamps untrusted REST bodies there.
func TestCrawlRequestPreservesCodeAndClampsTimeout(t *testing.T) {
	var params, filter map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		params = req["crawler_config"].(map[string]any)["params"].(map[string]any)
		filter = params["markdown_generator"].(map[string]any)["params"].(map[string]any)["content_filter"].(map[string]any)["params"].(map[string]any)

		resp := map[string]any{"success": true, "results": []any{map[string]any{
			"success": true, "status_code": 200, "markdown": map[string]any{"raw_markdown": "# ok"},
		}}}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	e := New(Config{
		Endpoint: srv.URL,
		Client:   httpx.New(nil),
		Limiter:  httpx.NewDomainLimiter(2, 0),
	})
	if _, err := e.Crawl(context.Background(), "https://example.com/page", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl() error = %v", err)
	}

	strSlice := func(key string) []string {
		raw, ok := filter[key].([]any)
		if !ok {
			t.Fatalf("%s = %#v, want a JSON array", key, filter[key])
		}
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			out = append(out, v.(string))
		}
		return out
	}
	tags := strSlice("preserve_tags")
	for _, want := range []string{"pre", "code"} {
		if !slices.Contains(tags, want) {
			t.Errorf("preserve_tags = %v, want it to contain %q", tags, want)
		}
	}
	if slices.Contains(tags, "table") {
		t.Errorf("preserve_tags = %v, must NOT contain \"table\"", tags)
	}
	classes := strSlice("preserve_classes")
	for _, want := range []string{"highlight", "chroma", "highlighter-rouge", "codehilite"} {
		if !slices.Contains(classes, want) {
			t.Errorf("preserve_classes = %v, want it to contain %q", classes, want)
		}
	}

	timeout, ok := params["page_timeout"].(float64)
	if !ok {
		t.Fatalf("page_timeout = %#v, want a number", params["page_timeout"])
	}
	if timeout > 60000 {
		t.Errorf("page_timeout = %v, want <= 60000 (crawl4ai clamps untrusted bodies)", timeout)
	}
}

// crawlParams drives one crawl against a fake crawl4ai and returns the
// crawler_config params the engine sent, so payload knobs can be asserted
// without a live upstream.
func crawlParams(t *testing.T, cfg Config) map[string]any {
	t.Helper()
	var params map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		params = req["crawler_config"].(map[string]any)["params"].(map[string]any)
		resp := map[string]any{"success": true, "results": []any{map[string]any{
			"success": true, "status_code": 200, "markdown": map[string]any{"raw_markdown": "# ok"},
		}}}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	cfg.Endpoint = srv.URL
	cfg.Client = httpx.New(nil)
	cfg.Limiter = httpx.NewDomainLimiter(2, 0)
	if _, err := New(cfg).Crawl(context.Background(), "https://example.com/page", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl() error = %v", err)
	}
	return params
}

func jsonStrings(t *testing.T, params map[string]any, key string) []string {
	t.Helper()
	raw, ok := params[key].([]any)
	if !ok {
		t.Fatalf("%s = %#v, want a JSON array", key, params[key])
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

// Chrome trimming: script/style/noscript join the excluded tags, consent popups
// are removed, and the conservative excluded_selector default ships on by
// default. target_elements is opt-in, so it must be absent unless configured.
func TestCrawlRequestChromeDefaults(t *testing.T) {
	params := crawlParams(t, Config{})

	tags := jsonStrings(t, params, "excluded_tags")
	for _, want := range []string{"nav", "footer", "header", "form", "aside", "script", "style", "noscript"} {
		if !slices.Contains(tags, want) {
			t.Errorf("excluded_tags = %v, want it to contain %q", tags, want)
		}
	}
	if params["remove_consent_popups"] != true {
		t.Errorf("remove_consent_popups = %#v, want true", params["remove_consent_popups"])
	}
	if got := params["excluded_selector"]; got != DefaultExcludedSelector {
		t.Errorf("excluded_selector = %#v, want %q", got, DefaultExcludedSelector)
	}
	if _, ok := params["target_elements"]; ok {
		t.Errorf("target_elements = %#v, want the key absent by default", params["target_elements"])
	}
}

// A configured excluded_selector replaces the default; empty falls back to it.
func TestCrawlRequestExcludedSelectorOverride(t *testing.T) {
	custom := ".ad,.promo"
	params := crawlParams(t, Config{ExcludedSelector: custom})
	if got := params["excluded_selector"]; got != custom {
		t.Errorf("excluded_selector = %#v, want %q", got, custom)
	}

	params = crawlParams(t, Config{})
	if got := params["excluded_selector"]; got != DefaultExcludedSelector {
		t.Errorf("excluded_selector = %#v, want the default when unset", got)
	}
}

// Commas inside functional pseudo-classes must not split a selector in two.
func TestSplitSelectorsFunctionalPseudoClasses(t *testing.T) {
	got := splitSelectors("main :is(h1, h2), article:not(.a, .b) , aside")
	want := []string{"main :is(h1, h2)", "article:not(.a, .b)", "aside"}
	if !slices.Equal(got, want) {
		t.Errorf("splitSelectors = %v, want %v", got, want)
	}
}

// A non-empty OMNIFEED_CRAWL4AI_TARGET_ELEMENTS arrives as one comma-separated
// string and must reach crawl4ai as a trimmed list.
func TestCrawlRequestTargetElements(t *testing.T) {
	params := crawlParams(t, Config{TargetElements: "article, main , [role=main]"})
	got := jsonStrings(t, params, "target_elements")
	want := []string{"article", "main", "[role=main]"}
	if !slices.Equal(got, want) {
		t.Errorf("target_elements = %v, want %v", got, want)
	}
}

// fit_markdown wins when present; raw_markdown is the fallback only when fit
// is empty. (The old lost-fences heuristic is gone: it could not detect partial
// fence loss and mis-fired when only page chrome held a fence — preserve_tags/
// preserve_classes in the crawl request are the real fix, crawl4ai >= 0.9.1.)
func TestCrawlContentSelection(t *testing.T) {
	cases := []struct {
		name, fit, raw, want string
	}{
		{"fit wins when present", "Fit\n\n```go\nfit()\n```\n", "raw text", "fit()"},
		{"empty fit falls back to raw", "", "Intro\n\n```go\nfunc main() {}\n```\n", "func main() {}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				resp := map[string]any{"success": true, "results": []any{map[string]any{
					"success": true, "status_code": 200,
					"markdown": map[string]any{"raw_markdown": tc.raw, "fit_markdown": tc.fit},
				}}}
				b, _ := json.Marshal(resp)
				_, _ = w.Write(b)
			}))
			defer srv.Close()

			e := New(Config{
				Endpoint: srv.URL,
				Client:   httpx.New(nil),
				Limiter:  httpx.NewDomainLimiter(2, 0),
			})
			doc, err := e.Crawl(context.Background(), "https://example.com/page", domain.EngineOptions{})
			if err != nil {
				t.Fatalf("Crawl() error = %v", err)
			}
			if !strings.Contains(doc.PageContent, tc.want) {
				t.Fatalf("PageContent = %q, want it to contain %q", doc.PageContent, tc.want)
			}
		})
	}
}

// A thin-content result with the default excluded selector active must be
// retried once without the selector: on a page whose main content matches a
// chrome shape (e.g. a docs index living in #toc), the exclusion is what
// emptied the page.
func TestCrawlRetriesWithoutExcludedSelectorOnThinContent(t *testing.T) {
	var selectors []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		params := req["crawler_config"].(map[string]any)["params"].(map[string]any)
		sel, hasSel := params["excluded_selector"]
		selectors = append(selectors, sel)
		content := ""
		if !hasSel {
			content = "the toc page content"
		}
		resp := map[string]any{"success": true, "results": []any{map[string]any{
			"success": true, "status_code": 200,
			"markdown": map[string]any{"raw_markdown": content, "fit_markdown": content},
		}}}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	e := New(Config{
		Endpoint: srv.URL,
		Client:   httpx.New(nil),
		Limiter:  httpx.NewDomainLimiter(2, 0),
	})
	doc, err := e.Crawl(context.Background(), "https://example.com/toc-page", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl() error = %v", err)
	}
	if !strings.Contains(doc.PageContent, "the toc page content") {
		t.Fatalf("PageContent = %q, want the retry's content", doc.PageContent)
	}
	if len(selectors) != 2 || selectors[0] != DefaultExcludedSelector || selectors[1] != nil {
		t.Fatalf("selectors per attempt = %#v, want [default, absent]", selectors)
	}
}

// crawl4ai has no "extracted nothing" flag: a page it failed to render comes back
// as success with every content field empty. That must be a thin_content failure
// with the diagnostic lengths, not a successful empty document handed to the LLM.
func TestCrawlEmptyExtractionIsThinContent(t *testing.T) {
	cases := []struct {
		name             string
		markdown         map[string]any
		cleanedHTML      string
		wantErr          bool
		wantReasonOrBody string
	}{
		{"all fields empty", map[string]any{"raw_markdown": "", "fit_markdown": ""}, "", true, "thin_content"},
		{"whitespace only", map[string]any{"raw_markdown": "  \n", "fit_markdown": "\t"}, "   ", true, "thin_content"},
		{"real content succeeds", map[string]any{"raw_markdown": "# Hello"}, "", false, "# Hello"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				resp := map[string]any{"success": true, "results": []any{map[string]any{
					"success": true, "status_code": 200,
					"markdown": tc.markdown, "cleaned_html": tc.cleanedHTML,
				}}}
				b, _ := json.Marshal(resp)
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
				if !strings.Contains(doc.PageContent, tc.wantReasonOrBody) {
					t.Fatalf("PageContent = %q, want it to contain %q", doc.PageContent, tc.wantReasonOrBody)
				}
				return
			}
			var fe *domain.FetchError
			if !errors.As(err, &fe) {
				t.Fatalf("want *domain.FetchError, got %T: %v", err, err)
			}
			if fe.Kind != domain.KindThinContent {
				t.Fatalf("Kind = %q, want %q", fe.Kind, domain.KindThinContent)
			}
			if !strings.Contains(fe.Error(), "0 chars") {
				t.Fatalf("Error() = %q, want it to contain %q", fe.Error(), "0 chars")
			}
			if got := observability.Reason(err); got != tc.wantReasonOrBody {
				t.Fatalf("observability.Reason(err) = %q, want %q", got, tc.wantReasonOrBody)
			}
			if doc.PageContent != "" {
				t.Fatalf("PageContent = %q, want empty on failure", doc.PageContent)
			}
		})
	}
}

// The latency knobs must reach the wire exactly as configured: DelayBeforeHTML
// always, scan_full_page/scroll_delay only when the scan is on (crawl4ai's own
// default is no scan, so the keys stay out of the payload), and max_retries
// never (crawl4ai's default 0 — re-driving a content-gate 500 burns tens of
// seconds for nothing).
func TestCrawlRequestLatencyKnobs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cfg          func(Config) Config
		wantDelay    float64
		wantScanKeys bool
		wantScroll   float64
	}{
		{
			name:      "scan off omits scan keys",
			cfg:       func(c Config) Config { c.DelayBeforeHTML = 0.1; return c },
			wantDelay: 0.1,
		},
		{
			name: "scan on sends scan_full_page and scroll_delay",
			cfg: func(c Config) Config {
				c.DelayBeforeHTML = 1.0
				c.ScanFullPage = true
				c.ScrollDelay = 0.5
				return c
			},
			wantDelay:    1.0,
			wantScanKeys: true,
			wantScroll:   0.5,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var params map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var req map[string]any
				if err := json.Unmarshal(raw, &req); err != nil {
					t.Errorf("decode request: %v", err)
				}
				params = req["crawler_config"].(map[string]any)["params"].(map[string]any)

				resp := map[string]any{"success": true, "results": []any{map[string]any{
					"success": true, "status_code": 200, "markdown": map[string]any{"raw_markdown": "# ok"},
				}}}
				b, _ := json.Marshal(resp)
				_, _ = w.Write(b)
			}))
			defer srv.Close()

			e := New(tc.cfg(Config{
				Endpoint: srv.URL,
				Client:   httpx.New(nil),
				Limiter:  httpx.NewDomainLimiter(2, 0),
			}))
			if _, err := e.Crawl(context.Background(), "https://example.com/page", domain.EngineOptions{}); err != nil {
				t.Fatalf("Crawl() error = %v", err)
			}

			if got := params["delay_before_return_html"].(float64); got != tc.wantDelay {
				t.Errorf("delay_before_return_html = %v, want %v", got, tc.wantDelay)
			}
			if _, ok := params["max_retries"]; ok {
				t.Errorf("max_retries present in payload, want it omitted (crawl4ai default 0)")
			}
			scan, scanOK := params["scan_full_page"]
			scroll, scrollOK := params["scroll_delay"]
			if tc.wantScanKeys {
				if !scanOK || scan != true {
					t.Errorf("scan_full_page = %v (present=%v), want true", scan, scanOK)
				}
				if !scrollOK || scroll.(float64) != tc.wantScroll {
					t.Errorf("scroll_delay = %v (present=%v), want %v", scroll, scrollOK, tc.wantScroll)
				}
			} else if scanOK || scrollOK {
				t.Errorf("scan_full_page/scroll_delay present (%v/%v), want both omitted when scan is off", scan, scroll)
			}
		})
	}
}
