package discourse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// testHosts is the allowlist the matching table is written against.
var testHosts = []string{"meta.discourse.org", "discuss.python.org", "Users.Rust-Lang.org"}

// newTestEngine points an Engine at an httptest server: the server's host goes on
// the allowlist and the scheme is overridden to http (httptest serves plaintext).
// Production always uses https — see Engine.scheme.
func newTestEngine(t *testing.T, srvURL string) *Engine {
	t.Helper()
	u, err := url.Parse(srvURL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	e := New(Config{Client: httpx.New(nil), Hosts: []string{u.Hostname()}})
	e.scheme = "http"
	return e
}

// Matches must claim topic paths on the configured hosts only — everything else,
// including a Discourse-looking topic URL on an unlisted host, falls through to
// the generic crawl4ai fallback.
func TestMatches(t *testing.T) {
	e := New(Config{Hosts: testHosts})

	claim := []string{
		"https://discuss.python.org/t/pep-777-how-to-re-invent-the-wheel/55763",
		"https://discuss.python.org/t/pep-777-how-to-re-invent-the-wheel/55763/6", // post number
		"https://discuss.python.org/t/pep-777/55763/",                             // trailing slash
		"https://discuss.python.org/t/pep-777/55763?u=someone",                    // query
		"https://discuss.python.org/t/pep-777/55763#post_6",                       // fragment
		"https://discuss.python.org/t/55763",                                      // slugless
		"https://discuss.python.org/t/55763/6",                                    // slugless + post number
		"https://discuss.python.org/t/-/55763",                                    // placeholder slug
		"https://meta.discourse.org/t/topic-json-api/1234",
		"https://DISCUSS.python.ORG/t/pep-777/55763",     // host case-insensitive
		"https://users.rust-lang.org/t/lifetimes/9876",   // config entry was mixed-case
		"https://discuss.python.org/t/l%C3%A9gume/55763", // percent-escaped slug
	}
	for _, u := range claim {
		if !e.Matches(u) {
			t.Errorf("Matches(%q) = false, want true", u)
		}
	}

	fallThrough := []string{
		"https://discuss.python.org/",                         // front page
		"https://discuss.python.org/latest",                   // listing
		"https://discuss.python.org/c/packaging/14",           // category
		"https://discuss.python.org/u/someone",                // user
		"https://discuss.python.org/t/",                       // no topic
		"https://discuss.python.org/t/pep-777",                // slug only, no id
		"https://discuss.python.org/t/pep-777/abc",            // non-numeric id
		"https://discuss.python.org/t/pep-777/55763/6/extra",  // too deep
		"https://discuss.python.org/t/a/b/55763",              // slug with a slash
		"https://discuss.python.org/t/pep-777/55763/latest",   // non-numeric post number
		"https://forum.example.com/t/some-topic/42",           // unlisted Discourse-looking host
		"https://sub.discuss.python.org/t/pep-777/55763",      // no subdomain wildcarding
		"https://discuss.python.org.evil.com/t/pep-777/55763", // lookalike host
		"https://notdiscuss.python.org/t/pep-777/55763",       // suffix lookalike
		"https://internals.rust-lang.org/t/some-topic/42",     // in the shipped default, not in testHosts
		"https://news.ycombinator.com/t/pep-777/55763",        // other engine's host
		"http://%zz/t/pep-777/55763",                          // unparseable
	}
	for _, u := range fallThrough {
		if e.Matches(u) {
			t.Errorf("Matches(%q) = true, want false (should fall through)", u)
		}
	}
}

// An empty allowlist (OMNIFEED_DISCOURSE_HOSTS explicitly set to "") must make the
// engine claim nothing, so it can be registered harmlessly.
func TestMatchesEmptyAllowlist(t *testing.T) {
	for _, hosts := range [][]string{nil, {}, {"", "  "}} {
		e := New(Config{Hosts: hosts})
		if e.Matches("https://discuss.python.org/t/pep-777/55763") {
			t.Errorf("Hosts=%q: Matches = true, want false", hosts)
		}
	}
}

// printJSON is a 3-post topic as the print view returns it, with raw bodies.
const printJSON = `{"title":"PEP 777: how to re-invent the wheel","posts_count":3,
	"created_at":"2026-01-02T03:04:05Z","post_stream":{"stream":[11,12,13],"posts":[
	{"id":11,"post_number":1,"username":"alice","created_at":"2026-01-02T03:04:05Z",
	 "reply_to_post_number":null,"raw":"first post body","cooked":"<p>first post body</p>"},
	{"id":12,"post_number":2,"username":"bob","created_at":"2026-01-03T00:00:00Z",
	 "reply_to_post_number":1,"raw":"second post body","cooked":"<p>second post body</p>"},
	{"id":13,"post_number":3,"username":"carol","created_at":"2026-01-04T00:00:00Z",
	 "reply_to_post_number":2,"raw":"third post body","cooked":"<p>third post body</p>"}]}}`

// The happy path must fetch the print view once, with include_raw, and emit every
// post in order with its number, author, reply parent, and raw body.
func TestCrawlPrintView(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		if r.URL.Path != "/t/55763.json" {
			t.Errorf("unexpected path %q", r.URL.RequestURI())
		}
		_, _ = io.WriteString(w, printJSON)
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	doc, err := e.Crawl(context.Background(), srv.URL+"/t/pep-777/55763", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("fetched %d urls (%v), want 1", len(paths), paths)
	}
	for _, want := range []string{"print=true", "include_raw=1"} {
		if !strings.Contains(paths[0], want) {
			t.Errorf("request %q missing %q", paths[0], want)
		}
	}
	if doc.Metadata["posts"] != "3" {
		t.Errorf("posts = %q, want 3", doc.Metadata["posts"])
	}
	if doc.Metadata["engine"] != "discourse" {
		t.Errorf("engine = %q, want discourse", doc.Metadata["engine"])
	}
	if doc.Metadata["status_code"] != "200" {
		t.Errorf("status_code = %q, want 200", doc.Metadata["status_code"])
	}
	if doc.Metadata["source"] != srv.URL+"/t/pep-777/55763" {
		t.Errorf("source = %q", doc.Metadata["source"])
	}
	if got := doc.Metadata[domain.ContentTypeKey]; got != domain.ContentTypeTOON {
		t.Errorf("content_type = %q, want %q", got, domain.ContentTypeTOON)
	}
	if _, ok := doc.Metadata["truncated_from"]; ok {
		t.Errorf("unexpected truncated_from = %q", doc.Metadata["truncated_from"])
	}
	for _, want := range []string{
		"PEP 777: how to re-invent the wheel", "posts_count: 3",
		"posts[#3]:", // TOON length marker
		"alice", "bob", "carol",
		"first post body", "second post body", "third post body",
		"number: 2", "reply_to: 1", "reply_to: 2",
	} {
		if !strings.Contains(doc.PageContent, want) {
			t.Errorf("output missing %q:\n%s", want, doc.PageContent)
		}
	}
	// The host of the topic is reported in the header, and post order survives.
	if !strings.Contains(doc.PageContent, "host: 127.0.0.1") {
		t.Errorf("output missing topic host:\n%s", doc.PageContent)
	}
	i, j, k := strings.Index(doc.PageContent, "first post body"),
		strings.Index(doc.PageContent, "second post body"),
		strings.Index(doc.PageContent, "third post body")
	if i >= j || j >= k {
		t.Errorf("posts out of order (%d,%d,%d):\n%s", i, j, k, doc.PageContent)
	}
}

// A 429 on the print view must fall back to the plain topic JSON plus batched
// posts.json requests for the ids the first chunk didn't include, merged back in
// post_stream.stream order.
func TestCrawlFallsBackToBatchesOn429(t *testing.T) {
	testCrawlFallsBackToBatches(t, http.StatusTooManyRequests)
}

// discuss.python.org rate-limits the print view with 422, not 429 (observed live
// 2026-08-03: {"errors":["You’ve performed this action too many times…"]}).
func TestCrawlFallsBackToBatchesOn422(t *testing.T) {
	testCrawlFallsBackToBatches(t, http.StatusUnprocessableEntity)
}

func testCrawlFallsBackToBatches(t *testing.T, printStatus int) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch {
		case r.URL.Query().Get("print") == "true":
			http.Error(w, "rate limited", printStatus)
		case r.URL.Path == "/t/55763.json":
			// First chunk: the LAST two posts of the stream, so a merge that just
			// appends batches would produce the wrong order.
			_, _ = io.WriteString(w, `{"title":"Batched topic","posts_count":4,
				"created_at":"2026-01-01T00:00:00Z","post_stream":{"stream":[11,12,13,14],"posts":[
				{"id":13,"post_number":3,"username":"carol","created_at":"2026-01-04T00:00:00Z",
				 "reply_to_post_number":2,"raw":"third post body"},
				{"id":14,"post_number":4,"username":"dave","created_at":"2026-01-05T00:00:00Z",
				 "reply_to_post_number":3,"raw":"fourth post body"}]}}`)
		case r.URL.Path == "/t/55763/posts.json":
			_, _ = io.WriteString(w, `{"post_stream":{"posts":[
				{"id":11,"post_number":1,"username":"alice","created_at":"2026-01-01T00:00:00Z",
				 "raw":"first post body"},
				{"id":12,"post_number":2,"username":"bob","created_at":"2026-01-02T00:00:00Z",
				 "reply_to_post_number":1,"raw":"second post body"}]}}`)
		default:
			t.Errorf("unexpected path %q", r.URL.RequestURI())
		}
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	doc, err := e.Crawl(context.Background(), srv.URL+"/t/batched/55763", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("fetched %d urls (%v), want 3 (print + topic + one batch)", len(paths), paths)
	}
	// The batch request must carry exactly the two missing ids, plus include_raw.
	batch := paths[2]
	for _, want := range []string{"post_ids%5B%5D=11", "post_ids%5B%5D=12", "include_raw=1"} {
		if !strings.Contains(batch, want) {
			t.Errorf("batch request %q missing %q", batch, want)
		}
	}
	for _, unwanted := range []string{"post_ids%5B%5D=13", "post_ids%5B%5D=14", "print=true"} {
		if strings.Contains(batch, unwanted) {
			t.Errorf("batch request %q should not contain %q", batch, unwanted)
		}
	}
	if doc.Metadata["posts"] != "4" {
		t.Errorf("posts = %q, want 4", doc.Metadata["posts"])
	}
	if _, ok := doc.Metadata["truncated_from"]; ok {
		t.Errorf("unexpected truncated_from = %q", doc.Metadata["truncated_from"])
	}
	// Stream order, not batch-arrival order.
	var idx []int
	for _, body := range []string{"first post body", "second post body", "third post body", "fourth post body"} {
		at := strings.Index(doc.PageContent, body)
		if at < 0 {
			t.Fatalf("output missing %q:\n%s", body, doc.PageContent)
		}
		idx = append(idx, at)
	}
	for n := 1; n < len(idx); n++ {
		if idx[n-1] >= idx[n] {
			t.Fatalf("posts not merged in stream order (%v):\n%s", idx, doc.PageContent)
		}
	}
}

