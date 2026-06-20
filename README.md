<!-- markdownlint-disable MD033 MD041 -->
<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=waving&color=0:F97316,100:7C3AED&height=220&section=header&text=omnifeed&fontSize=82&fontColor=ffffff&animation=fadeIn&fontAlignY=36&desc=One%20self-hosted%20gateway%20-%20web%20search%20%2B%20crawling%20for%20AI%20agents&descAlignY=56&descSize=17" alt="omnifeed" width="100%"/>
</p>

<p align="center">
  <img src="https://readme-typing-svg.demolab.com?font=Fira+Code&weight=600&size=22&pause=1000&color=F97316&center=true&vCenter=true&width=820&height=45&lines=search+%E2%86%92+URLs+%E2%86%92+content%2C+in+one+gateway;Full+Reddit+comment+trees+as+TOON+%E2%80%94+no+API+key;MCP+%2B+Open+WebUI+%2B+REST%2C+one+Go+binary;~40%25+fewer+tokens+than+JSON%2C+lossless" alt="search → URLs → content"/>
</p>

<p align="center">
  <a href="https://github.com/kinorai/omnifeed/actions/workflows/ci.yml"><img src="https://github.com/kinorai/omnifeed/actions/workflows/ci.yml/badge.svg" alt="CI"/></a>
  <a href="https://github.com/kinorai/omnifeed/actions/workflows/security.yml"><img src="https://github.com/kinorai/omnifeed/actions/workflows/security.yml/badge.svg" alt="Security"/></a>
  <a href="https://github.com/kinorai/omnifeed/releases"><img src="https://img.shields.io/github/v/release/kinorai/omnifeed?style=flat-square&color=F97316" alt="Release"/></a>
  <img src="https://img.shields.io/github/go-mod/go-version/kinorai/omnifeed?style=flat-square&logo=go&logoColor=white&color=00ADD8" alt="Go"/>
  <a href="https://hub.docker.com/r/kinorai/omnifeed"><img src="https://img.shields.io/docker/pulls/kinorai/omnifeed?style=flat-square&logo=docker&logoColor=white&color=2496ED" alt="Docker pulls"/></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/kinorai/omnifeed?style=flat-square&color=7C3AED" alt="License"/></a>
  <a href="https://github.com/kinorai/omnifeed/stargazers"><img src="https://img.shields.io/github/stars/kinorai/omnifeed?style=flat-square&color=F97316" alt="Stars"/></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/MCP-server-7C3AED?style=for-the-badge" alt="MCP server"/>
  <img src="https://img.shields.io/badge/Open_WebUI-loader-F97316?style=for-the-badge" alt="Open WebUI loader"/>
  <img src="https://img.shields.io/badge/REST-API-06B6D4?style=for-the-badge" alt="REST API"/>
  <img src="https://img.shields.io/badge/Reddit-TOON-FF4500?style=for-the-badge&logo=reddit&logoColor=white" alt="Reddit TOON"/>
  <img src="https://img.shields.io/badge/crawl4ai-engine-10B981?style=for-the-badge" alt="crawl4ai"/>
  <img src="https://img.shields.io/badge/SearXNG-search-3050FF?style=for-the-badge" alt="SearXNG"/>
</p>

<p align="center">
  <a href="#-quick-start"><b>Quick start</b></a> ·
  <a href="#-demo"><b>Demo</b></a> ·
  <a href="#-configuration"><b>Configuration</b></a> ·
  <a href="#-api"><b>API</b></a> ·
  <a href="#-architecture"><b>Architecture</b></a> ·
  <a href="openapi.yaml"><b>OpenAPI</b></a>
</p>

<p align="center">
omnifeed gives an AI agent the full research loop — <b>search → URLs → content</b> — against self-hosted
<a href="https://github.com/searxng/searxng">SearXNG</a> + <a href="https://github.com/unclecode/crawl4ai">crawl4ai</a>,
with a dedicated Reddit engine that returns <b>full comment trees as <a href="https://github.com/toon-format/toon">TOON</a></b>
(~40% fewer tokens than JSON, lossless) and <b>no Reddit API key</b>.
</p>

