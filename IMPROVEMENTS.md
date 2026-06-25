# omnifeed — improvement plan (from the 2026-06-25 benchmark)

Derived from [`BENCHMARK_RESULTS.md`](./BENCHMARK_RESULTS.md) plus the server-side telemetry captured during that run
(omnifeed/crawl4ai logs + Prometheus `/metrics`). Every item is tied to concrete evidence and a code location.

omnifeed already *wins the matchup that matters* — it reaches Reddit, Stack Exchange, X, npm, and SPAs that the native
stack is blocked from. These improvements target the places where it lost or burned time, almost all of which are small,
localized changes.

## Priority

| # | Improvement | Impact | Effort | Fixes |
|---|---|---|---|---|
| **1** | Stop dropping hyperlink anchor text (`ignore_links: true`) | **High** | XS (1 line + test) | HN1, G1, link-index pages |
| **2** | Don't retry the non-transient anti-bot 500 (generic path) | **High** | S | ~47% of wasted crawl time (X2, PDFs) |
| **3** | Detect PDFs/non-HTML; extract or fail fast | **High** | M | E1, anti-bot misfire |
| **4** | Add a structured Hacker News engine | Medium | M | HN1 (titles), HN3 (nesting) |
| **5** | Handle Reddit subreddit *listing* URLs | Medium | S–M | R1 |
| **6** | Tame boilerplate/duplication in generic markdown | Medium | S | W4 bloat, G "Uh oh" |
| **7** | SearXNG engine hygiene *(deployment repo)* | Medium | S | search latency/noise |
| **8** | Observability: count attempts / retry-seconds | Low | XS | makes #2 measurable |

---

## 1. Stop dropping hyperlink anchor text — **biggest fidelity win for the cost**

**Problem.** The generic engine tells crawl4ai's markdown generator to drop links entirely. On link-dense pages this is
fatal: the HN front page came back as `126 points by exploraz | 16 comments` with **no title** (titles are links), and
the kubernetes/kubernetes README lost every "See our documentation on …" anchor.

**Evidence.** HN1 fidelity 2/5 (B scored 5/5 — it kept all titles); G1 dropped anchors; the pattern recurred on MDN/W1.

**Where.** `internal/engine/crawl4ai/engine.go:112`
```go
"options": { "ignore_links": true }   // ← drops all anchor text
```
omnifeed does **no** link post-processing of its own; the loss is entirely this one flag (confirmed: `engine.go:191-197`).
Note `crawlMarkdown.MarkdownWithCitations` is already defined in the wire type (`engine.go:59-72`) but never read.

**Change (pick one).**
- Simplest: set `ignore_links: false` so anchors render as `[text](url)`.
- Token-leaner: switch the response read to crawl4ai's **citations** markdown (keeps anchor text inline, moves URLs to a
  compact footnote table) by enabling it in the generator and reading `MarkdownWithCitations` first in the precedence at
  `engine.go:164-169`.
- Safest rollout: gate it behind a new `OMNIFEED_CRAWL4AI_KEEP_LINKS` (default **true**) in `internal/config/config.go`.

**Test.** Extend `internal/engine/crawl4ai/engine_test.go` to capture the request body and assert `ignore_links == false`
(or that citations are requested), plus a fixture whose markdown contains `[Title](https://…)` and assert it survives.

**Risk.** Links add tokens and can reintroduce nav-link noise — mitigated by the citations option and by the existing
`excluded_tags`/pruning. Worth measuring token delta on a few pages.

---

## 2. Don't retry the non-transient anti-bot 500 on the generic path — **biggest efficiency win**

**Problem.** crawl4ai signals an anti-bot block as **HTTP 500** with body `Blocked by anti-bot protection: …`. The generic
engine calls `DoRetry` with an empty `RetryConfig{}`, which defaults to **3 attempts** and retries all 5xx — so every block
re-drives the full (expensive) browser crawl 3×. The Reddit path already learned this and uses `MaxAttempts: 1` with a
comment saying exactly this; the generic path didn't get the memo.

**Evidence (telemetry).** `crawl4ai status="error"` averaged **28.4 s** vs **6.7 s** for success; **113.6 s / 47%** of all
crawl time was spent failing. `x.com/dhh` was re-rendered ~9× (`minimal_text, no_content_elements`), the two PDFs 3× each.

**Where.**
- Retry defaults: `internal/httpx/retry.go:19-143` (`MaxAttempts` defaults to 3; retries on 5xx/429/network; it already
  captures a 2 KiB body snippet into `StatusError.Body`).
