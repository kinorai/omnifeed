package lightpanda

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeDriver scripts the low-level CDP surface so the session's own logic (nav
// reuse, the URL-stability readiness machine, JS wrapping) is tested without a
// live browser.
type fakeDriver struct {
	navCalls  []string      // URLs passed to navigate
	navErr    error         // returned by navigate
	probes    []probeResult // consumed one per probe() call; last repeats
	probeIdx  int           //
	evalReqs  []string      // exprs passed to evaluate
	evalReply func(string) (string, error)
	closed    bool
}

type probeResult struct {
	ready bool
	url   string
	err   error
}

func (d *fakeDriver) navigate(_ context.Context, url string) error {
	d.navCalls = append(d.navCalls, url)
	return d.navErr
}

func (d *fakeDriver) probe(context.Context) (bool, string, error) {
	if len(d.probes) == 0 {
		return true, "about:blank", nil
	}
	i := d.probeIdx
	if i >= len(d.probes) {
		i = len(d.probes) - 1 // last result repeats
	}
	d.probeIdx++
	p := d.probes[i]
	return p.ready, p.url, p.err
}

func (d *fakeDriver) evaluate(_ context.Context, expr string) (string, error) {
	d.evalReqs = append(d.evalReqs, expr)
	if d.evalReply != nil {
		return d.evalReply(expr)
	}
	return "", nil
}

func (d *fakeDriver) close() { d.closed = true }

func newSession(d *fakeDriver) *session {
	return &session{driver: d, readyTimeout: time.Second, pollInterval: time.Millisecond}
}

// A page that redirects (the bot-wall challenge) then settles must be reported
// ready only once the URL has stopped changing across two consecutive probes.
func TestWaitReadyURLStability(t *testing.T) {
	d := &fakeDriver{probes: []probeResult{
		{ready: false, url: "https://r/x"},            // still loading
		{ready: true, url: "https://r/x?challenge=1"}, // challenge page, ready but transient
		{ready: true, url: "https://r/x?solution=ab"}, // redirected again
		{ready: true, url: "https://r/x?solution=ab"}, // stable — settle here
	}}
	s := newSession(d)
	if err := s.Navigate(context.Background(), "https://r/x"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	// 4 probes consumed to reach two identical ready URLs.
	if d.probeIdx != 4 {
		t.Errorf("expected 4 probes to settle, got %d", d.probeIdx)
	}
}

// Transient probe errors during a redirect must not abort — waitReady keeps
// polling until the page settles.
func TestWaitReadyToleratesProbeErrors(t *testing.T) {
	d := &fakeDriver{probes: []probeResult{
		{err: context.DeadlineExceeded}, // transient
		{ready: true, url: "https://r/x"},
		{ready: true, url: "https://r/x"},
	}}
	s := newSession(d)
	if err := s.Navigate(context.Background(), "https://r/x"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
}

// A page that never settles must fail with a not-ready error before the deadline
// — that error is fallback-worthy, so the crawl retries on crawl4ai.
func TestWaitReadyTimeout(t *testing.T) {
	d := &fakeDriver{probes: []probeResult{{ready: false, url: "https://r/x"}}}
	s := &session{driver: d, readyTimeout: 20 * time.Millisecond, pollInterval: time.Millisecond}
	err := s.Navigate(context.Background(), "https://r/x")
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("want not-ready error, got %v", err)
	}
}

// Navigating to the URL the session is already on must be a no-op — this is what
// gives the live-page backend its session-reuse win across morechildren rounds.
func TestNavigateReusesLivePage(t *testing.T) {
	d := &fakeDriver{probes: []probeResult{
		{ready: true, url: "https://r/x"}, {ready: true, url: "https://r/x"},
		{ready: true, url: "https://r/y"}, {ready: true, url: "https://r/y"},
	}}
	s := newSession(d)
	if err := s.Navigate(context.Background(), "https://r/x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Navigate(context.Background(), "https://r/x"); err != nil {
		t.Fatal(err)
	}
	if len(d.navCalls) != 1 {
		t.Fatalf("second Navigate to the same URL must be a no-op; navCalls = %v", d.navCalls)
	}
	// A different URL does navigate.
	if err := s.Navigate(context.Background(), "https://r/y"); err != nil {
		t.Fatal(err)
	}
	if len(d.navCalls) != 2 {
		t.Fatalf("Navigate to a new URL must drive a navigation; navCalls = %v", d.navCalls)
	}
}

// Navigate must record the URL the page SETTLED on (post-redirect), and treat
// both the requested and the settled URL as "already here" — a share-link crawl
// that settles on the canonical /comments/ page must reuse the live page when
// the engine then navigates to that canonical URL.
func TestNavigateRecordsSettledURL(t *testing.T) {
	d := &fakeDriver{probes: []probeResult{
		{ready: true, url: "https://r/canonical"}, // share URL redirected here
		{ready: true, url: "https://r/canonical"},
	}}
	s := newSession(d)
	if err := s.Navigate(context.Background(), "https://r/share"); err != nil {
		t.Fatal(err)
	}
	if s.current != "https://r/canonical" {
		t.Fatalf("s.current = %q, want the settled URL", s.current)
	}
	// Both the settled URL and the originally requested one no-op.
	if err := s.Navigate(context.Background(), "https://r/canonical"); err != nil {
		t.Fatal(err)
	}
	if err := s.Navigate(context.Background(), "https://r/share"); err != nil {
		t.Fatal(err)
	}
	if len(d.navCalls) != 1 {
		t.Fatalf("settled/requested URLs must reuse the live page; navCalls = %v", d.navCalls)
	}
}

// The pre-navigation document must never count as settled: Lightpanda keeps
// the old page (about:blank on a fresh target) in place when a navigation
// fails, and settling on it would report success against the wrong page.
func TestWaitReadyIgnoresPreNavigationPage(t *testing.T) {
	// Fresh session: probes keep showing a ready about:blank, then the real page.
	d := &fakeDriver{probes: []probeResult{
		{ready: true, url: "about:blank"},
		{ready: true, url: "about:blank"},
		{ready: true, url: "https://r/x"},
		{ready: true, url: "https://r/x"},
	}}
	s := newSession(d)
	if err := s.Navigate(context.Background(), "https://r/x"); err != nil {
		t.Fatalf("Navigate: %v", err)
	}
	if d.probeIdx != 4 {
		t.Errorf("about:blank probes must not settle; consumed %d probes, want 4", d.probeIdx)
	}

	// Reused session: a nav away from the current page that never commits (the
	// probes keep showing the OLD page) must time out, not succeed.
	d2 := &fakeDriver{probes: []probeResult{{ready: true, url: "https://r/old"}, {ready: true, url: "https://r/old"}}}
	s2 := &session{driver: d2, readyTimeout: 20 * time.Millisecond, pollInterval: time.Millisecond}
	if err := s2.Navigate(context.Background(), "https://r/old"); err != nil {
		t.Fatal(err)
	}
	err := s2.Navigate(context.Background(), "https://r/new")
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("nav that stays on the old page must time out, got %v", err)
	}
	// And the failure must clear reuse: the old URL re-navigates for real.
	if err := s2.Navigate(context.Background(), "https://r/old"); err != nil {
		t.Fatal(err)
	}
	if len(d2.navCalls) != 3 {
		t.Fatalf("failed Navigate must clear live-page reuse; navCalls = %v", d2.navCalls)
	}
}

