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
`down` all resolve the container names through `OMNIFEED_PREFIX`. Exporting them once
per shell is the easy way. Only SearXNG needs no host port: omnifeed reaches it by
container IP, so two stacks never collide there.

These are read by `scripts/container` itself, not by the omnifeed binary — they are
absent from [configuration.md](configuration.md) for that reason.

## Brave API key (optional)

SearXNG can't read engine keys from the environment, so the key has to be inside its
`settings.yml` — the committed one stays empty and `braveapi` inactive. Give `up` the
key either directly with `BRAVEAPI_KEY`, or with `BRAVEAPI_KEY_COMMAND` — any command
that prints it, so any secret manager works:

```bash
export BRAVEAPI_KEY_COMMAND="security find-generic-password -s omnifeed-braveapi -w"  # macOS Keychain
export BRAVEAPI_KEY_COMMAND="bw get password omnifeed-braveapi"                       # Bitwarden
```

`./scripts/container up` then renders `searxng/settings.runtime.yml` (gitignored, mode
`600`) with the key filled in and mounts that instead. It also disables the `brave`
HTML scraper and `startpage` in that rendered copy — both are redundant once the API
engine is keyed. `BRAVEAPI_KEY` takes precedence; with neither set, the committed
keyless file is mounted as before, with the full engine pool intact. Either way the
key never enters git and is never printed.

## Your own engine pool (optional)

Set `SEARXNG_SETTINGS` to the path of your own `settings.yml` to change engines,
timeouts, or anything else without forking this repo. The Brave rendering above still
applies to your file.

```bash
export SEARXNG_SETTINGS=/path/to/my-searxng-settings.yml
```
