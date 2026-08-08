// Package lightpanda implements the browser.Browser port over a Lightpanda CDP
// server (https://lightpanda.io) — a from-scratch headless browser built for
// automation that is far lighter than Chromium (~16× less memory, ~9× faster on
// its own crawler benchmark). It is the opt-in fast path for Reddit-style crawls;
// crawl4ai stays the default and the fallback.
//
// Unlike the crawl4ai backend, a Session here holds a live page: Navigate drives
// one real navigation and Evals run against that page, so a deep crawl's
// follow-up in-page fetches (e.g. Reddit /api/morechildren rounds) reuse the same
// page and skip re-navigation. Navigating to the URL the session is already on is
// a no-op, which is what lets the Reddit engine issue Navigate before every fetch
// yet pay the navigation cost only once.
//
// Connection is over the Chrome DevTools Protocol via chromedp, the client
// Lightpanda documents. The bot-wall JavaScript challenge Reddit serves is
// handled the same way it is in a real browser: Navigate waits until the page has
// stopped redirecting (URL stable) and finished loading before any Eval runs.
//
// The chromedp glue is isolated in chromedpDriver; the session's own logic
// (navigation reuse, the URL-stability readiness state machine, JS wrapping) runs
// against the pageDriver interface so it is testable without a live browser.
package lightpanda

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/kinorai/omnifeed/internal/browser"
)

// defaultReadyTimeout bounds how long Navigate waits for a page to settle (finish
// redirecting and loading) before giving up. A giving-up Navigate is a
// fallback-worthy error, so the Reddit engine retries the crawl on crawl4ai.
// It also bounds each navigate/probe CDP round trip, so a connected-but-
// unresponsive server fails within this budget instead of hanging until the
// crawl's own (minutes-long) timeout.
const defaultReadyTimeout = 20 * time.Second

// defaultPollInterval is the gap between readiness checks during Navigate.
const defaultPollInterval = 250 * time.Millisecond

// evalTimeout bounds one Eval CDP round trip. Wider than the ready budget: an
// in-page fetch of a large thread's JSON legitimately takes longer than a
// readiness probe.
const evalTimeout = 60 * time.Second

// errConnLost marks the session's CDP connection as dead (browser crash,
// dropped websocket, closed target). It deliberately does NOT wrap
// context.Canceled — the raw error chromedp surfaces for this case — so the
// Reddit engine can tell a dead browser (fallback-worthy) from a caller
// cancellation (not).
var errConnLost = errors.New("lightpanda: cdp connection lost")

// Browser drives a Lightpanda CDP server. One allocator (one CDP connection) is
// shared; each Open creates its own target (page), so sessions are isolated.
type Browser struct {
	allocCtx     context.Context
	allocCancel  context.CancelFunc
	readyTimeout time.Duration
	pollInterval time.Duration
}

// New dials the Lightpanda CDP server at wsURL (e.g. ws://lightpanda:9222) and
// returns a Browser. The connection is lazy — chromedp connects on first use —
// so New does not fail on an unreachable server; the first crawl does (and falls
// back). Call Close to release the shared allocator at shutdown.
func New(wsURL string) *Browser {
	allocCtx, cancel := chromedp.NewRemoteAllocator(context.Background(), wsURL, chromedp.NoModifyURL)
	return &Browser{
		allocCtx:     allocCtx,
		allocCancel:  cancel,
		readyTimeout: defaultReadyTimeout,
		pollInterval: defaultPollInterval,
	}
}

// Name identifies the backend.
func (*Browser) Name() string { return "lightpanda" }

// Close releases the shared CDP allocator. Call once at shutdown.
func (b *Browser) Close() { b.allocCancel() }

// Open creates a new target (page) on the shared connection and returns a
// Session bound to it. The session's page lives until Close, independent of the
// context passed here.
func (b *Browser) Open(context.Context) (browser.Session, error) {
	taskCtx, cancel := chromedp.NewContext(b.allocCtx)
	return &session{
		driver:       &chromedpDriver{taskCtx: taskCtx, cancel: cancel, readyTimeout: b.readyTimeout},
		readyTimeout: b.readyTimeout,
		pollInterval: b.pollInterval,
	}, nil
}

