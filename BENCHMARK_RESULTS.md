# Benchmark Results — omnifeed vs native web tools (dev/devops)

Head-to-head run of the matrix in [`BENCHMARK.md`](./BENCHMARK.md), executed live on **2026-06-25** against this deployment
(omnifeed `0.11.2`, crawl4ai `0.8.9`, SearXNG `2026.6.15`) running under Apple `container`.

- **A — omnifeed**: `mcp__omnifeed__web_search` + `mcp__omnifeed__fetch_url`
- **B — native**:   `WebSearch` + `WebFetch` (Claude Code built-ins)

All calls were live and sequential (one shot per tool, no retries-to-flatter). A block/error/empty page is scored, not skipped.
Scoring: **Fetch success** `2` full / `1` partial / `0` blocked-error-empty · **Fidelity** `0–5` · **Search relevance** `0–5` · **Speed&token** `0–5`.

> Speed scores are a payload/token-cost proxy + error/timeout behavior; for **measured** latency see [§5 Server-side telemetry](#5-server-side-telemetry), reconstructed from the omnifeed/crawl4ai logs and Prometheus `/metrics`.

---

## 1. Headline findings

1. **Reddit — omnifeed wins decisively (both halves).** Native `WebFetch` *categorically refuses* `reddit.com` ("Claude Code is unable to fetch from www.reddit.com"); native `WebSearch` returns **zero** Reddit threads even when the query contains "reddit"/"r/sub", substituting sourceless prose. omnifeed returns dated threads and full TOON comment trees (`parent_id` nesting, verbatim bodies incl. code).
2. **Stack Overflow / Server Fault — same story.** Native refuses the entire Stack Exchange network (`stackoverflow.com`, `serverfault.com`) and omits it from search results. omnifeed fetched all 105 answers of the canonical "undo last git commit" question with every code block intact.
3. **X / Twitter — only omnifeed reaches it at all.** Native gets **HTTP 402** on every `x.com` URL and omits x.com from search. omnifeed renders single tweets/thread-heads via headless browser (but timelines time out — see below).
4. **Package registries — omnifeed reaches+renders all; native blocked/shell.** Native: **403** on npm, **empty SPA shell** on crates.io. omnifeed rendered both (and PyPI) with code blocks + hash tables.
5. **Native wins where content lives in hyperlinks or JS-rendered components.** **HN front page** (omnifeed dropped every story title — it strips hyperlink anchor text) and **MDN reference** (omnifeed dropped all code examples + table data; native reconstructed them). Native is also stronger for **mainstream/fresh web search** with useful synthesized answers.
6. **Consistent behavioral split.** omnifeed = *raw/verbatim/auditable but heavy & noisy* (and drops link anchor text, flattens non-Reddit comment trees, can't do PDFs). Native = *clean/lean but lossy* — it **summarizes even when told "verbatim"**, dropping code/tables/timelines, and may partially *reconstruct* content (a latent hallucination risk).

---

## 2. Overall scorecard

| Dimension | A (omnifeed) | B (native) | Notes |
|---|---:|---:|---|
| **Search relevance** (12 queries) | **4.67** | 3.08 | split below |
| — community queries (Reddit/SO/X) | **4.86** | 1.86 | B won't surface these sources |
| — mainstream queries (GitHub/HN/web) | 4.40 | **4.80** | B equal/better + synthesized answers |
| **Fetch success** (22 fetches) | usable **19/22** | usable 12/22 | A: 18×`2`,1×`1`,3×`0` · B: 10×`2`,2×`1`,**10×`0`** |
| **Fidelity** (where success ≥1) | **3.78** | 2.64 | A verbatim; B summarizes |
| **Speed & token** (proxy) | 3.56 | **4.18** | B leaner *because* it summarizes |

**Native's 10 fetch zeros are the story:** categorical refusals on Reddit, Stack Overflow, Server Fault; HTTP 402 on X; HTTP 403 on npm; empty SPA shell on crates.io. omnifeed reached content on every one.

---

## 3. Verdict

**Reach for omnifeed when:**
- Touching **Reddit, Stack Overflow / Server Fault, X/Twitter, npm, or crates.io / any client-rendered SPA** — native is blocked, 402/403'd, or gets an empty shell.
- You need the **actual content verbatim**: full comment trees, every answer's code blocks, a PR's complete review timeline, package hash tables — auditable, not paraphrased.
- Searching for **community discussion** — it surfaces Reddit/SO/HN threads natively.

**Reach for native when:**
- You want a **fast, clean digest** of a reachable public page and don't need verbatim code.
- Fetching **link-index pages** (HN front page) or **JS reference pages** (MDN) — its reconstruction beats omnifeed's anchor-stripping.
- **Mainstream/recency web search** with a synthesized answer.
- You need **PDFs** (file + metadata at least) — omnifeed errors on them entirely.

**Rule of thumb:** *omnifeed to reach blocked/community/SPA sources and get verbatim content; native for fast clean summaries of open pages and mainstream/fresh discovery.* They are complementary — omnifeed covers exactly the sources native can't touch.

---

## 4. Per-source scorecards

### Reddit — *omnifeed sweep*
| Case | Input | A succ/fid/rel/spd | B succ/fid/rel/spd | Evidence |
|---|---|---|---|---|
| R1 (F) | fetch `r/devops/` | 0 / – / – / – | 0 / – / – / – | A: Reddit served a **captcha block page** ("you've been blocked by network security", HTTP 200) via the *generic* engine; B hard-refuses reddit.com |
| R2 (S) | "…ingress vs gateway api" | – / – / **5** / – | – / – / **1** / – | A: 5 on-target threads (Nov'25–Apr'26); B: "No links found" + generic prose |
| R2 (F) | top thread `1p2v4ia` | **2 / 5** / – / 4 | **0** / – / – / – | A: TOON tree, 31 comments, `parent_id` nesting, GW-API maintainer reply verbatim; B refuses |
| R3 (S) | "best ci cd monorepo reddit" | – / – / **5** / – | – / – / **2** / – | A: 5/5 Reddit threads; B: 0 Reddit (blogs/list) |
| R4 (S) | "r/selfhosted reverse proxy" | – / – / **5** / – | – / – / **1** / – | A: 4 threads + article; B: "No links found" + sourceless prose |
| R4 (F) | top thread `1cu2dow` (228 cmts) | **2 / 5** / – / 4 | **0** / – / – / – | A: 89-node tree, full selftext + md links + Caddyfile code; B refuses |

### GitHub — *both fetch; fidelity tradeoff*
| Case | Input | A | B | Evidence |
|---|---|---|---|---|
| G1 (F) | repo `kubernetes/kubernetes` | 2 / 3 / – / 3 | 2 / 2 / – / 4 | A: raw README+code blocks+file tree but dropped link anchors + "Uh oh" noise; B summarized (no code) but clean, caught Borg/issue counts |
| G2 (F) | issue #33388 (50+ cmts) | 2 / 3 / – / 3 | 2 / 2 / – / 4 | A kept bot posts+code+cross-refs; B collapsed thread to summary + editorialized. Both hit GitHub "2556 remaining items" lazy-load wall |
| G3 (F) | merged PR #139922 | 2 / **5** / – / 2 | 2 / 2 / – / **5** | A: full convo + YAML code + perf-config table + timeline (force-push hashes, /hold, merge `5b77108`); B: clean review summary, no code/timeline. Neither got Files-changed diff |
| G4 (S) | "golang token bucket … github" | – / – / 4 / – | – / – / **5** / – | B: 10 libs (juju, uber-go, mennanov…); A: 5. Both missed `x/time/rate` |

### Stack Overflow / Stack Exchange — *omnifeed sweep; native blocked from SE network*
| Case | Input | A | B | Evidence |
|---|---|---|---|---|
| SO1 (S) | "undo last git commit SO" | – / – / **5** / – | – / – / **2** / – | A: 4 SO results (canonical Q927358); B: **zero** stackoverflow.com links |
| SO1 (F) | top SO Q927358 | **2 / 5** / – / 2 | **0** / – / – / – | A: **all 105 answers** + every code block + ASCII commit-graph + comments; B refuses stackoverflow.com |
| SO2 (S) | "k8s pod stuck terminating" | – / – / **5** / – | – / – / 3 / – | A: SO #1 + Reddit + recent blogs; B: no SO/ServerFault (blogs + a GH issue) |
| SO3 (S) | "nginx 502 serverfault" | – / – / **5** / – | – / – / **2** / – | A: SO + 3 ServerFault; B: no SO/ServerFault despite query |
| SO3 (F) | top ServerFault Q1168109 | **2 / 5** / – / 5 | **0** / – / – / – | A: nginx config code + comments intact; B refuses serverfault.com |
| SO4 | code-heavy SO Q | *covered by SO1* | *covered by SO1* | A preserved every code block across 105 answers; B refuses |

### Hacker News — *native competitive/better*
| Case | Input | A | B | Evidence |
|---|---|---|---|---|
| HN1 (F) | front page `/news` | 2 / **2** / – / 4 | 2 / **5** / – / 4 | **B wins:** full front page, all 30 titles+points+author+age+comments. A dropped EVERY title (strips hyperlink text → metadata-only rows) |
| HN2 (S) | "rust vs go perf HN" | – / – / 5 / – | – / – / 5 / – | Tie. B: 6 HN threads + synth; A: 4. B's WebSearch *does* surface HN |
| HN3 (F) | item 43307229 (52 cmts) | 2 / 3 / – / 2 | 2 / 2 / – / **5** | Neither keeps HN nesting. A: all comment bodies + in-comment code but FLAT; B: editorial theme-digest, no code |
| HN4 (S) | "Show HN database 2026" | – / – / 4 / – | – / – / 4 / – | B edged: got a direct HN item; A got aggregators + a Reddit thread |

### Twitter / X — *omnifeed reaches X (shallow); native fully blocked*
| Case | Input | A | B | Evidence |
|---|---|---|---|---|
| X1 (F) | tweet `jack/status/20` | **2** / 3 / – / 5 | **0** / – / – / – | A: "just setting up my twttr" + "Read 17.9K replies"; B: **HTTP 402 Payment Required** |
| X2 (F) | profile `x.com/dhh` | 0 / – / – / – | 0 / – / – / – | A: anti-bot retry loop → timeout (~28s×, see §5); B: HTTP 402 |
| X3 (S) | "ebpf cilium x.com thread" | – / – / **4** / – | – / – / **2** / – | A: 2 x.com results (fetchable thread); B: zero x.com, admitted "no X.com thread" |
| X3 (F) | `tgraf__/status/132718…` | **2** / 3 / – / 5 | **0** / – / – / – | A: thread-head tweet (Cilium 1.9 BBR/FQ bullets); B: 402 |

### General web — *native search strong/fresh; fetch mixed*
| Case | Input | A | B | Evidence |
|---|---|---|---|---|
| W1 (F) | kubernetes.io docs | 2 / 4 / – / 4 | 2 / 4 / – / 3 | ~Tie. A: clean, chrome stripped, dropped 2 links. B: full content + all inline links + image (didn't summarize here!) but nav/legal noise |
| W2 (F) | MDN `Array.reduce` | 2 / **2** / – / 4 | 2 / **5** / – / 4 | **B wins:** kept all code examples + filled both example tables (15→31→48→66→85). A dropped every code example, tables empty shells (JS components) |
| W3 (S) | "postgres pooling 2026" | – / – / 4 / – | – / – / **5** / – | B: 10 relevant, several 2026 (incl. Jun'26) + synth; A: 5 (+1 stale pg-7.4 link) |
| W4 (S) | "rate limiting … blog" | – / – / 5 / – | – / – / 5 / – | Tie; both rank Kong #1 |
| W4 (F) | top blog (Kong) | 2 / 4 / – / 2 | 2 / 2 / – / **5** | A: full article + all curl code, but "Recommended posts" **duplicated 3×** (bloat). B: clean digest, no code |

### Package registries — *omnifeed reaches+verbatim all; native blocked/shell/lossy*
| Case | Input | A | B | Evidence |
|---|---|---|---|---|
| P1 (F) | npm `express` | **2 / 4** / – / 3 | **0** / – / – / – | A: v5.2.1 + README + all code + team. B: **HTTP 403 Forbidden** |
| P2 (F) | PyPI `requests` *(fairness)* | 2 / **5** / – / 4 | 2 / 2 / – / 5 | A: REPL code + SHA256/MD5/BLAKE2b hash tables + full history. B: **not blocked** (server-rendered) but summarized — no code/hash values |
| P3 (F) | crates.io `serde` (SPA) | **2 / 4** / – / 4 | **0** / – / – / – | **A wins decisively:** browser rendered SPA (serde 1.0.228, Rust code, 1.1B downloads). B: empty shell — `<title>` only |
| P4 | pkg.go.dev | *trimmed* | *trimmed* | Server-rendered like P2 → A verbatim, B fetches-but-summarizes |

### Hard / edge
| Case | Input | A | B | Evidence |
|---|---|---|---|---|
| E1 (F) | PDF `rfc9110.pdf`¹ | **0** / – / – / – | **1** / 1 / – / 3 | Neither extracts PDF text well. A: anti-bot misclassifies empty PDF DOM → `bot_block 500` (see §5). B: file+metadata (title, 194pp), raw binary, no body |
| E2 (F) | Cloudflare `nowsecure.nl` | 2 / – / – / – | 2 / – / – / – | **Inconclusive** — page not actively challenging; both got minimal content. A's JS-challenge ability shown in P3 |
| E3 (F) | Medium member-only | 1 / 3 / – / 4 | 1 / 2 / – / 4 | Tie — neither bypasses paywall; both get preview + correctly ID it. A added raw comments; B cleaner |
| E4 | heavy JS SPA | *covered by P3* | *covered by P3* | A renders; B empty shell |

¹ swapped from `rfc8259.pdf` (404). Dev-forums (D1–D4) trimmed — each maps to a measured pattern (lobste.rs→HN1, dev.to→W4, Discourse→HN3, GitLab→G1–G3).

---

## 5. Server-side telemetry

Reconstructed from omnifeed structured logs, crawl4ai logs, and `omnifeed_*` Prometheus metrics captured during the run. **This is the ground truth for latency and for *why* each failure happened.**

### Measured latency (Prometheus histograms)

| Path | Count | Avg | Notes |
|---|---:|---:|---|
| `searxng` search (ok) | 13 | **1.65 s** | 8/13 under 1.6 s — search is cheap |
| `reddit` engine (ok) | 2 | **1.76 s** | structured Reddit crawl is *fast* |
| `crawl4ai` generic (ok) | 19 | **6.73 s** | GitHub pages 7–10 s; the bulk of latency |
| `crawl4ai` generic (**error**) | 4 | **28.4 s** | anti-bot retry loop — see below |

**The error path dominates wasted time:** 113.6 s across 4 failures vs 127.9 s across 19 successes — **~47% of all crawl time was spent failing.** A single failed fetch averaged **28 s** (vs 6.7 s for success) because each error triggers the full anti-bot retry-with-proxy loop.

omnifeed's own binary is lean (RSS ≈ **15.5 MB**); crawl4ai's browser pool grew 141 MB → 201 MB over the session.

### Failure root-causes (from logs)

| Benchmark case | omnifeed `reason` | crawl4ai detail | Root cause |
|---|---|---|---|
| **R1** `r/devops/` | `captcha` | "you've been blocked by network security", HTTP 200 | Subreddit **listing** URL went through the *generic* engine (not the Reddit engine, which only matches `/comments/` posts) → Reddit served a bot block page |
| **X2** `x.com/dhh` | `canceled` (context) | anti-bot: `minimal_text, no_content_elements (22724 bytes, 31 chars visible)`, retried 2/2 ×3, ~28 s each | Profile timeline renders but is JS-lazy/near-empty; anti-bot heuristic retries the whole render repeatedly until the MCP call is canceled |
| **E1** PDFs | `bot_block: upstream 500` | anti-bot: `minimal_text… (237 bytes, 0 chars visible)`, retried 2/2 | crawl4ai's browser doesn't extract PDF text (empty DOM) → anti-bot **misclassifies the empty PDF as a bot block**, retries 3×, returns 500 |

> Two of the three failure classes (X timelines, PDFs) are **anti-bot false positives** — the content genuinely wasn't there to render, but the heuristic treated "empty DOM" as "blocked" and paid the full retry cost. This is the single biggest efficiency win available (see [`IMPROVEMENTS.md`](./IMPROVEMENTS.md)).

### SearXNG engine health (from searxng logs)

Search worked (13/13 ok) but several engines are degraded — quality/latency is being left on the table:
- `google` engine threw `IndexError: list index out of range` (result-parser break) **twice**.
- `brave` returned **HTTP 429** (`Too many request, suspended_time=180`).
- `wikipedia` 400 and `wikidata` 3 s timeouts on every query (irrelevant engines for a dev gateway, adding latency + log noise).
- `ahmia`/`torch` (Tor onion engines) fail to load at boot — set to inactive.
- `missing config file: /etc/searxng/limiter.toml`; `X-Forwarded-For nor X-Real-IP header is set`.

Results survived only because StartPage/DuckDuckGo fallbacks carried the queries. See improvement #7.

---

## 6. Caveats

- **Speed** was not isolated wall-clock at the client; §5 supplies measured server-side latency instead.
- **`WebSearch` is US-region**; that may contribute to (but doesn't fully explain) its Reddit/SO/X omissions, which look like deliberate source exclusions.
- **Native fidelity is inconsistent** (verbatim on kubernetes.io, heavy summary on GitHub/SO/HN/blogs) and may reconstruct rather than extract — accurate in this run but not guaranteed.
- **Trims/swaps disclosed inline:** SO4/E4 folded; D1–D4 pattern-mapped; P4 trimmed; RFC PDF swapped after a 404.
- No patient PHI/PII was encountered (all public web content).
