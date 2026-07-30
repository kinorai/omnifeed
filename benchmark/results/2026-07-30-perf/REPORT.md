# Performance benchmark: omnifeed vs Claude Code native web tools — 2026-07-30

- **A — omnifeed**: `mcp__omnifeed__web_search` + `mcp__omnifeed__fetch_url` (local stack: omnifeed + SearXNG + crawl4ai 0.9.x on Apple container; MCP over Streamable HTTP, localhost:8081)
- **B — native**: `WebSearch` + `WebFetch`, with WebFetch always given the exact
  verbatim-markdown prompt from [BENCHMARK_QUALITY.md](../../BENCHMARK_QUALITY.md)
- Method: [BENCHMARK_PERF.md](../../BENCHMARK_PERF.md). 35 cases, strictly sequential
  (one fresh executor per case), identical inputs, one shot per tool, A/B order
  alternated. Zero client-side retries ("strikes") were needed anywhere.
- Raw per-case reports (timestamps, verbatim errors, Prometheus snapshots/deltas):
  [raw/](raw/) — every number below is auditable there.

## Method caveats

- **Bracket overhead.** Every wall-clock number carries the agent-loop/MCP round-trip:
  a no-op bracket measured 5.2–6.7 s at setup; across the run, A's bracket minus
  server-side time was median 8.3 s (min 6.6, max 16.5). Identical method for both
  stacks, so A-vs-B comparisons are fair, but absolute numbers overstate both tools.
  Omnifeed's server-side latency (Prometheus delta per case) is the trustworthy
  absolute number; native has no equivalent.
- **Shared instance.** Foreign traffic hit the omnifeed instance during the run;
  per-case deltas were validated against expected count increments and contaminated
  windows are flagged in the raw reports (G2, G3).
- **Sizes** are chars as received (tokens ≈ chars/4); a few are executor estimates
  marked `~` in the raw reports.

## Overall summary

| Metric | A: omnifeed | B: native |
|---|---|---|
| Search median / p95 (bracket) | **8.4 s / 12.8 s** | 14.6 s / 15.5 s |
| Search server-side median / p95 | **0.91 s / 1.47 s** | n/a |
| Search success | 10/17 ok, **7/17 empty `[]`** | **17/17 ok** |
| Fetch median / p95 (bracket) | 16.9 s / 25.5 s | **14.6 s / 22.2 s** |
| Fetch server-side median / p95 | 8.46 s / 10.71 s | n/a |
| Fetch success | **18 ok · 9 partial · 2 error** | 1 ok · 18 partial · **10 error/block** |
| Total output | 807 KB ≈ 202k tokens | 89 KB ≈ 22k tokens |
| Server-side engine medians | HN 0.85 s · Reddit 2.3 s · crawl4ai 9.2 s | n/a |
| Server-side retries | 1 case (E1 PDF, +2) | n/a |

MCP token-cap overflows (payload spooled to disk by the client): R4 71 KB, SO3 51 KB,
SO4 84 KB, HN3 98 KB, P4 175 KB.

## Per-source tables (bracket ms · size · status; srv = omnifeed server-side)

### Reddit — omnifeed 4–0 (structural: native cannot touch reddit.com)

| Case | A | B | Note |
|---|---|---|---|
| R1 fetch /r/devops | 8.9 s · 37.6 KB · ok (srv 2.3 s) | 7.9 s · 65 ch · error | A: 25 real posts (TOON). B: "Claude Code is unable to fetch from www.reddit.com" |
| R2 S→F ingress vs gateway | S 10.1 s/10 res · F 13.0 s ok (srv 1.6 s) | S 14.7 s/0 reddit URLs · F error | A: 9/9 comments, 3-level nesting verified, 1.7 KB |
| R3 search monorepo CI | 8.8 s · 4 reddit-domain threads in top 5 | 14.9 s · 0 reddit URLs | |
| R4 S→F selfhosted proxy | S 8.3 s · F 11.7 s ok (srv 3.0 s), 205/228 comments, 71 KB | S 13.0 s/0 reddit · F error | A payload overflowed MCP token cap |

### GitHub — content to A, but slow crawls, lazy-load truncation, heavy chrome

