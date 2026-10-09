"""Engine presence vs recent search load, from the search audit log (complete since it was added).

For every audited search: how many searches the deployment made in the prior
1 min / 1 h / 24 h, and which engines contributed rows. Presence rate per load
bucket says at what volume each engine starts dropping out.
"""
import json, bisect, collections
from datetime import datetime, timezone

ts = lambda s: datetime.fromisoformat(s[:26].rstrip('Z')).replace(tzinfo=timezone.utc).timestamp()
A = sorted((json.loads(l) for l in open('audit.jsonl')), key=lambda d: d['_time'])
T = [ts(d['_time']) for d in A]
ENG = ['google cse', 'braveapi', 'privacywall', 'yandex', 'mojeek', 'wikipedia', 'infospace', 'searchtoday']
print(f"audited searches: {len(A)}  {datetime.fromtimestamp(T[0], timezone.utc):%Y-%m-%d %H:%M} → {datetime.fromtimestamp(T[-1], timezone.utc):%Y-%m-%d %H:%M} UTC")
general = [i for i, d in enumerate(A) if d.get('site_scoped') != 'true']
scoped = [i for i, d in enumerate(A) if d.get('site_scoped') == 'true']
print(f"general: {len(general)}  site-scoped: {len(scoped)}  empty (total=0): {sum(1 for d in A if d.get('total') in ('0', 0, None))}")

def prior(i, w): return i - bisect.bisect_left(T, T[i] - w)
def has(d, e): return (e + '=') in (d.get('engine_rows') or '')

for label, w, edges in (('24h', 86400, [0, 25, 50, 75, 100, 150, 1e9]), ('1h', 3600, [0, 5, 10, 20, 30, 1e9]), ('1m', 60, [0, 2, 4, 8, 1e9])):
    print(f"\npresence by searches in prior {label} (general searches only)")
    print(f"{'bucket':>10} {'n':>5} " + ' '.join(f'{e[:10]:>10}' for e in ENG))
    for lo, hi in zip(edges, edges[1:]):
        idx = [i for i in general if lo <= prior(i, w) < hi]
        if not idx: continue
        row = [f"{100 * sum(has(A[i], e) for i in idx) / len(idx):9.0f}%" for e in ENG]
        print(f"{f'{lo:.0f}-{hi:.0f}' if hi < 1e8 else f'>={lo:.0f}':>10} {len(idx):5} " + ' '.join(row))

# daily volume and google cse share per day
day = collections.defaultdict(lambda: [0, 0, 0])
for d in A:
    k = d['_time'][:10]
    day[k][0] += 1
    day[k][1] += has(d, 'google cse')
    day[k][2] += has(d, 'braveapi')
print('\nper day: searches, google cse present %, braveapi present %')
for k in sorted(day):
    n, g, b = day[k]
    print(f"  {k} {n:4}  gcse {100*g/n:3.0f}%  brave {100*b/n:3.0f}%")
