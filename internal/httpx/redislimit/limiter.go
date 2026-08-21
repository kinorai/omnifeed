// Package redislimit paces outbound requests with the pacing state held in
// Redis, so every replica of a deployment counts against one limit instead of
// one limit each. It implements httpx.Limiter and mirrors *httpx.DomainLimiter:
// the minimum delay is measured from the previous request's COMPLETION, the
// rolling-window quota keeps the admission instants still inside the window
// (not a token bucket, so a burst cannot be spent all at once), and a nonzero
// wait carries jitter.
//
// Two things stay per pod on purpose. The concurrency semaphore does: a
// distributed one needs TTL leases and heartbeats to survive a crawl that runs
// for minutes, which is heavy machinery for a bound that replicas × N already
// keeps small. And a caller that abandons its wait books nothing at all, so
// cancellation needs no cleanup — the price is that cross-pod queueing is
// jittered retry rather than the local limiter's future-booked FIFO order.
//
// The third divergence is the one MinDelay buys. The acquire script sets the
// next-allowed-at at admission and release bumps it again from completion, so
// with MinDelay > 0 admissions to one host are strictly serialized across the
// whole cluster: same-host concurrency collapses to 1 however many MaxConcurrent
// slots each pod holds. That is politer than the in-process limiter's N
// concurrent requests, and slower — deliberate for a politeness control, and
// crash-safe, since a pod that dies mid-request leaves correct spacing behind.
//
// Both scripts take the clock from Redis' TIME rather than from the pod. That
// was gated on a question about the test double: redis.call('TIME') inside
// miniredis' EVAL does honour miniredis.SetTime (verified), so tests drive the
// same server-clock path production uses. Note that miniredis.FastForward
// expires keys WITHOUT moving that clock, so a test that advances time must
// call both.
//
// Redis is never allowed to break retrieval: every backend failure comes back
// wrapped in httpx.ErrLimiterUnavailable for *httpx.FallbackLimiter to absorb.
package redislimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kinorai/omnifeed/internal/httpx"
	"github.com/redis/go-redis/v9"
)

// keyTTLSlack pads every key's TTL past the instant it stops being needed, so
// no reader can lose a key that still constrains it to a rounding error or a
// slow round-trip.
const keyTTLSlack = 5 * time.Second

// releaseCeiling bounds the release script's own context. The real per-operation
// budget is the client's Dial/Read/WriteTimeout (set at wiring time); this only
// stops a release from pinning a goroutine forever if the client was built
// without one.
const releaseCeiling = 10 * time.Second

// Config describes one limiter scope. Scope separates the key spaces of
// limiters that share a Redis instance ("domain" for crawling, "searxng" for
// queries), so a crawl and a search never consume each other's quota.
type Config struct {
	// Client is a UniversalClient rather than *redis.Client so a deployment can
	// point the same wiring at a standalone, sentinel or cluster Redis. This
	// package only needs EVALSHA, which every variant serves.
	Client redis.UniversalClient

	Scope  string // key-space separator, e.g. "domain" or "searxng"
	Prefix string // key namespace, e.g. "omnifeed:ratelimit"

	MaxConcurrent int           // per-pod, see the package doc
	MinDelay      time.Duration // minimum gap between completion and next send
	Quota         int           // 0 disables the rolling-window cap
	Window        time.Duration // width of that window
}

// Limiter admits outbound requests against state shared through Redis.
type Limiter struct {
	cfg  Config
	sems sync.Map // host → chan struct{}

	// nonce keeps two admissions in the same millisecond distinct as ZSET
	// members. The random prefix is what makes it unique across replicas.
	noncePrefix string
	nonceSeq    atomic.Uint64

	// OnWait, when non-nil, is called on every Acquire exit that made a pacing
	// decision, with the engine that waited, the outcome ("acquired", or
	// "canceled" when the caller's ctx died) and the time spent blocked. Same
	// contract as DomainLimiter.OnWait. A backend failure deliberately emits
	// NOTHING: FallbackLimiter immediately re-runs the acquire on the in-process
	// limiter, which observes the whole wait itself, and a second observation
	// here would double-count every acquire made while Redis is down. Set once
	// at wiring time.
	OnWait func(engine, outcome string, waited time.Duration)

	// OnError, when non-nil, is called on every backend failure with the
	// operation that failed ("acquire", "release" or "penalize"). Release and
	// penalize failures reach the caller no other way. Set once at wiring time.
	OnError func(op string)
}

var _ httpx.Limiter = (*Limiter)(nil)

// New returns a limiter for one scope. Per-operation timeouts are NOT this
// package's business: they belong to the redis client's options, set once at
// wiring time. An Acquire legitimately sleeps for minutes between attempts, so
// there is no timeout this package could impose that would not be wrong.
func New(cfg Config) *Limiter {
	return &Limiter{cfg: cfg, noncePrefix: noncePrefix()}
}

