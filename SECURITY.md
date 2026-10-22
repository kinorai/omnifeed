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

- **SSRF.** By default (`OMNIFEED_BLOCK_PRIVATE_IPS=true`) omnifeed refuses URLs whose host is, or resolves to, an address that is not globally reachable: private, loopback, link-local, shared (100.64.0.0/10), benchmarking, multicast, broadcast, IPv4-mapped and NAT64 IPv6, and the other IANA special-purpose ranges. It also refuses non-canonical IPv4 forms (`0x7f.1`, `2130706433`), `localhost` names, and hosts that do not resolve. Direct fetches re-check every dial and every redirect. Disable it only on a trusted internal network.
- **crawl4ai egress — required.** The headless browser in crawl4ai resolves DNS and follows redirects on its own, so omnifeed's checks cannot stop a redirect or a DNS rebind to an internal address there; omnifeed only refuses to return the page afterwards. **Operators must restrict crawl4ai's egress to public addresses** with a network policy or firewall (deny RFC 1918, loopback, link-local, 100.64.0.0/10 and your cluster's service and pod ranges). Never expose crawl4ai itself: its token holder can run arbitrary JavaScript through `/execute_js`.
- **Auth.** One shared bearer token, `OMNIFEED_API_KEY`, gates `/crawl` and `/mcp`, compared in constant time. Stdio MCP is unauthenticated by design, because it runs as a local subprocess and inherits its parent's trust.
- **Container.** The base is distroless static (`gcr.io/distroless/static-debian13:nonroot`), with no shell or package manager, running as uid 65532. The binary needs no writable paths, so deploy it with a read-only root filesystem and dropped capabilities.
- **TLS.** Terminate it at your reverse proxy or ingress. The binary speaks plain HTTP.
- **Reddit.** The optional Reddit credentials (`OMNIFEED_REDDIT_SESSION`, `OMNIFEED_REDDIT_CLIENT_ID`) are sent only over https to exactly `www.reddit.com` or `oauth.reddit.com`, never to the host a caller's URL names.

## Threat model

| Threat | Mitigation |
|---|---|
| SSRF via a caller-supplied URL | Special-purpose-range filter at request validation, at dial time and on every redirect of direct fetches; an egress policy on crawl4ai (operator) |
| Resource exhaustion via a huge URL list | `OMNIFEED_MAX_URLS_PER_REQUEST` cap, default 30 |
| Resource exhaustion via a large Reddit thread | `OMNIFEED_REDDIT_TIMEOUT` and a 40-round expansion cap |
| Reddit rate-limit blocking | Per-domain limiter, identifiable User-Agent, `Retry-After` honored |
| Unauthorized `/crawl` or `/mcp` access | `OMNIFEED_API_KEY` bearer token, constant-time compare |
| Container escape | Non-root uid 65532, distroless static base with no shell or libc, read-only root FS, dropped capabilities |