| Case | A | B | Note |
|---|---|---|---|
| G1 repo page | 17.5 s · 20.6 KB · ok (srv 9.7 s) | 15.9 s · 1.6 KB · partial | A: full README but ~80% chrome. B dropped all code/links |
| G2 issue #43916 (144 comments) | 23.1 s · 26.5 KB · partial (srv 10.3 s) | 14.6 s · 1.5 KB · partial | A: 12/144 comments — "209 remaining items / Load more" |
| G3 merged PR #140882 | 27.5 s · 21.7 KB · partial (srv 14.6 s) | 18.4 s · 2.2 KB · partial | A: full review convo, no diff, usernames missing |
| G4 search rate-limiter lib | 7.6 s · `[]` empty | 14.9 s · 10 results · ok | First omnifeed empty search |

### Stack Overflow — A decisive when search cooperates; native refuses SO fetches

| Case | A | B | Note |
|---|---|---|---|
| SO1 S→F undo git commit | S `[]` · F (git-tower) 19.5 s ok, all 7 code blocks | S 14.3 s/0 SO URLs · F partial | Neither search surfaced stackoverflow.com |
| SO2 search pod terminating | 12.8 s · ok, SO at rank 2 + dates | 14.6 s · 0 SO/SF | |
| SO3 S→F nginx 502 | S `[]` · F 18.3 s · 51 KB · **100% chrome, 0 article** | S 14.9 s/0 SF · F 1.9 KB on-topic | Only case where B's fetch beat A's on usefulness |
| SO4 S→F merge dicts | S 9.0 s · 9/10 SO results · F 21.6 s · 84 KB · 105 code blocks | S 14.6 s/0 SO · F error (domain refused) | |

### Hacker News — dedicated engine is the run's speed champion; A search flaked

| Case | A | B | Note |
|---|---|---|---|
| HN1 front page | 7.4 s · ok (srv **0.44 s**) | 12.9 s · partial | A: 30/30 stories structured; B: 6/30 |
| HN2 search rust vs go | `[]` | 12.0 s · 4/5 HN threads (old) | |
| HN3 item 49103285 (269 comments) | 8.9 s · 98 KB · ok (srv **1.26 s**) | 15.5 s · partial | A: 268/269 comments, nesting depth 13; B: 0 comments |
| HN4 search Show HN 2026 | `[]` | 13.8 s · weak relevance | |

### X / Twitter — fetch is omnifeed-exclusive; search reaches X on neither stack

| Case | A | B | Note |
|---|---|---|---|
| X1 tweet (karpathy status) | 16.0 s · 2.9 KB · ok (srv 7.7 s) | 9.7 s · **HTTP 402** | A: full long-form tweet + views + 3 replies |
| X2 profile x.com/karpathy | 16.4 s · partial (srv 5.9 s) | 7.2 s · **HTTP 402** | A: bio, 3.6M followers, 5 tweets truncated at "Show more" |
| X3 S→F outage thread | S `[]` · F (newsweek fallback) ok | S 0 x.com URLs · F partial | No x.com URL discoverable via either search |

### General web — A fidelity wins undermined by code corruption; one silent-empty bug

| Case | A | B | Note |
|---|---|---|---|
| W1 k8s Service doc | 17.2 s · 45 KB · ok (srv 8.9 s) | 15.5 s · 3.6 KB · partial | A: all 19 blocks, but **every YAML collapsed to one line** |
| W2 MDN Array.reduce | 18.7 s · 16.9 KB · partial (srv 8.5 s) | 13.5 s · 1.8 KB · partial | A: **all 12+ JS examples dropped**, table cells empty |
| W3 search pg pooling 2026 | `[]` | 15.1 s · 4/5 relevant, 2026 items | |
| W4 S→F ratelimit blog | S ok · F **0 chars while server logged ok 9.75 s** | F paywall preamble | Silent empty-success bug (paywalled ByteByteGo) |

### Dev forums — A wins GitLab/dev.to outright; Discourse lazy-load bites A

