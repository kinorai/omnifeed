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
omnifeed gives an AI agent the full research loop, <b>search → URLs → content</b>, on self-hosted
<a href="https://github.com/searxng/searxng">SearXNG</a> and <a href="https://github.com/unclecode/crawl4ai">crawl4ai</a>.
Its <b>Reddit engine</b> returns full comment trees as <a href="https://github.com/toon-format/toon">TOON</a>,
lossless and about 40% fewer tokens than JSON, with <b>no Reddit API key</b>. Hacker News, GitHub, Bluesky and Discourse
get their own engines too.
</p>

- **`web_search`** queries SearXNG (Google, Bing, DDG, Reddit included) and returns ranked URLs with titles and snippets. Pass `site` to scope results to one hostname. Naming the site in the query text fails, because engines read it as a topic word.
- **`fetch_url`** returns any URL as clean markdown through crawl4ai. Dedicated engines return TOON instead: Reddit threads and `/r/{sub}` listings through a real browser (listings honor the URL's `?t=` and `?limit=`), plus Hacker News, GitHub issues and pull requests, Bluesky posts and profiles, and Discourse topics from their public APIs.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Activities/Sparkles.png" width="26" height="26" /> Why omnifeed

| | omnifeed | Cloud web MCPs and other Reddit MCPs |
|---|---|---|
| Works on Reddit | ✅ your residential IP + real browser | ❌ datacenter IPs get 403 |
| Search and crawl in one self-hosted service | ✅ SearXNG + crawl4ai | ❌ search-only or crawl-only |
| Full comment tree (`/api/morechildren` expansion) | ✅ up to 40 rounds, about 4k comments | ❌ |
| Token-efficient output | ✅ TOON, about 40% smaller than JSON | ❌ verbose JSON or truncated bodies |
| Generic crawl for non-Reddit URLs | ✅ via crawl4ai | ❌ |
| Front-ends | MCP, Open WebUI, REST | MCP only, mostly |

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Travel%20and%20places/Rocket.png" width="26" height="26" /> Quick start

```bash
# Fetch the compose file and SearXNG settings, then start:
curl -fsSL https://raw.githubusercontent.com/kinorai/omnifeed/main/docker-compose.yml -o docker-compose.yml
curl -fsSL --create-dirs https://raw.githubusercontent.com/kinorai/omnifeed/main/searxng/settings.yml -o searxng/settings.yml
docker compose up
```

This starts omnifeed, SearXNG and crawl4ai, <b>tokenless out of the box</b>, because the compose file sets `OMNIFEED_DEV_NO_AUTH=true`. `searxng/settings.yml` enables the `json` format that `web_search` needs. For Open WebUI, set `WEB_LOADER_ENGINE=external` and point it at `http://localhost:8080`. To require a token, see **Authentication** below.

**On Apple Silicon** you can skip Docker and use Apple's native [`container`](https://github.com/apple/container) runtime. See **[docs/apple-container.md](docs/apple-container.md)**.

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Electric%20Plug.png" width="22" height="22" /> As an MCP server

Works with any MCP client, including **Claude Code, Cursor, Codex, Gemini CLI, OpenCode, Windsurf and Pi**. One endpoint serves the stateless MCP protocol and the older initialize-era revisions. Stateless requests get the spec's HTTP statuses, 400 for header or version violations and 404 for unknown methods. Initialize-era responses stay 200. omnifeed rejects cross-origin browser requests unless [`OMNIFEED_ALLOWED_ORIGINS`](docs/configuration.md) lists them.

**HTTP, recommended.** `docker compose up` already serves MCP on `:8081`. Point your client at it:

```jsonc
{ "mcpServers": { "omnifeed": { "url": "http://localhost:8081/mcp" } } }
```

**Stdio, for clients that speak nothing else.** The client spawns and owns a stdio server, so it can't be a long-running compose service. Launch the compose file's `mcp` profile instead, which reuses its upstreams, network and image:

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

`run -T` disables the TTY so JSON-RPC pipes cleanly. Start the stack first with `docker compose up -d` so the upstreams are healthy.

<details>
<summary><b>Standalone stdio, without the compose stack</b></summary>

Spawn the container and tell it where crawl4ai and SearXNG are. omnifeed exits at startup without `OMNIFEED_CRAWL4AI_URL`.

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

On Linux, add `"--add-host=host.docker.internal:host-gateway"` to the args.
</details>

**`fetch_url`** is always available. **`web_search`** appears only when `OMNIFEED_SEARXNG_URL` is set. The agent calls `web_search`, picks URLs, then calls `fetch_url`.

`/crawl` returns `[{"page_content": "...", "metadata": {...}}]`, the shape of a LangChain or LlamaIndex `Document`, so a custom document loader takes a few lines.

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Locked%20with%20Key.png" width="22" height="22" /> Authentication

The compose stack runs **tokenless** for local use. To require a bearer token, generate one:

```bash
openssl rand -hex 32
```

Set `OMNIFEED_API_KEY` to it in `docker-compose.yml` and remove `OMNIFEED_DEV_NO_AUTH`. Clients send it as `Authorization: Bearer <token>`. With neither variable set, omnifeed **refuses to start**, so it can't be left open by accident. Stdio MCP needs no token. It inherits the trust of the process that spawned it.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Gear.png" width="26" height="26" /> Configuration

omnifeed reads `OMNIFEED_`-prefixed environment variables. You usually **set only three**: `OMNIFEED_API_KEY`, `OMNIFEED_CRAWL4AI_URL` and, for search, `OMNIFEED_SEARXNG_URL`.

**[docs/configuration.md](docs/configuration.md)** lists every variable, plus fetch truncation (`max_chars` and `start_char`), infinite-scroll fetching, Reddit size limits and Prometheus metrics.

**Running more than one replica?** Set `OMNIFEED_REDIS_URL` so the rate limiters share state and the deployment obeys one limit. Without it, N replicas send N times the configured rate, which upstream search engines notice. If Redis goes down, the limiters fall back to per-process pacing and crawls keep working.

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

Reddit 403-blocks non-browser HTTP clients. The Reddit engine drives a **real headless browser** to a `www.reddit.com` page and fetches Reddit's JSON from inside it, with no auth, cookies or API key. Sustained scraping can still raise your IP's risk score, so slow down if fetches return the block page. [Details and tuning](docs/configuration.md#reddit-anti-bot-handling).

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Activities/Puzzle%20Piece.png" width="22" height="22" /> Extending it

Engines, searchers, MCP tools and transports each plug into one small port. **[AGENTS.md, Adding things](AGENTS.md#adding-things)** has the steps.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Hammer%20and%20Wrench.png" width="26" height="26" /> Development

```bash
git clone https://github.com/kinorai/omnifeed.git && cd omnifeed
make check        # vet + lint + test, hermetic: no upstreams or token needed
docker compose up # full stack, tokenless, ports 8080 / 8081 / 9090
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for the workflow and [SECURITY.md](SECURITY.md) to report a vulnerability. Coding agents read [AGENTS.md](AGENTS.md).

Prometheus metrics are on `:9090/metrics`. [docs/configuration.md](docs/configuration.md#prometheus-metrics) lists them.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Hand%20gestures/Handshake.png" width="26" height="26" /> Contributing

<div align="center">

**If omnifeed is useful, star it so others find it.**

[![Star](https://img.shields.io/badge/⭐_Star_omnifeed-FF4500?style=for-the-badge)](https://github.com/kinorai/omnifeed)
[![Open an issue](https://img.shields.io/badge/🐛_Open_an_Issue-161b22?style=for-the-badge)](https://github.com/kinorai/omnifeed/issues/new)
[![Submit a PR](https://img.shields.io/badge/🔧_Submit_a_PR-7C3AED?style=for-the-badge)](https://github.com/kinorai/omnifeed/pulls)

</div>

New engines, searchers, MCP tools and transports are welcome. Start with [AGENTS.md](AGENTS.md#adding-things) and [CONTRIBUTING.md](CONTRIBUTING.md).

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Page%20Facing%20Up.png" width="26" height="26" /> License

[MIT](LICENSE) © kinorai

<img src="https://capsule-render.vercel.app/api?type=waving&color=0:7C3AED,100:FF4500&height=120&section=footer" width="100%"/>