// pageDriver is the low-level CDP surface a session needs. The real
// implementation (chromedpDriver) wraps chromedp; tests substitute a fake so the
// session's own logic runs without a live browser.
type pageDriver interface {
	// navigate issues a raw navigation to url (no load-event wait).
	navigate(ctx context.Context, url string) error
	// probe reads the page's load state and current URL in one round trip.
	probe(ctx context.Context) (ready bool, url string, err error)
	// evaluate runs expr (a complete JS expression) with the returned promise
	// awaited and the value returned by value.
	evaluate(ctx context.Context, expr string) (string, error)
	// close tears down the page/target.
	close()
}

// session owns one Lightpanda page. Not safe for concurrent use.
type session struct {
	driver       pageDriver
	readyTimeout time.Duration
	pollInterval time.Duration
	current      string // URL the live page settled on; "" until a Navigate succeeds
	requested    string // URL the last successful Navigate was asked for
}

// Close tears down the page/target.
func (s *session) Close(context.Context) error {
	s.driver.close()
	return nil
}

// Navigate loads rawURL and waits for it to settle. Navigating to the URL the
// page is already on — by the name it was requested under or the URL it settled
// on after redirects — is a no-op, so the Reddit engine can Navigate before
// every fetch while paying the navigation cost only once per page.
func (s *session) Navigate(ctx context.Context, rawURL string) error {
	if s.current != "" && (rawURL == s.current || rawURL == s.requested) {
		return nil // page already here — reuse it
	}
	prev := s.current
	// Cleared until the navigation settles: a Navigate that fails partway may
	// have moved the page, so the old URL must not no-op a later re-Navigate.
	s.current, s.requested = "", ""
	if err := s.driver.navigate(ctx, rawURL); err != nil {
		return fmt.Errorf("lightpanda navigate %q: %w", rawURL, err)
	}
	settled, err := s.waitReady(ctx, prev)
	if err != nil {
		return err
	}
	s.current, s.requested = settled, rawURL
	return nil
}

// waitReady polls until the page has finished loading AND its URL has stopped
// changing (two consecutive identical URLs), and returns the URL it settled on.
// The URL-stability check is what clears a bot-wall challenge that redirects: we
// only return once the redirects have settled, so the first Eval runs on the
// real, cleared page — not the interstitial.
//
// preNav is the URL the page was on before this navigation. Lightpanda keeps
// the old document in place when a navigation fails (about:blank on a fresh
// target), so the pre-navigation URL never counts as settled — otherwise a
// failed navigation would be reported as success against the wrong page.
func (s *session) waitReady(ctx context.Context, preNav string) (string, error) {
	deadline := time.Now().Add(s.readyTimeout)
	var lastURL string
	var lastErr error
	stable := 0
	for {
		// Probe before the first sleep: Lightpanda's Page.navigate blocks until
		// the page has loaded, so it is often already settled on entry.
		ready, curURL, err := s.driver.probe(ctx)
		switch {
		case errors.Is(err, errConnLost):
			return "", err // dead browser — no amount of polling brings it back
		case err != nil:
			lastErr = err // transient during a redirect; keep polling until the deadline
		case !ready, curURL == preNav, curURL == "about:blank":
			stable = 0 // not loaded, or still the pre-navigation document
		case curURL == lastURL:
			if stable++; stable >= 2 {
				return curURL, nil
			}
		default:
			lastURL, stable = curURL, 1
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(s.pollInterval):
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("lightpanda: page not ready within %s (last url %q, last probe err %v)", s.readyTimeout, lastURL, lastErr)
		}
	}
}

// Eval runs js (an async function body) against the live page and returns its
// resolved value. js is wrapped in an async IIFE so its `await`/`return` work
// under Runtime.evaluate, and the returned promise is awaited.
func (s *session) Eval(ctx context.Context, js string) (string, error) {
	out, err := s.driver.evaluate(ctx, wrapAsync(js))
	if err != nil {
		return "", fmt.Errorf("lightpanda eval: %w", err)
	}
	return out, nil
}

