# ADR 0001: Reading Reddit after the logged-out JSON shutdown

- **Status:** accepted
- **Date:** 2026-10-15
- **Scope:** the Reddit engine (`internal/engine/reddit`)

## Context

The Reddit engine read every thread and listing from Reddit's `.json` endpoints. It
fetched them from inside a headless browser through crawl4ai, because Reddit's edge
blocks non-browser HTTP clients that are logged out.

On 2026-05-28 Reddit announced on r/modnews: "We'll also be shutting down
unauthenticated .json endpoints ... Logged-in and authenticated access won't be
impacted." Enforcement reached our deployment later. The engine worked for 117
fetches from 2026-08-20 to 2026-10-08, with no failure. From 2026-10-15 every
`.json` fetch failed with `403` and a "You've been blocked by network security" page.

On 2026-09-30 a Reddit admin posted the end of the public Data API on r/redditdev:

| Date | What happens |
|---|---|
| 2026-10-31 | New requests for public API access are no longer accepted. |
| 2027-01-12 | API access starts to close for apps and users that have not registered. |
| March 2027 | Public API access closes for all remaining apps and users. |

The replacement is the Developer Platform (Devvit), where apps run inside Reddit.
It is not a data API an outside agent can call. RSS feeds end on 2026-11-13.

### What causes the 403

The block is about the login state. The IP address, the ISP and the browser fingerprint
do not cause it. Measured on 2026-10-15 from a residential IP:

| Client | Logged out | Logged in |
|---|---|---|
| curl, Go `net/http` | 403 | 200 |
| Headless Chromium, default user agent (`HeadlessChrome`) | 403 | 403 |
| Headless Chromium, normal Chrome user agent | 403 | 200 |
| Real Chrome (headful) | 403 | 200 |

A logged-out browser still gets the HTML thread page (200), and only `.json` is
refused. Stealth flags, a matching user agent and real plugins did not change the
logged-out result.

## Decision

The engine reads Reddit through a **chain of methods**, tried in a configurable order
(`OMNIFEED_REDDIT_METHODS`, default `client_id,session,browser`). A method without
its credential is left out.

| Method | Credential | How it reads | Budget |
|---|---|---|---|
| `client_id` | `OMNIFEED_REDDIT_CLIENT_ID`: an OAuth app's client ID, installed-client grant, no secret | app-only token, then `oauth.reddit.com` JSON | 1000 requests per window |
| `session` | `OMNIFEED_REDDIT_SESSION`: a logged-in `reddit_session` cookie | `www.reddit.com` JSON over plain HTTP | 100 requests per window, per account |
| `browser` | none, or a session held by crawl4ai | the rendered thread page through crawl4ai, read from the DOM | the IP's logged-out budget |

The two direct methods use plain HTTP. One request returns the whole thread
(`limit=500&depth=20`) with live scores.

Failure handling:

- **Failover:** a failure that the next method can get around moves the call to that
  method at once. These failures are `403`, `429`, a challenge page, a block page,
  and an upstream `5xx`. A missing thread, or omnifeed's own quota refusal, does not
  move on, because every method would answer the same.
- **Cooldown:** the failed method is then skipped for `OMNIFEED_REDDIT_METHOD_COOLDOWN`
  (default `10m`). A failure therefore costs one request per pod per cooldown, not one
  per fetch.
- **Back-off:** Reddit's back-off (`Retry-After`, `X-Ratelimit-Remaining: 0`) holds only
  the method that got it. It never holds the shared crawl limiter, because a hold there
  would stall a working credential.
- **All methods down:** when every method is cooling down, the call fails at once with
  `quota_exhausted` and the shortest wait. It does not wait, and it does not retry a
  method that asked for back-off.
- **Expansion:** `/api/morechildren` runs on the direct methods only. Logged out,
  Reddit refuses it with `403`, and that `403` would hold `browser`, the one
  logged-out path that still reads threads. If the expansion fails, the engine keeps
  the partial thread.