| Case | A | B | Note |
|---|---|---|---|
| D1 lobste.rs front page | 14.9 s · partial (srv 4.3 s) | 19.5 s · partial | A: 25/25 stories + URLs, no scores; B: 10/25 with scores, no URLs |
| D2 dev.to article | F 16.9 s · 10.4 KB · ok (srv 9.8 s) | 14.5 s · 0 code blocks | A: 4/4 blocks but `FROMgolang:1.21ASbuilder` corruption |
| D3 discuss.python.org PEP 703 | F 17.6 s · partial (srv 9.2 s) | 17.2 s · partial | A: only posts 5–6 of 6 (lazy-load), ~40% nav chrome |
| D4 GitLab runner issue | F 15.5 s · 19.5 KB · ok (srv 5.8 s) | 17.7 s · 1.3 KB · partial | A: full thread + code + CI logs — GitLab renders in one shot |

### Package registries — omnifeed sweep, including the SPA test

| Case | A | B | Note |
|---|---|---|---|
| P1 npm express | 21.0 s · 8.9 KB · partial (srv 10.8 s) | 8.3 s · **403 block** | npm bot-walls native |
| P2 pypi requests | 16.9 s · 22.8 KB · ok (srv 6.5 s) | 14.0 s · partial | A: ~60% bloat (release history); B hallucinated a maintainer |
| P3 crates.io serde (SPA) | 16.5 s · 3.4 KB · ok (srv 3.5 s) — **fully rendered** | 11.4 s · **empty shell** | The SPA differentiator, as predicted |
| P4 pkg.go.dev gin | 17.7 s · **175 KB / ~44k tokens** · ok (srv 10.1 s) | 24.9 s · 7 KB · partial | Complete but unusable in-context |

### Edge cases — PDF kills both; both pass Cloudflare; Medium paywall equal

| Case | A | B | Note |
|---|---|---|---|
| E1 rfc9110.pdf | **error** `MCP error -32603` (srv err 3.9 s, retry ×2) | error (2.7 MB binary saved, no text) | Only server-side retries of the run |
| E2 nowsecure.nl (CF challenge) | 10.8 s · ok (srv 3.3 s) | 10.0 s · ok | No challenge markers on either |
| E3 Medium member-only | teaser + literal paywall markers · partial | teaser paraphrased · partial | Nobody bypasses the wall |
| E4 Apple SwiftUI docs | 16.8 s · 12.3 KB · ok (srv 9.2 s) | 10.2 s · **title-only shell** | |

## Headline findings

1. **Reddit: omnifeed 4–0, structurally.** Native WebFetch refuses reddit.com at the
   client, and native WebSearch returned zero reddit URLs in all four cases even with
   "reddit" in the query. The same domain refusal covers stackoverflow.com, x.com
   (402) and npmjs.com (403): for those sources native isn't slower — it's absent.
2. **X: fetch-only exclusive.** Omnifeed rendered tweets and profiles (5.9–7.7 s
   server-side); native 402'd. Neither stack can *discover* x.com URLs via search —
   the known omnifeed asymmetry is really a both-stacks asymmetry.
3. **Omnifeed search: ~6 s faster when it answers, 41% empty under sustained load.**
   Median 8.4 s bracket / 0.91 s server vs native 14.6 s; richer results (snippets,
   dates, the domains asked for). But 7/17 searches returned `[]` in 0.2–0.6 s
   server-side with status "ok" — the SearXNG upstream-engine-suspension signature
   (ddg+brave early → brave-only → ddg-only across the run). Healthy at the start
   and end of the run; empties clustered mid-run under our own load.
4. **Fetch trade: fidelity vs weight.** A: 18/29 full-content fetches, 765 KB total.
   B: 1/29 "ok" — its summarizer ignored the verbatim prompt in every case, dropping
   code/comments/URLs (and once hallucinating a maintainer). But A shipped ~191k
   tokens over 29 fetches and 5 payloads blew the MCP client token cap.
5. **Dedicated engines are 10–20× faster than the browser.** Server-side medians:
   HN 0.85 s, Reddit 2.3 s, crawl4ai 9.2 s (max 14.6 s). The clearest perf lever.
6. Wall-clock A-vs-B bracket differences are mostly noise around the ~8.3 s harness
   overhead; server-side numbers are the signal.

## Ranked optimization candidates

