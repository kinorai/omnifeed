package hackernews

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

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
	_, err := e.Crawl(ctx, "https://news.ycombinator.com/item?id=1", domain.EngineOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindQuotaExhausted || fe.RetryAfter < 100*time.Second {
		t.Fatalf("err = %v, want quota_exhausted with ~2m RetryAfter", err)
	}
	if hits != 0 {
		t.Fatalf("upstream hit %d times despite the refusal", hits)
	}
}
