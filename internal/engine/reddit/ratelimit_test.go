package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/engine"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// rlEnv builds an envelope carrying Reddit's rate-limit headers.
func rlEnv(status int, body, retryAfter, reset, remaining string) string {
	b, _ := json.Marshal(fetchEnvelope{S: status, B: body, RA: retryAfter, RS: reset, RM: remaining})
	return string(b)
}

// Reddit's back-off headers must survive the envelope: they are the only
// signal of how long a block lasts. A 429 becomes an http_429 error carrying
// the wait; a 200 with the budget spent passes the body through but still
// reports the wait, so the next request is held back.
func TestUnwrapEnvelope_RateLimitHeaders(t *testing.T) {
	cases := []struct {
		name     string
		env      string
		wantWait time.Duration
		wantKind domain.FailureKind // "" = success
	}{
		{"429 retry-after wins", rlEnv(429, "Too Many Requests", "90", "300", "0"), 90 * time.Second, domain.KindHTTP429},
		{"429 reset only", rlEnv(429, "Too Many Requests", "", "300", "0.0"), 300 * time.Second, domain.KindHTTP429},
		{"429 no hint", rlEnv(429, "Too Many Requests", "", "", ""), defaultRateLimitBackoff, domain.KindHTTP429},
		{"200 budget spent", rlEnv(200, validListing, "", "42", "0.0"), 42 * time.Second, ""},
		{"200 budget left", rlEnv(200, validListing, "", "42", "57.0"), 0, ""},
		{"403 no headers", rlEnv(403, "<html>blocked</html>", "", "", ""), 0, domain.KindHTTP403},
		{"garbage headers ignored", rlEnv(200, validListing, "soon", "later", "lots"), 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, wait, err := unwrapEnvelope(tc.env)
			if wait != tc.wantWait {
				t.Errorf("wait = %s, want %s", wait, tc.wantWait)
			}
			if tc.wantKind == "" {
				if err != nil || body != validListing {
					t.Fatalf("got (%q, %v), want the body and no error", body, err)
				}
				return
			}
			var fe *domain.FetchError
			if !errors.As(err, &fe) || fe.Kind != tc.wantKind {
				t.Fatalf("err = %v, want kind %s", err, tc.wantKind)
			}
			if tc.wantKind == domain.KindHTTP429 && fe.RetryAfter != tc.wantWait {
				t.Errorf("RetryAfter = %s, want %s", fe.RetryAfter, tc.wantWait)
			}
		})
	}
}

// On a 429 the fetcher hands Reddit's Retry-After to Penalize, keyed on the
// host the engine's limiter paces (www.reddit.com): the next crawl is then
// refused by the limiter up front instead of walking into the same wall.
func TestFetch_RateLimitPenalizesDomain(t *testing.T) {
	limiter := httpx.NewDomainLimiter(2, 0)
	var penalized []string
	sess := &fakeSession{evalFn: func(string) (string, error) {
		return rlEnv(429, "Too Many Requests", "120", "", "0"), nil
	}}
	f := NewFetcher(FetcherConfig{
		Browser: &fakeBrowser{name: "crawl4ai", session: sess},
		Penalize: func(rawURL string, d time.Duration) {
			penalized = append(penalized, rawURL)
			limiter.Penalize(rawURL, d)
		},
	})
	e := New(Config{Fetcher: f, Limiter: limiter})

	_, err := e.Crawl(context.Background(), "https://www.reddit.com/r/news/comments/abc123/t/", domain.EngineOptions{})
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindHTTP429 || fe.RetryAfter != 120*time.Second {
		t.Fatalf("first crawl err = %v, want http_429 with RetryAfter 120s", err)
	}
	if len(penalized) != 1 || penalized[0] != redditOrigin+"/" {
		t.Fatalf("Penalize calls = %v, want one for %s/", penalized, redditOrigin)
	}

	// The next crawl, with a budget shorter than the hold, is refused before it
	// reaches the browser and says how long to wait.
	evals := len(sess.evals)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = e.Crawl(ctx, "https://www.reddit.com/r/news/comments/abc123/t/", domain.EngineOptions{})
	if !errors.As(err, &fe) || fe.Kind != domain.KindQuotaExhausted || fe.RetryAfter < 100*time.Second {
		t.Fatalf("second crawl err = %v, want quota_exhausted with ~120s RetryAfter", err)
	}
	if len(sess.evals) != evals {
		t.Fatal("second crawl reached Reddit despite the penalty")
	}
}

