package redislimit

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// base is an arbitrary fixed instant; every script test drives the clock
// explicitly from it, so no test depends on wall-clock time.
var base = time.Unix(1_700_000_000, 0)

// clock drives miniredis' notion of time. SetTime moves what TIME reports
// (which the Lua scripts read) but does NOT expire keys, and FastForward
// expires keys without moving TIME — a test that advances time needs both.
type clock struct {
	m   *miniredis.Miniredis
	now time.Time
}

func newFixture(t *testing.T, cfg Config) (*Limiter, *miniredis.Miniredis, *clock) {
	t.Helper()
	m := miniredis.RunT(t)
	c := &clock{m: m, now: base}
	m.SetTime(base)

	cfg.Client = redis.NewClient(&redis.Options{Addr: m.Addr()})
	cfg.Scope = "test"
	cfg.Prefix = "omnifeed:ratelimit"
	t.Cleanup(func() { _ = cfg.Client.Close() })
	return New(cfg), m, c
}

func (c *clock) advance(d time.Duration) {
	c.now = c.now.Add(d)
	c.m.SetTime(c.now)
	c.m.FastForward(d)
}

// book runs one admission attempt and asserts the noeviction invariant: every
// key the script wrote must carry a TTL, on every call. An un-TTL'd key on the
// shared Redis this targets is a leak that eventually OOMs the instance.
func book(t *testing.T, l *Limiter, m *miniredis.Miniredis, host string) time.Duration {
	t.Helper()
	win, next := l.keys(host)
	wait, err := l.book(context.Background(), win, next)
	if err != nil {
		t.Fatalf("book: %v", err)
	}
	assertTTLs(t, m)
	return wait
}

func assertTTLs(t *testing.T, m *miniredis.Miniredis) {
	t.Helper()
	for _, k := range m.Keys() {
		if m.TTL(k) <= 0 {
			t.Fatalf("key %q has no TTL (%v): Redis runs noeviction, so this leaks", k, m.TTL(k))
		}
	}
}

// A minimum delay alone spaces admissions, and the wait it reports is exactly
// what is left of the delay.
func TestAcquireScriptSpacesByMinDelay(t *testing.T) {
	l, m, c := newFixture(t, Config{MinDelay: 2 * time.Second})
	const host = "example.com"

	if wait := book(t, l, m, host); wait != 0 {
		t.Fatalf("first admission: wait = %v, want 0", wait)
	}
	if wait := book(t, l, m, host); wait != 2*time.Second {
		t.Fatalf("immediate retry: wait = %v, want 2s", wait)
	}
	c.advance(1500 * time.Millisecond)
	if wait := book(t, l, m, host); wait != 500*time.Millisecond {
		t.Fatalf("after 1.5s: wait = %v, want 500ms", wait)
	}
	c.advance(500 * time.Millisecond)
	if wait := book(t, l, m, host); wait != 0 {
		t.Fatalf("after the delay: wait = %v, want 0", wait)
	}
}

// The quota bounds a burst: once the window holds `quota` admissions, the wait
// is however long the oldest one still has to live.
func TestAcquireScriptBoundsTheBurst(t *testing.T) {
	l, m, c := newFixture(t, Config{Quota: 3, Window: 90 * time.Second})
	const host = "searxng"

	for i := range 3 {
		if wait := book(t, l, m, host); wait != 0 {
			t.Fatalf("admission %d: wait = %v, want 0", i, wait)
		}
		c.advance(time.Second)
	}
	// Three admissions at base, base+1s, base+2s; asking at base+3s.
	if got, want := book(t, l, m, host), 87*time.Second; got != want {
		t.Fatalf("4th: wait = %v, want %v", got, want)
	}
	// A waiting caller books nothing, so the answer does not move until the
	// window actually rolls.
	if got, want := book(t, l, m, host), 87*time.Second; got != want {
		t.Fatalf("4th again: wait = %v, want %v (a waiter holds nothing)", got, want)
	}
	c.advance(87 * time.Second)
	if wait := book(t, l, m, host); wait != 0 {
		t.Fatalf("after the window rolled: wait = %v, want 0", wait)
	}
}

