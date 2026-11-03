package stackexchange

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// Matches must claim question URLs on the network's hosts only — every other
// page on those hosts, and every other host, falls through to the generic
// crawl4ai fallback.
func TestMatches(t *testing.T) {
	e := New(Config{})

	claim := []string{
		"https://stackoverflow.com/questions/11227809/why-is-processing-a-sorted-array-faster",
		"https://stackoverflow.com/questions/11227809",                          // slugless
		"https://stackoverflow.com/questions/11227809/",                         // trailing slash
		"https://stackoverflow.com/questions/11227809/why-is/11227902",          // answer permalink
		"https://stackoverflow.com/questions/11227809/why-is/11227902#11227902", // answer anchor
		"https://stackoverflow.com/questions/11227809/why-is#answer-11227902",   // #answer fragment
		"https://stackoverflow.com/questions/11227809/why-is?noredirect=1",      // query
		"https://stackoverflow.com/q/11227809",                                  // share link
		"https://stackoverflow.com/q/11227809/87234",                            // share link + user id
		"https://stackoverflow.com/a/11227902",                                  // answer share link
		"https://stackoverflow.com/a/11227902/922184",                           // answer share link + user id
		"https://www.stackoverflow.com/questions/11227809/x",                    // www.
		"https://STACKOVERFLOW.COM/questions/11227809/x",                        // host case-insensitive
		"http://stackoverflow.com/questions/11227809/x",                         // plain http
		"https://superuser.com/questions/1/x",
		"https://serverfault.com/questions/1/x",
		"https://askubuntu.com/questions/1/x",
		"https://mathoverflow.net/questions/1/x",
		"https://unix.stackexchange.com/questions/1/x",
		"https://meta.stackoverflow.com/questions/1/x",
		"https://meta.stackexchange.com/questions/1/x",
		"https://meta.unix.stackexchange.com/questions/1/x",
		"https://unix.meta.stackexchange.com/questions/1/x",
	}
	for _, u := range claim {
		if !e.Matches(u) {
			t.Errorf("Matches(%q) = false, want true", u)
		}
	}

	fallThrough := []string{
		"https://stackoverflow.com/",                             // front page
		"https://stackoverflow.com/questions",                    // question list
		"https://stackoverflow.com/questions/tagged/go",          // tag listing
		"https://stackoverflow.com/questions/ask",                // ask page
		"https://stackoverflow.com/questions/11227809/x/y",       // non-numeric answer segment
		"https://stackoverflow.com/questions/11227809/x/1/extra", // too deep
		"https://stackoverflow.com/users/87234/gmannickg",        // user
		"https://stackoverflow.com/tags",                         // tags
		"https://stackoverflow.com/search?q=go",                  // search
		"https://stackoverflow.com/q/abc",                        // non-numeric short link
		"https://stackexchange.com/questions/1/x",                // network hub, not a site
		"https://api.stackexchange.com/questions/1/x",            // the API host itself
		"https://chat.stackexchange.com/questions/1/x",           // infrastructure subdomain
		"https://a.b.stackexchange.com/questions/1/x",            // not a single-label site
		"https://ru.stackoverflow.com/questions/1/x",             // localized site: not mapped
		"https://stackoverflow.com.evil.com/questions/1/x",       // lookalike host
		"https://notstackoverflow.com/questions/1/x",             // suffix lookalike
		"ftp://stackoverflow.com/questions/1/x",                  // scheme
		"https://news.ycombinator.com/questions/1",               // other host
		"http://%zz/questions/1",                                 // unparseable
	}
	for _, u := range fallThrough {
		if e.Matches(u) {
			t.Errorf("Matches(%q) = true, want false (should fall through)", u)
		}
	}
}

