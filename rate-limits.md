# Rate limits: what gets omnifeed blocked

omnifeed has two kinds of upstream limit. **Search engines** behind SearXNG
limit how many queries one egress IP (or one API key) may send. **Websites**
behind `fetch_url` decide, mostly on the first request, whether the client looks
like a browser. This page measures both from one self-hosted deployment, so
you can size your own budget and recognize each block when you see it.

The short version:

- **Searches run out; fetches don't.** At 200 searches a day the free engines
  start dropping out one by one. At 200 fetches a day nothing happens: this
  deployment did 948 fetches in one day with no block that pace explains.
- **Google is the first to go silent.** Its share of answers falls past about
  **50 searches in 24 hours or 20 in one hour**, and the penalty outlasts three
  hours of slow searching.
- **Brave's API key is a monthly budget**, not a rate: about 1,000 queries,
  then `access denied` until the 1st.
- **Website blocks are about who you are, not how fast you go.** 91 of
  6,154 fetches were blocked; 85 of them had no fetch to the same host in the
  hour before, and none happened during a host's busy hours.

## Data

| Source | Window | Volume |
| --- | --- | --- |
| omnifeed `mcp tool call completed/failed` logs | 2026-06-13 → 2026-10-09 | 2,623 searches, 6,308 fetches |
| omnifeed `search audit` logs (which engines answered each query) | 2026-08-19 → 2026-10-09 | 1,607 searches |
| omnifeed partial-result logs (which engines failed, and why) | 2026-08-04 → 2026-10-09 | 2,276 partial searches |
| omnifeed unresponsive-engine logs | 2026-08-19 → 2026-10-09 | 2,815 engine refusals |
| A paced benchmark run (272 sites, 40 queries, about 2 fetches a minute and one search every 3 to 6 minutes) | 2026-10-09 | live block log below |

Everything goes out through **one residential IPv4 address**, shared with
every other client on the same network. Engines and sites see that IP, not
omnifeed, so anything else that hits Google from the network spends the same
reputation. If your ISP shares IPv4 addresses between subscribers (CGNAT,
MAP-T), strangers spend it too, and your budgets will be lower than these.

## Searches

Each `web_search` is one SearXNG query, and SearXNG fans it out to every
enabled engine. So every engine sees **your full query rate**, and the engine
with the tightest limit sets the pace for all of them.

### Engine budgets

Presence is the share of searches where the engine returned at least one row,
bucketed by how many searches the deployment had made in the window before.

**Searches in the prior 24 hours** (1,102 general searches):

| Prior 24 h | n | google cse | braveapi | yandex | privacywall |
| --- | --- | --- | --- | --- | --- |
| 0–24 | 297 | **87%** | 80% | 87% | 29% |
| 25–49 | 328 | **81%** | 68% | 85% | 38% |
| 50–74 | 243 | **55%** | 54% | 89% | 56% |
| 75–99 | 149 | **39%** | 12% | 72% | 79% |
| 100–149 | 85 | 55% | 45% | 96% | 85% |

**Searches in the prior hour**:

| Prior 1 h | n | google cse | braveapi | yandex | privacywall |
| --- | --- | --- | --- | --- | --- |
| 0–4 | 466 | 79% | 66% | 84% | 39% |
| 5–9 | 257 | 77% | 62% | 86% | 47% |
| 10–19 | 240 | 68% | 56% | 85% | 58% |
| 20–29 | 84 | **30%** | 37% | 89% | 69% |
| 30+ | 55 | **18%** | 31% | 91% | 67% |

Bursts within a minute don't matter: google cse answered 64–77% at every
per-minute rate up to 8 or more searches a minute. The 24-hour and 1-hour
counts are what it tracks.