// The opening a caller needs is the admission `quota` places back, not the
// oldest one — the same sent[n-limit] arithmetic the in-process window uses.
// More members than the quota is reachable in production: a rolling deploy runs
// two replicas with different quota settings against one key.
func TestAcquireScriptUsesTheQuotaRankedOpening(t *testing.T) {
	l, m, c := newFixture(t, Config{Quota: 2, Window: 90 * time.Second})
	const host = "example.com"
	win, _ := l.keys(host)

	// Four admissions already in the window, one second apart.
	for i := range 4 {
		at := base.Add(time.Duration(i) * time.Second)
		if _, err := l.cfg.Client.ZAdd(context.Background(), win,
			redis.Z{Score: float64(at.UnixMilli()), Member: "seed-" + string(rune('a'+i))}).Result(); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// The seed stands in for keys another replica wrote, TTL included.
	if err := l.cfg.Client.PExpire(context.Background(), win, 95*time.Second).Err(); err != nil {
		t.Fatalf("seed ttl: %v", err)
	}
	c.advance(4 * time.Second)

	// n=4, quota=2 → the opening is member 2 (base+2s), which frees at base+92s;
	// we ask at base+4s. Taking sent[0] instead would answer 86s.
	if got, want := book(t, l, m, host), 88*time.Second; got != want {
		t.Fatalf("wait = %v, want %v (opening = the quota-ranked member)", got, want)
	}
}

// Delay and quota compose: whichever constraint reaches further into the future
// wins, and neither is lost.
func TestAcquireScriptComposesDelayAndQuota(t *testing.T) {
	cases := []struct {
		name     string
		delay    time.Duration
		quota    int
		window   time.Duration
		wantWait time.Duration
	}{
		{name: "delay dominates", delay: 20 * time.Second, quota: 1, window: 10 * time.Second, wantWait: 20 * time.Second},
		{name: "quota dominates", delay: 2 * time.Second, quota: 1, window: 30 * time.Second, wantWait: 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, m, _ := newFixture(t, Config{MinDelay: tc.delay, Quota: tc.quota, Window: tc.window})
			if wait := book(t, l, m, "example.com"); wait != 0 {
				t.Fatalf("first admission: wait = %v, want 0", wait)
			}
			if got := book(t, l, m, "example.com"); got != tc.wantWait {
				t.Fatalf("second admission: wait = %v, want %v", got, tc.wantWait)
			}
		})
	}
}

// The minimum delay is measured from COMPLETION, like the in-process limiter's
// lastSend: releasing a request that ran for a second still leaves a full delay
// before the next send.
func TestReleaseScriptBumpsFromCompletion(t *testing.T) {
	l, m, c := newFixture(t, Config{MinDelay: 2 * time.Second})
	const host = "example.com"
	_, next := l.keys(host)

	if wait := book(t, l, m, host); wait != 0 {
		t.Fatalf("first admission: wait = %v, want 0", wait)
	}
	c.advance(time.Second) // the request itself takes a second
	l.release(next)
	assertTTLs(t, m)

	// Send-to-send spacing would answer 1s here; completion-based answers 2s.
	if got, want := book(t, l, m, host), 2*time.Second; got != want {
		t.Fatalf("after release: wait = %v, want %v", got, want)
	}
}

// release never shortens a gap another replica already reserved.
func TestReleaseScriptNeverShortensTheGap(t *testing.T) {
	l, m, c := newFixture(t, Config{MinDelay: 2 * time.Second})
	_, next := l.keys("example.com")

	if err := l.cfg.Client.Set(context.Background(), next,
		base.Add(time.Minute).UnixMilli(), time.Minute).Err(); err != nil {
		t.Fatalf("seed: %v", err)
	}
	l.release(next)
	assertTTLs(t, m)

	c.advance(0)
	if got, want := book(t, l, m, "example.com"), time.Minute; got != want {
		t.Fatalf("wait = %v, want %v (the longer reservation must survive)", got, want)
	}
}

// Two admissions in the same millisecond must be two members, or the window
// silently undercounts and admits more than its quota.
func TestAcquireScriptNonceKeepsSameMillisecondAdmissionsDistinct(t *testing.T) {
	l, m, _ := newFixture(t, Config{Quota: 5, Window: time.Minute})
	const host = "example.com"
	win, _ := l.keys(host)

	for i := range 3 {
		if wait := book(t, l, m, host); wait != 0 {
			t.Fatalf("admission %d: wait = %v, want 0", i, wait)
		}
	}
	n, err := l.cfg.Client.ZCard(context.Background(), win).Result()
	if err != nil {
		t.Fatalf("ZCARD: %v", err)
	}
	if n != 3 {
		t.Fatalf("ZCARD = %d, want 3 (same-millisecond admissions collapsed)", n)
	}
}

// With no quota the window key is never created, so a crawl limiter's unbounded
// host cardinality only ever costs one short-lived key per host.
func TestAcquireScriptWritesNoWindowWithoutQuota(t *testing.T) {
	l, m, _ := newFixture(t, Config{MinDelay: time.Second})
	win, _ := l.keys("example.com")

	if wait := book(t, l, m, "example.com"); wait != 0 {
		t.Fatalf("wait = %v, want 0", wait)
	}
	if m.Exists(win) {
		t.Fatalf("%s exists with no quota configured", win)
	}
}

// Neither key may outlive its meaning by more than the slack.
func TestScriptTTLsAreBounded(t *testing.T) {
	l, m, _ := newFixture(t, Config{MinDelay: 2 * time.Second, Quota: 2, Window: 30 * time.Second})
	win, next := l.keys("example.com")

	if wait := book(t, l, m, "example.com"); wait != 0 {
		t.Fatalf("wait = %v, want 0", wait)
	}
	if got, want := m.TTL(win), 30*time.Second+keyTTLSlack; got != want {
		t.Errorf("window TTL = %v, want %v", got, want)
	}
	if got, want := m.TTL(next), 2*time.Second+keyTTLSlack; got != want {
		t.Errorf("next TTL = %v, want %v", got, want)
	}
}
