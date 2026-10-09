# Configuration

omnifeed reads `OMNIFEED_`-prefixed environment variables. You usually **set only three**:
`OMNIFEED_API_KEY`, `OMNIFEED_CRAWL4AI_URL` and, for search, `OMNIFEED_SEARXNG_URL`.

The **binary** reads every variable below. The Apple `container` launcher has a few
`OMNIFEED_` variables of its own, for container names, host ports and images, which
the binary never sees. They are in [apple-container.md](apple-container.md).

**Egress.** Reddit and the generic fallback reach the web through crawl4ai. Five
engines call their upstream directly: Hacker News reads `hn.algolia.com`, GitHub reads
`api.github.com`, Bluesky reads `public.api.bsky.app`, Discourse reads each host in
`OMNIFEED_DISCOURSE_HOSTS`, and Twitter/X reads `api.fxtwitter.com` (or
`OMNIFEED_TWITTER_FXTWITTER_URL`), `cdn.syndication.twimg.com`, `api.vxtwitter.com` and
`t.co`. If outbound traffic may reach only crawl4ai, those five **break**; Twitter/X then
falls back to crawl4ai on the `x.com` URL.

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
| `OMNIFEED_CRAWL4AI_WAIT_UNTIL` | `domcontentloaded` | crawl4ai page-ready signal: `domcontentloaded`, `load`, `networkidle` or `commit`. `domcontentloaded` fires before client-side frameworks hydrate, so JS-only SPAs can come back **empty**. `networkidle` waits for them and slows every page. |
| `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE` | `false` | Scroll the full page before extraction by default (crawl4ai `scan_full_page`). In benchmarks it gained content **only** on append-style infinite-scroll feeds, tripled latency on every page, and corrupted virtualized pages through an open crawl4ai bug. Prefer the per-request `scan_full_page` argument on `fetch_url` or `POST /crawl?scan_full_page=true`. Either overrides this setting. |
| `OMNIFEED_CRAWL4AI_SCROLL_DELAY` | `0.5` | Seconds between scroll steps during a full-page scan. Sent only when the scan is on. |
| `OMNIFEED_CRAWL4AI_REMOVE_OVERLAYS` | `false` | Send crawl4ai's `remove_overlay_elements`. **Leave it off.** It deletes every large fixed- or absolute-position element, which empties pages whose content lives in one: Wikipedia and several news fronts return only their `<title>`. `remove_consent_popups` handles cookie modals and stays on regardless. |
| `OMNIFEED_CRAWL4AI_DELAY_BEFORE_HTML` | `0.1` | Seconds to wait after the page-ready signal before extracting HTML (crawl4ai `delay_before_return_html`), on **every** crawl. Raise it, e.g. to `1.0`, if pages that render late come back thin. |
| `OMNIFEED_CRAWL4AI_EXCLUDED_SELECTOR` | `.sidebar,.toc,#toc,.related,.newsletter,.cookie-banner,[aria-label*='cookie'],svg title,[class*='icon'] title,.highcharts-axis-labels,#didomi-host,#onetrust-consent-sdk,#usercentrics-root,.truste_box_overlay` | CSS selectors the generic engine drops before extraction (crawl4ai `excluded_selector`). The default matches only page chrome and text that is never content: SVG and icon-font labels, chart axis labels, and consent-manager roots. It can't remove article text. Your list replaces it. Empty keeps the default. If exclusion empties a page, because its content was a `#toc` or `.sidebar`, the crawl retries once without it. |
| `OMNIFEED_CRAWL4AI_USER_AGENT` | Chrome 153 on Linux | Browser identity sent as crawl4ai `user_agent`. crawl4ai's own default is a malformed Chrome/116 string that Cloudflare challenges. Keep the version in step with the Chromium bundled in your crawl4ai image (153 in 0.9.4). A page blocked under this identity is retried once under crawl4ai's default, because Akamai-fronted sites accept that one and reject the modern string. Set it empty to use crawl4ai's own and skip that retry. |
| `OMNIFEED_CRAWL4AI_STEALTH` | `true` | Send crawl4ai `enable_stealth` (playwright-stealth patches). |
| `OMNIFEED_CRAWL4AI_CHALLENGE_WAIT` | `8s` | Wait up to this long for a self-clearing bot challenge (a "Just a moment..." title) to pass before extraction. Pages without one pay nothing. Longer waits rarely clear more and make every blocked page that much slower to fail. `0` disables. Max `60s`. |
| `OMNIFEED_CRAWL4AI_MIN_PROSE_CHARS` | `100` | Reject a rendered page as `thin_content` when its readable text, without link targets, URLs and markdown syntax, is shorter than this. Catches navigation-only shells and empty app frames that would otherwise return as success. `0` disables. |
| `OMNIFEED_CRAWL4AI_TARGET_ELEMENTS` | _(unset, off)_ | Comma-separated CSS selectors. When set, the generic engine extracts markdown **only** from matching containers (crawl4ai `target_elements`). Good on article and repo pages, but a page with no match returns **empty content**, and the thin-content guard reports an error. Test it on your own URLs first. A starting list: `article, main, [role=main], .markdown-body, .post-content, #content`. |
| `OMNIFEED_SEARXNG_TIMEOUT` | `15s` | Per-query timeout to SearXNG |
| `OMNIFEED_SEARXNG_SITE_ENGINES` | _(unset, whole pool)_ | Comma-separated SearXNG engines to query when the caller passes a valid `site` filter, e.g. `privacywall,google cse`. SearXNG forwards `site:` to its engines. Some honor it, some ignore it and return unrelated pages, and some return **zero results**. List the ones that honor it. Unset queries every engine. |
| `OMNIFEED_SEARXNG_DELAY` | `0` (off) | Minimum gap between queries to SearXNG. This paces the **engines behind** SearXNG: each query fans out to every enabled engine, so each engine sees omnifeed's query rate, and one that finds it bot-like suspends itself or serves a CAPTCHA. A gap alone doesn't bound a burst, so pair it with `OMNIFEED_SEARXNG_QUOTA`. |
| `OMNIFEED_SEARXNG_QUOTA` | `0` (off) | Maximum queries in any rolling `OMNIFEED_SEARXNG_QUOTA_WINDOW`. Engines enforce two limits. One measured pool answered at a 3s gap from a quiet start, then blocked after about 20 queries in 85s because it counts requests per window. The delay sets the gap and the quota bounds the burst. Measure both on your own pool. |
| `OMNIFEED_SEARXNG_QUOTA_WINDOW` | `90s` | Rolling window for `OMNIFEED_SEARXNG_QUOTA`. Ignored when the quota is `0`. Must be above `0` when the quota is set. |
| `OMNIFEED_SEARXNG_CONCURRENCY` | `0` (off) | Maximum searches in flight to SearXNG across **all replicas**. A nonzero `OMNIFEED_SEARXNG_DELAY` alone serializes admissions, so the whole deployment runs one search at a time. Above `0`, the delay spaces sends and this sets the bound. With Redis, slots are TTL leases, so a pod that dies mid-search frees its slot. On one pool (2026-08-24), 16 concurrent ran clean and 24 got the strictest engine suspended for 1200s. Leave margin: overshooting costs a 20-minute engine outage, not one failed query. **A single instance needs no Redis.** With several, set `OMNIFEED_REDIS_URL`, or N instances send N times this. |
| `OMNIFEED_SEARXNG_MAX_WAIT` | `15s` | How long a query may wait in the pacing limiter before it gives up. A fanned-out caller that sees `context deadline exceeded` hit this queue limit, not a slow upstream. Raise it to trade latency for completions. |
| `OMNIFEED_REDIS_URL` | _(unset, off)_ | Share rate-limiter state through Redis, e.g. `redis://user:pass@redis:6379/2`, or `rediss://` for TLS. Unset, each replica paces alone, so N replicas send up to N times the configured rate, and `OMNIFEED_SEARXNG_QUOTA` and `OMNIFEED_PER_DOMAIN_DELAY` apply per pod. Set, the delay and quota count once for the deployment. If Redis is unreachable, the limiters pace in process and crawls keep working. `OMNIFEED_PER_DOMAIN_CONCURRENCY` stays per pod by design, so the replica count multiplies it. |
| `OMNIFEED_REDIS_KEY_PREFIX` | `omnifeed:ratelimit` | Key namespace, so several deployments can share one Redis without counting each other's requests. Every key has a TTL, which matters on a shared `noeviction` instance. |
| `OMNIFEED_REDIS_TIMEOUT` | `250ms` | Budget for **one Redis operation**. An acquisition can wait minutes for a quota window, and only each Redis call inside it gets this budget. On a breach the limiter paces in process for a 30s cooldown before trying Redis again, so a dead Redis costs one timeout per cooldown, not one per request. |
| `OMNIFEED_SEARCH_AUDIT` | `off` | Per-search audit log: `off`, `summary` or `full`. It is a data feed, **not a log level**. Putting it behind `debug` would force every component's debug output on, and invite sampling, which biases the per-engine statistics. `summary` logs one line per search with `query_id`, query, `site`, `time_range`, `total`, `duration_ms` and per-engine row counts, plus one line per unresponsive engine with `engine` and `reason_class`. `full` adds one line per engine and result with that engine's own rank, joined on `query_id`. Lines stay flat because log stores turn JSON arrays into opaque strings that can't be grouped or averaged. **Both** modes log the query, and `full` logs every result URL, so retention is your call. Lines log at INFO, so startup rejects any mode but `off` unless `OMNIFEED_LOG_LEVEL` is `debug` or `info`. The `omnifeed_search_engine_position_rank` and `omnifeed_search_engine_unique_results_total` metrics carry no queries or URLs and are always recorded. |
| `OMNIFEED_SEARCH_MAX_RESULTS` | `25` | Cap on the search `limit` argument, 1 to 100 |
| `OMNIFEED_FETCH_MAX_CHARS` | `120000` | Default cap on **markdown** returned by `fetch_url`. `0` is unlimited. Over the cap, the reply ends with a resumable truncation marker. TOON and JSON output from the dedicated engines is never cut; use their own limits. Doesn't apply to `/crawl`. See [Controlling fetched content size](#controlling-fetched-content-size). |
| `OMNIFEED_GITHUB_TOKEN` | _(unset)_ | Personal access token for the GitHub engine. Anonymous access allows 60 requests per hour per IP. A token raises it to 5000. A repository root costs 3 requests, most other pages 1. Discussions need it: they are read through GraphQL, which refuses anonymous callers, so without a token discussion URLs go to the generic fallback. A fine-grained token with no extra permissions (public repositories, read-only) is enough. |
| `OMNIFEED_DISCOURSE_HOSTS` | `meta.discourse.org,discuss.python.org,users.rust-lang.org,internals.rust-lang.org,discuss.pytorch.org` | Hostnames where the Discourse engine claims `/t/…` topic URLs. Discourse runs on **arbitrary** domains and the engine can't detect it, so **list the forums you use**. Matching is exact and case-insensitive, with no subdomain wildcards. Unlisted forums go through the generic browser fallback, which returns less of the thread. An **empty string** disables the engine. |
| `OMNIFEED_TWITTER_ENABLED` | `true` | Render X/Twitter post URLs (`x.com`, `twitter.com`, `fxtwitter.com`, `fixupx.com`, `vxtwitter.com`, `fixvx.com`, and `t.co` links that land on a post) with the Twitter engine. `false` sends them to the generic fallback, which loads `x.com` in crawl4ai. See [Twitter/X posts](#twitterx-posts). |
| `OMNIFEED_TWITTER_FXTWITTER_URL` | `https://api.fxtwitter.com` | Base URL of the FxTwitter API the engine reads first. Point it at a **self-hosted [FxEmbed](https://github.com/FxEmbed/FxEmbed)** to stop depending on the public instance. |
| `OMNIFEED_TWITTER_MAX_REPLIES` | `20` | Replies rendered under a post, from the first page FxTwitter returns (about 35). Must be at least `1`. |
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
| `OMNIFEED_REDDIT_QUOTA` | `0` (off) | Maximum requests to Reddit in any rolling `OMNIFEED_REDDIT_QUOTA_WINDOW`. It counts **every request**, not every crawl: the thread fetch, each `/api/morechildren` round, a subreddit listing and a share-link resolve each take one, so an `expand=full` crawl can take 41. Shared across replicas when `OMNIFEED_REDIS_URL` is set. Reddit's unauthenticated budget is about 10 requests per minute per IP in practice. On 2026-10-08 Reddit blocked after about 50 requests in 5 minutes, and once at about 3 per minute while other clients shared the egress IP. **Recommended: `6`** with the default `1m` window, or `3` if the IP is shared. A request refused for lack of quota fails with `quota_exhausted` and its `retry_after_s`, and nothing is sent. |
| `OMNIFEED_REDDIT_QUOTA_WINDOW` | `1m` | Rolling window for `OMNIFEED_REDDIT_QUOTA`. Ignored when the quota is `0`. Must be above `0` when the quota is set. |
| `OMNIFEED_MAX_URLS_PER_REQUEST` | `30` | Cap on `urls[]` length |
| `OMNIFEED_PER_DOMAIN_CONCURRENCY` | `2` | Max concurrent requests to one domain |
| `OMNIFEED_PER_DOMAIN_DELAY` | `1500ms` | Minimum gap between requests to one domain |
| `OMNIFEED_BLOCK_PRIVATE_IPS` | `true` | SSRF protection. Keep it on in production. |
| `OMNIFEED_ALLOWED_ORIGINS` | _(unset)_ | Comma-separated browser `Origin` values, e.g. `https://app.example.com`, allowed to call the HTTP APIs cross-origin. Requests with no `Origin`, which covers every native MCP client, always pass, as do loopback origins such as the MCP inspector. Anything else gets **403**. This is the DNS-rebinding guard the MCP transport spec requires, applied to every HTTP endpoint. |
| `OMNIFEED_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `OMNIFEED_LOG_FORMAT` | `json` | `json` or `text` |
| `OMNIFEED_ENABLE_PPROF` | `false` | Expose `/debug/pprof/*` |
| `OMNIFEED_CACHE_ENABLED` | `true` | Cache successful `fetch_url` and `POST /crawl` results. See [Response cache](#response-cache). |
| `OMNIFEED_CACHE_TTL_THREADS` | `10m` | How long a result from a dedicated engine (Reddit, Hacker News, Discourse, Bluesky, GitHub) stays cached. `0` turns caching off for them. |
| `OMNIFEED_CACHE_TTL_PAGES` | `30m` | How long a generic page (the crawl4ai engine) stays cached. `0` turns caching off for them. |
| `OMNIFEED_CACHE_MAX_BYTES` | `67108864` (64 MiB) | Size of the in-process LRU, used when `OMNIFEED_REDIS_URL` is unset, or when Redis refuses the cache's keys (see [Response cache](#response-cache)). Approximate: content plus metadata plus a small per-entry overhead. Per pod. |
| `OMNIFEED_CACHE_MAX_ITEM_BYTES` | `4194304` (4 MiB) | Largest single entry. In Redis it is the gzip-compressed size, in memory the raw size. A bigger result is still returned, just not cached. |
| `OMNIFEED_CACHE_KEY_PREFIX` | `omnifeed:cache` | Redis key namespace for cache entries, separate from the rate limiter's `OMNIFEED_REDIS_KEY_PREFIX`. A Redis ACL user must be allowed `~<prefix>:*` with `GET` and `SET` (see [Response cache](#response-cache)). |

### Retry-After propagation

When an upstream answers `429` or `503` with `Retry-After`, omnifeed holds that host
for the given duration, capped at 5 minutes, across later requests. With
`OMNIFEED_REDIS_URL` set, the hold spans replicas. It applies only to upstreams an
engine calls directly, because crawl4ai hides the crawled site's own `Retry-After`.
Reddit is the exception: its fetch runs inside the browser, so the engine reads
`Retry-After`, `X-Ratelimit-Reset` and `X-Ratelimit-Remaining` from the in-page
response. On a `429`, or when `X-Ratelimit-Remaining` reaches `0`, it holds
`www.reddit.com` for `Retry-After`, else `X-Ratelimit-Reset`, else 60s on a bare
`429`. The `429` error carries the wait as `retry_after_s`.
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

A long article can overflow an MCP client's per-response budget. The client then writes the text to a file and the model never reads it. So `fetch_url` caps **markdown** at `OMNIFEED_FETCH_MAX_CHARS`, 120000 by default and `0` for unlimited, and ends a cut reply with a resumable marker:

```
[omnifeed: content truncated at 120000 of 174233 characters. Call fetch_url again with start_char=120000 to continue.]
```

Both per-request parameters apply to **markdown only**:

| Param | Default | Purpose |
|---|---|---|
| `max_chars` | `OMNIFEED_FETCH_MAX_CHARS` | Max characters to return, up to `500000`. `0` or omitted uses the server default. |
| `start_char` | `0` | Character offset to start from. Pass the value from a truncation marker to read the next chunk. |

Offsets and lengths count **characters, not bytes**, so a window never splits a multibyte character. The marker counts against `max_chars`, so a reply never exceeds it. The response `_meta` carries the same numbers as `truncated`, `total_chars` and `next_start_char`. `fetch_url` declares `anthropic/maxResultSizeChars: 500000` in `tools/list`, which raises Claude Code's per-tool text limit to match. Other clients ignore it.

omnifeed **never truncates TOON or JSON output**, which covers every dedicated engine. Its length markers would describe rows that were cut, so those engines use their own element caps, described below.

`POST /crawl` has **no size parameters and never truncates**. RAG pipelines chunk and embed whatever they receive, so a cap would drop retrievable text, and one shared offset would break every other URL in a batch.

## Fetching infinite-scroll feeds

Generic crawls **don't scroll** by default. Scrolling costs seconds on every page, gains content only on append-style infinite feeds, and corrupts virtualized pages. When a feed, listing or gallery comes back missing items, opt in per request with `scan_full_page: true` on `fetch_url` or `POST /crawl?scan_full_page=true`. Both accept `false` too, which turns the scroll off where `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE` turns it on. `OMNIFEED_CRAWL4AI_SCROLL_DELAY` sets the pause between scroll steps.

## Thread totals in the body

AI agents read the TOON (or JSON) body, not the response `_meta`, so thread headers state how complete the comment list is:

- Hacker News `story`: `total_comments`, the live comments in Algolia's tree before any cap, and `truncated`, whether a cap dropped some of them.
- Reddit `post`: `total_comments`, Reddit's `num_comments` (it counts deleted and removed comments, so it can exceed what is fetchable); `returned_comments`, the comments emitted; `hidden_more`, the sum of the remaining `more` gaps' counts; and `truncated`, true when a cap dropped comments or any gap is left unexpanded.

`_meta` keeps its own fields (`comments`, `truncated_from`, `total_comments`, `gaps`) unchanged.

## Controlling Reddit response size

Reddit comment trees can be huge. There are two kinds of limit:

- **Reddit parameters**, forwarded as-is to Reddit's API: `OMNIFEED_REDDIT_FETCH_LIMIT` as `limit`, `OMNIFEED_REDDIT_DEPTH` as `depth`, and `OMNIFEED_REDDIT_SORT` as `sort`. They reduce what Reddit sends, which saves latency and tokens, but they are **approximate**, and `limit` and `depth` bound only the **initial** fetch. Reddit defines them in its [API docs](https://www.reddit.com/dev/api/) under `GET [/r/subreddit]/comments/article`: `limit` is "maximum number of comments to return" and `depth` is "maximum depth of subtrees".
- **omnifeed caps**, applied after fetch and expansion, so they are **exact**: `OMNIFEED_REDDIT_MAX_COMMENTS` cuts the flat comment list breadth-first (every top-level comment first, then their replies, then the next depth, never a reply without its parent, output order unchanged), and `OMNIFEED_REDDIT_MAX_TOP_LEVEL` keeps the first N top-level threads, in `sort` order, with their replies.

Use the Reddit parameters to fetch less. Use the caps for a guaranteed ceiling: `OMNIFEED_REDDIT_MAX_ROUNDS` expansion adds comments beyond `limit`, so only the caps bound the total. `fetch_url` accepts all five per request as `limit`, `depth`, `sort`, `max_comments` and `max_top_level`, and a positive value overrides the env default. They apply to **threads** only. A subreddit listing has no comment tree and takes its post count and time window from the URL: `?limit=`, 1 to 100 with a default of 25, and `?t=hour|day|week|month|year|all`, which Reddit applies only to `top` and `controversial`.

## Controlling Hacker News response size

The Algolia item API returns a Hacker News thread's **whole** tree in one response, and top-level subtree sizes are skewed. One measured thread had subtrees of 105, 55, 16, 13, 12, 11, 10, 6 and so on across 62 top-level threads. There are no upstream parameters to forward, only omnifeed caps applied after the fetch, all three per request on `fetch_url`:

| Param | Default | Purpose |
|---|---|---|
| `max_per_subtree` | unlimited | Max comments kept in **each** top-level thread, root included. Selection is breadth-first, so a thread keeps its root and shallow replies before its deep tail. |
| `max_top_level` | unlimited | Keep the first N top-level threads, in HN's order, with all their replies. |
| `max_comments` | 500 | Ceiling on the flat comment list, applied last. `0` or omitted means 500; a caller can ask for up to `5000` (Algolia already returned the whole tree, so this costs no extra request). The cut is breadth-first across the whole thread. |

Start with `max_per_subtree`, which fits that skew. On two real threads, `max_per_subtree=12` returned about 56% and 47% of the full-tree bytes and kept 9 of 12 and 11 of 14 of the comments a human rated substantive. `max_comments` is one breadth-first cut over the whole thread: every top-level comment first, then their direct replies, then the next depth. It used to keep the first N comments in pre-order, which spent most of the budget in the biggest branch: at 100 comments that covered 4 of 62 top-level threads, with 72% of its comments in one subtree.

Hacker News has **no depth cap** on purpose, because depth says nothing about quality there. The most valuable comment in both measured threads sat at depth 7, and adding `depth<=5` to a subtree cap lost real content while saving under 1k of 84k characters. `depth` and `sort` are Reddit-only and ignored on HN URLs.

Caps never reorder output. Comments stay in HN's order, and breadth-first selection keeps each comment's ancestors, so the `parent_id` chain never breaks.

## Dedicated-engine fallback

When a dedicated engine (Reddit, Hacker News, GitHub, Discourse, Bluesky) fails,
the generic browser engine renders the same URL instead, with three exceptions:

- **The caller passed `format`** (`json` or `toon`, on `fetch_url` or
  `POST /crawl`). Passing it means the caller parses the reply, so it gets the
  engine's error rather than markdown in the same success shape. This holds for
  every engine and every failure.
- **A same-host engine got a block or rate verdict**: `http_429`, `http_403`,
  `captcha`, `bot_block`, or omnifeed's own `quota_exhausted`. Reddit and
  Discourse read the page's own host (`www.reddit.com`, the forum), so a browser
  render would hit the host that just refused us, or that our pacing is holding
  back, and prolong the block. The engine's error is returned.
- **The engine marked its error final** (`domain.NoFallback`): the Twitter/X
  engine already tried the generic render itself, so a second one is skipped.

GitHub, Hacker News and Bluesky read a separate API host (`api.github.com`,
`hn.algolia.com`, the Bluesky AppView). Their blocks and quota refusals are about
that API host, not the page, so they still fall back: an anonymous GitHub
deployment over its 60 requests per hour renders `github.com` instead. An engine
declares the page's host by implementing `domain.SameHostEngine`.

A fallback result is marked. `_meta` (or the loader's `metadata`) carries
`fallback_from` (the engine that failed) and `fallback_reason` (its failure kind),
and the body starts with one line:
`> Note: the dedicated reddit engine failed (timeout); this is the generic page render instead.`

Errors reach MCP callers as a tool result with `isError: true` whose text is
`fetch_url failed: <reason> (HTTP <status>): <cause> [<code>] Retryable after Ns.`
(the back-off is stated once; `retry_after_s` is also in `structuredContent`).
See [errors.md](errors.md).

## Twitter/X posts

x.com walls headless browsers, and crawl4ai fails every `twitter.com` link (the
anti-bot verdict is a 39-byte page), so the Twitter engine reads public JSON mirrors
directly, in order, and stops at the first that answers:

1. **FxTwitter API v2**: `/2/conversation/{id}` for the post, its parents and the first
   page of replies, plus `/2/thread/{id}` when the post belongs to the author's
   self-thread. Covers long posts, X Articles, quotes, media alt text, community notes,
   polls and link cards.
2. **Embed syndication** (`cdn.syndication.twimg.com/tweet-result`). It cuts long posts at
   about 275 characters, so for those the engine asks **vxTwitter** for the full text. If
   that fails too, the cut text is served with `text_truncated: true` in `_meta` and a
   note in the body. vxTwitter alone is tried if syndication fails.
3. **crawl4ai** on the canonical `https://x.com/i/status/{id}`, which X renders for
   logged-out readers. X's own error pages are rejected, not returned.

If every source fails, the error says **tweet unavailable** and why: not found (FxTwitter
and a second source both answered 404: deleted, protected or suspended), blocked or rate
limited, or the last source's failure. The error is final: the registry does not render
the URL a second time. `t.co` links are resolved with one `HEAD` whose redirect is **not
followed**; the destination passes the SSRF check, then renders as a post or, for any
other site, through the generic engine. A `twitter.com` URL the engine does not claim, such
as a profile, is rewritten to `x.com` before crawl4ai sees it.

Output is **markdown** by default: a header line (`@handle (Name) · date · x.com URL ·
likes · reposts · replies · views`), the text with `t.co` links expanded, the quoted post
as a blockquote, media as `[image: alt]` or `[video, 1:23]`, poll, community note, link
card, the X Article body, the self-thread numbered, and the top replies as
`@handle (N likes): text`, with the author's own replies marked `[author]`. Pass
`format: toon` or `json` for the same data structured. `_meta` carries `upstream`
(`fxtwitter`, `syndication`, `syndication+vxtwitter`, `vxtwitter` or `crawl4ai`),
`replies`, `reply_total`, `thread_posts` and `text_truncated`. Fetched posts are cached in
process for 15 minutes.

FxTwitter, vxTwitter and the syndication endpoint are third-party or unofficial services X
can break or pressure at any time. A self-hosted FxEmbed
behind `OMNIFEED_TWITTER_FXTWITTER_URL` removes the first dependency.

## Reddit anti-bot handling

Reddit's edge fingerprints the TLS/JA3 handshake and 403-blocks non-browser HTTP clients, so the Reddit engine never calls Reddit directly. It drives a **real headless browser** to a `www.reddit.com` page, which clears the bot wall, then runs a **same-origin `fetch()`** of the `.json` and `/api/morechildren` endpoints from inside it. It needs no Reddit auth, cookies or API key. The default browser is crawl4ai, through its token-gated **`POST /execute_js`** endpoint, so crawl4ai must run with `CRAWL4AI_EXECUTE_JS_ENABLED=true`, and `OMNIFEED_CRAWL4AI_TOKEN` must match its `CRAWL4AI_API_TOKEN`.

> Sustained scraping can raise your IP's risk score. If fetches return the block page, slow down, keep `expand` modest, or route the browser through a residential proxy.

## Response cache

The same thread is often fetched again within minutes: a retry job re-reads
the threads it already has, and agents re-open URLs they fetched a turn ago.
Every repeat of a Reddit thread spends Reddit's per-IP budget, which is small
(on 2026-10-08 it ran out and the egress IP was blocked). So omnifeed caches
successful results and serves repeats from the cache.

- **What is cached.** Only complete, successful documents. Errors, generic
  fallback renders (`_meta.fallback_from` set) and partial Reddit threads
  (`_meta.partial`, see below) are never cached, so the next call tries again.
- **How long.** `OMNIFEED_CACHE_TTL_THREADS` (10 minutes) for the dedicated
  engines, `OMNIFEED_CACHE_TTL_PAGES` (30 minutes) for generic pages.
- **The key.** The normalized URL (scheme and host lowercased, default port and
  `#fragment` dropped, query parameters sorted; the path is kept as-is) plus
  every option that reaches the engine: `format`, `expand`, `limit`, `depth`,
  `sort`, `max_comments`, `max_top_level`, `max_per_subtree`, `scan_full_page`
  and the Reddit defaults. `max_chars` and `start_char` are **not** part of the
  key: `fetch_url` cuts its character window from the whole cached document, so
  paging through a long page with `start_char` costs one upstream fetch.
- **Where.** In Redis when `OMNIFEED_REDIS_URL` is set, so every replica serves
  every other replica's fetches. Entries are gzip-compressed and expire through
  a Redis TTL. Without Redis, an in-process LRU of `OMNIFEED_CACHE_MAX_BYTES`
  per pod.
- **Redis permissions.** The Redis user needs the key pattern
  `~<OMNIFEED_CACHE_KEY_PREFIX>:*` (default `~omnifeed:cache:*`) and the
  commands `GET` and `SET`, on top of whatever the rate limiter needs under
  `OMNIFEED_REDIS_KEY_PREFIX`. A user created for the rate limiter alone
  (`~omnifeed:ratelimit:*`) is refused every cache command with `NOPERM`. For
  example: `ACL SETUSER omnifeed ~omnifeed:ratelimit:* ~omnifeed:cache:* +get +set …`.
- **Startup check.** With Redis, omnifeed writes a probe key
  (`<prefix>:probe:<n>`, 10-second TTL, never deleted) and reads it back, each
  command bounded by `OMNIFEED_REDIS_TIMEOUT`. If Redis refuses (`NOPERM`,
  `NOAUTH`, `WRONGPASS`) or gives any other answer that will not change on
  retry (e.g. `READONLY`, or the value is not read back), the pod logs one
  `ERROR` naming the prefix to grant and uses the in-process LRU instead. If
  Redis does not answer or asks to retry (timeout, connection refused, `LOADING`,
  `OOM`, `MISCONF`), the pod keeps
  the Redis backend and the rule below applies until it does.
- **Permission errors at runtime.** A `NOPERM`/`NOAUTH`/`WRONGPASS` on a later
  `GET` or `SET` (an ACL tightened under a running pod) does the same: one
  `ERROR`, then the in-process LRU for the rest of the process's life, starting
  with the request that hit the error. Restart the pods after fixing the ACL to
  share the cache again. `omnifeed_cache_backend{backend="memory"} 1` with
  `OMNIFEED_REDIS_URL` set means this happened.
- **When Redis fails** (any other error). The lookup counts as `result="error"` and the request is
  served as a miss, never failed. After a failure, the cache skips Redis for 30
  seconds, so a dead Redis costs one `OMNIFEED_REDIS_TIMEOUT` per 30 seconds,
  not one per request.
- **Concurrent identical requests** share one upstream fetch. If that fetch
  fails, all of them get its error, because the upstream that refused one would
  refuse the others. The exception is when the first caller hangs up: then the
  others fetch for themselves.

Every response from the cache layer carries `_meta.cache` (or `metadata.cache`
on `POST /crawl`): `hit` (served from the cache, or shared with an identical
in-flight request), `miss` (fetched upstream) or `bypass`. `_meta.cached_at` is
the time (RFC 3339, UTC) the content was fetched and stored. It is absent when the
response is not in the cache.

**Bypass.** Pass `no_cache: true` to `fetch_url`, or `POST /crawl?no_cache=true`
(or `=1`), to skip the cache and fetch fresh. The fresh result still replaces
the cached copy. Use it only when `cached_at` is too old for the job: every
uncached Reddit fetch spends the per-IP budget.

## Partial Reddit threads

A thread whose `/api/morechildren` expansion is cut short, because a round was
blocked, rate limited, timed out or returned something unparseable, is still
returned with the comments loaded so far. omnifeed flags it:

- `_meta.partial: "true"`, `_meta.partial_reason` with the failure kind (e.g.
  `http_429`, `timeout`, `bot_block`, `parse_error`), and
  `_meta.missing_replies` with the count still behind `more` gaps.
- The body starts with a note an agent reading only text will see. In TOON it is
  the first line, `note: 212 more replies could not be loaded (http_429)`. In
  JSON it is a top-level `"note"` field.

A thread that simply used up its `expand` budget is not partial. Partial
threads are never cached.

## Timeouts

Each engine has its own time budget. A client calling omnifeed should wait
**longer** than the budget of the engine it is calling, or it gives up on a
request that would have succeeded and, for Reddit, spends the rate budget for
nothing.

| Engine | Budget | Set by | Notes |
|---|---|---|---|
| Hacker News | 30 s per crawl | fixed (`internal/engine/hackernews`) | Wall clock for the whole crawl, including the pacing wait and retries against `hn.algolia.com`. |
| GitHub, Discourse, Bluesky | 30 s per crawl | fixed, per engine | Same shape as Hacker News. |
| Reddit | 4 min per crawl | `OMNIFEED_REDDIT_TIMEOUT` | Wall clock for the whole crawl: pacing wait, share-link resolve, thread fetch and every `morechildren` round. A round cut off by the deadline returns what was loaded, flagged [partial](#partial-reddit-threads). Each browser call inside it is also bounded by `OMNIFEED_CRAWL4AI_TIMEOUT`. |
| Generic page (crawl4ai) | 90 s per crawl4ai call | `OMNIFEED_CRAWL4AI_TIMEOUT` | Inside each call, crawl4ai's own `page_timeout` is 60 s (the most crawl4ai accepts over REST), so crawl4ai normally answers before omnifeed's 90 s run out. There is no overall cap on a generic crawl: a transient failure is retried once, a Playwright navigation race once with `networkidle`, a blocked page once under the alternate browser identity, a thin page once without the excluded selector, and a page that turns out to be a PDF once through crawl4ai's PDF strategy, so the worst case is several 90 s calls. |
| SearXNG search | 15 s per attempt | `OMNIFEED_SEARXNG_TIMEOUT` | Up to 3 attempts with backoff, after a pacing wait of up to `OMNIFEED_SEARXNG_MAX_WAIT` (15 s by default). |

Suggested client timeouts:

- **Reddit: at least 270 s** (the 4-minute budget plus margin for the pacing
  queue and the response). If you raise `OMNIFEED_REDDIT_TIMEOUT`, raise the
  client by the same amount.
- **Generic pages: at least 200 s.** Most pages take seconds, but a slow page
  with a retry can take two crawl4ai calls.
- **Hacker News, GitHub, Discourse, Bluesky: at least 45 s.**
- **web_search / `POST /search`: at least 75 s.**

The loader (`/crawl`, `/search`) and MCP listeners stop writing a response after
300 s, so waiting longer than that gains nothing, and an
`OMNIFEED_REDDIT_TIMEOUT` above about 4m30s gets cut by the server first. A
cached result returns in milliseconds whatever the engine.

## Raw-text bypass

Raw code, JSON, markdown and plain text have nothing for a browser to render, and Chromium's page-idle wait makes them slow: a raw `githubusercontent.com` file takes 30 to 39 s in the browser and about 200 ms direct. When a URL's extension looks raw (`.md`, `.txt`, `.json`, source files), the generic engine sends a HEAD request. If the server confirms a non-HTML text type, a plain GET fetches the body and returns it unchanged. Anything uncertain, such as a failed probe, `text/html`, binary bytes or blocked egress, falls back to the browser. With `OMNIFEED_BLOCK_PRIVATE_IPS` on, direct fetches refuse private and reserved addresses **when dialing**, so DNS rebinding can't bypass URL validation. This needs outbound access to the **target sites**, not just crawl4ai. Without it the probe fails and everything goes through crawl4ai.

## Prometheus metrics

Served at `/metrics` on `OMNIFEED_METRICS_ADDR`, default `:9090`, alongside the Go and process collectors:

| Metric | Type | Labels | What it measures |
|---|---|---|---|
| `omnifeed_requests_total` | counter | `engine, tenant, status, reason` | Crawl requests, with a bounded failure `reason`, `ok` on success |
| `omnifeed_request_seconds` | histogram | `engine, status, reason` | End-to-end crawl latency |
| `omnifeed_request_attempts_total` | counter | `upstream, attempt` | HTTP attempts by the retrying client, `first` or `retry` |
| `omnifeed_upstream_seconds` | histogram | `upstream, op, status` | Upstream round-trip per attempt, from start until the body is read: `crawl4ai/crawl`, `crawl4ai/execute_js`, `searxng/search`, `searxng/config`, `github/api`, `hackernews/api`, `discourse/api` |
| `omnifeed_domain_limiter_wait_seconds` | histogram | `engine, outcome` | Time blocked acquiring the per-domain limiter, semaphore plus delay. `outcome="canceled"`: the wait died in the queue. `outcome="budget_exceeded"`: the wait exceeded the caller's remaining deadline, so nothing queued; about 0 seconds, with `reason="quota_exhausted"` on the request metric |
| `omnifeed_ratelimit_backend_errors_total` | counter | `op` | Failed Redis operations in the distributed limiter. On `acquire` the limiter paces in process. `release` and `penalize` failures cost only pacing accuracy |
| `omnifeed_ratelimit_penalties_total` | counter | `upstream` | Upstream `Retry-After` headers, on 429 or 503, turned into a hold on that host. `upstream="reddit"` counts Reddit's own back-off headers read inside the browser. This often precedes a CAPTCHA or block |
| `omnifeed_ratelimit_degraded` | gauge | `scope` | `1` while pacing falls back to per-pod limits because Redis is unreachable, `0` while shared. Exists only when `OMNIFEED_REDIS_URL` is set, published at `0` on startup. One series per limiter scope, `domain` for crawling, `searxng` for queries and `reddit` for the Reddit request quota when set, each degrading and recovering on its own |
| `omnifeed_response_chars` | histogram | `engine` | Engine output length before any `max_chars` truncation, successful crawls only |
| `omnifeed_engine_fallbacks_total` | counter | `from_engine, reason` | Dedicated-engine failures re-crawled by the generic fallback. Block and rate reasons appear only for separate-host engines (GitHub, Hacker News, Bluesky), see [Dedicated-engine fallback](#dedicated-engine-fallback) |
| `omnifeed_searxng_unresponsive_engines_total` | counter | `engine, error` | Engines SearXNG reported unresponsive, per search. `error` is one of `timeout`, `captcha`, `suspended`, `too_many_requests`, `access_denied`, `error`, `unknown` |
| `omnifeed_searxng_engine_results_total` | counter | `engine` | Result rows per SearXNG engine. A blocked engine keeps answering 200 with zero results, so its series goes flat while the rest of the pool moves. Alert on that divergence. Starts at 0 at startup for known engines (see below). Pair it with `absent_over_time()` all the same: an engine nothing names at startup has no series until a response names it |
| `omnifeed_searxng_queries_total` | counter | `scoped` | Queries **sent** to SearXNG after the limiter, the rate the engines see. Compare with `omnifeed_search_requests_total` to see what pacing refused |
| `omnifeed_searxng_engine_zero_results_total` | counter | `engine` | Searches where one engine returned no rows **while the search had results**: a silent block, per engine. Excludes engines SearXNG reported unresponsive. Counts only engines seen answering before, so pair with `absent_over_time()` |
| `omnifeed_searxng_empty_searches_total` | counter | `scoped` | Searches with zero results **and** no unresponsive-engine report. `scoped="true"` is a `site:` query, where silent blocks concentrate |
| `omnifeed_reddit_expansion_rounds` | histogram | none | `/api/morechildren` rounds per Reddit crawl |
| `omnifeed_search_requests_total` | counter | `searcher, status, reason`, `scoped` | Search queries as **callers** sent them, refused ones included, unlike `omnifeed_searxng_queries_total`. `scoped="true"` is a `site:` query. `sum()` queries ignore the added label. Queries matching an exact label set don't |
| `omnifeed_search_request_seconds` | histogram | `searcher, status` | Search latency |
| `omnifeed_search_engine_position_rank` | histogram | `engine` | The rank each engine gave each row it returned. 1 to 3 is a result a caller reads, 20 and above is filler |
| `omnifeed_search_engine_unique_results_total` | counter | `engine` | Results no other engine returned, which shows whether an engine earns its slot |
| `omnifeed_cache_requests_total` | counter | `result` | [Response cache](#response-cache) lookups: `hit` (served from the cache or shared with an identical in-flight fetch), `miss` (fetched upstream), `bypass` (`no_cache`), `error` (the cache backend failed, served as a miss). `hit / (hit + miss)` is the hit rate |
| `omnifeed_cache_bytes` | gauge | none | Approximate bytes in the in-process cache. Only with the in-memory backend: the Redis backend is shared and not measured per pod |
| `omnifeed_cache_backend` | gauge | `backend` | `1` on the [response cache](#response-cache)'s active backend (`redis` or `memory`), `0` on the other. `memory` while `OMNIFEED_REDIS_URL` is set means Redis refused the cache's keys and this pod fell back to its in-process LRU |
| `omnifeed_cache_backend_errors_total` | counter | `op`, `kind` | Response-cache Redis errors by `op` (`get`, `set`, `probe`) and `kind`: `noperm` (ACL or auth refusal: the pod switches to memory), `transient` (timeout, refused connection, `LOADING`…: 30-second cooldown, then retry), `other` (startup probe only: an answer the cache cannot use, e.g. `READONLY`) |

**Per-engine SearXNG series start at 0.** `increase()` and `rate()` in Prometheus and VictoriaMetrics do not reliably count a counter's first sample. A series created on a pod's first search, at say 20 rows, reads as no increase at all, so after every restart the first results per engine went missing. That made an "engine silent" alert such as `sum(increase(omnifeed_searxng_engine_results_total{engine="google cse"}[6h])) == 0` fire after deploys. At startup omnifeed therefore creates every per-engine series at 0: `omnifeed_searxng_engine_results_total`, `omnifeed_searxng_engine_zero_results_total`, `omnifeed_search_engine_unique_results_total`, `omnifeed_search_engine_position_rank` (`_count` 0) and `omnifeed_searxng_unresponsive_engines_total` for each `error` value. This happens at once for the engines in `OMNIFEED_SEARXNG_SITE_ENGINES`. For every engine SearXNG reports `enabled` in `GET /config` **in the `general` category**, it happens in the background. It is retried with backoff for up to 5 minutes, because SearXNG may start later, and never delays readiness. Only `general` counts because omnifeed sends no `categories`, so SearXNG runs only `general` engines. Engines in other categories (images, music, maps…) would only add series that never move. If `/config` does not answer, omnifeed logs one WARN. Engines then get their series the first time a response names them, as before.
