package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/kinorai/omnifeed/internal/domain"
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
