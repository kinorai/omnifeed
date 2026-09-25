# Ideas and parked work

Ideas considered for omnifeed that are **not** in the code, and why. One entry per idea,
newest first.

A rejected idea should stay rejected for a reason, and a good idea that came at the
wrong time should be findable. Write an entry when the reasoning cost more than the
code.

**Format.** Each entry states the idea, the evidence for and against it, what would
justify revisiting it, and, if code was written, the git ref that holds it. Without a
ref, nobody can recover the code.

---

## Server-side article-fetch fallback chain

**Status.** Parked 2026-08-22, never built. No code, no git ref.

### The idea

When `fetch_url` fails on an article, the caller has no second option. A digest of
"what a community thinks of X" needs X, so the caller writes the retry chain into its
own prompt: try the URL, and on failure try a reader proxy such as
`https://r.jina.ai/{url}`.

The idea moves that chain into the engine registry: crawl4ai direct, then a
reader-proxy fallback, then give up. The response adds a provenance field
(`source: direct|jina`, `degraded: bool`) so the caller can report the article as
unreachable instead of implying it read one.

### The evidence for it

Measured 2026-08-21 on three Hacker News front-page URLs:

| reader | BBC (soft paywall) | WSJ (hard paywall) | Substack (JS) |
|---|---|---|---|
| crawl4ai direct | fail | fail (500) | **pass**, clean |
| `r.jina.ai/{url}` | **pass** | soft-fail: HTTP 200, CAPTCHA warning, empty body | **pass**, adds `Published Time` |
| `web.archive.org/web/2/{url}` | no snapshot | fail 503 | fail 503 |
| `md.dhr.wtf/?url=` | fail | fail (401) | fail |
| `archive.is/newest/{url}` | CAPTCHA | CAPTCHA | CAPTCHA |

The WSJ soft-fail decides it. `r.jina.ai` returned **HTTP 200** with an empty body, so a
caller-side "on failure, try the proxy" rule never fires, and the caller summarizes an
empty article. omnifeed already classifies this: `internal/antibot` detects block and
CAPTCHA pages, and it correctly turned the direct WSJ attempt into a 500.

A chain in a caller's prompt can't be observed, so nobody can say how often the
fallback helped. In omnifeed it would be a `reason` label on an existing counter,
and `internal/observability` already classifies crawl failures, so existing alerts
would cover it. Comparable tools work this way. Firecrawl's `proxy: auto` escalates
from basic to enhanced on the server and returns one `warning` field, and Jina reports
its escalation in `X-Engine`.

### The evidence against it

It makes a third-party reader service a hard dependency of the fetch path. `r.jina.ai`
allows 20 requests per minute without a key, 500 with a free one, and averages 7.9s,
so a serial chain nears 20s at worst. That suits a daily batch, not interactive use.

Rejected outright, so nobody retries them:

- **Wayback.** The Internet Archive throttles this cluster's egress IP. Every attempt
  returned the same 11,974-byte 503 anti-bot body, and so did a control fetch of
  `web.archive.org/web/2020/https://example.com`. Unthrottled, it would help only for
  pages someone already archived, which for a fresh front-page link is a coin flip.
- **archive.is.** CAPTCHA-walled.
- **Self-hosted Firecrawl.** Its self-hosting doc excludes Fire-engine, the anti-bot
  layer: "Run and configure that service separately; it is not included." What's left
  is a Playwright fetch, which crawl4ai already does. Firecrawl doesn't claim paywall
  bypass either. Its engineering blog says "Pages behind logins, paywalls, or session
  tokens require persistent browser state that a stateless scraper can't hold."
- **`md.dhr.wtf`, `urltotext`.** Failed on a page that works direct, or paid only.
- **`urlreader.dev`, `reader.tsuki.dev`, `textance`.** The domains don't resolve.

### What would justify building it

Any one of:

1. A caller hits the soft-fail in production: a 200 with no article body that it
   can't detect. Only this fixes that.
2. More than one caller needs the chain. One caller can keep it in a prompt. Two means
   it belongs in the server.
3. Someone wants the provenance field itself, so output can say an article was
   unreachable.

If built, bound the third-party dependency with a hard timeout and a circuit breaker,
and use it only as a fallback, never the primary path.

---

## Rebuild the Hacker News comment tree from Algolia comment search

**Status.** Parked 2026-08-22, never built. No code, no git ref.

### The idea

The Hacker News engine builds its tree from Algolia `/items/{id}`, which returns nested
`children`. Algolia leaves dead (flagged) comments out of that tree, and because the
tree is nested, dropping a node **drops its whole subtree**, live replies included.

