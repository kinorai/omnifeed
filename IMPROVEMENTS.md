# Improvement Ideas & References

> Living backlog for evolving this project from a **crawl-only** proxy into a
> **search + crawl** MCP. Captures the accepted direction, deferred ideas,
> rejected options, and the reference projects (with GitHub links) surfaced
> during planning. Last updated: 2026-06-09.

---

## Decisions locked

- **Web search = SearXNG only.** Add a general web-search capability backed by a
  self-hosted SearXNG. SearXNG already surfaces `reddit.com` results via its
  general engines (Google/Bing/DDG), so **no separate reddit-search backend is
  added for now.**
- **Search and crawl stay separate tools.** No fused `search_and_crawl` (see
  _Considered & rejected_).
- **Search is a new architectural port** (`domain.Searcher`), not a fake crawl
  `Engine` — search is *query → list of URLs*, crawl is *URL → content*. Reddit
  comment reading stays on the existing browser/crawl4ai + TOON engine.

## Decisions resolved (2026-06-10)

1. **New name** — `search-crawl-reddit-proxy`, env prefix `SCRM_`, metrics
   `scrm_*`. ✅ shipped.
2. **Rebrand depth** — **full** (module path, binary, image, env, metrics,
   Helm release, Terraform dir, ingress host). ✅ shipped.
3. **SearXNG deploy** — app-template controller, in-cluster. ✅ shipped.

---

## Shipped (2026-06-10)

### A. Web search via SearXNG — `Searcher` port + `search` MCP tool ✅
- `domain.Searcher` port + `internal/search/searxng` adapter (JSON API,
  requires `search.formats: [json]` in settings.yml). Tested.
- `search` MCP tool exposed only when `SCRM_SEARXNG_URL` is set; `crawl`
  untouched. Pipeline: `search → URLs → crawl`.

### B. Full rebrand ✅
- Everything renamed; broken `docker-compose.standalone.yml` removed
  (it predated the hard `CRAWL4AI_URL` requirement and could no longer start).

### C. SearXNG in-cluster ✅
- App-template controller in the same Helm release (official chart is
  archived). URL stays configurable via `SCRM_SEARXNG_URL`.

### D. Clean-architecture pass ✅ (partial)
- MCP server is now a **pure JSON-RPC transport**: tools injected as
  `[]mcp.Tool`; definitions live in `internal/transport/mcp/tools`. The MCP
  package no longer imports engine/reddit.
- MCP crawl/search calls now emit metrics (tenant="mcp") — previously only
  the Open WebUI path was measured.
- Still open: `domain.EngineOptions` carries Reddit-specific fields and
  `transport/openwebui` still imports `reddit.Options` (its query-param
  contract is reddit-flavored). Generalize when a second engine needs options.
- Kept `internal/engine/crawl4ai` package name: vendor-named adapters are
  idiomatic; "generic" hides what it talks to.

---

## Backlog / deferred ideas

### Reddit RSS search (native discovery) — DEFERRED
Reddit-native search via `https://www.reddit.com/search.rss?q=...`
(+ `/r/<sub>/search.rss?...&restrict_sr=1`). Returns permalinks; lightweight, no
creds, no browser. **Deferred** because SearXNG covers reddit discovery
(secondhand, via Google/Bing) for now. **Tradeoff:** SearXNG misses very fresh /
long-tail reddit threads that Google hasn't indexed yet — revisit if
reddit-native freshness becomes a need.
Ref: <https://github.com/ninjackster/reddit-rss-mcp>

