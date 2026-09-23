# Configuration

omnifeed reads `OMNIFEED_`-prefixed environment variables. You usually set only
`OMNIFEED_API_KEY`, `OMNIFEED_CRAWL4AI_URL` and, for search, `OMNIFEED_SEARXNG_URL`.

The binary reads every variable below. The Apple `container` launcher has a few
`OMNIFEED_` variables of its own, for container names, host ports and images, which
the binary never sees. They are in [apple-container.md](apple-container.md).

**Egress.** Reddit and the generic fallback reach the web through crawl4ai. Four
engines call their upstream directly: Hacker News reads `hn.algolia.com`, GitHub reads
`api.github.com`, Bluesky reads `public.api.bsky.app`, and Discourse reads each host in
`OMNIFEED_DISCOURSE_HOSTS`. If outbound traffic may reach only crawl4ai, those four break.

| Variable | Default | Purpose |
|---|---|---|
| `OMNIFEED_API_KEY` | _(unset)_ | Bearer token for `/crawl`, `/search` and `/mcp`. If unset, omnifeed refuses to start unless `OMNIFEED_DEV_NO_AUTH=true`. Stdio MCP ignores it. |
| `OMNIFEED_CRAWL4AI_URL` | _(required)_ | crawl4ai endpoint. Reddit and the generic fallback fetch through it. If empty, omnifeed exits at startup. |
| `OMNIFEED_CRAWL4AI_TOKEN` | _(unset)_ | Bearer token sent to crawl4ai, matching its `CRAWL4AI_API_TOKEN`. crawl4ai binds beyond loopback only when a token is set. Unset sends no `Authorization` header. |
| `OMNIFEED_SEARXNG_URL` | _(unset)_ | SearXNG base URL, e.g. `http://searxng:8080`. Unset hides `web_search` and `/search`. The instance must enable the `json` format. |
| `OMNIFEED_DEV_NO_AUTH` | `false` | Run the HTTP transports with no auth when no key is set. Local use only. Ignored if a key is set. |
| `OMNIFEED_LISTEN_ADDR` | `:8080` | HTTP listen address for `/crawl` and `/search` |
| `OMNIFEED_MCP_LISTEN_ADDR` | `:8081` | MCP HTTP/SSE listen address |
| `OMNIFEED_MCP_STDIO` | `false` | Run MCP over stdio, same as `--mcp-stdio` |
| `OMNIFEED_METRICS_ADDR` | `:9090` | Prometheus and health listen address |
| `OMNIFEED_CRAWL4AI_TIMEOUT` | `90s` | Per-call timeout to crawl4ai |
| `OMNIFEED_CRAWL4AI_KEEP_LINKS` | `true` | Keep link anchor text and external links in fetched markdown. `false` strips them, which loses link-dense content like HN titles. |
| `OMNIFEED_CRAWL4AI_PRUNE_THRESHOLD` | `0.48` | PruningContentFilter cutoff, 0 to 1, for the generic engine. Raise it to strip more boilerplate from noisy pages. Lower it to keep more. |
| `OMNIFEED_CRAWL4AI_WAIT_UNTIL` | `domcontentloaded` | crawl4ai page-ready signal: `domcontentloaded`, `load`, `networkidle` or `commit`. `domcontentloaded` fires before client-side frameworks hydrate, so JS-only SPAs can come back empty. `networkidle` waits for them and slows every page. |
| `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE` | `false` | Scroll the full page before extraction by default (crawl4ai `scan_full_page`). In benchmarks it gained content only on append-style infinite-scroll feeds, tripled latency on every page, and corrupted virtualized pages through an open crawl4ai bug. Prefer the per-request `scan_full_page` argument on `fetch_url` or `POST /crawl?scan_full_page=true`. Either overrides this setting. |
| `OMNIFEED_CRAWL4AI_SCROLL_DELAY` | `0.5` | Seconds between scroll steps during a full-page scan. Sent only when the scan is on. |
| `OMNIFEED_CRAWL4AI_REMOVE_OVERLAYS` | `false` | Send crawl4ai's `remove_overlay_elements`. Leave it off. It deletes every large fixed- or absolute-position element, which empties pages whose content lives in one: Wikipedia and several news fronts return only their `<title>`. `remove_consent_popups` handles cookie modals and stays on regardless. |
| `OMNIFEED_CRAWL4AI_DELAY_BEFORE_HTML` | `0.1` | Seconds to wait after the page-ready signal before extracting HTML (crawl4ai `delay_before_return_html`), on every crawl. Raise it, e.g. to `1.0`, if pages that render late come back thin. |
| `OMNIFEED_CRAWL4AI_EXCLUDED_SELECTOR` | `.sidebar,.toc,#toc,.related,.newsletter,.cookie-banner,[aria-label*='cookie']` | CSS selectors the generic engine drops before extraction (crawl4ai `excluded_selector`). The default matches only page chrome, so it can't remove article text. Your list replaces it. Empty keeps the default. If exclusion empties a page, because its content was a `#toc` or `.sidebar`, the crawl retries once without it. |
| `OMNIFEED_CRAWL4AI_TARGET_ELEMENTS` | _(unset, off)_ | Comma-separated CSS selectors. When set, the generic engine extracts markdown only from matching containers (crawl4ai `target_elements`). Good on article and repo pages, but a page with no match returns empty content, and the thin-content guard reports an error. Test it on your own URLs first. A starting list: `article, main, [role=main], .markdown-body, .post-content, #content`. |
| `OMNIFEED_SEARXNG_TIMEOUT` | `15s` | Per-query timeout to SearXNG |
| `OMNIFEED_SEARXNG_SITE_ENGINES` | _(unset, whole pool)_ | Comma-separated SearXNG engines to query when the caller passes a valid `site` filter, e.g. `privacywall,google cse`. SearXNG forwards `site:` to its engines. Some honor it, some ignore it and return unrelated pages, and some return zero results. List the ones that honor it. Unset queries every engine. |
| `OMNIFEED_SEARXNG_DELAY` | `0` (off) | Minimum gap between queries to SearXNG. This paces the engines behind SearXNG: each query fans out to every enabled engine, so each engine sees omnifeed's query rate, and one that finds it bot-like suspends itself or serves a CAPTCHA. A gap alone doesn't bound a burst, so pair it with `OMNIFEED_SEARXNG_QUOTA`. |
| `OMNIFEED_SEARXNG_QUOTA` | `0` (off) | Maximum queries in any rolling `OMNIFEED_SEARXNG_QUOTA_WINDOW`. Engines enforce two limits. One measured pool answered at a 3s gap from a quiet start, then blocked after about 20 queries in 85s because it counts requests per window. The delay sets the gap and the quota bounds the burst. Measure both on your own pool. |
| `OMNIFEED_SEARXNG_QUOTA_WINDOW` | `90s` | Rolling window for `OMNIFEED_SEARXNG_QUOTA`. Ignored when the quota is `0`. Must be above `0` when the quota is set. |
| `OMNIFEED_SEARXNG_CONCURRENCY` | `0` (off) | Maximum searches in flight to SearXNG across all replicas. A nonzero `OMNIFEED_SEARXNG_DELAY` alone serializes admissions, so the whole deployment runs one search at a time. Above `0`, the delay spaces sends and this sets the bound. With Redis, slots are TTL leases, so a pod that dies mid-search frees its slot. On one pool (2026-08-24), 16 concurrent ran clean and 24 got the strictest engine suspended for 1200s. Leave margin: overshooting costs a 20-minute engine outage, not one failed query. A single instance needs no Redis. With several, set `OMNIFEED_REDIS_URL`, or N instances send N times this. |
| `OMNIFEED_SEARXNG_MAX_WAIT` | `15s` | How long a query may wait in the pacing limiter before it gives up. A fanned-out caller that sees `context deadline exceeded` hit this queue limit, not a slow upstream. Raise it to trade latency for completions. |
| `OMNIFEED_REDIS_URL` | _(unset, off)_ | Share rate-limiter state through Redis, e.g. `redis://user:pass@redis:6379/2`, or `rediss://` for TLS. Unset, each replica paces alone, so N replicas send up to N times the configured rate, and `OMNIFEED_SEARXNG_QUOTA` and `OMNIFEED_PER_DOMAIN_DELAY` apply per pod. Set, the delay and quota count once for the deployment. If Redis is unreachable, the limiters pace in process and crawls keep working. `OMNIFEED_PER_DOMAIN_CONCURRENCY` stays per pod by design, so the replica count multiplies it. |
| `OMNIFEED_REDIS_KEY_PREFIX` | `omnifeed:ratelimit` | Key namespace, so several deployments can share one Redis without counting each other's requests. Every key has a TTL, which matters on a shared `noeviction` instance. |
| `OMNIFEED_REDIS_TIMEOUT` | `250ms` | Budget for one Redis operation. An acquisition can wait minutes for a quota window, and only each Redis call inside it gets this budget. On a breach the limiter paces in process for a 30s cooldown before trying Redis again, so a dead Redis costs one timeout per cooldown, not one per request. |
| `OMNIFEED_SEARCH_AUDIT` | `off` | Per-search audit log: `off`, `summary` or `full`. It is a data feed, not a log level. Putting it behind `debug` would force every component's debug output on, and invite sampling, which biases the per-engine statistics. `summary` logs one line per search with `query_id`, query, `site`, `time_range`, `total`, `duration_ms` and per-engine row counts, plus one line per unresponsive engine with `engine` and `reason_class`. `full` adds one line per engine and result with that engine's own rank, joined on `query_id`. Lines stay flat because log stores turn JSON arrays into opaque strings that can't be grouped or averaged. Both modes log the query, and `full` logs every result URL, so retention is your call. Lines log at INFO, so startup rejects any mode but `off` unless `OMNIFEED_LOG_LEVEL` is `debug` or `info`. The `omnifeed_search_engine_position_rank` and `omnifeed_search_engine_unique_results_total` metrics carry no queries or URLs and are always recorded. |
| `OMNIFEED_SEARCH_MAX_RESULTS` | `25` | Cap on the search `limit` argument, 1 to 100 |
| `OMNIFEED_FETCH_MAX_CHARS` | `120000` | Default cap on markdown returned by `fetch_url`. `0` is unlimited. Over the cap, the reply ends with a resumable truncation marker. TOON and JSON output from the dedicated engines is never cut; use their own limits. Doesn't apply to `/crawl`. See [Controlling fetched content size](#controlling-fetched-content-size). |
| `OMNIFEED_GITHUB_TOKEN` | _(unset)_ | Personal access token for the GitHub engine. Anonymous access allows 60 requests per hour per IP. A token raises it to 5000. |
| `OMNIFEED_DISCOURSE_HOSTS` | `meta.discourse.org,discuss.python.org,users.rust-lang.org,internals.rust-lang.org,discuss.pytorch.org` | Hostnames where the Discourse engine claims `/t/…` topic URLs. Discourse runs on arbitrary domains and the engine can't detect it, so list the forums you use. Matching is exact and case-insensitive, with no subdomain wildcards. Unlisted forums go through the generic browser fallback, which returns less of the thread. An empty string disables the engine. |
| `OMNIFEED_REDDIT_TIMEOUT` | `4m` | Wall-clock cap for a Reddit thread expansion |
| `OMNIFEED_REDDIT_MAX_ROUNDS` | `3` | Default `/api/morechildren` rounds. `?expand=full` allows up to 40. |
| `OMNIFEED_REDDIT_FORMAT` | `toon` | Default Reddit output: `toon` or `json` |
| `OMNIFEED_REDDIT_FETCH_LIMIT` | `500` | Reddit `limit`: max comments in the initial tree |
| `OMNIFEED_REDDIT_DEPTH` | `20` | Reddit `depth`: max nesting depth of the initial tree |
| `OMNIFEED_REDDIT_SORT` | `top` | Reddit `sort`: `confidence` (best), `top`, `new`, `controversial`, `old`, `random`, `qa` or `live` |
| `OMNIFEED_REDDIT_MAX_COMMENTS` | `0` | Cap on total comments after expansion. `0` is unlimited. |
| `OMNIFEED_REDDIT_MAX_TOP_LEVEL` | `0` | Cap on top-level threads, replies included. `0` is unlimited. |
| `OMNIFEED_REDDIT_KEEP_CREATED` | `true` | Include each comment's `created` timestamp |
| `OMNIFEED_REDDIT_KEEP_DEPTH` | `false` | Include each comment's `depth` |
| `OMNIFEED_MAX_URLS_PER_REQUEST` | `30` | Cap on `urls[]` length |
| `OMNIFEED_PER_DOMAIN_CONCURRENCY` | `2` | Max concurrent requests to one domain |
| `OMNIFEED_PER_DOMAIN_DELAY` | `1500ms` | Minimum gap between requests to one domain |
| `OMNIFEED_BLOCK_PRIVATE_IPS` | `true` | SSRF protection. Keep it on in production. |
| `OMNIFEED_ALLOWED_ORIGINS` | _(unset)_ | Comma-separated browser `Origin` values, e.g. `https://app.example.com`, allowed to call the HTTP APIs cross-origin. Requests with no `Origin`, which covers every native MCP client, always pass, as do loopback origins such as the MCP inspector. Anything else gets 403. This is the DNS-rebinding guard the MCP transport spec requires, applied to every HTTP endpoint. |
| `OMNIFEED_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `OMNIFEED_LOG_FORMAT` | `json` | `json` or `text` |
| `OMNIFEED_ENABLE_PPROF` | `false` | Expose `/debug/pprof/*` |

### Retry-After propagation

When an upstream answers `429` or `503` with `Retry-After`, omnifeed holds that host
for the given duration, capped at 5 minutes, across later requests. With
`OMNIFEED_REDIS_URL` set, the hold spans replicas. It applies only to upstreams an
engine calls directly, because crawl4ai hides the crawled site's own `Retry-After`.
Callers waiting on a held host time out normally at their own budget: the engine
timeout, about 30s, or `MaxWait`, 15s, for a search.

### Pacing fail-fast

If a request has a deadline and the pacing wait is longer than the time left, the
limiter refuses at once. Searches always have one, `MaxWait`, 15s. Nothing is sent,
no quota is spent, and the caller gets `quota_exhausted` with the wait:
`pacing quota exhausted; retry in 47s`. Before this, omnifeed held the query for 15s
and then timed out anyway. On 2026-08-21, 21 searches each waited about 20s and all failed.

`quota_exhausted` means pacing works as configured, and the caller should retry
later. If it fires more often than the engines require, raise
`OMNIFEED_SEARXNG_QUOTA` or lower `OMNIFEED_SEARXNG_DELAY`. A request with no
deadline still queues. Refusals show up as `outcome="budget_exceeded"` on
`omnifeed_domain_limiter_wait_seconds`, at about 0 seconds each, unlike
`outcome="canceled"`.

## Controlling fetched content size

A long article can overflow an MCP client's per-response budget. The client then writes the text to a file and the model never reads it. So `fetch_url` caps markdown at `OMNIFEED_FETCH_MAX_CHARS`, 120000 by default and `0` for unlimited, and ends a cut reply with a resumable marker:

```
[omnifeed: content truncated at 120000 of 174233 characters. Call fetch_url again with start_char=120000 to continue.]
```

Both per-request parameters apply to markdown only:

| Param | Default | Purpose |
|---|---|---|
| `max_chars` | `OMNIFEED_FETCH_MAX_CHARS` | Max characters to return, up to `500000`. `0` or omitted uses the server default. |
| `start_char` | `0` | Character offset to start from. Pass the value from a truncation marker to read the next chunk. |

Offsets and lengths count characters, not bytes, so a window never splits a multibyte character. The marker counts against `max_chars`, so a reply never exceeds it. The response `_meta` carries the same numbers as `truncated`, `total_chars` and `next_start_char`. `fetch_url` declares `anthropic/maxResultSizeChars: 500000` in `tools/list`, which raises Claude Code's per-tool text limit to match. Other clients ignore it.

omnifeed never truncates TOON or JSON output, which covers every dedicated engine. Its length markers would describe rows that were cut, so those engines use their own element caps, described below.

`POST /crawl` has no size parameters and never truncates. RAG pipelines chunk and embed whatever they receive, so a cap would drop retrievable text, and one shared offset would break every other URL in a batch.

## Fetching infinite-scroll feeds

Generic crawls don't scroll by default. Scrolling costs seconds on every page, gains content only on append-style infinite feeds, and corrupts virtualized pages. When a feed, listing or gallery comes back missing items, opt in per request with `scan_full_page: true` on `fetch_url` or `POST /crawl?scan_full_page=true`. Both accept `false` too, which turns the scroll off where `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE` turns it on. `OMNIFEED_CRAWL4AI_SCROLL_DELAY` sets the pause between scroll steps.

## Controlling Reddit response size

Reddit comment trees can be huge. There are two kinds of limit:

- **Reddit parameters**, forwarded as-is to Reddit's API: `OMNIFEED_REDDIT_FETCH_LIMIT` as `limit`, `OMNIFEED_REDDIT_DEPTH` as `depth`, and `OMNIFEED_REDDIT_SORT` as `sort`. They reduce what Reddit sends, which saves latency and tokens, but they are approximate, and `limit` and `depth` bound only the initial fetch. Reddit defines them in its [API docs](https://www.reddit.com/dev/api/) under `GET [/r/subreddit]/comments/article`: `limit` is "maximum number of comments to return" and `depth` is "maximum depth of subtrees".
- **omnifeed caps**, applied after fetch and expansion, so they are exact: `OMNIFEED_REDDIT_MAX_COMMENTS` truncates the flat comment list, and `OMNIFEED_REDDIT_MAX_TOP_LEVEL` keeps the first N top-level threads, in `sort` order, with their replies.

Use the Reddit parameters to fetch less. Use the caps for a guaranteed ceiling: `OMNIFEED_REDDIT_MAX_ROUNDS` expansion adds comments beyond `limit`, so only the caps bound the total. `fetch_url` accepts all five per request as `limit`, `depth`, `sort`, `max_comments` and `max_top_level`, and a positive value overrides the env default. They apply to threads only. A subreddit listing has no comment tree and takes its post count and time window from the URL: `?limit=`, 1 to 100 with a default of 25, and `?t=hour|day|week|month|year|all`, which Reddit applies only to `top` and `controversial`.

## Controlling Hacker News response size

The Algolia item API returns a Hacker News thread's whole tree in one response, and top-level subtree sizes are skewed. One measured thread had subtrees of 105, 55, 16, 13, 12, 11, 10, 6 and so on across 62 top-level threads. There are no upstream parameters to forward, only omnifeed caps applied after the fetch, all three per request on `fetch_url`:

| Param | Default | Purpose |
|---|---|---|
| `max_per_subtree` | unlimited | Max comments kept in each top-level thread, root included. Selection is breadth-first, so a thread keeps its root and shallow replies before its deep tail. |
| `max_top_level` | unlimited | Keep the first N top-level threads, in HN's order, with all their replies. |
| `max_comments` | 500 | Ceiling on the flat comment list, applied last. A caller can lower it below 500, never raise it. |

Start with `max_per_subtree`, which fits that skew. On two real threads, `max_per_subtree=12` returned about 56% and 47% of the full-tree bytes and kept 9 of 12 and 11 of 14 of the comments a human rated substantive. A flat `max_comments` cut spends most of its budget in the biggest branch: at 100 comments it covered 4 of 62 top-level threads, with 72% of its comments in one subtree.

Hacker News has no depth cap on purpose, because depth says nothing about quality there. The most valuable comment in both measured threads sat at depth 7, and adding `depth<=5` to a subtree cap lost real content while saving under 1k of 84k characters. `depth` and `sort` are Reddit-only and ignored on HN URLs.

Caps never reorder output. Comments stay in HN's order, and breadth-first selection keeps each comment's ancestors, so the `parent_id` chain never breaks.

## Reddit anti-bot handling

Reddit's edge fingerprints the TLS/JA3 handshake and 403-blocks non-browser HTTP clients, so the Reddit engine never calls Reddit directly. It drives a real headless browser to a `www.reddit.com` page, which clears the bot wall, then runs a same-origin `fetch()` of the `.json` and `/api/morechildren` endpoints from inside it. It needs no Reddit auth, cookies or API key. The default browser is crawl4ai, through its token-gated `POST /execute_js` endpoint, so crawl4ai must run with `CRAWL4AI_EXECUTE_JS_ENABLED=true`, and `OMNIFEED_CRAWL4AI_TOKEN` must match its `CRAWL4AI_API_TOKEN`.

> Sustained scraping can raise your IP's risk score. If fetches return the block page, slow down, keep `expand` modest, or route the browser through a residential proxy.

## Raw-text bypass

Raw code, JSON, markdown and plain text have nothing for a browser to render, and Chromium's page-idle wait makes them slow: a raw `githubusercontent.com` file takes 30 to 39 s in the browser and about 200 ms direct. When a URL's extension looks raw (`.md`, `.txt`, `.json`, source files), the generic engine sends a HEAD request. If the server confirms a non-HTML text type, a plain GET fetches the body and returns it unchanged. Anything uncertain, such as a failed probe, `text/html`, binary bytes or blocked egress, falls back to the browser. With `OMNIFEED_BLOCK_PRIVATE_IPS` on, direct fetches refuse private and reserved addresses when dialing, so DNS rebinding can't bypass URL validation. This needs outbound access to the target sites, not just crawl4ai. Without it the probe fails and everything goes through crawl4ai.

## Prometheus metrics

Served at `/metrics` on `OMNIFEED_METRICS_ADDR`, default `:9090`, alongside the Go and process collectors:

| Metric | Type | Labels | What it measures |
|---|---|---|---|
| `omnifeed_requests_total` | counter | `engine, tenant, status, reason` | Crawl requests, with a bounded failure `reason`, `ok` on success |
| `omnifeed_request_seconds` | histogram | `engine, status, reason` | End-to-end crawl latency |
| `omnifeed_request_attempts_total` | counter | `upstream, attempt` | HTTP attempts by the retrying client, `first` or `retry` |
| `omnifeed_upstream_seconds` | histogram | `upstream, op, status` | Upstream round-trip per attempt, from start until the body is read: `crawl4ai/crawl`, `crawl4ai/execute_js`, `searxng/search`, `github/api`, `hackernews/api`, `discourse/api` |
| `omnifeed_domain_limiter_wait_seconds` | histogram | `engine, outcome` | Time blocked acquiring the per-domain limiter, semaphore plus delay. `outcome="canceled"`: the wait died in the queue. `outcome="budget_exceeded"`: the wait exceeded the caller's remaining deadline, so nothing queued; about 0 seconds, with `reason="quota_exhausted"` on the request metric |
| `omnifeed_ratelimit_backend_errors_total` | counter | `op` | Failed Redis operations in the distributed limiter. On `acquire` the limiter paces in process. `release` and `penalize` failures cost only pacing accuracy |
| `omnifeed_ratelimit_penalties_total` | counter | `upstream` | Upstream `Retry-After` headers, on 429 or 503, turned into a hold on that host. This often precedes a CAPTCHA or block |
| `omnifeed_ratelimit_degraded` | gauge | `scope` | `1` while pacing falls back to per-pod limits because Redis is unreachable, `0` while shared. Exists only when `OMNIFEED_REDIS_URL` is set, published at `0` on startup. One series per limiter scope, `domain` for crawling and `searxng` for queries, each degrading and recovering on its own |
| `omnifeed_response_chars` | histogram | `engine` | Engine output length before any `max_chars` truncation, successful crawls only |
| `omnifeed_engine_fallbacks_total` | counter | `from_engine, reason` | Dedicated-engine failures re-crawled by the generic fallback |
| `omnifeed_searxng_unresponsive_engines_total` | counter | `engine, error` | Engines SearXNG reported unresponsive, per search. `error` is one of `timeout`, `captcha`, `suspended`, `too_many_requests`, `access_denied`, `error`, `unknown` |
| `omnifeed_searxng_engine_results_total` | counter | `engine` | Result rows per SearXNG engine. A blocked engine keeps answering 200 with zero results, so its series goes flat while the rest of the pool moves. Alert on that divergence, and pair it with `absent_over_time()`, because an engine blocked at startup has no series |
| `omnifeed_searxng_queries_total` | counter | `scoped` | Queries sent to SearXNG after the limiter, the rate the engines see. Compare with `omnifeed_search_requests_total` to see what pacing refused |
| `omnifeed_searxng_engine_zero_results_total` | counter | `engine` | Searches where one engine returned no rows while the search had results: a silent block, per engine. Excludes engines SearXNG reported unresponsive. Counts only engines seen answering before, so pair with `absent_over_time()` |
| `omnifeed_searxng_empty_searches_total` | counter | `scoped` | Searches with zero results and no unresponsive-engine report. `scoped="true"` is a `site:` query, where silent blocks concentrate |
| `omnifeed_reddit_expansion_rounds` | histogram | none | `/api/morechildren` rounds per Reddit crawl |
| `omnifeed_search_requests_total` | counter | `searcher, status, reason`, `scoped` | Search queries as callers sent them, refused ones included, unlike `omnifeed_searxng_queries_total`. `scoped="true"` is a `site:` query. `sum()` queries ignore the added label. Queries matching an exact label set don't |
| `omnifeed_search_request_seconds` | histogram | `searcher, status` | Search latency |
| `omnifeed_search_engine_position_rank` | histogram | `engine` | The rank each engine gave each row it returned. 1 to 3 is a result a caller reads, 20 and above is filler |
| `omnifeed_search_engine_unique_results_total` | counter | `engine` | Results no other engine returned, which shows whether an engine earns its slot |
