# Security Policy

## Reporting a vulnerability

**Do not open a public GitHub issue for security vulnerabilities.**

Instead, please open a [private security advisory](https://github.com/kinorai/crawl4ai-reddit-proxy/security/advisories/new) on GitHub.

Include:

- A description of the vulnerability and impact
- Steps to reproduce (or a proof-of-concept)
- Affected versions (image tags or git SHAs)
- Suggested mitigation if you have one

You can expect:

- Acknowledgment within 72 hours
- A fix or coordinated disclosure timeline within 14 days for high-severity issues

## Supported versions

Only the latest minor release receives security fixes. Pin to `:latest` or the highest `:vX.Y.Z` tag.

## Security posture

- **SSRF protection**: requests to RFC 1918 / loopback / link-local ranges are blocked by default (`CARP_BLOCK_PRIVATE_IPS=true`). Disable only when running on a trusted internal network.
- **Auth**: a single shared bearer token (`CARP_API_KEY`) gates the `/crawl` endpoint. Constant-time comparison.
- **Container**: distroless/scratch base, runs as non-root (uid 65532), read-only root filesystem in shipped manifests.
- **TLS**: terminated at your reverse proxy / ingress — the binary speaks plain HTTP internally.
- **Reddit auth**: the proxy uses Reddit's *public* JSON API. No user credentials are stored, transmitted, or required.

## Threat model

| Threat | Mitigation |
|---|---|
| SSRF via attacker-supplied URL | Private-IP filter at request validation |
| Resource exhaustion via huge URL list | `CARP_MAX_URLS_PER_REQUEST` cap (default 30) |
| Resource exhaustion via large Reddit thread | `CARP_REDDIT_TIMEOUT` + `MaxExpansionRounds=40` cap |
| Reddit rate-limit blocking | Per-domain limiter + identifiable User-Agent + Retry-After honoring |
| Unauthorized `/crawl` access | `CARP_API_KEY` bearer token (constant-time compare) |
| Container escape | Non-root user, read-only root FS, dropped capabilities, scratch base |
