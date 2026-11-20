# Benchmarks

Each benchmark answers one question. Pick the suite by what changed, run it the
way its doc says, and file the result under `results/<date>-<topic>/REPORT.md`.

**Before you start, read the reports of the same suite.** Most "new" findings
are already in them: npm and Stack Overflow walls, the challenge wait, the
24-character Wikipedia page. A run that ignores them repeats their mistakes.

## Which suite to run

| Suite | Question | Run it when | How |
|---|---|---|---|
| A. Fetch backend | Is fetcher X faster, lighter or better than what ships? | A new backend or engine is proposed; a crawl4ai version bump | `fetch/run.py` on `corpus/fetch.jsonl` |
| B. Settings | Did a change to crawl settings keep the content? | A change to the payload omnifeed sends crawl4ai, or to an engine | `fetch/run.py` before and after, then diff `raw.jsonl`; the method is in `results/2026-08-09-latency-pass` |
| C. Resources | Peak and idle memory, Chrome count, and does memory come back when idle? | A change to crawl4ai config, a backend, or concurrency | `fetch/run.py --container ... --concurrency 8 --settle 30`; for one request kind at a time, the pool probe in `results/2026-11-18-crawl4ai-ram` |
| D. Bot walls | Which walls block us (Cloudflare, Akamai, DataDome)? | A fingerprint or user-agent change; a backend change | The `walled` rows of suite A, and the 272 sites in `builtin-tools/` |
| E. Search | Relevance, freshness, duplicate snippets | A searcher change; a SearXNG bump | `BENCHMARK_QUALITY.md` (search cases), `results/2026-10-19-tinyfish` |
| F. End to end | omnifeed against Claude Code's built-in WebSearch and WebFetch | Minor releases | `builtin-tools/` (one directory per version) |
| G. Rate limits | Where each engine and search provider starts throttling us | An engine or pacing change | `../docs/rate-limits.md` and `builtin-tools/limits/` |
| H. Production check | Does the bench agree with production? | The end of every suite | The queries below |

A suite that is not on this list does not exist yet. Before you add one, check
that none of these already answers the question.

## What every fetch run records

`fetch/run.py` records these for each URL, per candidate:

- **Time**, measured by the client around one call. Report p50 and p95, never
  one run's mean.
- **Grade**: `ok` (every must-contain phrase is there), `partial`, `miss`,
  `empty`, `blocked` or `error`. **`miss` and `empty` are silent failures**: the
  call succeeded and the content is not there. Every benchmark so far has found
  one (v1's soft-blocks, the 24-character Wikipedia page, an empty Stack Overflow
  page from Firecrawl Local). Timing and size can't catch them; the phrases can.
- **Tokens** (characters / 4). A backend that keeps page chrome costs tokens on
  every call.
- **Memory** of each `--container`: the working set (cgroup `memory.current`
  minus `inactive_file`, what the kubelet reports, not `docker stats`, which
  counts page cache) and the number of Chrome browsers. It records idle before,
  peak during, and the value `--settle` seconds after.

`raw.jsonl` holds one line per call; `summary.md` the totals and every URL
that was not `ok`.

## The corpus

`corpus/fetch.jsonl` holds one URL per line: `url`, `cat` (docs, reference,
pdf, listing, app, spa, js-only, forum, walled, news, registry) and `must`,
the phrases a good result contains. Half of it is docs, because production
fetches are mostly docs (the top hosts over 30 days are raw GitHub files,
netbird, CloudNativePG, Claude Code, OpenRouter and Home Assistant).

- The phrases were picked from omnifeed's own render and checked by hand.
  They test that the body came back, not the site's navigation.
- `weak: true` marks the news and listing pages whose content changes every
  hour. Their phrase only proves that the site's own page came back, not a
  block page.
- A page changes; when a phrase stops matching on every candidate, check the
  page and update the phrase in the same PR as the report.

`corpus/top-sites.jsonl` holds 54 URLs that production actually fetched,
up to two from each of the 28 most-fetched hosts on the generic crawl4ai path
(VictoriaLogs, June to November 2026; dead URLs are left out, and so are
Reddit, Hacker News, GitHub, X and the Discourse forums, which have their own
engines). It has no
phrases: run it with `--reference`, which scores each candidate by the share
of the reference candidate's sentences it contains (`cov`). Coverage tells
you a candidate lost text; it can't tell you the reference was right, so
read the pages behind every low score.

## Running suite A or C

Everything runs locally. Tests are hermetic, but benchmarks are not: they
fetch the live sites. On kube-03 check the agents' memory first (the `Memory:`
line of `systemctl --user status app-t3code-agents.slice`), run one backend
container at a time, and remove containers and images when done.

1. Start crawl4ai with production's `config.yml` (the block under `config.yml:`
   in the infrastructure repo's `clusters/home-prod/apps/omnifeed/values.yaml`)
   and the same image digest, with `--memory 5g --shm-size 1g` like the pod.
   Name the container `c4a` (the `--container` below) and publish its port on
   `127.0.0.1:11298`.
2. Build omnifeed and start it with a clean environment, so no key or upstream
   from your shell leaks in:

   ```bash
   go build -o /tmp/omnifeed ./cmd/omnifeed
   env -i PATH="$PATH" HOME="$HOME" \
     OMNIFEED_CRAWL4AI_URL=http://127.0.0.1:11298/crawl \
     OMNIFEED_CRAWL4AI_TOKEN=benchtoken OMNIFEED_API_KEY=benchkey \
     OMNIFEED_LISTEN_ADDR=127.0.0.1:18080 OMNIFEED_MCP_LISTEN_ADDR=127.0.0.1:18081 \
     OMNIFEED_METRICS_ADDR=127.0.0.1:19090 OMNIFEED_CACHE_ENABLED=false \
     /tmp/omnifeed
   ```

3. Run:

   ```bash
   python3 -I benchmark/fetch/run.py --out /tmp/bench-run --token benchkey \
     --candidate omnifeed=omnifeed:http://127.0.0.1:18080 \
     --candidate http=http:- \
     --container c4a --concurrency 8 --settle 30
   ```

Candidate kinds: `omnifeed` (the shipped path, through the loader route),
`crawl4ai` (omnifeed's payload sent straight to crawl4ai), `firecrawl`
(`/v2/scrape`), `http` (one plain GET with Firecrawl Local's
`needs_browser()` check; it tells you which pages need no browser, nothing
about markdown quality), and `gomd` (one plain GET converted to markdown in
Go by `fetch/gomd`, the converter a plain-HTTP step in omnifeed would use;
start it with `go -C benchmark/fetch/gomd run . -addr 127.0.0.1:18090` and
pass `gomd:http://127.0.0.1:18090/?mode=raw`, or `?mode=read` for
Readability first).

## Suite H: check against production

A bench number that production contradicts is wrong. Compare with the last
7 days in VictoriaMetrics:

```promql
histogram_quantile(0.5, sum(rate(omnifeed_request_seconds_bucket{status="ok"}[7d])) by (le, engine))
quantiles_over_time("q", 0.5, 0.9, 0.99, 1, max(container_memory_working_set_bytes{namespace="omnifeed",container="main",pod=~"omnifeed-crawl4ai.*"})[30d:5m])
sum(increase(omnifeed_requests_total[7d])) by (engine)
```

The third one tells you how much traffic a change can touch: in November 2026
crawl4ai served about 200 fetches a week.