| Engine | Limit | Symptom | How long it lasts | Measured |
| --- | --- | --- | --- | --- |
| **google cse** (keyless, shared CX) | soft: about 50 / 24 h and 20 / 1 h | `too many requests` (HTTP 429) on every query | over 3½ hours; SearXNG suspends it for only 180 s, so each later query re-probes it | presence 87% → 39% between 0–24 and 75–99 searches / 24 h; gone at 68 / 24 h and 25 / 1 h on 2026-10-09 |
| **braveapi** (free API key) | about 1,000 queries / month (the $5 monthly credit) | `access denied` on every query | until the credit resets on the 1st | silent 2026-08-16 → 2026-09-01; 88–100% on most days since |
| **wikipedia** | about 11 / minute; 200+ / day | `too many requests` | SearXNG suspension, 180 s | 10–11 searches in the prior minute at each onset |
| **mojeek** | volume-linked | `access denied` | hours | onsets at a median 112 searches / 24 h (August; absent from the logs since) |
| **yandex** | none seen | — | — | 72–96% at every load |
| **privacywall** | none from our volume | `access denied` on about half the queries | per query | refuses more at *low* load than at high, so not our rate |
| **infospace**, **searchtoday** | none from our volume | `access denied` | per query | 8–42%, unrelated to load |

### Site-scoped searches lean on Brave and Google

A search with `site=` (or `site:` in the query) only gets answers from
**braveapi (67%)**, **google cse (63%)** and **privacywall (44%)** — yandex and
the others return nothing for it. When google cse is silent, a `site=reddit.com`
search runs on Brave's monthly credit alone. Measured on 2026-10-09: with google
cse silent, `site=reddit.com` got 10 results, all from braveapi.

General searches stay complete when one engine drops out: across the period
only 0.9% of general searches came back empty, against 2.8% of site-scoped
ones.

### The `quota_exhausted` error is omnifeed, not an engine

