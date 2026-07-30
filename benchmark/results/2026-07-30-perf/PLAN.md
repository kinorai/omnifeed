# Optimization plan — from the 2026-07-30 perf benchmark

## Status (2026-07-30, end of day)

Implemented on `perf-improvements`:
- `e1b81c8` feat(searxng): degraded-search detection (§1 detection — MCP error,
  `degraded` reason, 3 table tests)
- `26c9a46` fix(crawl4ai): preserve_tags/classes payload, raw-markdown fence
  fallback, empty-extraction guard (KindThinContent), page_timeout 60s clamp
  (§3 + §6)
- `b11ac88` feat(mcp): classified reason + HTTP status in tool error messages,
  same enrichment in openwebui/searchapi, URL redaction (§7)
- `9dde05e` chore(compose): images pinned (crawl4ai 0.9.2, searxng 2026.7.22),
  engine pool widened (mojeek, bing, duckduckgo web), braveapi block staged
  inactive awaiting the owner's key (§1 config)
- Upstream: crawl4ai issue #2110 + PR #2111 filed.

Second batch (same day):
- `5baeaf8` feat(searxng): per-domain pacing + one retry on degraded
  (OMNIFEED_SEARXNG_DEGRADED_RETRY_DELAY, default 2s) — §1 complete
- `58840b5` feat(fetch): max_chars/start_char + OMNIFEED_FETCH_MAX_CHARS
  (default 120000) + anthropic/maxResultSizeChars annotation; engines stamp
  content_type; TOON/JSON never byte-truncated; openwebui default unlimited — §2 complete
- `850ae3b` feat(crawl4ai): chrome trim (script/style/noscript, consent popups,
  OMNIFEED_CRAWL4AI_EXCLUDED_SELECTOR default-on; OMNIFEED_CRAWL4AI_TARGET_ELEMENTS
  gated off) — §5 code complete
- `9e62951` style: gofmt drift
- `3aa2934` feat(github): dedicated issues/PR engine (REST, optional
  OMNIFEED_GITHUB_TOKEN, 500-comment / 30KB-diff caps) — §4 first half complete
- `2fd43a2` feat(discourse): topic engine (OMNIFEED_DISCOURSE_HOSTS allowlist,
  print-view + batched fallback, 500-post cap) — §4 second half complete

Remaining — validation, not code (needs the rebuilt binary + restarted stack):
1. Rebuild omnifeed + `compose up` with the new pins/settings.
2. Owner: put the Brave key into searxng/settings.yml locally (braveapi block,
   remove `inactive: true`; never commit the key); one keyed `site:reddit.com`
   diff vs the scrapers.
3. Live regression pass vs the benchmark raw data: the 7 empty-search queries;
   W1/W2/D2/P3 code-block fidelity (§3 golden fixtures); SO3 (chrome canary);
   W4 (must now error thin_content, not empty-ok); the 5 overflow URLs with the
   default cap; G2/G3 via the GitHub engine and D3 via the Discourse engine
   (completeness + server-side latency vs crawl4ai numbers).
4. Push `perf-improvements` and open the single MR.

Scope decided 2026-07-30: implement candidates 1–6 from [REPORT.md](REPORT.md)
plus clearer errors (7). PDF *support* declined. Candidate 8 (Reddit caps)
dropped — already fully configurable and documented (README Reddit knobs +
per-request MCP params). All code lands on branch `perf-improvements`, one MR
together with these docs.

Each item below was validated by a dedicated research pass (repo read + upstream
source/issues + live measurements where possible). Citations in the item notes.

## 1. SearXNG empty-result reliability — lossless · effort S–M

**Confirmed mechanism** (searxng source): engines suspended after 429/CAPTCHA are
skipped *without a network call* for `search.suspended_times.*` seconds → HTTP 200,
`results: []`, near-instant — exactly the benchmark's 0.2–0.6 s empties. The
failure is visible in the same JSON response: `unresponsive_engines` as
`[[engine, "Suspended: too many requests"], …]` (searx/webutils.py
`get_json_response`; only emitted on the success path).

