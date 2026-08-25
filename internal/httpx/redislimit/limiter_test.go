package redislimit

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/redis/go-redis/v9"
)

// Acquire loops: it sleeps the wait Redis reported and retries, so a caller is
// admitted as soon as the window rolls. The wait itself is short here because
// the sleep is a real timer; the arithmetic is pinned deterministically in
// script_test.go.
func TestAcquireAdmitsOnceTheWindowRolls(t *testing.T) {
	l, m, c := newFixture(t, Config{MaxConcurrent: 4, Quota: 1, Window: 200 * time.Millisecond})
	const url = "https://example.com/a"

	release, err := l.Acquire(context.Background(), "crawl4ai", url)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	release()

	var outcomes []string
	l.OnWait = func(_, outcome string, _ time.Duration) { outcomes = append(outcomes, outcome) }

	done := make(chan error, 1)
	go func() {
		_, err := l.Acquire(context.Background(), "crawl4ai", url)
		done <- err
	}()

	// The waiter is sleeping on a 200ms timer; roll the window under it.
	time.Sleep(20 * time.Millisecond)
	c.advance(250 * time.Millisecond)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second Acquire: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Acquire never returned")
	}
	if len(outcomes) != 1 || outcomes[0] != "acquired" {
		t.Fatalf("outcomes = %v, want [acquired]", outcomes)
	}
	assertTTLs(t, m)
}

// A caller that gives up mid-wait must leave nothing behind: waits are not
// booked, so no cleanup path exists to get wrong. The caller is CANCELED rather
// than given a deadline, because a deadline shorter than the wait is now
// refused up front (see TestAcquireFailsFastWhenWaitExceedsTheBudget) and would
// never reach the sleep this covers.
func TestAcquireCancelBooksNothing(t *testing.T) {
	l, m, _ := newFixture(t, Config{MaxConcurrent: 4, Quota: 1, Window: time.Hour})
	const url = "https://example.com/a"
	win, _, _ := l.keys("example.com")

	release, err := l.Acquire(context.Background(), "crawl4ai", url)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	before, err := l.cfg.Client.ZCard(context.Background(), win).Result()
	if err != nil {
		t.Fatalf("ZCARD: %v", err)
	}

	var outcomes []string
	l.OnWait = func(_, outcome string, _ time.Duration) { outcomes = append(outcomes, outcome) }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := l.Acquire(ctx, "crawl4ai", url)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond) // let the waiter reach its sleep
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire on a spent quota: err = %v, want a cancellation", err)
	}

	after, err := l.cfg.Client.ZCard(context.Background(), win).Result()
	if err != nil {
		t.Fatalf("ZCARD: %v", err)
	}
	if after != before {
		t.Fatalf("ZCARD %d → %d: the abandoned waiter booked a slot", before, after)
	}
	if len(outcomes) != 1 || outcomes[0] != "canceled" {
		t.Fatalf("outcomes = %v, want [canceled]", outcomes)
	}
	assertTTLs(t, m)
}

// Every backend failure must arrive wrapped: FallbackLimiter keys on
// ErrLimiterUnavailable to pace in process instead, and an unwrapped error
// would fail the crawl rather than degrade pacing.
func TestAcquireOnDeadRedisIsUnavailable(t *testing.T) {
	l, m, _ := newFixture(t, Config{MaxConcurrent: 4, MinDelay: time.Second})

	var ops []string
	var waits []string
	l.OnError = func(op string) { ops = append(ops, op) }
	l.OnWait = func(_, outcome string, _ time.Duration) { waits = append(waits, outcome) }

	m.Close()

	_, err := l.Acquire(context.Background(), "crawl4ai", "https://example.com/a")
	if !errors.Is(err, httpx.ErrLimiterUnavailable) {
		t.Fatalf("err = %v, want errors.Is ErrLimiterUnavailable", err)
	}
	if len(ops) != 1 || ops[0] != "acquire" {
		t.Fatalf("OnError ops = %v, want [acquire]", ops)
	}
	// The fallback observes the wait it actually serves; a second observation
	// here would double-count every acquire made while Redis is down.
	if len(waits) != 0 {
		t.Fatalf("OnWait fired %v on a backend failure, want nothing", waits)
	}

	// The semaphore slot must have been given back, or a dead Redis would
	// permanently wedge the host even after the fallback takes over.
	sem := l.semFor("example.com")
	if len(sem) != 0 {
		t.Fatalf("semaphore holds %d slots after a failure, want 0", len(sem))
	}
}