- Generic call site: `internal/engine/crawl4ai/engine.go:125` → `DoRetry(..., httpx.RetryConfig{})`.
- Block classification already exists: `engine.go:207-237` demotes a 500 whose body contains `blocked by anti-bot
  protection` to `KindBotBlock`.
- Contrast/precedent: `internal/engine/reddit/fetcher.go:216-221` (`MaxAttempts: 1`, with the rationale in a comment).

**Change.** Make anti-bot/captcha 500s non-retryable. Cleanest: add an optional predicate to `RetryConfig`, e.g.
`Retryable func(status int, body string) bool`, and in the generic engine pass one that returns `false` when
`isAntibotBlock(body)` (the helper already exists at `engine.go:`). Genuine transient 5xx still retry; non-transient
blocks fail in ~1 attempt. This alone roughly **halves total crawl time** in a blocked-heavy workload.

**Test.** `internal/httpx/retry_test.go`: assert a 500 whose body matches the marker is attempted once; a generic 500 still
retries to `MaxAttempts`.

---

## 3. Detect PDFs / non-HTML and extract (or fail fast) — **turns E1 from a loss into a win**

**Problem.** crawl4ai loads a PDF in the browser, the DOM is empty, and the anti-bot heuristic reports
`minimal_text … (0 chars visible)` → 500 → retried 3× → `bot_block`. No code inspects `Content-Type`/extension before
deciding to browser-render (confirmed by both the antibot and crawl4ai-engine reviews).

**Evidence.** E1: both `rfc8259.pdf` and `rfc9110.pdf` → `bot_block: upstream returned 500` after ~8 s each. Native at least
returned the file + metadata (scored 1; omnifeed 0).

**Where.** Engine selection: `internal/engine/registry.go:47-55` (`Resolve` is first-match-wins). Interface to implement:
`domain.Engine` = `Name()/Matches()/Crawl()` (`internal/domain/document.go:62-66`). Wiring: `cmd/omnifeed/main.go:95-98`.

**Change.** Add a small `internal/engine/pdf` engine that `Matches` a `.pdf` path (and/or sniffs `application/pdf`), fetches
the bytes via `internal/httpx`, and extracts text with a Go PDF lib (e.g. `rsc.io/pdf` or `github.com/ledongthuc/pdf`).
Register it **before** the crawl4ai fallback in `main.go`. Minimum viable version: short-circuit `.pdf` to a clear
"PDF not supported" `Document` so it never enters the browser/anti-bot retry loop. Pair with #2 so even un-typed binary
blobs fail fast.

**Test.** `internal/engine/pdf/engine_test.go` with a tiny fixture PDF; assert text extracted and that `Matches` claims
`.pdf` but not `.html`.

---

## 4. Add a structured Hacker News engine — **mirrors the Reddit win for HN**