- [ ] `internal/search/searxng`: add `UnresponsiveEngines [][]string` to the wire
  struct. Rule: `len(results)==0 && len(unresponsive_engines)>0` → degraded
  upstream (distinct `observability.Reason`, e.g. `degraded`, + metric label +
  log), vs honest zero hits. Decided 2026-07-30: surface the degraded case as an
  **MCP error** (an agent seeing bare `[]` wrongly concludes "no results exist").
- [ ] **Keep the SearXNG image current and date-pin it** (community finding #1,
  gh-CLI pass 2026-07-30): every 429/CAPTCHA wave in the upstream record was
  fixed within hours-to-weeks, and the burst-blocking DDG failure specifically
  was fixed by PR #5943 (`Sec-Fetch-*` headers, merged 2026-04-04 — "blocked on
  the second consecutive search" → "4 concurrent tabs fine"). SearXNG publishes
  no GitHub releases, only date-tagged images: pin a dated tag instead of
  `latest`. RESOLVED 2026-07-30: running image digest `854f239d…` =
  **2026.7.22-ef8f6470e** — fully current, already contains #5943 and the
  google→google-cse swap. So the benchmark's suspensions happened *despite* a
  current image: burst load alone trips the engines, which reweights this item
  toward pool widening + braveapi + caller pacing + detection.