- **`web_search`** queries a SearXNG instance (Google/Bing/DDG, Reddit included) and returns ranked URLs with titles and snippets.
- **`fetch_url`** renders any URL through crawl4ai as clean markdown — and Reddit URLs come back as the full comment tree encoded in TOON.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Activities/Sparkles.png" width="26" height="26" /> Why omnifeed

| | omnifeed | Other Reddit MCPs |
|---|---|---|
| Web search → crawl in one self-hosted service | ✅ SearXNG + crawl4ai | ❌ search-only or crawl-only |
| Full comment tree (`/api/morechildren` expansion) | ✅ up to 40 rounds (~4k comments) | ❌ |
| Token-efficient output | ✅ TOON, ~40% smaller than JSON | ❌ verbose JSON or truncated bodies |
| Generic crawl fallback for non-Reddit URLs | ✅ via crawl4ai | ❌ |
| Front-ends | MCP **+** Open WebUI **+** REST | MCP only (most) |

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Clapper%20Board.png" width="26" height="26" /> Demo

<p align="center">
  <img src="assets/demo.gif" alt="omnifeed demo — web_search then fetch_url returning a Reddit thread as TOON (render assets/demo.tape with vhs)" width="100%"/>
</p>

<p align="center"><sub><code>web_search</code> → pick a URL → <code>fetch_url</code> → full Reddit comment tree as TOON. Generate this clip with <code>vhs assets/demo.tape</code>.</sub></p>

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Travel%20and%20places/Rocket.png" width="26" height="26" /> Quick start

```bash
# No clone needed — fetch the compose file + SearXNG settings, then start:
curl -fsSL https://raw.githubusercontent.com/kinorai/omnifeed/main/docker-compose.yml -o docker-compose.yml
curl -fsSL --create-dirs https://raw.githubusercontent.com/kinorai/omnifeed/main/searxng/settings.yml -o searxng/settings.yml
docker compose up
```

Starts omnifeed + SearXNG + crawl4ai. Point Open WebUI at `http://localhost:8080` with `WEB_LOADER_ENGINE=external`. (SearXNG is mounted with `searxng/settings.yml`, which enables the `json` format the `web_search` tool needs.)

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Electric%20Plug.png" width="22" height="22" /> As an MCP server (Claude Code, Cursor, Windsurf, …)

Stdio — most clients. omnifeed always fetches through crawl4ai (and SearXNG, if you want `web_search`), so the container your client spawns must be told where they are. The simplest setup joins the network from `docker compose up` and uses the service names:

```jsonc
{
  "mcpServers": {
    "omnifeed": {
      "command": "docker",
      "args": [
        "run", "--rm", "-i",
        "--network", "omnifeed_default",
        "-e", "OMNIFEED_CRAWL4AI_URL=http://crawl4ai:11235/crawl",
        "-e", "OMNIFEED_SEARXNG_URL=http://searxng:8080",
        "kinorai/omnifeed:latest", "--mcp-stdio"
      ]
    }
  }
}
```

Without `OMNIFEED_CRAWL4AI_URL` the container exits at startup. If crawl4ai/SearXNG run elsewhere, point the URLs there (e.g. `http://host.docker.internal:11235/crawl`, adding `--add-host=host.docker.internal:host-gateway` on Linux).

HTTP — remote clients, or the simplest option when the `docker compose up` stack is already running (no second container, no networking to wire up):

```jsonc
{ "mcpServers": { "omnifeed": { "url": "http://your-host:8081/mcp" } } }
```

Tools: **`fetch_url`** (always) and **`web_search`** (only when `OMNIFEED_SEARXNG_URL` is set). The intended loop is `web_search` → pick URLs → `fetch_url`.

`/crawl` returns `[{"page_content": "...", "metadata": {...}}]` — already the shape of a LangChain / LlamaIndex `Document`, so wrapping it as a custom document loader takes only a few lines.

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Locked%20with%20Key.png" width="22" height="22" /> Authentication

