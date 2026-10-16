package reddit

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
)

// chainFixture fakes Reddit for both direct methods: the client-ID method
// calls oauth.reddit.com, the session method www.reddit.com. status maps a
// host to the status its API answers; hits counts API calls per host.
type chainFixture struct {
	status   map[string]int
	hits     map[string]int
	outcomes []string
	states   []string
}

func newChainFetcher(t *testing.T, fx *chainFixture, methods ...string) *Fetcher {
	t.Helper()
	directRetryDelay = time.Millisecond
	rt := roundTrip(func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/v1/access_token" {
			return reply(r, 200, `{"access_token":"tok","expires_in":86400}`, nil)
		}
		fx.hits[r.URL.Host]++
		code := fx.status[r.URL.Host]
		if code == 0 {
			code = 200
		}
		if code != 200 {
			return reply(r, code, "<html>blocked</html>", nil)
		}
		return reply(r, 200, validListing, nil)
	})
	return NewFetcher(FetcherConfig{
		ClientID: "app", SessionCookie: "sess", UserAgent: "omnifeed/test",
		HTTP: &http.Client{Transport: rt}, Methods: methods, Cooldown: time.Hour,
		OnOutcome: func(m, o string) { fx.outcomes = append(fx.outcomes, m+"="+o) },
		OnState: func(m string, up bool, _ string, _ time.Duration) {
			fx.states = append(fx.states, fmt.Sprintf("%s:%v", m, up))
		},
	})
}

func fetchThread(t *testing.T, f *Fetcher) error {
	t.Helper()
	c, err := f.OpenChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	_, err = c.FetchThread(context.Background(), "/r/news/comments/abc123/t/", 500, 20, "top", 3)
	return err
}

func TestChainUsesFirstMethod(t *testing.T) {
	fx := &chainFixture{status: map[string]int{}, hits: map[string]int{}}
	f := newChainFetcher(t, fx)
	if got := fmt.Sprint(f.Methods()); got != "[client_id session]" {
		t.Fatalf("methods = %s (browser has no backend here, so it is left out)", got)
	}
	if err := fetchThread(t, f); err != nil {
		t.Fatal(err)
	}
	if fx.hits["oauth.reddit.com"] != 1 || fx.hits["www.reddit.com"] != 0 {
		t.Errorf("hits = %v, want only the client-ID method", fx.hits)
	}
}

// A refused method fails over to the next one in the same call, and is then
// skipped for its cooldown: the failure is paid once, not on every fetch.
func TestChainFailsOverAndRemembers(t *testing.T) {
	fx := &chainFixture{status: map[string]int{"oauth.reddit.com": 403}, hits: map[string]int{}}
	f := newChainFetcher(t, fx)
	for range 3 {
		if err := fetchThread(t, f); err != nil {
			t.Fatal(err)
		}
	}
	// 2 calls: the 403 and its one retry, in the first fetch only.
	if fx.hits["oauth.reddit.com"] != 2 || fx.hits["www.reddit.com"] != 3 {
		t.Errorf("hits = %v, want the client-ID method tried once (2 calls) then skipped", fx.hits)
	}
	if fmt.Sprint(fx.states) != "[client_id:false]" {
		t.Errorf("states = %v", fx.states)
	}
	if fx.outcomes[0] != "client_id=http_403" || fx.outcomes[1] != "session=ok" {
		t.Errorf("outcomes = %v", fx.outcomes)
	}
}

// A missing thread is the same on every method: no failover, no cooldown.
func TestChainKeepsContentErrors(t *testing.T) {
	fx := &chainFixture{status: map[string]int{"oauth.reddit.com": 404}, hits: map[string]int{}}
	f := newChainFetcher(t, fx)
	if err := fetchThread(t, f); err == nil {
		t.Fatal("a 404 must come back to the caller")
	}
	if fx.hits["www.reddit.com"] != 0 || len(fx.states) != 0 {
		t.Errorf("hits = %v states = %v, want no failover", fx.hits, fx.states)
	}
}

// When every method is cooling down, the chain fails at once with
// quota_exhausted instead of waiting or retrying a method that asked for
// back-off.
func TestChainFailsFastWhenAllAreDown(t *testing.T) {
	fx := &chainFixture{status: map[string]int{"oauth.reddit.com": 403, "www.reddit.com": 403}, hits: map[string]int{}}
	f := newChainFetcher(t, fx)
	if err := fetchThread(t, f); err == nil {
		t.Fatal("every method refused: the call must fail")
	}
	before := fx.hits["oauth.reddit.com"] + fx.hits["www.reddit.com"]
	err := fetchThread(t, f)
	var fe *domain.FetchError
	if !errors.As(err, &fe) || fe.Kind != domain.KindQuotaExhausted || fe.RetryAfter <= 0 {
		t.Fatalf("err = %v, want quota_exhausted with a retry-after", err)
	}
	if after := fx.hits["oauth.reddit.com"] + fx.hits["www.reddit.com"]; after != before {
		t.Errorf("a cooled-down chain sent %d requests, want 0", after-before)
	}
}

