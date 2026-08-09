# omnifeed before-pass summary (run 1 only)

Before-pass against the live deployment on 2026-08-09 (~10:03-10:08Z), config: crawl4ai `delay_before_return_html=1.0`, `scan_full_page=true`, `max_retries=2`. Images at time of run: `unclecode/crawl4ai:0.9.2`, `kinorai/omnifeed:dev-11f8c52`, `searxng/searxng:2026.8.3-0734ee6c7` (see `provenance.txt`).

**Run 2 was skipped**: the deployment rolled to a new image with new crawl4ai knobs at 10:22Z, so a second pass would no longer measure the "before" config. Run 1 (17/17 URLs) is the complete before-pass dataset. The after-pass should therefore also be compared on single-run numbers, and should reuse the reddit control permalink recorded in `manifest.jsonl` verbatim.

## Results (run 1)

| # | url | time_total_s | chars | ok |
|---|-----|-------------:|------:|----|
| 1 | https://raw.githubusercontent.com/golang/go/master/README.md | 39.00 | 1466 | yes |
| 2 | https://en.wikipedia.org/wiki/Kubernetes | 9.09 | 24 | yes* |
| 3 | https://go.dev/blog/loopvar-preview | 8.27 | 8447 | yes |
| 4 | https://react.dev/learn | 9.65 | 14466 | yes |
| 5 | https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Content-Type | 9.16 | 8792 | yes |
| 6 | https://www.paulgraham.com/greatwork.html | 9.71 | 66434 | yes |
| 7 | https://stackoverflow.com/questions/11227809/why-is-processing-a-sorted-array-faster-than-processing-an-unsorted-array | 6.27 | 120001 | yes |
| 8 | https://kubernetes.io/docs/concepts/workloads/pods/ | 9.42 | 27332 | yes |
| 9 | https://docs.python.org/3/library/asyncio.html | 5.86 | 5384 | yes |
| 10 | https://arxiv.org/abs/1706.03762 | 5.14 | 6280 | yes |
| 11 | https://htmx.org/docs/ | 9.60 | 86208 | yes |
| 12 | https://tailwindcss.com/docs/installation | 6.52 | 2624 | yes |
| 13 | https://lite.cnn.com/ | 6.72 | 17283 | yes |
| 14 | https://vercel.com/docs | 82.84 | 128 | NO |
| 15 | https://blog.cloudflare.com/ | 8.89 | 7586 | yes |
| 16 | CONTROL (hackernews) https://news.ycombinator.com/item?id=8863 | 0.55 | 27505 | yes |
| 17 | CONTROL (reddit) https://www.reddit.com/r/selfhosted/comments/1viw9sj/searched_for_proxmox_ve_on_the_internet/ | 3.06 | 11305 | yes |

## Latency stats — crawl4ai URLs only (idx 1-15, controls excluded)

- All 15 calls: mean **15.08 s**, median **9.09 s** (mean inflated by the 82.8 s vercel failure)
- 14 successful calls only: mean **10.24 s**, median **8.99 s**

## Anomalies

- **#14 vercel.com/docs FAILED** after 82.8 s with JSON-RPC error: `fetch_url failed: upstream_error (HTTP 500): upstream returned 500`. Saved payload is the raw error envelope.
- **#2 wikipedia** returned only 24 chars — just the page title `Kubernetes - Wikipedia`, no article body. HTTP/MCP-level success but a content-extraction quality failure; a key page to re-check in the after-pass.
- **#1 raw go README** confirmed the known ~30 s pathological case (39.0 s for a 1.5 KB text file) — the primary target of the knob change.
- **#12 tailwindcss.com/docs/installation** succeeded but extracted only 2624 chars (intro section); no error, noted per spec.
- **#5 MDN** worked with the `/Reference/` path as-is — no retry without `/Reference` was needed.
- Controls behaved as expected: HN 0.55 s / 27.5k chars, reddit 3.06 s / 11.3k chars.

Note: `manifest.jsonl` consumers should key on `"run":1` only; there are no run-2 lines.
