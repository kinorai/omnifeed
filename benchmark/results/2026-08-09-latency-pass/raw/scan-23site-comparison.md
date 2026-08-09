# OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE — risk-maximized 23-URL set (on1 / off / on2)

Extension of `../scanfullpage/SCAN_COMPARISON.md`, same method, new URL set chosen to
maximize lazy-load / virtualization / infinite-scroll risk.

Local docker-compose stack (omnifeed + crawl4ai 0.9.2 + searxng), image built from
`feat/latency-metrics` @ `6c118ad` (`omnifeed:bench-6c118ad`).
Endpoint: `POST http://localhost:8080/crawl`, `{"urls":["<url>"]}`, no auth
(`OMNIFEED_DEV_NO_AUTH=true`), 120 s curl timeout, sequential, **one run per URL per pass**.

Three passes, each preceded by a full `docker compose down -v` teardown and a fresh
`up -d` (no crawl4ai cache carry-over):

| pass | scan | started |
|---|---|---|
| on1 | `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE=true`  | 11:28 |
| off | `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE=false` | 11:33 |
| on2 | `OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE=true`  | 11:36 |

`on2` is the churn control: if `off` differs from **both** ons the same way, that's the
scan; if `on1` and `on2` differ from each other just as much, that's live-content churn.

The env value was verified on the running container each pass via
`docker inspect omnifeed --format '{{range .Config.Env}}…'` (the image is distroless —
`docker exec … env` fails with `exec: "env": executable file not found in $PATH`), plus
the startup log line confirming `"version":"6c118ad"`.

`links` = distinct `http(s)` URLs in `page_content`
(`grep -oE 'https?://[^) "]+' | sort -u | wc -l`).

## Table

| idx | url | on1 s | off s | on2 s | on1 chars | off chars | on2 chars | on1 links | off links | on2 links | verdict |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 1 | https://dev.to/ | 10.61 | 4.46 | 10.50 | 48329 | 37096 | 59735 | 240 | 190 | 285 | **SCAN-WINS** |
| 2 | https://www.theverge.com/tech | 9.73 | 3.55 | 10.81 | 24829 | 26785 | 24829 | 113 | 113 | 113 | NO-DIFF |
| 3 | https://medium.com/tag/programming | 5.39 | 2.93 | 4.90 | 10016 | 9968 | 9986 | 20 | 20 | 20 | CHURN |
| 4 | https://www.producthunt.com/ | 11.14 | 7.04 | 10.37 | 69 | 69 | 69 | 0 | 0 | 0 | FAILED |
| 5 | https://www.tumblr.com/explore/trending | 10.40 | 5.03 | 11.60 | 6229 | 21644 | 51137 | 1 | 129 | 333 | CHURN |
| 6 | https://www.theguardian.com/technology | 3.02 | 2.44 | 3.23 | 27 | 27 | 27 | 0 | 0 | 0 | NO-DIFF |
| 7 | https://arstechnica.com/ | 8.99 | 3.22 | 9.05 | 82 | 82 | 82 | 0 | 0 | 0 | NO-DIFF |
| 8 | https://techcrunch.com/ | 9.49 | 3.14 | 9.86 | 18265 | 18385 | 18265 | 66 | 66 | 66 | NO-DIFF |
| 9 | https://github.com/golang/go/blob/master/src/net/http/server.go | 11.44 | 5.75 | 11.10 | 887 | 947 | 887 | 4 | 4 | 4 | OFF-WINS |
| 10 | https://mui.com/material-ui/react-table/ | 9.90 | 3.43 | 9.39 | 10973 | 10973 | 10973 | 24 | 24 | 24 | NO-DIFF |
| 11 | https://tanstack.com/virtual/latest/docs/introduction | 7.77 | 2.60 | 6.89 | 2484 | 2484 | 2484 | 2 | 2 | 2 | NO-DIFF |
| 12 | https://nextjs.org/docs | 19.66 | 7.30 | 20.42 | 69 | 69 | 69 | 0 | 0 | 0 | FAILED |
| 13 | https://angular.dev/overview | 8.81 | 2.75 | 8.30 | 7820 | 7820 | 7820 | 12 | 12 | 12 | NO-DIFF |
| 14 | https://svelte.dev/docs/svelte/overview | 4.22 | 2.06 | 3.86 | 1417 | 1417 | 1417 | 7 | 7 | 7 | NO-DIFF |
| 15 | https://vuejs.org/guide/introduction.html | 8.82 | 2.54 | 8.81 | 11488 | 11488 | 11488 | 28 | 28 | 28 | NO-DIFF |
| 16 | https://docs.stripe.com/api | 7.80 | 6.41 | 6.10 | 22409 | 22409 | 22409 | 68 | 68 | 68 | NO-DIFF |
| 17 | https://docs.github.com/en/actions | 4.64 | 2.32 | 5.56 | 3375 | 3375 | 3375 | 15 | 15 | 15 | NO-DIFF |
| 18 | https://caniuse.com/?search=grid | 17.85 | 6.05 | 17.86 | 109 | 110 | 109 | 0 | 0 | 0 | FAILED |
| 19 | https://bsky.app/profile/bsky.app | 3.78 | 3.34 | 3.78 | 31 | 31 | 31 | 0 | 0 | 0 | NO-DIFF |
| 20 | https://www.ebay.com/sch/i.html?_nkw=laptop | 18.77 | 10.04 | 16.53 | 72558 | 71696 | 71771 | 204 | 205 | 203 | CHURN |
| 21 | https://www.etsy.com/search?q=poster | 10.07 | 8.34 | 8.82 | 69 | 69 | 69 | 0 | 0 | 0 | FAILED |
| 22 | https://html.spec.whatwg.org/multipage/sections.html | 9.59 | 4.03 | 9.53 | 81941 | 81941 | 81941 | 126 | 126 | 126 | NO-DIFF |
| 23 | https://news.ycombinator.com/ | 0.36 | 0.38 | 0.36 | 4558 | 4557 | 4557 | 30 | 30 | 30 | NO-DIFF |

