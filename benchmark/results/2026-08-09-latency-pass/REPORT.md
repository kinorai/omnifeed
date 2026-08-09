# 2026-08-09 latency pass — full engineering record

The complete record of the generic-crawl latency/quality pass: baseline audit →
instrumentation → knob decisions → implementation → three benchmarks → two root-caused
bugs (one config-shipped, one upstream regression) → code-review hardening → deploy →
verification. Raw data in [`raw/`](raw/); local content snapshots (per-URL page text for
every benchmark run) live outside the repo at `~/projects/omnifeed-bench/`.

Commits on `feat/latency-metrics` covering this pass:

| Commit | What |
|---|---|
| `d287861` | upstream/limiter/response-chars metrics |
| `ecfb63d` | per-request completion-exemplar INFO logs |
| `11f8c52` | first code-review hardening of the metrics |
| `6c118ad` | the latency pass itself (this report §4) |
| `0538b2d` | overlay fix + per-request scan + scrubbed-500 handling (§7–§9) |
| `f46bbac` | second code-review fixes (§10) |

---

## 1. Baseline and audit (2026-08-08)

7-day production metrics (omnifeed.valiere.uk, k3s NUC+Pi cluster):

- crawl4ai engine: **p50 9.4s / p90 14s / p99 81s**; reddit p50 2.1s; search p50 0.7s;
  nginx/tunnel overhead 3–5ms; no resource pressure.
- Browser FETCH phase = **98.6%** of crawl time (SCRAPE avg 0.096s).
- raw.githubusercontent.com text files took **30–39s** in Chromium (page-idle wait
  never settles on non-HTML).

Myth-busts confirmed from crawl4ai 0.9.2 source:

- `crawler.rate_limiter.base_delay` is a **no-op** for single-URL `/crawl` (fresh
  dispatcher per request; not even used for 1 URL).
- `app.workers` and `crawler.timeouts.*` in config.yml are **dead config** — the image's
  supervisord hardcodes gunicorn `--workers 1 --keep-alive 300`.
- crawl4ai's `max_retries` default is 0; the observed 3-attempt burn came from omnifeed
  sending `max_retries: 2`.
- `/execute_js` has no session support in 0.9.x — every Reddit round-trip is a full
  navigation on a warm pooled browser (structural, not configurable).

Identified sinks, all client-sent from `engine/crawl4ai/engine.go` (then hardcoded):
`delay_before_return_html: 1.0` (flat +1s; upstream default 0.1), `scan_full_page: true`
+ `scroll_delay: 0.5` (multi-second scroll), `max_retries: 2`; plus Go DefaultTransport's
`MaxIdleConnsPerHost=2`, sequential GitHub PR API calls, duplicate SSRF DNS lookup on the
`/crawl` path.

## 2. Decisions (R1–R13, settled one-by-one)

| # | Recommendation | Decision |
|---|---|---|
| R1 | `scan_full_page` | env `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE`, OSS default false — initially "infra keeps true", **reversed by §6's evidence** to per-request opt-in |
| R2 | `delay_before_return_html` 1.0→0.1 | env, 0.1 everywhere, gated on a before/after quality benchmark (§5) |
| R3 | `max_retries` 2→0 | apply (later rebalanced, §10) |
| R4 | shared `http.Transport`, MaxIdleConnsPerHost 10 | apply |
| R5 | raw-text bypass of Chromium | apply, content-type sniff gated on path extension |
| R6 | parallel GitHub PR fetch (5 calls → concurrent) | apply |
| R7 | cache/singleflight | **skip** (near-zero hit rate at 30–60 crawls/day) |
| R8 | dedupe double SSRF DNS lookup | apply (loader pre-check → syntax-only; resolving check once at the registry) |
| R9 | SearXNG `request_timeout` 6.0→3.5 | apply (deployment) |
| R10 | SearXNG `keepalive_expiry` 60 | apply (deployment) |
| R11 | disable startpage engine | apply (deployment) |
| R12 | `GRANIAN_BLOCKING_THREADS` | 12 (deployment) |
| R13 | disable wikidata engine | apply (deployment) |

