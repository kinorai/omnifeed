package reddit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/kinorai/omnifeed/internal/domain"
)

// The ways the fetcher can read Reddit, in the order the chain tries them by
// default. Reddit closed logged-out .json in 2026, so each needs something:
//   - MethodClientID: an OAuth client ID (installed-client grant). No account,
//     1000 requests per window, but Reddit closes the public API in March 2027.
//   - MethodSession: a logged-in reddit_session cookie. 100 requests per window,
//     and a human must log in again about every 6 months.
//   - MethodBrowser: the rendered page through crawl4ai. Slow, and logged out
//     unless crawl4ai itself holds a session.
const (
	MethodClientID = "client_id"
	MethodSession  = "session"
	MethodBrowser  = "browser"
)

// DefaultMethods is the chain order when none is configured.
var DefaultMethods = []string{MethodClientID, MethodSession, MethodBrowser}

// DefaultMethodCooldown is how long a method that failed in a way another
// method can get around (blocked, rate-limited, refused credential) is
// skipped before the chain tries it again.
const DefaultMethodCooldown = 10 * time.Minute

// method is one configured way to read Reddit, with its memory of failure.
// downUntil is this pod's own memory. With a SharedState, the holds other
// replicas recorded are read on top of it (see Chain.run).
type method struct {
	name      string
	open      func(context.Context) (*Session, error)
	mu        sync.Mutex
	downUntil time.Time
	down      bool // last state reported through OnState
}

// until is when this pod's own skip ends.
func (m *method) until() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.downUntil
}

// hold extends the skip to at least now+d. It is used both for a failure and
// for a back-off Reddit asked for on an answer that still succeeded
// (X-Ratelimit-Remaining 0, or a 429 during expansion that kept a partial
// thread), so it never shortens a longer hold.
func (m *method) hold(now time.Time, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if until := now.Add(d); until.After(m.downUntil) {
		m.downUntil = until
	}
}

// clear ends this pod's skip, unless a hold placed after start (by a
// concurrent call that failed while this one ran) still runs. It reports
// whether the skip ended.
func (m *method) clear(start time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.downUntil.After(start) {
		return false
	}
	m.downUntil = time.Time{}
	return true
}

// report records the method's state and reports whether it changed, so the
// caller signals each change once. The state follows what was observed, not
// the clock: a method stays reported down after its cooldown ends, until a
// call through it succeeds.
func (m *method) report(down bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := m.down != down
	m.down = down
	return changed
}

// SharedState shares the methods' holds between replicas, so a back-off or a
// refused credential seen by one pod stops every pod. Reddit's budget belongs
// to the credential, not to the pod. Implementations: redisstate.Store.
type SharedState interface {
	// Holds returns the time left on each held method. A method that is not
	// held is absent from the map.
	Holds(ctx context.Context, methods []string) (map[string]time.Duration, error)
	// Hold skips method for at least d. It never shortens a longer hold.
	Hold(ctx context.Context, method string, d time.Duration) error
	// Clear ends the hold on method.
	Clear(ctx context.Context, method string) error
}

// sharedCooldown is how long the chain uses only its own memory after the
// shared state failed, before it tries it again. A dead Redis then costs one
// timeout per cooldown, not one per call, as with the rate limiters.
const sharedCooldown = 30 * time.Second

// failover reports whether err is one the next method can get around: Reddit
// refused this method's access (blocked, challenge, rate limit, refused
// credential) or failed upstream. A missing thread or our own pacing refusal
// would fail the same on every method, so those return to the caller.
func failover(err error) bool {
	var fe *domain.FetchError
	if !errors.As(err, &fe) {
		return false
	}
	switch fe.Kind {
	case domain.KindHTTP403, domain.KindHTTP429, domain.KindCaptcha, domain.KindBotBlock, domain.KindUpstreamError:
		return true
	}
	return false
}

// Chain is one crawl's view of every configured method. Each call goes to the
// first available method; on a failover error the method is skipped for its
// cooldown and the call moves to the next one. Not safe for concurrent use.
type Chain struct {
	f    *Fetcher
	legs map[string]*Session
}

// OpenChain starts a crawl over the configured methods. Each method's
// session opens on first use. The caller owns the chain and must Close it.
func (f *Fetcher) OpenChain(context.Context) (*Chain, error) {
	return &Chain{f: f, legs: map[string]*Session{}}, nil
}

