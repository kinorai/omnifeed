# Fetch benchmark

corpus: 54 URLs, passes: 1

## before (omnifeed)

- grades: {'ok': 52, 'partial': 0, 'miss': 0, 'empty': 0, 'blocked': 0, 'error': 2} (silent failures = miss + empty: 0)
- seconds p50 2.68, p95 5.09
- tokens total 539629, median 3378.0
- c4a: idle 915 MiB / 2 Chrome, peak 1173 MiB / 2 Chrome, after 0 s 861 MiB / 2 Chrome

| url | grade | cov | s | chars | err |
|---|---|---|---|---|---|
| https://blog.cloudflare.com/clef-decision-models/ | error |  | 9.04 | 0 | captcha |
| https://blog.cloudflare.com/dns-cache-memory-optimization-1111/ | error |  | 10.56 | 0 | captcha |

## after (omnifeed)

- grades: {'ok': 52, 'partial': 0, 'miss': 0, 'empty': 0, 'blocked': 0, 'error': 2} (silent failures = miss + empty: 0)
- seconds p50 2.79, p95 4.84
- tokens total 330409, median 3378.0
- coverage of before: median 1.0, below 0.8 on 2/52
- c4a: idle 861 MiB / 2 Chrome, peak 1141 MiB / 2 Chrome, after 0 s 872 MiB / 2 Chrome

| url | grade | cov | s | chars | err |
|---|---|---|---|---|---|
| https://code.claude.com/docs/en/remote-control | ok | 0.7 | 1.58 | 68703 |  |
| https://code.claude.com/docs/en/routines | ok | 0.7 | 3.07 | 51631 |  |
| https://blog.cloudflare.com/clef-decision-models/ | error | None | 9.15 | 0 | captcha |
| https://blog.cloudflare.com/dns-cache-memory-optimization-1111/ | error | None | 10.86 | 0 | captcha |

## gomd-raw (gomd)

- grades: {'ok': 51, 'partial': 0, 'miss': 0, 'empty': 0, 'blocked': 0, 'error': 3} (silent failures = miss + empty: 0)
- seconds p50 0.08, p95 0.37
- tokens total 373743, median 3806.5
- coverage of before: median 1.0, below 0.8 on 5/52
- c4a: idle 872 MiB / 2 Chrome, peak 870 MiB / 2 Chrome, after 0 s 870 MiB / 2 Chrome

| url | grade | cov | s | chars | err |
|---|---|---|---|---|---|
| https://code.claude.com/docs/en/remote-control | ok | 0.7 | 0.09 | 73871 |  |
| https://code.claude.com/docs/en/routines | ok | 0.7 | 0.06 | 57268 |  |
| https://www.manualslib.com/manual/922890/Focal-Dimension.html | error | 0.0 | 0.03 | 0 | http 403 |
| https://www.manualslib.com/manual/922890/Focal-Dimension.html?page=15 | error | 0.0 | 0.01 | 0 | http 403 |
| https://openai.com/index/gpt-6-astra/ | error | 0.0 | 0.03 | 0 | http 403 |

## gomd-read (gomd)

- grades: {'ok': 51, 'partial': 0, 'miss': 0, 'empty': 0, 'blocked': 0, 'error': 3} (silent failures = miss + empty: 0)
- seconds p50 0.06, p95 0.36
- tokens total 305619, median 2620.0
- coverage of before: median 0.9, below 0.8 on 18/52
- c4a: idle 870 MiB / 2 Chrome, peak 870 MiB / 2 Chrome, after 0 s 869 MiB / 2 Chrome

| url | grade | cov | s | chars | err |
|---|---|---|---|---|---|
| https://artificialanalysis.ai/models | ok | 0.5 | 0.12 | 10194 |  |
| https://docs.k3s.io/cli/secrets-encrypt | ok | 0.6 | 0.01 | 6834 |  |
| https://openrouter.ai/docs/api/reference/limits | ok | 0.5 | 0.38 | 21037 |  |
| https://openrouter.ai/docs/faq | ok | 0.6 | 0.13 | 16964 |  |
| https://code.claude.com/docs/en/remote-control | ok | 0.6 | 0.09 | 73128 |  |
| https://code.claude.com/docs/en/routines | ok | 0.6 | 0.08 | 55829 |  |
| https://www.manualslib.com/manual/922890/Focal-Dimension.html | error | 0.0 | 0.01 | 0 | http 403 |
| https://www.manualslib.com/manual/922890/Focal-Dimension.html?page=15 | error | 0.0 | 0.01 | 0 | http 403 |
| https://www.anthropic.com/engineering/claude-code-best-practices | ok | 0.7 | 0.22 | 55528 |  |
| https://huggingface.co/meta-llama/Llama-3.1-8B-Instruct | ok | 0.6 | 0.36 | 31413 |  |
| https://www.sony.com/electronics/support/articles/00167113 | ok | 0.6 | 0.08 | 3428 |  |
| https://www.sony.com/electronics/support/articles/00173195 | ok | 0.5 | 0.08 | 3108 |  |
| https://arxiv.org/abs/2602.12670 | ok | 0.3 | 0.01 | 7709 |  |
| https://claude.com/blog/complete-guide-to-building-skills-for-claude | ok | 0.7 | 0.21 | 2531 |  |
| https://nextjs.org/docs/app/api-reference/cli/create-next-app | ok | 0.7 | 0.13 | 7185 |  |
| https://openai.com/index/gpt-6-astra/ | error | 0.0 | 0.01 | 0 | http 403 |
| https://openai.com/index/gpt-6-for-everyone/ | ok | 0.4 | 0.09 | 7429 |  |
| https://ubuntu.com/about/release-cycle | ok | 0.5 | 0.04 | 12634 |  |
