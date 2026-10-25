# ADR 0002: Waiting out Cloudflare challenges with a CSS selector

- **Status:** proposed (PR #72, not merged)
- **Date:** 2026-10-25
- **Scope:** the generic crawl4ai engine (`internal/engine/crawl4ai`), its challenge
  wait (`OMNIFEED_CRAWL4AI_CHALLENGE_WAIT`) and default user agent
  (`OMNIFEED_CRAWL4AI_USER_AGENT`)

## Context

A Cloudflare challenge page ("Just a moment...") often clears by itself after a few
seconds, then navigates to the real page. So every non-PDF `/crawl` sent crawl4ai a
`wait_for` that held extraction until the challenge was gone. The wait was JavaScript:

```
js:() => !/^\s*(just a moment|un instant|...)\s*$/i.test(document.title)
```

crawl4ai 0.9.5 is a security release, and it refuses that wait.

- **The advisory:** GHSA-qj46-wp7m-rgj8. `wait_for: "js:..."` reached `page.evaluate()`,
  and so did `css_selector`, through string interpolation. Any holder of a crawl4ai
  token could run any JavaScript in the crawl page. That got around the 0.9.0 ban on
  `js_code`.
- **The fix:** since 0.9.5, a request body is untrusted. A `wait_for` that starts with
  `js:`, `()` or `function` gets HTTP 400 ("a JavaScript wait_for is not permitted from
  an untrusted request"). Any other value is forced to `css:` and never falls back to
  JavaScript.
- **The effect on omnifeed:** every non-PDF crawl would fail with 400 on 0.9.5.

### Upstream will not bring it back

- The Docker server hardcodes `Provenance.UNTRUSTED` at every `/crawl` call site.
  There is no trusted-token mode for a request.
- No issue or plan re-allows a JavaScript wait from a request body (none found when
  PR #72 was written).
- Upstream adds operator knobs instead, set in env vars or `config.yml`: for example
  `CRAWL4AI_MAX_TIMEOUT_MS` (issue #2211), and `/execute_js` stays behind
  `CRAWL4AI_EXECUTE_JS_ENABLED`. A JavaScript wait is the operator's decision, never
  the caller's.

## Decision

The challenge wait is a **CSS selector**, the form 0.9.5 accepts by design:

```
css::root:not(:has(script[src*="/cdn-cgi/challenge-platform/"][src*="/orchestrate/"])) :visible
```

- **What it matches:** a Cloudflare challenge page loads its solver from
  `/cdn-cgi/challenge-platform/.../orchestrate/...` (checked on live Glassdoor and
  Indeed challenge pages). A normal page behind Cloudflare loads only
  `/cdn-cgi/challenge-platform/scripts/jsd/main.js`, so it matches on the first poll and
  pays nothing. Once the challenge clears, Cloudflare navigates away and the script is
  gone.
- **`:visible`:** crawl4ai hands the selector to Playwright, which waits for the *first*
  match to be visible. Without the trailing ` :visible` (`:root:not(...)` alone), the
  match is `<html>` itself, and a page whose `<html>` has zero height (all its content
  fixed or absolute) waits out the whole timeout. Reproduced before the fix.
- **`:root`, not `html`:** a URL that renders as a bare SVG document has no `<html>`.
  With `html`, it would wait out the timeout and then fail as a challenge.
- **A wait that runs out is not a verdict.** A page with no visible element (an empty
  200, a slow SPA, an extensionless XML feed) runs it out too, and crawl4ai returns no
  page with "Wait condition failed: Timeout". So the engine crawls once more without the
  wait (the `no_wait` rung), and that page decides. Only a page that still loads the
  `/orchestrate/` solver or carries a challenge title ("Just a moment...") is a
  `captcha`. Anything else goes down the rest of the ladder without the wait, so an XML
  feed still reaches the direct-GET rescue. The engine recognizes the timeout by
  crawl4ai's message alone, which holds while this is its only `wait_for`
  (`docs/ideas.md` has the follow-up).
- **A Cloudflare challenge skips the retry under crawl4ai's default identity.**
  Cloudflare rejects that identity, so the retry would only add a second wait. Other
  walls (Akamai, AWS WAF) keep it.
- **It also catches challenges with a site's own title** ("Security | Glassdoor"),
  which the title test missed.

### Rollout order

The selector works on crawl4ai 0.9.4 as well as 0.9.5, so:

1. Release omnifeed with the selector, on the crawl4ai you run today.
2. Then move crawl4ai to 0.9.5.

The other way round, every non-PDF crawl fails with 400 between the two steps. The
quickstart stacks (`docker-compose.yml`, `scripts/container`) move to 0.9.5 in the same
PR, because they always run the omnifeed from the same checkout.

### Default user agent: Chrome 156

The same PR moves the default `OMNIFEED_CRAWL4AI_USER_AGENT` from Chrome 153 to Chrome
156, the Chromium bundled in crawl4ai 0.9.5 (Playwright 1.64). A user agent that
disagrees with the browser engine is itself a bot signal, so the default follows the
image (`DefaultCrawl4AIUserAgent` in `internal/config/config.go`).

## Options rejected

| Option | Why |
|---|---|
| Turn on crawl4ai's JavaScript hooks (`CRAWL4AI_HOOKS_ENABLED`) | A far bigger surface than one wait: any caller could run code in the browser |
| Set `crawler.base_config.wait_for` in the crawl4ai server config | It works and is trusted, but every consumer of that crawl4ai server gets the wait, and a request cannot turn it off |
| Detect the challenge in the returned page and crawl again, instead of waiting | Each retry starts a fresh challenge, so it rarely clears, and each try costs a full page load. The `no_wait` rung crawls again only to read the page, not to clear the challenge |
| Stay on crawl4ai 0.9.4 | Keeps the token-holder-runs-JavaScript hole open |

## Measurements

On 2026-10-25, against `unclecode/crawl4ai:0.9.5` and `0.9.4`, with the repo's
`crawl4ai/config.yml`:

| Case | Result |
|---|---|
| The old JavaScript wait | HTTP 400 on 0.9.5 |
| go.dev, Wikipedia, Hacker News, crunchbase (Cloudflare with `jsd`), upwork, indeed, glassdoor | success, the same latency as with no wait |
| A simulated challenge that navigates away after 2.5 s | waits, then returns the real page (about 3 s) |
| A simulated challenge that never clears | `Wait condition failed: Timeout after ...`. The `no_wait` rung and its `captcha` verdict came after this run, from review; unit tests cover them, not a live run |
| A page with a zero-height `<html>` | success (it timed out without `:visible`) |

## Consequences

- omnifeed runs on crawl4ai 0.9.5 and keeps its challenge wait, with no change in
  crawl4ai's server config.
- **The wait is tied to Cloudflare's markup.** If Cloudflare renames the
  `/orchestrate/` path, the selector matches at once on a challenge page, and the page
  comes back as a challenge without the wait: blocked, as it was before there was any
  wait. Other vendors' challenges were never covered by this wait.
- **A challenge that never clears costs the wait plus one crawl without it**, and an
  empty page or an XML feed costs the same before it is handled as usual. Before the
  review fix, both cost two waits (the default-identity retry) and the empty page ended
  as a `captcha`.
- **The user agent must follow crawl4ai's Chromium.** Each crawl4ai bump that changes
  the bundled Chromium needs a matching bump of `DefaultCrawl4AIUserAgent`. Between
  step 1 and step 2 above, the default claims Chrome 156 on 0.9.4's Chromium 153. A
  deployment that wants no such window can set `OMNIFEED_CRAWL4AI_USER_AGENT` until it
  moves crawl4ai.
- If crawl4ai ever offers a trusted way to send a JavaScript wait, this ADR is worth
  revisiting. The selector would still be the smaller surface.

## Sources

- GHSA-qj46-wp7m-rgj8:
  <https://github.com/unclecode/crawl4ai/security/advisories/GHSA-qj46-wp7m-rgj8>
- crawl4ai 0.9.5 changelog:
  <https://github.com/unclecode/crawl4ai/blob/v0.9.5/CHANGELOG.md>
- crawl4ai issue #2211 (`CRAWL4AI_MAX_TIMEOUT_MS`):
  <https://github.com/unclecode/crawl4ai/issues/2211>