Confirmed before deciding: these knobs only reach the **generic crawl4ai engine** —
Reddit rides `/execute_js` (schema `{url, scripts}`, cannot receive them) and
GitHub/HN/Discourse call their JSON APIs directly.

## 3. Instrumentation shipped first (measure-before-optimize)

- `omnifeed_upstream_seconds{upstream,op,status}` — per-attempt upstream round-trip,
  body-read inclusive.
- `omnifeed_domain_limiter_wait_seconds{engine,outcome}` incl. `canceled` queue-deaths.
- `omnifeed_response_chars{engine}` — successful-crawl content length, **pre-truncation**
  (the quality yardstick this whole pass leans on).
- `omnifeed_request_attempts_total{upstream,attempt}` — retry volume.
- Completion-exemplar INFO logs (`crawl completed` w/ url+duration_ms+chars on all three
  transports) — the URL-level join metrics can't carry.
- crawl4ai's own `/metrics` scraped (bearer-token ServiceMonitor — its AuthGateMiddleware
  covers everything but `/health` and `/token`).
- Recording rules: `omnifeed:{request,search,upstream}_seconds:p{50,90,99}_24h`
  (status="ok"-filtered so failure bursts can't shift the yardstick).

## 4. Implementation (`6c118ad`)

- `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE` (false) / `OMNIFEED_CRAWL4AI_SCROLL_DELAY` (0.5) /
  `OMNIFEED_CRAWL4AI_DELAY_BEFORE_HTML` (0.1); scan keys omitted from the payload when off.
- `max_retries` removed from the payload (crawl4ai default 0).
- `httpx.NewTransport()` — DefaultTransport clone, `MaxIdleConnsPerHost: 10`, shared by the
  crawl and searxng clients.
- **Raw-text bypass** (`engine/crawl4ai/rawtext.go`): URLs with raw-looking path extensions
  (~60-entry allowlist: .md/.txt/.json/source files/…) fetched directly; the response
  Content-Type decides (non-HTML text → returned as-is, ≤10MiB, UTF-8-validated; anything
  else → browser path, silently). Direct fetches use `httpx.NewGuardedClient`: a
  `net.Dialer.Control` hook refuses private/reserved IPs **post-DNS-resolution**, closing
  the DNS-rebinding TOCTOU that validate-then-fetch leaves open. Deployment egress note:
  rides the existing 443-only netpol rule; http:// raw files just fall back to crawl4ai.
- GitHub `crawlPull`: the five REST reads (PR, conversation comments w/ pagination, inline
  comments, reviews, files) run concurrently via a local `runAll` helper (~5×RTT → 1×RTT).
- Open WebUI loader pre-check made syntax-only (no DNS); the resolving private-IP check
  runs once at `Registry.Crawl`. Behavior delta: a private-IP URL is now a per-URL error
  document, not a whole-batch 400.

## 5. Benchmark 1 — production before/after (17 URLs)

Method: `POST https://omnifeed.valiere.uk/mcp` `tools/call fetch_url`, curl-timed,
sequential; 15 crawl4ai URLs + 2 controls (HN item → hackernews engine, reddit thread →
reddit engine). **Before** = run 1 only against `dev-11f8c52` (old knobs) at 10:03–10:08Z —
the second run was pre-empted by the deploy at 10:22Z, so the baseline is single-run
(it matches the 7d metric p50 9.31s, so trusted). **After** = 2 runs against `6c118ad`
with `scan_full_page` still ON (deployment choice at the time — this benchmark isolates
the delay/retry/bypass changes). Full table: [`raw/prod-comparison.md`](raw/prod-comparison.md).

| | before | after (run 2) |
|---|---|---|
| median (idx 1–15) | 9.09s | **8.03s** |
| mean (idx 1–15) | 15.08s | **8.01s** |

- raw go README: **39.00s → 0.06s** (bypass; content equivalent, minus a stray code fence
  crawl4ai used to inject).
- vercel.com/docs: fails on both configs (hard crawl4ai content-gate), **82.8s → 26s**.
- HTML pages: −1.1s median (the 1.0→0.1 delay), **12/15 byte-identical content** — the
  delay cut lost nothing.
- Controls: HN byte-identical (0.55→0.49s); reddit ±live-thread churn only.
- Anomalies logged for follow-up: Wikipedia returned **24 chars on BOTH configs** (→ §8);
  StackOverflow served 120k before but hit a Cloudflare JS challenge on both after-runs
  (→ §9 sidebar).

## 6. Benchmarks 2+3 — `scan_full_page` on/off

**Local method** (both benches): repo docker-compose stack (searxng + crawl4ai 0.9.2 +
omnifeed built from the tree), loader `POST /crawl`, full `down -v` between passes,
env verified in the running container. Absolute times are NOT comparable to prod
(localhost, no tunnel, shared loaded host) — only the on/off delta counts.

**Bench 2 — 16 URLs × 2 runs, on vs off** ([`raw/scan-16url-comparison.md`](raw/scan-16url-comparison.md)):

- off ≈ **3.1× faster**: mean 8.11→2.62s, median 7.98→2.34s — a near-uniform ~5.6s
  penalty per crawl4ai page regardless of size.
- Content: **14/16 byte-identical**; both diffs favor OFF —
  - react.dev: scan **mangles** its virtualized code samples (``` fences and JSX brackets
    lost);
  - theverge: scan's only delta is removing 5× cookie-consent boilerplate.
- The two lazy-load pages added to defend the scan (theverge, dev.to/t/programming)
  gained **zero real content** from it.

**Bench 3 — 23 lazy-load-risk sites × 3 passes (on1/off/on2, churn-controlled)**
([`raw/scan-23site-comparison.md`](raw/scan-23site-comparison.md)). The on2 pass exists so
live feed churn can't masquerade as a scan effect. Verdicts over the 16 sites with real
content:

| verdict | count | detail |
|---|---|---|
| NO-DIFF | 14 | news indexes, e-commerce grids, virtualized/JS docs, SPAs, huge-static + link-density controls |
| SCAN-WINS | 1 | **dev.to feed**: 127→159/194 unique article permalinks; off's link set is a strict subset of both ons; 32 permalinks in both ons and neither in off. +25% links for +6.1s |
| OFF-WINS | 1 | github blob code view (the a-priori best case!): off is a strict superset; neither pass got real code (GitHub's own error banner) |
| CHURN | 3 | medium, tumblr (suggestive of scan-wins but on1 returned a different page variant), ebay |
| latency | | off ~2.2× faster (crawl4ai subset mean 8.4→3.9s) |

**Field research** (crawl4ai GitHub + docs + code search + reddit + comparables):

- Upstream default **false**; absent from README/quickstart; documented in
  *advanced/lazy-loading.md*, scoped to **images/`result.media`**, not text.
- Our react.dev corruption is **unclecode/crawl4ai#731** (open since 2025-02; on
  DOM-recycling/virtualized pages the scan keeps only the LAST visible batch — net
  content loss vs not scrolling). Two community fix PRs (#1853, #1868) unmerged. Docs
  route that case to `virtual_scroll_config`, which needs a per-site `container_selector`
  a gateway can't supply.
- Failure mode is silent: scan hard-caps at 10 scroll steps (docstring says unlimited),
  times out into "continuing with partial scroll", runs BEFORE `wait_for`/
  `delay_before_return_html`.
- Every "scan fixed my lazy content" report is site-tuned (`scroll_delay` 2–4s + custom
  `wait_for`); at the default speed you pay the latency and still outrun the loading.
- GitHub code search (100 files/76 repos): site-specific scrapers set it true; the
  agent/MCP/RAG gateways closest to omnifeed converge on a **per-call boolean defaulting
  false**. Firecrawl doesn't scroll by default either (explicit per-request action).

**Decision**: OSS default false; **per-request tri-state opt-in** — `fetch_url` gains a
`scan_full_page` boolean argument, the loader accepts `?scan_full_page=true|false`
(false can force the scroll off where a deployment enables it globally). Deployment rides
the false default. Verified live: dev.to 58 article links default → **171** with the flag.

## 7. Bug 1 — `remove_overlay_elements` silently guts entire sites

Symptom (pre-existing, surfaced by benchmark 1): en.wikipedia.org/wiki/Kubernetes returned
**24 chars** — the `<title>` line — as an HTTP-200 *success*. Benchmark 3 showed the same
signature on theguardian.com/technology (27), arstechnica.com (82), bsky.app (31).

Investigation (research agent, then local A/B):

- Not a known upstream issue (zero hits on GitHub/web/reddit); no newer crawl4ai exists
  (0.9.2 is head of main).
- Both initial hypotheses **killed with evidence**: the PruningContentFilter's own math
  can't strip a Wikipedia-sized container (its `0.1·ln(text_len)` term alone ≈1.15 vs
  threshold 0.48, and pruning can't touch `raw_markdown` anyway, which was also empty);
  and the measured ancestor chain of `#mw-content-text` matches none of our
  excluded_tags/excluded_selector entries.
- The tell: crawl4ai's SCRAPE phase took **0.01s on a 597KB page** (vs 0.1–1.6s on much
  smaller pages) — the DOM was already empty before extraction. The only mechanism
  upstream of the scraper is `remove_overlay_elements`' in-browser JS, whose geometry rule
  deletes ANY visible element that is `position:absolute|fixed` or `z-index>999` AND
  bigger than half the viewport — no allowlist. Wikipedia Vector-2022's full-height
  column containers sit exactly in the kill zone; `<title>` survives in `<head>`.

A/B proof (standalone crawl4ai 0.9.2, our exact crawler_config, one boolean toggled):

| config | fit_markdown | raw_markdown | cleaned_html |
|---|---:|---:|---:|
| `remove_overlay_elements: true` (shipped until now) | **23** | 1 | 687 |
| overlay off, `remove_consent_popups` on | **96,316** | 135,296 | 264,489 |
| both off | 96,316 | 135,296 | 264,489 |

→ `remove_consent_popups` is innocent (stays on). Fix (`0538b2d`):
`OMNIFEED_CRAWL4AI_REMOVE_OVERLAYS`, **default false**. Also fixed nearby: the
fit→raw→cleaned fallback now treats whitespace-only fields as empty (`== ""` would let a
`"\n"` fit_markdown win the pick).

Adjacent upstream footguns found (not biting us): #2125 (preserve_tags/classes silently
ignored for the filter's hard-coded excluded tags), #2121 (server-side `base_config`
booleans inert — client `crawler_config` is the only lever).