// Acquire blocks until Redis admits this request, or until ctx is done. The
// returned release func must be called when the request finishes: it is what
// starts the minimum delay for the next one.
func (l *Limiter) Acquire(ctx context.Context, engine, rawURL string) (func(), error) {
	start := time.Now()
	host := hostOf(rawURL)
	sem := l.semFor(host)
	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		l.observeWait(engine, "canceled", start)
		return nil, ctx.Err()
	}

	winKey, nextKey := l.keys(host)
	for {
		wait, err := l.book(ctx, winKey, nextKey)
		if err != nil {
			<-sem
			// A caller that walked away mid-EVALSHA is not a backend failure:
			// the redis client reports the CALLER's dead ctx as its own error,
			// and wrapping that would open FallbackLimiter's circuit against a
			// healthy Redis. Same exit as the sleep-cancel path below.
			//
			// Discriminate on ctx, never on the error alone: a
			// DeadlineExceeded raised by the client's own ReadTimeout arrives
			// while ctx is still alive, and that one IS a backend failure.
			if ctx.Err() != nil &&
				(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				l.observeWait(engine, "canceled", start)
				return nil, ctx.Err()
			}
			l.observeError("acquire")
			// Wrapped so FallbackLimiter recognizes it and paces in process
			// instead: an unwrapped error here would fail the crawl.
			return nil, fmt.Errorf("redislimit acquire: %w: %w", httpx.ErrLimiterUnavailable, err)
		}
		if wait == 0 {
			break
		}
		// Jitter so replicas released by the same opening do not re-synchronize
		// on it. The wait itself is not booked, so retrying is the whole
		// protocol: another pod may have taken the opening meanwhile.
		if !sleepCtx(ctx, wait+jitter()) {
			<-sem
			l.observeWait(engine, "canceled", start)
			return nil, ctx.Err()
		}
	}

	l.observeWait(engine, "acquired", start)
	return func() {
		l.release(nextKey)
		<-sem
	}, nil
}

// book runs one admission attempt. It returns 0 when the request was admitted
// and booked, or the wait Redis says is still owed.
func (l *Limiter) book(ctx context.Context, winKey, nextKey string) (time.Duration, error) {
	ms, err := acquire.Run(ctx, l.cfg.Client,
		[]string{winKey, nextKey},
		l.cfg.Quota,
		l.cfg.Window.Milliseconds(),
		l.cfg.MinDelay.Milliseconds(),
		keyTTLSlack.Milliseconds(),
		l.nonce(),
	).Int64()
	if err != nil {
		return 0, err
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// release starts the minimum delay from this instant. It runs on its own
// context because the request's ctx is usually already dead by the time a
// caller releases, and errors go to the OnError hook only — a failed release
// costs pacing accuracy, never the request that just succeeded.
func (l *Limiter) release(nextKey string) {
	if l.cfg.MinDelay <= 0 {
		return
	}
	l.bump(nextKey, l.cfg.MinDelay, "release")
}

// Penalize holds the host back for d across every replica — what an upstream's
// Retry-After on a 429 or 503 is worth. It is the same next-allowed-at bump a
// release makes, with the upstream's delay instead of the configured one, so it
// only ever extends the hold and never shortens one another replica reserved.
//
// Fire and forget, like release: a failed penalty costs pacing accuracy, and the
// caller is a response handler with nothing to do about it. A non-positive d
// does nothing.
func (l *Limiter) Penalize(rawURL string, d time.Duration) {
	if d <= 0 {
		return
	}
	_, nextKey := l.keys(hostOf(rawURL))
	l.bump(nextKey, d, "penalize")
}

// bump pushes the next admission for one host to at least d from now, and
// reports a backend failure under the given op label.
func (l *Limiter) bump(nextKey string, d time.Duration, op string) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseCeiling)
	defer cancel()
	if err := release.Run(ctx, l.cfg.Client, []string{nextKey},
		d.Milliseconds(), keyTTLSlack.Milliseconds()).Err(); err != nil {
		l.observeError(op)
	}
}

// keys names the two keys the acquire script takes for one host. The host is
// braced as a Redis Cluster hash tag so both keys land in the same slot: a
// multi-key EVALSHA across two slots is a CROSSSLOT error on every call, which
// on a cluster would mean permanent, silent degradation to per-pod pacing.
func (l *Limiter) keys(host string) (win, next string) {
	base := l.cfg.Prefix + ":" + l.cfg.Scope + ":{" + host + "}"
	return base + ":win", base + ":next"
}

func (l *Limiter) semFor(host string) chan struct{} {
	if v, ok := l.sems.Load(host); ok {
		return v.(chan struct{})
	}
	v, _ := l.sems.LoadOrStore(host, make(chan struct{}, max(l.cfg.MaxConcurrent, 1)))
	return v.(chan struct{})
}

func (l *Limiter) nonce() string {
	return l.noncePrefix + "-" + strconv.FormatUint(l.nonceSeq.Add(1), 10)
}

func (l *Limiter) observeWait(engine, outcome string, start time.Time) {
	if l.OnWait != nil {
		l.OnWait(engine, outcome, time.Since(start))
	}
}

func (l *Limiter) observeError(op string) {
	if l.OnError != nil {
		l.OnError(op)
	}
}

// hostOf mirrors DomainLimiter.slotFor: a malformed URL yields the empty host,
// which paces all such callers together rather than not at all.
func hostOf(rawURL string) string {
	u, _ := url.Parse(rawURL)
	if u == nil {
		return ""
	}
	return u.Hostname()
}

// sleepCtx waits for d and reports whether it completed. It returns false when
// ctx died first — a canceled caller must not keep queueing behind a host.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// jitter spreads waiters that were released by the same opening.
//
// Scheduling noise, not a secret: nothing downstream is authenticated or made
// unguessable by it, and an attacker able to predict it would learn only when a
// politeness delay ends. crypto/rand would be slower and can fail.
// #nosec G404 -- non-cryptographic timing jitter
func jitter() time.Duration {
	return time.Duration(mrand.Intn(500)) * time.Millisecond
}

// noncePrefix is drawn once per process so that two replicas admitting in the
// same millisecond write two distinct ZSET members. On the (practically
// impossible) failure of the system entropy source, fall back to the clock:
// a duplicated member would undercount the window, not corrupt it.
func noncePrefix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b[:])
}
