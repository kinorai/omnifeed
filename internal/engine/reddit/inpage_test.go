package reddit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
	"github.com/kinorai/omnifeed/internal/httpx"
)

// countingQuota admits everything and records what it was asked: each Acquire,
// and each Penalize with its hold.
type countingQuota struct {
	mu        sync.Mutex
	acquired  int
	penalties []time.Duration
}

func (q *countingQuota) Acquire(context.Context, string, string) (func(), error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.acquired++
	return func() {}, nil
}

func (q *countingQuota) Penalize(_ string, d time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.penalties = append(q.penalties, d)
}

func (q *countingQuota) count() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.acquired
}

// waitForCount polls q until it has admitted want requests: the in-page
// requests are charged after the answer, off the caller's path.
func waitForCount(t *testing.T, q *countingQuota, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for q.count() < want && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := q.count(); got != want {
		t.Fatalf("quota admitted %d requests, want %d", got, want)
	}
}

// The requests a page sends itself (the envelope's n) count in the quota on
// top of the page load, and are reported by kind.
func TestFetch_InPageRequestsAreCharged(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		fetch func(*Session) error
	}{
		{"thread", func(s *Session) error {
			_, err := s.FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top", 10)
			return err
		}},
		{"listing", func(s *Session) error {
			_, err := s.FetchListing(context.Background(), "news", "top", 100, "week")
			return err
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			env, _ := json.Marshal(fetchEnvelope{S: 200, B: validListing, N: 4})
			sess := &fakeSession{evalFn: func(string) (string, error) { return string(env), nil }}
			q := &countingQuota{}
			var reported []string
			f := NewFetcher(FetcherConfig{Browser: &fakeBrowser{name: "crawl4ai", session: sess}, Quota: q,
				OnInPage: func(kind string, n int) { reported = append(reported, fmt.Sprintf("%s=%d", kind, n)) }})
			if err := tc.fetch(openFirst(t, f)); err != nil {
				t.Fatalf("fetch: %v", err)
			}
			waitForCount(t, q, 1+4)
			if want := tc.kind + "=4"; len(reported) != 1 || reported[0] != want {
				t.Fatalf("reported %v, want [%s]", reported, want)
			}
		})
	}
}

// A page that sent nothing itself costs its page load only.
func TestFetch_NoInPageRequestsChargesThePageOnly(t *testing.T) {
	sess := &fakeSession{evalFn: func(string) (string, error) { return envStr(200, validListing), nil }}
	q := &countingQuota{}
	f := NewFetcher(FetcherConfig{Browser: &fakeBrowser{name: "crawl4ai", session: sess}, Quota: q})
	if _, err := openFirst(t, f).FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top", 3); err != nil {
		t.Fatalf("FetchThread: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if got := q.count(); got != 1 {
		t.Fatalf("quota admitted %d requests, want 1", got)
	}
}

// expand bounds the "more replies" requests the thread snippet may send: 0
// sends none, and no value goes past maxExpandRequests.
func TestFetchThread_ExpandCapsInPageRequests(t *testing.T) {
	for _, tc := range []struct{ expand, want int }{{0, 0}, {2, 2}, {-1, 0}, {40, maxExpandRequests}} {
		sess := &fakeSession{evalFn: func(string) (string, error) { return envStr(200, validListing), nil }}
		f := NewFetcher(FetcherConfig{Browser: &fakeBrowser{name: "crawl4ai", session: sess}})
		if _, err := openFirst(t, f).FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top", tc.expand); err != nil {
			t.Fatalf("expand %d: %v", tc.expand, err)
		}
		if want := fmt.Sprintf("MAX_REQ = %d,", tc.want); !strings.Contains(sess.evals[0], want) {
			t.Errorf("expand %d: snippet lacks %q", tc.expand, want)
		}
	}
}

// A page load crawl4ai answered with its scrubbed 500 holds every Reddit
// request for a minute: crawl4ai hides the cause, and it is usually Reddit's
// 429. A 500 that names its verdict, any other status, and a page that
// answered do not.
func TestFetch_FailedPageHoldsTheQuota(t *testing.T) {
	c4 := func(status int, body string) error {
		return httpx.ClassifyClientError(&httpx.StatusError{StatusCode: status, Body: body}, domain.KindUpstreamError)
	}
	for _, tc := range []struct {
		name string
		err  error
		want []time.Duration
	}{
		{"scrubbed 500", c4(500, `{"error":"Internal server error","correlation_id":"7efd9b9dc621"}`),
			[]time.Duration{defaultRateLimitBackoff}},
		{"content gate 500", c4(500, `{"error":"Blocked by anti-bot protection: Structural: minimal_text"}`), nil},
		{"crawl4ai restarting 503", c4(503, `{"error":"Service Unavailable"}`), nil},
		{"crawl4ai time limit 504", c4(504, ""), nil},
		{"no status", errors.New("connection refused"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess := &fakeSession{evalFn: func(string) (string, error) { return "", tc.err }}
			q := &countingQuota{}
			f := NewFetcher(FetcherConfig{Browser: &fakeBrowser{name: "crawl4ai", session: sess}, Quota: q})
			if _, err := openFirst(t, f).FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top", 0); err == nil {
				t.Fatal("FetchThread succeeded, want the crawl4ai error")
			}
			if fmt.Sprint(q.penalties) != fmt.Sprint(tc.want) {
				t.Fatalf("penalties %v, want %v", q.penalties, tc.want)
			}
		})
	}
}