## 8. Bug 2 — crawl4ai 0.9.2 scrubs its 500 bodies (upstream regression for us)

Empirically captured: `/crawl` on a content-gated page (PDF) returns HTTP 500 with body
`{"error":"Internal server error","correlation_id":"…"}` — the verdict ("Blocked by
anti-bot protection: …") goes only to crawl4ai's **log**. Deliberate 0.9.2 hardening
(server.py routes every application 500 through a scrubbing handler; 502/503/504 "pass
through" as operational statuses). Consequences for omnifeed since the 0.9.2 upgrade:

- the body-marker demotion path (`antibot.IsBlockResponse` → bot_block/thin_content) was
  **dead** for hard 500s → every block/content-gate classified `upstream_error` (which
  pages) — the OmnifeedCrawlErrors alert noise observed during the benchmarks;
- `RetryableStatus` never vetoed → 3 full client-side re-crawls per deterministic block
  (the 26s vercel failures; StackOverflow triple-navigations).

Fix (`0538b2d`, rebalanced in `f46bbac`): scrubbed-500 shape recognized
(`antibot.IsScrubbedServerError`) → new reason **`upstream_rejected`** (excluded from
outage semantics); both crawl4ai call sites cap `MaxAttempts: 2` — one retry rescues the
transient faults that share the scrubbed channel (worker OOM, pool churn: body-identical
to deterministic verdicts), while a deterministic page costs at most 2× one crawl (down
from 3×).

### Sidebar: the StackOverflow block

SO served 120k chars in the before-pass, then Cloudflare-JS-challenged every later
attempt — including after the operator's WAN IP changed, so the block is **not IP-keyed**
(fingerprint/ASN-level; echoes the Lightpanda-Reddit lesson). One observed 403 carried a
1.2MB HTML body — plausibly the full page served with a 403 (soft block). Left open:
re-test after a day's decay.

## 9. Deployment changes (infra repo, applied 2026-08-09)

- omnifeed module: image `dev-f46bbac`; scan/overlay/delay/retries all ride OSS defaults;
  `GRANIAN_BLOCKING_THREADS=12` on searxng.
- searxng settings: `request_timeout 3.5`, `keepalive_expiry 60`, startpage + wikidata
  disabled (braveapi covers; startpage was the slowest + 1h-CAPTCHA repeat offender).
- victoria-stack: recording rules `omnifeed:response_chars:{avg,p10,p50}_24h` (the
  long-horizon quality yardstick — avg for trend, p10 for the gutted-page floor) and
  alert **OmnifeedThinSuccessRatio** (warning: >30% of an engine's successes ≤100 chars
  over 6h, ≥5 crawls — the rule that would have caught §7 within a day of shipping).

## 10. Code-review hardening (`f46bbac`, /code-review of the branch — 7 findings, all applied)

1. **response_chars mislabeling**: transports resolved the engine from the URL, so a
   fallback crawl attributed crawl4ai's output to the engine that FAILED. Moved the
   observation into `Registry.Crawl` under the engine that actually produced the document.
2. **Retry regression**: the interim blanket never-retry-500 stranded genuine transients
   (no retry anywhere once `max_retries: 2` was also gone). Reverted to marker-only vetoes
   + `MaxAttempts: 2` at both call sites (see §8).
3. **Raw-text bypass**: HEAD probe removed (duplicated the GET's own content-type check,
   one wasted RTT, forfeited the bypass on HEAD-hostile hosts); mismatched/oversized
   bodies closed unread instead of drained without bound (1GiB-logfile scenario).
4. **GitHub fan-out vs limiter**: the 5-concurrent-reads-in-one-slot design is deliberate
   (API quota semantics, not scrape politeness) — now documented at the Acquire site.
5. gofmt pass (3 files). 6. Duplicate registry fallback-counter test deleted.
7. `ObserveResponseChars` godoc corrected to pre-truncation.

## 11. Verification

Local smoke (exact `f46bbac` binary, compose stack): Wikipedia 96,316 chars; raw README
0.19s; PDF → `upstream_rejected`; dev.to 58→171 links with the opt-in. Post-deploy (live
cluster): both replicas on `dev-f46bbac`; Wikipedia **96,316** and Guardian **8,177**
chars via the public MCP endpoint; PDF fails as `upstream_rejected` with the
correlation-id message, 2 attempts; `omnifeed:response_chars:*` rules and
`OmnifeedThinSuccessRatio` loaded in vmalert; `up{job="omnifeed-crawl4ai"}==1` (the
cluster's first bearer-token scrape).

## 12. Open follow-ups

- Re-test StackOverflow after the challenge decays (not IP-keyed; consider a
  challenge-settle or retry-on-bot-block only if it persists on fresh URLs).
- Watch upstream crawl4ai #731 / PRs #1853/#1868 (virtual-scroll fix) and whether a
  future release re-exposes 500 verdicts (then `upstream_rejected` can re-split into
  bot_block/thin_content).
- Agents must learn the `scan_full_page: true` opt-in for feed URLs that come back thin
  (tool description teaches it; watch usage).
- Considered and not built (deliberate): response cache/singleflight (R7 — no traffic to
  justify it), Gatus content canary + links-in-logs + bypass-hit-rate rule (offered,
  not picked — revisit if thin-success alerting proves insufficient).
- Release: this branch is local-only; PR → `0.18.0` retires the `dev-f46bbac` tag.
