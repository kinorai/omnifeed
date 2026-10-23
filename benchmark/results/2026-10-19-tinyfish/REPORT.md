# TinyFish Search vs omnifeed `web_search`, small benchmark (2026-10-19)

12 queries with the same query string on both sides, run one after another. TinyFish was called with a 4 s gap between requests. omnifeed was called through MCP.
The queries are in `queries.txt`. TinyFish raw JSON is in `raw/tinyfish/` (the last line of each file holds the HTTP code and the time).

| # | Query | TinyFish | omnifeed | Notes |
|---|---|---|---|---|
| Q1 | gateway api vs ingress-nginx retirement | 5 | 5 | Both return official docs, vendor guides and an r/kubernetes thread |
| Q2 | site:reddit.com reverse proxy | 5 | 5 | 10/10 Reddit threads on both, about 7 of them the same |
| Q3 | site:news.ycombinator.com rust vs go | 5 | 5 | 10/10 HN items on both, about 6 of them the same |
| Q4 | undo last git commit | 4 | 5 | omnifeed puts the SO canonical answer at #1; TinyFish puts git-tower at #1 and SO at #2 |
| Q5 | go token bucket library github | 5 | 5 | Same libraries (webriots, uber-go, mennanov, juju) |
| Q6 | postgres 18 async io | 5 | 5 | TinyFish also returns the HN thread |
| Q7 | site:x.com karpathy | 5 | 5 | 10/10 x.com status/profile URLs on both |
| Q8 | crawl4ai execute_js endpoint | 5 | 5 | Docs plus the CVE-2026-56264 advisories on both |
| Q9 | 2025 nobel physics (SimpleQA-style) | 5 | 5 | Answer in the #1 snippet on both |
| Q10 | reddit v perplexity/serpapi ruling | 5 | 5 | omnifeed slightly fresher (Reuters 2026-10-12, Loeb) |
| Q11 | site:linkedin.com k8s platform eng paris | 4 | 4 | TinyFish returns job posts and listing pages; omnifeed returns jobs and profiles |
| Q12 | meilleur hébergeur vps france (FR) | 5 | 5 | French results on both, Clubic at #1 on both |

**Relevance: effectively a tie (58 vs 59 out of 60).** Top-10 overlap is high. That is expected, because both are largely Google underneath:
- omnifeed: most hits are tagged `engine: google cse`.
- TinyFish: the results carry Google SERP fingerprints. Relative dates ("12 months ago"), the snippet date prefix "Sep 25, 2026 —", Reddit `?tl=fr` auto-translate links, the `&xstg=` param on a YouTube URL, and only 8–10 results per page.

**Latency:**
- TinyFish, measured client-side from here: 12/12 HTTP 200. Median about 1.95 s, range 1.29–6.50 s. TLS setup is about 50 ms.
  - The 6.5 s outlier was the `site:x.com` query.
  - It did not reproduce the advertised 556 ms p50.
- omnifeed, server-side (`omnifeed_search_request_seconds`, last 20 min): p50 0.64 s, p90 1.12 s. This does not include MCP overhead, so it is not a strict like-for-like comparison.

**Limits:**
- TinyFish: 30 req/min, 12,000/day free.
- omnifeed: the pacing guard returned `quota_exhausted` after about 10 benchmark queries. A research subagent had spent about 25 calls just before, which counts against the same budget.

**TinyFish advantages:**
- A structured `date` field on most results.
- Clean snippets.
- `include_domains`/`exclude_domains`, `recency_minutes`, and `domain_type=news|research_paper`.

**omnifeed issue found:** snippets from the `privacywall` engine come back **doubled** (the text concatenated with itself, e.g. Q1, Q6, Q7). This wastes tokens on every such hit.

## Addendum: freshness, recency, Fetch, capacity (run at 2026-10-19 ~20:00 UTC)

Six stories posted to HN in the previous 0–3 hours (`fresh_queries.txt`). Each was run once, with no recency filter.

| # | Story | TinyFish | omnifeed |
|---|---|---|---|
| F1 | DHS bans DeepSeek | WSJ, Reuters, Federal Register, a Meritalk piece **55 min old**, an r/LocalLLM thread 3 h old | WSJ, Federal Register + PDF, BBC 2 h old, Engadget, Reuters 5 h old |
| F2 | Firefox 158 | Release notes, MDN, 9to5linux, PCWorld | Same set |
| F3 | RISC-V on Intel 3 | HotHardware article at #1 (Feb 2026 story, resurfaced on HN) | Same article at #1 |
| F4 | Ex-OpenAI employee, Saudi backing | Reuters 2 h old, CBS **37 min old**, BBC 4 h old | Reuters, CBS, The Hindu, BBC 4 h old, Al Jazeera 2 h old |
| F5 | Stratospheric airship US→Japan | Older coverage only; **missed the Ars Technica article** | **Ars Technica article at #1** |
| F6 | "Terminal Bench 4 is saturated" (blog post, about 18 min old) | Missed (TB4 leaderboards only) | Missed (TB4 leaderboards only) |

Both sides read a live Google index and both are fresh to within about an hour.

Recency filter (`recency_minutes=1440` vs `time_range=day`):
- Both work on F1.
- On F2, TinyFish returned off-topic pages (webdesign.co.uk, vimma.dev, a Manjaro forum). omnifeed returned on-topic Firefox 158 posts from the last 1–2 days.

**omnifeed dates:** the SearXNG `publishedDate` field is plumbed through as `published_date`. The `google cse` and `privacywall` engines never fill it, so the date only appears as a "N hours ago ..." prefix inside the snippet. TinyFish returns a separate `date` field.

**Snippet doubling:** this is not limited to privacywall. `google cse` hits are doubled too (e.g. F2: firefox.com, MDN).

**TinyFish Fetch** (`ttl:0`, 3 URLs in one call, 16.5 s total, free up to 1,000 URLs/day):
- Reddit thread: 5 KB of rendered-page markdown. Only the first screen of comments, flat, with no scores or nesting, and with ads mixed in. omnifeed returns the full nested tree as TOON.
- x.com post: 678 chars, just the post text behind a "Log in / Sign up" wall. No thread, replies, quote or community note; omnifeed's X engine returns all of those.
- Blog post published 18 min earlier: clean, 14 KB.

**Capacity, from VictoriaMetrics, last 30 days:**
- 875 searches in total: 831 ok, 41 `quota_exhausted`, 3 timeouts.
- Peak 12 req/min (1-minute window), peak 35 per 5 min.
- Latency p50 1.09 s, p95 3.18 s, p99 11.6 s.

Configured pacing: DELAY 1s, QUOTA 10 per 90 s window, CONCURRENCY 8. That gives about 6.7 req/min sustained, with bursts of 10. TinyFish allows 30 req/min and 12k/day, about 4.5× our sustained ceiling.
