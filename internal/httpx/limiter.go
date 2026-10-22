package httpx

import (
	"context"
	"math/rand"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LimiterHost is the key a URL is paced under: its hostname, lowercased,
// without a trailing dot or port. Example.COM., example.com:443 and
// example.com are one host to the upstream, so they share one slot; a caller
// that varied the spelling would otherwise get a fresh budget each time. A
// malformed URL yields the empty host, which paces all such callers together
// rather than not at all.
func LimiterHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

// slotSweepEvery is how often Acquire looks for idle per-host slots to drop,
// and slotIdleFloor the least time a slot must sit unused before it goes.
// Without the sweep, every host a caller ever named keeps a slot for the life
// of the process.
const (
	slotSweepEvery = time.Minute
	slotIdleFloor  = 5 * time.Minute
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
	spaceSends    bool          // measure the delay from admission, not completion
	limits        sync.Map      // host → *domainSlot
	lastSweep     atomic.Int64  // unix nanos of the last idle-slot sweep

	// OnWait, when non-nil, is called on every Acquire exit with the engine
	// that waited, the outcome, and the time spent blocked (semaphore wait +
	// politeness delay); ~0 when uncontended. Set once at wiring time.
	//
	// Three outcomes: "acquired"; "canceled" when the caller's ctx died in the
	// queue — the worst waits, which never acquire; and "budget_exceeded" when
	// the computed wait was longer than the caller's remaining deadline, so
	// Acquire refused to queue at all (see WaitBudgetError). The last one is
	// the cheap failure: it costs ~0 seconds, not the caller's whole budget.
	OnWait func(engine, outcome string, waited time.Duration)
}

type domainSlot struct {
	sem chan struct{}
	mu  sync.Mutex
	// lastSend is the completion instant of the previous request. Read only in
	// completion-spaced mode (spaceSends false), where the delay is measured
	// from it.
	lastSend time.Time
	// booked holds the send instants reserve has handed out and that can still
	// constrain a new caller, oldest first. Read only in send-spaced mode
	// (spaceSends true), which is the mode that lets minDelay and a concurrency
	// cap above 1 compose: the next caller spaces itself from the last entry
	// rather than from a completion. A set rather than a high-water mark so a
	// caller that gives up can withdraw its own instant whatever order the
	// cancellations land in — see unbook.
	booked     []time.Time
	spaceSends bool
	// notBefore holds this domain back until an instant an upstream itself
	// asked for (a Retry-After on a 429/503, applied via Penalize). Zero when
	// no upstream has asked, which is the only state Acquire ever sees unless
	// Penalize is called.
	notBefore time.Time
	window    quotaWindow
	// refs counts the callers holding this slot, from slotFor until done;
	// lastUsed is when the last of them let go. The sweep drops only a slot
	// with no refs whose state can no longer delay anyone, and marks it dead
	// so a caller that loaded it just before retries on a fresh one.
	refs     int
	lastUsed time.Time
	dead     bool
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

// NewSpacedDomainQuotaLimiter returns a limiter that spaces SENDS instead of
// gaps: minDelay is measured from the previous caller's admission rather than
// from its completion, so N callers are admitted minDelay apart and up to
// maxConcurrent of them run at once.
//
// The distinction only matters above maxConcurrent 1, and there it is the
// difference between working and not. NewDomainQuotaLimiter measures from
// lastSend, which is written on Release, so on an idle slot every waiting
// goroutine reads the same stale instant, computes wait 0 and sends together —
// maxConcurrent 8 emits an 8-wide burst rather than eight sends 8×minDelay
// apart. That is why every search limiter built on the completion-spaced
// constructor passes maxConcurrent 1.
//
// The crawl limiter is the exception and does NOT: it is built completion-spaced
// with OMNIFEED_PER_DOMAIN_CONCURRENCY (default 2) and a 1500ms delay, so two
// requests can reach a cold domain at once despite that gap. Left alone on
// purpose — the gap it protects was measured against the completion-spaced
// shape, and moving it is a change to crawl politeness that needs its own
// measurement, not a side effect of this one.
//
// This mirrors redislimit's ClusterConcurrency > 0 mode, one process instead of
// a cluster. Prefer it for an upstream that counts requests in a window (search
// engines do); prefer the completion-spaced limiter for one that punishes short
// gaps, since serialized spacing is the politer shape.
func NewSpacedDomainQuotaLimiter(maxConcurrent int, minDelay time.Duration, quota int, window time.Duration) *DomainLimiter {
	l := NewDomainQuotaLimiter(maxConcurrent, minDelay, quota, window)
	l.spaceSends = true
	return l
}

// slotFor returns the slot for rawURL's host with a reference held, so the
// idle sweep cannot drop it while in use. Callers release it with done.
func (d *DomainLimiter) slotFor(rawURL string) *domainSlot {
	d.maybeSweep(time.Now())
	host := LimiterHost(rawURL)
	for {
		val, ok := d.limits.Load(host)
		if !ok {
			val, _ = d.limits.LoadOrStore(host, &domainSlot{
				sem:        make(chan struct{}, d.maxConcurrent),
				window:     quotaWindow{limit: d.quota, period: d.quotaWindow},
				spaceSends: d.spaceSends,
			})
		}
		s := val.(*domainSlot)
		s.mu.Lock()
		if s.dead {
			s.mu.Unlock()
			continue
		}
		s.refs++
		s.mu.Unlock()
		return s
	}
}

// done drops a reference slotFor took. Callers must hold s.mu.
func (s *domainSlot) done() {
	s.refs--
	s.lastUsed = time.Now()
}

// maybeSweep runs sweep at most once per slotSweepEvery, on whichever caller
// gets there first.
func (d *DomainLimiter) maybeSweep(now time.Time) {
	last := d.lastSweep.Load()
	if now.UnixNano()-last < int64(slotSweepEvery) || !d.lastSweep.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	d.sweep(now)
}

// sweep drops every slot that no caller holds and whose pacing state can no
// longer delay a new caller: its last use is older than the minimum delay and
// the quota window, and any upstream hold has passed. A fresh slot then
// behaves exactly like the dropped one would have.
func (d *DomainLimiter) sweep(now time.Time) {
	idle := max(slotIdleFloor, d.minDelay, d.quotaWindow)
	d.limits.Range(func(key, val any) bool {
		s := val.(*domainSlot)
		s.mu.Lock()
		if s.refs == 0 && now.Sub(s.lastUsed) > idle && now.After(s.notBefore) {
			s.dead = true
			d.limits.CompareAndDelete(key, s)
		}
		s.mu.Unlock()
		return true
	})
}

// Acquire blocks until a slot is available and the minimum delay since the
// last request to the same domain has elapsed, or until ctx is done — a
// canceled caller must not keep queuing behind a slow domain. When ctx carries
// a deadline and the pacing wait is longer than what is left of it, Acquire
// returns *WaitBudgetError immediately instead of queueing (fail fast, the
// wait is reported so the caller can retry later). engine names the
// caller for the wait metric (the limiter itself only knows hosts). Caller must
// Release (call the returned func) when done; on error there is nothing to
// release.
func (d *DomainLimiter) Acquire(ctx context.Context, engine, rawURL string) (func(), error) {
	start := time.Now()
	s := d.slotFor(rawURL)
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		s.mu.Lock()
		s.done()
		s.mu.Unlock()
		d.observeWait(engine, "canceled", start)
		return nil, ctx.Err()
	}

	s.mu.Lock()
	wait, admitAt := s.reserve(time.Now(), d.minDelay)
	s.mu.Unlock()
	// Fail fast when the wait cannot fit in the caller's budget: queueing for a
	// slot that opens after the deadline burns the whole budget and then fails
	// anyway. Hand the booking back, and report the wait so the caller can say
	// when to retry. No deadline means no budget to exceed, so such a caller
	// queues as before.
	if budget, hasBudget := RemainingBudget(ctx); hasBudget && wait > budget {
		s.mu.Lock()
		s.unbook(admitAt, d.minDelay)
		s.done()
		s.mu.Unlock()
		<-s.sem
		d.observeWait(engine, "budget_exceeded", start)
		return nil, &WaitBudgetError{RetryAfter: wait}
	}
	if wait > 0 {
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			// Give the booking back: a caller that never sent must not consume
			// quota, or repeated cancellations starve the domain.
			s.mu.Lock()
			s.unbook(admitAt, d.minDelay)
			s.done()
			s.mu.Unlock()
			<-s.sem
			d.observeWait(engine, "canceled", start)
			return nil, ctx.Err()
		}
	}

	d.observeWait(engine, "acquired", start)
	return func() {
		s.mu.Lock()
		// In send-spaced mode the next admission was already booked from this
		// one's, so completion says nothing about when the next caller may go.
		// Writing lastSend here would leave a plausible but unread instant
		// behind for the next reader of this struct.
		if !s.spaceSends {
			s.lastSend = time.Now()
		}
		s.done()
		s.mu.Unlock()
		<-s.sem
	}, nil
}

