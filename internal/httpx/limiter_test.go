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
