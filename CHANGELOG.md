# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

This file is automatically updated by the release workflow on `main`. Use
[Conventional Commits](https://www.conventionalcommits.org/) when committing.

## [Unreleased]

### Added

- Initial public release.
- Open WebUI external-loader compatible `/crawl` endpoint.
- Reddit-aware engine: full `/api/morechildren` expansion (up to 40 rounds),
  deleted-comment stripping, TOON encoding (~40% fewer tokens vs JSON).
- crawl4ai upstream fallback for non-Reddit URLs.
- MCP server: stdio + HTTP/SSE transports. Tools: `crawl`, `reddit_get_post`.
- Kubernetes-style health endpoints (`/livez`, `/readyz`, `/healthz`).
- Prometheus metrics on `/metrics`.
- Graceful shutdown on SIGINT/SIGTERM.
- Structured JSON logging via `log/slog`.
- SSRF protection (private-IP filtering) on by default.
- Per-domain rate limiting + identifiable Reddit User-Agent.
- All configuration via `CARP_*` environment variables.