The HTTP transports (`/crawl`, `/search`, `/mcp`) share one bearer token. Set **`OMNIFEED_API_KEY`** and send `Authorization: Bearer <token>`:

```bash
# Generate a key and print it — clients send it as the bearer token, so save it somewhere.
export OMNIFEED_API_KEY="$(openssl rand -hex 32)"
echo "OMNIFEED_API_KEY=$OMNIFEED_API_KEY"

# `-e OMNIFEED_API_KEY` (no value) forwards the variable from your shell.
docker run -e OMNIFEED_API_KEY \
  -e OMNIFEED_CRAWL4AI_URL=http://crawl4ai:11235/crawl \
  kinorai/omnifeed
```

Without a key the proxy **refuses to start**, so it can't be left open by accident. For a throwaway local run, opt out with **`OMNIFEED_DEV_NO_AUTH=true`** (the compose files already do). Stdio MCP needs no token — it inherits the trust of the process that spawned it.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Gear.png" width="26" height="26" /> Configuration

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
| `OMNIFEED_REDDIT_FETCH_LIMIT` | `500` | Reddit `limit`: max comments fetched in the initial tree |
| `OMNIFEED_REDDIT_DEPTH` | `20` | Reddit `depth`: max nesting depth of the initial tree |
| `OMNIFEED_REDDIT_SORT` | `top` | Reddit `sort`: one of `confidence` (=best), `top`, `new`, `controversial`, `old`, `random`, `qa`, `live` |
| `OMNIFEED_REDDIT_MAX_COMMENTS` | `0` | Hard cap on total comments emitted after expansion (0 = unlimited) |
| `OMNIFEED_REDDIT_MAX_TOP_LEVEL` | `0` | Hard cap on top-level comment threads, replies included (0 = unlimited) |
| `OMNIFEED_REDDIT_KEEP_CREATED` | `true` | Include each comment's `created` timestamp |
| `OMNIFEED_REDDIT_KEEP_DEPTH` | `false` | Include each comment's `depth` field |
| `OMNIFEED_MAX_URLS_PER_REQUEST` | `30` | Cap on `urls[]` length |
| `OMNIFEED_PER_DOMAIN_CONCURRENCY` | `2` | Max concurrent requests to one domain |
| `OMNIFEED_PER_DOMAIN_DELAY` | `1500ms` | Minimum delay between same-domain requests |
| `OMNIFEED_BLOCK_PRIVATE_IPS` | `true` | SSRF protection (keep on in production) |
| `OMNIFEED_LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `OMNIFEED_LOG_FORMAT` | `json` | `json` or `text` |
| `OMNIFEED_ENABLE_PPROF` | `false` | Expose `/debug/pprof/*` (opt-in) |

### Controlling Reddit response size

A Reddit thread's comment tree can be huge. The size knobs come in two kinds — it matters which is which:

- **Upstream Reddit params** — forwarded verbatim to Reddit's API, so Reddit owns their behavior: `OMNIFEED_REDDIT_FETCH_LIMIT` → `limit`, `OMNIFEED_REDDIT_DEPTH` → `depth`, `OMNIFEED_REDDIT_SORT` → `sort`. They shape *what Reddit sends back* (less latency, fewer tokens) but are **approximate**, and `limit`/`depth` bound only the **initial** fetch. Semantics are Reddit's, not ours — see <https://www.reddit.com/dev/api/> → `GET [/r/subreddit]/comments/article` (`limit` = "maximum number of comments to return", `depth` = "maximum depth of subtrees").
- **omnifeed engine caps** — our own, applied *after* fetch + expansion, so they're **exact and independent of Reddit**: `OMNIFEED_REDDIT_MAX_COMMENTS` (truncate the flat comment list) and `OMNIFEED_REDDIT_MAX_TOP_LEVEL` (keep the first N top-level threads, in `sort` order, with their replies).

Rule of thumb: reach for the **upstream params** to fetch less from Reddit; reach for the **engine caps** when you need a guaranteed ceiling — `OMNIFEED_REDDIT_MAX_ROUNDS` expansion adds comments on top of `limit`, so only the caps bound the final total. All five are also per-request on the `fetch_url` MCP tool (`limit`, `depth`, `sort`, `max_comments`, `max_top_level`); a positive value overrides the env default.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Satellite%20Antenna.png" width="26" height="26" /> API

Two bearer-authenticated JSON endpoints on the loader port (`:8080`). The full reference — request/response schemas, query params, and error codes — lives in **[`openapi.yaml`](openapi.yaml)** (paste it into [editor.swagger.io](https://editor.swagger.io/) or any OpenAPI viewer).

- **`POST /crawl`** — URL → content (Open WebUI external-loader contract). Body `{"urls": [...]}` → `[{"page_content", "metadata"}, ...]`. Reddit-only query params: `format=toon|json`, `expand=N|full`, `depth=1`, `nocreated=1`.
- **`POST /search`** — query → ranked result URLs. Exposed only when `OMNIFEED_SEARXNG_URL` is set. Body `{"query", "limit", "time_range", "language"}`.

```bash
curl -s http://localhost:8080/crawl \
  -H "Authorization: Bearer $OMNIFEED_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"urls": ["https://www.reddit.com/r/golang/comments/.../"]}'
```

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Bar%20Chart.png" width="22" height="22" /> Health & metrics

- `GET /livez` — liveness; 200 unless shutting down
- `GET /readyz` — checks crawl4ai (and SearXNG, when configured); `GET /healthz` is an alias
- `GET /metrics` — Prometheus (`omnifeed_requests_total`, `omnifeed_request_seconds`, `omnifeed_reddit_expansion_rounds`, `omnifeed_search_requests_total`, `omnifeed_search_request_seconds`)

Health and metrics listen on `OMNIFEED_METRICS_ADDR` (default `:9090`), separate from the API (`:8080`) and MCP (`:8081`) ports.

### MCP

JSON-RPC 2.0 at:

- **stdio** when `OMNIFEED_MCP_STDIO=true` or `--mcp-stdio`
- **Streamable HTTP** at `/mcp` (spec 2025-03-26) on `OMNIFEED_MCP_LISTEN_ADDR` — `POST /mcp` for one-shot calls, `GET /mcp` for the SSE stream
- `GET /mcp/sse` is a legacy alias for older dual-endpoint SSE clients; new clients target `/mcp`

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Travel%20and%20places/Building%20Construction.png" width="26" height="26" /> Architecture

Two ports answer two questions. The `Searcher` port answers *query → URLs*; the `Engine` port answers *URL → content*. MCP tools and REST handlers compose them; transports stay thin.

```mermaid
%%{init: {"theme":"base","themeVariables":{"background":"transparent","mainBkg":"#161b22","primaryColor":"#161b22","primaryTextColor":"#e6edf3","primaryBorderColor":"#F97316","lineColor":"#8b949e","secondaryColor":"#161b22","tertiaryColor":"#161b22","fontFamily":"Inter, system-ui, sans-serif"},"flowchart":{"curve":"basis","htmlLabels":true}}}%%
flowchart TB
  crawl["POST /crawl"] e1@--> owt["Open WebUI<br/>transport"]
  search["POST /search"] e2@--> sat["SearchAPI<br/>transport"]
  mcpStdio["MCP stdio"] e3@--> mcp["MCP server"]
  mcpHTTP["MCP HTTP /mcp"] e4@--> mcp

  owt e5@--> reg["Engine Registry"]
  mcp -- crawl tools --> reg
  sat e6@--> searcher["Searcher<br/>(SearXNG)"]
  mcp -- search tool --> searcher

  reg e7@--> reddit["Reddit engine<br/>(TOON)"]
  reg e8@--> generic["Generic fallback<br/>(markdown)"]
  reddit e9@--> c4["crawl4ai upstream<br/>(headless browser)"]
  generic e10@--> c4
  searcher e11@--> sx["SearXNG upstream<br/>(Google / Bing / DDG)"]

  classDef box fill:#161b22,stroke:#30363d,stroke-width:1px,color:#e6edf3;
  classDef accent fill:#0d1117,stroke:#F97316,stroke-width:2px,color:#ffd9b3;
  classDef animate stroke:#F97316,stroke-width:2px,stroke-dasharray:10 6,stroke-dashoffset:900,animation:dash 14s linear infinite;
  class crawl,search,mcpStdio,mcpHTTP,owt,sat box;
  class mcp,reg,searcher,reddit,generic,c4,sx accent;
  class e1,e2,e3,e4,e5,e6,e7,e8,e9,e10,e11 animate;
```

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Shield.png" width="22" height="22" /> Reddit anti-bot handling

Reddit's edge 403-blocks non-browser HTTP clients (it fingerprints the TLS/JA3 handshake), so the Reddit engine never calls Reddit directly. It drives crawl4ai's headless Chromium to a `www.reddit.com` page (which clears the bot wall), then runs a **same-origin `fetch()`** of the `.json` and `/api/morechildren` endpoints from inside that page. No Reddit auth, cookies, or API key. A per-thread crawl4ai `session_id` is reused across expansion rounds to keep one warmed context.

> Sustained scraping can raise your source IP's risk score. If fetches start returning the block page, slow down, keep `expand` modest, or route crawl4ai through a residential proxy.

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Activities/Puzzle%20Piece.png" width="22" height="22" /> Extending it

New engines (Hacker News, Stack Overflow, …), searchers (Brave, Tavily, …), MCP tools, and transports each plug into one small port without touching the rest. See **[AGENTS.md → Adding things](AGENTS.md#adding-things)** for the architecture and a step-by-step.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Hammer%20and%20Wrench.png" width="26" height="26" /> Development

```bash
git clone https://github.com/kinorai/omnifeed.git && cd omnifeed
make check        # vet + lint + test (what CI runs)
go run ./cmd/omnifeed
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full workflow, [SECURITY.md](SECURITY.md) for vulnerability reporting, and [AGENTS.md](AGENTS.md) if you're a coding agent working in this repo.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Travel%20and%20places/Star.png" width="26" height="26" /> Star history

<a href="https://star-history.com/#kinorai/omnifeed&Date">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://api.star-history.com/svg?repos=kinorai/omnifeed&type=Date&theme=dark" />
   <source media="(prefers-color-scheme: light)" srcset="https://api.star-history.com/svg?repos=kinorai/omnifeed&type=Date" />
   <img alt="Star History Chart" src="https://api.star-history.com/svg?repos=kinorai/omnifeed&type=Date" width="70%" />
 </picture>
</a>

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Hand%20gestures/Handshake.png" width="26" height="26" /> Contributing

<div align="center">

**Star it if it's useful — it helps other AI builders find omnifeed.**

[![Star](https://img.shields.io/badge/⭐_Star_omnifeed-F97316?style=for-the-badge)](https://github.com/kinorai/omnifeed)
[![Open an issue](https://img.shields.io/badge/🐛_Open_an_Issue-161b22?style=for-the-badge)](https://github.com/kinorai/omnifeed/issues/new)
[![Submit a PR](https://img.shields.io/badge/🔧_Submit_a_PR-7C3AED?style=for-the-badge)](https://github.com/kinorai/omnifeed/pulls)

</div>

New engines, searchers, MCP tools, and transports are all welcome — start with [AGENTS.md](AGENTS.md#adding-things) and [CONTRIBUTING.md](CONTRIBUTING.md).

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Page%20Facing%20Up.png" width="26" height="26" /> License

[MIT](LICENSE) © kinorai

<img src="https://capsule-render.vercel.app/api?type=waving&color=0:7C3AED,100:F97316&height=120&section=footer" width="100%"/>
