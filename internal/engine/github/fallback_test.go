package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// pageStub stands in for the generic browser engine.
type pageStub struct{ called bool }

func (*pageStub) Name() string        { return "crawl4ai" }
func (*pageStub) Matches(string) bool { return false }
func (p *pageStub) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	p.called = true
	return domain.Document{PageContent: "rendered github.com page", Metadata: map[string]string{}}, nil
}

// GitHub reads api.github.com, a separate host from the github.com page, so
// an API 403 (anonymous 60/h quota spent) still falls back to the page render
// for an agent, marked as such — but never for a caller that passed format.
func TestRegistry_GitHubAPIQuotaFallsBackToPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"API rate limit exceeded"}`)
	}))
	defer srv.Close()
	e := New(Config{Client: httpx.New(nil), APIBase: srv.URL})
	if sh, ok := any(e).(domain.SameHostEngine); ok && sh.SameHostAsPage() {
		t.Fatal("the GitHub engine reads api.github.com; it must not claim the page's host")
	}
	const issue = "https://github.com/octo/repo/issues/1"

	page := &pageStub{}
	doc, err := engine.New().Register(e).Fallback(page).Crawl(context.Background(), issue, domain.EngineOptions{})
	if err != nil || !page.called {
		t.Fatalf("Crawl = %v (fallback called=%v), want the page render", err, page.called)
	}
	if doc.Metadata["fallback_from"] != "github" || doc.Metadata["fallback_reason"] != string(domain.KindHTTP403) {
		t.Errorf("fallback not marked: %v", doc.Metadata)
	}
	if !strings.HasPrefix(doc.PageContent, "> Note: the dedicated github engine failed (http_403)") {
		t.Errorf("missing notice line: %q", doc.PageContent)
	}

	page = &pageStub{}
	_, err = engine.New().Register(e).Fallback(page).Crawl(context.Background(), issue,
		domain.EngineOptions{RedditFormat: "json", FormatExplicit: true})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindHTTP403 || page.called {
		t.Fatalf("explicit format: err=%v fallback called=%v, want http_403 and no fallback", err, page.called)
	}
}
