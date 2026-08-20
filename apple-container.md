# omnifeed on Apple `container` (macOS)

On an Apple-Silicon Mac you can skip Docker entirely and run the stack on Apple's
[`container`](https://github.com/apple/container) runtime (macOS 26+):

```bash
git clone https://github.com/kinorai/omnifeed.git && cd omnifeed
./scripts/container up        # or: make container-up
```

Same result as `docker compose up` — SearXNG + crawl4ai + omnifeed, **tokenless out
of the box** — with omnifeed on `http://localhost:8080` (`/crawl`, `/search`), MCP on
`:8081/mcp`, and health + metrics on `:9090`.

| Command | Does |
|---|---|
| `./scripts/container up` | start the stack (`make container-up`) |
| `./scripts/container down` | stop + remove it (`make container-down`) |
| `./scripts/container status` | state / IP / health at a glance |
| `./scripts/container logs [svc] [-f]` | tail logs (`omnifeed` \| `searxng` \| `crawl4ai`) |
| `./scripts/container exec <svc> [cmd…]` | run a command in one service (default `sh`) |
| `./scripts/container stats` | live CPU / memory of the three containers |
| `./scripts/container restart` | recreate the stack |
| `./scripts/container update` | pull newer images, then recreate |
| `./scripts/container build` | build omnifeed from local source, then start |
| `./scripts/container mcp` | one-shot stdio MCP server (stdio-only clients) |

The omnifeed image is distroless and ships no shell, so `exec omnifeed sh` fails by
design — read its HTTP surface (`:9090/metrics`, `/livez`, `/readyz`) or use `logs`.

crawl4ai requires a bearer token to serve beyond loopback and gates in-page JS behind
`POST /execute_js`, so `up` generates a shared token, wires it to crawl4ai
(`CRAWL4AI_API_TOKEN`) and omnifeed (`OMNIFEED_CRAWL4AI_TOKEN`), and enables
`execute_js` automatically.

## Your settings (`.env`)

Everything below is optional, and every variable can go in one file instead of your
shell:

```bash
cp .env.example .env    # then edit it
```

`up` reads `.env` from the repo root before anything else. It is a plain list of
`KEY=value` lines, not a shell script: `#` starts a whole-line comment, one layer of
surrounding quotes is stripped, and a `#` after a value stays part of the value. An
exported shell variable wins over the file, so `GOOGLE_CSE_CX=… ./scripts/container up`
still overrides it for one run. `.gitignore` covers `.env`, and only `OMNIFEED_*`,
`SEARXNG_*`, `CRAWL4AI_*`, `BRAVEAPI_*` and `GOOGLE_CSE_*` names are read from it, so a
stray line cannot rewrite `PATH`.

Most of these are read by `scripts/container` itself. Any `OMNIFEED_*` name it does not
recognise is passed through to the binary, so the knobs in
[configuration.md](configuration.md) work from the same file:

```bash
OMNIFEED_SEARCH_AUDIT=full        # per-search audit log, with each engine's own ranks
OMNIFEED_REDDIT_MAX_COMMENTS=500
```

Four names are ignored there, because `up` derives them from the containers it starts:
`OMNIFEED_CRAWL4AI_URL`, `OMNIFEED_CRAWL4AI_TOKEN`, `OMNIFEED_SEARXNG_URL`, and
`OMNIFEED_DEV_NO_AUTH`. Set `OMNIFEED_API_KEY` to turn auth on: the binary prefers the
key over the dev opt-out. `OMNIFEED_LOG_LEVEL` defaults to `info` and
`OMNIFEED_LOG_FORMAT` to `json`, and yours win if you set them.

## A second stack on the same Mac (optional)

`up` names its containers `searxng`, `crawl4ai` and `omnifeed`, and publishes host
ports `8080`, `8081`, `9090` and `11235`. Run it from a second clone with those
defaults and it does **not** start a second stack — it adopts the running `searxng`
and `crawl4ai`, and removes and recreates the running `omnifeed`. The same host ports
also collide with the Docker Compose stack, which publishes exactly the same four.

Give the second copy its own name prefix and ports:

```bash
OMNIFEED_PREFIX=dev- \
OMNIFEED_PORT_HTTP=18080 OMNIFEED_PORT_MCP=18081 OMNIFEED_PORT_METRICS=19090 \
OMNIFEED_CRAWL4AI_PORT=21235 \
  ./scripts/container up
```

| Variable | Default | Purpose |
|---|---|---|
| `OMNIFEED_PREFIX` | _(empty)_ | Prepended to all three container names (`dev-` → `dev-searxng`, …) |
| `OMNIFEED_PORT_HTTP` | `8080` | Host port for `/crawl` + `/search` |
| `OMNIFEED_PORT_MCP` | `8081` | Host port for MCP HTTP/SSE |
| `OMNIFEED_PORT_METRICS` | `9090` | Host port for `/metrics`, `/livez`, `/readyz` |
| `OMNIFEED_CRAWL4AI_PORT` | `11235` | Host port for crawl4ai (the `up` health-wait needs it) |

Pass the same variables to every later verb — `status`, `logs`, `exec`, `stats`,
`down` all resolve the container names through `OMNIFEED_PREFIX`. A `.env` per checkout
is the easy way; exporting them once per shell also works. Only SearXNG needs no host
port: omnifeed reaches it by container IP, so two stacks never collide there.

## Brave API key (optional)

SearXNG can't read engine keys from the environment, so the key has to be inside its
`settings.yml` — the committed one stays empty and `braveapi` inactive. Put the key in
your `.env` as `BRAVEAPI_KEY`, or use `BRAVEAPI_KEY_COMMAND` — any command that prints
it, so any secret manager works:

```bash
export BRAVEAPI_KEY_COMMAND="security find-generic-password -s omnifeed-braveapi -w"  # macOS Keychain
export BRAVEAPI_KEY_COMMAND="bw get password omnifeed-braveapi"                       # Bitwarden
```

`./scripts/container up` then renders `searxng/settings.runtime.yml` (gitignored, mode
`600`) with the key filled in and mounts that instead. It also disables the `brave`
HTML scraper and `startpage` in that rendered copy — both are redundant once the API
engine is keyed. `BRAVEAPI_KEY` takes precedence; with neither set, no key is injected
and `brave` and `startpage` stay enabled. Either way the key never enters git and is
never printed.

## Google Programmable Search engine id (optional)

`google cse` is enabled by default, but with no `CX` of its own it queries the one
engine id hardcoded in `searx/engines/google_cse.py`. Every SearXNG instance on the
internet shares that id, so Google rate-limits it globally and the engine self-suspends
on most queries. Create your own [Programmable Search
Engine](https://programmablesearchengine.google.com/), set it to search the whole web,
and put its id in your `.env` as `GOOGLE_CSE_CX` — or `GOOGLE_CSE_CX_COMMAND` for any
command that prints it:

```bash
GOOGLE_CSE_CX=0123456789abcdef0
```

`up` appends a `- name: google cse` override to the rendered settings file carrying that
id. The id needs no API key: the engine reads a token from `www.google.com/cse/cse.js`
and queries the free Search Element tier. It also restores date filtering — `google cse`
maps `time_range` onto `sort=date:r:FROM:TO`, and SearXNG skips any engine that cannot
honour a requested range.

Your own engine keeps SearXNG's default weight, so it ranks by consensus with the rest
of the pool. Set `GOOGLE_CSE_WEIGHT` to rank it higher. A result's score is the product
of the weights of the engines that returned it, times the number of positions, times the
sum of `1 / position` — so consensus multiplies, and a weight above ~5 puts this engine's
top hits above agreement between several others. Measure your own pool before you raise
it.

```bash
GOOGLE_CSE_WEIGHT=3
```

## Where the rendered settings file goes

Both values above end up inside `searxng/settings.runtime.yml`, which is gitignored and
mode `600` but still sits in this working tree. `.gitignore` covers it and the Docker
build context excludes it, so neither a `git add -A` nor an image build can carry it out.
If your policy forbids credentials in a git tree at all, move the rendered file:

```bash
SEARXNG_RUNTIME_SETTINGS=~/.config/omnifeed/settings.runtime.yml
```

## Your own engine pool (optional)

Set `SEARXNG_SETTINGS` to the path of your own `settings.yml` to change engines,
timeouts, or anything else without forking this repo. The Brave rendering above still
applies to your file.

```bash
export SEARXNG_SETTINGS=/path/to/my-searxng-settings.yml
```