// Concurrency stays bounded per host, per pod.
func TestAcquireBoundsConcurrencyPerHost(t *testing.T) {
	l, _, _ := newFixture(t, Config{MaxConcurrent: 2})

	var inFlight, peak atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := l.Acquire(context.Background(), "crawl4ai", "https://example.com/a")
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			n := inFlight.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			inFlight.Add(-1)
			release()
		}()
	}
	close(start)
	wg.Wait()

	if peak.Load() > 2 {
		t.Fatalf("peak in-flight = %d, want <= 2", peak.Load())
	}
}

// Hosts are paced independently: one host's semaphore must not block another.
func TestAcquireSeparatesHosts(t *testing.T) {
	l, _, _ := newFixture(t, Config{MaxConcurrent: 1})

	releaseA, err := l.Acquire(context.Background(), "crawl4ai", "https://a.example.com/x")
	if err != nil {
		t.Fatalf("Acquire a: %v", err)
	}
	defer releaseA()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	releaseB, err := l.Acquire(ctx, "crawl4ai", "https://b.example.com/x")
	if err != nil {
		t.Fatalf("Acquire b while a is held: %v", err)
	}
	releaseB()
}

// A penalty bumps the shared next-allowed-at key, with a TTL (the instance runs
// noeviction), and the next admission waits for it.
func TestPenalizeBumpsNextKey(t *testing.T) {
	l, m, _ := newFixture(t, Config{MaxConcurrent: 1})
	const url = "https://news.ycombinator.com/item?id=1"
	_, nextKey, _ := l.keys("news.ycombinator.com")

	l.Penalize(url, 30*time.Second)

	got, err := m.Get(nextKey)
	if err != nil {
		t.Fatalf("next key missing after Penalize: %v", err)
	}
	want := base.Add(30 * time.Second).UnixMilli()
	if got != strconv.FormatInt(want, 10) {
		t.Fatalf("next = %s, want %d (now + 30s)", got, want)
	}
	assertTTLs(t, m)

	// The hold is what the next admission attempt now waits for.
	if wait := book(t, l, m, "news.ycombinator.com"); wait != 30*time.Second {
		t.Fatalf("book after Penalize waited %v, want 30s", wait)
	}

	// A milder penalty must not shorten it, and a zero one must do nothing.
	l.Penalize(url, time.Second)
	l.Penalize(url, 0)
	if got, _ := m.Get(nextKey); got != strconv.FormatInt(want, 10) {
		t.Fatalf("next = %s after a milder penalty, want %d unchanged", got, want)
	}
}

// A penalty against a dead backend reports op="penalize" and nothing else: the
// caller is a response handler with nothing to do about it.
func TestPenalizeReportsBackendErrors(t *testing.T) {
	l, m, _ := newFixture(t, Config{MaxConcurrent: 1})
	var ops []string
	l.OnError = func(op string) { ops = append(ops, op) }
	m.Close()

	l.Penalize("https://example.com/a", time.Second)

	if len(ops) != 1 || ops[0] != "penalize" {
		t.Fatalf("OnError ops = %v, want [penalize]", ops)
	}
}

// countingLimiter is the minimum httpx.Limiter a FallbackLimiter can fall back
// to: it records how often it was asked.
type countingLimiter struct {
	mu    sync.Mutex
	calls int
}

func (c *countingLimiter) Acquire(context.Context, string, string) (func(), error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return func() {}, nil
}