// Each host form maps to its API site parameter, and each path form to the id
// it names.
func TestParseTarget(t *testing.T) {
	cases := []struct {
		url, site, question, answer string
	}{
		{"https://stackoverflow.com/questions/11227809/slug", "stackoverflow", "11227809", ""},
		{"https://stackoverflow.com/questions/11227809/slug/11227902", "stackoverflow", "11227809", ""},
		{"https://stackoverflow.com/q/11227809/87234", "stackoverflow", "11227809", ""},
		{"https://stackoverflow.com/a/11227902/922184", "stackoverflow", "", "11227902"},
		{"https://www.superuser.com/questions/1/x", "superuser", "1", ""},
		{"https://serverfault.com/questions/2/x", "serverfault", "2", ""},
		{"https://askubuntu.com/questions/3/x", "askubuntu", "3", ""},
		{"https://mathoverflow.net/questions/4/x", "mathoverflow", "4", ""},
		{"https://unix.stackexchange.com/questions/5/x", "unix", "5", ""},
		{"https://meta.stackoverflow.com/questions/6/x", "meta.stackoverflow", "6", ""},
		{"https://meta.stackexchange.com/questions/7/x", "meta", "7", ""},
		{"https://meta.unix.stackexchange.com/questions/8/x", "meta.unix", "8", ""},
		{"https://unix.meta.stackexchange.com/questions/9/x", "unix.meta.stackexchange.com", "9", ""},
	}
	for _, tc := range cases {
		got, ok := parseTarget(tc.url)
		if !ok {
			t.Errorf("parseTarget(%q) not ok", tc.url)
			continue
		}
		if got.site != tc.site || got.questionID != tc.question || got.answerID != tc.answer {
			t.Errorf("parseTarget(%q) = %+v, want site=%s question=%s answer=%s", tc.url, got, tc.site, tc.question, tc.answer)
		}
	}
}

