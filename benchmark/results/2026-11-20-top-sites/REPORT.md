# The sites we fetch most: plain HTTP, a Go converter, and crawl4ai's template leak (2026-11-20)

Suites A and B ([README](../../README.md)). The question: would running
Firecrawl next to crawl4ai make fetches faster on the sites production
actually fetches? And what would a plain-HTTP step inside omnifeed need?

**Answer:** not Firecrawl. What makes it fast is one plain GET before any
browser, and a Go version of that gets 51 of the top 54 production URLs (the
other 3 are bot walls) with the same text, 0.08 s against 2.7 s. It is not
ready to ship: on the 38-URL corpus it returned a "documentation has moved"
page as a success, and a page whose tables load from JavaScript without
them, which no cheap check catches. The run also found that crawl4ai converts `<template>` content: on Mintlify docs
(code.claude.com, openrouter.ai) that is 75–93% of the reply. omnifeed now
excludes the tag: 39% fewer tokens on the top sites, no text lost.

## What production fetches

`fetch_url` calls in VictoriaLogs, 2026-06-29 to 2026-11-19:

| Host | Fetches | Served by |
|---|---|---|
| reddit.com | 2,605 | Reddit engine (API with a credential) |
| news.ycombinator.com + hn.algolia.com | 1,209 | Hacker News engine (Algolia API) |
| raw.githubusercontent.com | 386 | generic engine's plain GET (`rawtext.go`), about 0.25 s |
| github.com | 353 | GitHub engine (REST API) |
| x.com | 82 | Twitter engine (FxTwitter) |
| about 800 other hosts | about 4,850 | crawl4ai |

Per month: 158 (July), 2,661 (August), 1,494 (September), 3,655 (October),
1,183 (November to the 19th). About half reaches crawl4ai, spread thin: the
29 most-fetched generic hosts are about 21% of it.

What a Firecrawl engine would do for the dedicated engines' hosts: Firecrawl
Cloud refused Reddit, LinkedIn and YouTube in the June benchmark
(`clusters/home-prod/apps/omnifeed/docs/FETCH_BENCHMARK.md` in the
infrastructure repo), and Firecrawl Local has no API engines, so it would
lose the comment trees. Neither has stealth for the walled pages below.

## Method

- `corpus/top-sites.jsonl`: 54 URLs production fetched, up to two from each
  of the 28 most-fetched generic hosts. Dead URLs (404 everywhere) and the
  Discourse forums (their own engine in production) are left out. It has no
  phrases; candidates are scored by coverage of the shipped output
  (`--reference`, see the README), and the 38-URL `corpus/fetch.jsonl`
  keeps its phrase grades.
