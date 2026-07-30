# SO2 — S — "kubernetes pod stuck terminating" — B-first

CASE: SO2
A_SEARCH: ms=12804 chars=3718 results=10 status=ok  (server searxng 0.679s; all results engine="brave")
B_SEARCH: ms=14649 chars=3018 results=9 status=ok
SERVER_A: search +1 ok 0.679s; no fetch; attempts unchanged
STRIKES: none
QUALITY_A: SO at rank 2 (Q35453792) + 3 r/kubernetes threads; 4 items have published_date (2026-02-09/02-07/01-13, 2022-09-13); snippets 7/10.
QUALITY_B: No stackoverflow/serverfault anywhere; no reddit; no dates; 2017 GitHub issue in top5; 2 "DEV Community" boilerplate titles; + LLM prose summary + REMINDER line.

A top5: 1) reddit 1q4fnl7 2) SO 35453792 3) broadcom techdocs 4) reddit 1q914gz 5) educative. (6-10: ms-learn, reddit, ibm, kodekloud, oneuptime)
B top5: 1) medium haroldfinch 2) broadcom techdocs 3) kodekloud 4) gh issue 51835 (2017) 5) oneuptime. Overlap 2 URLs.
Timestamps: B 1785414594245→1785414608894; A 1785414615582→1785414628386.
Metrics after: searxng ok=229 sum=188.5145s; attempts first=284 retry=6.
Findings: empty-[] issue intermittent (recovered here); A search overhead varied to ~12.1s (loop noise) — server-side remains sub-second; earlier A searches showed ddg+brave, this one brave-only → suggests ddg upstream failing intermittently inside searxng.