`OMNIFEED_SEARXNG_QUOTA` paces queries before they leave (see
[configuration](configuration.md#pacing-fail-fast)). With Redis configured the
quota is shared across replicas and across every client of the deployment, so
one client's burst spends everyone's window. 99 searches failed this way in
the period, never more than 12 in one day, and none of them reached an engine.

### Search budget

| Goal | Budget |
| --- | --- |
| Keep google cse answering (≥ 80%) | **≤ 40 searches / 24 h and ≤ 15 / hour** |
| Make Brave's credit last the month | ≤ 33 searches / day on average |
| Keep wikipedia answering | ≤ 10 searches / minute |
| Recover after google cse goes silent | stop for hours; one search every 6 minutes did not bring it back in 3½ hours |

At **200 searches a day** you are 4× over google cse's soft budget and spend a
month of Brave credit in five days. Results still come back — yandex and
privacywall carry general queries — but site-scoped queries are left on Brave's
credit alone.

## Fetches

### Blocks are first-visit fingerprinting

Of 6,154 fetches with a URL, **91 were blocked** (1.5%): `bot_block`,
`captcha`, HTTP 403 or 429. Measured per host:

- **85 of the 91** had no fetch to the same host in the hour before, and **75**
  were the first fetch to that host in 24 hours. The block came from who we
  looked like (DataDome, Cloudflare challenge, Akamai, PerimeterX, LinkedIn's
  HTTP 999), not from how often we came back.
- **No host was blocked during its busy hours.** Of the 32 hosts with 15 or
  more fetches, none had a block inside an hour running at half or more of
  its peak hourly rate.
- Retrying a blocked host right away gets the same block: same fingerprint,
  same verdict.

### Proven-safe paces

These rates were reached with no block. They are lower bounds on the real
limit, not the limit itself.

| Host | Fetches | Blocks | Max / 1 min | Max / 10 min | Max / 1 h | Max / 24 h |
| --- | --- | --- | --- | --- | --- | --- |
| reddit.com | 1,727 | 8 | 21 | 111 | 142 | 218 |
| news.ycombinator.com | 644 | 0 | 25 | 104 | 104 | 146 |
| github.com | 243 | 0 | 7 | 16 | 21 | 30 |
| raw.githubusercontent.com | 239 | 0 | 12 | 22 | 23 | 30 |
| hn.algolia.com | 97 | 0 | 10 | 10 | 10 | 33 |
| developers.cloudflare.com | 60 | 1 | 13 | 19 | 19 | 39 |
| kubernetes.io | 48 | 0 | 4 | 4 | 8 | 14 |

All fetches together peaked at **72 a minute, 530 an hour, 1,035 in 24 hours**
(948 on 2026-10-08) with no pace-caused block.

Reddit is the exception that proves the rule. One `fetch_url` call can make up
to 41 Reddit requests (thread, `morechildren` rounds, share-link resolve), and
Reddit counts requests, not calls. Six of its 8 blocks came in July at one
fetch an hour or less, which points at IP reputation, not rate. The other two
came in August after a burst of 3–4 fetches in one minute, on days with about
90 Reddit fetches. Keep
`OMNIFEED_REDDIT_QUOTA` set anyway ([configuration](configuration.md)): on
2026-10-08 Reddit blocked after about 50 requests in 5 minutes.

### What a block looks like

| Error code | Typical cause | Retry? |
| --- | --- | --- |
| `captcha` | DataDome, Cloudflare "just a moment", Google `/sorry`, AWS WAF | no: the next request gets the same page |
| `blocked` | HTTP 403, Akamai interstitial, Cloudflare JS challenge | no |
| `upstream_error` (reason `site_error`) | a non-standard refusal such as LinkedIn's HTTP 999 | no, despite the `retryable` flag |
| `rate_limited` | HTTP 429 (Read the Docs on a first visit, Debian packages, Expedia) | after a pause |
| `thin_content` | a JS-only shell, or a page that is genuinely tiny | no |

The full list is in [errors](errors.md). One false positive to know about:
`thin_content` fires on any page with under 100 characters of prose, so a
page that really is that small (web.archive.org's 2005 Google homepage, 85
characters) fails too.

## Block log, 2026-10-09

A deliberately slow benchmark run: 272 fetches at about two a minute, 40
searches spaced 3 minutes apart, then 6 minutes once google cse went silent.
Times are UTC; searches/24 h counts every client of the deployment.

| Time | Event | Load at the time | Cause |
| --- | --- | --- | --- |
| 15:36 | `quota_exhausted` on one search | another client sent 10 searches in 35 s | omnifeed's own shared pacing quota; retry after 45 s worked |
| 15:43 | google cse stops answering (`too many requests`) | 68 searches / 24 h, 25 in the prior hour | Google's soft per-IP budget |
| 15:48 | `scholar.google.com` serves `/sorry` ("unusual traffic") to **both** omnifeed and Claude Code's WebFetch | same | Google flags the IP across products, not per tool. The same page worked for both the day before |
| 15:43 → 19:18 | google cse still silent on every search at one search per 6 minutes (one 30-minute gap) | 28 searches in a row | penalty longer than 3½ hours |
| 16:20 | linkedin.com profile → HTTP 999 | first visit | LinkedIn bot wall |
| 16:24 | zeit.de → HTTP 403 | first visit | anti-bot, not pace |
| all run | no HTTP 429 to omnifeed from any website (the built-in got one from Expedia on its first visit, as on 2026-10-08) | ≤ 2 fetches / minute | — |

## Check it yourself

Which engines answered, per query (LogsQL):

```
"search audit" | fields _time, total, site_scoped, engine_rows
```

Engine refusals over the last day, by engine and reason (PromQL):

```
sum by (engine, error) (increase(omnifeed_searxng_unresponsive_engines_total[1d]))
```

Your own search rate, to compare with the budgets above:

```
sum(increase(omnifeed_searxng_queries_total[24h]))
sum(increase(omnifeed_searxng_queries_total[1h]))
```

Blocked fetches and their hosts (LogsQL):

```
"mcp tool call failed" tool:fetch_url err:~"bot_block|captcha|http_403|http_429" | fields _time, args.url, err
```

The analysis scripts that produced the tables are in
[`benchmark/builtin-tools/limits/`](https://github.com/kinorai/omnifeed/tree/main/benchmark/builtin-tools/limits).
