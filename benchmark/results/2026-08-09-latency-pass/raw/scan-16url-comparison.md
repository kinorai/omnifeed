# OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE — on vs off

Local docker-compose stack (omnifeed + crawl4ai 0.9.2 + searxng), image built from
`feat/latency-metrics` @ `6c118ad` (`omnifeed:bench-6c118ad`, single-arch linux/amd64).
Endpoint: `POST http://localhost:8080/crawl`, `{"urls":["<url>"]}`, no auth
(`OMNIFEED_DEV_NO_AUTH=true`), 120s curl timeout, sequential.

**Absolute times are not comparable to the prod benchmark** — this is a local
single-arch build reached over localhost (no cloudflared tunnel, no nginx ingress,
no k3s network hop), and the host is a busy machine (also running the prod cluster).
Only the on/off delta is meaningful.

Between passes the whole stack was torn down with `docker compose down -v` and brought
back up (full restart, not just a container recreate) so no crawl4ai cache could carry
scan=on results into pass B.

## Table

`on s (r1/r2)` / `off s (r1/r2)` = `time_total` seconds. `Δchars %` = (on − off) / off.

| idx | url | on s (r1/r2) | off s (r1/r2) | on chars | off chars | Δchars % |
|---|---|---|---|---|---|---|
| 1 | https://raw.githubusercontent.com/golang/go/master/README.md | 0.01 / 0.03 | 0.19 / 0.01 | 1455 | 1455 | +0.0% |
| 2 | https://en.wikipedia.org/wiki/Kubernetes | 9.37 / 8.13 | 2.70 / 2.09 | 24 | 24 | +0.0% |
| 3 | https://go.dev/blog/loopvar-preview | 7.05 / 6.76 | 2.55 / 2.39 | 8447 | 8447 | +0.0% |
| 4 | https://react.dev/learn | 8.92 / 9.51 | 2.57 / 2.52 | 14466 | 14779 | -2.1% |
| 5 | https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Content-Type | 8.07 / 8.09 | 2.19 / 2.15 | 8792 | 8792 | +0.0% |
| 6 | https://www.paulgraham.com/greatwork.html | 8.49 / 8.02 | 2.50 / 1.96 | 66434 | 66434 | +0.0% |
| 7 | https://kubernetes.io/docs/concepts/workloads/pods/ | 8.36 / 8.20 | 2.34 / 2.28 | 27332 | 27332 | +0.0% |
| 8 | https://docs.python.org/3/library/asyncio.html | 4.85 / 4.75 | 2.12 / 2.05 | 5384 | 5384 | +0.0% |
| 9 | https://arxiv.org/abs/1706.03762 | 4.39 / 4.09 | 2.38 / 2.34 | 6280 | 6280 | +0.0% |
| 10 | https://htmx.org/docs/ | 9.26 / 8.30 | 2.50 / 2.41 | 86208 | 86208 | +0.0% |
| 11 | https://tailwindcss.com/docs/installation | 5.54 / 5.61 | 2.20 / 2.25 | 2624 | 2624 | +0.0% |
| 12 | https://lite.cnn.com/ | 5.71 / 5.32 | 2.17 / 2.02 | 17151 | 17151 | +0.0% |
| 13 | https://vercel.com/docs | 26.35 / 26.59 | 7.77 / 8.37 | 69 | 69 | +0.0% (both failed) |
| 14 | https://blog.cloudflare.com/ | 7.94 / 7.51 | 2.23 / 2.15 | 7586 | 7586 | +0.0% |
| 15 | https://www.theverge.com/ | 10.12 / 10.19 | 3.70 / 3.71 | 51631 | 54891 | -5.9% |
| 16 | https://dev.to/t/programming | 7.22 / 6.84 | 2.51 / 2.44 | 13465 | 13465 | +0.0% |

## Latency

All 32 samples per pass (both runs):