### Byte-identity matrix

14 of 23 URLs produced **byte-identical** `page_content` across all three passes
(idx 4, 6, 7, 10, 11, 12, 13, 14, 15, 16, 17, 19, 21, 22 — `cmp -s` clean on all pairs).
For a further 4 (idx 2, 8, 9, 18) `on1` and `on2` are byte-identical to each other and
only `off` differs — i.e. a *reproducible* scan effect. The remaining 5 (idx 1, 3, 5, 20, 23)
differ pairwise in all three combinations.

## Verdict counts

| verdict | n | idx |
|---|---|---|
| NO-DIFF    | 14 | 2, 6, 7, 8, 10, 11, 13, 14, 15, 16, 17, 19, 22, 23 |
| SCAN-WINS  | 1  | 1 |
| CHURN      | 3  | 3, 5, 20 |
| OFF-WINS   | 1  | 9 |
| FAILED     | 4  | 4, 12, 18, 21 |

## Latency

| pass | n | mean | median |
|---|---|---|---|
| on1 | 23 | 9.23 s | 9.49 s |
| off | 23 | 4.31 s | 3.43 s |
| on2 | 23 | 9.03 s | 9.05 s |

Excluding idx 23 (served by the direct/generic engine, 0.36 s, never touches crawl4ai)
and the 4 all-pass failures (idx 4, 12, 18, 21):

| pass | n | mean | median |
|---|---|---|---|
| on1 | 18 | 8.51 s | 8.99 s |
| off | 18 | 3.89 s | 3.34 s |
| on2 | 18 | 8.32 s | 9.05 s |

**scan=off is ~2.2× faster overall (mean −4.7 s vs the two-on average of 9.13 s, −52%)
and ~2.15× faster on the crawl4ai-only subset (mean −4.53 s, −54%).** on1 and on2 agree
to within 0.2 s in the mean, so the machine was not drifting between passes.

## SCAN-WINS — the decision-relevant case

### idx 1 — https://dev.to/ (infinite-scroll feed front page)

The only URL in the set where scan recovers real, missing content, and the recovery is
**reproducible in both ons and strictly additive**.

Counting article permalinks (`https://dev.to/<user>/<slug>`, deduped):

| set | article links |
|---|---|
| off | 127 |
| on1 | 159 |
| on2 | 194 |

Set arithmetic:

```
off-only vs on1: 0    on1-only vs off: 32
off-only vs on2: 1    on2-only vs off: 68
on1-only vs on2: 1    on2-only vs on1: 36
in BOTH ons but not off: 32
in off but in NEITHER on: 0
```

**off's article set is a strict subset of on1's** (0 links present in off and absent from
on1), and 32 article permalinks appear in *both* ons and in neither the off pass — that is
the scroll appending the next page of the feed, not churn. Only 1 link is unique to off
across both ons, and 0 links are in off but missing from both ons: nothing is lost by
scrolling.

Churn is present too — on2 has 36 links on1 lacks (5 minutes later, and dev.to's front
page turns over fast) — but churn alone cannot explain the pattern, because `off` ran
*between* the two on passes and still has the *smallest* set. Time ordering was
on1 (11:28) → off (11:33) → on2 (11:36); a pure-churn model would put off between the ons.

