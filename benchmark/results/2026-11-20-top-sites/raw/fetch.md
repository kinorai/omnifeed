# Fetch benchmark

corpus: 38 URLs, passes: 1

## before (omnifeed)

- grades: {'ok': 38, 'partial': 0, 'miss': 0, 'empty': 0, 'blocked': 0, 'error': 0} (silent failures = miss + empty: 0)
- seconds p50 1.54, p95 4.45
- tokens total 493529, median 4632.0
- c4a: idle 420 MiB / 1 Chrome, peak 1104 MiB / 2 Chrome, after 0 s 1046 MiB / 2 Chrome

## after (omnifeed)

- grades: {'ok': 37, 'partial': 0, 'miss': 0, 'empty': 0, 'blocked': 0, 'error': 1} (silent failures = miss + empty: 0)
- seconds p50 1.33, p95 3.71
- tokens total 383180, median 4021.5
- coverage of before: median 1.0, below 0.8 on 1/38
- c4a: idle 1046 MiB / 2 Chrome, peak 1222 MiB / 3 Chrome, after 0 s 917 MiB / 2 Chrome

| url | grade | cov | s | chars | err |
|---|---|---|---|---|---|
| https://blog.cloudflare.com/ | error | 0.0 | 9.07 | 0 | captcha |

## gomd-raw (gomd)

- grades: {'ok': 30, 'partial': 2, 'miss': 1, 'empty': 2, 'blocked': 0, 'error': 3} (silent failures = miss + empty: 3)
- seconds p50 0.11, p95 0.38
- tokens total 393986, median 4724.0
- coverage of before: median 1.0, below 0.8 on 9/38
- c4a: idle 917 MiB / 2 Chrome, peak 916 MiB / 2 Chrome, after 0 s 915 MiB / 2 Chrome

| url | grade | cov | s | chars | err |
|---|---|---|---|---|---|
| https://cloudnative-pg.io/documentation/current/ | miss | 0.0 | 0.14 | 271 |  |
| https://grafana.com/docs/grafana/latest/ | partial | 0.8 | 0.1 | 5814 |  |
| https://docs.github.com/en/actions/get-started/understand-github-actions | partial | 1.0 | 0.38 | 8926 |  |
| https://arxiv.org/pdf/1706.03762 | error | 0.0 | 0.0 | 0 | content-type application/pdf |
| https://artificialanalysis.ai/ | ok | 0.2 | 0.14 | 20060 |  |
| https://vercel.com/blog | ok | 0.6 | 0.08 | 10502 |  |
| https://quotes.toscrape.com/js/ | empty | 0.0 | 0.29 | 94 |  |
| https://lafibre.info/ | ok | 0.7 | 0.19 | 87258 |  |
| https://stackoverflow.com/questions/11227809 | error | 0.0 | 0.03 | 0 | http 403 |
| https://www.npmjs.com/package/react | error | 0.0 | 0.02 | 0 | http 403 |
| https://crates.io/crates/serde | empty | 0.0 | 0.36 | 0 |  |

## gomd-read (gomd)

- grades: {'ok': 27, 'partial': 3, 'miss': 3, 'empty': 1, 'blocked': 0, 'error': 4} (silent failures = miss + empty: 4)
- seconds p50 0.07, p95 0.34
- tokens total 302394, median 2428.0
- coverage of before: median 0.7, below 0.8 on 21/38
- c4a: idle 915 MiB / 2 Chrome, peak 915 MiB / 2 Chrome, after 0 s 916 MiB / 2 Chrome

| url | grade | cov | s | chars | err |
|---|---|---|---|---|---|
| https://cloudnative-pg.io/documentation/current/ | miss | 0.0 | 0.01 | 272 |  |
| https://code.claude.com/docs/en/overview | ok | 0.5 | 0.06 | 14928 |  |
| https://openrouter.ai/docs/quickstart | ok | 0.7 | 0.14 | 11214 |  |
| https://developers.cloudflare.com/workers/ | ok | 0.7 | 0.08 | 3732 |  |
| https://grafana.com/docs/grafana/latest/ | partial | 0.6 | 0.09 | 3201 |  |
| https://nextjs.org/docs | ok | 0.6 | 0.12 | 3373 |  |
| https://docs.github.com/en/actions/get-started/understand-github-actions | partial | 1.0 | 0.02 | 8212 |  |
| https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Status/429 | ok | 0.78 | 0.04 | 2590 |  |
| https://arxiv.org/abs/1706.03762 | partial | 0.4 | 0.01 | 1963 |  |
| https://arxiv.org/pdf/1706.03762 | error | 0.0 | 0.0 | 0 | content-type application/pdf |
| https://blog.cloudflare.com/ | ok | 0.6 | 0.07 | 4176 |  |
| https://www.githubstatus.com/ | miss | 0.0 | 0.04 | 3023 |  |
| https://artificialanalysis.ai/ | ok | 0.1 | 0.11 | 6445 |  |
| https://vercel.com/blog | ok | 0.6 | 0.07 | 1719 |  |
| https://quotes.toscrape.com/js/ | empty | 0.0 | 0.1 | 42 |  |
| https://lafibre.info/ | ok | 0.6 | 0.17 | 79332 |  |
| https://stackoverflow.com/questions/11227809 | error | 0.0 | 0.01 | 0 | http 403 |
| https://www.npmjs.com/package/react | error | 0.0 | 0.01 | 0 | http 403 |
| https://www.lequipe.fr/ | miss | 0.1 | 0.09 | 2629 |  |
| https://www.theverge.com/ | ok | 0.6 | 0.04 | 24370 |  |
| https://pypi.org/project/requests/ | ok | 0.1 | 0.03 | 17383 |  |
| https://crates.io/crates/serde | error | 0.0 | 0.35 | 0 | readability: no content |