The fix builds from the flat endpoint,
`/search?tags=comment,story_<id>&hitsPerPage=1000`, and rebuilds the hierarchy from
`parent_id`. It has every field the parser needs.

### The evidence

Measured 2026-08-21 on story `49371857`:

| source | comments |
|---|---|
| Algolia `/items/{id}` (current) | 503 |
| Algolia comment search | 573 |
| Firebase `descendants` | 573 |
| HN HTML rows | 574 |

71 live comments, about 12%, were missing, including a 70-reply argument under one
`[flagged]` parent. Firebase confirms it: the dead node `49372309` has `dead: True`,
its parent is in the tree, and its child `49372670` is not. Latency is the same,
0.79 to 0.83s against 0.76 to 0.81s, and one page at `hitsPerPage=1000` covered the
whole thread.

### Why it is parked

The caller who asked wanted the **flagged comment itself**, not its replies. Neither
Algolia endpoint returns it, because comment search also excludes the dead node.
Firebase returns `dead: True` with the text `[flagged]`. That leaves HN's own HTML,
where that thread has 73 `noshow` rows that may carry the real text. That is
unverified, and it needs egress to `news.ycombinator.com`, which the network policy
denies.

Without the flagged text, the caller judged the live replies alone not worth the
change. That is a product call. The 71 live comments are real content and cheap to
recover.

### What would justify building it

Any one of:

1. A caller wants complete threads, or an accurate `total_comments` field. Today a
   consumer can't tell a gutted thread from a complete one.
2. Someone confirms HN's `noshow` rows carry the flagged text. The HTML fetch would
   then also give HN's own comment ranking, since Algolia children are strictly
   id-ascending and every cap selects chronologically, and per-comment downvotes from
   the CSS colour class.
3. Someone finds a thread where the dropped subtree changed a summary's conclusion.

---

## Vertical search: query a site's own search API instead of the web

**Status.** Built, shipped in 0.22.0, removed 2026-08-19.
**Code.** `git show v0.22.0 -- internal/search/router internal/search/hackernews
internal/search/reddit internal/search/bluesky`. The sitewide Reddit variant is on
branch `feat/reddit-sitewide-search` (PR #34, closed).

### The idea

`web_search(site=X)` goes to a web engine, which ranks X's pages by whatever it
scraped. X's own search often ranks its content better and exposes signals no scraper
has: Hacker News points, Reddit score and comment count, Bluesky likes. The design was
a `domain.Searcher` per site, a router dispatching on `SearchOptions.Site`, and SearXNG
as the fallback whenever a vertical declined, returned nothing or failed.

Three were built: Hacker News (Algolia), Reddit (in-site search through the browser
port) and Bluesky (`app.bsky.feed.searchPosts`).

### Why it was removed

The code was tested and worked. The problem it solved turned out to be one
deployment's engine pool **configuration**, and fixing that made the verticals redundant
there.

Measured 2026-08-18, four queries, `site=reddit.com`:

| query | Reddit in-site (sitewide) | Reddit in-site (`r/<sub>`) | web `site:reddit.com` |
|---|---|---|---|
| longhorn disk pressure | **0/7**: Windows Longhorn, disc golf | 5/5 on topic | **6/8** |
| best self hosted rss reader | 2/5 | 4/7 | **7/8** |
| wireguard mtu slow mobile | 3/4 | 3/6 | **6/6** |
| opus 5 coding (past week) | **5/5 fresh** | **6/6 fresh** | **0 results** |

Web search beat Reddit's own search on three of four. Sitewide in-site search collapses
on an ambiguous term that has a large non-technical following on the site. It wins on
recency, where the web engine returned nothing.

Two caveats:

- **Hacker News and Bluesky were never benchmarked.** They were removed because
  production never called either one (`omnifeed_search_routes_total` had no series
  for them), not because they lost a measurement. Nobody has tested whether Algolia
  beats a scraper for HN, or whether web engines barely index `bsky.app`.
- The recency win was real. It went away only because that deployment added a search
  engine with working date filters.

### What would justify bringing it back

Any one of:

1. A deployment with no engine that both honors `site:` and indexes recent content.
   The verticals were built for that case, which is common for a keyless self-hosted
   pool.
2. A measurement where HN Algolia or Bluesky `searchPosts` beats a good web engine.
   Nobody knows yet.
3. A caller that needs the site's ranking signals, such as points, score or comment
   count, as data rather than as ordering. No web engine exposes them.

### What to do differently if it returns

Don't prefer a vertical unconditionally. Verticals win on recency and lose elsewhere,
so the router should use a vertical when `SearchOptions.TimeRange` is set and skip it
otherwise, per vertical. That was designed and never built, and it is the change that
would have made the feature correct.
