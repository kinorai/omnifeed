# omnifeed

> Self-hosted web search + LLM-friendly crawling, with a dedicated Reddit engine.
> One Go binary, three front-ends: **MCP server**, **Open WebUI loader**, and a plain **REST API**.

[![CI](https://github.com/kinorai/omnifeed/actions/workflows/ci.yml/badge.svg)](https://github.com/kinorai/omnifeed/actions/workflows/ci.yml)
[![Security](https://github.com/kinorai/omnifeed/actions/workflows/security.yml/badge.svg)](https://github.com/kinorai/omnifeed/actions/workflows/security.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

omnifeed gives an AI agent the full research loop — **search → URLs → content** — against self-hosted upstreams:

- **`web_search`** queries a [SearXNG](https://github.com/searxng/searxng) instance (Google/Bing/DDG, Reddit included) and returns ranked URLs with titles and snippets.
- **`fetch_url`** renders any URL through [crawl4ai](https://github.com/unclecode/crawl4ai) as clean markdown — and Reddit URLs come back as the **full comment tree** encoded in [TOON](https://github.com/toon-format/toon): typically **40% fewer tokens than JSON**, lossless.

Reddit blocks non-browser HTTP clients at its edge, so Reddit fetches (like every other URL) go through a crawl4ai headless browser — no Reddit auth or API key. Comment trees are fully expanded via `/api/morechildren`, with `[deleted]`/`[removed]` stubs stripped.

## Why omnifeed

| | omnifeed | Other Reddit MCPs |
|---|---|---|
| Web search → crawl in one self-hosted service | ✅ SearXNG + crawl4ai | ❌ search-only or crawl-only |
| Full comment tree (`/api/morechildren` expansion) | ✅ up to 40 rounds (~4k comments) | ❌ |
| Token-efficient output | ✅ TOON, ~40% smaller than JSON | ❌ verbose JSON or truncated bodies |
| Strips `[deleted]` / `[removed]` stubs | ✅ | ❌ |
| Generic crawl fallback for non-Reddit URLs | ✅ via crawl4ai | ❌ |
| Front-ends | MCP **+** Open WebUI **+** REST | MCP only (most) |

## Quick start

> **crawl4ai is required.** Every engine — Reddit and the generic fallback — fetches through it, so the binary won't start without `OMNIFEED_CRAWL4AI_URL`. The compose stack below wires one up for you.

### Full stack (compose)

```bash
git clone https://github.com/kinorai/omnifeed.git && cd omnifeed
docker compose up
```

Starts omnifeed + SearXNG + crawl4ai. Point Open WebUI at `http://localhost:8080` with `WEB_LOADER_ENGINE=external`. (SearXNG is mounted with `searxng/settings.yml`, which enables the `json` format the `web_search` tool needs.)

### As an MCP server (Claude Code, Cursor, Windsurf, …)

Stdio — most clients:

```jsonc
{
  "mcpServers": {
    "omnifeed": {
      "command": "docker",
      "args": ["run", "--rm", "-i", "kinorai/omnifeed:latest", "--mcp-stdio"]
    }
  }
}
```

HTTP — remote clients:

```jsonc
{ "mcpServers": { "omnifeed": { "url": "http://your-host:8081/mcp" } } }
```

Tools: **`fetch_url`** (always) and **`web_search`** (only when `OMNIFEED_SEARXNG_URL` is set). The intended loop is `web_search` → pick URLs → `fetch_url`.

### Without MCP (plain REST)

MCP is only one front-end — omnifeed is a normal HTTP/JSON service. Any client (curl, Python, n8n, a cron job feeding a vector DB) can call it directly:

```bash
# Crawl: URL → clean content
curl -X POST http://localhost:8080/crawl \
  -H 'Content-Type: application/json' \
  -d '{"urls":["https://www.reddit.com/r/LocalLLaMA/comments/.../"]}'

# Search: query → result URLs (when SearXNG is configured)
curl -X POST http://localhost:8080/search \
  -H 'Content-Type: application/json' \
  -d '{"query":"go generics","limit":10}'
```

`/crawl` returns `[{"page_content": "...", "metadata": {...}}]` — already the shape of a LangChain / LlamaIndex `Document`, so wrapping it as a custom document loader takes only a few lines.

### Authentication

The HTTP transports (`/crawl`, `/search`, `/mcp`) share one bearer token. Set **`OMNIFEED_API_KEY`** and send `Authorization: Bearer <token>`:

```bash
docker run -e OMNIFEED_API_KEY="$(openssl rand -hex 32)" \
  -e OMNIFEED_CRAWL4AI_URL=http://crawl4ai:11235/crawl \
  kinorai/omnifeed
```

Without a key the proxy **refuses to start**, so it can't be left open by accident. For a throwaway local run, opt out with **`OMNIFEED_DEV_NO_AUTH=true`** (the compose files already do). Stdio MCP needs no token — it inherits the trust of the process that spawned it.

## Configuration

Everything is configured with `OMNIFEED_`-prefixed environment variables. In practice you only ever **set three** — `OMNIFEED_API_KEY`, `OMNIFEED_CRAWL4AI_URL`, and (optionally) `OMNIFEED_SEARXNG_URL`. The rest have sane defaults.

| Variable | Default | Purpose |
|---|---|---|
| `OMNIFEED_API_KEY` | _(unset)_ | Bearer token for `/crawl`, `/search`, `/mcp`. If unset, the proxy refuses to start unless `OMNIFEED_DEV_NO_AUTH=true`. Stdio MCP is unaffected. |
| `OMNIFEED_CRAWL4AI_URL` | _(required)_ | Upstream crawl4ai endpoint. Every engine fetches through it; if empty, the proxy exits at startup. |
| `OMNIFEED_SEARXNG_URL` | _(unset)_ | Upstream SearXNG base URL (e.g. `http://searxng:8080`). When unset, `web_search` / `/search` are not exposed. The instance must enable the `json` format. |
| `OMNIFEED_DEV_NO_AUTH` | `false` | Run the HTTP transports with **no** auth when no key is set (local/dev only). Ignored if a key is set. |
| `OMNIFEED_LISTEN_ADDR` | `:8080` | HTTP listen address (`/crawl`, `/search`) |
| `OMNIFEED_MCP_LISTEN_ADDR` | `:8081` | MCP HTTP/SSE listen address |
| `OMNIFEED_MCP_STDIO` | `false` | Run MCP over stdio (also via `--mcp-stdio`) |
| `OMNIFEED_METRICS_ADDR` | `:9090` | Prometheus + health listen address |
| `OMNIFEED_CRAWL4AI_TIMEOUT` | `90s` | Per-call timeout to crawl4ai |
| `OMNIFEED_SEARXNG_TIMEOUT` | `15s` | Per-query timeout to SearXNG |
| `OMNIFEED_SEARCH_MAX_RESULTS` | `25` | Hard cap on the search `limit` argument (1–100) |
| `OMNIFEED_REDDIT_TIMEOUT` | `4m` | Wall-clock cap for a Reddit thread expansion |
| `OMNIFEED_REDDIT_MAX_ROUNDS` | `3` | Default `/api/morechildren` rounds (max 40 via `?expand=full`) |
| `OMNIFEED_REDDIT_FORMAT` | `toon` | Default Reddit output: `toon` or `json` |
| `OMNIFEED_MAX_URLS_PER_REQUEST` | `30` | Cap on `urls[]` length |
| `OMNIFEED_PER_DOMAIN_CONCURRENCY` | `2` | Max concurrent requests to one domain |
| `OMNIFEED_PER_DOMAIN_DELAY` | `1500ms` | Minimum delay between same-domain requests |
| `OMNIFEED_BLOCK_PRIVATE_IPS` | `true` | SSRF protection (keep on in production) |
| `OMNIFEED_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `OMNIFEED_LOG_FORMAT` | `json` | `json` or `text` |
| `OMNIFEED_ENABLE_PPROF` | `false` | Expose `/debug/pprof/*` (opt-in) |

## API

### `POST /crawl` — URL → content (Open WebUI loader contract)

```http
POST /crawl
Authorization: Bearer $OMNIFEED_API_KEY
Content-Type: application/json

{"urls": ["https://www.reddit.com/r/foo/comments/.../"]}
```

Response: `[{"page_content": "...", "metadata": {...}}, ...]`

Per-request query params (Reddit URLs only): `?format=toon|json`, `?expand=N|full` (0–40), `?depth=1` (include depth field), `?nocreated=1` (drop the created field, ~7% fewer tokens).

### `POST /search` — query → result URLs

Exposed only when `OMNIFEED_SEARXNG_URL` is set.

```http
POST /search
Authorization: Bearer $OMNIFEED_API_KEY
Content-Type: application/json

{"query": "go generics", "limit": 10, "time_range": "week", "language": "en"}
```

Response: `[{"title": "...", "url": "...", "snippet": "...", "engine": "...", "published_date": "..."}, ...]`. `limit` is clamped to `OMNIFEED_SEARCH_MAX_RESULTS`; `time_range` is one of `day|week|month|year`.

### Health & metrics

- `GET /livez` — liveness; 200 unless shutting down
- `GET /readyz` — checks crawl4ai (and SearXNG, when configured); `GET /healthz` is an alias
- `GET /metrics` — Prometheus (`omnifeed_requests_total`, `omnifeed_request_seconds`, `omnifeed_reddit_expansion_rounds`, `omnifeed_search_requests_total`, `omnifeed_search_request_seconds`)

### MCP

JSON-RPC 2.0 at:

- **stdio** when `OMNIFEED_MCP_STDIO=true` or `--mcp-stdio`
- **Streamable HTTP** at `/mcp` (spec 2025-03-26) on `OMNIFEED_MCP_LISTEN_ADDR` — `POST /mcp` for one-shot calls, `GET /mcp` for the SSE stream
- `GET /mcp/sse` is a legacy alias for older dual-endpoint SSE clients; new clients target `/mcp`

## Architecture

Two ports answer two questions. The `Searcher` port answers *query → URLs*; the `Engine` port answers *URL → content*. MCP tools and REST handlers compose them; transports stay thin.

```
   /crawl   ──► OpenWebUI transport ──────────────┐
   /search  ──► SearchAPI transport ──► Searcher  │
                                                   ▼
   MCP stdio ──► ┌──────────────┐  crawl tools  ┌────────────────┐
   MCP HTTP  ──► │  MCP server  │ ────────────► │     Engine     │
                 │ (transport)  │               │    Registry    │
                 └──────┬───────┘               └───┬────────┬───┘
                        │ search tool               ▼        ▼
                        ▼                    ┌────────┐ ┌────────────┐
                 ┌──────────────┐            │ reddit │ │  generic   │
                 │   Searcher   │            │ engine │ │  fallback  │
                 │  (searxng)   │            │ (TOON) │ │ (markdown) │
                 └──────┬───────┘            └───┬────┘ └─────┬──────┘
                        ▼                        └──────┬─────┘
              ┌──────────────────┐                      ▼
              │ SearXNG upstream │           ┌───────────────────────┐
              │  (google/bing/   │           │    crawl4ai upstream   │
              │       ddg)       │           │   (headless browser)   │
              └──────────────────┘           └───────────────────────┘
```

### Reddit anti-bot handling

Reddit's edge 403-blocks non-browser HTTP clients (it fingerprints the TLS/JA3 handshake), so the Reddit engine never calls Reddit directly. It drives crawl4ai's headless Chromium to a `www.reddit.com` page (which clears the bot wall), then runs a **same-origin `fetch()`** of the `.json` and `/api/morechildren` endpoints from inside that page. No Reddit auth, cookies, or API key. A per-thread crawl4ai `session_id` is reused across expansion rounds to keep one warmed context.

> Sustained scraping can raise your source IP's risk score. If fetches start returning the block page, slow down, keep `expand` modest, or route crawl4ai through a residential proxy.

### Extending it

- **New engine** (HN, Stack Overflow, …): implement `domain.Engine`, register before the fallback.
- **New searcher** (Brave, Tavily, …): implement `domain.Searcher`.
- **New MCP tool**: add a constructor in `internal/transport/mcp/tools` — the MCP server never changes.
- **New transport**: build on `engine.Registry` / `domain.Searcher`.
- **Output encoding** (e.g. TOON): `internal/domain/document.go`.

## Development

```bash
git clone https://github.com/kinorai/omnifeed.git && cd omnifeed
make check        # vet + lint + test (what CI runs)
go run ./cmd/omnifeed
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full workflow, [SECURITY.md](SECURITY.md) for vulnerability reporting, and [AGENTS.md](AGENTS.md) if you're a coding agent working in this repo.

## License

[MIT](LICENSE) © kinorai
