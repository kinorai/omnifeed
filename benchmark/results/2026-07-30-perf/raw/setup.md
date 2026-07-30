# SETUP (sanity check, not scored)

SETUP: ok
TOOLS: all 4 callable
OVERHEAD_MS: 5248,6730,5792 (mean ~5923)
WARMUP_A_SEARCH: ms=8463 chars=~4800 status=ok (server 0.995s)
WARMUP_B_SEARCH: ms=17504 chars=~2450 status=ok
WARMUP_A_FETCH: ms=10684 chars=166 status=ok (server 2.991s, example.com)
WARMUP_B_FETCH: ms=11509 chars=~900 status=ok
METRICS_SCRAPE: ok
RATE_LIMITS: none

Notes:
- Bracket ms − server s gap ≈ 7.5s on A calls (agent loop ~5.9s + MCP round trip).
- attempts_total{first} counts Engine fetches only, not searches.
- B search warm-up returned links + synthesized prose overview (not raw list).
- WebFetch on example.com returned a summarized "## Overview" doc despite verbatim prompt (~900 chars vs A's 166 verbatim).
- Baseline after warm-up: attempts first=247 retry=6; crawl4ai ok=164 sum=1802.087s; searxng ok=186 sum=169.773s; reddit ok=58; hn ok=13.
