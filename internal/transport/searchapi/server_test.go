package searchapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/auth"
	"github.com/kinorai/omnifeed/internal/domain"
)

type fakeSearcher struct {
	gotQuery string
	gotOpts  domain.SearchOptions
	results  []domain.SearchResult
	err      error
}

func (f *fakeSearcher) Name() string { return "fake" }

func (f *fakeSearcher) Search(_ context.Context, query string, opts domain.SearchOptions) ([]domain.SearchResult, error) {
	f.gotQuery = query
	f.gotOpts = opts
	return f.results, f.err
}

func newServer(s domain.Searcher, a auth.Authenticator) *Server {
	return New(Config{Searcher: s, Authenticator: a, MaxResults: 25})
}

func do(srv *Server, method, body string, header http.Header) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/search", strings.NewReader(body))
	if header != nil {
		req.Header = header
	}
	rec := httptest.NewRecorder()
	srv.search(rec, req)
	return rec
}

func TestSearch_OKMapsRequestAndReturnsResults(t *testing.T) {
	fake := &fakeSearcher{results: []domain.SearchResult{
		{Title: "First", URL: "https://example.com/a", Snippet: "a", Engine: "google"},
		{Title: "Second", URL: "https://example.com/b"},
	}}
	srv := newServer(fake, auth.AlwaysAllow{})

	rec := do(srv, http.MethodPost, `{"query":"golang","limit":2,"time_range":"week","language":"en"}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if fake.gotQuery != "golang" {
		t.Fatalf("query: got %q, want golang", fake.gotQuery)
	}
	if fake.gotOpts.Limit != 2 || fake.gotOpts.TimeRange != "week" || fake.gotOpts.Language != "en" {
		t.Fatalf("opts mapped wrong: %+v", fake.gotOpts)
	}
	var got []domain.SearchResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got) != 2 || got[0].Title != "First" || got[0].URL != "https://example.com/a" {
		t.Fatalf("results mapped wrong: %+v", got)
	}
}

func TestSearch_DefaultLimitWhenOmitted(t *testing.T) {
	fake := &fakeSearcher{}
	srv := newServer(fake, auth.AlwaysAllow{})

	do(srv, http.MethodPost, `{"query":"q"}`, nil)

	if fake.gotOpts.Limit != defaultSearchLimit {
		t.Fatalf("default limit: got %d, want %d", fake.gotOpts.Limit, defaultSearchLimit)
	}
}

func TestSearch_LimitClampedToMax(t *testing.T) {
	fake := &fakeSearcher{}
	srv := newServer(fake, auth.AlwaysAllow{})

	do(srv, http.MethodPost, `{"query":"q","limit":1000}`, nil)

	if fake.gotOpts.Limit != 25 {
		t.Fatalf("clamp: got %d, want 25 (maxResults)", fake.gotOpts.Limit)
	}
}

func TestSearch_EmptyQueryRejected(t *testing.T) {
	srv := newServer(&fakeSearcher{}, auth.AlwaysAllow{})

	rec := do(srv, http.MethodPost, `{"query":""}`, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestSearch_InvalidTimeRangeRejected(t *testing.T) {
	srv := newServer(&fakeSearcher{}, auth.AlwaysAllow{})

	rec := do(srv, http.MethodPost, `{"query":"q","time_range":"decade"}`, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", rec.Code)
	}
}

func TestSearch_MethodNotAllowed(t *testing.T) {
	srv := newServer(&fakeSearcher{}, auth.AlwaysAllow{})

	rec := do(srv, http.MethodGet, "", nil)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", rec.Code)
	}
}

func TestSearch_UpstreamErrorIsBadGateway(t *testing.T) {
	srv := newServer(&fakeSearcher{err: context.DeadlineExceeded}, auth.AlwaysAllow{})

	rec := do(srv, http.MethodPost, `{"query":"q"}`, nil)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", rec.Code)
	}
}

func TestSearch_Unauthorized(t *testing.T) {
	srv := newServer(&fakeSearcher{}, auth.NewSharedBearer("secret"))

	rec := do(srv, http.MethodPost, `{"query":"q"}`, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}
