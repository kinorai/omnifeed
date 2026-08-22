# Ideas and parked work

Things considered for omnifeed that are **not** in the code, and why. One entry
per idea, newest first.

This exists so a rejected idea stays rejected for a reason, and a good idea that
arrived at the wrong time can be found again. An entry is worth writing when the
reasoning cost more than the code did.

**Format.** Each entry states what the idea is, what evidence exists for or
against it, what would have to be true to revisit it, and — if code was ever
written — the git ref that still holds it. An entry with no way back to the code
is a regret, not a record.

---

## Server-side article-fetch fallback chain

**Status:** parked 2026-08-22, never built. No code, no git ref.

### The idea

`fetch_url` on an article either works or fails. The caller then has no second
option. A digest that summarizes "what a community thinks of X" needs X, so the
caller ends up writing the retry chain into its own prompt: try the URL, and on
failure try a reader proxy such as `https://r.jina.ai/{url}`.

The idea is to move that chain into the engine registry. crawl4ai direct, then a
reader-proxy fallback on failure, then give up. Return the content plus a
provenance field (`source: direct|jina`, `degraded: bool`) so the caller can say
the article was unreachable instead of implying it read one.

### The evidence for it

Measured 2026-08-21, three real Hacker News front-page URLs:

| reader | BBC (soft paywall) | WSJ (hard paywall) | Substack (JS) |
|---|---|---|---|
| crawl4ai direct | fail | fail (500) | **pass**, clean |
| `r.jina.ai/{url}` | **pass** | soft-fail: HTTP 200, CAPTCHA warning, empty body | **pass**, adds `Published Time` |
| `web.archive.org/web/2/{url}` | no snapshot exists | fail 503 | fail 503 |
| `md.dhr.wtf/?url=` | fail | fail (401) | fail |
| `archive.is/newest/{url}` | CAPTCHA | CAPTCHA | CAPTCHA |

The decisive line is the WSJ soft-fail. `r.jina.ai` returned **HTTP 200** with an
empty body. A caller-side rule that says "on failure, try the proxy" never fires,
because nothing failed. The caller then summarizes an empty article. Classifying
that is exactly this codebase's job: `internal/antibot` already detects block and
CAPTCHA pages, and the WSJ direct attempt was correctly demoted to a 500.

Three supporting arguments. A chain in a caller's prompt is unobservable, so
"how often did the fallback save us" is unanswerable; here it is a `reason` label
on an existing counter. `internal/observability` already classifies crawl
failures, so the chain extends existing alerting for free. And the comparable
tools are built this way: Firecrawl's `proxy: auto` escalates basic to enhanced
server-side and exposes a single `warning` field, and Jina reports its own
escalation in `X-Engine`. Both keep the chain in the server and hand the caller
one hint.

### The evidence against it

It puts a hard dependency on a third-party reader service in the fetch path.
`r.jina.ai` is 20 RPM keyless (500 with a free key) and averages 7.9s, so a
serial chain approaches ~20s worst case. Acceptable for a daily batch, not for
anything interactive.

Rejected outright, with reasons, so they are not retried:

- **Wayback** — the Internet Archive throttles this cluster's egress IP. Every
  attempt returned an identical 11,974-byte 503 anti-bot body, and a control
  fetch of `web.archive.org/web/2020/https://example.com` also 503'd. Even
  unthrottled it only helps for pages someone already archived, which for a
  fresh front-page link is a coin flip.