// Reddit's back-off on a successful answer holds that method only.
func TestChainHoldsOnBackoff(t *testing.T) {
	fx := &chainFixture{status: map[string]int{}, hits: map[string]int{}}
	f := newChainFetcher(t, fx)
	f.methods[0].open = func(context.Context) (*Session, error) {
		s := &Session{direct: newTokenClient(&http.Client{Transport: roundTrip(func(r *http.Request) *http.Response {
			if r.URL.Path == "/api/v1/access_token" {
				return reply(r, 200, `{"access_token":"tok","expires_in":86400}`, nil)
			}
			fx.hits[r.URL.Host]++
			return reply(r, 200, validListing, map[string]string{"X-Ratelimit-Remaining": "0", "X-Ratelimit-Reset": "300"})
		})}, "omnifeed/test", "app")}
		s.penalize = f.penalizeMethod(f.methods[0], s)
		return s, nil
	}
	for range 2 {
		if err := fetchThread(t, f); err != nil {
			t.Fatal(err)
		}
	}
	if fx.hits["oauth.reddit.com"] != 1 || fx.hits["www.reddit.com"] != 1 {
		t.Errorf("hits = %v, want the held client-ID method skipped on the second fetch", fx.hits)
	}
}

// The configured order is the order tried.
func TestChainOrder(t *testing.T) {
	fx := &chainFixture{status: map[string]int{}, hits: map[string]int{}}
	f := newChainFetcher(t, fx, MethodSession, MethodClientID)
	if err := fetchThread(t, f); err != nil {
		t.Fatal(err)
	}
	if fx.hits["www.reddit.com"] != 1 || fx.hits["oauth.reddit.com"] != 0 {
		t.Errorf("hits = %v, want the session method first", fx.hits)
	}
}

// The reported state follows what was observed, not the clock: after the
// cooldown the method stays down until a call through it succeeds, and a
// failure on the retry does not report "down" a second time.
func TestChainReportsRecoveryOnSuccess(t *testing.T) {
	fx := &chainFixture{status: map[string]int{"oauth.reddit.com": 403}, hits: map[string]int{}}
	f := newChainFetcher(t, fx)
	clk := time.Now()
	f.now = func() time.Time { return clk }
	if err := fetchThread(t, f); err != nil {
		t.Fatal(err)
	}
	clk = clk.Add(2 * time.Hour) // past the 1h cooldown, still refused
	if err := fetchThread(t, f); err != nil {
		t.Fatal(err)
	}
	clk = clk.Add(2 * time.Hour)
	fx.status["oauth.reddit.com"] = 200
	if err := fetchThread(t, f); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(fx.states); got != "[client_id:false client_id:true]" {
		t.Errorf("states = %s, want one down then one up", got)
	}
}

// fakeShared is an in-memory SharedState. err, when set, fails every call.
type fakeShared struct {
	holds map[string]time.Duration
	calls []string
	err   error
}

func (s *fakeShared) Holds(_ context.Context, _ []string) (map[string]time.Duration, error) {
	s.calls = append(s.calls, "read")
	if s.err != nil {
		return nil, s.err
	}
	return s.holds, nil
}

func (s *fakeShared) Hold(_ context.Context, m string, d time.Duration) error {
	s.calls = append(s.calls, "hold:"+m)
	if s.err != nil {
		return s.err
	}
	if d > s.holds[m] {
		s.holds[m] = d
	}
	return nil
}

func (s *fakeShared) Clear(_ context.Context, m string) error {
	s.calls = append(s.calls, "clear:"+m)
	if s.err != nil {
		return s.err
	}
	delete(s.holds, m)
	return nil
}

// Two replicas share one state: a credential refused on one is skipped on the
// other without a request, and the other reports it down too.
func TestChainSharesHoldsBetweenReplicas(t *testing.T) {
	shared := &fakeShared{holds: map[string]time.Duration{}}
	fxA := &chainFixture{status: map[string]int{"oauth.reddit.com": 403}, hits: map[string]int{}}
	fxB := &chainFixture{status: map[string]int{}, hits: map[string]int{}}
	a, b := newChainFetcher(t, fxA), newChainFetcher(t, fxB)
	a.shared, b.shared = shared, shared
	if err := fetchThread(t, a); err != nil {
		t.Fatal(err)
	}
	if shared.holds[MethodClientID] != time.Hour {
		t.Fatalf("holds = %v, want client_id held for the cooldown", shared.holds)
	}
	if err := fetchThread(t, b); err != nil {
		t.Fatal(err)
	}
	if fxB.hits["oauth.reddit.com"] != 0 || fxB.hits["www.reddit.com"] != 1 {
		t.Errorf("replica B hits = %v, want client_id skipped", fxB.hits)
	}
	if fmt.Sprint(fxB.states) != "[client_id:false]" {
		t.Errorf("replica B states = %v", fxB.states)
	}
}