// The engine reads a separate API host, not the page's own host, so it does
// not declare domain.SameHostEngine: a block or quota refusal from the API
// still falls back to the generic render (docs/configuration.md, Dedicated-engine
// fallback).
func TestNotSameHost(t *testing.T) {
	if _, ok := any(New(Config{})).(domain.SameHostEngine); ok {
		t.Fatal("stackexchange engine must be a separate-host engine")
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// fakeAPI serves recorded responses by path and records every request.
type fakeAPI struct {
	mu     sync.Mutex
	routes map[string][]byte
	gzip   string // "" plain, "declared" with Content-Encoding, "silent" without
	reqs   []*http.Request
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r)
	f.mu.Unlock()
	body, ok := f.routes[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error_id":404,"error_message":"no method found with this name","error_name":"no_method"}`))
		return
	}
	status := http.StatusOK
	if bytes.Contains(body, []byte(`"error_id"`)) {
		status = http.StatusBadRequest // every API error is a 400
	}
	if f.gzip != "" {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(body)
		_ = zw.Close()
		body = buf.Bytes()
		if f.gzip == "declared" {
			w.Header().Set("Content-Encoding", "gzip")
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func newFake(t *testing.T, routes map[string][]byte) (*fakeAPI, *httptest.Server) {
	t.Helper()
	f := &fakeAPI{routes: routes}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func questionRoutes(t *testing.T) map[string][]byte {
	return map[string][]byte{
		"/questions/11227809":         fixture(t, "question.json"),
		"/questions/11227809/answers": fixture(t, "answers.json"),
		"/answers/11227902":           fixture(t, "answer_short.json"),
	}
}

// A question URL renders the recorded question and its answers by votes: the
// title line, the header fields, the accepted marker, code blocks as fences,
// and the truncation note (the fixture's page is 3 of 25 answers).
func TestCrawl_Question(t *testing.T) {
	f, srv := newFake(t, questionRoutes(t))
	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})

	doc, err := e.Crawl(context.Background(), "https://stackoverflow.com/questions/11227809/why-is-processing-a-sorted-array-faster", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	out := doc.PageContent
	for _, want := range []string{
		"# Why is conditional processing of a sorted array faster than of an unsorted array?\n",
		"site: stackoverflow.com",
		"score: 27548",
		"branch-prediction",
		"author: GManNickG",
		"asked: \"2012-06-27T13:51:36Z\"",
		"accepted_answer: 11227902",
		"answers: 25",
		"```cpp",               // the question's lang-cpp block
		"#include <algorithm>", // entity-decoded inside the fence
		"answer list truncated: showing the top 3 of 25 answers by votes",
		"Mysticial",
		"Daniel Fischer",
		"[branch prediction](https://en.wikipedia.org/wiki/Branch_predictor)",
		"`data[c] >= 128`",
		// A protocol-relative link in the third answer is made absolute.
		"https://stackoverflow.com/questions/11227809/why-is-it-faster-to-process-a-sorted-array-than-an-unsorted-array/11227902#11227902",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	// Answers keep the API's vote order, and only the accepted one is marked.
	if i, j := strings.Index(out, "Mysticial"), strings.Index(out, "Daniel Fischer"); i > j {
		t.Errorf("answers out of vote order")
	}
	for _, row := range []string{
		"answers[3]{id,score,accepted,author,created,body}:",
		"11227902,35297,true,Mysticial,",
		"11227877,4767,false,Daniel Fischer,",
		"11237235,3860,false,WiSaGaN,",
	} {
		if !strings.Contains(out, row) {
			t.Errorf("output missing answer row %q", row)
		}
	}

	wantMeta := map[string]string{
		"engine":              "stackexchange",
		"answers":             "3",
		"truncated_from":      "25",
		"quota_remaining":     "298",
		domain.ContentTypeKey: domain.ContentTypeTOON,
	}
	for k, v := range wantMeta {
		if got := doc.Metadata[k]; got != v {
			t.Errorf("metadata[%s] = %q, want %q", k, got, v)
		}
	}

	if len(f.reqs) != 2 {
		t.Fatalf("made %d requests, want 2 (question + answers)", len(f.reqs))
	}
	q := f.reqs[1].URL.Query()
	if q.Get("site") != "stackoverflow" || q.Get("filter") != "withbody" || q.Get("sort") != "votes" ||
		q.Get("order") != "desc" || q.Get("pagesize") != "100" {
		t.Errorf("answers query = %v", q)
	}
	if q.Has("key") {
		t.Errorf("keyless engine sent a key")
	}
}

// A short /a/ link names only the answer: the engine looks its question up
// through /answers/{id}, then renders the question as usual.
func TestCrawl_ShortAnswerLink(t *testing.T) {
	f, srv := newFake(t, questionRoutes(t))
	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})

	doc, err := e.Crawl(context.Background(), "https://stackoverflow.com/a/11227902/922184", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if !strings.HasPrefix(doc.PageContent, "# Why is conditional processing") {
		t.Errorf("content = %.80q", doc.PageContent)
	}
	paths := make([]string, 0, len(f.reqs))
	for _, r := range f.reqs {
		paths = append(paths, r.URL.Path)
	}
	if got := strings.Join(paths, ","); got != "/answers/11227902,/questions/11227809,/questions/11227809/answers" {
		t.Errorf("request paths = %s", got)
	}
}

// The API compresses every response and tells clients to assume gzip when
// Content-Encoding is missing. Both shapes decode.
func TestCrawl_Gzip(t *testing.T) {
	for _, mode := range []string{"declared", "silent"} {
		t.Run(mode, func(t *testing.T) {
			f, srv := newFake(t, questionRoutes(t))
			f.gzip = mode
			e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
			doc, err := e.Crawl(context.Background(), "https://stackoverflow.com/q/11227809", domain.EngineOptions{})
			if err != nil {
				t.Fatalf("Crawl: %v", err)
			}
			if !strings.Contains(doc.PageContent, "Mysticial") {
				t.Errorf("content not decoded: %.80q", doc.PageContent)
			}
		})
	}
}

// A configured key goes on every call (it raises the daily quota).
func TestCrawl_KeySent(t *testing.T) {
	f, srv := newFake(t, questionRoutes(t))
	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL, Key: "k3y"})
	if _, err := e.Crawl(context.Background(), "https://stackoverflow.com/q/11227809", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	for _, r := range f.reqs {
		if r.URL.Query().Get("key") != "k3y" {
			t.Errorf("%s sent no key", r.URL.Path)
		}
	}
}

// A question without answers makes one call, not two, and has no note.
func TestCrawl_NoAnswers(t *testing.T) {
	f, srv := newFake(t, map[string][]byte{
		"/questions/42": []byte(`{"items":[{"question_id":42,"title":"Is &quot;x&quot; &amp; y valid?","score":0,"answer_count":0,` +
			`"owner":{"display_name":"Ren&#233;"},"creation_date":1,"body":"<p>Hi</p>"}],"has_more":false,"quota_max":300,"quota_remaining":10}`),
	})
	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	doc, err := e.Crawl(context.Background(), "https://unix.stackexchange.com/questions/42/x", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if len(f.reqs) != 1 {
		t.Errorf("made %d requests, want 1", len(f.reqs))
	}
	if f.reqs[0].URL.Query().Get("site") != "unix" {
		t.Errorf("site = %q, want unix", f.reqs[0].URL.Query().Get("site"))
	}
	// Titles and display names arrive HTML-encoded.
	if !strings.HasPrefix(doc.PageContent, `# Is "x" & y valid?`) || !strings.Contains(doc.PageContent, "René") {
		t.Errorf("entities not decoded: %q", doc.PageContent)
	}
	if strings.Contains(doc.PageContent, "truncated") || doc.Metadata["truncated_from"] != "" {
		t.Errorf("unexpected truncation note")
	}
}

// API failures are classified by error_id (the HTTP status is 400 for all of
// them); a non-API answer (a CDN page) by its status; an empty item list is a
// deleted or missing question.
func TestCrawl_ErrorClassification(t *testing.T) {
	cases := []struct {
		name       string
		body       []byte
		status     int // 0 = the fake's own choice
		wantKind   domain.FailureKind
		wantRetry  time.Duration
		wantSubstr string
	}{
		{"bad parameter", fixture(t, "error_bad_parameter.json"), 0, domain.KindError, 0, "bad_parameter: No site found for name `nosuchsite`"},
		{"throttle violation", fixture(t, "error_throttle.json"), 0, domain.KindHTTP429, 79007 * time.Second, "throttle_violation"},
		{"internal error", []byte(`{"error_id":500,"error_message":"oops","error_name":"internal_error"}`), 0, domain.KindUpstreamError, 0, "internal_error"},
		{"temporarily unavailable", []byte(`{"error_id":503,"error_message":"down","error_name":"temporarily_unavailable"}`), 0, domain.KindUpstreamError, 0, "temporarily_unavailable"},
		{"cdn block page", []byte(`<html>Just a moment...</html>`), http.StatusForbidden, domain.KindHTTP403, 0, "returned 403"},
		{"deleted question", []byte(`{"items":[],"has_more":false,"quota_max":300,"quota_remaining":200}`), 0, domain.KindNotFound, 0, "not found on stackoverflow.com"},
		{"garbage 200", []byte(`not json`), 0, domain.KindBadResponse, 0, "parse response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var srv *httptest.Server
			if tc.status != 0 {
				srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(tc.status)
					_, _ = w.Write(tc.body)
				}))
				t.Cleanup(srv.Close)
			} else {
				_, srv = newFake(t, map[string][]byte{"/questions/1": tc.body})
			}
			e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
			_, err := e.Crawl(context.Background(), "https://stackoverflow.com/questions/1/x", domain.EngineOptions{})
			var fe *domain.FetchError
			if !errors.As(err, &fe) {
				t.Fatalf("err = %v, want a FetchError", err)
			}
			if fe.Kind != tc.wantKind || fe.RetryAfter != tc.wantRetry {
				t.Errorf("kind=%s retry_after=%v, want %s %v (err %v)", fe.Kind, fe.RetryAfter, tc.wantKind, tc.wantRetry, err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("err = %q, want it to contain %q", err, tc.wantSubstr)
			}
			if domain.IsNoFallback(err) {
				t.Errorf("error marked final: the registry's fallback rule must decide, as for GitHub and Discourse")
			}
		})
	}
}

// A backoff in a response is honoured through the limiter: the client's
// OnRetryAfter hook (main.go wires it to the domain limiter's Penalize) gets
// the wait, so the next crawl is held back — here refused as quota_exhausted
// because its budget is shorter than the hold — and nothing is sent upstream.
func TestCrawl_BackoffHoldsTheNextCrawl(t *testing.T) {
	routes := questionRoutes(t)
	routes["/questions/11227809/answers"] = bytes.Replace(routes["/questions/11227809/answers"],
		[]byte(`"has_more":true,`), []byte(`"has_more":true,"backoff":120,`), 1)
	f, srv := newFake(t, routes)

	limiter := httpx.NewDomainLimiter(1, 0)
	client := httpx.New(nil)
	var hooked []time.Duration
	client.OnRetryAfter = func(upstream, rawURL string, d time.Duration) {
		if upstream != "stackexchange" || !strings.HasPrefix(rawURL, srv.URL+"/questions/11227809/answers?") {
			t.Errorf("hook(%s, %s)", upstream, rawURL)
		}
		hooked = append(hooked, d)
		limiter.Penalize(rawURL, d)
	}
	e := New(Config{Client: client, APIBase: srv.URL, Limiter: limiter})

	if _, err := e.Crawl(context.Background(), "https://stackoverflow.com/q/11227809", domain.EngineOptions{}); err != nil {
		t.Fatalf("first Crawl: %v (a backoff does not fail the response that carries it)", err)
	}
	if len(hooked) != 1 || hooked[0] != 120*time.Second {
		t.Fatalf("hook calls = %v, want [2m0s]", hooked)
	}

	sent := len(f.reqs)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := e.Crawl(ctx, "https://stackoverflow.com/q/11227809", domain.EngineOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindQuotaExhausted || fe.RetryAfter < 100*time.Second {
		t.Fatalf("second Crawl err = %v, want quota_exhausted with ~2m RetryAfter", err)
	}
	if len(f.reqs) != sent {
		t.Errorf("second crawl sent %d requests despite the backoff", len(f.reqs)-sent)
	}
}

// A throttle violation's wait is handed to the limiter too, not just to the
// caller.
func TestCrawl_ThrottleHoldsTheLimiter(t *testing.T) {
	_, srv := newFake(t, map[string][]byte{"/questions/1": fixture(t, "error_throttle.json")})
	client := httpx.New(nil)
	var hooked time.Duration
	client.OnRetryAfter = func(_, _ string, d time.Duration) { hooked = d }
	e := New(Config{Client: client, APIBase: srv.URL})
	if _, err := e.Crawl(context.Background(), "https://stackoverflow.com/questions/1", domain.EngineOptions{}); err == nil {
		t.Fatal("Crawl succeeded on a throttle violation")
	}
	if hooked != 79007*time.Second {
		t.Errorf("hook got %v, want 79007s", hooked)
	}
}

// A pacing refusal from the engine's own limiter (the wait exceeds the
// caller's budget) reaches the caller as quota_exhausted with its RetryAfter —
// not an opaque "error" — and nothing is sent upstream.
func TestCrawl_LimiterRefusalIsQuotaExhausted(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	defer srv.Close()

	limiter := httpx.NewDomainLimiter(1, 0)
	limiter.Penalize(srv.URL, 2*time.Minute)
	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL, Limiter: limiter})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := e.Crawl(ctx, "https://stackoverflow.com/questions/1", domain.EngineOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindQuotaExhausted || fe.RetryAfter < 100*time.Second {
		t.Fatalf("err = %v, want quota_exhausted with ~2m RetryAfter", err)
	}
	if hits != 0 {
		t.Fatalf("upstream hit %d times despite the refusal", hits)
	}
}
