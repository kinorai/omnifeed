# SO4 — S→F — "python merge two dictionaries stackoverflow" — B-first

CASE: SO4
PICKED_URL: https://stackoverflow.com/questions/38987/how-do-i-merge-two-dictionaries-in-a-single-expression-in-python (A rank-1; API fallback not needed)
A_SEARCH: ms=8984 chars=2164 results=10 status=ok  (server 0.911s; 9/10 stackoverflow.com)
B_SEARCH: ms=14642 chars=2548 results=8 status=ok  (0 SO URLs despite "stackoverflow" in query; ~62% prose summary)
A_FETCH: ms=21589 chars=84432 status=ok  (server crawl4ai 10.469s; 3rd TOKEN-CAP OVERFLOW → spooled)
B_FETCH: ms=11459 chars=65 status=error  (verbatim `<error>Claude Code is unable to fetch from stackoverflow.com</error>`)
SERVER_A: search +1 ok 0.911s; crawl4ai +1 ok 10.469s; attempts first +1 retry +0
STRIKES: none
QUALITY_A: Full question page — 105 fenced code blocks + 40 indented groups, all top answers + comment threads, 0 block markers; leading survey-banner noise. 82.5KB ≈ 21.1k tokens.
QUALITY_B: Search: GeeksforGeeks/favtutor/treyhunner/medium/anthropic. Fetch: domain-refused, 0 content.

Timestamps: B_s 1785415025434→1785415040076; A_s 1785415048029→1785415057013; B_f 1785415061605→1785415073064; A_f 1785415076477→1785415098066.
Metrics after: attempts first=286 retry=6; crawl4ai ok=190 sum=2000.3818s; searxng ok=231 sum=189.9384s.
Findings: (1) Native WebFetch domain-blocklist now covers reddit.com AND stackoverflow.com; (2) A search recovered (empties are intermittent, not query-shape-specific — "stackoverflow" keyword worked here); (3) 84KB SO page → pruning candidate (survey banner, sidebar, related-questions chrome).