Sample of content present with scan on and absent with scan off (lines unique to on1):

```
###  [ A 200 response is not a page, and your policy check is grepping an empty shell ](https://dev.to/jacksonxly/a-200-response-is-not-a-page-and-your-policy-check-is-grepping-an-empty-shell-3ah1)
[ 1 reaction ](https://dev.to/aniket28dot/mitigating-http-request-smuggling-be6) [Add Comment](https://dev.to/aniket28dot/mitigating-http-request-smuggling-be6#comments)
[#a11y](https://dev.to/t/a11y) [#shopify](https://dev.to/t/shopify) [#testing](https://dev.to/t/testing)
```

Cost: **+6.1 s** (10.6 s vs 4.5 s) for **+25% article links** (127 → 159), or +53% at the
on2 sample.

## OFF-WINS

### idx 9 — github.com/golang/go/blob/…/server.go (virtualized code viewer)

The prime a-priori scan candidate, and scan makes it slightly *worse*. on1 and on2 are
byte-identical (887 chars); off is a strict superset (947 chars). Full diff, off → on1:

```
9,13d8
< /
< # server.go
< Copy path
< More file actions
< More file actions
```

Scrolling drops the file-header block (`# server.go`, `Copy path`). **Neither** pass gets
any of the actual Go source — all three outputs begin with GitHub's own failure banner:

```
###  Uh oh!
There was an error while loading. [Please reload this page](https://github.com/golang/go/blob/master/src/net/http/server.go).
```

GitHub's virtualized blob viewer defeats crawl4ai in both configurations; scan costs
+5.7 s (11.4 s vs 5.7 s) and returns 60 fewer chars.

## CHURN

- **idx 3 medium.com/tag/programming** — off↔on1 and on1↔on2 differ by exactly the same
  amount (68 changed lines each). The differences are Medium's "recommended stories" module
  reshuffling per request (different `?source=---recommended_stories---programming---N-107---…`
  session GUIDs each fetch) and the topic-nav tag list rotating. Link count is 20 in all
  three passes.
- **idx 5 tumblr.com/explore/trending** — the noisiest row in the set, and the one that
  most needed the on2 control. off (21,644 chars / 129 links) and on2 (51,137 / 333) share
  the same page skeleton and **off's tumblr.com link set is a strict subset of on2's**
  (0 off-only, 201 on2-only) — that looks exactly like scan appending trending posts.
  But on1 (6,229 chars / **1** link) rendered an entirely different, link-free page variant
  under the *same* scan=true setting: it starts on unrelated post text
  (`⋆༺𓆩⚖𓆪༻⋆ / boosty| patreon | comic`) instead of the tag list that both off and on2 open
  with, and shares 0 of off's 124 tumblr.com links. Since the two on passes disagree with
  each other more than off disagrees with either, this is site-side variance by the stated
  rule. **Caveat for the decision:** the on2-vs-off comparison here is the second-strongest
  scan-appends-feed-items signal in the set after dev.to, and one sample was not enough to
  resolve it.
- **idx 20 ebay.com search grid** — all three pass pairs differ by a comparable number of
  lines (off↔on1 1089, on1↔on2 768, off↔on2 1001) while link counts are flat within 1%
  (204 / 205 / 203) and chars within 1.2% (72,558 / 71,696 / 71,771). The eBay result grid
  is already fully in the initial DOM; what changes is listing rotation, prices and ad slots.
  Scan buys nothing here and costs +7 s.

## NO-DIFF worth naming

- **idx 2 theverge.com/tech** — reproducible but not a content difference: off has 12 lines
  the ons lack, all 6 repetitions of the cookie-consent placeholder
  *"This content isn't visible due to your cookie preferences… AllowView and Manage all
  cookie consent preferences"*. Identical 113 links in all three passes. Same finding as
  idx 15 of the earlier 16-URL run: scrolling resolves the embeds and removes boilerplate,
  it does not add articles.
- **idx 8 techcrunch.com** — off has exactly one extra line, an a11y notice
  (`Some areas of this page may shift around if you resize the browser window…`);
  66 links in all three passes.
- **idx 6 guardian.com/technology (27 chars), idx 7 arstechnica.com (82), idx 19 bsky.app (31)**
  — title-only extractions in *all three* passes
  (`Technology | The Guardian`, `Ars Technica - Serving the Technologist since 1998. …`,
  `Bluesky (@bsky.app) — Bluesky`). Same pre-existing pipeline extraction failure as
  `en.wikipedia.org/wiki/Kubernetes` in the earlier run — reported `ok:true` with no content.
  Not scan-related, but it means three of the eight feed/index URLs produced no measurable
  content either way.
