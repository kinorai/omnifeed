# SO1 — S→F — "how to undo last git commit stackoverflow" — A-first

CASE: SO1
PICKED_URL: https://www.git-tower.com/learn/git/faq/undo-last-commit (B rank-1 fallback; NEITHER stack surfaced any stackoverflow.com URL)
A_SEARCH: ms=7029 chars=2 results=0 status=error  (verbatim `[]`; server searxng 0.197s "ok" — 2nd consecutive empty; suspiciously fast → upstream-engine throttling suspected)
B_SEARCH: ms=14336 chars=2671 results=10 status=ok (0 SO urls; git-tower/github-discussion/educative/dev.to×4/medium×3)
A_FETCH: ms=19466 chars=6563 status=ok  (server crawl4ai 9.670s)
B_FETCH: ms=15352 chars=2053 status=partial
SERVER_A: search +1 ok 0.197s; crawl4ai +1 ok 9.670s; attempts first +1 retry +0
STRIKES: none
QUALITY_A: Full page verbatim, all 7 git code blocks intact + nav/CTA boilerplate. (No SO answer structure — picked page is a vendor FAQ.)
QUALITY_B: All 6 code blocks kept but prose paraphrased, tip boxes/related sections dropped (2053 vs 6563 chars).

Timestamps: A_s 1785414321861→1785414328890; B_s 1785414332834→1785414347170; A_f 1785414376448→1785414395914; B_f 1785414404426→1785414419778.
Metrics after: attempts first=284 retry=6; crawl4ai ok=188 sum=1979.4461s; searxng ok=227 sum=187.4588s.
NOT DONE: no stackoverflow.com fetch datapoint (search-derived pick rule; neither stack found SO).
Findings: (1) A search empty-result streak begins (G4, SO1) — searxng zero-row at 0.2-0.4s; (2) searxng counter jumped 185→227 in ~20min incl. heavy foreign traffic — plausible upstream (ddg/brave) rate limiting; (3) B WebSearch ranks SEO/listicle content over canonical SO.