// Penalize holds the domain back for d, on top of whatever the pacing settings
// already impose — what an upstream's Retry-After on a 429 or 503 is worth. It
// only ever extends an existing hold, so the strictest answer any upstream gave
// stands, and it never shortens one. A non-positive d does nothing.
//
// Nothing is released here: this is a note left for the next Acquire, not an
// admission.
func (d *DomainLimiter) Penalize(rawURL string, dur time.Duration) {
	if dur <= 0 {
		return
	}
	s := d.slotFor(rawURL)
	until := time.Now().Add(dur)
	s.mu.Lock()
	if until.After(s.notBefore) {
		s.notBefore = until
	}
	s.done()
	s.mu.Unlock()
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
// that delay would let us send. In send-spaced mode the delay is measured from
// the previous caller's booked admission (nextSend) instead, which is what lets
// a concurrency cap above 1 coexist with a nonzero delay — see
// NewSpacedDomainQuotaLimiter.
//
// Jitter is applied last, after the booking, and ONLY in completion-spaced
// mode. Booked instants must stay monotonic across concurrent callers — both
// evict (a prefix scan that stops at the first live entry) and the
// sent[n-limit] lookup assume oldest-first — and jittering before the booking
// lets two callers a microsecond apart book descending instants, which would
// let the window admit more than its limit. Sending marginally later than
// booked only ever under-uses the window, which is the safe direction for a
// politeness control.
//
// Send-spaced mode takes no jitter at all, and must not. Jitter exists so
// goroutines released together do not re-synchronize on the next send, which is
// a hazard only when they were all admitted at once — exactly what this mode
// removes by construction, since it staggers admissions itself. Adding it here
// would also make sends land CLOSER than minDelay rather than further apart:
// the spacing is measured between booked instants, so a caller that draws 480ms
// followed by one that draws 10ms sends 530ms after it, half the configured
// gap, against upstreams that answer an overshoot with a 20-minute suspension.
func (s *domainSlot) reserve(now time.Time, minDelay time.Duration) (time.Duration, time.Time) {
	var wait time.Duration
	if s.spaceSends {
		wait = s.spacedFrom(now, minDelay).Sub(now)
	} else if since := now.Sub(s.lastSend); since < minDelay {
		wait = minDelay - since
	}
	// An upstream's own Retry-After and our politeness delay are two lower
	// bounds on the same instant, so the later one wins.
	if hold := s.notBefore.Sub(now); hold > wait {
		wait = hold
	}
	booked := now.Add(wait)
	bookWait := s.window.book(booked)
	booked = booked.Add(bookWait)
	wait += bookWait
	if s.spaceSends {
		// Record the instant this caller will actually send, quota wait
		// included, so a full window spaces the callers behind it too.
		s.booked = append(s.booked, booked)
		return wait, booked
	}

	if wait > 0 {
		// Scheduling noise, not a secret. Nothing downstream is authenticated,
		// keyed or made unguessable by it, and an attacker who could predict it
		// would learn only when a politeness delay ends. crypto/rand would be
		// slower, can fail, and would buy nothing.
		// #nosec G404 -- non-cryptographic timing jitter
		wait += time.Duration(rand.Intn(500)) * time.Millisecond
	}
	return wait, booked
}

// spacedFrom returns the earliest instant a new caller may send: minDelay past
// the latest booking that still constrains it, or `now` when none does. It
// prunes the bookings that no longer can, which is what keeps the set bounded
// by the callers actually in flight. Callers must hold s.mu.
//
// The cutoff is now-minDelay, not now: a caller that sent a moment ago still
// owes the next one its gap, and dropping it would admit an immediate send.
func (s *domainSlot) spacedFrom(now time.Time, minDelay time.Duration) time.Time {
	cutoff := now.Add(-minDelay)
	i := 0
	for i < len(s.booked) && !s.booked[i].After(cutoff) {
		i++
	}
	s.booked = s.booked[i:]
	if len(s.booked) == 0 {
		return now
	}
	// Bookings are appended in order, so the last one is the latest.
	return s.booked[len(s.booked)-1].Add(minDelay)
}

// unbook withdraws the reservation reserve made at `booked`, for a caller that
// gave up before sending. Callers must hold s.mu.
//
// Withdrawing by value, not by rewinding a high-water mark, because the shape
// this has to survive is a fanned-out client whose shared ctx dies: every
// booking is abandoned at once and in arbitrary order. A mark can only be wound
// back by whoever booked last, so all but one of those withdrawals would be
// lost and the slot would hold a phantom wait of up to maxConcurrent × minDelay
// with nothing in flight — the next search would queue behind callers that no
// longer exist, or be refused outright on its wait budget.
func (s *domainSlot) unbook(booked time.Time, minDelay time.Duration) {
	s.window.forget(booked)
	if !s.spaceSends {
		return
	}
	for i, at := range s.booked {
		if at.Equal(booked) {
			s.booked = append(s.booked[:i], s.booked[i+1:]...)
			return
		}
	}
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