func (c *countingLimiter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// failingEval is a real client whose script calls fail with a scripted error,
// optionally killing the caller's ctx first — the mid-EVALSHA cancellation
// go-redis reports when a caller walks away while its call waits for a pool
// turn or a retry backoff. miniredis cannot produce it: it always answers.
// Everything else still reaches miniredis, so a test can prove nothing was
// booked.
type failingEval struct {
	redis.UniversalClient
	cancel func() // called before the failure, nil to leave the ctx alive
	err    error
}

func (f *failingEval) EvalSha(ctx context.Context, _ string, _ []string, _ ...any) *redis.Cmd {
	if f.cancel != nil {
		f.cancel()
	}
	cmd := redis.NewCmd(ctx, "evalsha")
	cmd.SetErr(f.err)
	return cmd
}

// A caller that walks away mid-EVALSHA is not a Redis outage. The client reports
// the caller's own dead ctx as its error, and wrapping that in
// ErrLimiterUnavailable would open the fail-open circuit against a healthy
// backend: 30s of per-pod pacing bought by a client disconnect. A timeout raised
// by the client's OWN ReadTimeout arrives with the ctx still alive, and that one
// is a real backend failure.
func TestAcquireDiscriminatesCallerCancellationFromBackendFailure(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		killCtx   bool
		wantWrap  bool
		wantWaits []string
		wantOps   []string
	}{
		{
			name:      "caller canceled mid-eval",
			err:       context.Canceled,
			killCtx:   true,
			wantWaits: []string{"canceled"},
		},
		{
			name:      "caller deadline expired mid-eval",
			err:       context.DeadlineExceeded,
			killCtx:   true,
			wantWaits: []string{"canceled"},
		},
		{
			name:     "client read timeout, caller still alive",
			err:      context.DeadlineExceeded,
			wantWrap: true,
			wantOps:  []string{"acquire"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, m, _ := newFixture(t, Config{MaxConcurrent: 4, MinDelay: time.Second})
			win, next, _ := l.keys("example.com")

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := &failingEval{UniversalClient: l.cfg.Client, err: tc.err}
			if tc.killCtx {
				fake.cancel = cancel
			}
			l.cfg.Client = fake

			var ops, waits []string
			l.OnError = func(op string) { ops = append(ops, op) }
			l.OnWait = func(_, outcome string, _ time.Duration) { waits = append(waits, outcome) }

			_, err := l.Acquire(ctx, "crawl4ai", "https://example.com/a")
			if tc.wantWrap {
				if !errors.Is(err, httpx.ErrLimiterUnavailable) {
					t.Fatalf("err = %v, want errors.Is ErrLimiterUnavailable", err)
				}
			} else {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("err = %v, want context.Canceled (the ctx error, unwrapped)", err)
				}
				if errors.Is(err, httpx.ErrLimiterUnavailable) {
					t.Fatalf("err = %v, want it UNWRAPPED — a wrap opens the circuit", err)
				}
			}
			if len(ops) != len(tc.wantOps) || (len(ops) == 1 && ops[0] != tc.wantOps[0]) {
				t.Fatalf("OnError ops = %v, want %v", ops, tc.wantOps)
			}
			if len(waits) != len(tc.wantWaits) || (len(waits) == 1 && waits[0] != tc.wantWaits[0]) {
				t.Fatalf("OnWait outcomes = %v, want %v", waits, tc.wantWaits)
			}
			// Nothing booked, either way: the script never ran.
			if m.Exists(win) || m.Exists(next) {
				t.Fatalf("keys written after a failed acquire: %v", m.Keys())
			}
			// And the slot is back, or the host stays wedged for good.
			if sem := l.semFor("example.com"); len(sem) != 0 {
				t.Fatalf("semaphore holds %d slots, want 0", len(sem))
			}
		})
	}
}

