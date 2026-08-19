# omnifeed docs

`omnifeed` gives an AI agent the full research loop — **search → URLs → content** — against
self-hosted [SearXNG](https://github.com/searxng/searxng) + [crawl4ai](https://github.com/unclecode/crawl4ai),
with a dedicated Reddit engine returning full comment trees as
[TOON](https://github.com/toon-format/toon).

Start at the [README](https://github.com/kinorai/omnifeed#readme) for install and quickstart.

## Guides

- [Configuration](configuration.md) — every `OMNIFEED_` variable, content-size controls, Reddit
  size knobs, and the Prometheus metrics table.
- [omnifeed on Apple `container` (macOS)](apple-container.md) — run the stack on Apple's native
  container runtime instead of Docker.

## Reference

- [API reference](api/index.md) — Go package docs generated from source.
- [Ideas and parked work](ideas.md) — features considered and not built, or built
  and removed: the measurements behind each call, what would justify revisiting
  it, and the git ref that still holds the code.
