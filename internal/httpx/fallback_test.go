package httpx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// stubLimiter records its calls and returns whatever the test scripted. The
// release func it hands back is tagged with the stub's name, so a test can
// prove which backend admitted the request.
type stubLimiter struct {
	name string
	err  error // returned by every Acquire

	mu       sync.Mutex
	calls    int
	released int
}

func (s *stubLimiter) Acquire(_ context.Context, _, _ string) (func(), error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return func() {
		s.mu.Lock()
		s.released++
		s.mu.Unlock()
	}, nil
}

func (s *stubLimiter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubLimiter) releases() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.released
}

// A healthy primary serves everything; the fallback is never touched.
func TestFallbackLimiterDelegatesToPrimary(t *testing.T) {
	primary := &stubLimiter{name: "primary"}
	fallback := &stubLimiter{name: "fallback"}
	f := &FallbackLimiter{Primary: primary, Fallback: fallback}

	for range 3 {
		release, err := f.Acquire(context.Background(), "searxng", "http://searxng:8080/search")
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		release()
	}

	if primary.count() != 3 {
		t.Fatalf("primary calls = %d, want 3", primary.count())
	}
	if fallback.count() != 0 {
		t.Fatalf("fallback calls = %d, want 0", fallback.count())
	}
}

// ErrLimiterUnavailable must be absorbed: the acquire that hit the dead backend
// is itself served by the fallback, and the caller sees no error.
func TestFallbackLimiterDegradesOnBackendFailure(t *testing.T) {
	primary := &stubLimiter{name: "primary", err: fmt.Errorf("dial redis: %w", ErrLimiterUnavailable)}
	fallback := &stubLimiter{name: "fallback"}
	f := &FallbackLimiter{Primary: primary, Fallback: fallback}

	release, err := f.Acquire(context.Background(), "reddit", "https://www.reddit.com/")
	if err != nil {
		t.Fatalf("Acquire: %v, want the fallback to absorb it", err)
	}
	if release == nil {
		t.Fatal("Acquire returned a nil release")
	}
	if primary.count() != 1 || fallback.count() != 1 {
		t.Fatalf("calls: primary %d, fallback %d — want 1 and 1", primary.count(), fallback.count())
	}

	// The circuit is now open: later acquires skip the primary entirely.
	if _, err := f.Acquire(context.Background(), "reddit", "https://www.reddit.com/"); err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if primary.count() != 1 {
		t.Fatalf("primary calls = %d, want 1 (circuit open)", primary.count())
	}
	if fallback.count() != 2 {
		t.Fatalf("fallback calls = %d, want 2", fallback.count())
	}
}

// The release func must come from whichever backend admitted, so a degraded
// acquire releases the fallback's slot, not the primary's.
func TestFallbackLimiterReleaseRoutesToTheAdmittingBackend(t *testing.T) {
	primary := &stubLimiter{name: "primary"}
	fallback := &stubLimiter{name: "fallback"}
	f := &FallbackLimiter{Primary: primary, Fallback: fallback}

	release, err := f.Acquire(context.Background(), "searxng", "http://searxng:8080/search")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
	if primary.releases() != 1 || fallback.releases() != 0 {
		t.Fatalf("releases: primary %d, fallback %d — want 1 and 0", primary.releases(), fallback.releases())
	}

	primary.err = ErrLimiterUnavailable
	release, err = f.Acquire(context.Background(), "searxng", "http://searxng:8080/search")
	if err != nil {
		t.Fatalf("degraded Acquire: %v", err)
	}
	release()
	if primary.releases() != 1 || fallback.releases() != 1 {
		t.Fatalf("releases: primary %d, fallback %d — want 1 and 1", primary.releases(), fallback.releases())
	}
}

// A dead caller ctx is the caller's own timeout, not backend health: the error
// passes through untouched and the circuit stays closed.
func TestFallbackLimiterPassesContextErrorsThrough(t *testing.T) {
	for _, ctxErr := range []error{context.Canceled, context.DeadlineExceeded} {
		primary := &stubLimiter{name: "primary", err: ctxErr}
		fallback := &stubLimiter{name: "fallback"}
		var degraded int
		f := &FallbackLimiter{
			Primary:    primary,
			Fallback:   fallback,
			OnDegraded: func(bool) { degraded++ },
		}

		if _, err := f.Acquire(context.Background(), "reddit", "https://www.reddit.com/"); !errors.Is(err, ctxErr) {
			t.Fatalf("Acquire err = %v, want %v", err, ctxErr)
		}
		if fallback.count() != 0 {
			t.Fatalf("fallback calls = %d, want 0 — a ctx error must not fail over", fallback.count())
		}
		if degraded != 0 {
			t.Fatalf("OnDegraded fired %d times on a ctx error, want 0", degraded)
		}

		// Still healthy: the next acquire goes to the primary.
		primary.err = nil
		if _, err := f.Acquire(context.Background(), "reddit", "https://www.reddit.com/"); err != nil {
			t.Fatalf("Acquire after ctx error: %v", err)
		}
		if primary.count() != 2 {
			t.Fatalf("primary calls = %d, want 2 (circuit never opened)", primary.count())
		}
	}
}

