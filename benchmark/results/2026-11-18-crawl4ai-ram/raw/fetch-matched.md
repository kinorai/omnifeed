# Fetch benchmark

corpus: 38 URLs, passes: 1, concurrency pass: 8

## omnifeed (omnifeed)

- grades: {'ok': 38, 'partial': 0, 'miss': 0, 'empty': 0, 'blocked': 0, 'error': 0} (silent failures = miss + empty: 0)
- seconds p50 1.40, p95 4.06
- tokens total 450056, median 4989.5
- concurrency 8: wall 23.7 s, ok 37/38
- c4a: idle 292 MiB / 1 Chrome, peak 1752 MiB / 2 Chrome, after 30 s 749 MiB / 1 Chrome
