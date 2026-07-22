package reddit

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

func mustJSON(v interface{}) []byte { b, _ := json.Marshal(v); return b }

// crawl4aiOK builds a successful /execute_js response whose js_execution_result
// wraps a {s,b} fetch envelope — the exact shape browserFetch must unwrap
// (crawl4ai resp → js_execution_result.results[0] → JSON-encoded string → {s,b}
// → reddit body).
func crawl4aiOK(fetchStatus int, fetchBody string) []byte {
	env := mustJSON(fetchEnvelope{S: fetchStatus, B: fetchBody})
	return mustJSON(map[string]interface{}{
		"success":             true,
		"status_code":         200,
		"js_execution_result": map[string]interface{}{"results": []interface{}{string(env)}},
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
		wantErr    string             // substring; "" = expect success
		wantKind   domain.FailureKind // asserted when wantErr != ""
	}{
		{"success returns reddit body", 200, crawl4aiOK(200, validReddit), validReddit, "", ""},
		{"reddit 403 block", 200, crawl4aiOK(403, "<html>You've been blocked</html>"), "", "reddit returned 403", domain.KindHTTP403},
		{"reddit network-security wall", 200, crawl4aiOK(200, "<html>You've been blocked by network security. Please prove you're a human.</html>"), "", "captcha", domain.KindCaptcha},
		{"non-JSON reddit body", 200, crawl4aiOK(200, "<html>not json</html>"), "", "not JSON", domain.KindBotBlock},
		// crawl4ai's anti-bot detector hard-errors a blocked Reddit nav as a 500;
		// it must classify as bot_block (the OmnifeedRedditBlocked alert keys on it),
		// not upstream_error.
		{"crawl4ai 5xx (anti-bot)", 500, []byte(`{"error":"Blocked by anti-bot protection"}`), "", "500", domain.KindBotBlock},
		{"crawl4ai 4xx", 400, []byte(`{"error":"bad request"}`), "", "crawl4ai returned 400", domain.KindError},
		{"crawl4ai result failed", 200, mustJSON(map[string]interface{}{
			"success": false, "error_message": "nav timeout",
		}), "", "nav timeout", domain.KindBotBlock},
		{"empty js result", 200, mustJSON(map[string]interface{}{
			"success":             true,
			"js_execution_result": map[string]interface{}{"results": []interface{}{}},
		}), "", "no js result", domain.KindBotBlock},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.httpStatus)
				_, _ = w.Write(tc.body)
			}))
			defer srv.Close()

			f := NewFetcher(httpx.New(nil), srv.URL, "")
			got, err := f.FetchThread(context.Background(), permalink, 500, 20, "top")

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
				}
				if tc.wantKind != "" {
					var fe *domain.FetchError
					if !errors.As(err, &fe) {
						t.Fatalf("want *domain.FetchError, got %T: %v", err, err)
					}
					if fe.Kind != tc.wantKind {
						t.Fatalf("reason mismatch: Kind = %q, want %q", fe.Kind, tc.wantKind)
					}
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
	f := NewFetcher(httpx.New(nil), "", "")
	if _, err := f.FetchThread(context.Background(), "/r/news/comments/abc/t/", 500, 20, "top"); err == nil {
		t.Fatal("expected error when crawl4ai URL is empty")
	}
}

// TestFetcherSendsBearerToken: when a token is configured, every crawl4ai call
// carries `Authorization: Bearer <token>` — required for crawl4ai 0.9.x, which
// binds non-loopback only when CRAWL4AI_API_TOKEN is set. Empty token → no header.
func TestFetcherSendsBearerToken(t *testing.T) {
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
				_, _ = w.Write(crawl4aiOK(200, `[{"kind":"Listing","data":{"children":[]}}]`))
			}))
			defer srv.Close()

			f := NewFetcher(httpx.New(nil), srv.URL, tc.token)
			if _, err := f.FetchThread(context.Background(), "/r/news/comments/abc/t/", 500, 20, "top"); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotAuth != tc.want {
				t.Fatalf("Authorization = %q, want %q", gotAuth, tc.want)
			}
		})
	}
}

