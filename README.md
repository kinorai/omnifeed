<!-- markdownlint-disable MD033 MD041 -->
<p align="center">
  <img src="https://capsule-render.vercel.app/api?type=waving&color=0:FF4500,100:7C3AED&height=220&section=header&text=omnifeed&fontSize=82&fontColor=ffffff&animation=fadeIn&fontAlignY=36" alt="omnifeed" width="100%"/>
</p>

<p align="center">
  <img src="https://readme-typing-svg.demolab.com?font=Fira+Code&weight=700&size=28&color=FF4500&center=true&vCenter=true&multiline=true&repeat=false&duration=1500&pause=500&width=860&height=110&lines=Self-hosted+web+search+%2B+LLM-friendly+crawling;with+dedicated+Reddit+and+Hacker+News+engines" alt="Self-hosted web search + LLM-friendly crawling, with dedicated Reddit and Hacker News engines"/>
</p>

<p align="center">
  <a href="https://github.com/kinorai/omnifeed/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/kinorai/omnifeed/ci.yml?branch=main&label=CI&style=flat-square" alt="CI"/></a>
  <a href="https://github.com/kinorai/omnifeed/releases"><img src="https://img.shields.io/github/v/release/kinorai/omnifeed?style=flat-square&color=FF4500" alt="Release"/></a>
  <a href="https://hub.docker.com/r/kinorai/omnifeed"><img src="https://img.shields.io/docker/pulls/kinorai/omnifeed?style=flat-square&logo=docker&logoColor=white&color=2496ED" alt="Docker pulls"/></a>
</p>

<p align="center">
omnifeed gives an AI agent the full research loop — <b>search → URLs → content</b> — against self-hosted
<a href="https://github.com/searxng/searxng">SearXNG</a> + <a href="https://github.com/unclecode/crawl4ai">crawl4ai</a>,
with a dedicated Reddit engine that returns <b>full comment trees as <a href="https://github.com/toon-format/toon">TOON</a></b>
(~40% fewer tokens than JSON, lossless) and <b>no Reddit API key</b>.
</p>

- **`web_search`** queries a SearXNG instance (Google/Bing/DDG, Reddit included) and returns ranked URLs with titles and snippets.
- **`fetch_url`** renders any URL through crawl4ai as clean markdown — and Reddit URLs (threads *and* `/r/{sub}` listings) come back as TOON, as do Hacker News threads and front-page / Ask / Show feeds (read directly from the Algolia HN API) and GitHub issue / pull-request pages (read directly from the GitHub REST API) and Discourse forum topics on the hosts you list (read directly from the public topic JSON API).

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
  <img src="assets/demo.gif" alt="omnifeed demo — web_search then fetch_url returning a Reddit thread as TOON" width="100%"/>
</p>

<p align="center"><sub><code>web_search</code> → pick a URL → <code>fetch_url</code> → full Reddit comment tree as TOON.</sub></p>

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Travel%20and%20places/Rocket.png" width="26" height="26" /> Quick start

```bash
# Fetch the compose file + SearXNG settings, then start:
curl -fsSL https://raw.githubusercontent.com/kinorai/omnifeed/main/docker-compose.yml -o docker-compose.yml
curl -fsSL --create-dirs https://raw.githubusercontent.com/kinorai/omnifeed/main/searxng/settings.yml -o searxng/settings.yml
docker compose up
```

Starts omnifeed + SearXNG + crawl4ai — **tokenless out of the box** (the compose file sets `OMNIFEED_DEV_NO_AUTH=true`), so `docker compose up` just works. Point Open WebUI at `http://localhost:8080` with `WEB_LOADER_ENGINE=external`. (SearXNG is mounted with `searxng/settings.yml`, which enables the `json` format `web_search` needs.) See **Authentication** below to require a bearer token.

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Travel%20and%20places/Desktop%20Computer.png" width="22" height="22" /> On Apple Silicon — native `container`, no Docker

On an Apple-Silicon Mac you can skip Docker entirely and run the stack on Apple's [`container`](https://github.com/apple/container) runtime (macOS 26+):

```bash
git clone https://github.com/kinorai/omnifeed.git && cd omnifeed
./scripts/container up        # or: make container-up
```

Same result as `docker compose up` — SearXNG + crawl4ai + omnifeed, **tokenless out of the box** — with omnifeed on `http://localhost:8080` (`/crawl`, `/search`), MCP on `:8081/mcp`, and health + metrics on `:9090`.

