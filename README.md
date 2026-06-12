# omnifeed

> **Self-hosted web search (SearXNG) + LLM-friendly crawling with a dedicated Reddit engine — MCP server, Open WebUI compatible**

[![CI](https://github.com/kinorai/omnifeed/actions/workflows/ci.yml/badge.svg)](https://github.com/kinorai/omnifeed/actions/workflows/ci.yml)
[![Security](https://github.com/kinorai/omnifeed/actions/workflows/security.yml/badge.svg)](https://github.com/kinorai/omnifeed/actions/workflows/security.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A single Go binary that gives an LLM agent the full research loop — **search → URLs → content** — against self-hosted upstreams:

- **`web_search`** queries a [SearXNG](https://github.com/searxng/searxng) instance (Google/Bing/DDG and friends, reddit.com included) and returns result URLs with titles and snippets.
- **`fetch_url`** renders any URL through [crawl4ai](https://github.com/unclecode/crawl4ai) as filtered markdown — with a **dedicated Reddit engine** that returns full comment trees encoded as **[TOON](https://github.com/toon-format/toon)**, typically **40% fewer tokens than JSON**, lossless.

Implements both an **MCP server** (stdio + Streamable HTTP/SSE) and the **Open WebUI external-loader contract**.

Most Reddit MCP servers either ship pretty-printed JSON or "save tokens" by truncating comments. This one does neither: full `/api/morechildren` expansion, TOON encoding, deleted-comment stripping. Reddit now blocks non-browser HTTP clients at its edge, so Reddit fetches are routed through a [crawl4ai](https://github.com/unclecode/crawl4ai) headless browser — no auth or API key required.

## Why this exists

| | omnifeed | Other Reddit MCPs |
|---|---|---|
| Web search → crawl in one self-hosted MCP | ✅ SearXNG + crawl4ai | ❌ search-only or crawl-only |
| Full comment tree (`/api/morechildren` expansion) | ✅ up to 40 rounds (~4k comments) | ❌ none implement this |
| Token-efficient output | ✅ TOON, ~40% smaller than JSON | ❌ verbose JSON or truncated bodies |
| Strip `[deleted]` / `[removed]` stubs | ✅ | ❌ |
| Open WebUI external loader contract | ✅ drop-in | ❌ |
| MCP transport (stdio + HTTP) | ✅ | ✅ (most) |
| Generic crawl fallback for non-Reddit URLs | ✅ via crawl4ai | ❌ |

## Quick start

### Try it

> **A crawl4ai upstream is required.** Reddit now blocks non-browser HTTP clients, so the Reddit engine — like the generic fallback — fetches through crawl4ai's headless browser. The proxy needs `OMNIFEED_CRAWL4AI_URL` set or it exits at startup; the [compose file](#full-mode-proxy--crawl4ai-upstream) below wires it for you.

Once it's running, open a Reddit thread:

```bash
curl -X POST http://localhost:8080/crawl \
  -H 'Content-Type: application/json' \
  -d '{"urls":["https://www.reddit.com/r/LocalLLaMA/comments/.../"]}'
```

Returns the canonical Open WebUI shape: `[{"page_content": "...TOON...", "metadata": {...}}]`.

### Full stack (proxy + searxng + crawl4ai upstreams)

```bash
git clone https://github.com/kinorai/omnifeed.git && cd omnifeed
docker compose up
```

Then point Open WebUI at `http://localhost:8080` as `WEB_LOADER_ENGINE=external`. The compose file also starts SearXNG (with `searxng/settings.yml` mounted — the `json` format it enables is required for the `web_search` tool).

### As an MCP server (Claude Code, Cursor, Windsurf, …)

**Stdio transport (most clients):**

```jsonc
// .cursor/mcp.json or Claude Code MCP config
{
  "mcpServers": {
    "omnifeed": {
      "command": "docker",
      "args": ["run", "--rm", "-i", "kinorai/omnifeed:latest", "--mcp-stdio"]
    }
  }
}
```

**HTTP transport (remote MCP clients):**

```jsonc
{
  "mcpServers": {
    "omnifeed": {
      "url": "http://your-host:8081/mcp"
    }
  }
}
```

Tools exposed:

- `web_search(query, limit?, time_range?, language?)` — web search via SearXNG; returns `[{title, url, snippet, engine, published_date}]` as JSON. Only listed when `OMNIFEED_SEARXNG_URL` is set.
- `fetch_url(url, format?, expand?)` — any URL → LLM-friendly content (reddit.com → TOON comment tree, everything else → filtered markdown).

The intended loop: `web_search` finds URLs (reddit threads included — they surface through SearXNG's general engines), `fetch_url` reads them.

### Authentication

The HTTP transports (`/crawl`, `/mcp`) are guarded by a shared bearer token. Set **`OMNIFEED_API_KEY`** and send it as `Authorization: Bearer <token>`:

```bash
docker run -e OMNIFEED_API_KEY="$(openssl rand -hex 32)" \
  -e OMNIFEED_CRAWL4AI_URL=http://crawl4ai:11235/crawl \
  kinorai/omnifeed
```

Without a key the proxy **refuses to start**, so it can't be left open by accident. For a throwaway local run, opt out explicitly with **`OMNIFEED_DEV_NO_AUTH=true`** (the bundled compose files already do). Stdio MCP doesn't use the token — it inherits the trust of the process that spawned it.

## Configuration

All knobs are OMNIFEED_-prefixed environment variables.

| Variable | Default | Purpose |
|---|---|---|
| `OMNIFEED_LISTEN_ADDR` | `:8080` | HTTP loader (Open WebUI) listen address |
| `OMNIFEED_MCP_LISTEN_ADDR` | `:8081` | MCP HTTP/SSE listen address |
| `OMNIFEED_MCP_STDIO` | `false` | Run MCP over stdio (also via `--mcp-stdio` flag) |
| `OMNIFEED_METRICS_ADDR` | `:9090` | Prometheus + health listen address |
| `OMNIFEED_API_KEY` | _(unset)_ | Bearer token for `/crawl` and `/mcp` (HTTP transport). If unset, the proxy refuses to start unless `OMNIFEED_DEV_NO_AUTH=true`. Stdio MCP is unaffected. |
| `OMNIFEED_DEV_NO_AUTH` | `false` | Explicitly run the HTTP transports with **no** auth when `OMNIFEED_API_KEY` is unset (local/dev only). Ignored if a key is set. |
| `OMNIFEED_CRAWL4AI_URL` | _(required)_ | Upstream crawl4ai endpoint. **Required** — every engine (Reddit + fallback) fetches through crawl4ai; if empty, the proxy exits at startup. |
| `OMNIFEED_CRAWL4AI_TIMEOUT` | `90s` | Per-call timeout to crawl4ai |
| `OMNIFEED_SEARXNG_URL` | _(unset)_ | Upstream SearXNG base URL (e.g. `http://searxng:8080`). **Optional** — when unset, the `web_search` MCP tool is not exposed. The instance must enable the `json` format in `settings.yml`. |
| `OMNIFEED_SEARXNG_TIMEOUT` | `15s` | Per-query timeout to SearXNG |
| `OMNIFEED_SEARCH_MAX_RESULTS` | `25` | Hard cap on the `web_search` tool's `limit` argument (1–100) |
| `OMNIFEED_REDDIT_TIMEOUT` | `4m` | Wall-clock cap for a Reddit thread expansion |
| `OMNIFEED_REDDIT_MAX_ROUNDS` | `3` | Default `/api/morechildren` rounds (max 40 via `?expand=full`) |
| `OMNIFEED_REDDIT_FORMAT` | `toon` | Default Reddit output: `toon` or `json` |
| `OMNIFEED_MAX_URLS_PER_REQUEST` | `30` | Cap on `urls[]` array length |
| `OMNIFEED_PER_DOMAIN_CONCURRENCY` | `2` | Max concurrent requests to one domain |
| `OMNIFEED_PER_DOMAIN_DELAY` | `1500ms` | Minimum delay between same-domain requests |
| `OMNIFEED_BLOCK_PRIVATE_IPS` | `true` | SSRF protection (always on in production) |
| `OMNIFEED_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `OMNIFEED_LOG_FORMAT` | `json` | `json` or `text` |
| `OMNIFEED_ENABLE_PPROF` | `false` | Expose `/debug/pprof/*` (opt-in) |

## API

### POST `/crawl` (Open WebUI external loader contract)

```http
POST /crawl
Authorization: Bearer $OMNIFEED_API_KEY
Content-Type: application/json

{"urls": ["https://www.reddit.com/r/foo/comments/.../"]}
```

Response: `[{"page_content": "...", "metadata": {...}}, ...]`

Per-request query parameters (Reddit URLs only):

- `?format=toon|json` — output format
- `?expand=N|full` — expansion budget (0–40)
- `?depth=1` — include depth field on each comment
- `?nocreated=1` — drop the created field (~7% token savings)

### Health endpoints

- `GET /livez` — process liveness; always 200 unless shutting down
- `GET /readyz` — checks crawl4ai (and SearXNG, when configured) upstream reachability
- `GET /healthz` — alias of `/readyz` (backwards compatibility)
- `GET /metrics` — Prometheus format (`omnifeed_requests_total`, `omnifeed_request_seconds`, `omnifeed_reddit_expansion_rounds`, `omnifeed_search_requests_total`, `omnifeed_search_request_seconds`)

### MCP

JSON-RPC 2.0 at:

- `stdio` when `OMNIFEED_MCP_STDIO=true` or `--mcp-stdio`
- Canonical: **Streamable HTTP** at `/mcp` (MCP spec 2025-03-26) on `OMNIFEED_MCP_LISTEN_ADDR` — `POST /mcp` for one-shot JSON-RPC, `GET /mcp` for the SSE event stream.
- Legacy: `GET /mcp/sse` is kept as an alias for older clients that only speak the deprecated dual-endpoint SSE shape. New clients should target `/mcp`.

## Architecture

Two ports, two flows. The `Searcher` port answers *query → URLs*; the `Engine`
port answers *URL → content*. MCP tools compose them; transports stay pure.

```
            /crawl ──► OpenWebUI transport ─────────────┐
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
              │  (meta-search:   │           │    crawl4ai upstream   │
              │ google/bing/ddg) │           │  (headless browser —   │
              └──────────────────┘           │   fetches all URLs)    │
                                             └───────────────────────┘
```

### Reddit anti-bot handling

Reddit's edge 403-blocks non-browser HTTP clients (it fingerprints the TLS/JA3 handshake), so the Reddit engine never calls Reddit directly. It drives crawl4ai's headless Chromium to a `www.reddit.com` page — which clears the bot wall — then runs a **same-origin `fetch()`** of the `.json` (and `/api/morechildren`) endpoint from inside that page and returns the raw JSON. No Reddit auth, cookies, or API key.

The browser request sets **`enable_stealth` + `override_navigator`** — fingerprint-level evasions evaluated at page *load*, which is what clears the wall. It deliberately omits crawl4ai's **`magic` / `simulate_user`**: those drive post-*load* behavioral simulation (scroll/click) that an in-page `fetch()` never needs, so they were pure latency here. If Reddit's wall starts challenging this path, re-add them as insurance. A per-thread crawl4ai `session_id` is reused across `morechildren` rounds (one warmed context, fewer requests, lower bot-risk).

> Heads-up: the residential/source IP's risk score can rise under sustained scraping — if fetches start returning the block page, slow down, keep `expand` modest, or route crawl4ai through a residential proxy.

Extensibility points:

- **New engines** (HN, Stack Overflow, …): implement `domain.Engine` and register before the fallback
- **New searchers** (Brave, Tavily, reddit-native, …): implement `domain.Searcher`
- **New MCP tools**: add a constructor in `internal/transport/mcp/tools` — the MCP server is a pure transport and never changes
- **New transports**: implement on top of `engine.Registry` / `domain.Searcher`
- **Output encoding** (e.g. TOON rendering): `internal/domain/document.go`

## Development

```bash
git clone https://github.com/kinorai/omnifeed.git
cd omnifeed
go test ./...
go run ./cmd/omnifeed
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Security

See [SECURITY.md](SECURITY.md) for vulnerability reporting.

## License

[MIT](LICENSE) © kinorai