- Candidates, one sequential pass each, the same crawl4ai container
  (production image digest and `config.yml`):
  - `before`: omnifeed at main (58ce8d2);
  - `after`: omnifeed with `template` added to the excluded tags;
  - `gomd-raw`: one plain GET, then
    [html-to-markdown](https://github.com/JohannesKaufmann/html-to-markdown)
    v2.5.2 with omnifeed's excluded tags removed (`fetch/gomd`);
  - `gomd-read`: the same, with
    [go-readability](https://codeberg.org/readeck/go-readability) v2.1.3
    picking the main content first.
- A first look the day before (`raw/plain-http-first-look.jsonl`) ran the
  `http` candidate (tag-stripped text with Firecrawl Local's
  `needs_browser()` check) on the same URLs.

## Results

| | before | after | gomd-raw | gomd-read |
|---|---|---|---|---|
| Top sites: content | 52/54 | 52/54 | 51/54 | 51/54 |
| Top sites: coverage below 0.8 | (reference) | 2 | 5 | 18 |
| Top sites: tokens | 540k | **330k** | 374k | 306k |
| Top sites: p50 | 2.68 s | 2.79 s | **0.08 s** | 0.06 s |
| Corpus: phrase grades ok | 38/38 | 37/38 | 30/38 | 27/38 |
| Corpus: silent failures | 0 | 0 | 3 | 4 |
| Corpus: tokens | 494k | 384k | 394k | 302k |

Raw: [raw/top-sites.md](raw/top-sites.md), [raw/fetch.md](raw/fetch.md)
and their `.jsonl`. Coverage here was scored before two fixes to `run.py`
in the same PR: a sentence containing "http" was skipped (now only one with
`://`), and a sentence with no Latin letters or digits counted as found
(now skipped). The corpus is English, so the second can't have moved a
number; the first left fewer sentences to check on pages about HTTP.

### The template leak, fixed

crawl4ai serialises the page with `<template>` content in it, and converts
that content like any other. A browser never renders it (MDN: a template
holds fragments for scripts; a declarative shadow root is attached, not left
as a template, by the time the page is read). Mintlify docs ship their
compiled MDX in `<template data-mint="manifest">`: `_jsx(_components.p, …)`
after the real text, 100k–300k characters.

Every page that changed by more than 2% with the fix:

| Page | before | after |
|---|---|---|
| openrouter.ai/docs/quickstart | 332,745 | 22,199 |
| openrouter.ai/docs/api/reference/limits | 255,116 | 27,173 |
| code.claude.com/docs/en/remote-control | 257,521 | 68,703 |
| anthropic.com/engineering/claude-code-best-practices (redirects to code.claude.com) | 230,907 | 52,603 |
| code.claude.com/docs/en/routines | 210,577 | 51,631 |
| code.claude.com/docs/en/overview | 120,089 | 16,191 |
| openrouter.ai/docs/faq | 100,276 | 17,545 |

lafibre.info, lequipe.fr and one Apple page also moved between runs; fetched
again side by side, before and after were identical (they change by the
hour). On the Mintlify pages every sentence the fix removed was either the
compiled MDX or the page manifest's JSON; the OpenRouter FAQ answers, which
looked lost at first, are in both (the copies that went were the MDX
source's, with `\n` inside the words). The one `after` error,
blog.cloudflare.com, is Cloudflare's CAPTCHA, which comes and goes on every
build (`before` hit it on both top-sites Cloudflare URLs).

### Plain HTTP with a Go converter

- **Same text, about the same tokens.** `gomd-raw` matched crawl4ai on 47 of
  the 49 top-site pages both fetched (coverage 0.8 or more; the other two
  are code.claude.com pages, scored against `before`, whose compiled MDX it
  rightly lacks), and its
  markdown is 3–13% bigger than the fixed crawl4ai output. The 2.3× of
  Firecrawl Local on lemonde.fr doesn't happen once the excluded tags go.
- **Readability costs content**: coverage below 0.8 on 18 of 52 pages
  (tables, code samples and intros dropped). Don't use it.
- **Bot walls:** manualslib and openai.com answer 403 to a plain GET (sony.com
  did the day before, not this time); crawl4ai's stealth Chrome gets
  through. A plain step must hand every 403 to crawl4ai.
- **Silent failures**, all on the corpus:
  - cloudnative-pg.io/documentation/current/ is a meta-refresh page: 271
    characters of "documentation has moved". Firecrawl Local follows it in
    `meta_refresh_target()`; a port must too.
  - artificialanalysis.ai fills its leaderboards from JavaScript: the GET
    has 20k characters of prose, so `needs_browser()` says it is fine, and
    197 table rows are missing. No cheap check catches this one.
  - quotes.toscrape.com/js and crates.io come back empty; `needs_browser()`
    catches both.

## Recommendations

1. **Ship the template exclusion** (this PR). It needs no option: no
   rendered text lives in a template.
2. **Don't run Firecrawl next to crawl4ai.** It adds a Chrome that
   stays resident once any page needs JavaScript, AGPL, a cloud hand-off on by
   default, and no stealth, so crawl4ai stays for the walled sites anyway.
3. **A plain-HTTP step inside omnifeed is feasible, for speed only** (the
   2026-11-18 report: it saves almost no memory). Building it means
   html-to-markdown v2 without Readability, the meta-refresh follow,
   `needs_browser()`, and a 403 hand-off. It will still return silent
   partials on pages that load data from JavaScript, as artificialanalysis.ai
   does; a per-host list of such sites would be the only guard, and it is the
   duct tape the rest avoids. Weigh that against the gain: production sends
   about 200 generic fetches a week to crawl4ai, so saving 2.6 s on 85% of
   them is about 30 minutes of agent wait a month.