// Within the cooldown the primary is not retried; after it, the next acquire
// probes it and recovery is reported.
func TestFallbackLimiterReprobesAfterTheCooldown(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	primary := &stubLimiter{name: "primary", err: ErrLimiterUnavailable}
	fallback := &stubLimiter{name: "fallback"}
	var events []bool
	f := &FallbackLimiter{
		Primary:    primary,
		Fallback:   fallback,
		Cooldown:   30 * time.Second,
		OnDegraded: func(down bool) { events = append(events, down) },
		now:        func() time.Time { return now },
	}

	if _, err := f.Acquire(context.Background(), "searxng", "http://searxng:8080/search"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	// Before the cooldown expires: fallback only.
	now = now.Add(29 * time.Second)
	if _, err := f.Acquire(context.Background(), "searxng", "http://searxng:8080/search"); err != nil {
		t.Fatalf("Acquire inside the cooldown: %v", err)
	}
	if primary.count() != 1 {
		t.Fatalf("primary calls = %d, want 1 inside the cooldown", primary.count())
	}

	// After it: the next acquire probes the primary, which is healthy again.
	now = now.Add(2 * time.Second)
	primary.err = nil
	if _, err := f.Acquire(context.Background(), "searxng", "http://searxng:8080/search"); err != nil {
		t.Fatalf("Acquire after the cooldown: %v", err)
	}
	if primary.count() != 2 {
		t.Fatalf("primary calls = %d, want 2 (probe)", primary.count())
	}
	if fallback.count() != 2 {
		t.Fatalf("fallback calls = %d, want 2", fallback.count())
	}
	if len(events) != 2 || events[0] != true || events[1] != false {
		t.Fatalf("OnDegraded events = %v, want [true false]", events)
	}
}

// OnDegraded is a transition hook, not a per-call one: many failed acquires
// report one degradation, and a failed probe does not report a second.
func TestFallbackLimiterOnDegradedFiresOnTransitionsOnly(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	primary := &stubLimiter{name: "primary", err: ErrLimiterUnavailable}
	fallback := &stubLimiter{name: "fallback"}
	var events []bool
	f := &FallbackLimiter{
		Primary:    primary,
		Fallback:   fallback,
		Cooldown:   time.Second,
		OnDegraded: func(down bool) { events = append(events, down) },
		now:        func() time.Time { return now },
	}

	// Five acquires while down, spanning two cooldowns (so the primary is
	// probed again and fails again) — still one single "down" event.
	for range 5 {
		if _, err := f.Acquire(context.Background(), "reddit", "https://www.reddit.com/"); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		now = now.Add(2 * time.Second)
	}
	if len(events) != 1 || !events[0] {
		t.Fatalf("OnDegraded events = %v, want a single [true]", events)
	}

	// Recovery reports once, and staying healthy reports nothing more.
	primary.err = nil
	for range 3 {
		if _, err := f.Acquire(context.Background(), "reddit", "https://www.reddit.com/"); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
	}
	if len(events) != 2 || events[1] {
		t.Fatalf("OnDegraded events = %v, want [true false]", events)
	}
}

// The circuit state is shared by every in-flight request, so it must be safe
// for concurrent Acquires. Run under -race.
func TestFallbackLimiterConcurrentAcquire(t *testing.T) {
	primary := &stubLimiter{name: "primary", err: ErrLimiterUnavailable}
	fallback := &stubLimiter{name: "fallback"}
	var degraded int
	var mu sync.Mutex
	f := &FallbackLimiter{
		Primary:  primary,
		Fallback: fallback,
		Cooldown: time.Millisecond,
		OnDegraded: func(bool) {
			mu.Lock()
			degraded++
			mu.Unlock()
		},
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := f.Acquire(context.Background(), "reddit", "https://www.reddit.com/")
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			release()
		}()
	}
	wg.Wait()

	if fallback.count() != 50 {
		t.Fatalf("fallback calls = %d, want 50 (every request served)", fallback.count())
	}
	mu.Lock()
	defer mu.Unlock()
	if degraded == 0 {
		t.Fatal("OnDegraded never fired")
	}
}
