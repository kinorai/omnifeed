# Benchmark: omnifeed MCP vs native web tools (dev/devops focus)

## Goal
Run a controlled, identical-input head-to-head between two web stacks and tell me
which to reach for, per source. Actually CALL the tools and record real evidence —
do NOT answer from prior knowledge.

## Contestants
- **A — omnifeed**: `mcp__omnifeed__web_search` + `mcp__omnifeed__fetch_url`
- **B — native**:   `WebSearch` + `WebFetch`

## Setup / sanity check (do first)
1. Confirm all 4 tools are callable (load deferred schemas via ToolSearch if needed).
   If omnifeed's two are missing, STOP and tell me.
2. One warm-up call per tool. Note any rate-limiting; if either stack throttles, pause and log it.
3. Run cases **sequentially**, one shot per tool — do NOT parallelize across subagents
   (keeps latency comparable and avoids retry-flattering).

## Method — same input to BOTH stacks, per case
- **SEARCH cases (S)**: fire the *identical query string* at `mcp__omnifeed__web_search` and
  `WebSearch`. Capture the top 5 results (title + URL + snippet) from each.
- **FETCH cases (F)**: fire the *identical URL* at `mcp__omnifeed__fetch_url` and `WebFetch`.
  - ⚠️ **Fairness rule**: `WebFetch` summarizes by default; `fetch_url` returns raw. To compare
    raw fidelity, ALWAYS call `WebFetch` with this exact prompt:
    > "Output the full page content verbatim as markdown. Preserve every code block, table, and
    > comment thread. Do not summarize, omit, reorder, or editorialize."
  - `fetch_url` takes only the URL.
- **Search→Fetch cases (S→F)**: run the search, pick the top relevant *live* result, then fetch
  that same URL through both fetchers. (Avoids stale hardcoded URLs/IDs.)
- Record per call: raw result list / content snippet, any error/block page **verbatim**, approx
  output size (KB or ~tokens), and rough wall-clock feel.
- **One shot per tool.** No retries to flatter a tool; if you must retry, log it as a strike.
- A block / error / empty / "enable JS" page is a **result**, not a skip — score it.

## Scoring (each case, each stack)
- **Fetch success** [F cases]: `2` = full usable content · `1` = partial/truncated/stub · `0` = blocked/error/empty
- **Content fidelity** [F cases, if success ≥1]: `0–5` — code blocks intact, tables kept, comment
  threads captured, nav/ads/cookie noise stripped, sane structure.
- **Search relevance & freshness** [S cases]: `0–5` — top 3 on-target, expected source present,
  recent items surfaced where the query implies recency.
- **Speed & token cost** [all cases]: `0–5` — fast + lean = 5; slow or bloated = lower (approximate).

## Test matrix (~4 per source; trim if a stack is clearly throttled)

**Reddit** (omnifeed's claimed strength; native often hits block/JS pages)
- R1 (F) fetch `https://www.reddit.com/r/devops/` — real posts vs block page?
- R2 (S→F) search "reddit kubernetes ingress vs gateway api"; fetch the top reddit.com thread; judge comment-tree fidelity.
- R3 (S) search "best ci cd for monorepo reddit" — how many real Reddit threads surface?
- R4 (S→F) search "r/selfhosted reverse proxy recommendations"; fetch the top thread.

**GitHub**
- G1 (F) fetch `https://github.com/kubernetes/kubernetes` — README / links / code intact?
- G2 (F) open kubernetes/kubernetes issues, pick one with 50+ comments, fetch it — comment-thread fidelity.
- G3 (F) pick a recent merged PR in a popular repo, fetch it — diff + review conversation captured?
- G4 (S) search "golang token bucket rate limiter library github" — relevance.

**Stack Overflow / Stack Exchange**
- SO1 (S→F) search "how to undo last git commit stackoverflow"; fetch top SO result — accepted + alt answers, all code blocks?
- SO2 (S) search "kubernetes pod stuck terminating" — does top 5 include SO / Server Fault?
- SO3 (S→F) search "nginx reverse proxy 502 serverfault"; fetch top result.
- SO4 (S→F) find an SO question with multiple code-heavy answers; fetch — are ALL code blocks preserved?

**Hacker News**
- HN1 (F) fetch `https://news.ycombinator.com/news` — structured front page?
- HN2 (S) search "rust vs go performance hacker news" — recall + recency.
- HN3 (S→F) find a 200+ comment HN item, fetch `news.ycombinator.com/item?id=…` — deep comment nesting captured?
- HN4 (S) search "Show HN database 2026" — surfaces HN posts?

**Twitter / X** (auth-walled — a potential second differentiator like Reddit)
- X1 (F) fetch a public tweet URL from a well-known eng account.
- X2 (F) fetch a public X profile timeline (a public dev account).
- X3 (S→F) search "<recent infra topic> x.com thread"; attempt to fetch — can either stack reach X content at all?

**General web search**
- W1 (F) fetch a kubernetes.io docs page — JS-rendered content captured?
- W2 (F) fetch an MDN reference page — tables / code intact?
- W3 (S) search "postgres connection pooling best practices 2026" — relevance + freshness.
- W4 (S→F) search "rate limiting algorithms engineering blog"; fetch top blog post.

**Dev forums & docs**
- D1 (F) fetch `https://lobste.rs` front page or a story thread.
- D2 (S→F) search "dev.to docker multi-stage build"; fetch top dev.to article.
- D3 (S→F) find a Discourse thread (discuss.python.org / users.rust-lang.org), fetch it — replies captured?
- D4 (S→F) find a GitLab issue or MR, fetch it — diff / discussion captured?

**Package registries** (crates.io is a JS SPA — strong fidelity test)
- P1 (F) fetch `https://www.npmjs.com/package/express` — version, deps, README?
- P2 (F) fetch `https://pypi.org/project/requests/`
- P3 (F) fetch `https://crates.io/crates/serde` — SPA: content rendered or empty shell?
- P4 (F) fetch `https://pkg.go.dev/github.com/gin-gonic/gin`

**Hard / edge cases**
- E1 (F) fetch a PDF: `https://www.rfc-editor.org/rfc/rfc9110` — text extracted?
- E2 (F) fetch a known Cloudflare-protected page — challenge page or content?
- E3 (S→F) search a Medium member-only article; fetch it — paywall handling.
- E4 (F) fetch a heavy JS SPA doc site — content vs empty shell.

## Output
1. **Per-source scorecard tables** — one row per case: input · A scores · B scores · 1-line evidence.
2. **Overall scorecard** — average each dimension per stack, plus fetch-success counts (how many 2/1/0 each).
3. **Headline findings** — call out the Reddit and X matchups explicitly; any source where one stack wins decisively.
4. **Verdict** — a crisp "Use omnifeed when… / use native when…" guide for a dev/devops workflow.
5. **Appendix** — raw evidence (result lists, content/error snippets) so every score is auditable.
6. After printing the tables, offer to render the report as an HTML artifact.

Note any caveats: WebSearch region limits, rate-limiting encountered, stale URLs (swap in a live one and say so).
