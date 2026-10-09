# omnifeed vs Claude Code's built-in WebSearch / WebFetch

A head-to-head benchmark: the same **272 sites** and **40 search queries**, each
hit once by omnifeed's `fetch_url` / `web_search` and once by Claude Code's
built-in `WebFetch` / `WebSearch`, from the same connection. Each version
re-runs the full set, so a version-to-version diff shows what a release fixed
or broke.

Open [`index.html`](index.html) through any static server
(`python3 -m http.server` in this directory): the pages load `data.json` with
`fetch()`, which browsers refuse over `file://`.

| Version | Date | omnifeed | What it found |
| --- | --- | --- | --- |
| [v2](v2/) | 2026-10-09 | v0.35.0 | The v1 fixes hold; the new fetch-time cost of challenge waits; a prose floor that rejects genuinely tiny pages; the search and fetch rate limits ([docs/rate-limits.md](../../docs/rate-limits.md)) |
| [v1](v1/) | 2026-10-08 | v0.34.2 | Silent failures returned as success, a stale Chrome 116 fingerprint, crashes on large pages, link-only content dropped |

Each version directory is self-contained: `data.json` holds every measurement,
`stats.js` computes the shared numbers, and the HTML pages are different views
of the same data (a dashboard, a one-page brief, a story, and some sillier
ones).

## Method

- **Sequential, one call at a time.** Every fetch and search runs alone, so
  latency is never the sum of parallel calls. Latency is the gap between the
  `tool_use` and `tool_result` timestamps in the Claude Code session
  transcript, so it includes the MCP round trip for omnifeed and the
  summarization step for WebFetch.
- **Same inputs as v1.** omnifeed gets the v1 arguments (`max_chars` 800 on
  half the sites, 1,500 to 10,000 on the rest, `max_comments` on threads);
  WebFetch gets a short prompt asking for two or three facts plus "is this a
  block page?".
- **Graded by hand.** *Success*: the requested facts came back. *Partial*: a
  real page came back but a requested fact did not. *Soft-block*: a block,
  error or empty page returned as success. *Blocked*: an explicit block or
  error. *Refused*: Claude Code declined the domain without fetching it.
- **Paced (v2).** About two fetches a minute and one search every 3 minutes,
  then every 6 minutes once Google's engine went silent, to keep rate limits
  out of the comparison. The limits themselves are measured separately in
  [docs/rate-limits.md](../../docs/rate-limits.md).
- **Diff rules (v2).** A site counts as *fixed* or *regressed* only when its
  usable / not-usable status flips, a silent failure becomes an explicit error
  (or back), or the v2 note records an observed change. A success ↔ partial
  difference on an unchanged page is shown as a *regrade*, not a change.

## Caveats

- Two v2 calls ran in parallel by mistake (search S1 and one fetch); their
  notes flag the latency as contaminated.
- One run per version, one connection, one day each. Sites change between
  runs: a few v2 differences are the site, not omnifeed (a stale product ID
  that now redirects, a Substack page that now shows a subscribe interstitial).
- The two tools egress from the same IP, so a site or search engine that
  flags it flags both (Google Scholar did during v2).
- The built-in WebFetch answers through a small summarization model, so its
  grade includes that model's reading. omnifeed returns the page itself.

## Rate-limit scripts

[`limits/`](limits/) holds the scripts behind
[docs/rate-limits.md](../../docs/rate-limits.md). `export.sh` pulls omnifeed's
logs from VictoriaLogs; `presence.py`, `onsets.py` and `sites.py` compute the
engine and per-host tables. The exports contain the URLs and queries your
clients sent, so they are git-ignored.
