# HN1 — F — https://news.ycombinator.com/news — A-first

CASE: HN1
A_FETCH: ms=7397 chars=4364 status=ok  (server hackernews engine 0.441s — Algolia path, crawl4ai untouched)
B_FETCH: ms=12855 chars=1303 status=partial
SERVER_A: hackernews +1 ok 0.441s; attempts first +1 retry +0
STRIKES: none
QUALITY_A: All 30 stories TOON {id,title,url,author,points,num_comments,created}; complete, no block.
QUALITY_B: Summary despite verbatim prompt — 6/30 stories, points only, no URLs/authors/comment counts; rest collapsed to "Notable Trending Topics" bullets.

Timestamps: A 1785415310315→1785415317712; B 1785415330132→1785415342987.
Metrics after: attempts first=287 retry=6; hackernews ok=18 sum=21.5590s.
Perf finding: dedicated-API engine = 0.44s server vs crawl4ai's 9.7-14.6s browser renders — validates the "bypass browser for API-friendly sources" design; GitHub/SO engine candidates by analogy.
