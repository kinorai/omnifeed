package httpx

import (
	"context"
	"math/rand"
	"net/url"
	"sync"
	"time"
)

// DomainLimiter caps concurrency and enforces a minimum delay between
// successive requests to the same domain. The delay carries a small random
// jitter so bursts don't synchronize across goroutines.
type DomainLimiter struct {
	maxConcurrent int
	minDelay      time.Duration
	limits        sync.Map // host → *domainSlot

	// OnWait, when non-nil, is called on every successful Acquire with the
	// engine that waited and the time it spent blocked (semaphore wait +
	// politeness delay); ~0 when uncontended. Set once at wiring time.
	OnWait func(engine string, waited time.Duration)
}

type domainSlot struct {
	sem      chan struct{}
	mu       sync.Mutex
	lastSend time.Time
}

// NewDomainLimiter returns a limiter with the given concurrency cap and
// per-domain minimum delay.
func NewDomainLimiter(maxConcurrent int, minDelay time.Duration) *DomainLimiter {
	return &DomainLimiter{maxConcurrent: maxConcurrent, minDelay: minDelay}
}

func (d *DomainLimiter) slotFor(rawURL string) *domainSlot {
	u, _ := url.Parse(rawURL)
	host := u.Hostname()
	val, _ := d.limits.LoadOrStore(host, &domainSlot{
		sem: make(chan struct{}, d.maxConcurrent),
	})
	return val.(*domainSlot)
}

// Acquire blocks until a slot is available and the minimum delay since the
// last request to the same domain has elapsed, or until ctx is done — a
// canceled caller must not keep queuing behind a slow domain. engine names the
// caller for the wait metric (the limiter itself only knows hosts). Caller must
// Release (call the returned func) when done; on error there is nothing to
// release.
func (d *DomainLimiter) Acquire(ctx context.Context, engine, rawURL string) (func(), error) {
	start := time.Now()
	s := d.slotFor(rawURL)
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	s.mu.Lock()
	since := time.Since(s.lastSend)
	s.mu.Unlock()
	if since < d.minDelay {
		wait := d.minDelay - since + time.Duration(rand.Intn(500))*time.Millisecond
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			<-s.sem
			return nil, ctx.Err()
		}
	}

	if d.OnWait != nil {
		d.OnWait(engine, time.Since(start))
	}
	return func() {
		s.mu.Lock()
		s.lastSend = time.Now()
		s.mu.Unlock()
		<-s.sem
	}, nil
}