| Command | Does |
|---|---|
| `./scripts/container up` | start the stack (`make container-up`) |
| `./scripts/container down` | stop + remove it (`make container-down`) |
| `./scripts/container status` | state / IP / health at a glance |
| `./scripts/container logs [svc] [-f]` | tail logs (`omnifeed` \| `searxng` \| `crawl4ai`) |
| `./scripts/container restart` | recreate the stack |
| `./scripts/container update` | pull newer images, then recreate |
| `./scripts/container build` | build omnifeed from local source, then start |
| `./scripts/container mcp` | one-shot stdio MCP server (stdio-only clients) |

All three images track `latest`; crawl4ai `0.9.x` needs a token and routes Reddit through `POST /execute_js`, so `up` generates a shared token, wires it to crawl4ai (`CRAWL4AI_API_TOKEN`) and omnifeed (`OMNIFEED_CRAWL4AI_TOKEN`), and enables `execute_js` automatically.

**Brave API key (optional).** SearXNG can't read engine keys from the environment, so the key has to be inside its `settings.yml` — the committed one stays empty and `braveapi` inactive. Give `up` the key either directly with `BRAVEAPI_KEY`, or with `BRAVEAPI_KEY_COMMAND` — any command that prints it, so any secret manager works:

```bash
export BRAVEAPI_KEY_COMMAND="security find-generic-password -s omnifeed-braveapi -w"  # macOS Keychain
export BRAVEAPI_KEY_COMMAND="bw get password omnifeed-braveapi"                       # Bitwarden
```

`./scripts/container up` then renders `searxng/settings.runtime.yml` (gitignored, mode `600`) with the key filled in and mounts that instead. It also disables the `brave` HTML scraper and `startpage` in that rendered copy — both are redundant once the API engine is keyed. `BRAVEAPI_KEY` takes precedence; with neither set, the committed keyless file is mounted as before, with the full engine pool intact. Either way the key never enters git and is never printed.

**Your own engine pool (optional).** Set `SEARXNG_SETTINGS` to the path of your own `settings.yml` to change engines, timeouts, or anything else without forking this repo. The Brave rendering above still applies to your file.

```bash
export SEARXNG_SETTINGS=/path/to/my-searxng-settings.yml
```

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Electric%20Plug.png" width="22" height="22" /> As an MCP server

Works with any MCP client — **Claude Code, Cursor, Codex, Gemini CLI, OpenCode, Windsurf, Pi**, and more.

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