- **archive.is** — CAPTCHA-walled.
- **Self-hosted Firecrawl** — its own self-hosting doc excludes Fire-engine, the
  anti-bot layer ("Run and configure that service separately; it is not
  included"). What remains is a Playwright fetch, which is where crawl4ai
  already is. It also does not claim paywall bypass: their engineering blog says
  "Pages behind logins, paywalls, or session tokens require persistent browser
  state that a stateless scraper can't hold."
- **`md.dhr.wtf`, `urltotext`** — failed on the page that works bare, or paid only.
- `urlreader.dev`, `reader.tsuki.dev`, `textance` — the domains do not resolve.

### What would have to be true to build it

Any one of:

1. A caller hits the soft-fail case in production, i.e. gets a 200 with no
   article body and cannot tell. That is the failure this fixes and nothing else
   does.
2. More than one caller needs the chain. One caller can carry it in a prompt;
   two means the logic belongs in the server.
3. The provenance field is wanted for its own sake, so output can state honestly
   that an article was unreachable.

If it is built, bound the third-party dependency: hard timeout, circuit breaker,
and fallback only, never the primary path.

---

## Rebuild the Hacker News comment tree from Algolia comment search

**Status:** parked 2026-08-22, never built. No code, no git ref.

### The idea

The Hacker News engine builds its tree from Algolia `/items/{id}`, which returns
nested `children`. Algolia excludes dead (flagged) comments from that tree, and
because the structure is nested, excluding a node **drops its whole subtree** —
including replies that are alive.

The fix is to build from the flat endpoint instead:
`/search?tags=comment,story_<id>&hitsPerPage=1000`, then rebuild the hierarchy
from `parent_id`. Every field the parser needs is present.

### The evidence

Measured 2026-08-21 on story `49371857`:

| source | comments |
|---|---|
| Algolia `/items/{id}` (current) | 503 |
| Algolia comment search | 573 |
| Firebase `descendants` | 573 |
| HN HTML rows | 574 |

71 live comments missing, about 12%, including one 70-reply argument hanging
under a single `[flagged]` parent. Verified against Firebase: the dead node
`49372309` carries `dead: True`, its parent is present in the tree, and its
child `49372670` is absent. Latency is the same either way (0.79-0.83s against
0.76-0.81s) and one page at `hitsPerPage=1000` covered the whole thread.

### Why it is parked

The caller who asked for it wanted the **flagged comment itself**, not its
replies. That is not recoverable from either Algolia endpoint: the comment search
excludes the dead node too. Firebase returns `dead: True` but its text field came
back as the literal string `[flagged]`. The only remaining source is HN's own
HTML, where 73 `noshow` rows exist in the DOM on that thread and may carry the
real text — unverified, and it needs egress to `news.ycombinator.com`, which the
netpol currently denies.

With the flagged text out of reach, the caller judged recovering only the live
replies not worth the change. That is a product call, not a technical one: the
71 live comments are real content and are cheaply recoverable.

### What would have to be true to build it

Any one of:

1. A caller wants completeness for its own sake, or a `total_comments` field that
   is true. Today a consumer cannot tell a gutted thread from a complete one.
2. Someone confirms HN's `noshow` HTML rows carry the flagged text. Then the
   HTML fetch justifies itself three times over: flagged text, HN's own comment
   ranking (Algolia children are strictly id-ascending, so every cap currently
   selects chronologically rather than by rank), and per-comment downvotes from
   the CSS colour class.
3. A thread is found where the dropped subtree changed a summary's conclusion.

---

## Vertical search: query a site's own search API instead of the web

**Status:** built, shipped in 0.22.0, removed 2026-08-19.
**Code:** `git show v0.22.0 -- internal/search/router internal/search/hackernews
internal/search/reddit internal/search/bluesky`. Sitewide Reddit variant on
branch `feat/reddit-sitewide-search` (PR #34, closed).

### The idea

`web_search(site=X)` goes to a web engine, which ranks X's pages by whatever it
scraped. X's own search usually ranks its own content better and exposes signals
no scraper has: Hacker News points, Reddit score and comment count, Bluesky
likes. A `domain.Searcher` per site, a router dispatching on
`SearchOptions.Site`, and SearXNG as the fallback for anything a vertical
declines, empties on, or fails at.

Three were built: Hacker News (Algolia), Reddit (in-site search through the
browser port), Bluesky (`app.bsky.feed.searchPosts`).

### Why it was removed

Not because the code was bad — it was tested and worked. Because the problem it
solved turned out to be a **configuration** problem in one deployment's engine
pool, and fixing that made the verticals redundant there.

Measured 2026-08-18, four queries, `site=reddit.com`:

| query | Reddit in-site (sitewide) | Reddit in-site (`r/<sub>`) | web `site:reddit.com` |
|---|---|---|---|
| longhorn disk pressure | **0/7** — Windows Longhorn, disc golf | 5/5 on topic | **6/8** |
| best self hosted rss reader | 2/5 | 4/7 | **7/8** |
| wireguard mtu slow mobile | 3/4 | 3/6 | **6/6** |
| opus 5 coding (past week) | **5/5 fresh** | **6/6 fresh** | **0 results** |

A web search beat Reddit's own search on three of four. The failure mode of
sitewide in-site search is specific: an ambiguous term with a large
non-technical population on that site collapses it entirely. Its win is equally
specific: recency, where the web engine returned nothing at all.

Two honest caveats:

- **Hacker News and Bluesky were never benchmarked.** They were removed because
  neither was invoked once in production (`omnifeed_search_routes_total` had no
  series for either), not because they were measured and lost. The reasoning
  that Algolia beats a scraper for HN, and that web engines barely index
  `bsky.app`, is untested.
- The recency win was real, and disappeared only because that deployment gained
  a search engine with working date filters.

### What would have to be true to bring it back

Any one of:

1. A deployment whose engine pool has no member that both honours `site:` and
   indexes recent content. That is the situation the verticals were built for,
   and it is the common case for a keyless self-hosted pool.
2. Someone measures HN-Algolia or Bluesky `searchPosts` against a good web
   engine and it wins. Genuinely unknown.
3. A caller that needs the site's own ranking signals — points, score, comment
   count — as data rather than as ordering. No web engine exposes those, and no
   amount of engine tuning will produce them.

### What to do differently if it returns

Do not prefer a vertical unconditionally. The measurement says verticals win on
recency and lose elsewhere, so the router should serve a vertical when
`SearchOptions.TimeRange` is set and decline otherwise, per vertical. That was
designed and never built — it is the single change that would have made the
feature correct rather than merely available.
