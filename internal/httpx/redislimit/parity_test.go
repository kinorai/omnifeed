package redislimit

import (
	"context"
	"testing"
	"time"
)

// This file proves that the Lua scripts decide the same waits as the in-process
// *httpx.DomainLimiter on the same settings and the same clock. Divergence is
// the real risk of this package: an operator keeps the same OMNIFEED_ values
// and expects the same pacing, only shared between replicas.
//
// The local side is a transcription of httpx.domainSlot.reserve and
// quotaWindow.book (jitter excluded — it is random on both sides and is added
// after the decision). It has to be a transcription: those types are unexported,
// and a test inside package httpx cannot import this package without an import
// cycle. Keep the two in step — the comments name the lines they mirror.
//
// The scenarios are sequential (each caller is admitted and released before the
// next arrives), which is where the two are required to agree exactly. Under
// concurrency they diverge by design: the local window books waiting callers
// into the future, while a Redis waiter books nothing and retries.
type localSlot struct {
	lastSend time.Time
	limit    int
	period   time.Duration
	sent     []time.Time
}

// reserve mirrors domainSlot.reserve, minus the jitter.
func (s *localSlot) reserve(now time.Time, minDelay time.Duration) time.Duration {
	var wait time.Duration
	if since := now.Sub(s.lastSend); since < minDelay {
		wait = minDelay - since
	}
	booked := now.Add(wait)
	return wait + s.book(booked)
}

// book mirrors quotaWindow.book.
func (s *localSlot) book(at time.Time) time.Duration {
	if s.limit <= 0 {
		return 0
	}
	cutoff := at.Add(-s.period)
	i := 0
	for i < len(s.sent) && !s.sent[i].After(cutoff) {
		i++
	}
	s.sent = s.sent[i:]

	var wait time.Duration
	if n := len(s.sent); n >= s.limit {
		if free := s.sent[n-s.limit].Add(s.period).Sub(at); free > 0 {
			wait = free
		}
	}
	s.sent = append(s.sent, at.Add(wait))
	return wait
}

// step is one instant in a scenario: wait for `advance`, then acquire. `hold`
// is how long the request runs before it releases.
type step struct {
	advance  time.Duration
	hold     time.Duration
	wantWait time.Duration
}

func TestParityWithTheInProcessLimiter(t *testing.T) {
	cases := []struct {
		name   string
		cfg    Config
		steps  []step
		effect string
	}{
		{
			name:   "min delay only, measured from completion",
			cfg:    Config{MinDelay: 3 * time.Second},
			effect: "a request that runs 1s still owes the full delay after it",
			steps: []step{
				{hold: time.Second},
				{advance: 500 * time.Millisecond, hold: time.Second, wantWait: 2500 * time.Millisecond},
				{advance: 10 * time.Second, hold: 0},
			},
		},
		{
			name:   "quota only, window rolls",
			cfg:    Config{Quota: 2, Window: 30 * time.Second},
			effect: "the third caller waits out the oldest admission",
			steps: []step{
				{},
				{advance: time.Second},
				{advance: time.Second, wantWait: 28 * time.Second},
				{advance: 40 * time.Second},
			},
		},
		{
			name:   "delay and quota together",
			cfg:    Config{MinDelay: 5 * time.Second, Quota: 2, Window: 30 * time.Second},
			effect: "the delay is served first, then the window is consulted at the instant it would allow",
			steps: []step{
				{hold: time.Second},
				{advance: 10 * time.Second, hold: time.Second},
				// 4s of delay left, but at that instant the window is still full
				// until 30s after its oldest admission: 17s in total, not 4s.
				{advance: time.Second, wantWait: 17 * time.Second},
				{advance: 5 * time.Second, wantWait: 6 * time.Second},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, m, c := newFixture(t, tc.cfg)
			const host = "example.com"
			win, next := l.keys(host)
			local := &localSlot{limit: tc.cfg.Quota, period: tc.cfg.Window}

			for i, st := range tc.steps {
				c.advance(st.advance)

				gotLocal := local.reserve(c.now, tc.cfg.MinDelay)
				if gotLocal != st.wantWait {
					t.Fatalf("step %d: in-process wait = %v, want %v (%s)", i, gotLocal, st.wantWait, tc.effect)
				}

				// The Redis side is asked at the same instant; when it reports a
				// wait it has NOT admitted, so advance the clock and ask again,
				// exactly as Acquire's loop does.
				gotRedis, err := l.book(context.Background(), win, next)
				if err != nil {
					t.Fatalf("step %d: book: %v", i, err)
				}
				if gotRedis != st.wantWait {
					t.Fatalf("step %d: redis wait = %v, want %v (parity with the in-process limiter)", i, gotRedis, st.wantWait)
				}
				if gotRedis > 0 {
					c.advance(gotRedis)
					again, err := l.book(context.Background(), win, next)
					if err != nil {
						t.Fatalf("step %d: retry book: %v", i, err)
					}
					if again != 0 {
						t.Fatalf("step %d: retry after waiting %v still owed %v", i, gotRedis, again)
					}
				}

				c.advance(st.hold)
				local.lastSend = c.now
				l.release(next)
				assertTTLs(t, m)
			}
		})
	}
}