- **idx 22 html.spec.whatwg.org (control)** — byte-identical 81,941 chars / 126 links in all
  three passes, as designed. Scan costs +5.5 s for nothing.
- **idx 23 news.ycombinator.com (control)** — served by the generic engine in 0.36 s in all
  three passes (scan is irrelevant to it, and the timing confirms crawl4ai was bypassed);
  the only deltas are HN vote/comment counters ticking (`…,groomlake,438,228,…` vs `…,437,228,…`).

## Failures (verbatim, identical in all three passes)

All four failed in every pass, so all four are excluded from the scan verdicts.

```
idx 4  https://www.producthunt.com/
  Error crawling URL: upstream_error (HTTP 500): upstream returned 500          [on1, off, on2]

idx 12 https://nextjs.org/docs
  Error crawling URL: upstream_error (HTTP 500): upstream returned 500          [on1, off, on2]

idx 21 https://www.etsy.com/search?q=poster
  Error crawling URL: upstream_error (HTTP 500): upstream returned 500          [on1, off, on2]

idx 18 https://caniuse.com/?search=grid
  on1: Error crawling URL: thin_content (HTTP 200): crawl4ai extracted 0 chars (raw_md=1 fit_md=1 cleaned_html=232)
  off: Error crawling URL: thin_content (HTTP 200): crawl4ai extracted 0 chars (raw_md=30 fit_md=1 cleaned_html=363)
  on2: Error crawling URL: thin_content (HTTP 200): crawl4ai extracted 0 chars (raw_md=1 fit_md=1 cleaned_html=232)
```

Scan made three of the four failures markedly slower without changing the outcome:
nextjs.org 19.7/20.4 s vs 7.3 s, caniuse 17.9 s vs 6.0 s, producthunt 11.1/10.4 s vs 7.0 s.
etsy was the exception (10.1/8.8 s vs 8.3 s). ebay and bsky did **not** hit a bot wall —
ebay returned a full 200-listing grid, bsky returned a title-only extraction.

## Conclusion

On a URL set deliberately built to maximize lazy-load and virtualization risk,
`OMNIFEED_CRAWL4AI_SCAN_FULL_PAGE=true` pays a flat ~4.5-6 s per crawl4ai page (~2.2×
end-to-end latency) and returns additional content on **1 of 19 non-failing URLs**.

What scan actually buys, by site class:

- **Server-paginated infinite-scroll feeds (dev.to; probably tumblr)** — this is the one
  real win. The scroll triggers the next page fetch and appends items, strictly additively:
  +25% article permalinks on dev.to with zero loss, reproduced in both on passes. Tumblr
  points the same direction (off ⊂ on2, +201 links) but its on1 outlier makes it
  unresolved at one run per pass.
- **Server-rendered news indexes and article listings (theverge, techcrunch, guardian,
  arstechnica, medium)** — nothing. The article list is already in the initial DOM; the
  lazy part is images and embeds, which produce no markdown either way. The only
  reproducible textual effect is *removing* cookie-consent and a11y boilerplate.
- **E-commerce lazy grids (ebay)** — nothing. Full grid in the initial DOM; differences are
  listing/ad rotation of the same magnitude between the two on passes.
- **Virtualized / JS-heavy docs and code views (github blob, mui, tanstack-virtual, nextjs,
  angular, svelte, vue, stripe, docs.github)** — nothing, on all nine, and this is the
  strongest result in the run: 8 of 9 are byte-identical across all three passes, and the
  ninth (GitHub's virtualized blob viewer, the single best a-priori candidate for scan) is
  *worse* with scan on, losing the file header while still recovering none of the source.
  These pages hydrate their full content on load; there is nothing below the fold to fetch.
- **SPAs (caniuse, bsky) and static controls (whatwg spec, HN)** — nothing, by failure or
  by byte-identity.

The earlier 16-URL run found zero pages where scan added content; this risk-maximized run
finds exactly one class where it does — feeds that fetch more items on scroll — at a cost
of roughly doubling latency on every other page. Keeping the default `false` and, if the
feed case matters, enabling scan per-request or per-domain for infinite-scroll feed URLs
is better than a global flag.

## Artifacts

- `on1/`, `off/`, `on2/` — each with `manifest.jsonl`
  (`{"idx","url","pass","time_total_s","http_code","chars","links","ok","note"}`)
  and `content/<nn>-<slug>.txt` (the extracted `page_content`).