- [ ] `searxng/settings.yml`: widen the engine pool so one suspension can't
  empty the result set — swap dead classic `google` (upstream `inactive: true`
  since ~2026-06, issue #6453) for `google cse` (active by default, no API key),
  and enable `mojeek`, `bing`, `duckduckgo web`, `startpage`. Caveat on `qwant`:
  open 2026-07 issue reports it silently returns fabricated results when the
  server IP is blocked — add it last, if at all. Engines merge by name under
  `use_default_settings: true`.
- [ ] `suspended_times`: do NOT shorten `SearxEngineTooManyRequests` (180 s) —
  community evidence says chasing results with shorter cooldowns just re-triggers
  the upstream block (rejected PR #5839 discussion). Only ensure CAPTCHA
  cooldowns are nonzero. (Reverses this plan's earlier draft.)
- [ ] Caller-side pacing in omnifeed (upgraded from optional): upstream has
  rejected outbound per-engine rate limiting twice (PRs #998, #1273) and the
  DDG throttle PR #5839 — pacing can only live in the caller. Extend the
  existing `httpx` per-domain limiter to the SearXNG search path, plus one
  delayed retry on the degraded signal (suspension is per-engine and
  time-boxed, so a retry genuinely recovers).
- [ ] **Authenticated engines** (researched 2026-07-30 — auth eliminates the
  suspension class outright, since 429/CAPTCHA comes from bot-detection on HTML
  endpoints; contractual APIs replace the *rate* problem with a *volume* quota):
  - `braveapi` (separate module from the scraping `brave` engine): $5 free
    credits/month at $5/1k ⇒ ~1,000 req/month, 50 qps. Known trap: issue #6173
    (422) closed not-planned, fix PR unmerged — triggered only by `time_range`
    values outside day/week/month/year; omnifeed's adapter only ever sends
    those four, so safe. Config: `engine: braveapi`, `api_key: …`.
    DECIDED 2026-07-30: go with braveapi. Reddit-exclusion concern researched
    and REFUTED: Brave kept Reddit through the 2024 crawler lockout (its
    crawler deliberately hides its UA to avoid Google-only robots rules, plus
    the Web Discovery Project ingestion path); fresh 2026 reddit.com results
    verified live via site:reddit.com, x.com indexed too, and the API docs
    have no excluded-domains policy. Exa likewise documents no reddit ban
    (noindex-based exclusion only; Reddit blocks via robots.txt, not noindex).
    Real risks instead: (a) storage-rights terms — caching API results for an
    LLM needs a plan that grants storage; (b) quota cliff (~1,000/mo, down
    from ~5,000 in 2025); (c) single-vendor 429. Mitigation: keep the scraping
    engines registered BEHIND braveapi as overflow — do NOT special-case
    reddit onto scrapers (the API index is the more reliable reddit source).
    Before cutover: one keyed `site:reddit.com` API call diffed against the
    scraping path.
  - `marginalia`: free non-commercial key by email; genuinely authenticated;
    niche small-web/blogs index — supplementary, not a primary.
  - `exaapi` (new, merged 2026-07-19): ~$10 credits/month ⇒ ~1,400 req/month;
    neural index, unproven engine — optional third.
  - Dead ends, do not pursue: `google_cse` **cannot authenticate** (it scrapes a
    `cse_tok` from cse.google.com; your key/quota is never used; the official
    JSON API is closed to new customers and ends 2027-01-01) — keyless
    pool-member only; Mojeek has no free tier and no searxng API engine; Bing
    Search APIs retired 2025-08-11; Kagi is paid-only; Yep has no auth knob.
  - Keys are secrets: keep them out of the committed dev settings.yml (env
    interpolation or an uncommitted overlay).
- Explicitly rejected (community-validated): touching SearXNG's `limiter`
  (inbound-only, irrelevant to upstream 429s), hardcoded token workarounds
  (#4437 pattern), evasion forks / proxy rotation (unmerged POCs, "an arms race
  SearXNG will likely lose", and loses `docker pull` upgrades), paid SERP APIs
  (third-party data flow, out of scope).
- Validate: table test with a fixture `{"results":[],"unresponsive_engines":
  [["brave","Suspended: too many requests"]]}`; check running image date vs
  2026-04-04; replay the 7 benchmark queries when healthy; 50-search soak run
  watching the new metric. (Live confirmation of the failure mode: during the
  2026-07-30 research pass itself, `web_search` returned `[]` for 4 consecutive
  queries mid-task.)

## 2. `fetch_url` size control — lossless (opt-out cap + markers) · effort M

**Facts**: tool returns `doc.PageContent` unbounded; Claude Code default limit is
25k tokens (`MAX_MCP_OUTPUT_TOKENS`), but a tool can declare
`_meta["anthropic/maxResultSizeChars"]` in `tools/list` (char-based, ceiling
500k) which replaces the token limit for text. MCP spec has **no** `tools/call`
pagination; the reference `fetch` server uses `max_length`/`start_index` params
with a truncation notice. omnifeed's `toolSchema` currently cannot emit `_meta`.

- [ ] Params on `fetch_url`: `max_chars` (int, 0 = server default) and
  `start_char` (int, ≥0) — **markdown only**. Truncation marker appended to text:
  `[omnifeed: content truncated at N of M characters. Call fetch_url again with
  start_char=N to continue.]` + `_meta` keys `truncated`, `total_chars`,
  `returned_chars`, `next_start_char`.
- [ ] `OMNIFEED_FETCH_MAX_CHARS` server default (120000 with the annotation;
  80000 if we skip it), 0 = unlimited; caller value wins, clamped to 500000.
- [ ] Add `Meta map[string]any` to `mcp.Tool` + emit `_meta` in `toolSchema`;
  set `anthropic/maxResultSizeChars: 500000` on `fetch_url` (annotate ≥ cap).
- [ ] **Never byte-truncate TOON/JSON** (length markers would lie): engines set
  `content_type` in `Document.Metadata`; generic truncation applies only to
  `markdown`. Structured engines keep structural caps (Reddit knobs exist; HN's
  `frontPageSize`/`maxThreadComments` promoted from constants to options) and
  report `truncated_from` in `_meta` + a prose line.
- [ ] openwebui `/crawl`: same `?max_chars=` param, but default **unlimited**
  (RAG pipelines chunk anyway) — deliberate divergence, documented. searchapi
  untouched (search-only).
- Validate: re-fetch the 5 overflow URLs (R4, SO3, SO4, HN3, P4) with the cap —
  no client spool, marker present, continuation works.

## 3. Code-block fidelity through crawl4ai — lossless · effort S (config) + S (Go)

**Root cause found** (crawl4ai source diffing): whitespace-only `<span>`s inside
highlighted code are decomposed. The scraper path (affects `raw_markdown`) was
fixed in v0.7.8 (issue #1181); the **`PruningContentFilter` path is still
unfixed** and omnifeed reads `fit_markdown` first — whitespace/operator/field
spans score ~0.26–0.36 < threshold 0.48 and get pruned. v0.9.1 added
`preserve_tags`/`preserve_classes` (PR #1904): `_is_preserved` skips the whole
subtree, protecting code.

- [ ] Pin the crawl4ai image: running instance resolved to **0.9.2** (digest
  `bd36741e…` = tags `0.9.2`/`latest`, pushed 2026-07-15) — so `preserve_tags`
  is available today and the benchmark corruption is confirmed to come from the
  fit_markdown path (the ≥0.7.8 scraper fix is already in the image). Pin
  `unclecode/crawl4ai:0.9.2` in docker-compose.yml instead of `latest`.
- [ ] Upstream contribution (decided 2026-07-30: issue + PR combo, PR authored
  by the orchestrator directly): patch written and verified locally — +8-line
  guard in `PruningContentFilter._prune_tree` (skip `pre`/`code` subtrees,
  mirroring the #1181 scraper guard) + `tests/test_pruning_code_whitespace.py`
  (4 tests; 2 fail on unpatched code, all 24 filter tests pass patched, zero
  regressions in the #1900 preserve-whitelist suite). Per CONTRIBUTING.md the
  PR targets `develop` from a fork. FILED 2026-07-30 (account kinorai):
  issue https://github.com/unclecode/crawl4ai/issues/2110 + PR
  https://github.com/unclecode/crawl4ai/pull/2111 (branch
  `fix/pruning-preserve-code-whitespace`, rebased on develop, 24/24 green).
  Until merged and released, omnifeed still ships the `preserve_tags`
  payload workaround (next bullet). Draft kept at
  [crawl4ai-issue-draft.md](crawl4ai-issue-draft.md).
- [ ] `internal/engine/crawl4ai`: add to the content_filter params:
  `"preserve_tags":["pre","code"]` (+ `preserve_classes` for common highlighter
  wrappers: `highlight`, `chroma`, `highlighter-rouge`). Keep `table` OUT of
  preserve_tags (would re-admit chrome tables, see item 5).
- [ ] Belt-and-braces in Go: if `fit_markdown` has no fenced block but
  `raw_markdown` does, prefer `raw_markdown` (both arrive in the same response).
- [ ] Side-fix discovered: crawl4ai clamps untrusted `page_timeout` to 60000 ms;
  omnifeed sends 90000 — align the config/docs.
- Validate: golden-fixture tests (k8s.io Service YAML, MDN reduce, dev.to
  Dockerfile, crates.io serde README) asserting code blocks match source.

## 4. Dedicated GitHub engine, then Discourse — tradeoff (new code) · effort L

**Measured**: unauthenticated REST reconstructs a 317-comment issue in 2.31 s
sequential (~0.6 s parallel), complete, vs the benchmark's 10.3 s browser render
capturing 12/144; PR (6 REST calls incl. real diff via
`Accept: application/vnd.github.v3.diff`) ≈ 2.0 s vs 14.6 s without diff.
GraphQL rejected: needs auth, measured slower (3.68 s). Rate limits: anonymous
60 req/h/IP (tight: ~10 PR fetches/h), token 5000/h; don't budget on 304s being
free (measured: counted anonymously).

- [ ] `internal/engine/github` mirroring the HN engine skeleton (Config{Client,
  Limiter, APIBase, Token, Timeout, Logger}; `parseTarget` claims ONLY
  `/{owner}/{repo}/issues/{n}` and `/pull/{n}` — blob/tree/actions/releases/
  discussions fall through to crawl4ai). Issue = issue + paginated /comments
  (per_page=100, Link header); PR = pulls/{n} + issues/{n}/comments (conversation
  tab!) + pulls/{n}/comments + /reviews + /commits + /files (structured patches →
  better truncation control than the raw .diff). Prune comment JSON to
  {user.login, created_at, body} (measured −39%+). TOON output with
  `truncated_from` metadata; caps as `EngineOptions` (`GitHubMaxComments`,
  `GitHubIncludeDiff`, `GitHubMaxDiffBytes`).
- [ ] `OMNIFEED_GITHUB_TOKEN` optional (empty = anonymous works out of the box);
  on 403/429 with `x-ratelimit-remaining: 0` return `domain.FetchError` with
  `x-ratelimit-reset` in metadata — never silently fall back to the browser.
- [ ] Register between HN engine and fallback; add the `api.github.com` outbound
  note in `main.go` next to the HN exception; README env table + AGENTS gotchas.
- [ ] `internal/engine/discourse` (second): host allowlist via
  `OMNIFEED_DISCOURSE_HOSTS` (Matches is a pure predicate — can't probe; explicit
  list is the honest failure mode). Fetch `/t/<id>.json?print=true` (measured:
  84-post topic in 1.33 s complete); fall back to `post_stream.stream` +
  `posts.json?post_ids[]=` batches (40 ids ≈ 0.9 s). Follow the slug 301s.
  HTML→markdown the `cooked` bodies. Anonymous rate limits are invisible —
  back off on 429.
- Validate: httptest fixtures per endpoint; live spot-check G2/G3/D3 URLs
  comparing completeness + latency vs the benchmark numbers.
- Known gap (accepted): GitHub Discussions are GraphQL-only — stays on crawl4ai.

## 5. Chrome/noise pruning for the crawl4ai fallback — tradeoff · effort M

**Facts**: the `word_count_threshold:10` omnifeed sends is inert (crawl4ai
hardcodes 1 on that path); `excluded_tags` misses GitHub commit tables / pypi
release history / marketing templates, which score above the 0.48 pruning
threshold (high text density). `target_elements` + `excluded_selector` pass the
REST allowlist.

- [ ] Low-risk first: extend `excluded_selector`
  (`nav,footer,header,aside,.sidebar,.toc,.related,.newsletter,.cookie,…`),
  add `remove_consent_popups: true`, `script/style/noscript` to excluded_tags.
- [ ] Careful second: `target_elements` (`article, main, [role=main],
  .markdown-body, .post-content, #content`) — NOT lossless: pages matching none
  of the selectors risk turning bloat into empty (= item 6 territory). Ship only
  after per-URL verification on the benchmark set, or gate behind an env flag.
- [ ] Do NOT raise the pruning threshold (amplifies item 3).
- Validate: regression corpus = this run's 29 fetch URLs; SO3 (redswitches) is
  the canary — today it returns 51 KB with 0% article; success = article text
  present AND size materially down on G1/P2/D3. Watch the item-6 guard for new
  empties.

## 6. Silent empty-success fetch — lossless (correctness) · effort S

**Root cause is omnifeed's**: `engine.go` falls through
`fit_markdown → raw_markdown → cleaned_html` and returns `PageContent: ""` with
`nil` error; nothing checks it anywhere. crawl4ai has no "empty extraction" flag.

- [ ] After the fallback chain: `content == ""` → `&domain.FetchError{Kind:
  domain.KindThinContent, StatusCode: …, Err: fmt.Errorf("crawl4ai extracted 0
  chars (html=%d cleaned=%d raw_md=%d fit_md=%d)", …)}` — lands in the existing
  thin-content metric series instead of handing the LLM an empty document.
- [ ] Decode the extra response fields needed for the diagnostic counts.
- Validate: unit test with an all-empty fixture; live re-fetch of the W4
  bytebytego URL returns an explicit error, not 0 bytes.

## 7. Error transparency — lossless · effort S

- [ ] Propagate the classified reason into the MCP error text: E1 surfaced only
  `MCP error -32603: fetch_url failed` while metrics knew `upstream_error`.
  Include reason + upstream status in the JSON-RPC error message.
- [ ] (PDF support itself: declined — out of scope.)
- Validate: E1 URL re-fetch shows a reason-bearing error string; unit test on the
  error-mapping path.

## Suggested implementation order

1 (searxng detect + settings) → 6 (empty guard) → 7 (error text) → 3 (preserve_tags
+ image pin + raw-md fallback) → 2 (size control) → 5 (pruning, gated) → 4 (GitHub
engine, then Discourse). Items 1/6/7/3 are small and independently shippable; 2
touches the MCP transport surface; 5 needs the regression corpus; 4 is the big one.

Definition of done per repo conventions: `make check` green, new behavior tested
(httptest fixtures, no live calls), README env-var table updated, Conventional
Commits on `perf-improvements`, single MR.