### Reddit RSS comment reading — FLAWED, degraded-fallback only
`get_post_comments` via `<permalink>.rss`. **Flaws:** flat (no parent/child
threading), ≤25 comments, no scores, 500-char excerpts. **Not** a replacement
for the browser/TOON deep reader. Only worth it as a *degraded fallback* when the
primary browser path truly fails (crawl4ai down / Reddit hard-block) — clearly
labelled `degraded` in metadata, and **never** triggered just because RSS
returns 200 (it almost always does, with shallow data). Low priority.
Ref: <https://github.com/ninjackster/reddit-rss-mcp> (single `server.js` maps
tools → RSS endpoints; good port reference if ever needed — reimplement Atom
parse with Go `encoding/xml`, not regex).

### Reddit-native search alternatives (if ever wanted)
- <https://github.com/karanb192/reddit-mcp-buddy> — best-maintained reddit search (no key)
- <https://github.com/Arindam200/reddit-mcp> / <https://github.com/hamzashahbaz/reddit-mcp> — official Reddit OAuth API (reliable, ToS-clean)
- <https://github.com/king-of-the-grackles/reddit-research-mcp> — semantic search, hosted, citations

### Clean-architecture cleanups (Uncle-Bob pass)
- **Decouple transports from Reddit options.** `reddit.Options` leaks into
  `transport/mcp` and `transport/openwebui`, and `domain.EngineOptions` carries
  Reddit-specific fields. Generalize to per-engine options so adding engines
  doesn't touch transports.
- **Rename `internal/engine/crawl4ai` → `internal/engine/generic`** — it's the
  generic fallback; the name should reflect intent, not the upstream vendor.

### Egress / NetworkPolicy
- `main` currently has **no internet egress** (only the `crawl4ai` controller
  reaches the internet). The SearXNG call from `main` is in-cluster (fine).
  **SearXNG itself** needs internet egress (like crawl4ai) — add a netpol.
  If reddit RSS is ever added, route it via crawl4ai or grant `main` a narrow
  `reddit.com:443` egress (a deliberate, scoped reopening — document it).

---

## Considered & rejected

### Fused `search_and_crawl` tool — REJECTED
A single tool that searches then auto-crawls top-N server-side. Rejected:
search→crawl is data-dependent (sequential), so merging adds **no parallelism** —
it only saves one agent round-trip, at the cost of losing model triage (which
results to crawl) and risking 100k-token result dumps. Keep `search` and `crawl`
as separate tools.

---

## Reference projects (GitHub)

Maintenance as of 2026-06: 🟢 active · 🟡 slowing · 🔴 dormant · ⛔ archived.

### SearXNG + crawl MCPs (closest prior art)
| Repo | Notes |
|---|---|
| 🟢 <https://github.com/ihor-sokoliuk/mcp-searxng> | Canonical SearXNG MCP — best reference for the search adapter (855★) |
| 🟢 <https://github.com/SecretiveShell/MCP-searxng> | Agent→SearXNG bridge |
| 🟢 <https://github.com/OvertliDS/mcp-searxng-enhanced> | Category-aware search + scraping |
| 🟢 <https://github.com/pwilkin/mcp-searxng-public> | Queries public instances, HTML→JSON |
| 🔴 <https://github.com/luxiaolei/searxng-crawl4ai-mcp> | **Exact same stack (SearXNG + Crawl4AI)** — dormant; read for design ideas |
| 🔴 <https://github.com/ToKiDoO/crawl4ai-rag-mcp> | Crawl4AI + SearXNG + Supabase RAG — dormant; reference only |

### SearXNG Helm charts
- ⛔ <https://github.com/searxng/searxng-helm-chart> — official, **archived** (~v1.0.1, dead)
- 🟢 <https://artifacthub.io/packages/helm/kubitodev/searxng> — maintained community chart (v1.1.1)

