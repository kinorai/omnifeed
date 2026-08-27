package httpx

import (
	"context"
	"errors"
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

// A Retry-After penalty must outlive the request that received it: the next
// reserve for that host waits until the penalized instant, even though the
// politeness delay alone would have admitted it at once.
func TestReserveHonorsNotBefore(t *testing.T) {
	s := &domainSlot{}
	now := time.Unix(1_000_000, 0)

	// First request: nothing holds it back.
	if wait, _ := s.reserve(now, 0); wait != 0 {
		t.Fatalf("first reserve waited %v, want 0", wait)
	}

	s.notBefore = now.Add(30 * time.Second)
	wait, _ := s.reserve(now, 0)
	if wait < 30*time.Second || wait > 30*time.Second+500*time.Millisecond {
		t.Fatalf("second reserve waited %v, want ~30s (the penalty, plus jitter)", wait)
	}
}

// The penalty and the minimum delay are two lower bounds on the same instant,
// so the later one wins — in both directions.
func TestReserveComposesPenaltyWithMinDelay(t *testing.T) {
	now := time.Unix(1_000_000, 0)

	// Penalty longer than the delay: the penalty decides.
	s := &domainSlot{lastSend: now, notBefore: now.Add(10 * time.Second)}
	if wait, _ := s.reserve(now, 2*time.Second); wait < 10*time.Second {
		t.Fatalf("penalty 10s vs delay 2s: waited %v, want >= 10s", wait)
	}

	// Delay longer than the penalty: the delay decides.
	s = &domainSlot{lastSend: now, notBefore: now.Add(time.Second)}
	if wait, _ := s.reserve(now, 8*time.Second); wait < 8*time.Second {
		t.Fatalf("penalty 1s vs delay 8s: waited %v, want >= 8s", wait)
	}
}

// Penalize keeps the strictest hold any upstream asked for, and never shortens
// one — two 429s in a row must not let the second, milder one release the host.
func TestPenalizeOnlyExtendsTheHold(t *testing.T) {
	d := NewDomainLimiter(1, 0)
	const rawURL = "https://example.com/a"

	d.Penalize(rawURL, time.Minute)
	first := d.slotFor(rawURL).notBefore

	d.Penalize(rawURL, time.Second)
	if got := d.slotFor(rawURL).notBefore; !got.Equal(first) {
		t.Fatalf("notBefore moved from %v to %v: a milder penalty shortened the hold", first, got)
	}

	d.Penalize(rawURL, 0)
	if got := d.slotFor(rawURL).notBefore; !got.Equal(first) {
		t.Fatalf("notBefore moved to %v on a zero penalty", got)
	}
}

// Acquire is unchanged when Penalize is never called: no hold, no extra wait.
func TestAcquireUnaffectedWithoutPenalty(t *testing.T) {
	d := NewDomainLimiter(1, 0)
	release, err := d.Acquire(context.Background(), "crawl4ai", "https://example.com/a")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
}

// A wait longer than the caller's remaining deadline must be refused
// immediately: queueing for a slot that opens after the deadline costs the
// caller its whole budget and fails anyway. The refusal reports the wait, and
// the booking it made is handed back — a request that never sent must not
// consume the window's quota.
func TestAcquireFailsFastWhenWaitExceedsTheBudget(t *testing.T) {
	const url = "https://www.reddit.com/"
	var outcomes []string
	var waits []time.Duration
	d := NewDomainQuotaLimiter(1, 0, 2, time.Hour)
	d.OnWait = func(_, outcome string, waited time.Duration) {
		outcomes = append(outcomes, outcome)
		waits = append(waits, waited)
	}

	// Spend the whole window: the next admission is an hour out.
	for range 2 {
		release, err := d.Acquire(context.Background(), "reddit", url)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		release()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	release, err := d.Acquire(ctx, "reddit", url)
	if err == nil {
		release()
		t.Fatal("Acquire: want a WaitBudgetError, got nil")
	}
	var wbe *WaitBudgetError
	if !errors.As(err, &wbe) {
		t.Fatalf("Acquire err = %T (%v), want *WaitBudgetError", err, err)
	}
	if wbe.RetryAfter < 50*time.Minute {
		t.Fatalf("RetryAfter = %v, want ~1h (the next window opening)", wbe.RetryAfter)
	}
	if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
		t.Fatalf("Acquire blocked for %v, want an immediate refusal", elapsed)
	}
	if len(outcomes) != 3 || outcomes[2] != "budget_exceeded" {
		t.Fatalf("outcomes = %v, want the third to be budget_exceeded", outcomes)
	}
	if waits[2] > 40*time.Millisecond {
		t.Fatalf("budget_exceeded wait = %v, want ~0 (nothing was queued)", waits[2])
	}

	// The refused caller's booking must be gone: two admissions were made, so
	// two bookings stand. A leaked third would push every later caller out by a
	// slot for nothing.
	s := d.slotFor(url)
	s.mu.Lock()
	booked := len(s.window.sent)
	s.mu.Unlock()
	if booked != 2 {
		t.Fatalf("window bookings = %d, want 2 (the refused caller's slot handed back)", booked)
	}
}

// A deadline the wait DOES fit in is not a fast-fail: the caller waits and is
// admitted, as before.
func TestAcquireQueuesWhenTheWaitFitsTheBudget(t *testing.T) {
	const minDelay = 20 * time.Millisecond
	var outcomes []string
	d := NewDomainLimiter(1, minDelay)
	d.OnWait = func(_, outcome string, _ time.Duration) { outcomes = append(outcomes, outcome) }

	release, err := d.Acquire(context.Background(), "reddit", "https://www.reddit.com/")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err = d.Acquire(ctx, "reddit", "https://www.reddit.com/")
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	release()

	if len(outcomes) != 2 || outcomes[1] != "acquired" {
		t.Fatalf("outcomes = %v, want [acquired acquired]", outcomes)
	}
}

// A caller with NO deadline has no budget to exceed, so it queues exactly as
// before — the fast-fail must not turn an unbounded caller into an error.
func TestAcquireWithoutADeadlineStillQueues(t *testing.T) {
	const url = "https://www.reddit.com/"
	const minDelay = 30 * time.Millisecond
	var outcomes []string
	d := NewDomainLimiter(1, minDelay)
	d.OnWait = func(_, outcome string, _ time.Duration) { outcomes = append(outcomes, outcome) }

	release, err := d.Acquire(context.Background(), "reddit", url)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()

	start := time.Now()
	release, err = d.Acquire(context.Background(), "reddit", url)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	release()
	if elapsed := time.Since(start); elapsed < minDelay/2 {
		t.Fatalf("second Acquire returned after %v, want it to sit out the politeness delay", elapsed)
	}
	if len(outcomes) != 2 || outcomes[1] != "acquired" {
		t.Fatalf("outcomes = %v, want [acquired acquired]", outcomes)
	}
}

// Send-spaced mode is the whole reason NewSpacedDomainQuotaLimiter exists:
// callers that arrive together are booked minDelay apart instead of all reading
// the same stale lastSend and computing wait 0.
func TestSpacedReserveSpacesSimultaneousCallers(t *testing.T) {
	const minDelay = time.Second
	s := &domainSlot{spaceSends: true}
	now := time.Unix(1_000_000, 0)

	var booked []time.Time
	var waits []time.Duration
	for range 8 {
		wait, at := s.reserve(now, minDelay)
		booked = append(booked, at)
		waits = append(waits, wait)
	}

	for i, at := range booked {
		want := now.Add(time.Duration(i) * minDelay)
		if !at.Equal(want) {
			t.Fatalf("caller %d booked %v, want %v — admissions are not spaced", i, at, want)
		}
		// The wait must be the booked gap EXACTLY. Jitter here would move the
		// send off its booking, and since the spacing is measured between
		// bookings, two neighbours drawing far apart would send closer together
		// than minDelay — the one thing this mode exists to prevent.
		if waits[i] != time.Duration(i)*minDelay {
			t.Fatalf("caller %d waited %v, want exactly %v — jitter leaked into send-spaced mode",
				i, waits[i], time.Duration(i)*minDelay)
		}
	}
}

// The companion to the test above, pinning the behaviour that forces the
// completion-spaced limiter to stay at concurrency 1: on a cold slot every
// caller measures from the zero lastSend, so eight of them send at once.
func TestCompletionSpacedReserveBurstsOnAColdSlot(t *testing.T) {
	s := &domainSlot{}
	now := time.Unix(1_000_000, 0)

	for i := range 8 {
		if wait, _ := s.reserve(now, time.Second); wait != 0 {
			t.Fatalf("caller %d waited %v, want 0 — the burst this mode cannot avoid", i, wait)
		}
	}
}

// A slot that has been quiet longer than minDelay must admit at once: nextSend
// is monotonic, so it sits in the past and the wait clamps to zero rather than
// going negative.
func TestSpacedReserveAdmitsAfterAQuietSpell(t *testing.T) {
	const minDelay = time.Second
	s := &domainSlot{spaceSends: true}
	now := time.Unix(1_000_000, 0)

	s.reserve(now, minDelay)

	wait, at := s.reserve(now.Add(time.Hour), minDelay)
	if wait != 0 {
		t.Fatalf("wait = %v after an hour idle, want 0", wait)
	}
	if !at.Equal(now.Add(time.Hour)) {
		t.Fatalf("booked %v, want the arrival instant %v", at, now.Add(time.Hour))
	}
}

// The quota's own wait must push nextSend too, or the callers queued behind a
// full window would space themselves from an instant nobody sends at.
func TestSpacedReserveSpacesFromTheQuotaWait(t *testing.T) {
	const minDelay = time.Second
	s := &domainSlot{spaceSends: true, window: quotaWindow{limit: 2, period: time.Minute}}
	now := time.Unix(1_000_000, 0)

	s.reserve(now, minDelay)             // sends at now
	s.reserve(now, minDelay)             // sends at now+1s
	_, third := s.reserve(now, minDelay) // window full: waits out the period
	if !third.Equal(now.Add(time.Minute)) {
		t.Fatalf("third booked %v, want %v — the quota did not hold it", third, now.Add(time.Minute))
	}

	// The fourth spaces itself from where the third will actually send, not
	// from where the third asked to send.
	_, fourth := s.reserve(now, minDelay)
	if !fourth.Equal(third.Add(minDelay)) {
		t.Fatalf("fourth booked %v, want %v — spacing lost behind the quota",
			fourth, third.Add(minDelay))
	}
}

// An upstream's Retry-After outranks the spacing mark, exactly as it does in
// completion-spaced mode.
func TestSpacedReserveHonorsNotBefore(t *testing.T) {
	s := &domainSlot{spaceSends: true}
	now := time.Unix(1_000_000, 0)
	s.notBefore = now.Add(30 * time.Second)

	wait, at := s.reserve(now, time.Second)
	if wait < 30*time.Second {
		t.Fatalf("wait = %v, want at least the 30s penalty", wait)
	}
	if !at.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("booked %v, want the penalized instant %v", at, now.Add(30*time.Second))
	}
}

