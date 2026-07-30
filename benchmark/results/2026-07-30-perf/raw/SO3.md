# SO3 — S→F — "nginx reverse proxy 502 serverfault" — A-first

CASE: SO3
PICKED_URL: https://www.redswitches.com/blog/nginx-502-bad-gateway-error/ (B rank-1 fallback; neither stack surfaced serverfault/SO)
A_SEARCH: ms=7865 chars=2 results=0 status=error  (`[]`; server 0.513s ok — 3rd empty: G4, SO1, SO3; SO2 worked)
B_SEARCH: ms=14934 chars=2590 results=10 status=ok (no SF/SO; incl. wikipedia, habr qna ×3, oreilly ×2)
A_FETCH: ms=18287 chars=51105 status=partial  (server crawl4ai 10.466s; TOKEN-CAP OVERFLOW → spooled to file, 2nd occurrence)
B_FETCH: ms=18873 chars=1896 status=partial
SERVER_A: search +1 ok 0.513s; crawl4ai +1 ok 10.466s; attempts first +1 retry +0
STRIKES: none
QUALITY_A: 51KB with ZERO article content — greps for "nginx"/"502"/backticks = 0 hits; all RedSwitches dedicated-server marketing template (nav, pricing rows, CTAs). Fetch "succeeded" but useless.
QUALITY_B: 1.9KB condensed but topically correct (6 causes, 3 diagnostic commands inline); explicit elision noted.

Timestamps: A_s 1785414762039→1785414769904; B_s 1785414776131→1785414791065; A_f 1785414802232→1785414820519; B_f 1785414842319→1785414861192.
Metrics after: attempts first=285 retry=6; crawl4ai ok=189 sum=1989.9124s; searxng ok=230 sum=189.0276s.
Findings: (1) A fetch has no relevance/extraction guard — 50KB chrome-only payload billed at ~12.8k tokens, B beat A on usable content here; (2) 2nd MCP token-cap overflow; (3) A search empty streak on "site-keyword" queries continues; B WebSearch seems to exclude SO/SF domains entirely.
NOT DONE: no SF/SO-domain fetch datapoint (search-derived rule).
