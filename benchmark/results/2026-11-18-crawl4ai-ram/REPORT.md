# crawl4ai memory: one Chrome too many, and what plain HTTP would save (2026-11-18)

Suites C and A ([README](../../README.md)). The question: crawl4ai takes about
85% of the omnifeed stack's memory. What can make it lighter without losing
content?

**Answer:** crawl4ai runs a Chrome that omnifeed's crawls never use, and starts
a second one for them. A `config.yml` change in the infrastructure repo makes
the crawls use the first one: about 100–190 MiB less while crawls run, with
identical content and latency, and no omnifeed change. A plain-HTTP first
step would make 74% of pages about 17× faster, but after that change it saves
almost no memory.

## Production, before any test

7 and 30 days of VictoriaMetrics, 2026-11-18:

| | |
|---|---|
| crawl4ai working set, 30 days | p50 833 MiB, p90 1.4 GiB, max 1.9 GiB |
| crawl4ai floor and mean, 7 days | 358 MiB (5th percentile), mean 507 MiB |
| omnifeed itself / SearXNG | 25–45 MiB / about 200 MiB |
| Generic crawls served by crawl4ai | about 200 a week; p50 1.5 s |
| Reddit fetches through the browser method | 0 since the current pods started (all `client_id`) |
| kube-01 memory requests | 10.1 of 10.4 GiB allocatable; crawl4ai requests 1.5 GiB |

The memory steps between about 360 and 570 MiB, following crawl4ai's pool of
browsers opening and closing.

## Why there are two or three Chromes

crawl4ai 0.9.5's server (`deploy/docker/crawler_pool.py`, `server.py`):

1. At boot it starts a **permanent** browser from `config.yml`
   (`crawler.browser.kwargs` and `extra_args`). It never closes. `/execute_js`
   (the Reddit browser method) uses it.
2. `/crawl` builds a browser config from the **request body** and keys a pool
   by a hash of the whole config. A request that hashes like the permanent
   one uses it; any other starts a pool browser, closed after `idle_ttl_sec`
   (600 s in production; twice that once used three times).
3. omnifeed's body never hashes like the permanent browser:
   - the body can't carry `extra_args` (crawl4ai rejects it from API callers),
     while production's `config.yml` sets three;
   - the server's default ends up with `text_mode: true`, while omnifeed's body
     leaves it `false`;
   - omnifeed sends its user agent and `enable_stealth`, which `config.yml`
     doesn't set.
4. omnifeed's retry with the other fingerprint (`browserParams(altFingerprint)`
   in `internal/engine/crawl4ai/engine.go`) drops the user agent, so it hashes
   differently again: a third Chrome.

The production comment that `text_mode: true` speeds up generic crawls is
wrong: crawl4ai's own docstring says `config.yml`'s browser kwargs never reach
`/crawl` browsers.

The three `extra_args` are redundant: crawl4ai launches Chrome with
`--no-sandbox`, `--disable-gpu`, `--disable-software-rasterizer` and
`--disable-dev-shm-usage` on its own (checked in the running container).

## Experiment 1: Chromes and memory per request kind

[raw/pool.py](raw/pool.py) against the production image digest and
`config.yml` (with `idle_ttl_sec: 60` so the idle step fits in 200 s),
`--memory 5g --shm-size 1g` like the pod. Working set = cgroup
`memory.current` minus `inactive_file`. Each cell is MiB / Chrome browsers.

| Step | Production config | Matched config |
|---|---|---|
| Boot | 328 / 1 | 289 / 1 |
| After 5 generic crawls | 596 / 2 | **420 / 1** |
| After 1 alternate-fingerprint crawl | 811 / 3 | 631 / 2 |
| After `/execute_js` | 847 / 3 | 629 / 2 |
| After 2 more crawls | 983 / 3 | 747 / 2 |
| After 200 s idle | 394 / 1 | 382 / 1 |

"Matched config" is the production `config.yml` with these changes
([raw/config.diff](raw/config.diff)):

```yaml
browser:
  kwargs:
    headless: true
    text_mode: false
    enable_stealth: true
    user_agent: "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/156.0.0.0 Safari/537.36"
  extra_args: []
```

Two other variants failed, and the failures show what must match
([raw/pool.jsonl](raw/pool.jsonl)): the same change without `text_mode: false`
still started a second Chrome, and sending `text_mode: true` from the body
instead works too but needs an omnifeed change.

## Experiment 2: the corpus, production config against matched config

`fetch/run.py` on the 38-URL corpus through the real omnifeed binary, one
sequential pass, then 8 concurrent, then 30 s idle
([raw/fetch-prod.md](raw/fetch-prod.md), [raw/fetch-matched.md](raw/fetch-matched.md)):

| | Production config | Matched config |
|---|---|---|
| Grades | 38/38 ok | 38/38 ok |
| p50 / p95 | 1.42 s / 4.20 s | 1.40 s / 4.06 s |
| 8 concurrent | 22.7 s, 37/38 ok | 23.7 s, 37/38 ok |
| Peak | 1842 MiB / 3 Chrome | 1752 MiB / 2 Chrome |
| 30 s after | 850 MiB / 2 Chrome | 749 MiB / 1 Chrome |

The one concurrent failure, the same on both, is a Cloudflare CAPTCHA on
blog.cloudflare.com. Only one page changed size by more than 2%: lequipe.fr,
a news front page that changed between the runs.

## Experiment 3: what plain HTTP would serve

The `http` candidate: one plain GET with omnifeed's user agent, then Firecrawl
Local's `needs_browser()` check (`src/browser.rs` in the 0.1.0 crate):

- **28 of 38 pages** came back complete (every phrase found), p50 0.08 s
  against 1.42 s through crawl4ai.
- 4 failed outright: 403 from Stack Overflow, npm and lequipe.fr (bot walls),
  and the PDF (not HTML).
- Twice the check correctly said a browser is needed (the JS-only quotes page,
  crates.io).
- **4 times it said "no browser needed" and phrases were missing**: on
  CloudNativePG the page is a redirect (Firecrawl handles that in a separate
  `meta_refresh_target()`, not ported here), and on three docs pages one of two
  phrases was missing from the tag-stripped text. A shipped version would need
  a fallback for these, or it returns a silent partial page.

What it would save: production's mean memory sits about 150 MiB above the
floor, and that is the most a plain-HTTP step could remove, by keeping the
pool browser closed. With the matched config there is no pool browser for
normal crawls, so the saving drops to almost nothing. Its gain is speed:
about 1.3 s on three pages in four.

## Recommendations

1. **Change crawl4ai's `config.yml` in the infrastructure repo** to the matched
   config above. It needs no omnifeed change and no new code: it uses
   crawl4ai's own pool. The cost is a coupling: when omnifeed's
   `DefaultCrawl4AIUserAgent` or stealth setting changes, `config.yml` must
   change in the same step, or the second Chrome silently returns. Comment
   both places. Check after the deploy: crawl4ai logs "Using permanent
   browser" for crawls, not "Creating new browser in cold pool".
2. **Lower the pod's memory request only after a week of data.** A smaller
   working set frees nothing on kube-01 until the request (1.5 GiB) drops.
3. **Don't build the plain-HTTP step for memory.** Build it only if the speed
   matters; then it needs the redirect handling and a fallback when phrases go
   missing, and a re-run of this suite.

Side effect: `/execute_js` would use omnifeed's user agent and stealth instead
of crawl4ai's default identity. The Reddit browser method is the only caller,
and it served no requests in the measured window.