// A dead CDP connection surfaced by a probe must abort waitReady immediately —
// polling through it would stall the crawl for the full ready budget.
func TestWaitReadyAbortsOnConnLost(t *testing.T) {
	d := &fakeDriver{probes: []probeResult{{err: errConnLost}}}
	s := newSession(d)
	err := s.Navigate(context.Background(), "https://r/x")
	if !errors.Is(err, errConnLost) {
		t.Fatalf("want errConnLost, got %v", err)
	}
	if d.probeIdx != 1 {
		t.Errorf("must abort on the first conn-lost probe; consumed %d", d.probeIdx)
	}
}

// Eval wraps the caller's function body in an async IIFE so `await`/`return`
// work under Runtime.evaluate, and returns the driver's value verbatim.
func TestEvalWrapsAndReturns(t *testing.T) {
	d := &fakeDriver{evalReply: func(expr string) (string, error) { return expr, nil }}
	s := newSession(d)
	out, err := s.Eval(context.Background(), `const r = await fetch(u); return r.text();`)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if !strings.HasPrefix(out, "(async () => { ") || !strings.HasSuffix(out, " })()") {
		t.Errorf("Eval did not wrap the body in an async IIFE: %q", out)
	}
	if !strings.Contains(out, "await fetch(u)") {
		t.Errorf("Eval dropped the caller body: %q", out)
	}
}

func TestNavigateSurfacesNavError(t *testing.T) {
	d := &fakeDriver{navErr: context.Canceled}
	s := newSession(d)
	if err := s.Navigate(context.Background(), "https://r/x"); err == nil {
		t.Fatal("expected navigate error to surface")
	}
}

func TestCloseTearsDown(t *testing.T) {
	d := &fakeDriver{}
	s := newSession(d)
	_ = s.Close(context.Background())
	if !d.closed {
		t.Fatal("Close must tear down the driver")
	}
}

func TestWrapAsync(t *testing.T) {
	if got := wrapAsync("return 1;"); got != "(async () => { return 1; })()" {
		t.Errorf("wrapAsync = %q", got)
	}
}
