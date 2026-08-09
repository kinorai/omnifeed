# omnifeed latency/quality benchmark — before vs after

- **Before**: old config (delay_before_return_html=1.0, max_retries=2, no raw-text bypass), run 1 only — `before/manifest.jsonl`.
- **After**: `kinorai/omnifeed:dev-6c118ad` (delay_before_return_html=0.1, max_retries=0, raw-text bypass for raw-extension URLs; scan_full_page still ON), runs 1+2 — `after/manifest.jsonl`. Fetched 2026-08-09 ~10:25–10:45 UTC (see `after/provenance.txt`).

## Comparison table (run-1 vs run-1; after run-2 for steady state)

| idx | url | before s (run1) | after s (run1) | after s (run2) | before chars | after chars (run1) | Δchars % |
|----:|-----|----------------:|---------------:|---------------:|-------------:|-------------------:|---------:|
| 1 | raw.githubusercontent.com/golang/go/master/README.md | 39.00 | 0.06 | 0.05 | 1466 | 1455 | -0.75 |
| 2 | en.wikipedia.org/wiki/Kubernetes | 9.09 | 8.67 | 8.22 | 24 | 24 | 0 |
| 3 | go.dev/blog/loopvar-preview | 8.27 | 6.96 | 6.82 | 8447 | 8447 | 0 |
| 4 | react.dev/learn | 9.65 | 8.83 | 8.66 | 14466 | 14466 | 0 |
| 5 | developer.mozilla.org/.../Headers/Content-Type | 9.16 | 8.07 | 8.10 | 8792 | 8792 | 0 |
| 6 | www.paulgraham.com/greatwork.html | 9.71 | 8.48 | 8.03 | 66434 | 66434 | 0 |
| 7 | stackoverflow.com/questions/11227809/... | 6.27 | 11.91 | 10.39 | 120001 | 128 | -99.89 |
| 8 | kubernetes.io/docs/concepts/workloads/pods/ | 9.42 | 8.46 | 8.32 | 27332 | 27332 | 0 |
| 9 | docs.python.org/3/library/asyncio.html | 5.86 | 4.88 | 4.82 | 5384 | 5384 | 0 |
| 10 | arxiv.org/abs/1706.03762 | 5.14 | 4.25 | 4.11 | 6280 | 6280 | 0 |
| 11 | htmx.org/docs/ | 9.60 | 8.47 | 8.35 | 86208 | 86208 | 0 |
| 12 | tailwindcss.com/docs/installation | 6.52 | 5.67 | 5.73 | 2624 | 2624 | 0 |
| 13 | lite.cnn.com/ | 6.72 | 5.78 | 5.34 | 17283 | 17283 | 0 |
| 14 | vercel.com/docs | 82.84 | 26.18 | 25.68 | 128 | 128 | 0 |
| 15 | blog.cloudflare.com/ | 8.89 | 8.02 | 7.56 | 7586 | 7586 | 0 |
| 16 | news.ycombinator.com/item?id=8863 (CONTROL) | 0.55 | 0.51 | 0.49 | 27505 | 27505 | 0 |
| 17 | reddit.com/r/selfhosted/.../searched_for_proxmox_ve... (CONTROL) | 3.06 | 3.39 | 3.19 | 11305 | 13101 | +15.89 |

Failures (`ok:false`): idx 7 after runs 1+2, idx 14 before + after runs 1+2 — all `fetch_url failed: upstream_error (HTTP 500): upstream returned 500`.

## Latency stats (crawl4ai URLs, idx 1–15, time_total_s)

| pass | median | mean |
|------|-------:|-----:|
| before (run1) | 9.09 | 15.08 |
| after (run1) | 8.07 | 8.31 |
| after (run2) | 8.03 | 8.01 |

- Mean nearly halved (15.08 → 8.01s steady state), driven by three outliers: idx 1 raw-text bypass (39.0 → 0.06s), idx 14 failing fast without retries (82.8 → 25.7s), and no 180s stragglers.
- Median improved modestly (9.09 → 8.03s, ~-1.1s) — consistent with delay_before_return_html 1.0 → 0.1 saving ~0.9s per page; scan_full_page (still ON) dominates the remaining ~8s on typical pages.
- No cold-start penalty visible in run 1: run 1 and run 2 are within noise on every URL.

## Quality

Byte-identical content before → after on 12 of 15 crawl4ai URLs (idx 3–6, 8–13, 15) and control idx 16. The exceptions:

- **idx 1 (go README, -0.75%)**: Content equivalent. The before-pass rendered the raw markdown through crawl4ai, which wrapped it in a stray ```` ``` ```` code fence (+ blank lines); the raw-text bypass now returns the file verbatim. Nothing lost — a small quality improvement.
- **idx 2 (Wikipedia, 24 chars) — pre-existing anomaly, unchanged**: before and after content files are byte-identical: the entire payload is `Kubernetes - Wikipedia\n` — just the page `<title>`. crawl4ai returns status 200 / content_type markdown with the whole article body stripped (its markdown fit/pruning discards the article on this page). Not caused or affected by the new knobs; it still burns the full ~8s browser render to return 24 chars. Worth a separate look in crawl4ai.
- **idx 7 (StackOverflow, -99.89%) — the one real regression in this run**: before returned the full 120k-char Q&A; after, both runs failed with `upstream_error (HTTP 500)`. crawl4ai logs show the actual cause: `Blocked by anti-bot protection: Cloudflare JS challenge`, then `HTTP 403 with HTML content (1202802 bytes)`. This is Cloudflare blocking SO fetches at benchmark time (plausibly escalated by repeatedly re-fetching the same question during the before/after passes), not a parsing effect of the new knobs — though with max_retries now 0 there is no second chance on such blocks. Real content lost in this run; re-test later before attributing it to the config.
- **idx 14 (vercel.com/docs)**: failed identically before and after (`upstream_error HTTP 500`, saved payload is the 128-char JSON-RPC error envelope). No regression — but it now fails in ~26s instead of ~83s thanks to max_retries 0.

## Controls (should not change — confirmed)

- **idx 16 hackernews**: 0.55s → 0.51/0.49s, chars 27505 → 27505, content byte-identical.
- **idx 17 reddit**: 3.06s → 3.39/3.19s, chars 11305 → 13101 (+15.9%). Diff is live-thread churn only: 4 new comments (45 → 49) and vote-score drift on existing comments — same engine behavior, same ballpark latency.
