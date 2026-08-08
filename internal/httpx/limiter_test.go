package httpx

import (
	"context"
	"testing"
	"time"
)

// OnWait fires on every successful Acquire with the caller's engine name and
// the time spent blocked — ~0 when uncontended, and at least the politeness
// delay when a second request follows the first on the same domain.
func TestDomainLimiterReportsWait(t *testing.T) {
	const minDelay = 20 * time.Millisecond

	var engines []string
	var waits []time.Duration
	d := NewDomainLimiter(1, minDelay)
	d.OnWait = func(engine string, waited time.Duration) {
		engines = append(engines, engine)
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
	if waits[0] > minDelay {
		t.Fatalf("uncontended wait = %v, want < %v", waits[0], minDelay)
	}
	// The exact wait is minDelay minus the (tiny) gap since release, plus random
	// jitter — assert against half the delay to stay robust.
	if waits[1] < minDelay/2 {
		t.Fatalf("contended wait = %v, want >= %v (politeness delay)", waits[1], minDelay/2)
	}
}
