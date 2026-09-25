# Security policy

## Reporting a vulnerability

**Do not open a public issue.** Open a [private security advisory](https://github.com/kinorai/omnifeed/security/advisories/new) instead, with:

- The vulnerability and its impact
- Steps to reproduce, or a proof of concept
- Affected versions, as image tags or git SHAs
- A suggested mitigation, if you have one

We acknowledge reports within 72 hours. High-severity issues get a fix or a coordinated disclosure timeline within 14 days.

## Supported versions

Only the latest minor release gets security fixes. Pin `:latest` or the highest `:vX.Y.Z` tag.

## Security posture

- **SSRF.** omnifeed blocks requests to RFC 1918, loopback and link-local ranges by default (`OMNIFEED_BLOCK_PRIVATE_IPS=true`). Disable it only on a trusted internal network.
- **Auth.** One shared bearer token, `OMNIFEED_API_KEY`, gates `/crawl` and `/mcp`, compared in constant time. Stdio MCP is unauthenticated by design, because it runs as a local subprocess and inherits its parent's trust.
- **Container.** The base is distroless static (`gcr.io/distroless/static-debian13:nonroot`), with no shell or package manager, running as uid 65532. The binary needs no writable paths, so deploy it with a read-only root filesystem and dropped capabilities.
- **TLS.** Terminate it at your reverse proxy or ingress. The binary speaks plain HTTP.
- **Reddit.** omnifeed uses Reddit's public JSON API and needs no Reddit credentials.

## Threat model

| Threat | Mitigation |
|---|---|
| SSRF via a caller-supplied URL | Private-IP filter at request validation |
| Resource exhaustion via a huge URL list | `OMNIFEED_MAX_URLS_PER_REQUEST` cap, default 30 |
| Resource exhaustion via a large Reddit thread | `OMNIFEED_REDDIT_TIMEOUT` and a 40-round expansion cap |
| Reddit rate-limit blocking | Per-domain limiter, identifiable User-Agent, `Retry-After` honored |
| Unauthorized `/crawl` or `/mcp` access | `OMNIFEED_API_KEY` bearer token, constant-time compare |
| Container escape | Non-root uid 65532, distroless static base with no shell or libc, read-only root FS, dropped capabilities |
