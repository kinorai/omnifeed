# HN2 — S — "rust vs go performance hacker news" — B-first

CASE: HN2
A_SEARCH: ms=12722 chars=2 results=0 status=error  (`[]`; server 0.395s ok — 4th empty: G4, SO1, SO3, HN2)
B_SEARCH: ms=11989 chars=2799 results=10 status=ok
SERVER_A: search +1 ok 0.395s; nothing else
STRIKES: none
QUALITY_A: Empty. 0 HN items.
QUALITY_B: 4/5 top-5 are news.ycombinator.com threads (ids 43307229≈2025, 9724483≈2015, 37107052≈2023, 13430108≈2017) — good recall, old recency; 7/10 HN overall.

Timestamps: B 1785415451960→1785415463949; A 1785415470225→1785415482947.
Metrics after: searxng ok=232 sum=190.3337s; attempts unchanged.
Pattern note: empties cluster after ~G4 (12:23): G4 ✗, SO1 ✗, SO2 ✓(brave-only), SO3 ✗, SO4 ✓, HN2 ✗ → searxng upstream engines (ddg/brave) intermittently rate-limited under sustained load; omnifeed surfaces [] with status ok, ~0.2-0.5s.
Empty-rate so far: 4/9 A searches (0/3 in R-block, 4/6 since).
