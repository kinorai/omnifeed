# Benchmark: omnifeed vs native web tools — PERFORMANCE edition

Companion to [BENCHMARK_QUALITY.md](BENCHMARK_QUALITY.md) (same contestants, same
test matrix, same fairness rules). This edition measures **performance**, not
scored quality: wall-clock latency per call, output size, success/error/block
rate, and retries. Quality is recorded only as opportunistic 1-line annotations
per case (block page, empty SPA shell, truncation, missing comments/code) — not
scored dimensions.

Main goal: find where omnifeed can be optimized for performance with lossless
quality — or name and validate the tradeoffs — versus the native stack.

## Contestants

- **A — omnifeed**: `mcp__omnifeed__web_search` + `mcp__omnifeed__fetch_url`
- **B — native**: `WebSearch` + `WebFetch`

## Rules carried over from the quality edition

- Setup/sanity check first: all 4 tools callable, one warm-up call each; note
  any rate limiting.
- Identical input string/URL to both stacks per case.
- `WebFetch` always gets the exact verbatim-markdown prompt (see quality doc §Method).
- **One shot per tool.** Any retry is logged as a strike.
- A block / error / empty / "enable JS" page is a **result**, not a skip.
- Test matrix: same ~35 cases as BENCHMARK_QUALITY.md (Reddit, GitHub,
  Stack Overflow, Hacker News, X, general web, dev forums, package registries,
  edge cases).

## Performance method

**Sequential, one runner per case.** Each case runs in its own executor, one at
a time — never parallelize case execution (keeps latency comparable and avoids
retry-flattering). Within a case, both stacks are called back-to-back on the
identical input.

**Order alternation.** Alternate A-first / B-first down the case list to cancel
ordering bias (origin CDN warm-up, DNS).

**Timing bracket.** Immediately before and after EACH benchmark call, run
`python3 -c 'import time; print(int(time.time()*1000))'` in its own step —
nothing else between t1 and t2. Latency = t2 − t1. The bracket includes
agent-loop/MCP overhead; that is expected and identical for both stacks.
Measure the bracket overhead itself during setup (3× timestamp → no-op →
timestamp) and report it as a calibration line.

**Server-side truth (omnifeed only).** Scrape Prometheus before the first and
after the last omnifeed call of each case (default `:9090`, see
`OMNIFEED_METRICS_ADDR`):

    curl -s http://localhost:9090/metrics | grep -E '^omnifeed_(request_seconds_(sum|count)|search_request_seconds_(sum|count)|request_attempts_total|requests_total|search_requests_total)'

Per-series deltas give server-side latency, the serving engine (reddit /
hackernews / crawl4ai), and server-side retries (`attempt="retry"`). Report it
as a separate column — never substitute it for the bracket. Validate each
delta against the expected count increment: the instance may be shared, and
foreign traffic contaminates windows (flag those).

**Sizes.** Per call: chars (count from the result received), KB = chars/1024,
tokens ≈ chars/4.

**Status classification.** `ok` = substantive expected content · `partial` =
truncated/stub/missing sections (say what) · `block` = CAPTCHA/login/challenge/
"enable JS" · `error` = tool error, timeout, or empty response (quote verbatim).
A harness "result exceeds maximum allowed tokens" spool is **ok** (client-side
guard, not a fetch failure) — record the overflow as a finding.

**Per-case parsable record.**

    CASE: <id>
    ORDER: <A-first|B-first>
    PICKED_URL: <url or ->
    A_SEARCH: ms=<int> chars=<int> results=<int> status=<...>
    B_SEARCH: ms=<int> chars=<int> results=<int> status=<...>
    A_FETCH: ms=<int> chars=<int> status=<...>
    B_FETCH: ms=<int> chars=<int> status=<...>
    SERVER_A: <per-series deltas: engine, count, seconds; attempts first/retry>
    STRIKES: <none | verbatim>
    QUALITY_A: <1 line>
    QUALITY_B: <1 line>

## Output

1. Per-source performance tables: case · A latency/size/status · B
   latency/size/status · 1-line quality note.
2. Overall summary: median + p95 latency per stack per call type (bracket AND
   server-side for A), total bytes/tokens, success counts, strikes.
3. Headline findings — Reddit and X matchups explicitly; anywhere one stack
   wins decisively.
4. Ranked list of omnifeed OPTIMIZATION CANDIDATES with evidence, each marked
   lossless vs tradeoff, and how to validate it.
5. Appendix: raw per-case runner reports (timestamps, verbatim errors, both
   metrics snapshots) so every number is auditable.

Results convention: `benchmark/results/<date>-perf/REPORT.md` + `raw/` with the
per-case reports.

Known runs: [results/2026-07-30-perf/](results/2026-07-30-perf/REPORT.md).
