<!-- markdownlint-disable MD033 MD041 -->
<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=waving&color=0:FF4500,100:7C3AED&height=220&section=header&text=omnifeed&fontSize=82&fontColor=ffffff&animation=fadeIn&fontAlignY=36" alt="omnifeed" width="100%"/>
</p>

<p align="center">
  <img src="https://readme-typing-svg.demolab.com?font=Fira+Code&weight=700&size=28&color=FF4500&center=true&vCenter=true&multiline=true&repeat=false&duration=1500&pause=500&width=860&height=110&lines=Self-hosted+web+search+%2B+fetch+MCP;with+a+dedicated+Reddit+engine+%E2%80%94+and+more" alt="Self-hosted web search + fetch MCP, with a dedicated Reddit engine — and more"/>
</p>

<p align="center">
  <a href="https://github.com/kinorai/omnifeed/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/kinorai/omnifeed/ci.yml?branch=main&label=CI&style=flat-square" alt="CI"/></a>
  <a href="https://github.com/kinorai/omnifeed/releases"><img src="https://img.shields.io/github/v/release/kinorai/omnifeed?style=flat-square&color=FF4500" alt="Release"/></a>
  <a href="https://hub.docker.com/r/kinorai/omnifeed"><img src="https://img.shields.io/docker/pulls/kinorai/omnifeed?style=flat-square&logo=docker&logoColor=white&color=2496ED" alt="Docker pulls"/></a>
</p>

<p align="center">
omnifeed gives an AI agent the full research loop — <b>search → URLs → content</b> — against self-hosted
<a href="https://github.com/searxng/searxng">SearXNG</a> + <a href="https://github.com/unclecode/crawl4ai">crawl4ai</a>.
A dedicated <b>Reddit engine</b> returns full comment trees as <a href="https://github.com/toon-format/toon">TOON</a>
(~40% fewer tokens than JSON, lossless) with <b>no Reddit API key</b> — and Hacker News, GitHub, and Discourse get
the same treatment.
</p>

