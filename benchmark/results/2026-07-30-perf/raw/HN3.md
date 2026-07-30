# HN3 — F — news.ycombinator.com/item?id=49103285 (269 comments) — A-first (S legs skipped by design)

CASE: HN3
PICKED_URL: https://news.ycombinator.com/item?id=49103285 ("AI's top startups are barely publishing their research", via Algolia prep)
A_FETCH: ms=8910 chars=97916 status=ok  (server hackernews engine 1.263s; 4th TOKEN-CAP OVERFLOW → spooled)
B_FETCH: ms=15538 chars=1763 status=partial
SERVER_A: hackernews +1 ok 1.263s; attempts first +1 retry +0
STRIKES: none
QUALITY_A: 268/269 comments TOON {id,parent_id,author,body,created}; 0 dangling parents; 42 top-level threads; depth histogram to 13. 95.6KB ≈ 24.5k tokens.
QUALITY_B: Thematic summary (21 bullets, 1 quote); zero comments/authors/nesting.

Timestamps: A 1785415603880→1785415612790; B 1785415640400→1785415655938.
Metrics after: attempts first=288 retry=6; hackernews ok=19 sum=22.8217s.
Perf finding: HN engine excellent server-side (1.3s for 269 comments) but payload 24.5k tokens — needs size control (depth/score-threshold pruning or pagination) to stay under client caps.
