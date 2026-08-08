// Package browser defines the port an engine uses to drive a real headless
// browser: navigate to a page, then run same-origin JavaScript against it. It
// exists so the Reddit engine (and any future engine that must clear a
// browser-only bot wall) can run on more than one backend without knowing which.
//
// Two backends implement it:
//
//   - browser/crawl4ai — drives crawl4ai's /execute_js endpoint. Navigate only
//     records the target; each Eval re-navigates there (crawl4ai has no session
//     reuse). This is the default and the fallback.
//   - browser/lightpanda — drives a Lightpanda CDP server. Navigate performs a
//     real navigation once; subsequent Evals against the same URL reuse the live
//     page, so a deep crawl's follow-up fetches skip re-navigation entirely.
//
// The two ports mirror domain's split: domain.Searcher answers "query → URLs",
// domain.Engine answers "URL → content", and browser.Browser is the transport an
// Engine reaches for when "URL → content" needs a real browser to get there.
package browser

import "context"

// Browser opens browsing sessions. Implementations are safe for concurrent use;
// each Open returns an independent Session.
type Browser interface {
	// Name identifies the backend in logs and metrics ("crawl4ai", "lightpanda").
	Name() string
	// Open starts a new session. The caller owns it and must Close it. The passed
	// context bounds session startup only, not the session's lifetime.
	Open(ctx context.Context) (Session, error)
}

// Session is a single browsing context — one page's worth of navigation and
// script execution. A Session is NOT safe for concurrent use; drive it from one
// goroutine at a time.
type Session interface {
	// Navigate points the session at rawURL. On a live-browser backend it performs
	// the navigation and waits for the page to settle; navigating to the URL the
	// session is already on is a no-op so follow-up Evals reuse the live page. On
	// the re-navigating crawl4ai backend it only records the target, and each
	// subsequent Eval navigates there first. It therefore makes no guarantee of a
	// round trip and returns no post-redirect URL — read location.href via Eval
	// when the redirect target is needed (e.g. resolving a share link).
	Navigate(ctx context.Context, rawURL string) error

	// Eval runs js against the session's current page and returns its resolved
	// value as a string. js is an async function body: it may use await and MUST
	// end in a `return` that yields the value (e.g. `return JSON.stringify(x);`).
	// Backends wrap it so `return` and `await` work — callers write the body only.
	Eval(ctx context.Context, js string) (string, error)

	// Close releases the session's resources (page, target, connection). Safe to
	// call once; the passed context bounds teardown.
	Close(ctx context.Context) error
}