// wrapAsync wraps a function body (statements ending in `return`) in an async
// IIFE, yielding an expression whose value is the returned promise — the shape
// Runtime.evaluate with awaitPromise needs.
func wrapAsync(js string) string { return "(async () => { " + js + " })()" }

// readyProbeExpr reads document readiness and the current URL in one evaluate.
const readyProbeExpr = `({ready: document.readyState === 'complete', url: location.href})`

// chromedpDriver is the real pageDriver. chromedp binds the browser target's
// lifetime to the context of the FIRST Run, so run() performs that first Run on
// the long-lived taskCtx once; every op then runs on a short-lived child of
// taskCtx carrying its own deadline, which bounds the op (a stalled server
// fails within the budget instead of hanging until the crawl times out) and can
// be cancelled by the caller's context without tearing the session down.
type chromedpDriver struct {
	taskCtx      context.Context
	cancel       context.CancelFunc
	readyTimeout time.Duration // per navigate/probe op budget
	bind         sync.Once
	bindErr      error
}

// run executes one CDP action bounded by opTimeout and by the caller's ctx,
// and classifies the failure: the caller's own cancellation/deadline surfaces
// as that ctx's error (the Reddit engine must NOT fall back on it), while a
// context error of our own plumbing means the op overran its budget or the CDP
// connection died — session faults the fallback should absorb.
func (d *chromedpDriver) run(ctx context.Context, opTimeout time.Duration, action chromedp.Action) error {
	d.bind.Do(func() { d.bindErr = chromedp.Run(d.taskCtx) })
	if err := d.bindErr; err != nil {
		return d.classify(ctx, opTimeout, err)
	}
	opCtx, cancel := context.WithTimeout(d.taskCtx, opTimeout)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel) // caller cancellation aborts the op, not the session
	defer stop()
	if err := chromedp.Run(opCtx, action); err != nil {
		return d.classify(ctx, opTimeout, err)
	}
	return nil
}

func (d *chromedpDriver) classify(ctx context.Context, opTimeout time.Duration, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("lightpanda: op exceeded %s: %v", opTimeout, err)
	case errors.Is(err, context.Canceled):
		// chromedp surfaces a crashed browser / dropped websocket as its own
		// cancellation of the task context.
		return fmt.Errorf("%w: %v", errConnLost, err)
	}
	return err
}

func (d *chromedpDriver) navigate(ctx context.Context, url string) error {
	// Raw Page.navigate (not chromedp.Navigate): we must NOT wait on the load
	// event here. Reddit's bot-wall JS challenge re-navigates the page, which
	// makes chromedp's load-event wait error out ("page load error"); the session
	// waits for the page to settle itself via probe.
	return d.run(ctx, d.readyTimeout, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, errText, _, err := page.Navigate(url).Do(ctx)
		if err != nil {
			return err
		}
		if errText != "" {
			// A network-level navigation failure (DNS, unreachable, fetch timeout)
			// replies err == nil with errorText set — it must not look like success.
			return fmt.Errorf("navigation failed: %s", errText)
		}
		return nil
	}))
}

func (d *chromedpDriver) probe(ctx context.Context) (bool, string, error) {
	var st struct {
		Ready bool   `json:"ready"`
		URL   string `json:"url"`
	}
	if err := d.run(ctx, d.readyTimeout, chromedp.Evaluate(readyProbeExpr, &st)); err != nil {
		return false, "", err
	}
	return st.Ready, st.URL, nil
}

func (d *chromedpDriver) evaluate(ctx context.Context, expr string) (string, error) {
	var out string
	err := d.run(ctx, evalTimeout, chromedp.Evaluate(expr, &out,
		func(p *runtime.EvaluateParams) *runtime.EvaluateParams {
			return p.WithAwaitPromise(true).WithReturnByValue(true)
		}))
	return out, err
}

func (d *chromedpDriver) close() {
	d.cancel()
}