// A caller that gives up while it is the last booker hands its spacing slot
// back, or a run of cancellations would push nextSend out forever.
func TestSpacedUnbookRewindsTheLastBooking(t *testing.T) {
	const minDelay = time.Second
	s := &domainSlot{spaceSends: true}
	now := time.Unix(1_000_000, 0)

	s.reserve(now, minDelay)
	_, second := s.reserve(now, minDelay)
	s.unbook(second, minDelay)

	_, retry := s.reserve(now, minDelay)
	if !retry.Equal(second) {
		t.Fatalf("booked %v after the rollback, want the freed slot %v", retry, second)
	}
}

// Withdrawing a booking that is NOT the last one frees that instant, but must
// not pull the callers behind it forward: they already spaced themselves from
// the LAST booking, and sending closer than minDelay is the failure this mode
// exists to prevent.
func TestSpacedUnbookKeepsTheSpacingBehindIt(t *testing.T) {
	const minDelay = time.Second
	s := &domainSlot{spaceSends: true}
	now := time.Unix(1_000_000, 0)

	_, first := s.reserve(now, minDelay)
	_, second := s.reserve(now, minDelay)
	s.unbook(first, minDelay)

	_, third := s.reserve(now, minDelay)
	if !third.Equal(second.Add(minDelay)) {
		t.Fatalf("booked %v, want %v — the rollback moved the mark under a live booking",
			third, second.Add(minDelay))
	}
}

