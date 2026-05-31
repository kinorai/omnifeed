## 0.1.0 (2026-05-31)


### Features

* initial commit ([bd73407](https://github.com/kinorai/crawl4ai-reddit-proxy/commit/bd73407a23b929cf0df3f94e1b29fc05aeda32e2))


### Bug Fixes

* **ci:** migrate .golangci.yml to v2 schema, fix trivy-action tag ([bd38a55](https://github.com/kinorai/crawl4ai-reddit-proxy/commit/bd38a55fa6abdffb2fc4e13e7822ea0fdcfc42e5))
* **ci:** satisfy golangci-lint v2 (godoc, errcheck, deprecations) ([81f669a](https://github.com/kinorai/crawl4ai-reddit-proxy/commit/81f669ae2645d795bc2477e31373a6fb323833fc))

### Security

- MCP HTTP transport (`/mcp`, `/mcp/sse`) is now gated by `CARP_API_KEY` when
  set. Previously only `/crawl` was protected, leaving the MCP endpoint open.
  Clients must send `Authorization: Bearer $CARP_API_KEY` on MCP requests when
  the key is configured. Stdio MCP behavior is unchanged.

### Added

