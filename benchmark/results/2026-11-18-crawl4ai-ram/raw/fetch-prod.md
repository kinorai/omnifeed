# Fetch benchmark

corpus: 38 URLs, passes: 1, concurrency pass: 8

## omnifeed (omnifeed)

- grades: {'ok': 38, 'partial': 0, 'miss': 0, 'empty': 0, 'blocked': 0, 'error': 0} (silent failures = miss + empty: 0)
- seconds p50 1.42, p95 4.20
- tokens total 449528, median 4989.5
- concurrency 8: wall 22.7 s, ok 37/38
- c4a: idle 290 MiB / 1 Chrome, peak 1842 MiB / 3 Chrome, after 30 s 850 MiB / 2 Chrome

## http (http)

- grades: {'ok': 28, 'partial': 3, 'miss': 1, 'empty': 2, 'blocked': 0, 'error': 4} (silent failures = miss + empty: 3)
- seconds p50 0.08, p95 0.69
- tokens total 254690, median 2917.5
- concurrency 8: wall 1.0 s, ok 28/38
- served without a browser: 28/38; heuristic said serve but phrases missing: 4
- c4a: idle 849 MiB / 2 Chrome, peak 850 MiB / 2 Chrome, after 30 s 840 MiB / 2 Chrome

| url | grade | s | chars | err |
|---|---|---|---|---|
| https://cloudnative-pg.io/documentation/current/ | miss | 0.02 | 226 |  |
| https://developers.cloudflare.com/workers/ | partial | 0.05 | 10855 |  |
| https://grafana.com/docs/grafana/latest/ | partial | 0.13 | 22598 |  |
| https://arxiv.org/pdf/1706.03762 | error | 0.06 | 0 | content-type application/pdf |
| https://react.dev/learn | partial | 0.04 | 14331 |  |
| https://quotes.toscrape.com/js/ | empty | 0.29 | 74 |  |
| https://stackoverflow.com/questions/11227809 | error | 0.03 | 0 | http 403 |
| https://www.npmjs.com/package/react | error | 0.02 | 0 | http 403 |
| https://www.lequipe.fr/ | error | 0.02 | 0 | http 403 |
| https://crates.io/crates/serde | empty | 0.36 | 0 |  |
