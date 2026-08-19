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
