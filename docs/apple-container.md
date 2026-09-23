# omnifeed on Apple `container` (macOS)

On an Apple Silicon Mac with macOS 26 or later, you can run the stack on Apple's
[`container`](https://github.com/apple/container) runtime instead of Docker:

```bash
git clone https://github.com/kinorai/omnifeed.git && cd omnifeed
./scripts/container up        # or: make container-up
```

You get the same stack as `docker compose up`, SearXNG, crawl4ai and omnifeed with auth
off. omnifeed serves `/crawl` and `/search` on `http://localhost:8080`, MCP on
`:8081/mcp`, and health and metrics on `:9090`.

| Command | Does |
|---|---|
| `./scripts/container up` | start the stack (`make container-up`) |
| `./scripts/container down` | stop and remove it (`make container-down`) |
| `./scripts/container status` | show state, IP and health |
| `./scripts/container logs [svc] [-f]` | tail logs for `omnifeed`, `searxng` or `crawl4ai` |
| `./scripts/container exec <svc> [cmd…]` | run a command in one service, `sh` by default |
| `./scripts/container stats` | live CPU and memory of the three containers |
| `./scripts/container restart` | recreate the stack |
| `./scripts/container update` | pull newer images, then recreate |
| `./scripts/container build` | build omnifeed from local source, then start |
| `./scripts/container mcp` | one-shot stdio MCP server for stdio-only clients |

The omnifeed image is distroless with no shell, so `exec omnifeed sh` fails. Use `logs`
or its HTTP endpoints: `:9090/metrics`, `/livez`, `/readyz`.

crawl4ai serves beyond loopback only with a bearer token, and gates in-page JS behind
`POST /execute_js`. So `up` generates a shared token, passes it to crawl4ai as
`CRAWL4AI_API_TOKEN` and to omnifeed as `OMNIFEED_CRAWL4AI_TOKEN`, and enables
`execute_js`.

## Your settings (`.env`)

Everything below is optional. Put any of it in one file instead of your shell:

```bash
cp .env.example .env    # then edit it
```

`up` reads `.env` from the repo root first. It is a list of `KEY=value` lines, not a
shell script. `#` starts a whole-line comment, one layer of surrounding quotes is
stripped, and a `#` after a value stays in the value. An exported shell variable
overrides the file, so `GOOGLE_CSE_CX=… ./scripts/container up` changes one run.
`.gitignore` covers `.env`. `up` reads only `OMNIFEED_*`, `SEARXNG_*`, `CRAWL4AI_*`,
`BRAVEAPI_*` and `GOOGLE_CSE_*` names from it, so a stray line can't rewrite `PATH`.

`scripts/container` reads most of these itself. It passes any `OMNIFEED_*` name it
doesn't recognize to the binary, so the variables in
[configuration.md](configuration.md) work from the same file:

```bash
OMNIFEED_SEARCH_AUDIT=full        # per-search audit log, with each engine's own ranks
OMNIFEED_REDDIT_MAX_COMMENTS=500
```

`up` ignores four names because it derives them from the containers it starts:
`OMNIFEED_CRAWL4AI_URL`, `OMNIFEED_CRAWL4AI_TOKEN`, `OMNIFEED_SEARXNG_URL` and
`OMNIFEED_DEV_NO_AUTH`. Set `OMNIFEED_API_KEY` to turn auth on, because the key wins
over the dev opt-out. `OMNIFEED_LOG_LEVEL` defaults to `info` and `OMNIFEED_LOG_FORMAT`
to `json`, and your values override both.

## A second stack on the same Mac (optional)

`up` names its containers `searxng`, `crawl4ai` and `omnifeed`, and publishes host
ports `8080`, `8081`, `9090` and `11235`. Run it with those defaults from a second
clone and it adopts the running `searxng` and `crawl4ai`, then removes and recreates
the running `omnifeed`. The Docker Compose stack publishes the same four ports, so the
two collide too.

Give the second copy its own name prefix and ports:

```bash
OMNIFEED_PREFIX=dev- \
OMNIFEED_PORT_HTTP=18080 OMNIFEED_PORT_MCP=18081 OMNIFEED_PORT_METRICS=19090 \
OMNIFEED_CRAWL4AI_PORT=21235 \
  ./scripts/container up
```

| Variable | Default | Purpose |
|---|---|---|
| `OMNIFEED_PREFIX` | _(empty)_ | Prefix for all three container names: `dev-` gives `dev-searxng`, and so on |
| `OMNIFEED_PORT_HTTP` | `8080` | Host port for `/crawl` and `/search` |
| `OMNIFEED_PORT_MCP` | `8081` | Host port for MCP HTTP/SSE |
| `OMNIFEED_PORT_METRICS` | `9090` | Host port for `/metrics`, `/livez`, `/readyz` |
| `OMNIFEED_CRAWL4AI_PORT` | `11235` | Host port for crawl4ai, which the `up` health check needs |

Pass the same variables to every later command. `status`, `logs`, `exec`, `stats` and
`down` find containers through `OMNIFEED_PREFIX`. A `.env` per checkout does this, or
export them once per shell. SearXNG needs no host port, because omnifeed reaches it by
container IP.

## Brave API key (optional)

SearXNG can't read engine keys from the environment, so the key must go in its
`settings.yml`. The committed file leaves it empty, so `braveapi` stays inactive. Put
the key in `.env` as `BRAVEAPI_KEY`, or set `BRAVEAPI_KEY_COMMAND` to any command that
prints it, which works with any secret manager:

```bash
export BRAVEAPI_KEY_COMMAND="security find-generic-password -s omnifeed-braveapi -w"  # macOS Keychain
export BRAVEAPI_KEY_COMMAND="bw get password omnifeed-braveapi"                       # Bitwarden
```

`./scripts/container up` then writes `searxng/settings.runtime.yml`, gitignored with
mode `600`, with the key filled in, and mounts it instead. That copy also disables the
`brave` HTML scraper and `startpage`, which the keyed API engine makes redundant.
`BRAVEAPI_KEY` wins over the command. With neither set, `up` injects no key and `brave`
and `startpage` stay on. The key never enters git and is never printed.

## Google Programmable Search engine ID (optional)

`google cse` is on by default. Without a `CX` of its own, it uses the engine ID
hardcoded in `searx/engines/google_cse.py`, which every SearXNG instance shares. Google
rate-limits that ID globally, so the engine suspends itself on most queries. Create your
own [Programmable Search Engine](https://programmablesearchengine.google.com/), set it
to search the whole web, and put its ID in `.env` as `GOOGLE_CSE_CX`, or set
`GOOGLE_CSE_CX_COMMAND` to a command that prints it:

```bash
GOOGLE_CSE_CX=0123456789abcdef0
```

`up` appends a `- name: google cse` override with that ID to the rendered settings
file. The ID needs no API key: the engine reads a token from `www.google.com/cse/cse.js`
and queries the free Search Element tier. It also restores date filtering. `google cse`
maps `time_range` to `sort=date:r:FROM:TO`, and SearXNG skips any engine that can't
honor a requested range.

Your engine keeps SearXNG's default weight, so it ranks by consensus with the rest of
the pool. Set `GOOGLE_CSE_WEIGHT` to rank it higher. SearXNG scores a result as the
product of the weights of the engines that returned it, times the number of positions,
times the sum of `1 / position`. Consensus multiplies, so a weight above about 5 puts
this engine's top hits above agreement between several others. Measure your pool
before raising it.

```bash
GOOGLE_CSE_WEIGHT=3
```

## Where the rendered settings file goes

Both secrets above end up in `searxng/settings.runtime.yml`, inside this working tree.
`.gitignore` and the Docker build context both exclude it, so neither `git add -A` nor
an image build picks it up. If your policy forbids credentials in a git tree at all,
move it:

```bash
SEARXNG_RUNTIME_SETTINGS=~/.config/omnifeed/settings.runtime.yml
```

## Your own engine pool (optional)

Set `SEARXNG_SETTINGS` to your own `settings.yml` to change engines, timeouts or
anything else without forking. The Brave key injection still applies to your file.

```bash
export SEARXNG_SETTINGS=/path/to/my-searxng-settings.yml
```