| Variable | Default | Purpose |
|---|---|---|
| `OMNIFEED_API_KEY` | _(unset)_ | Bearer token for `/crawl`, `/search`, `/mcp`. If unset, the proxy refuses to start unless `OMNIFEED_DEV_NO_AUTH=true`. Stdio MCP is unaffected. |
| `OMNIFEED_CRAWL4AI_URL` | _(required)_ | Upstream crawl4ai endpoint. Reddit + the generic fallback fetch through it (the Hacker News engine reads `hn.algolia.com` directly); if empty, the proxy exits at startup. |
| `OMNIFEED_CRAWL4AI_TOKEN` | _(unset)_ | Bearer token sent to crawl4ai (its `CRAWL4AI_API_TOKEN`). Needed when the upstream enforces auth — crawl4ai `0.9.x` binds non-loopback **only** when a token is set. Unset sends no `Authorization` header. |
| `OMNIFEED_SEARXNG_URL` | _(unset)_ | Upstream SearXNG base URL (e.g. `http://searxng:8080`). When unset, `web_search` / `/search` are not exposed. The instance must enable the `json` format. |
| `OMNIFEED_DEV_NO_AUTH` | `false` | Run the HTTP transports with **no** auth when no key is set (local/dev only). Ignored if a key is set. |
| `OMNIFEED_LISTEN_ADDR` | `:8080` | HTTP listen address (`/crawl`, `/search`) |
| `OMNIFEED_MCP_LISTEN_ADDR` | `:8081` | MCP HTTP/SSE listen address |
| `OMNIFEED_MCP_STDIO` | `false` | Run MCP over stdio (also via `--mcp-stdio`) |
| `OMNIFEED_METRICS_ADDR` | `:9090` | Prometheus + health listen address |
| `OMNIFEED_CRAWL4AI_TIMEOUT` | `90s` | Per-call timeout to crawl4ai |
| `OMNIFEED_CRAWL4AI_KEEP_LINKS` | `true` | Keep hyperlink anchor text + external links in fetched markdown. Set `false` for leaner, link-stripped output (loses link-dense content like HN titles). |
| `OMNIFEED_CRAWL4AI_PRUNE_THRESHOLD` | `0.48` | PruningContentFilter cutoff (0–1) for the generic engine. Raise it to strip more boilerplate/duplicated chrome from noisy pages; lower it to keep more. |
| `OMNIFEED_CRAWL4AI_WAIT_UNTIL` | `domcontentloaded` | crawl4ai page-ready signal (`domcontentloaded` \| `load` \| `networkidle` \| `commit`). `domcontentloaded` fires before client-side frameworks hydrate, so JS-only SPAs can render empty; set `networkidle` to wait for them (slower on every page). |
| `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE` | `false` | Deployment default for scrolling the full page before extraction (crawl4ai `scan_full_page`). Benchmarked: it buys content **only** on append-style infinite-scroll feeds, costs ~3× latency on every page, and corrupts virtualized pages (an open crawl4ai bug) — prefer the per-request opt-in: the `fetch_url` tool's `scan_full_page` argument or `POST /crawl?scan_full_page=true`, which override this in either direction. |
| `OMNIFEED_CRAWL4AI_SCROLL_DELAY` | `0.5` | Pause (seconds) between scroll steps while scanning the full page. Only sent when the scan is on. |
| `OMNIFEED_CRAWL4AI_REMOVE_OVERLAYS` | `false` | Send crawl4ai's `remove_overlay_elements`. **Leave off**: its geometry heuristic deletes any large fixed/absolute-position element before extraction, which silently empties entire pages whose content lives in such containers (Wikipedia and several news fronts return only their `<title>`). Cookie/consent modals are already handled by `remove_consent_popups`, which stays on regardless. |
| `OMNIFEED_CRAWL4AI_DELAY_BEFORE_HTML` | `0.1` | Unconditional settle (seconds) after the page-ready signal before HTML extraction (crawl4ai `delay_before_return_html`) — paid on **every** crawl. Raise it (e.g. `1.0`) if pages that render content shortly after load come back thin. |
| `OMNIFEED_CRAWL4AI_EXCLUDED_SELECTOR` | `.sidebar,.toc,#toc,.related,.newsletter,.cookie-banner,[aria-label*='cookie']` | CSS selectors the generic engine drops before extraction (crawl4ai `excluded_selector`). The default names chrome-shaped classes only — sidebars, tables of contents, related/newsletter boxes, cookie banners — so it can't eat article body text. Set your own list to replace it (empty keeps the default; a selector that matches nothing effectively excludes nothing). If the exclusion empties a page (its main content *was* a `#toc`/`.sidebar`), the crawl automatically retries once without the selector. |
| `OMNIFEED_CRAWL4AI_TARGET_ELEMENTS` | _(unset — feature off)_ | Comma-separated CSS selectors; when non-empty the generic engine extracts markdown **only** from matching containers (crawl4ai `target_elements`). Powerful on article/repo pages but risky: a page without a match returns **empty content**, and the thin-content guard then surfaces an explicit error instead of the page. Validate against your own corpus before enabling. Suggested starting list: `article, main, [role=main], .markdown-body, .post-content, #content`. |
| `OMNIFEED_SEARXNG_TIMEOUT` | `15s` | Per-query timeout to SearXNG |
| `OMNIFEED_SEARCH_MAX_RESULTS` | `25` | Hard cap on the search `limit` argument (1–100) |
| `OMNIFEED_FETCH_MAX_CHARS` | `120000` | Default character cap on **markdown** content returned by the `fetch_url` MCP tool (`0` = unlimited). Over the cap, the reply ends with a resumable truncation marker. TOON/JSON output (Reddit, Hacker News) is never cut — use the Reddit knobs below instead. Does not apply to `/crawl` (see [Controlling fetched content size](#controlling-fetched-content-size)). |
| `OMNIFEED_GITHUB_TOKEN` | _(unset)_ | Personal access token for the GitHub engine (issue / pull-request pages read from `api.github.com`). Unset means anonymous, which works but is limited to 60 requests/hour/IP; a token raises it to 5000/hour. |
| `OMNIFEED_DISCOURSE_HOSTS` | `meta.discourse.org,discuss.python.org,users.rust-lang.org,internals.rust-lang.org,discuss.pytorch.org` | Comma-separated hostnames the Discourse engine claims topic (`/t/…`) URLs on. Discourse is self-hosted software living on **arbitrary** domains and an engine cannot probe a host to find out, so the allowlist is explicit: **list the forums you actually use**. Matching is exact and case-insensitive (no subdomain wildcards); unlisted forums still work — they just go through the generic browser fallback, which returns less of the thread. Set to the **empty string** to disable the engine entirely. |
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

### Controlling fetched content size

A long article can overflow an MCP client's per-response budget, at which point the client spools the text to a file and the model never reads it. So `fetch_url` caps **markdown** content at `OMNIFEED_FETCH_MAX_CHARS` (120000 by default, `0` = unlimited) and, when it cuts, ends the reply with a resumable marker:

```
[omnifeed: content truncated at 120000 of 174233 characters. Call fetch_url again with start_char=120000 to continue.]
```

Both knobs are per-request on the tool, and both are **markdown-only**:

| Param | Default | Purpose |
|---|---|---|
| `max_chars` | `OMNIFEED_FETCH_MAX_CHARS` | Max characters to return, up to `500000`. `0`/omitted uses the server default. |
| `start_char` | `0` | Character offset to start from — pass the value from a truncation marker to read the next chunk. |

Offsets and lengths count **characters, not bytes**, so a window never splits a multibyte character, and the marker counts against `max_chars`, so a reply never exceeds the requested ceiling. The same numbers come back in the response `_meta` as `truncated`, `total_chars`, and `next_start_char`. `fetch_url` also declares `anthropic/maxResultSizeChars: 500000` in `tools/list`, which raises Claude Code's per-tool text budget to match the ceiling; other clients ignore it.

**TOON and JSON output is never truncated.** Its length markers would describe rows that are no longer there, so structured engines are bounded by their own element caps instead — see the Reddit knobs below.

`POST /crawl` has **no size params and is always unlimited**: RAG pipelines chunk and embed whatever they receive, so a cap there would silently drop retrievable text rather than protect a context window (and one shared offset would corrupt every other URL in a batch).

### Fetching infinite-scroll feeds

Generic crawls do **not** scroll by default — the scroll costs seconds on every page and gains content only on append-style infinite feeds (and actively corrupts virtualized pages). When a feed/listing/gallery URL clearly came back missing items, opt in per request: the `fetch_url` tool's `scan_full_page: true` argument, or `POST /crawl?scan_full_page=true` on the loader. Both are tri-state (`false` forces the scroll off where a deployment enables `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE` globally); `OMNIFEED_CRAWL4AI_SCROLL_DELAY` paces the scroll steps.

### Controlling Reddit response size

A Reddit thread's comment tree can be huge. The size knobs come in two kinds — it matters which is which:

- **Upstream Reddit params** — forwarded verbatim to Reddit's API, so Reddit owns their behavior: `OMNIFEED_REDDIT_FETCH_LIMIT` → `limit`, `OMNIFEED_REDDIT_DEPTH` → `depth`, `OMNIFEED_REDDIT_SORT` → `sort`. They shape *what Reddit sends back* (less latency, fewer tokens) but are **approximate**, and `limit`/`depth` bound only the **initial** fetch. Semantics are Reddit's, not ours — see <https://www.reddit.com/dev/api/> → `GET [/r/subreddit]/comments/article` (`limit` = "maximum number of comments to return", `depth` = "maximum depth of subtrees").
- **omnifeed engine caps** — our own, applied *after* fetch + expansion, so they're **exact and independent of Reddit**: `OMNIFEED_REDDIT_MAX_COMMENTS` (truncate the flat comment list) and `OMNIFEED_REDDIT_MAX_TOP_LEVEL` (keep the first N top-level threads, in `sort` order, with their replies).

Rule of thumb: reach for the **upstream params** to fetch less from Reddit; reach for the **engine caps** when you need a guaranteed ceiling — `OMNIFEED_REDDIT_MAX_ROUNDS` expansion adds comments on top of `limit`, so only the caps bound the final total. All five are also per-request on the `fetch_url` MCP tool (`limit`, `depth`, `sort`, `max_comments`, `max_top_level`); a positive value overrides the env default.

<img src="https://user-images.githubusercontent.com/74038190/212284100-561aa473-3905-4a80-b561-0d28506553ee.gif" width="100%">

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Travel%20and%20places/Building%20Construction.png" width="26" height="26" /> Architecture

Two ports answer two questions. The `Searcher` port answers *query → URLs*; the `Engine` port answers *URL → content*. MCP tools and REST handlers compose them; transports stay thin.

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

Reddit's edge 403-blocks non-browser HTTP clients (it fingerprints the TLS/JA3 handshake), so the Reddit engine never calls Reddit directly. It drives a **real headless browser** to a `www.reddit.com` page (which clears the bot wall), then runs a **same-origin `fetch()`** of the `.json` and `/api/morechildren` endpoints from inside that page. No Reddit auth, cookies, or API key. By default the browser is crawl4ai, reached through its token-gated **`POST /execute_js`** endpoint (crawl4ai `0.9.x` rejects caller `js_code` on `/crawl`), so the upstream must run with `CRAWL4AI_EXECUTE_JS_ENABLED=true`.

> Sustained scraping can raise your source IP's risk score. If fetches start returning the block page, slow down, keep `expand` modest, or route the browser through a residential proxy.

### <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Travel%20and%20places/High%20Voltage.png" width="22" height="22" /> Raw-text bypass

Non-HTML text — raw code files, JSON, markdown, plain text — has nothing for a browser to render, and Chromium's page-idle machinery makes such fetches pathologically slow (a raw `githubusercontent.com` file: 30–39 s in the browser vs ~200 ms direct). When a URL's path extension looks raw (`.md`, `.txt`, `.json`, source files, …), the generic engine spends a cheap HEAD probe: if the server confirms a non-HTML text content type, the body is fetched with a plain GET and returned as-is; anything uncertain (probe failure, `text/html`, binary bytes, restricted egress) silently takes the normal browser path. Direct fetches refuse private/reserved addresses **at dial time** when `OMNIFEED_BLOCK_PRIVATE_IPS` is on, so a DNS-rebinding race can't slip past the URL validation. Note the pod/host then needs outbound access to the target sites themselves, not just to crawl4ai — without it the probe fails and everything still flows through crawl4ai as before.

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

### Prometheus metrics

Served on `OMNIFEED_METRICS_ADDR` (default `:9090`) at `/metrics`, alongside the Go/process collectors:

| Metric | Type | Labels | What it measures |
|---|---|---|---|
| `omnifeed_requests_total` | counter | `engine, tenant, status, reason` | Crawl requests, with a bounded failure `reason` (`ok` on success) |
| `omnifeed_request_seconds` | histogram | `engine, status, reason` | End-to-end crawl latency |
| `omnifeed_request_attempts_total` | counter | `upstream, attempt` | HTTP attempts by the retrying client (`first` vs `retry`) |
| `omnifeed_upstream_seconds` | histogram | `upstream, op, status` | Upstream HTTP round-trip per attempt (start → body fully read); `crawl4ai/crawl`, `crawl4ai/execute_js`, `searxng/search`, `github/api`, `hackernews/api`, `discourse/api` |
| `omnifeed_domain_limiter_wait_seconds` | histogram | `engine, outcome` | Time blocked in per-domain limiter acquisition (semaphore + politeness delay); `outcome="canceled"` = the wait died in the queue |
| `omnifeed_response_chars` | histogram | `engine` | Extracted content length (pre-truncation — the engine's output, before any transport `max_chars` clipping), successful crawls only |
| `omnifeed_engine_fallbacks_total` | counter | `from_engine, reason` | Dedicated-engine failures re-crawled via the generic fallback |
| `omnifeed_searxng_unresponsive_engines_total` | counter | `engine, error` | Engines SearXNG reported unresponsive per search; `error` normalized to a closed set (`timeout`, `captcha`, `suspended`, `too_many_requests`, `access_denied`, `error`, `unknown`) |
| `omnifeed_reddit_expansion_rounds` | histogram | — | `/api/morechildren` rounds per Reddit crawl |
| `omnifeed_search_requests_total` | counter | `searcher, status, reason` | Search queries |
| `omnifeed_search_request_seconds` | histogram | `searcher, status` | Search latency |

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

[![Star](https://img.shields.io/badge/⭐_Star_omnifeed-FF4500?style=for-the-badge)](https://github.com/kinorai/omnifeed)
[![Open an issue](https://img.shields.io/badge/🐛_Open_an_Issue-161b22?style=for-the-badge)](https://github.com/kinorai/omnifeed/issues/new)
[![Submit a PR](https://img.shields.io/badge/🔧_Submit_a_PR-7C3AED?style=for-the-badge)](https://github.com/kinorai/omnifeed/pulls)

</div>

New engines, searchers, MCP tools, and transports are all welcome — start with [AGENTS.md](AGENTS.md#adding-things) and [CONTRIBUTING.md](CONTRIBUTING.md).

## <img src="https://raw.githubusercontent.com/Tarikul-Islam-Anik/Animated-Fluent-Emojis/master/Emojis/Objects/Page%20Facing%20Up.png" width="26" height="26" /> License

[MIT](LICENSE) © kinorai

<img src="https://capsule-render.vercel.app/api?type=waving&color=0:7C3AED,100:FF4500&height=120&section=footer" width="100%"/>