// TestRequestShape locks the /execute_js request the binary sends: {url, scripts}
// with the Reddit fetch params carried in the script, and morechildren
// re-navigating the thread page (crawl4ai's /execute_js has no session reuse).
func TestRequestShape(t *testing.T) {
	var captured []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		_, _ = w.Write(crawl4aiOK(200, `[{"kind":"Listing","data":{"children":[]}}]`))
	}))
	defer srv.Close()
	f := NewFetcher(httpx.New(nil), srv.URL, "")

	decode := func() execJSRequest {
		t.Helper()
		var req execJSRequest
		if err := json.Unmarshal(captured, &req); err != nil {
			t.Fatalf("decode captured request: %v", err)
		}
		return req
	}

	// FetchThread navigates the thread page and fetches the .json.
	if _, err := f.FetchThread(context.Background(), "/r/news/comments/abc123/some_title/", 123, 4, "new"); err != nil {
		t.Fatal(err)
	}
	req := decode()
	if len(req.Scripts) != 1 {
		t.Fatalf("want exactly one script, got %d", len(req.Scripts))
	}
	// The Reddit fetch params must pass through into the in-page fetch() URL.
	// jsLit JSON-escapes the URL, so assert each key=value separately, not joined.
	for _, want := range []string{"limit=123", "depth=4", "sort=new"} {
		if !strings.Contains(req.Scripts[0], want) {
			t.Errorf("thread fetch URL missing %q; script = %s", want, req.Scripts[0])
		}
	}
	if !strings.Contains(req.URL, "/r/news/comments/abc123/") {
		t.Errorf("FetchThread must navigate the thread page; url = %s", req.URL)
	}

	// FetchMoreChildren re-navigates the thread page and POSTs morechildren.
	if _, err := f.FetchMoreChildren(context.Background(), "t3_abc123", []string{"x1", "x2"}, "top"); err != nil {
		t.Fatal(err)
	}
	req = decode()
	if !strings.Contains(req.URL, "/comments/abc123/") {
		t.Errorf("morechildren must navigate the thread page; url = %s", req.URL)
	}
	if !strings.Contains(req.Scripts[0], "morechildren") {
		t.Errorf("morechildren script must POST /api/morechildren; script = %s", req.Scripts[0])
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

// crawl4aiRawJS builds an /execute_js response whose js_execution_result returns
// a plain string (e.g. location.href) rather than a {s,b} envelope.
func crawl4aiRawJS(jsReturn string) []byte {
	return mustJSON(map[string]interface{}{
		"success":             true,
		"status_code":         200,
		"js_execution_result": map[string]interface{}{"results": []interface{}{jsReturn}},
	})
}

func TestResolveShareURL(t *testing.T) {
	canonical := "https://www.reddit.com/r/news/comments/abc123/title/?utm_source=share"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(crawl4aiRawJS(canonical))
	}))
	defer srv.Close()
	f := NewFetcher(httpx.New(nil), srv.URL, "")
	got, err := f.ResolveShareURL(context.Background(), "https://www.reddit.com/r/news/s/abc")
	if err != nil || got != canonical {
		t.Fatalf("ResolveShareURL = %q, %v; want %q", got, err, canonical)
	}

	// A resolution that isn't a thread is an error.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(crawl4aiRawJS("https://www.reddit.com/r/news/"))
	}))
	defer srv2.Close()
	f2 := NewFetcher(httpx.New(nil), srv2.URL, "")
	if _, err := f2.ResolveShareURL(context.Background(), "https://www.reddit.com/r/news/s/abc"); err == nil {
		t.Error("expected error when share link resolves to a non-thread URL")
	}
}

// FetchListing must request /r/{sub}/{sort}.json (same-origin, browser path) and
// return Reddit's listing body unwrapped from the crawl4ai envelope.
func TestFetchListing(t *testing.T) {
	const listing = `{"kind":"Listing","data":{"children":[]}}`
	var reqBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		reqBody = string(b)
		_, _ = w.Write(crawl4aiOK(200, listing))
	}))
	defer srv.Close()

	f := NewFetcher(httpx.New(nil), srv.URL, "")
	got, err := f.FetchListing(context.Background(), "golang", "hot", 25)
	if err != nil {
		t.Fatalf("FetchListing: %v", err)
	}
	if string(got) != listing {
		t.Errorf("body = %q, want %q", got, listing)
	}
	if !strings.Contains(reqBody, `/r/golang/hot.json`) {
		t.Errorf("crawl4ai request js_code missing the listing .json URL; got %s", reqBody)
	}
}
