# Draft — upstream issue for unclecode/crawl4ai

Status: draft, not filed. File with:
`gh issue create -R unclecode/crawl4ai -t "<title>" -F <body>`

---

Title: **PruningContentFilter drops whitespace-only spans in `<pre>`/`<code>` — #1181's bug, still present on the fit_markdown path**

## Summary

#1181 ("importtorch") was fixed in v0.7.8 by skipping elements inside
`<pre>`/`<code>` in `remove_empty_elements_fast` (content_scraping_strategy.py).
`PruningContentFilter` has the same failure, unfixed: it scores and decomposes
whitespace-only token spans inside highlighted code, so `fit_markdown` corrupts
code that `raw_markdown` (post-0.7.8) preserves.

## Environment

crawl4ai 0.9.2, Docker image `unclecode/crawl4ai:0.9.2`, REST `/crawl`.
Code references below are v0.9.2.

## Repro

`crawler_config` → `markdown_generator: DefaultMarkdownGenerator` with
`content_filter: PruningContentFilter(threshold=0.48, threshold_type="fixed")`.
Fetch any page with `<span>`-based syntax highlighting and compare
`fit_markdown` vs `raw_markdown`:

- https://kubernetes.io/docs/concepts/services-networking/service/ —
  every YAML block collapses to one line:
  `apiVersion:v1kind:Servicemetadata:name:my-service…`
- https://dev.to/godofgeeks/multi-stage-builds-in-docker-g4i —
  `FROM golang:1.21 AS builder` → `FROMgolang:1.21ASbuilder`; `&&` dropped.
- https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/Array/reduce —
  all code blocks removed entirely (whole `<pre>` subtrees pruned).

`raw_markdown` is intact in the same responses.

## Root cause

`content_filter_strategy.py`: `_prune_tree` recurses into kept nodes and
`decompose()`s any child scoring under threshold. For a highlighter whitespace
span (`<span class="w"> </span>`), `_compute_composite_score` yields:
text_density 0 (text_len 0) · link_density term 0.20 · tag_weight `span`=0.3 →
0.06 · text_length `log(1)`=0 → composite ≈ 0.26–0.36 < 0.48 → removed.
This is the same arithmetic the v0.7.8 scraper fix bypasses via its
`<pre>`/`<code>` guard ("This preserves whitespace-only spans (e.g.,
`<span class="w"> </span>`) in code blocks"); the filter never got the guard.
Low-scoring whole `<pre>` blocks are likewise decomposed (the MDN case).

## Suggested fix

Port the scraper's guard into `_prune_tree`: skip scoring/pruning for any node
inside a `<pre>`/`<code>` subtree. Alternatively, default `preserve_tags` to
`["pre", "code"]`.

## Workaround (v0.9.1+)

`PruningContentFilter(..., preserve_tags=["pre","code"])` (added by #1904)
protects the whole subtree; or clients can fall back to `raw_markdown` when
`fit_markdown` loses code fences.
