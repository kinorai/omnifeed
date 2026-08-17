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
// jitter so bursts don't synchronize across goroutines. It can additionally
// cap how many requests are admitted within a rolling window (see
// NewDomainQuotaLimiter).
type DomainLimiter struct {
	maxConcurrent int
	minDelay      time.Duration
	quota         int           // 0 disables the rolling-window cap
	quotaWindow   time.Duration // width of that window
	limits        sync.Map      // host → *domainSlot

	// OnWait, when non-nil, is called on every Acquire exit with the engine
	// that waited, the outcome ("acquired", or "canceled" when the caller's
	// ctx died in the queue — the worst waits, which never acquire), and the
	// time spent blocked (semaphore wait + politeness delay); ~0 when
	// uncontended. Set once at wiring time.
	OnWait func(engine, outcome string, waited time.Duration)
}

type domainSlot struct {
	sem      chan struct{}
	mu       sync.Mutex
	lastSend time.Time
	window   quotaWindow
}

// NewDomainLimiter returns a limiter with the given concurrency cap and
// per-domain minimum delay, and no rolling-window cap.
func NewDomainLimiter(maxConcurrent int, minDelay time.Duration) *DomainLimiter {
	return NewDomainQuotaLimiter(maxConcurrent, minDelay, 0, 0)
}

// NewDomainQuotaLimiter returns a limiter that also admits at most `quota`
// requests per domain within any rolling `window`. quota <= 0 disables that
// cap, making this identical to NewDomainLimiter.
//
// A minimum delay alone cannot express the limit some upstreams actually
// enforce. Measured against this deployment's search pool on 2026-08-17: one
// engine kept answering at a 3s spacing from a quiet start, yet blocked after
// ~20 requests in ~85s — it counts requests in a window, not the gap between
// them. A 3s delay run continuously sends 30 requests per 90s and trips it, so
// the two controls are complementary: the delay shapes the gap, the quota
// bounds the burst.
func NewDomainQuotaLimiter(maxConcurrent int, minDelay time.Duration, quota int, window time.Duration) *DomainLimiter {
	return &DomainLimiter{
		maxConcurrent: maxConcurrent,
		minDelay:      minDelay,
		quota:         quota,
		quotaWindow:   window,
	}
}

func (d *DomainLimiter) slotFor(rawURL string) *domainSlot {
	u, _ := url.Parse(rawURL)
	host := u.Hostname()
	val, _ := d.limits.LoadOrStore(host, &domainSlot{
		sem:    make(chan struct{}, d.maxConcurrent),
		window: quotaWindow{limit: d.quota, period: d.quotaWindow},
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
		d.observeWait(engine, "canceled", start)
		return nil, ctx.Err()
	}

	s.mu.Lock()
	wait, admitAt := s.reserve(time.Now(), d.minDelay)
	s.mu.Unlock()
	if wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			// Give the window slot back: a caller that never sent must not
			// consume quota, or repeated cancellations starve the domain.
			s.mu.Lock()
			s.window.forget(admitAt)
			s.mu.Unlock()
			<-s.sem
			d.observeWait(engine, "canceled", start)
			return nil, ctx.Err()
		}
	}

	d.observeWait(engine, "acquired", start)
	return func() {
		s.mu.Lock()
		s.lastSend = time.Now()
		s.mu.Unlock()
		<-s.sem
	}, nil
}

func (d *DomainLimiter) observeWait(engine, outcome string, start time.Time) {
	if d.OnWait != nil {
		d.OnWait(engine, outcome, time.Since(start))
	}
}

// reserve computes how long this caller must wait before it may send, and
// books the resulting admission instant in the rolling window. It returns that
// wait and the BOOKED instant, so a caller whose ctx dies in the wait can hand
// the exact slot back. Callers must hold s.mu.
//
// The two controls compose: the minimum delay is measured from the previous
// request's completion (lastSend), then the window is consulted at the instant
// that delay would let us send.
//
// Jitter is applied last, after the booking. Booked instants must stay
// monotonic across concurrent callers — both evict (a prefix scan that stops at
// the first live entry) and the sent[n-limit] lookup assume oldest-first — and
// jittering before the booking lets two callers a microsecond apart book
// descending instants, which would let the window admit more than its limit.
// Sending marginally later than booked only ever under-uses the window, which
// is the safe direction for a politeness control.
func (s *domainSlot) reserve(now time.Time, minDelay time.Duration) (time.Duration, time.Time) {
	var wait time.Duration
	if since := now.Sub(s.lastSend); since < minDelay {
		wait = minDelay - since
	}
	booked := now.Add(wait)
	bookWait := s.window.book(booked)
	booked = booked.Add(bookWait)
	wait += bookWait

	if wait > 0 {
		// Scheduling noise, not a secret: the jitter exists so goroutines that
		// were released together do not re-synchronize on the next send. Nothing
		// downstream is authenticated, keyed or made unguessable by it, and an
		// attacker who could predict it would learn only when a politeness delay
		// ends. crypto/rand would be slower, can fail, and would buy nothing.
		// #nosec G404 -- non-cryptographic timing jitter
		wait += time.Duration(rand.Intn(500)) * time.Millisecond
	}
	return wait, booked
}

// quotaWindow admits at most limit requests in any rolling period. It keeps the
// admission instants of the requests still inside the window — at most `limit`
// timestamps — rather than refilling a token bucket, because the constraint it
// models is literally "no more than N requests per period": upstreams that
// impose it count arrivals in a window. A bucket would allow a full burst and
// then a trickle, which is the same average but a different, more bot-like
// shape. The zero value admits everything.
type quotaWindow struct {
	limit  int
	period time.Duration
	sent   []time.Time // booked admissions inside the window, oldest first
}

// book records an admission at `at` and returns the extra wait needed to keep
// the window under its limit. Bookings are made at the intended send instant,
// so concurrent callers queue behind each other instead of all computing the
// same free slot.
func (q *quotaWindow) book(at time.Time) time.Duration {
	if q.limit <= 0 {
		return 0
	}
	q.evict(at)

	var wait time.Duration
	if n := len(q.sent); n >= q.limit {
		// The window is full. The opening this caller needs is not the oldest
		// booking but the one `limit` places back: bookings already queued into
		// the future each hold a slot of their own, so waiting for sent[0] alone
		// would let every queued caller pile onto the same instant.
		if free := q.sent[n-q.limit].Add(q.period).Sub(at); free > 0 {
			wait = free
		}
	}
	q.sent = append(q.sent, at.Add(wait))
	return wait
}

// forget drops the booking made for `at`, for a caller that gave up before
// sending.
func (q *quotaWindow) forget(at time.Time) {
	for i, booked := range q.sent {
		if booked.Equal(at) {
			q.sent = append(q.sent[:i], q.sent[i+1:]...)
			return
		}
	}
}

// evict drops bookings that have left the window as of `now`.
func (q *quotaWindow) evict(now time.Time) {
	cutoff := now.Add(-q.period)
	i := 0
	for i < len(q.sent) && !q.sent[i].After(cutoff) {
		i++
	}
	q.sent = q.sent[i:]
}