// The same thing through the composite the wiring actually builds: a canceled
// caller must leave the circuit CLOSED, so the next request still gets shared
// pacing. The pass-through test in package httpx only stubs the error kind;
// this drives a real redislimit primary.
func TestCanceledCallerLeavesTheFallbackCircuitClosed(t *testing.T) {
	primary, _, _ := newFixture(t, Config{MaxConcurrent: 4, MinDelay: time.Second})
	fallback := &countingLimiter{}

	var tried []string
	primary.OnWait = func(_, outcome string, _ time.Duration) { tried = append(tried, outcome) }
	var events []bool
	f := &httpx.FallbackLimiter{
		Primary:    primary,
		Fallback:   fallback,
		OnDegraded: func(down bool) { events = append(events, down) },
	}

	client := primary.cfg.Client
	for i := range 2 {
		ctx, cancel := context.WithCancel(context.Background())
		primary.cfg.Client = &failingEval{UniversalClient: client, cancel: cancel, err: context.Canceled}
		if _, err := f.Acquire(ctx, "crawl4ai", "https://example.com/a"); !errors.Is(err, context.Canceled) {
			t.Fatalf("acquire %d: err = %v, want context.Canceled", i, err)
		}
		cancel()
	}

	if len(tried) != 2 {
		t.Fatalf("the primary was consulted %d times (%v), want 2 — the circuit opened", len(tried), tried)
	}
	if fallback.count() != 0 {
		t.Fatalf("fallback calls = %d, want 0 — a ctx error must not fail over", fallback.count())
	}
	if len(events) != 0 {
		t.Fatalf("OnDegraded events = %v, want none", events)
	}
}

// Both keys of one host must share a Redis Cluster hash slot, or every
// multi-key EVALSHA is a CROSSSLOT error and pacing degrades to per-pod
// silently and permanently.
func TestKeysBraceTheHostForClusterSlots(t *testing.T) {
	l, _, _ := newFixture(t, Config{})

	win, next, _ := l.keys("news.ycombinator.com")
	if win != "omnifeed:ratelimit:test:{news.ycombinator.com}:win" {
		t.Fatalf("win key = %q", win)
	}
	if next != "omnifeed:ratelimit:test:{news.ycombinator.com}:next" {
		t.Fatalf("next key = %q", next)
	}
}

// A wait longer than the caller's remaining deadline is refused immediately
// instead of slept out: the caller would have burned its whole budget and
// failed anyway. Nothing is booked on the wait path, so Redis must be untouched
// by the refusal.
func TestAcquireFailsFastWhenWaitExceedsTheBudget(t *testing.T) {
	l, m, _ := newFixture(t, Config{MaxConcurrent: 4, Quota: 1, Window: time.Hour})
	const url = "https://example.com/a"
	win, _, _ := l.keys("example.com")

	release, err := l.Acquire(context.Background(), "crawl4ai", url)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	before, err := l.cfg.Client.ZCard(context.Background(), win).Result()
	if err != nil {
		t.Fatalf("ZCARD: %v", err)
	}

	var outcomes []string
	var waits []time.Duration
	l.OnWait = func(_, outcome string, waited time.Duration) {
		outcomes = append(outcomes, outcome)
		waits = append(waits, waited)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := l.Acquire(ctx, "crawl4ai", url); err == nil {
		t.Fatal("Acquire on a spent quota: want a WaitBudgetError, got nil")
	} else {
		var wbe *httpx.WaitBudgetError
		if !errors.As(err, &wbe) {
			t.Fatalf("Acquire err = %T (%v), want *httpx.WaitBudgetError", err, err)
		}
		if wbe.RetryAfter < 50*time.Minute {
			t.Fatalf("RetryAfter = %v, want ~1h (the next window opening)", wbe.RetryAfter)
		}
		if errors.Is(err, httpx.ErrLimiterUnavailable) {
			t.Fatal("a pacing verdict must not be reported as a backend failure")
		}
	}
	if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
		t.Fatalf("Acquire blocked for %v, want an immediate refusal", elapsed)
	}

	after, err := l.cfg.Client.ZCard(context.Background(), win).Result()
	if err != nil {
		t.Fatalf("ZCARD: %v", err)
	}
	if after != before {
		t.Fatalf("ZCARD %d → %d: the refused caller wrote to Redis", before, after)
	}
	if len(outcomes) != 1 || outcomes[0] != "budget_exceeded" {
		t.Fatalf("outcomes = %v, want [budget_exceeded]", outcomes)
	}
	if waits[0] > 40*time.Millisecond {
		t.Fatalf("budget_exceeded wait = %v, want ~0 (nothing was slept)", waits[0])
	}
	assertTTLs(t, m)
}
