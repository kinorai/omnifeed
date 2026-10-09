# omnifeed docs

`omnifeed` lets an AI agent search the web and read the results, on self-hosted
[SearXNG](https://github.com/searxng/searxng) and [crawl4ai](https://github.com/unclecode/crawl4ai).
Its Reddit engine returns full comment trees as [TOON](https://github.com/toon-format/toon).

Install and quick start are in the [README](https://github.com/kinorai/omnifeed#readme).

## Guides

- [Configuration](configuration.md): every `OMNIFEED_` variable, content-size controls, Reddit
  size limits and Prometheus metrics.
- [omnifeed on Apple `container` (macOS)](apple-container.md): run the stack on Apple's native
  container runtime instead of Docker.
- [Rate limits](rate-limits.md): how many searches and fetches the upstream engines and sites
  tolerate, measured per minute, hour, day and month, with the block log and the queries to
  check your own deployment.

## Reference

- [Error codes](errors.md): the stable error codes every front-end reports, how
  failure reasons map onto them, and the MCP `isError` result shape.
- [API reference](api/index.md): Go package docs generated from source.
- [Ideas and parked work](ideas.md): features considered and not built, or built and
  removed, with the measurements, what would justify revisiting each, and the git ref
  that holds the code.