// A success that spends the last of the budget (X-Ratelimit-Remaining 0) is
// returned, and still penalizes so the next request waits for the reset.
func TestFetch_RemainingZeroPenalizesButSucceeds(t *testing.T) {
	var got time.Duration
	sess := &fakeSession{evalFn: func(string) (string, error) {
		return rlEnv(200, validListing, "", "37", "0.0"), nil
	}}
	f := NewFetcher(FetcherConfig{
		Browser:  &fakeBrowser{name: "crawl4ai", session: sess},
		Penalize: func(_ string, d time.Duration) { got = d },
	})
	s, err := f.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top"); err != nil {
		t.Fatalf("FetchThread = %v, want success", err)
	}
	if got != 37*time.Second {
		t.Fatalf("Penalize got %s, want 37s", got)
	}
}

// The Reddit quota counts every request that reaches Reddit — morechildren
// rounds included — not every crawl. With a quota of 2, the third request is
// refused (quota_exhausted, with a retry-after) without reaching the browser.
func TestFetch_QuotaCountsEveryRequest(t *testing.T) {
	sess := alwaysBody(validListing)
	f := NewFetcher(FetcherConfig{
		Browser: &fakeBrowser{name: "crawl4ai", session: sess},
		Quota:   httpx.NewDomainQuotaLimiter(2, 0, 2, time.Minute),
	})
	s, err := f.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := s.FetchThread(ctx, "/r/news/comments/abc123/t/", 500, 20, "top"); err != nil {
		t.Fatalf("thread: %v", err)
	}
	if _, err := s.FetchMoreChildren(ctx, "t3_abc123", []string{"c1"}, "top"); err != nil {
		t.Fatalf("morechildren 1: %v", err)
	}
	_, err = s.FetchMoreChildren(ctx, "t3_abc123", []string{"c2"}, "top")
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindQuotaExhausted || fe.RetryAfter <= 0 {
		t.Fatalf("third request err = %v, want quota_exhausted with a RetryAfter", err)
	}
	if len(sess.evals) != 2 {
		t.Fatalf("browser saw %d requests, want 2 (the third must be refused before sending)", len(sess.evals))
	}
}

// pageStub stands in for the generic browser engine.
type pageStub struct{ called bool }

func (*pageStub) Name() string        { return "crawl4ai" }
func (*pageStub) Matches(string) bool { return false }
func (p *pageStub) Crawl(context.Context, string, domain.EngineOptions) (domain.Document, error) {
	p.called = true
	return domain.Document{PageContent: "[ Skip to main content ](…)"}, nil
}

// Reddit's JSON comes from www.reddit.com, the page's own host: a 429 must
// reach the caller as the engine's error, never as a browser render that hits
// Reddit again (the 2026-10-08 incident) — with or without an explicit format.
func TestRegistry_RedditRateLimitDoesNotFallBack(t *testing.T) {
	for _, opts := range []domain.EngineOptions{{}, {RedditFormat: "json", FormatExplicit: true}} {
		sess := &fakeSession{evalFn: func(string) (string, error) {
			return rlEnv(429, "Too Many Requests", "30", "", "0"), nil
		}}
		e := New(Config{
			Fetcher: NewFetcher(FetcherConfig{Browser: &fakeBrowser{name: "crawl4ai", session: sess}}),
			Limiter: httpx.NewDomainLimiter(2, 0),
		})
		page := &pageStub{}
		_, err := engine.New().Register(e).Fallback(page).
			Crawl(context.Background(), "https://www.reddit.com/r/news/comments/abc123/t/", opts)
		var fe *domain.FetchError
		if !errors.As(err, &fe) || fe.Kind != domain.KindHTTP429 || fe.RetryAfter != 30*time.Second {
			t.Fatalf("opts %+v: err = %v, want http_429 with RetryAfter 30s", opts, err)
		}
		if page.called {
			t.Fatalf("opts %+v: fell back to a browser render of Reddit after a 429", opts)
		}
	}
}