// Close releases every session the chain opened.
func (c *Chain) Close(ctx context.Context) error {
	var errs []error
	for _, s := range c.legs {
		errs = append(errs, s.Close(ctx))
	}
	return errors.Join(errs...)
}

func (c *Chain) leg(ctx context.Context, m *method) (*Session, error) {
	if s, ok := c.legs[m.name]; ok {
		return s, nil
	}
	s, err := m.open(ctx)
	if err != nil {
		return nil, err
	}
	c.legs[m.name] = s
	return s, nil
}

// run sends fn to the methods in order and returns the first success. When
// every method is cooling down it fails at once with quota_exhausted and the
// shortest wait: waiting would hold the caller for minutes, and retrying a
// method that just asked for back-off extends its block.
func (c *Chain) run(ctx context.Context, fn func(*Session) error) error {
	return c.runOn(ctx, false, fn)
}

// runOn is run, limited to the direct methods when directOnly is set.
func (c *Chain) runOn(ctx context.Context, directOnly bool, fn func(*Session) error) error {
	f := c.f
	now := f.clock()
	shared := f.sharedHolds(ctx)
	candidates := make([]*method, 0, len(f.methods))
	var soonest time.Duration
	for _, m := range f.methods {
		if directOnly && m.name == MethodBrowser {
			continue
		}
		until := m.until()
		if left, ok := shared[m.name]; ok && now.Add(left).After(until) {
			until = now.Add(left)
		}
		if !now.Before(until) {
			candidates = append(candidates, m)
			continue
		}
		f.setDown(m, true)
		if wait := until.Sub(now); soonest == 0 || wait < soonest {
			soonest = wait
		}
	}
	if len(candidates) == 0 {
		return &domain.FetchError{Kind: domain.KindQuotaExhausted, RetryAfter: soonest,
			Err: fmt.Errorf("every reddit method is cooling down after failures; retry in %ds", int(soonest.Seconds())+1)}
	}
	var last error
	for _, m := range candidates {
		s, err := c.leg(ctx, m)
		if err == nil {
			err = fn(s)
		}
		if err == nil {
			if s.held {
				// Reddit answered but asked for back-off: the method stays
				// skipped for that hold (set by its penalize hook).
				s.held = false
			} else {
				f.markUp(ctx, m, now)
			}
			f.outcome(m.name, "ok")
			return nil
		}
		last = err
		if !failover(err) {
			f.outcome(m.name, "error")
			return err
		}
		hold := f.cooldown
		var fe *domain.FetchError
		if errors.As(err, &fe) && fe.RetryAfter > hold {
			hold = fe.RetryAfter
		}
		f.holdMethod(ctx, m, hold)
		f.outcome(m.name, string(fe.Kind))
		if ctx.Err() != nil {
			return err
		}
	}
	return last
}

// FetchThread is Session.FetchThread over the chain.
func (c *Chain) FetchThread(ctx context.Context, permalink string, limit, depth int, sort string) ([]byte, error) {
	var out []byte
	err := c.run(ctx, func(s *Session) (err error) {
		out, err = s.FetchThread(ctx, permalink, limit, depth, sort)
		return err
	})
	return out, err
}

// FetchListing is Session.FetchListing over the chain.
func (c *Chain) FetchListing(ctx context.Context, sub, sort string, limit int, t string) ([]byte, error) {
	var out []byte
	err := c.run(ctx, func(s *Session) (err error) {
		out, err = s.FetchListing(ctx, sub, sort, limit, t)
		return err
	})
	return out, err
}

// FetchMoreChildren is Session.FetchMoreChildren over the direct methods
// only. The browser would POST /api/morechildren logged out, which Reddit
// refuses with 403, and that 403 would hold the browser method, the one path
// left that still reads threads. Only a direct thread has child IDs to expand
// (the HTML path reports its gaps without them), so a failure here costs the
// expansion only: the engine keeps the partial thread.
func (c *Chain) FetchMoreChildren(ctx context.Context, linkID string, childIDs []string, sort string) ([]byte, error) {
	var out []byte
	err := c.runOn(ctx, true, func(s *Session) (err error) {
		out, err = s.FetchMoreChildren(ctx, linkID, childIDs, sort)
		return err
	})
	return out, err
}

// ResolveShareURL is Session.ResolveShareURL over the chain.
func (c *Chain) ResolveShareURL(ctx context.Context, shareURL string) (string, error) {
	var out string
	err := c.run(ctx, func(s *Session) (err error) {
		out, err = s.ResolveShareURL(ctx, shareURL)
		return err
	})
	return out, err
}