// More posts than the budget must be truncated, with truncated_from reporting the
// upstream total.
func TestCrawlCapsPosts(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(&b, `{"title":"Megathread","posts_count":%d,"created_at":"2026-01-01T00:00:00Z",`, maxPosts+10)
	b.WriteString(`"post_stream":{"posts":[`)
	for i := 0; i < maxPosts+10; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%d,"post_number":%d,"username":"u%d","created_at":"2026-01-01T00:00:00Z","raw":"body %d"}`,
			i+1, i+1, i, i)
	}
	b.WriteString(`]}}`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, b.String())
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	doc, err := e.Crawl(context.Background(), srv.URL+"/t/mega/1", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if got, want := doc.Metadata["posts"], fmt.Sprint(maxPosts); got != want {
		t.Errorf("posts = %q, want %q", got, want)
	}
	if got, want := doc.Metadata["truncated_from"], fmt.Sprint(maxPosts+10); got != want {
		t.Errorf("truncated_from = %q, want %q", got, want)
	}
}

// A missing topic must surface as a typed FetchError carrying the status, not a
// silent fall-through.
func TestCrawlNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Not Found", http.StatusNotFound)
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	_, err := e.Crawl(context.Background(), srv.URL+"/t/gone/404", domain.EngineOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.StatusCode != http.StatusNotFound {
		t.Fatalf("want *FetchError with 404, got %v", err)
	}
}

// A 200 with an unparseable body must classify as KindBadResponse.
func TestCrawlParseError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	_, err := e.Crawl(context.Background(), srv.URL+"/t/bad/1", domain.EngineOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindBadResponse {
		t.Fatalf("want KindBadResponse, got %v", err)
	}
}

// Without raw, the body falls back to the cooked HTML with tags stripped: script
// content dropped, block ends and <br> turned into line breaks, entities decoded.
func TestCrawlCookedFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"title":"Cooked only","posts_count":1,"created_at":"2026-01-01T00:00:00Z",
			"post_stream":{"posts":[{"id":1,"post_number":1,"username":"alice",
			"created_at":"2026-01-01T00:00:00Z","cooked":"<p>Use <code>a &amp; b</code></p><script>evil()</script><p>line one<br>line two</p>"}]}}`)
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	doc, err := e.Crawl(context.Background(), srv.URL+"/t/cooked/1", domain.EngineOptions{})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	for _, want := range []string{"Use a & b", "line one", "line two"} {
		if !strings.Contains(doc.PageContent, want) {
			t.Errorf("output missing %q:\n%s", want, doc.PageContent)
		}
	}
	for _, unwanted := range []string{"evil()", "<p>", "<code>", "&amp;"} {
		if strings.Contains(doc.PageContent, unwanted) {
			t.Errorf("output should not contain %q:\n%s", unwanted, doc.PageContent)
		}
	}
}

