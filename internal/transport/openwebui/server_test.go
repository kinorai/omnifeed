package openwebui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/auth"
	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"github.com/kinorai/omnifeed/internal/engine/reddit"
)

type fakeEngine struct {
	doc domain.Document
	err error
}

func (fakeEngine) Name() string        { return "fake" }
func (fakeEngine) Matches(string) bool { return true }
func (e fakeEngine) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	return e.doc, e.err
}

// newTestServer wires the fake engine as the registry fallback so every URL
// dispatches to it. maxURLs of 0 lets New apply its default (exercises that path).
func newTestServer(eng domain.Engine, a auth.Authenticator, maxURLs int) *Server {
	return New(Config{
		Registry:          engine.New().Fallback(eng),
		Authenticator:     a,
		MaxURLsPerRequest: maxURLs,
		RedditDefaults:    reddit.Options{Format: "toon", KeepCreated: true, MaxRounds: 3},
	})
}

func do(srv *Server, method, body string, hdr http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/crawl", strings.NewReader(body))
	if hdr != nil {
		req.Header = hdr
	}
	rec := httptest.NewRecorder()
	srv.crawl(rec, req)
	return rec
}

// New applies a default of 30 when MaxURLsPerRequest is unset. This guard is
// reachable: config.Load does NOT range-check OMNIFEED_MAX_URLS_PER_REQUEST, so
// an operator value of 0 lands here.
func TestNew_DefaultMaxURLs(t *testing.T) {
	if got := New(Config{}).maxURLsPerReq; got != 30 {
		t.Fatalf("default maxURLsPerReq: got %d, want 30", got)
	}
}

func TestCrawl_MethodNotAllowed(t *testing.T) {
	rec := do(newTestServer(fakeEngine{}, auth.AlwaysAllow{}, 30), http.MethodGet, "", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
}

func TestCrawl_Unauthorized(t *testing.T) {
	srv := newTestServer(fakeEngine{}, auth.NewSharedBearer("secret"), 30)
	rec := do(srv, http.MethodPost, `{"urls":["https://example.com"]}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

func TestCrawl_EmptyURLs(t *testing.T) {
	rec := do(newTestServer(fakeEngine{}, auth.AlwaysAllow{}, 30), http.MethodPost, `{"urls":[]}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

// 31 URLs against the default cap of 30 (maxURLs=0 -> New defaults to 30) must
// be rejected — the end-to-end guard behind item 4.
func TestCrawl_TooManyURLs(t *testing.T) {
	urls := make([]string, 31)
	for i := range urls {
		urls[i] = "https://example.com"
	}
	body, _ := json.Marshal(loaderRequest{URLs: urls})
	rec := do(newTestServer(fakeEngine{}, auth.AlwaysAllow{}, 0), http.MethodPost, string(body), nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "max 30") {
		t.Fatalf("expected 'max 30' in error, got %q", rec.Body.String())
	}
}

func TestCrawl_HappyPath(t *testing.T) {
	eng := fakeEngine{doc: domain.Document{
		PageContent: "hello world",
		Metadata:    map[string]string{"source": "https://example.com", "status_code": "200"},
	}}
	rec := do(newTestServer(eng, auth.AlwaysAllow{}, 30), http.MethodPost, `{"urls":["https://example.com"]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	var got []loaderDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].PageContent != "hello world" {
		t.Fatalf("body mapped wrong: %+v", got)
	}
}

// A per-URL engine failure is reported as a 200 with an error document (Open
// WebUI contract), NOT an HTTP error — pin this so a refactor can't change it.
func TestCrawl_EngineErrorBecomesErrorDoc(t *testing.T) {
	eng := fakeEngine{err: &domain.FetchError{Kind: domain.KindUpstreamError}}
	rec := do(newTestServer(eng, auth.AlwaysAllow{}, 30), http.MethodPost, `{"urls":["https://example.com"]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var got []loaderDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Metadata["error"] != "true" {
		t.Fatalf("expected error doc, got %+v", got)
	}
}

// buildEngineOptions parses the query knobs. The expand cases pin parseIntDefault
// (item 8): valid int, full/all/max alias, and garbage/negative -> fall back to
// the default. strconv.Atoi must reproduce all of these.
func TestBuildEngineOptions(t *testing.T) {
	srv := newTestServer(fakeEngine{}, auth.AlwaysAllow{}, 30)
	base := domain.EngineOptions{RedditFormat: "toon", RedditKeepCreated: true, RedditMaxRounds: 3}

	cases := []struct {
		name  string
		query string
		want  domain.EngineOptions
	}{
		{"defaults", "", base},
		{"expand_int", "expand=5", domain.EngineOptions{RedditFormat: "toon", RedditKeepCreated: true, RedditMaxRounds: 5}},
		{"expand_full", "expand=full", domain.EngineOptions{RedditFormat: "toon", RedditKeepCreated: true, RedditMaxRounds: reddit.MaxExpansionRounds}},
		{"expand_all", "expand=all", domain.EngineOptions{RedditFormat: "toon", RedditKeepCreated: true, RedditMaxRounds: reddit.MaxExpansionRounds}},
		{"expand_max", "expand=max", domain.EngineOptions{RedditFormat: "toon", RedditKeepCreated: true, RedditMaxRounds: reddit.MaxExpansionRounds}},
		{"expand_garbage_falls_back", "expand=abc", base},
		{"expand_negative_falls_back", "expand=-2", base},
		{"format_json", "format=json", domain.EngineOptions{RedditFormat: "json", RedditKeepCreated: true, RedditMaxRounds: 3}},
		{"format_invalid_keeps_default", "format=bogus", base},
		{"depth_on", "depth=1", domain.EngineOptions{RedditFormat: "toon", RedditKeepDepth: true, RedditKeepCreated: true, RedditMaxRounds: 3}},
		{"nocreated_off", "nocreated=1", domain.EngineOptions{RedditFormat: "toon", RedditKeepCreated: false, RedditMaxRounds: 3}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/crawl?"+c.query, nil)
			if got := srv.buildEngineOptions(req); got != c.want {
				t.Fatalf("query %q: got %+v, want %+v", c.query, got, c.want)
			}
		})
	}
}