**Problem.** HN goes through the generic engine, so (a) the front page loses every title (see #1) and (b) comment threads
come back **flat** — no nesting — unlike the Reddit engine's TOON tree. `AGENTS.md` literally names "Hacker News" as the
example new engine.

**Evidence.** HN1 fidelity 2/5 (titles dropped); HN3 captured comment bodies + in-comment code but no tree.

**Where.** Same extension points as #3 (`domain.Engine`, registry, `main.go:95`). Template: the Reddit engine
(`internal/engine/reddit/` — `engine.go`/`fetcher.go`/`parser.go` + `testdata/` fixtures).

**Change.** New `internal/engine/hackernews` engine matching `news.ycombinator.com`. Use the open HN APIs — Firebase
(`hacker-news.firebaseio.com/v0/`) for items or Algolia (`hn.algolia.com/api/v1/`) for front page + nested comments — and
render a TOON tree like Reddit. HN's APIs are open JSON (not bot-blocked), so this engine can fetch directly via `httpx`
(faster, and it sidesteps the empty-render/anti-bot path entirely). Register before the fallback.

**Test.** `engine_test.go` (URL matching), `parser_test.go` against a saved API fixture (front page + a nested thread),
mirroring the Reddit test layout.

---

## 5. Handle Reddit subreddit *listing* URLs

**Problem.** The Reddit engine deliberately claims only `/comments/<id>` permalinks and share links; `/r/<sub>/` listings
fall through to the generic engine — which Reddit then serves a bot block page (HTTP 200, "you've been blocked by network
security"). `engine_test.go` even lists `reddit.com/r/golang` as an expected fall-through.

**Evidence.** R1 (`/r/devops/`) → omnifeed `captcha` via the generic path; the two `/comments/` threads (R2, R4) succeeded
via the Reddit engine in ~1.2–1.7 s.

**Where.** Matcher: `internal/engine/reddit/engine.go:73-82` + `parser.go` (`permalinkRE`, `shareRE`). Fetch machinery:
`internal/engine/reddit/fetcher.go` already does in-browser `fetch()` of `…​.json` URLs and could build
`/r/<sub>/<sort>.json` the same way.

**Change.** Extend `Matches` to claim `/r/<sub>/` and `/r/<sub>/{hot,new,top,rising}` (not search/wiki/user), add
`FetchSubredditListing(...)` building `/r/<sub>/<sort>.json?limit=…` through the existing `browserFetch`, and a small
listing parser (post list, not a comment tree). Routes listings off the bot-blocked generic path.

**Test.** Add the listing URLs to `engine_test.go` `claim` set; a `testdata/` listing fixture + parser test.

---

## 6. Tame boilerplate & duplication in generic markdown

**Problem.** Generic fetches carry nav/footer noise and sometimes duplicate whole sections — the Kong blog repeated its
"Recommended posts" block **3×** (bloat → speed 2/5), GitHub pages carried "Uh oh! error while loading" fragments.

**Evidence.** W4 (Kong) tripled footer; G1/G2/G3 "Uh oh" fragments.

**Where.** `internal/engine/crawl4ai/engine.go:96-112`: `excluded_tags = [nav,footer,header,form,aside]` and
`PruningContentFilter{threshold:0.48, fixed}`; response prefers `fit_markdown` then `raw_markdown` (`:164-169`).

**Change.** Tune the content filter — raise the prune threshold or switch to crawl4ai's `BM25`/`fit_markdown`-first for
generic pages, and/or add common boilerplate selectors to `excluded_tags`. Expose the threshold as
`OMNIFEED_CRAWL4AI_PRUNE_THRESHOLD` so it's tunable without a rebuild. Lower priority than #1–#3.

**Test.** Fixture with a duplicated footer block; assert it's pruned / not tripled.

---

## 7. SearXNG engine hygiene — *deployment repo* (`omnifeed/searxng/settings.yml`)

**Problem.** Search returned results (13/13 ok, avg 1.65 s) but several default engines are dead weight: `google`'s parser
threw `IndexError` twice, `brave` hit **HTTP 429** (suspended 180 s), and `wikipedia` (400) + `wikidata` (3 s timeout) fire
on *every* query despite being useless for a dev gateway; `ahmia`/`torch` (Tor) fail to load at boot. Results survived only
via StartPage/DuckDuckGo fallbacks.

**Where.** `omnifeed/searxng/settings.yml` (currently `use_default_settings: true` with no `engines:` block). *(The
`limiter: false` and "X-Real-IP not set" warnings are benign-by-design per the file's own comment — leave them.)*

**Change.** Add an `engines:` override disabling the irrelevant/fragile ones for this workload:
```yaml
engines:
  - name: wikidata
    disabled: true
  - name: wikipedia
    disabled: true
  - name: ahmia
    disabled: true
  - name: torch
    disabled: true
  # consider de-prioritising brave if 429s persist; keep google/startpage/duckduckgo
```
Cuts per-query latency and log noise. Mirror the change into the production settings generator (the file notes prod renders
its own `settings.yml`).

---

## 8. Observability: make retry waste visible

**Problem.** The 47%-of-time-on-failures finding came from eyeballing logs; there's no metric for attempts or retry-seconds,
so the #2 regression/fix isn't trackable.

**Where.** `internal/observability/metrics.go:63-66` (`Observe`), reasons in `internal/domain/errors.go:16-26`.

**Change.** Add `omnifeed_request_attempts_total` (or an attempts label/histogram) so a blocked URL retried 3× is visible,
and #2's improvement shows up as a drop. XS change; do it alongside #2 to prove the win.

---

## Sequencing

Ship **#1 + #2 first** — two small diffs that recover the headline fidelity loss (titles/links) and ~half the wasted
latency. Then **#3** (PDF) and **#5** (subreddit listings) as self-contained engines/matchers, **#4** (HN engine) as the
larger structured-engine effort, and **#6–#8** as polish. **#7** is a deployment-repo config change, independent of the Go
code.

Per repo conventions (`AGENTS.md`): each change ships with a table/`httptest` test, a Conventional Commit (`feat:`/`fix:`/
`perf:`), a new `OMNIFEED_*` knob documented in the README table where one is added, and a green `make check`.
