package httpx

import (
	"context"
	"testing"
	"time"
)

// OnWait fires on every Acquire exit with the caller's engine name, the
// outcome, and the time spent blocked — ~0 when uncontended, and at least the
// politeness delay when a second request follows the first on the same domain.
func TestDomainLimiterReportsWait(t *testing.T) {
	const minDelay = 20 * time.Millisecond

	var engines, outcomes []string
	var waits []time.Duration
	d := NewDomainLimiter(1, minDelay)
	d.OnWait = func(engine, outcome string, waited time.Duration) {
		engines = append(engines, engine)
		outcomes = append(outcomes, outcome)
		waits = append(waits, waited)
	}

	release, err := d.Acquire(context.Background(), "reddit", "https://www.reddit.com/")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	// Second acquire on the same domain must sit out the politeness delay.
	release, err = d.Acquire(context.Background(), "reddit", "https://www.reddit.com/r/golang")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	if len(engines) != 2 || engines[0] != "reddit" || engines[1] != "reddit" {
		t.Fatalf("engines = %v, want two 'reddit' observations", engines)
	}
	if outcomes[0] != "acquired" || outcomes[1] != "acquired" {
		t.Fatalf("outcomes = %v, want two 'acquired'", outcomes)
	}
	if waits[0] > minDelay {
		t.Fatalf("uncontended wait = %v, want < %v", waits[0], minDelay)
	}
	// The exact wait is minDelay minus the (tiny) gap since release, plus random
	// jitter — assert against half the delay to stay robust.
	if waits[1] < minDelay/2 {
		t.Fatalf("contended wait = %v, want >= %v (politeness delay)", waits[1], minDelay/2)
	}
}

// A wait that dies in the queue (caller ctx canceled while blocked on the
// semaphore) must still be observed, as outcome="canceled" — those are the
// worst waits, and success-only observation would hide them entirely.
func TestDomainLimiterReportsCanceledWait(t *testing.T) {
	var outcomes []string
	d := NewDomainLimiter(1, 0)
	d.OnWait = func(_, outcome string, _ time.Duration) {
		outcomes = append(outcomes, outcome)
	}

	release, err := d.Acquire(context.Background(), "reddit", "https://www.reddit.com/")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := d.Acquire(ctx, "reddit", "https://www.reddit.com/r/golang")
		done <- err
	}()
	time.Sleep(10 * time.Millisecond) // let the goroutine queue on the held slot
	cancel()
	if err := <-done; err == nil {
		t.Fatal("Acquire on canceled ctx: want error, got nil")
	}
	release()

	if len(outcomes) != 2 || outcomes[0] != "acquired" || outcomes[1] != "canceled" {
		t.Fatalf("outcomes = %v, want [acquired canceled]", outcomes)
	}
}

// The rolling window is the control that bounds a burst. Driving quotaWindow
// directly keeps these deterministic: Acquire's real waits would make a
// window-sized test sleep for the whole period.
func TestQuotaWindowBoundsTheBurst(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	q := quotaWindow{limit: 3, period: 90 * time.Second}

	// The first `limit` requests go immediately.
	for i := range 3 {
		if wait := q.book(base.Add(time.Duration(i) * time.Second)); wait != 0 {
			t.Fatalf("request %d: wait = %v, want 0", i, wait)
		}
	}

	// The 4th must wait for the 1st to leave the window: booked at base+0, so
	// it frees at base+90, and we are asking at base+3.
	if got, want := q.book(base.Add(3*time.Second)), 87*time.Second; got != want {
		t.Fatalf("4th request: wait = %v, want %v", got, want)
	}
	// The 5th waits for the 2nd (base+1 → frees at base+91), NOT for the 1st
	// again — the 4th already holds that slot.
	if got, want := q.book(base.Add(3*time.Second)), 88*time.Second; got != want {
		t.Fatalf("5th request: wait = %v, want %v", got, want)
	}
	// The 6th waits for the 3rd (base+2 → frees at base+92).
	if got, want := q.book(base.Add(3*time.Second)), 89*time.Second; got != want {
		t.Fatalf("6th request: wait = %v, want %v", got, want)
	}
}