- **Transient 403:** a direct method retries one `403` after 2 seconds. In a 14-thread
  run, one `403` came between successes, and the same URL answered `200` seconds later.

Where the skip state lives:

- **Without Redis:** in each process's memory. After a restart, a pod pays one
  failure again.
- **With `OMNIFEED_REDIS_URL` set:** in Redis too, so a hold from one replica skips
  the method on every replica. Reddit's budget belongs to the credential, not to
  the pod. Without a shared hold, the other pods keep spending a budget Reddit asked
  them to rest, and a `429` during back-off can extend the block.
  - A hold is one key with a TTL, on Redis' clock. A shorter hold never cuts a
    longer one.
  - A success through a method that was down deletes the key.
  - If Redis fails, each pod uses its own memory for 30 seconds, then tries Redis
    again. A call never fails because of Redis.
  - The shared `browser` hold assumes that the replicas share one egress IP. If each
    pod has its own IP, a block on one pod also stops the others.

### Monitoring

- **`omnifeed_reddit_method_up{method}`:** `1` while a method is in use, `0` while
  it is skipped. It returns to `1` only after a call through the method succeeds.
- **`omnifeed_reddit_ratelimit_remaining{method}`:** the requests left in Reddit's
  window, from each direct answer.
- **`omnifeed_ratelimit_degraded{scope="reddit_methods"}`:** `1` while Redis is
  unreachable and each pod keeps its own holds. It exists only with Redis.
- **`omnifeed_reddit_method_calls_total{method,outcome}`:** `outcome` is `ok`,
  `error`, or the failure kind that moved the call on.
- **`omnifeed_reddit_session_expiry_timestamp_seconds`:** when the cookie expires.
  omnifeed also logs a warning at startup when fewer than 14 days are left.

Alert on:

- a method that stays down for 20 minutes;
- every direct method down, which leaves only the slow browser;
- blocked fetches piling up;
- the cookie expiring within 14 days.

## Options measured

All on 2026-10-15, on two reference threads: r/kubernetes `1x0wbf3` (72 comments) and
r/ChatGPT `1x66iap` (about 290). The reference tree is a logged-in `.json` fetch.

| Option | Time per thread | Comments (large thread) | Nesting and scores | Result |
|---|---|---|---|---|
| `client_id`, live in omnifeed | p50 2.9 s, p90 3.9 s | 291 of 297 | full depth, live | 14/14 ok. Chosen first |
| `session`, live in omnifeed | p50 1.4 s, p90 2.8 s | 282 of 287 | full depth, live | 13/14 ok, the miss was transient. Chosen second |
| `browser`, rendered page plus in-page expansion | 7.5 to 31 s | 261 (93%) | 12 empty bodies (media replies) | Works logged out, but each thread costs 1 page load plus up to 40 expansion requests, and Reddit answers `429` after about 2 large threads. Chosen last |
| crawl4ai holding the cookie (server config) | 2.6 s | full tree | live | Works. crawl4ai refuses per-request cookies ("not permitted on BrowserConfig from an untrusted request"), so the cookie must sit in its server config |
| Logged-in headful Chrome kept open (CDP) | 0.6 to 2.8 s | full tree | live | 5/5 ok. A browser adds nothing over the plain-HTTP cookie |
| Arctic Shift archive API | about 1 s | 236 (84%), post missing | 90 orphan comments, scores frozen at archive time | No Reddit load, but incomplete and stale. Not used |
| Redlib public instances | 1.1 to 2.3 s | full (small thread) | live | 2 of 5 instances answered, the others `403` or `429`. Not dependable |

For the large thread, the logged-out HTML path returned 261 comments in one fetch.
Expanding the `.json` tree fully took several `/api/morechildren` calls. A direct
method returns 282 to 291 comments in one call.

## Options rejected