- **`web_search`** queries a SearXNG instance (Google/Bing/DDG, Reddit included) and returns ranked URLs with titles and snippets. A `site` argument scopes results to one hostname — use it instead of naming the site in the query text, which the engines read as a topic word.
- **`fetch_url`** renders any URL through crawl4ai as clean markdown — and dedicated engines return TOON instead: Reddit (threads *and* `/r/{sub}` listings — which honor the URL's own `?t=` time window and `?limit=` post count) through a real browser, plus Hacker News, GitHub issues / pull requests, Bluesky posts and profiles, and Discourse topics read straight from their public APIs.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Activities/Sparkles.png" width="26" height="26" /> Why omnifeed

| | omnifeed | Cloud web MCPs / other Reddit MCPs |
|---|---|---|
| Works on Reddit | ✅ your residential IP + real browser | ❌ datacenter IPs → 403 |
| Web search → crawl in one self-hosted service | ✅ SearXNG + crawl4ai | ❌ search-only or crawl-only |
| Full comment tree (`/api/morechildren` expansion) | ✅ up to 40 rounds (~4k comments) | ❌ |
| Token-efficient output | ✅ TOON, ~40% smaller than JSON | ❌ verbose JSON or truncated bodies |
| Generic crawl fallback for non-Reddit URLs | ✅ via crawl4ai | ❌ |
| Front-ends | MCP **+** Open WebUI **+** REST | MCP only (most) |

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Travel%20and%20places/Rocket.png" width="26" height="26" /> Quick start

```bash
# Fetch the compose file + SearXNG settings, then start:
curl -fsSL https://raw.githubusercontent.com/kinorai/omnifeed/main/docker-compose.yml -o docker-compose.yml
curl -fsSL --create-dirs https://raw.githubusercontent.com/kinorai/omnifeed/main/searxng/settings.yml -o searxng/settings.yml
docker compose up
```

Starts omnifeed + SearXNG + crawl4ai — **tokenless out of the box** (the compose file sets `OMNIFEED_DEV_NO_AUTH=true`), so `docker compose up` just works. Point Open WebUI at `http://localhost:8080` with `WEB_LOADER_ENGINE=external`. (SearXNG is mounted with `searxng/settings.yml`, which enables the `json` format `web_search` needs.) See **Authentication** below to require a bearer token.

**On Apple Silicon** you can skip Docker entirely and run the stack on Apple's native [`container`](https://github.com/apple/container) runtime — see **[docs/apple-container.md](docs/apple-container.md)**.

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Electric%20Plug.png" width="22" height="22" /> As an MCP server

Works with any MCP client — **Claude Code, Cursor, Codex, Gemini CLI, OpenCode, Windsurf, Pi**, and more. Speaks both the current stateless MCP protocol and the older initialize-era revisions, so old and new clients share the same endpoint. Stateless-protocol requests get the spec's HTTP statuses (400 for header/version violations, 404 for unknown methods); initialize-era responses stay 200, and cross-origin browser requests are rejected unless allowlisted via [`OMNIFEED_ALLOWED_ORIGINS`](docs/configuration.md).

**HTTP — recommended.** `docker compose up` already runs the MCP server on `:8081` (tokenless in dev mode), so the simplest setup is no extra container at all — point your client at the URL:

```jsonc
{ "mcpServers": { "omnifeed": { "url": "http://localhost:8081/mcp" } } }
```

**Stdio — for clients that only speak stdio.** A stdio server is spawned and owned by your client (it pipes JSON-RPC over the process's stdin/stdout), so it can't be a long-running compose service — but you can launch the `mcp` profile from this compose file, which keeps every setting (upstreams, network, image) in one place:

```jsonc
{
  "mcpServers": {
    "omnifeed": {
      "command": "docker",
      "args": ["compose", "-f", "/abs/path/to/docker-compose.yml", "run", "-T", "--rm", "mcp"]
    }
  }
}
```

`run -T` disables the TTY so JSON-RPC pipes cleanly; the container joins the stack's network and reuses crawl4ai/SearXNG. Bring the stack up first (`docker compose up -d`) so the upstreams are healthy.

<details>
<summary><b>Standalone stdio — without the compose stack</b></summary>

Spawn the container directly and tell it where crawl4ai/SearXNG are reachable (omnifeed exits at startup without `OMNIFEED_CRAWL4AI_URL`):

```jsonc
{
  "mcpServers": {
    "omnifeed": {
      "command": "docker",
      "args": [
        "run", "--rm", "-i",
        "-e", "OMNIFEED_CRAWL4AI_URL=http://host.docker.internal:11235/crawl",
        "-e", "OMNIFEED_SEARXNG_URL=http://host.docker.internal:8080",
        "kinorai/omnifeed:latest", "--mcp-stdio"
      ]
    }
  }
}
```

On Linux, add `"--add-host=host.docker.internal:host-gateway"` to the args so `host.docker.internal` resolves.
</details>

Tools: **`fetch_url`** (always) and **`web_search`** (only when `OMNIFEED_SEARXNG_URL` is set). The intended loop is `web_search` → pick URLs → `fetch_url`.

`/crawl` returns `[{"page_content": "...", "metadata": {...}}]` — already the shape of a LangChain / LlamaIndex `Document`, so wrapping it as a custom document loader takes only a few lines.

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Locked%20with%20Key.png" width="22" height="22" /> Authentication

The compose stack runs **tokenless** for local use (`OMNIFEED_DEV_NO_AUTH=true`). To require a bearer token instead, generate one — this is the value clients send as `Authorization: Bearer <token>`, so copy it:

```bash
openssl rand -hex 32        # ← your token; copy this
```

Then in `docker-compose.yml` set `OMNIFEED_API_KEY` to that value and remove `OMNIFEED_DEV_NO_AUTH`. Without a key (and without `OMNIFEED_DEV_NO_AUTH=true`) the proxy **refuses to start**, so it can't be left open by accident. Stdio MCP needs no token — it inherits the trust of the process that spawned it.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Gear.png" width="26" height="26" /> Configuration

Everything is configured with `OMNIFEED_`-prefixed environment variables. In practice you only ever **set three** — `OMNIFEED_API_KEY`, `OMNIFEED_CRAWL4AI_URL`, and (optionally) `OMNIFEED_SEARXNG_URL`. The rest have sane defaults.

The full reference lives in **[docs/configuration.md](docs/configuration.md)** — every variable, content-size control (resumable `max_chars` / `start_char` truncation on `fetch_url`), infinite-scroll fetching, Reddit size knobs, and Prometheus metrics.

**Running more than one replica?** Set `OMNIFEED_REDIS_URL` and the rate limiters share their state through Redis, so the whole deployment obeys one limit instead of one limit per pod (N replicas otherwise send N times the configured rate, which is what upstream search engines notice). It is opt-in and fail-open: unset, every replica paces in its own memory exactly as before, and if Redis becomes unreachable the limiters fall straight back to that in-process pacing rather than failing a crawl.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Travel%20and%20places/Building%20Construction.png" width="26" height="26" /> Architecture

```mermaid
%%{init: {"theme":"base","themeVariables":{"background":"transparent","mainBkg":"#161b22","primaryColor":"#161b22","primaryTextColor":"#e6edf3","primaryBorderColor":"#FF4500","lineColor":"#8b949e","secondaryColor":"#161b22","tertiaryColor":"#161b22"},"flowchart":{"curve":"basis","htmlLabels":false}}}%%
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
  reg e12@--> hn["Hacker News engine<br/>(TOON)"]
  reg e14@--> gh["GitHub engine<br/>(TOON)"]
  reg e16@--> disc["Discourse engine<br/>(TOON)"]
  reg e8@--> generic["Generic fallback<br/>(markdown)"]
  reddit e9@--> c4["crawl4ai upstream<br/>(headless browser)"]
  generic e10@--> c4
  hn e13@--> algolia["Algolia HN API<br/>(hn.algolia.com)"]
  gh e15@--> ghapi["GitHub REST API<br/>(api.github.com)"]
  disc e17@--> discapi["Discourse topic JSON<br/>(allowlisted forums)"]
  searcher e11@--> sx["SearXNG upstream<br/>(Google / Bing / DDG)"]

  classDef box fill:#161b22,stroke:#30363d,stroke-width:1px,color:#e6edf3;
  classDef accent fill:#0d1117,stroke:#FF4500,stroke-width:2px,color:#ffd9b3;
  classDef animate stroke:#FF4500,stroke-width:2px,stroke-dasharray:10 6,stroke-dashoffset:900,animation:dash 14s linear infinite;
  class crawl,search,mcpStdio,mcpHTTP,owt,sat box;
  class mcp,reg,searcher,reddit,hn,gh,disc,generic,c4,sx,algolia,ghapi,discapi accent;
  class e1,e2,e3,e4,e5,e6,e7,e8,e9,e10,e11,e12,e13,e14,e15,e16,e17 animate;
```

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Shield.png" width="22" height="22" /> Reddit anti-bot handling

Reddit's edge 403-blocks non-browser HTTP clients, so the Reddit engine never calls Reddit directly: it drives a **real headless browser** to a `www.reddit.com` page, then fetches Reddit's JSON from inside that page — no auth, cookies, or API key. Sustained scraping can still raise your source IP's risk score, so slow down if fetches start returning the block page. Details and tuning: [docs/configuration.md](docs/configuration.md#reddit-anti-bot-handling).

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Activities/Puzzle%20Piece.png" width="22" height="22" /> Extending it

New engines (Hacker News, Stack Overflow, …), searchers (Brave, Tavily, …), MCP tools, and transports each plug into one small port without touching the rest. See **[AGENTS.md → Adding things](AGENTS.md#adding-things)** for the architecture and a step-by-step.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Hammer%20and%20Wrench.png" width="26" height="26" /> Development

```bash
git clone https://github.com/kinorai/omnifeed.git && cd omnifeed
make check        # vet + lint + test — hermetic, no upstreams or token needed
docker compose up # run the full stack locally (tokenless: ports 8080 / 8081 / 9090)
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the full workflow, [SECURITY.md](SECURITY.md) for vulnerability reporting, and [AGENTS.md](AGENTS.md) if you're a coding agent working in this repo.

Prometheus metrics are served on `:9090/metrics` — the full metric table is in [docs/configuration.md](docs/configuration.md#prometheus-metrics).

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Hand%20gestures/Handshake.png" width="26" height="26" /> Contributing

<div align="center">

**Star it if it's useful — it helps other AI builders find omnifeed.**

[![Star](https://img.shields.io/badge/⭐_Star_omnifeed-FF4500?style=for-the-badge)](https://github.com/kinorai/omnifeed)
[![Open an issue](https://img.shields.io/badge/🐛_Open_an_Issue-161b22?style=for-the-badge)](https://github.com/kinorai/omnifeed/issues/new)
[![Submit a PR](https://img.shields.io/badge/🔧_Submit_a_PR-7C3AED?style=for-the-badge)](https://github.com/kinorai/omnifeed/pulls)

</div>

New engines, searchers, MCP tools, and transports are all welcome — start with [AGENTS.md](AGENTS.md#adding-things) and [CONTRIBUTING.md](CONTRIBUTING.md).

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Page%20Facing%20Up.png" width="26" height="26" /> License

[MIT](LICENSE) © kinorai

<img src="https://capsule-render.vercel.app/api?type=waving&color=0:7C3AED,100:FF4500&height=120&section=footer" width="100%"/>