// Reddit's back-off on one replica holds the method on every replica.
func TestChainSharesBackoff(t *testing.T) {
	shared := &fakeShared{holds: map[string]time.Duration{}}
	fx := &chainFixture{status: map[string]int{}, hits: map[string]int{}}
	f := newChainFetcher(t, fx)
	f.shared = shared
	s, err := f.methods[0].open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.penalize(redditOrigin+"/", 5*time.Minute)
	if shared.holds[MethodClientID] != 5*time.Minute {
		t.Errorf("holds = %v, want client_id held 5m", shared.holds)
	}
}

// A success through a method that was down ends the hold for every replica.
// A success through a method that was up writes nothing.
func TestChainClearsSharedHoldOnRecovery(t *testing.T) {
	shared := &fakeShared{holds: map[string]time.Duration{}}
	fx := &chainFixture{status: map[string]int{"oauth.reddit.com": 403}, hits: map[string]int{}}
	f := newChainFetcher(t, fx)
	f.shared = shared
	clk := time.Now()
	f.now = func() time.Time { return clk }
	if err := fetchThread(t, f); err != nil {
		t.Fatal(err)
	}
	delete(shared.holds, MethodClientID) // the key's TTL ran out
	clk = clk.Add(2 * time.Hour)
	fx.status["oauth.reddit.com"] = 200
	for range 2 {
		if err := fetchThread(t, f); err != nil {
			t.Fatal(err)
		}
	}
	want := "[read hold:client_id read clear:client_id read]"
	if got := fmt.Sprint(shared.calls); got != want {
		t.Errorf("calls = %s, want %s", got, want)
	}
}

// A failing shared state never fails a call: the chain uses its own memory,
// skips the shared state for 30s, and reports each change once.
func TestChainSharedStateFailsOpen(t *testing.T) {
	shared := &fakeShared{holds: map[string]time.Duration{}, err: errors.New("redis down")}
	fx := &chainFixture{status: map[string]int{"oauth.reddit.com": 403}, hits: map[string]int{}}
	f := newChainFetcher(t, fx)
	f.shared = shared
	var degraded []bool
	var errs []string
	f.onSharedDown = func(down bool) { degraded = append(degraded, down) }
	f.onSharedError = func(op string, _ error) { errs = append(errs, op) }
	clk := time.Now()
	f.now = func() time.Time { return clk }
	for range 2 {
		if err := fetchThread(t, f); err != nil {
			t.Fatal(err)
		}
	}
	if fx.hits["oauth.reddit.com"] != 2 {
		t.Errorf("hits = %v, want client_id remembered in memory", fx.hits)
	}
	if fmt.Sprint(shared.calls) != "[read]" || fmt.Sprint(errs) != "[read]" {
		t.Errorf("calls = %v errs = %v, want one read, then no call during the cooldown", shared.calls, errs)
	}
	clk = clk.Add(sharedCooldown)
	shared.err = nil
	if err := fetchThread(t, f); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(degraded) != "[true false]" {
		t.Errorf("degraded = %v, want one change each way", degraded)
	}
}

// A success never clears a hold that a concurrent call placed while it ran,
// but it clears a hold that ran out before it started.
func TestChainSuccessKeepsConcurrentHold(t *testing.T) {
	fx := &chainFixture{status: map[string]int{}, hits: map[string]int{}}
	f := newChainFetcher(t, fx, MethodClientID)
	clk := time.Now()
	f.now = func() time.Time { return clk }
	m := f.methods[0]
	start := clk
	f.holdMethod(context.Background(), m, time.Hour, whyFailed)
	f.markUp(context.Background(), m, start)
	if !m.until().After(clk) {
		t.Fatal("a success cleared a hold placed during its call")
	}
	clk = clk.Add(2 * time.Hour)
	f.markUp(context.Background(), m, clk)
	if !m.until().IsZero() {
		t.Fatal("a success kept a hold that ran out before it started")
	}
}

// An expansion never falls back to the browser: its logged-out POST would get
// a 403 and hold the browser method, the one path left that reads threads.
func TestChainMoreChildrenSkipsBrowser(t *testing.T) {
	directRetryDelay = time.Millisecond
	sess := alwaysBody(validListing)
	f := NewFetcher(FetcherConfig{
		SessionCookie: "sess", UserAgent: "omnifeed/test", Cooldown: time.Hour,
		Browser: &fakeBrowser{name: "fake", session: sess},
		HTTP: &http.Client{Transport: roundTrip(func(r *http.Request) *http.Response {
			return reply(r, 429, "Too Many Requests", nil)
		})},
	})
	c, err := f.OpenChain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close(context.Background()) }()
	if _, err := c.FetchMoreChildren(context.Background(), "t3_abc123", []string{"c1"}, "top"); err == nil {
		t.Fatal("the only direct method refused: the expansion must fail")
	}
	if len(sess.evals) != 0 {
		t.Errorf("browser ran %d snippets, want 0", len(sess.evals))
	}
	if browser := f.methods[1]; !browser.until().IsZero() {
		t.Errorf("browser held until %v, want it untouched", browser.until())
	}
}
