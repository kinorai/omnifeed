package openwebui

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kinorai/omnifeed/internal/auth"
	"github.com/kinorai/omnifeed/internal/domain"
)

type panickyEngine struct{}

func (panickyEngine) Name() string        { return "panicky" }
func (panickyEngine) Matches(string) bool { return true }
func (panickyEngine) Crawl(_ context.Context, rawURL string, _ domain.EngineOptions) (domain.Document, error) {
	if strings.Contains(rawURL, "bad") {
		panic("engine bug")
	}
	return domain.Document{PageContent: "fine"}, nil
}

// Each URL of a batch is crawled on its own goroutine, out of reach of
// net/http's recover: a panic must fail that URL's document, not the process
// and not the rest of the batch.
func TestCrawl_PanicFailsOnlyThatURL(t *testing.T) {
	srv := newTestServer(panickyEngine{}, auth.AlwaysAllow{}, 30)
	rec := do(srv, http.MethodPost, `{"urls":["https://example.com/bad","https://example.com/good"]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var docs []loaderDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &docs); err != nil {
		t.Fatal(err)
	}
	if docs[0].Metadata["error"] != "true" || docs[1].PageContent != "fine" {
		t.Fatalf("docs = %+v, want [error, fine]", docs)
	}
}
