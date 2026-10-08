package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// A URL rejected at the choke point is the caller's mistake: transports report
// it as invalid_request, so the error must carry a *domain.InvalidRequestError
// — without changing its message.
func TestRegistryCrawl_RejectedURLIsInvalidRequest(t *testing.T) {
	for _, rawURL := range []string{"http://127.0.0.1/", "file:///etc/passwd", "http:///nohost"} {
		stub := &stubEngine{}
		_, err := New().Fallback(stub).BlockPrivateIPs(true).Crawl(context.Background(), rawURL, domain.EngineOptions{})
		var ire *domain.InvalidRequestError
		if !errors.As(err, &ire) {
			t.Errorf("Crawl(%q) err = %v, want a *domain.InvalidRequestError", rawURL, err)
		}
		if stub.called {
			t.Errorf("Crawl(%q) reached the engine", rawURL)
		}
	}
}

type pacedEngine struct{}

func (pacedEngine) Name() string        { return "paced" }
func (pacedEngine) Matches(string) bool { return false }
func (pacedEngine) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	return domain.Document{}, &httpx.WaitBudgetError{RetryAfter: 7 * time.Second}
}

// Engines return the limiter's fast-fail as-is; the registry types it so every
// transport reports quota_exhausted with the retry-after, not an untyped error.
func TestRegistryCrawl_PacingRefusalIsQuotaExhausted(t *testing.T) {
	_, err := New().Fallback(pacedEngine{}).Crawl(context.Background(), "https://example.com/", domain.EngineOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindQuotaExhausted || fe.RetryAfter != 7*time.Second {
		t.Fatalf("Crawl() err = %#v, want a quota_exhausted FetchError with RetryAfter 7s", err)
	}
}
