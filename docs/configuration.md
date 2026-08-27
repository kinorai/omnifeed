# Configuration

Everything is configured with `OMNIFEED_`-prefixed environment variables. In practice
you only ever **set three** — `OMNIFEED_API_KEY`, `OMNIFEED_CRAWL4AI_URL`, and
(optionally) `OMNIFEED_SEARXNG_URL`. The rest have sane defaults.

Every variable below is read by the omnifeed **binary**. The Apple `container`
launcher reads a few `OMNIFEED_`-prefixed variables of its own — container name
prefix, host ports, images — which the binary never sees. Those live in
[apple-container.md](apple-container.md).

**Egress.** Reddit and the generic fallback reach the web through crawl4ai, but four
engines call their upstream themselves: Hacker News reads `hn.algolia.com`, GitHub reads
`api.github.com`, Bluesky reads `public.api.bsky.app`, and Discourse reads each host in
`OMNIFEED_DISCOURSE_HOSTS`. A deployment that firewalls outbound traffic to crawl4ai
alone breaks those four.

| Variable | Default | Purpose |
|---|---|---|
| `OMNIFEED_API_KEY` | _(unset)_ | Bearer token for `/crawl`, `/search`, `/mcp`. If unset, the proxy refuses to start unless `OMNIFEED_DEV_NO_AUTH=true`. Stdio MCP is unaffected. |
| `OMNIFEED_CRAWL4AI_URL` | _(required)_ | Upstream crawl4ai endpoint. Reddit + the generic fallback fetch through it (the Hacker News engine reads `hn.algolia.com` directly); if empty, the proxy exits at startup. |
| `OMNIFEED_CRAWL4AI_TOKEN` | _(unset)_ | Bearer token sent to crawl4ai (its `CRAWL4AI_API_TOKEN`). Needed when the upstream enforces auth — crawl4ai binds beyond loopback **only** when a token is set. Unset sends no `Authorization` header. |
| `OMNIFEED_SEARXNG_URL` | _(unset)_ | Upstream SearXNG base URL (e.g. `http://searxng:8080`). When unset, `web_search` / `/search` are not exposed. The instance must enable the `json` format. |
| `OMNIFEED_DEV_NO_AUTH` | `false` | Run the HTTP transports with **no** auth when no key is set (local/dev only). Ignored if a key is set. |
| `OMNIFEED_LISTEN_ADDR` | `:8080` | HTTP listen address (`/crawl`, `/search`) |
| `OMNIFEED_MCP_LISTEN_ADDR` | `:8081` | MCP HTTP/SSE listen address |
| `OMNIFEED_MCP_STDIO` | `false` | Run MCP over stdio (also via `--mcp-stdio`) |
| `OMNIFEED_METRICS_ADDR` | `:9090` | Prometheus + health listen address |
| `OMNIFEED_CRAWL4AI_TIMEOUT` | `90s` | Per-call timeout to crawl4ai |
| `OMNIFEED_CRAWL4AI_KEEP_LINKS` | `true` | Keep hyperlink anchor text + external links in fetched markdown. Set `false` for leaner, link-stripped output (loses link-dense content like HN titles). |
| `OMNIFEED_CRAWL4AI_PRUNE_THRESHOLD` | `0.48` | PruningContentFilter cutoff (0–1) for the generic engine. Raise it to strip more boilerplate/duplicated chrome from noisy pages; lower it to keep more. |
| `OMNIFEED_CRAWL4AI_WAIT_UNTIL` | `domcontentloaded` | crawl4ai page-ready signal (`domcontentloaded` \| `load` \| `networkidle` \| `commit`). `domcontentloaded` fires before client-side frameworks hydrate, so JS-only SPAs can render empty; set `networkidle` to wait for them (slower on every page). |
| `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE` | `false` | Deployment default for scrolling the full page before extraction (crawl4ai `scan_full_page`). Benchmarked: it buys content **only** on append-style infinite-scroll feeds, costs ~3× latency on every page, and corrupts virtualized pages (an open crawl4ai bug) — prefer the per-request opt-in: the `fetch_url` tool's `scan_full_page` argument or `POST /crawl?scan_full_page=true`, which override this in either direction. |
| `OMNIFEED_CRAWL4AI_SCROLL_DELAY` | `0.5` | Pause (seconds) between scroll steps while scanning the full page. Only sent when the scan is on. |
| `OMNIFEED_CRAWL4AI_REMOVE_OVERLAYS` | `false` | Send crawl4ai's `remove_overlay_elements`. **Leave off**: its geometry heuristic deletes any large fixed/absolute-position element before extraction, which silently empties entire pages whose content lives in such containers (Wikipedia and several news fronts return only their `<title>`). Cookie/consent modals are already handled by `remove_consent_popups`, which stays on regardless. |
| `OMNIFEED_CRAWL4AI_DELAY_BEFORE_HTML` | `0.1` | Unconditional settle (seconds) after the page-ready signal before HTML extraction (crawl4ai `delay_before_return_html`) — paid on **every** crawl. Raise it (e.g. `1.0`) if pages that render content shortly after load come back thin. |
| `OMNIFEED_CRAWL4AI_EXCLUDED_SELECTOR` | `.sidebar,.toc,#toc,.related,.newsletter,.cookie-banner,[aria-label*='cookie']` | CSS selectors the generic engine drops before extraction (crawl4ai `excluded_selector`). The default names chrome-shaped classes only — sidebars, tables of contents, related/newsletter boxes, cookie banners — so it can't eat article body text. Set your own list to replace it (empty keeps the default; a selector that matches nothing effectively excludes nothing). If the exclusion empties a page (its main content *was* a `#toc`/`.sidebar`), the crawl automatically retries once without the selector. |
| `OMNIFEED_CRAWL4AI_TARGET_ELEMENTS` | _(unset — feature off)_ | Comma-separated CSS selectors; when non-empty the generic engine extracts markdown **only** from matching containers (crawl4ai `target_elements`). Powerful on article/repo pages but risky: a page without a match returns **empty content**, and the thin-content guard then surfaces an explicit error instead of the page. Validate against your own corpus before enabling. Suggested starting list: `article, main, [role=main], .markdown-body, .post-content, #content`. |
| `OMNIFEED_SEARXNG_TIMEOUT` | `15s` | Per-query timeout to SearXNG |
| `OMNIFEED_SEARXNG_SITE_ENGINES` | _(unset — whole pool)_ | Comma-separated SearXNG engine names to query **when the caller passes a `site` filter**, e.g. `privacywall,google cse`. SearXNG forwards `site:` to the engines rather than applying it itself, and engines differ: some honour it, some ignore the operator and answer with unrelated pages, and some return **zero results** for any `site:` query. On a mixed pool that makes a site-scoped search thin and noisy, so name the engines that implement the operator. Unset queries every engine, as before. Only site-scoped searches are affected — and only when the filter is valid, since an invalid `site` is dropped from the query. |
| `OMNIFEED_SEARXNG_DELAY` | `0` (off) | Minimum gap between queries sent to SearXNG. Paces the **engines behind** SearXNG, not SearXNG itself: one query fans out to every enabled engine, so each engine sees exactly omnifeed's query rate, and an engine that judges that rate bot-like suspends itself or serves a CAPTCHA. Pair with `OMNIFEED_SEARXNG_QUOTA` — a gap alone does not bound a burst. |
| `OMNIFEED_SEARXNG_QUOTA` | `0` (off) | Maximum queries admitted within any rolling `OMNIFEED_SEARXNG_QUOTA_WINDOW`. Needed because engines enforce two different limits: one measured pool kept answering at a 3s gap from a quiet start yet blocked after ~20 queries in ~85s, because it counts requests in a window. The delay shapes the gap, the quota bounds the burst. Measure both against your own pool. |
| `OMNIFEED_SEARXNG_QUOTA_WINDOW` | `90s` | Width of the rolling window for `OMNIFEED_SEARXNG_QUOTA`. Ignored when the quota is `0`; must be `> 0` when it is set. |
| `OMNIFEED_SEARXNG_CONCURRENCY` | `0` (off) | Maximum searches in flight to SearXNG across **every replica**. Needed because `OMNIFEED_SEARXNG_DELAY` cannot do it: a nonzero delay serializes admissions cluster-wide, so without this the whole deployment runs one search at a time however many replicas it has. Above `0`, the delay spaces sends instead and this becomes the bound, held in Redis as TTL leases so a pod that dies mid-search returns its slot instead of stranding it. Measured on one pool (2026-08-24): 16 concurrent ran clean, 24 blocked the strictest engine for the 1200s its suspension lasts — leave margin, because overshooting costs a 20-minute engine outage rather than a failed query. **Redis is not required.** A single instance is the whole deployment, so the cap is honoured in process and the delay spaces sends exactly as it does with Redis. `OMNIFEED_REDIS_URL` is what makes the number one shared budget instead of one per instance, so set it once you run more than one — without it, N instances send N times this. |
| `OMNIFEED_SEARXNG_MAX_WAIT` | `15s` | How long one query may sit in the pacing limiter before it gives up. This is the knob behind the `context deadline exceeded` a fanned-out caller sees: the queue was longer than this, not the upstream slower. Raise it to trade latency for completions. |
| `OMNIFEED_REDIS_URL` | _(unset — off)_ | Share the rate limiters' state through Redis (e.g. `redis://user:pass@redis:6379/2`, `rediss://` for TLS). The **single** switch for distributed pacing: unset, every replica paces in its own memory, so a deployment of N replicas sends up to N times the configured rate — the `OMNIFEED_SEARXNG_QUOTA` and `OMNIFEED_PER_DOMAIN_DELAY` limits each apply per pod. Set, the delay and the rolling-window quota are counted once for the whole deployment. Redis is never on the critical path: if it is unreachable the limiters fall back to in-process pacing (the unset behaviour) and crawls keep working. Per-pod concurrency stays per pod by design, so `OMNIFEED_PER_DOMAIN_CONCURRENCY` is still multiplied by the replica count. |
| `OMNIFEED_REDIS_KEY_PREFIX` | `omnifeed:ratelimit` | Namespace for the limiter keys, so several omnifeed deployments can share one Redis instance without counting each other's requests. Every key omnifeed writes carries a TTL, which matters on a shared instance running `noeviction`. |
| `OMNIFEED_REDIS_TIMEOUT` | `250ms` | Budget for **one Redis operation**, not for one acquisition — an acquisition legitimately sleeps for minutes while it waits out a quota window, and only the individual Redis calls inside it are bounded by this. On a breach the limiter reports its backend unavailable and paces in process for a 30s cooldown before probing again, so a dead Redis costs about one timeout per cooldown rather than one per request. |
| `OMNIFEED_SEARCH_AUDIT` | `off` | Per-search audit log: `off`, `summary` or `full`. **Not a log level** — a level says how bad something is, this says what the engine pool returned, and gating it behind `debug` would mean enabling every other component's debug output to get it (and would invite sampling, which biases the per-engine statistics it exists to produce). Both modes log at INFO. `summary` emits one line per search: `query_id`, query, `site`, `time_range`, `total`, `duration_ms`, and per-engine row counts, plus one line per unresponsive engine with `engine` and `reason_class` as separate fields. `full` adds one line per (engine, result) carrying that engine's **own** rank — the position table — joined to the summary by `query_id`. Flat lines on purpose: log stores that flatten JSON turn arrays into opaque strings, so a nested `positions: [1,4]` could never be grouped or averaged. **Both** modes write the query string; `full` additionally writes every result URL. It is opt-in and its retention is your decision. Emitted at INFO, so anything but `off` requires `OMNIFEED_LOG_LEVEL=debug` or `info` — a higher level would discard the feed silently, so startup refuses the combination. The `omnifeed_search_engine_position_rank` and `omnifeed_search_engine_unique_results_total` metrics carry no query text or URLs and are always recorded, whatever this is set to. |
| `OMNIFEED_SEARCH_MAX_RESULTS` | `25` | Hard cap on the search `limit` argument (1–100) |
| `OMNIFEED_FETCH_MAX_CHARS` | `120000` | Default character cap on **markdown** content returned by the `fetch_url` MCP tool (`0` = unlimited). Over the cap, the reply ends with a resumable truncation marker. TOON/JSON output (every dedicated engine: Reddit, Hacker News, GitHub, Discourse) is never cut — use the Reddit knobs below instead. Does not apply to `/crawl` (see [Controlling fetched content size](#controlling-fetched-content-size)). |
| `OMNIFEED_GITHUB_TOKEN` | _(unset)_ | Personal access token for the GitHub engine (issue / pull-request pages read from `api.github.com`). Unset means anonymous, which works but is limited to 60 requests/hour/IP; a token raises it to 5000/hour. |
| `OMNIFEED_DISCOURSE_HOSTS` | `meta.discourse.org,discuss.python.org,users.rust-lang.org,internals.rust-lang.org,discuss.pytorch.org` | Comma-separated hostnames the Discourse engine claims topic (`/t/…`) URLs on. Discourse is self-hosted software living on **arbitrary** domains and an engine cannot probe a host to find out, so the allowlist is explicit: **list the forums you actually use**. Matching is exact and case-insensitive (no subdomain wildcards); unlisted forums still work — they just go through the generic browser fallback, which returns less of the thread. Set to the **empty string** to disable the engine entirely. |
| `OMNIFEED_REDDIT_TIMEOUT` | `4m` | Wall-clock cap for a Reddit thread expansion |
| `OMNIFEED_REDDIT_MAX_ROUNDS` | `3` | Default `/api/morechildren` rounds (max 40 via `?expand=full`) |
| `OMNIFEED_REDDIT_FORMAT` | `toon` | Default Reddit output: `toon` or `json` |
| `OMNIFEED_REDDIT_FETCH_LIMIT` | `500` | Reddit `limit`: max comments fetched in the initial tree |
| `OMNIFEED_REDDIT_DEPTH` | `20` | Reddit `depth`: max nesting depth of the initial tree |
| `OMNIFEED_REDDIT_SORT` | `top` | Reddit `sort`: one of `confidence` (=best), `top`, `new`, `controversial`, `old`, `random`, `qa`, `live` |
| `OMNIFEED_REDDIT_MAX_COMMENTS` | `0` | Hard cap on total comments emitted after expansion (0 = unlimited) |
| `OMNIFEED_REDDIT_MAX_TOP_LEVEL` | `0` | Hard cap on top-level comment threads, replies included (0 = unlimited) |
| `OMNIFEED_REDDIT_KEEP_CREATED` | `true` | Include each comment's `created` timestamp |
| `OMNIFEED_REDDIT_KEEP_DEPTH` | `false` | Include each comment's `depth` field |
| `OMNIFEED_MAX_URLS_PER_REQUEST` | `30` | Cap on `urls[]` length |
| `OMNIFEED_PER_DOMAIN_CONCURRENCY` | `2` | Max concurrent requests to one domain |
| `OMNIFEED_PER_DOMAIN_DELAY` | `1500ms` | Minimum delay between same-domain requests |
| `OMNIFEED_BLOCK_PRIVATE_IPS` | `true` | SSRF protection (keep on in production) |
| `OMNIFEED_ALLOWED_ORIGINS` | _(unset)_ | Comma-separated browser `Origin` values (e.g. `https://app.example.com`) allowed to call the HTTP APIs cross-origin. Requests without an `Origin` header — every native MCP client — always pass, and so do loopback origins (browser tools like the MCP inspector). Anything else gets **403** — the DNS-rebinding guard the MCP transport spec requires, applied to all HTTP endpoints. |
| `OMNIFEED_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `OMNIFEED_LOG_FORMAT` | `json` | `json` or `text` |
| `OMNIFEED_ENABLE_PPROF` | `false` | Expose `/debug/pprof/*` (opt-in) |

### Retry-After propagation

An upstream that answers `429` or `503` with a `Retry-After` header is telling
omnifeed to back off, so the answer outlives the request that got it: the host is
held back for that duration, capped at 5 minutes, across later requests — and
across replicas when `OMNIFEED_REDIS_URL` is set. This applies with and without
Redis, and only to upstreams an engine calls directly (the crawled site's own
`Retry-After` is invisible behind crawl4ai). A held host makes the next callers
wait, each bounded by its own request budget — the engine timeouts, about 30s, or
`MaxWait` for a search, 15s — and then time out normally.

### Pacing fail-fast

A caller is never held for a wait it cannot use. When a request carries a
deadline (a search always does — `MaxWait`, 15s) and the pacing wait the limiter
computes is longer than what is left of that deadline, the limiter refuses up
front instead of queueing: nothing is sent, the window's quota is not consumed,
and the caller gets `quota_exhausted` with the wait in the message — `pacing
quota exhausted; retry in 47s`. The old behaviour was to hold the query for the
whole 15s and then fail it with a timeout anyway, which cost an agent the wait
**and** the answer (measured 2026-08-21: 21 searches held ~20s each, all of them
dead on arrival).

It is a verdict, not a fault: `quota_exhausted` means the deployment is pacing as
configured and the caller should come back later. Raise
`OMNIFEED_SEARXNG_QUOTA` / lower `OMNIFEED_SEARXNG_DELAY` if it fires more often
than the engines actually require. A request with **no** deadline queues as
before, and these refusals are the `outcome="budget_exceeded"` series of
`omnifeed_domain_limiter_wait_seconds` (~0 seconds each, unlike
`outcome="canceled"`).

## Controlling fetched content size

A long article can overflow an MCP client's per-response budget, at which point the client spools the text to a file and the model never reads it. So `fetch_url` caps **markdown** content at `OMNIFEED_FETCH_MAX_CHARS` (120000 by default, `0` = unlimited) and, when it cuts, ends the reply with a resumable marker:

```
[omnifeed: content truncated at 120000 of 174233 characters. Call fetch_url again with start_char=120000 to continue.]
```

Both knobs are per-request on the tool, and both are **markdown-only**:

| Param | Default | Purpose |
|---|---|---|
| `max_chars` | `OMNIFEED_FETCH_MAX_CHARS` | Max characters to return, up to `500000`. `0`/omitted uses the server default. |
| `start_char` | `0` | Character offset to start from — pass the value from a truncation marker to read the next chunk. |

Offsets and lengths count **characters, not bytes**, so a window never splits a multibyte character, and the marker counts against `max_chars`, so a reply never exceeds the requested ceiling. The same numbers come back in the response `_meta` as `truncated`, `total_chars`, and `next_start_char`. `fetch_url` also declares `anthropic/maxResultSizeChars: 500000` in `tools/list`, which raises Claude Code's per-tool text budget to match the ceiling; other clients ignore it.

**TOON and JSON output is never truncated** — that covers every dedicated engine, not just Reddit. Its length markers would describe rows that are no longer there, so structured engines are bounded by their own element caps instead — see the Reddit knobs below.

`POST /crawl` has **no size params and is always unlimited**: RAG pipelines chunk and embed whatever they receive, so a cap there would silently drop retrievable text rather than protect a context window (and one shared offset would corrupt every other URL in a batch).

## Fetching infinite-scroll feeds

Generic crawls do **not** scroll by default — the scroll costs seconds on every page and gains content only on append-style infinite feeds (and actively corrupts virtualized pages). When a feed/listing/gallery URL clearly came back missing items, opt in per request: the `fetch_url` tool's `scan_full_page: true` argument, or `POST /crawl?scan_full_page=true` on the loader. Both are tri-state (`false` forces the scroll off where a deployment enables `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE` globally); `OMNIFEED_CRAWL4AI_SCROLL_DELAY` paces the scroll steps.

## Controlling Reddit response size

A Reddit thread's comment tree can be huge. The size knobs come in two kinds — it matters which is which:

- **Upstream Reddit params** — forwarded verbatim to Reddit's API, so Reddit owns their behavior: `OMNIFEED_REDDIT_FETCH_LIMIT` → `limit`, `OMNIFEED_REDDIT_DEPTH` → `depth`, `OMNIFEED_REDDIT_SORT` → `sort`. They shape *what Reddit sends back* (less latency, fewer tokens) but are **approximate**, and `limit`/`depth` bound only the **initial** fetch. Semantics are Reddit's, not ours — see <https://www.reddit.com/dev/api/> → `GET [/r/subreddit]/comments/article` (`limit` = "maximum number of comments to return", `depth` = "maximum depth of subtrees").
- **omnifeed engine caps** — our own, applied *after* fetch + expansion, so they're **exact and independent of Reddit**: `OMNIFEED_REDDIT_MAX_COMMENTS` (truncate the flat comment list) and `OMNIFEED_REDDIT_MAX_TOP_LEVEL` (keep the first N top-level threads, in `sort` order, with their replies).

Rule of thumb: reach for the **upstream params** to fetch less from Reddit; reach for the **engine caps** when you need a guaranteed ceiling — `OMNIFEED_REDDIT_MAX_ROUNDS` expansion adds comments on top of `limit`, so only the caps bound the final total. All five are also per-request on the `fetch_url` MCP tool (`limit`, `depth`, `sort`, `max_comments`, `max_top_level`); a positive value overrides the env default. All five are **thread** knobs: a subreddit listing has no comment tree, and takes its post count and time window from the URL itself instead — `?limit=` (1–100, default 25) and `?t=hour|day|week|month|year|all` (on `top`/`controversial`, which are the only sorts Reddit applies it to).

## Controlling Hacker News response size

A Hacker News megathread is shaped very differently from a Reddit thread: the Algolia item API returns the **whole** tree in one response, and the top-level subtree sizes are wildly skewed (one measured thread: 105, 55, 16, 13, 12, 11, 10, 6, … over 62 top-level threads). So there are no upstream params to forward — only omnifeed caps, applied after the fetch, all three per-request on the `fetch_url` MCP tool:

| Param | Default | Purpose |
|---|---|---|
| `max_per_subtree` | unlimited | Max comments kept inside **each** top-level thread, counting that thread's root comment. Selection is breadth-first per thread, so a thread contributes its root and shallow replies before its deep tail. |
| `max_top_level` | unlimited | Keep the first N top-level threads (in HN's order) with all their replies. |
| `max_comments` | 500 | Absolute ceiling on the flat comment list, applied last. A caller value can only lower the 500 engine ceiling, never raise it. |

Reach for `max_per_subtree` first: it is the cap that matches the mega-thread shape. Measured on two real threads, `max_per_subtree=12` returned ~56% and ~47% of the full-tree bytes while keeping 9/12 and 11/14 of the comments a human reader rated substantive — a flat `max_comments` truncation instead spends most of its budget inside the single biggest branch (at 100 comments it covered 4 of 62 top-level threads, 72% of them in one subtree).

There is deliberately **no depth cap for Hacker News**. Depth carries no quality signal there — the single most valuable comment in both measured threads sat at depth 7, and adding `depth<=5` on top of a subtree cap cost real content while saving under 1k characters out of 84k. The `depth` and `sort` params stay Reddit-only, and are ignored on an HN URL.

Output order is never reordered by a cap: survivors stay in the order HN returned them, and because breadth-first selection keeps a comment's ancestors before the comment itself, the `parent_id` chain in the output is never broken.

## Reddit anti-bot handling

Reddit's edge 403-blocks non-browser HTTP clients (it fingerprints the TLS/JA3 handshake), so the Reddit engine never calls Reddit directly. It drives a **real headless browser** to a `www.reddit.com` page (which clears the bot wall), then runs a **same-origin `fetch()`** of the `.json` and `/api/morechildren` endpoints from inside that page. No Reddit auth, cookies, or API key. By default the browser is crawl4ai, reached through its token-gated **`POST /execute_js`** endpoint, so the upstream must run with `CRAWL4AI_EXECUTE_JS_ENABLED=true` and `OMNIFEED_CRAWL4AI_TOKEN` set to its `CRAWL4AI_API_TOKEN`.

> Sustained scraping can raise your source IP's risk score. If fetches start returning the block page, slow down, keep `expand` modest, or route the browser through a residential proxy.

## Raw-text bypass

Non-HTML text — raw code files, JSON, markdown, plain text — has nothing for a browser to render, and Chromium's page-idle machinery makes such fetches pathologically slow (a raw `githubusercontent.com` file: 30–39 s in the browser vs ~200 ms direct). When a URL's path extension looks raw (`.md`, `.txt`, `.json`, source files, …), the generic engine spends a cheap HEAD probe: if the server confirms a non-HTML text content type, the body is fetched with a plain GET and returned as-is; anything uncertain (probe failure, `text/html`, binary bytes, restricted egress) silently takes the normal browser path. Direct fetches refuse private/reserved addresses **at dial time** when `OMNIFEED_BLOCK_PRIVATE_IPS` is on, so a DNS-rebinding race can't slip past the URL validation. Note the pod/host then needs outbound access to the target sites themselves, not just to crawl4ai — without it the probe fails and everything still flows through crawl4ai as before.

## Prometheus metrics

Served on `OMNIFEED_METRICS_ADDR` (default `:9090`) at `/metrics`, alongside the Go/process collectors:

| Metric | Type | Labels | What it measures |
|---|---|---|---|
| `omnifeed_requests_total` | counter | `engine, tenant, status, reason` | Crawl requests, with a bounded failure `reason` (`ok` on success) |
| `omnifeed_request_seconds` | histogram | `engine, status, reason` | End-to-end crawl latency |
| `omnifeed_request_attempts_total` | counter | `upstream, attempt` | HTTP attempts by the retrying client (`first` vs `retry`) |
| `omnifeed_upstream_seconds` | histogram | `upstream, op, status` | Upstream HTTP round-trip per attempt (start → body fully read); `crawl4ai/crawl`, `crawl4ai/execute_js`, `searxng/search`, `github/api`, `hackernews/api`, `discourse/api` |
| `omnifeed_domain_limiter_wait_seconds` | histogram | `engine, outcome` | Time blocked in per-domain limiter acquisition (semaphore + politeness delay); `outcome="canceled"` = the wait died in the queue; `outcome="budget_exceeded"` = the fast-fail, the wait was longer than the caller's remaining deadline so nothing was queued (~0 seconds, and `reason="quota_exhausted"` on the request metric) |
| `omnifeed_ratelimit_backend_errors_total` | counter | `op` | Failed Redis operations in the distributed limiter: `acquire` (the limiter then paces in process), `release` or `penalize` (both cost pacing accuracy only) |
| `omnifeed_ratelimit_penalties_total` | counter | `upstream` | Upstream `Retry-After` headers (on 429 or 503) fed back into the limiters as a hold on that host — the signal that precedes a CAPTCHA or a block |
| `omnifeed_ratelimit_degraded` | gauge | `scope` | `1` while pacing is degraded to per-pod limits because Redis is unreachable, `0` while it is shared. The series exists only when `OMNIFEED_REDIS_URL` is set: each scope is published at `0` on startup, and with Redis off there is no series at all. One series per limiter scope (`domain` for crawling, `searxng` for queries) — each degrades and recovers on its own |
| `omnifeed_response_chars` | histogram | `engine` | Extracted content length (pre-truncation — the engine's output, before any transport `max_chars` clipping), successful crawls only |
| `omnifeed_engine_fallbacks_total` | counter | `from_engine, reason` | Dedicated-engine failures re-crawled via the generic fallback |
| `omnifeed_searxng_unresponsive_engines_total` | counter | `engine, error` | Engines SearXNG reported unresponsive per search; `error` normalized to a closed set (`timeout`, `captcha`, `suspended`, `too_many_requests`, `access_denied`, `error`, `unknown`) |
| `omnifeed_searxng_engine_results_total` | counter | `engine` | Result rows each SearXNG engine contributed — the silent-block detector: a blocked engine keeps answering 200 with zero results, so its series goes flat while the pool moves. Alert on the divergence, not the absolute rate (and pair it with `absent_over_time()`: an engine already blocked at startup mints no series at all) |
| `omnifeed_searxng_queries_total` | counter | `scoped` | Queries actually **sent** to SearXNG (past the limiter, before the HTTP call) — the admitted rate the engines behind it see. Compare with `omnifeed_search_requests_total` to see what pacing refused |
| `omnifeed_searxng_engine_zero_results_total` | counter | `engine` | Searches in which one engine contributed no rows **while the search returned results** — the silent-block signature, named per engine. Engines SearXNG reported unresponsive are excluded (they failed openly). Only engines the process has already seen answer can be counted, so pair with `absent_over_time()` |
| `omnifeed_searxng_empty_searches_total` | counter | `scoped` | Searches with zero results **and** no unresponsive-engine report; `scoped="true"` is a `site:`-filtered query, where silent blocks concentrate |
| `omnifeed_reddit_expansion_rounds` | histogram | — | `/api/morechildren` rounds per Reddit crawl |
| `omnifeed_search_requests_total` | counter | `searcher, status, reason` + `scoped` | Search queries as CALLERS asked for them (a refused query is counted here and not in `omnifeed_searxng_queries_total`); `scoped="true"` is a `site:`-filtered query. Every `sum()` over this counter is unaffected by the added label; a query pinning an exact label set is not |
| `omnifeed_search_request_seconds` | histogram | `searcher, status` | Search latency |
| `omnifeed_search_engine_position_rank` | histogram | `engine` | Where in its own ranking an engine placed each row it returned (ranks, not seconds: 1–3 is a result a caller reads, 20+ is filler) |
| `omnifeed_search_engine_unique_results_total` | counter | `engine` | Results no other engine in the pool returned — the metric that decides whether an engine earns its slot |
