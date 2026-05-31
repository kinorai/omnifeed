package httpx

import (
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
// last request to the same domain has elapsed. Caller must Release when done.
func (d *DomainLimiter) Acquire(rawURL string) func() {
	s := d.slotFor(rawURL)
	s.sem <- struct{}{}

	s.mu.Lock()
	since := time.Since(s.lastSend)
	if since < d.minDelay {
		wait := d.minDelay - since + time.Duration(rand.Intn(500))*time.Millisecond
		s.mu.Unlock()
		time.Sleep(wait)
	} else {
		s.mu.Unlock()
	}

	return func() {
		s.mu.Lock()
		s.lastSend = time.Now()
		s.mu.Unlock()
		<-s.sem
	}
}
