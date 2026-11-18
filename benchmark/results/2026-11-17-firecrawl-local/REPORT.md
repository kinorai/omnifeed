# Firecrawl Local 0.1.0 against crawl4ai 0.9.5 (2026-11-17)

Suites A and C ([README](../../README.md)). Firecrawl Local is the Rust,
single-binary Firecrawl announced on 2026-11-17: it fetches over plain HTTP and
starts Chrome only when a page looks like it needs JavaScript. The question:
could it replace crawl4ai, which takes about 85% of the omnifeed stack's memory?

**Verdict: no.** It is much faster and lighter on static pages, but it can't do
what crawl4ai does for omnifeed, and it returned one empty page as a success.
What it does show is that plain HTTP alone serves most pages; that idea is
measured on omnifeed's own corpus in
[2026-11-18-crawl4ai-ram](../2026-11-18-crawl4ai-ram/REPORT.md).

## Method

- Both tools in containers on kube-03, the same 22 URLs (docs, static pages, a
  JS-only page, a PDF, listings, news, bot-walled sites). Two sequential passes,
  then all 22 at 8 concurrent. Script and results: [raw/](raw/).
- crawl4ai got omnifeed's generic payload, but **one attempt per URL**, not
  omnifeed's retry chain, and an older user agent (Chrome/142, not omnifeed's
  Chrome/156). Its two failures (npm, the Cloudflare blog) timed out on the
  challenge wait; omnifeed fetched both the next day, retries included.
- Firecrawl Local was built from the 0.1.0 crate without link-time
  optimisation (the optimised build ran out of memory), so its speed is a
  lower bound. It ran with `FIRECRAWL_CLOUD=off`.
- Memory came from `docker stats`, which counts page cache. Later suites use
  the cgroup working set instead (see the README).

## Results

| | crawl4ai | Firecrawl Local |
|---|---|---|
| Sequential p50, pass 1 / pass 2 | 1.77 s / 1.59 s | 0.23 s / 0.09 s |
| All phrases found, pass 1 / pass 2 | 20/22 / 19/22 | 21/22 / 20/22 |
| 22 URLs at 8 concurrent | 17.1 s | 2.3 s |
| Memory idle / peak sequential / peak at 8 / after | 281 / 1150 / 1670 / 836 MiB | 10 / 538 / 700 / 488 MiB |

- **Why it is fast:** no browser on static pages. The crate has no response
  cache, so pass 2 is connection reuse, not a cache hit.
- **Where it is slower:** the JS-only page (quotes.toscrape.com/js), 2.2 s
  against 1.3 s, because it tries HTTP first.
- **Memory:** 10 MiB idle only until the first page needs JavaScript. Then
  Chrome stays resident at about 500 MiB: 0.1.0 has no idle shutdown.

## What would break in omnifeed

- **Reddit:** no `/execute_js`, so the Reddit browser method has no backend.
- **Bot walls:** no stealth, no fingerprint switch. lequipe.fr (Akamai) came
  back as a 199-character "Access Denied" page.
- **Silent failure:** on pass 2, Stack Overflow came back **empty, with no
  error**.
- **Tokens:** it keeps more page chrome. The lemonde.fr home page was 250k
  characters against 109k from crawl4ai.
- **Cloud hand-off** is on by default (`FIRECRAWL_CLOUD=off` turns it off), and
  the licence is AGPL-3.0.

`docs/ideas.md` rejected self-hosted Firecrawl for a fallback chain because it
ships without its anti-bot layer. Firecrawl Local doesn't change that: it has
none either.
