# AGENTS.md

Guide for coding agents (and humans) working in this repo. Keep changes small, tested, and faithful to the architecture below.

## What this is

`omnifeed` is one Go binary that gives an AI agent **search → URLs → content**: web search via SearXNG and LLM-friendly crawling via crawl4ai, with a dedicated Reddit engine (full comment trees as TOON). It exposes three front-ends over the same core: an MCP server, the Open WebUI loader, and a REST API.

## Commands

The Makefile is the single source of truth — CI and pre-commit run the same targets.

```bash
make check        # vet + lint + test — the gate. Run before every commit.
make test         # go test ./...
make test-cover   # tests with coverage
make lint         # golangci-lint
make fmt          # gofmt + goimports
make tidy         # go mod tidy
make run          # go run ./cmd/omnifeed
make compose-up   # full local stack (omnifeed + searxng + crawl4ai)
make install-tools && make pre-commit-install   # once per clone
```

Tests are hermetic: they use `httptest` fakes, so **no live crawl4ai/SearXNG is needed** to run `make check`.

## Architecture (the dependency rule)

Dependencies point **inward** toward `internal/domain`. Domain defines the ports and imports nothing from the outer layers; adapters and transports depend on domain, never the reverse — and transports never import each other or the engines directly.

Two ports, two questions:

- **`domain.Searcher`** — *query → result URLs* (discovery).
- **`domain.Engine`** — *URL → content* (rendering). Engines are tried in order via `engine.Registry`, with a fallback.

Transports compose these ports and stay thin (pure protocol/HTTP plumbing). `cmd/omnifeed/main.go` is the only place that wires concrete adapters into ports.

```
internal/
  domain/        Ports + core types (Engine, Searcher, Document, SearchResult,
                 EngineOptions, errors). Depends on nothing.
  engine/        Registry + fallback ordering
    reddit/      Engine: drives crawl4ai's browser, TOON comment trees
    crawl4ai/    Engine: generic markdown fallback
  search/searxng/  Searcher adapter (JSON API)
  transport/
    mcp/         MCP server (stdio + Streamable HTTP) — pure JSON-RPC transport
      tools/     fetch_url + web_search tool constructors (bind ports → tools)
    openwebui/   POST /crawl (Open WebUI external-loader contract)
    searchapi/   POST /search (REST mirror of the web_search tool)
  auth/          Authenticator (shared bearer / always-allow)
  httpx/         HTTP client w/ retry, per-domain limiter, URL validation (SSRF)
  observability/ metrics, health (livez/readyz), logging, error classification
  antibot/       block-page / CAPTCHA detection
  config/        all OMNIFEED_* env loading, declared once
  version/       build version
cmd/omnifeed/    entry point + wiring
```

## Adding things

- **New engine**: create `internal/engine/<name>/engine.go` implementing `domain.Engine`; register it in `main.go` **before** the fallback; add a `*_test.go` with a fixture covering URL matching. `internal/engine/hackernews` is a worked example (it and `internal/engine/github` are the engines that fetch their upstream directly rather than through crawl4ai).
- **New searcher** (e.g. Brave): implement `domain.Searcher`; wire in `main.go`.
- **New MCP tool**: add a constructor in `internal/transport/mcp/tools`; append it to `mcpTools` in `main.go`. The MCP server itself never changes.
- **New transport**: create `internal/transport/<name>/server.go` taking the port(s) it needs; mount it from `main.go`. (`searchapi` is the smallest example to copy.)

## Conventions

- **Commits:** [Conventional Commits](https://www.conventionalcommits.org/). `feat:` → minor release, `fix:`/`perf:`/`chore(deps):` → patch, others don't release (git-cliff + goreleaser read history).
- **Tests:** new behavior ships with a test. Prefer table tests and `httptest` fakes over live calls.
- **Config:** every knob is an `OMNIFEED_`-prefixed env var declared in `internal/config/config.go` — add new ones there, with a default, and document them in the README table.
- **Errors/metrics:** classify with `observability.Reason`; record search via `metrics.ObserveSearch`, crawl via `metrics.Observe`.
- Match the surrounding style; don't refactor unrelated code in a feature change.

## Gotchas

- **crawl4ai is mandatory.** `OMNIFEED_CRAWL4AI_URL` must be set or the binary exits at startup — the Reddit engine and the generic fallback both fetch through it. **Exceptions:** the Hacker News engine reads the public Algolia HN API (`hn.algolia.com`) and the GitHub engine reads the public GitHub REST API (`api.github.com`) directly — neither is bot-walled, so a headless browser would only add latency (and lose comments) — and they therefore need outbound access to those two hosts. The Discourse engine does the same against each host listed in `OMNIFEED_DISCOURSE_HOSTS` (public topic JSON), so those hosts need outbound access too.
- **Never call Reddit directly.** Reddit 403-blocks non-browser clients; the Reddit engine fetches through crawl4ai's headless browser. See `internal/engine/reddit`.
- **HTTP transports fail closed.** No `OMNIFEED_API_KEY` → the binary refuses to start, unless `OMNIFEED_DEV_NO_AUTH=true` (local only). Stdio MCP is unauthenticated by design.
- **SSRF:** validate caller-supplied URLs with `httpx.ValidateURL`; `OMNIFEED_BLOCK_PRIVATE_IPS` defaults to on.
- **Search is optional.** `web_search` and `POST /search` are exposed only when `OMNIFEED_SEARXNG_URL` is set.

## Definition of done

1. `make check` is green (vet + lint + test).
2. New behavior has a test.
3. Conventional Commit message.
4. README/docs updated if user-visible behavior changed.