| Option | Why |
|---|---|
| Logged-out `.json` with a better fingerprint (stealth, real user agent, headful) | Still `403`: the block is about the login state |
| Arriving on Reddit from a DuckDuckGo result to get an unlocking cookie (a published agent skill) | Landed on the real thread, then both `.json` requests got `403` |
| An empty or fake `reddit_session` cookie (an old trick posted on X) | `403` for an empty value, a junk value and a fake token |
| PullPush API | Returns empty data |
| RSS | No scores, no comment tree, and it ends on 2026-11-13 |
| Public OAuth API for new apps | Needs Reddit's approval, requests close on 2026-10-31, and the API closes in March 2027 |
| Spoofing the official Android app's API client | Terms-of-service gray zone, and no better than `client_id` |
| Logged-out old.reddit | Login wall |
| Paid Reddit data APIs | Cost money, and the methods above work |

## Agent tooling surveyed

| Tool | Access method | Result when run |
|---|---|---|
| reddit-mcp-buddy | OAuth app, else RSS | Anonymous mode: listings with every score `null`, thread details refused |
| jordanburke/reddit-mcp-server | OAuth app, else RSS | Comments need OAuth credentials |
| eliasbiondo/reddit-mcp-server | logged-out `.json` | Every call fails |
| hawstein/mcp-server-reddit | redditwarp, installed-client grant | Crashes on start (SDK and pydantic versions). Its library's grant is what revealed the `client_id` method |
| dialog.tools hosted MCP | hosted, PRAW behind it | Needs an account |
| rdt-cli (agent skill) | logged-in browser cookies | Same mechanism as `session` |

None of the Reddit MCP servers or skills surveyed uses a session cookie over plain HTTP.

## Consequences

- **Speed and quality:** a thread is one request again, with every comment, full nesting
  and live scores.
- **`client_id` has an end date:** Reddit closes the public API in March 2027, and an
  unregistered app can be cut off from 2027-01-12. New app approvals ended on
  2026-10-31. Using a public installed-client ID that another project ships works, but
  it is a terms-of-service gray zone, and Reddit can revoke that ID.
- **`session` needs a human about every 6 months:**
  - The cookie is a signed token with a fixed expiry, 181 days after login.
  - Reddit does not renew it on use.
  - A server cannot log in by itself without fighting the login form's bot checks,
    so a human logs in again before the expiry.
  - The fetches run as that account, so use a secondary account.
- **`browser` is a degraded fallback:**
  - It is slow, and it hits rate limits quickly.
  - A listing is read from the rendered page. The default card view rendered 3
    posts for r/kubernetes top-of-week on 2026-10-16. The compact view
    (`feedViewType=compactView`), which omnifeed uses, renders 34, and each
    scroll to the bottom loads 25 more. When the caller asks for more than the
    page holds, omnifeed scrolls: 100 posts took about 11s per listing
    (2026-10-16), against about 4s for the default 25, which needs no scroll.
    Each batch is one more request to Reddit that the quota does not count,
    so 100 posts cost 1+4 requests. A batch Reddit refuses, or the 15s cap,
    marks the listing partial (timings: `listingHTMLJS` in `html.go`).
  - It handles comment replies below depth 4 by following their permalink pages,
    inside the same request cap.
- **The output format is unchanged:** every method produces omnifeed's existing
  Reddit schema, so clients do not change.
- **Reddit can change the rules:** it said logged-in access "won't be impacted", but
  that is Reddit's choice to keep.

## Operations

To refresh the session cookie:

1. Log in to Reddit in a browser profile kept for this purpose.
2. Copy the `reddit_session` cookie value. It is `HttpOnly`, so use the browser's
   developer tools or its DevTools protocol.
3. Set `OMNIFEED_REDDIT_SESSION` to that value and restart omnifeed.
4. Check that `omnifeed_reddit_session_expiry_timestamp_seconds` moved about 6
   months ahead.

Keep the cookie out of logs and version control. It gives full access to the account.

## Sources

- r/modnews, 2026-05-28: "Protecting communities from scrapers and platform abuse"
  (the `.json` shutdown).
- r/redditdev, 2026-09-30, Reddit admin post: "Moving Data API Apps to the Developer
  Platform: migration dates and next steps" (the API end dates).
- Measurements: the omnifeed test stack and direct probes, 2026-10-15.
