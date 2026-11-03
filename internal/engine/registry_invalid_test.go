package engine

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/kinorai/omnifeed/internal/observability"
)

// A URL rejected at the choke point is the caller's mistake: transports report
// it as invalid_request, so the error must carry a *domain.InvalidRequestError
// — without changing its message.
func TestRegistryCrawl_RejectedURLIsInvalidRequest(t *testing.T) {
	for _, rawURL := range []string{"http://127.0.0.1/", "file:///etc/passwd", "http:///nohost"} {
		stub := &stubEngine{}
		_, err := New().Fallback(stub).BlockPrivateIPs(true).Crawl(context.Background(), rawURL, domain.EngineOptions{})
		if _, ok := errors.AsType[*domain.InvalidRequestError](err); !ok {
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

// A host the DNS filter sinkholes to 0.0.0.0 is still refused, but as
// dns_blocked — its own reason label and error code, not retryable — instead
// of invalid_request with reason "error", which fired OmnifeedCrawlErrors for
// sites that are simply blocked. A real private address keeps invalid_request.
func TestRejected_DNSBlocked(t *testing.T) {
	err := rejected(&httpx.DNSBlockedError{Host: "umami.is", Addr: netip.MustParseAddr("0.0.0.0")})
	if got := observability.Reason(err); got != "dns_blocked" {
		t.Errorf("Reason = %q, want dns_blocked", got)
	}
	if f := observability.Classify(err); f.Code != observability.CodeDNSBlocked || f.Retryable {
		t.Errorf("Classify = %+v, want dns_blocked, not retryable", f)
	}
	if want := "dns_blocked: the resolver answered 0.0.0.0 for umami.is: a DNS filter blocks this name"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}

	private := rejected(errors.New("private/reserved IP 10.0.0.5 not allowed"))
	if _, ok := errors.AsType[*domain.InvalidRequestError](private); !ok {
		t.Errorf("private address: err = %v, want invalid_request", private)
	}
	if got := observability.Reason(private); got == "dns_blocked" {
		t.Errorf("private address classified as dns_blocked")
	}
}