### Reddit MCPs
| Repo | Notes |
|---|---|
| 🟢 <https://github.com/ninjackster/reddit-rss-mcp> | RSS-based (the one analyzed above) — search/browse/flat-comments, no creds |
| 🟢 <https://github.com/karanb192/reddit-mcp-buddy> | Best-maintained reddit *search* (695★) |
| 🟡 <https://github.com/Arindam200/reddit-mcp> | PRAW / official API |
| 🟢 <https://github.com/king-of-the-grackles/reddit-research-mcp> | Semantic search, hosted |
| 🟢 <https://github.com/jordanburke/reddit-mcp-server> | Fetch + create content |
| 🟢 <https://github.com/Panniantong/Agent-Reach> | Multi-platform (reddit/twitter/yt/gh…), 21k★ |
| 🔴 <https://github.com/Hawstein/mcp-server-reddit> | Browse only, no keyword search |
| 🟡 <https://github.com/eliasbiondo/reddit-mcp-server> | Zero-config search |
| 🔴 <https://github.com/adhikasp/mcp-reddit> | Fetch/analyze |

### General web-search MCPs / APIs
| Repo | Notes |
|---|---|
| 🟢 <https://github.com/firecrawl/firecrawl-mcp-server> | Scrape + search (hosted/paid or self-host) |
| 🟢 <https://github.com/exa-labs/exa-mcp-server> | Exa search + crawl — **excludes reddit** |
| 🟢 <https://github.com/tavily-ai/tavily-mcp> | Built on Brave index → includes reddit |
| 🟢 <https://github.com/brave/brave-search-mcp-server> | Brave's own index → includes reddit |
| 🟢 <https://github.com/perplexityai/modelcontextprotocol> | Perplexity/Sonar — synthesis, not raw URLs |
| 🟢 <https://github.com/brightdata/brightdata-mcp> | SERP + scrape, bypasses blocks; commercial |
| 🟢 <https://github.com/kagisearch/kagimcp> | Official Kagi |
| 🟢 <https://github.com/nickclyde/duckduckgo-mcp-server> | DuckDuckGo, no key |
| 🔴 <https://github.com/marcopesani/mcp-server-serper> | Serper/Google SERP |
| 🟢 <https://github.com/Aas-ee/open-webSearch> | Multi-engine, no keys |
| 🟢 <https://github.com/yokingma/one-search-mcp> | Multi-backend (SearXNG/Tavily/DDG/Bing) |
| 🟢 <https://github.com/damionrashford/RivalSearchMCP> | 5-engine + social search, no keys |
| 🟢 <https://github.com/pinkpixel-dev/web-scout-mcp> | DDG search + extraction |

### Crawl / scrape (also name inspiration)
| Repo | Notes |
|---|---|
| 🟢 <https://github.com/0xMassi/webclaw> | Rust local-first scrape+crawl+extract (1.3k★) — name collision risk |
| 🟢 <https://github.com/us/crw> | Rust Firecrawl-alt: scrape+crawl+search API+MCP |
| 🟢 <https://github.com/getmaxun/maxun> | No-code scrape/crawl/search platform |
| 🟢 <https://github.com/D4Vinci/Scrapling> | Adaptive scraping framework (library) |
| 🟢 <https://github.com/olostep/olostep-mcp-server> | Commercial scraping infra MCP |

---

## Name candidates (rebrand)
- `search-crawl-proxy` — descriptive, matches house style
- `anansi` — spider myth; brandable, distinctive, clean identifiers
- `maw` / `quarry` — short brandable, no hard collisions
- `webclaw` / `scout` — ⚠️ collisions (`0xMassi/webclaw` 1.3k★; `cortex-scout`, `web-scout-mcp`)

## Key findings / context
- Reddit **403-blocks the anonymous JSON API**; RSS feeds still return 200. This
  repo's reddit engine bypasses the block via crawl4ai's real browser.
- **Exa and Anthropic WebSearch do NOT return `reddit.com` URLs** (blocked).
  SearXNG surfaces reddit via its **general** engines (Google/Bing/DDG); its
  native "reddit" engine hits the blocked JSON API and is unreliable.
- The two closest prior-art projects to this exact stack (SearXNG + Crawl4AI) are
  **dormant** — good reference reading, not dependencies.