// stripHTML is the only hand-rolled parsing in this package; pin its behavior.
func TestStripHTML(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "hello", "hello"},
		{"paragraphs", "<p>one</p><p>two</p>", "one\n\ntwo"},
		{"br", "one<br/>two", "one\ntwo"},
		{"entities", "a &lt;b&gt; &amp; c", "a <b> & c"},
		{"style dropped", "<style>p{color:red}</style>text", "text"},
		{"attributes", `<a href="https://x/">link</a>`, "link"},
		{"blank line collapse", "<p>a</p><div></div><div></div><p>b</p>", "a\n\nb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripHTML(tc.in); got != tc.want {
				t.Errorf("stripHTML(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Discourse's slugless share-link form /t/{topic_id}/{post_number} (both numeric)
// must resolve to topic_id — parsing it as slug={topic_id}, id={post_number}
// silently fetches a completely different topic.
func TestCrawlSluglessPostNumberFetchesRightTopic(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/t/55763.json" {
			t.Errorf("fetched %q, want /t/55763.json", r.URL.Path)
		}
		_, _ = io.WriteString(w, printJSON)
	}))
	defer srv.Close()

	e := newTestEngine(t, srv.URL)
	if _, err := e.Crawl(context.Background(), srv.URL+"/t/55763/6", domain.EngineOptions{}); err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if len(paths) != 1 {
		t.Fatalf("fetched %d urls (%v), want 1", len(paths), paths)
	}
}
