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
// booked, so no cleanup path exists to get wrong.
func TestAcquireCancelBooksNothing(t *testing.T) {
	l, m, _ := newFixture(t, Config{MaxConcurrent: 4, Quota: 1, Window: time.Hour})
	const url = "https://example.com/a"
	win, _ := l.keys("example.com")

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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx, "crawl4ai", url); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire on a spent quota: err = %v, want a deadline error", err)
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
	_, nextKey := l.keys("news.ycombinator.com")

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