| pass | mean | median | min | max |
|---|---|---|---|---|
| on  | 8.11 s | 7.98 s | 0.01 s | 26.59 s |
| off | 2.62 s | 2.34 s | 0.01 s | 8.37 s |

Excluding idx 1 (served by the direct-fetch engine, never touches crawl4ai) and
idx 13 (upstream failure), i.e. 28 real crawl4ai samples per pass:

| pass | mean | median |
|---|---|---|
| on  | 7.38 s | 7.98 s |
| off | 2.41 s | 2.34 s |

**Delta: scan=off is ~3.1× faster — mean −5.49 s (−67.7%), median −5.63 s (−70.6%)
over the full set; mean −4.97 s (−67.4%) on the crawl4ai-only subset.**
The cost is remarkably uniform: nearly every crawl4ai page pays ~5-6 s extra with
scan on, independent of page size.

## Quality

**No URL exceeded |Δchars| > 10%.** 14 of 16 pages produced *byte-identical*
`page_content` between the two passes (verified with `cmp`). The two that differ
both have scan=on returning *less* text than scan=off:

- **idx 4 — react.dev/learn (−2.1%).** Not a content loss in the "missing sections"
  sense — it is a *formatting regression caused by scan=on*. With scan on, the
  syntax-highlighted code samples lose their ``` fences and their JSX angle brackets
  (`return (` / `button` / `I'm a button` / `</button` instead of
  `  return (` / `    <button>` / `      I'm a button` / `    </button>`).
  Scrolling appears to re-render react.dev's virtualised code blocks into a state
  crawl4ai's markdown pass handles worse. scan=off is strictly better here.
- **idx 15 — theverge.com (−5.9%).** The scan=on output is a strict subset of the
  scan=off output (0 lines present only in the on file). All 20 extra lines in the
  off file are 5 repetitions of the cookie-consent placeholder
  *"This content isn't visible due to your cookie preferences… AllowView and Manage
  all cookie consent preferences"* — pure boilerplate. Scrolling apparently lets the
  embeds resolve/replace those placeholders, so scan=on removes noise, not content.

### Lazy-load pages — verdicts

- **idx 15 https://www.theverge.com/ — scan_full_page buys nothing.** Both passes
  extract the same 128 unique theverge.com article links and the same headline set;
  the only textual difference is the cookie-consent boilerplate above. The Verge's
  index is server-rendered enough that the initial DOM already carries the article
  list; the lazy part is images/embeds, which produce no markdown text either way.
  Cost of the scroll here: +6.4 s (10.1 s vs 3.7 s).
- **idx 16 https://dev.to/t/programming — scan_full_page buys nothing.** The two
  outputs are byte-identical (13,465 chars, 104 `dev.to/` links each), ending on the
  same trailing "Sign in for the ability to sort posts" line. The infinite-scroll
  feed does not append further posts within the scan window; the scroll costs
  +4.7 s (7.2 s vs 2.5 s) for zero extra content.

### Notes / anomalies (identical in both passes, so not scan-related)

- **idx 2 en.wikipedia.org/wiki/Kubernetes returns 24 chars** — just
  `Kubernetes - Wikipedia` — in *both* passes, run 1 and run 2. This is a
  pre-existing extraction/pruning problem in the pipeline, unrelated to scan_full_page.
- **idx 13 vercel.com/docs fails in both passes** (`ok:false`), 4/4 attempts:
  loader body `Error crawling URL: upstream_error (HTTP 500): upstream returned 500`;
  crawl4ai log: `Blocked by anti-bot protection: Structural: no <body> tag (14878 bytes)`.
  scan=on made the failure slower (26.4 s vs 7.8 s) but no less fatal.

## Conclusion

On this 16-URL set, `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE=true` costs ~5-6 s per
crawl4ai page (~3.1× total latency) and returns **zero additional content on any
page**, including the two pages picked specifically because they lazy-load. On
react.dev it actively degrades code-block formatting. The default (`false`) is the
right setting for this workload.