// Once the window has rolled past, capacity comes back in full.
func TestQuotaWindowRecoversAfterThePeriod(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	q := quotaWindow{limit: 2, period: 90 * time.Second}

	q.book(base)
	q.book(base.Add(time.Second))
	if wait := q.book(base.Add(91 * time.Second)); wait != 0 {
		t.Fatalf("after the period: wait = %v, want 0", wait)
	}
	// Only the two bookings still inside the window are retained.
	if len(q.sent) > 2 {
		t.Fatalf("retained %d bookings, want <= 2 (evicted)", len(q.sent))
	}
}

// A caller that gives up must hand its slot back, or repeated cancellations
// starve the domain for a whole period.
func TestQuotaWindowForgetReleasesTheSlot(t *testing.T) {
	base := time.Unix(1_000_000, 0)
	q := quotaWindow{limit: 1, period: 90 * time.Second}

	q.book(base)
	wait := q.book(base) // queued behind the first
	if wait == 0 {
		t.Fatal("second booking: want a wait, got 0")
	}
	q.forget(base.Add(wait))

	if got := q.book(base); got != 90*time.Second {
		t.Fatalf("after forget: wait = %v, want the first booking's slot only (90s)", got)
	}
}

// The zero value must admit everything, so every existing caller of
// NewDomainLimiter is unaffected.
func TestQuotaWindowZeroValueAdmitsEverything(t *testing.T) {
	var q quotaWindow
	for i := range 100 {
		if wait := q.book(time.Unix(int64(i), 0)); wait != 0 {
			t.Fatalf("request %d: wait = %v, want 0", i, wait)
		}
	}
}

// End-to-end through Acquire: with a quota of 1 the second caller is made to
// wait, and a canceled wait does not consume the quota.
func TestDomainQuotaLimiterDelaysBeyondTheQuota(t *testing.T) {
	d := NewDomainQuotaLimiter(4, 0, 1, time.Hour)

	release, err := d.Acquire(context.Background(), "searxng", "http://searxng:8080/search")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	// The quota is spent, so the next caller must block until its ctx dies.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := d.Acquire(ctx, "searxng", "http://searxng:8080/search"); err == nil {
		t.Fatal("second Acquire: want a deadline error, got nil")
	}
}

// Booked instants must stay ordered oldest-first: both evict (a prefix scan)
// and the sent[n-limit] lookup depend on it. Jitter is therefore applied after
// the booking, never before — jittering first lets two callers a microsecond
// apart book descending instants, and the window then admits more than its
// limit. This drives reserve directly because the defect needs two bookings
// whose jitter happens to invert them.
func TestReserveKeepsBookingsOrdered(t *testing.T) {
	s := &domainSlot{window: quotaWindow{limit: 10, period: time.Minute}}
	now := time.Unix(1_000_000, 0)

	// Same instant, minimum delay in play: pre-jitter booking would scatter
	// these across a 500ms range in arbitrary order.
	for range 10 {
		s.reserve(now, time.Second)
	}

	for i := 1; i < len(s.window.sent); i++ {
		if s.window.sent[i].Before(s.window.sent[i-1]) {
			t.Fatalf("booking %d (%v) precedes %d (%v): the window is unordered",
				i, s.window.sent[i], i-1, s.window.sent[i-1])
		}
	}
}

// The instant handed back for cancellation must be the BOOKED one, not the
// jittered send time, or forget cannot find the slot and it leaks.
func TestReserveReturnsTheBookedInstant(t *testing.T) {
	s := &domainSlot{window: quotaWindow{limit: 1, period: time.Minute}}
	now := time.Unix(1_000_000, 0)

	_, booked := s.reserve(now, 0)
	if len(s.window.sent) != 1 || !s.window.sent[0].Equal(booked) {
		t.Fatalf("returned %v, window holds %v — forget would not match", booked, s.window.sent)
	}

	s.window.forget(booked)
	if len(s.window.sent) != 0 {
		t.Fatalf("forget left %d bookings, want 0", len(s.window.sent))
	}
}