// Completion-spaced mode must be untouched by the booking machinery: lastSend
// is its clock, and keeping a parallel set of instants would be a silent mode
// leak.
func TestUnbookLeavesCompletionSpacedModeAlone(t *testing.T) {
	s := &domainSlot{window: quotaWindow{limit: 4, period: time.Minute}}
	now := time.Unix(1_000_000, 0)

	_, booked := s.reserve(now, time.Second)
	s.unbook(booked, time.Second)

	if len(s.booked) != 0 {
		t.Fatalf("booked = %v, want empty in completion-spaced mode", s.booked)
	}
	if len(s.window.sent) != 0 {
		t.Fatalf("window holds %d bookings, want 0", len(s.window.sent))
	}
}

// The shape a high-water mark cannot survive: a fanned-out client whose shared
// ctx dies abandons every booking at once, in arbitrary order. All of them must
// be withdrawn, or the slot keeps a phantom wait with nothing in flight and the
// next search queues behind callers that no longer exist.
func TestSpacedUnbookClearsAnAbandonedFanOut(t *testing.T) {
	const minDelay = time.Second
	s := &domainSlot{spaceSends: true}
	now := time.Unix(1_000_000, 0)

	var booked []time.Time
	for range 8 {
		_, at := s.reserve(now, minDelay)
		booked = append(booked, at)
	}
	// Out of order, the way concurrent cancellations land.
	for _, i := range []int{3, 0, 7, 1, 6, 2, 5, 4} {
		s.unbook(booked[i], minDelay)
	}

	wait, at := s.reserve(now, minDelay)
	if wait != 0 {
		t.Fatalf("wait = %v after every booking was withdrawn, want 0", wait)
	}
	if !at.Equal(now) {
		t.Fatalf("booked %v, want the arrival instant %v", at, now)
	}
}

// End to end: the spaced limiter runs maxConcurrent requests at once, which is
// the property the cluster cap buys and the completion-spaced limiter at
// concurrency 1 cannot offer.
func TestSpacedLimiterRunsCallersConcurrently(t *testing.T) {
	d := NewSpacedDomainQuotaLimiter(4, 0, 0, 0)

	var releases []func()
	for i := range 4 {
		release, err := d.Acquire(context.Background(), "searxng", "https://searxng.test/search")
		if err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
		releases = append(releases, release)
	}

	// A fifth must block: the cap is 4, and nothing has been released.
	blocked := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := d.Acquire(ctx, "searxng", "https://searxng.test/search"); err == nil {
			t.Error("fifth Acquire succeeded, want it held by the concurrency cap")
		}
		close(blocked)
	}()
	<-blocked

	for _, release := range releases {
		release()
	}
}
