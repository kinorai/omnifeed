# crawl4ai-reddit-proxy

> **LLM-friendly web crawler & scraper with a dedicated Reddit engine, built on Crawl4AI — Open WebUI compatible**

[![CI](https://github.com/kinorai/crawl4ai-reddit-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/kinorai/crawl4ai-reddit-proxy/actions/workflows/ci.yml)
[![Security](https://github.com/kinorai/crawl4ai-reddit-proxy/actions/workflows/security.yml/badge.svg)](https://github.com/kinorai/crawl4ai-reddit-proxy/actions/workflows/security.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

A single Go binary that wraps [crawl4ai](https://github.com/unclecode/crawl4ai) and adds a **dedicated Reddit engine** that returns full comment trees encoded as **[TOON](https://github.com/toon-format/toon)** — typically **40% fewer tokens than JSON**, lossless. Implements both the **Open WebUI external-loader contract** and an **MCP server** (stdio + HTTP/SSE).

Most Reddit MCP servers either ship pretty-printed JSON or "save tokens" by truncating comments. This one does neither: full `/api/morechildren` expansion, TOON encoding, deleted-comment stripping. Reddit now blocks non-browser HTTP clients at its edge, so Reddit fetches are routed through a [crawl4ai](https://github.com/unclecode/crawl4ai) headless browser — no auth or API key required.

## Why this exists

| | crawl4ai-reddit-proxy | Other Reddit MCPs |
|---|---|---|
| Full comment tree (`/api/morechildren` expansion) | ✅ up to 40 rounds (~4k comments) | ❌ none implement this |
| Token-efficient output | ✅ TOON, ~40% smaller than JSON | ❌ verbose JSON or truncated bodies |
| Strip `[deleted]` / `[removed]` stubs | ✅ | ❌ |
| Open WebUI external loader contract | ✅ drop-in | ❌ |
| MCP transport (stdio + HTTP) | ✅ | ✅ (most) |
| Generic crawl fallback for non-Reddit URLs | ✅ via crawl4ai | ❌ |

## Quick start

### Try it

> **A crawl4ai upstream is required.** Reddit now blocks non-browser HTTP clients, so the Reddit engine — like the generic fallback — fetches through crawl4ai's headless browser. The proxy needs `CARP_CRAWL4AI_URL` set or it exits at startup; the [compose file](#full-mode-proxy--crawl4ai-upstream) below wires it for you.

Once it's running, open a Reddit thread:

```bash
curl -X POST http://localhost:8080/crawl \
  -H 'Content-Type: application/json' \
  -d '{"urls":["https://www.reddit.com/r/LocalLLaMA/comments/.../"]}'
```

Returns the canonical Open WebUI shape: `[{"page_content": "...TOON...", "metadata": {...}}]`.

### Full mode (proxy + crawl4ai upstream)

```bash
curl -O https://raw.githubusercontent.com/kinorai/crawl4ai-reddit-proxy/main/docker-compose.yml
docker compose up
```

Then point Open WebUI at `http://localhost:8080` as `WEB_LOADER_ENGINE=external`.

### As an MCP server (Claude Code, Cursor, Windsurf, …)

**Stdio transport (most clients):**

```jsonc
// .cursor/mcp.json or Claude Code MCP config
{
  "mcpServers": {
    "crawl4ai-reddit-proxy": {
      "command": "docker",
      "args": ["run", "--rm", "-i", "kinorai/crawl4ai-reddit-proxy:latest", "--mcp-stdio"]
    }
  }
}
```

**HTTP transport (remote MCP clients):**

```jsonc
{
  "mcpServers": {
    "crawl4ai-reddit-proxy": {
      "url": "http://your-host:8081/mcp"
    }
  }
}
```

Tools exposed: `crawl(url, format?, expand?)` and `reddit_get_post(url, expand?)`.

### Authentication

The HTTP transports (`/crawl`, `/mcp`) are guarded by a shared bearer token. Set **`CARP_API_KEY`** and send it as `Authorization: Bearer <token>`:

```bash
docker run -e CARP_API_KEY="$(openssl rand -hex 32)" \
  -e CARP_CRAWL4AI_URL=http://crawl4ai:11235/crawl \
  kinorai/crawl4ai-reddit-proxy
```

Without a key the proxy **refuses to start**, so it can't be left open by accident. For a throwaway local run, opt out explicitly with **`CARP_DEV_NO_AUTH=true`** (the bundled compose files already do). Stdio MCP doesn't use the token — it inherits the trust of the process that spawned it.

## Configuration

All knobs are CARP_-prefixed environment variables.

| Variable | Default | Purpose |
|---|---|---|
| `CARP_LISTEN_ADDR` | `:8080` | HTTP loader (Open WebUI) listen address |
| `CARP_MCP_LISTEN_ADDR` | `:8081` | MCP HTTP/SSE listen address |
| `CARP_MCP_STDIO` | `false` | Run MCP over stdio (also via `--mcp-stdio` flag) |
| `CARP_METRICS_ADDR` | `:9090` | Prometheus + health listen address |
| `CARP_API_KEY` | _(unset)_ | Bearer token for `/crawl` and `/mcp` (HTTP transport). If unset, the proxy refuses to start unless `CARP_DEV_NO_AUTH=true`. Stdio MCP is unaffected. |
| `CARP_DEV_NO_AUTH` | `false` | Explicitly run the HTTP transports with **no** auth when `CARP_API_KEY` is unset (local/dev only). Ignored if a key is set. |
| `CARP_CRAWL4AI_URL` | _(required)_ | Upstream crawl4ai endpoint. **Required** — every engine (Reddit + fallback) fetches through crawl4ai; if empty, the proxy exits at startup. |
| `CARP_CRAWL4AI_TIMEOUT` | `90s` | Per-call timeout to crawl4ai |
| `CARP_REDDIT_TIMEOUT` | `4m` | Wall-clock cap for a Reddit thread expansion |
| `CARP_REDDIT_MAX_ROUNDS` | `3` | Default `/api/morechildren` rounds (max 40 via `?expand=full`) |
| `CARP_REDDIT_FORMAT` | `toon` | Default Reddit output: `toon` or `json` |
| `CARP_MAX_URLS_PER_REQUEST` | `30` | Cap on `urls[]` array length |
| `CARP_PER_DOMAIN_CONCURRENCY` | `2` | Max concurrent requests to one domain |
| `CARP_PER_DOMAIN_DELAY` | `1500ms` | Minimum delay between same-domain requests |
| `CARP_BLOCK_PRIVATE_IPS` | `true` | SSRF protection (always on in production) |
| `CARP_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `CARP_LOG_FORMAT` | `json` | `json` or `text` |
| `CARP_ENABLE_PPROF` | `false` | Expose `/debug/pprof/*` (opt-in) |

## API

### POST `/crawl` (Open WebUI external loader contract)

```http
POST /crawl
Authorization: Bearer $CARP_API_KEY
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
- `GET /readyz` — checks crawl4ai upstream reachability when configured
- `GET /healthz` — alias of `/readyz` (backwards compatibility)
- `GET /metrics` — Prometheus format

### MCP

JSON-RPC 2.0 at:

- `stdio` when `CARP_MCP_STDIO=true` or `--mcp-stdio`
- Canonical: **Streamable HTTP** at `/mcp` (MCP spec 2025-03-26) on `CARP_MCP_LISTEN_ADDR` — `POST /mcp` for one-shot JSON-RPC, `GET /mcp` for the SSE event stream.
- Legacy: `GET /mcp/sse` is kept as an alias for older clients that only speak the deprecated dual-endpoint SSE shape. New clients should target `/mcp`.

## Architecture

```
                                       ┌────────────────┐
                  /crawl   ─────────►  │  OpenWebUI     │
                                       │  transport     │
                                       └───────┬────────┘
                                               ▼
                                       ┌────────────────┐
                       MCP stdio  ───► │     Engine     │
                       MCP HTTP   ───► │    Registry    │
                                       └───┬────────┬───┘
                                           ▼        ▼
                                    ┌────────┐ ┌────────────┐
                                    │ reddit │ │  generic   │
                                    │ engine │ │  fallback  │
                                    │ (TOON) │ │ (markdown) │
                                    └───┬────┘ └─────┬──────┘
                                        └──────┬─────┘
                                               ▼
                                    ┌───────────────────────┐
                                    │    crawl4ai upstream   │
                                    │  (headless browser —   │
                                    │   fetches all URLs)    │
                                    └───────────────────────┘
```

### Reddit anti-bot handling

Reddit's edge 403-blocks non-browser HTTP clients (it fingerprints the TLS/JA3 handshake), so the Reddit engine never calls Reddit directly. It drives crawl4ai's headless Chromium to a `www.reddit.com` page — which clears the bot wall — then runs a **same-origin `fetch()`** of the `.json` (and `/api/morechildren`) endpoint from inside that page and returns the raw JSON. No Reddit auth, cookies, or API key.

The browser request sets **`enable_stealth` + `override_navigator`** — fingerprint-level evasions evaluated at page *load*, which is what clears the wall. It deliberately omits crawl4ai's **`magic` / `simulate_user`**: those drive post-*load* behavioral simulation (scroll/click) that an in-page `fetch()` never needs, so they were pure latency here. If Reddit's wall starts challenging this path, re-add them as insurance. A per-thread crawl4ai `session_id` is reused across `morechildren` rounds (one warmed context, fewer requests, lower bot-risk).

> Heads-up: the residential/source IP's risk score can rise under sustained scraping — if fetches start returning the block page, slow down, keep `expand` modest, or route crawl4ai through a residential proxy.

Extensibility points:

- **New engines** (HN, Stack Overflow, …): implement `domain.Engine` and register before the fallback
- **New transports**: implement on top of `engine.Registry`
- **Output encoding** (e.g. TOON rendering): `internal/domain/document.go`

## Development

```bash
git clone https://github.com/kinorai/crawl4ai-reddit-proxy.git
cd crawl4ai-reddit-proxy
go test ./...
go run ./cmd/crawl4ai-reddit-proxy
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Security

See [SECURITY.md](SECURITY.md) for vulnerability reporting.

## License

[MIT](LICENSE) © kinorai