1. **SearXNG empty-result reliability** — *lossless (reliability)*. 7/17 searches
   `[]` at 0.2–0.6 s "ok". Parse `unresponsive_engines` from the SearXNG JSON
   (present on the 200 path; `[[engine, "Suspended: too many requests"], …]`),
   surface a distinct `observability.Reason`, widen the engine pool / tune
   `suspended_times` in settings.yml, consider a fallback Searcher. Validate:
   replay the 7 failed queries healthy; 50-search soak watching searxng logs.
2. **Payload size control on `fetch_url`** — *lossless if opt-out with markers*.
   5/29 payloads (51–175 KB) overflowed the client cap. Add `max_chars`/`start_char`
   params + server default cap + truncation marker; declare
   `anthropic/maxResultSizeChars` in tools/list `_meta`. Never byte-truncate
   TOON/JSON — translate the budget into structural caps (Reddit/HN knobs exist).
3. **Code-block corruption via crawl4ai markdown** — *lossless (quality bug)*.
   5 instances: k8s.io YAML one-lined, MDN examples dropped, dev.to spaces/`&&`
   stripped, crates.io struct fields dropped, pypi pip command mangled. Root cause:
   flattening of `<span>`-based syntax highlighting. Fix via crawl4ai markdown
   options or post-processing `<pre>` text. Validate with golden-file fixtures.
4. **Dedicated GitHub engine (then Discourse)** — *tradeoff: new code, big wins*.
   Measured: REST reconstructs a 317-comment issue in ~2.3 s sequential (~0.6 s
   parallel) complete, vs 10.3 s browser render capturing 12/144 comments; PR with
   real diff ≈ 2.0 s vs 14.6 s without diff. Unauthenticated 60 req/h is tight;
   optional `OMNIFEED_GITHUB_TOKEN` → 5000/h. Discourse: `/t/<id>.json?print=true`
   returned an 84-post topic in 1.3 s vs the browser's 2-of-6-posts failure.
5. **Chrome/noise pruning for the crawl4ai fallback** — *tradeoff (over-pruning
   risk)*. G1 ~80% chrome, SO3 100% marketing template, P2 ~60% release-history,
   D3 ~40% nav, duplicate body emission on X3/D4. crawl4ai content-filter options;
   SO3 is the regression test (today's output has 0% of the article).
6. **Silent empty-success fetch** — *lossless (correctness)*. W4: 0 chars delivered,
   server logged ok after 9.75 s. Guard empty extraction → explicit error with its
   own Reason.
7. **Error transparency** — *lossless*. E1 surfaced only `MCP error -32603:
   fetch_url failed`; the reason (`upstream_error`) exists in metrics but not in the
   error text. Propagate it. (PDF support itself: deliberately out of scope.)
8. ~~Reddit expansion caps~~ — already fully configurable and documented
   (`OMNIFEED_REDDIT_*` env vars + per-request MCP params; README §Reddit knobs).
   R4's 205/228 was the default `MAX_ROUNDS=3`; `?expand=full` allows up to 40.

## Native-stack observations (for the record)

- WebFetch never honored the verbatim prompt (29/29 summarized or refused); it
  hard-refuses reddit.com and stackoverflow.com, 402s on x.com, is 403-walled by
  npm, and returns empty shells on crates.io and developer.apple.com.
- WebSearch: 17/17 responses, ~14.6 s median, but zero reddit/SO/SF URLs across all
  relevant cases, no dates, frequent boilerplate titles ("DEV Community", "Jump to
  Content"), and it appends an LLM prose digest + a "REMINDER" line to every result.

## Appendix

- [raw/setup.md](raw/setup.md) — tool sanity check, warm-ups, bracket-overhead calibration.
- [raw/R1.md](raw/R1.md) … [raw/E4.md](raw/E4.md) — one file per case: parsable
  summary block, timestamps, sizes, verbatim errors, Prometheus deltas, quality notes.
- Baseline metrics chain start (pre-benchmark): attempts first=246 retry=6;
  crawl4ai ok=163 (1799.10 s), err=10; hackernews ok=13; reddit ok=58; searxng ok=185.
  End of run: first=306 retry=8; crawl4ai ok=207 (2130.96 s), err=11; hackernews
  ok=19; reddit ok=67 (foreign traffic included; per-case attribution in raw files).
