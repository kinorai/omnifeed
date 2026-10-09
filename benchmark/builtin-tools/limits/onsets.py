import json, bisect, collections, statistics as st
from datetime import datetime, timezone
ts = lambda s: datetime.fromisoformat(s[:26].rstrip('Z')).replace(tzinfo=timezone.utc).timestamp()
calls = [json.loads(l) for l in open('calls.jsonl')]
S = sorted(ts(c['_time']) for c in calls if c.get('tool') == 'web_search')
F = sorted(ts(c['_time']) for c in calls if c.get('tool') == 'fetch_url')
def window(arr, t, w): return bisect.bisect_left(arr, t) - bisect.bisect_left(arr, t - w)
W = [('10s', 10), ('1m', 60), ('10m', 600), ('1h', 3600), ('24h', 86400)]
print(f"searches logged: {len(S)}  fetches: {len(F)}  span {datetime.fromtimestamp(S[0], timezone.utc):%Y-%m-%d} → {datetime.fromtimestamp(S[-1], timezone.utc):%Y-%m-%d}")
# trigger events (not "Suspended:")
trig = collections.defaultdict(list)
for l in open('partial.jsonl'):
    d = json.loads(l)
    for part in (d.get('unresponsive_engines') or '').split(';'):
        eng, _, why = part.strip().partition(':'); why = why.strip()
        if eng and why and not why.startswith('Suspended'):
            trig[(eng.strip(), why)].append(ts(d['_time']))
# successes per engine from the audit (engine answered with rows)
ok = collections.defaultdict(list)
for l in open('audit.jsonl'):
    d = json.loads(l)
    rows = d.get('engine_rows') or ''
    for eng in ('google cse', 'braveapi', 'privacywall', 'yandex', 'infospace', 'searchtoday', 'mojeek', 'wikipedia'):
        if eng + '=' in rows: ok[eng].append(ts(d['_time']))
def q(xs, p): xs = sorted(xs); return xs[min(len(xs) - 1, int(len(xs) * p))] if xs else 0
for (eng, why), times in sorted(trig.items(), key=lambda kv: -len(kv[1])):
    if len(times) < 5: continue
    times.sort()
    # onsets: first trigger after >= 15 min without one
    onsets = [t for i, t in enumerate(times) if i == 0 or t - times[i - 1] > 900]
    row = []
    for name, w in W:
        v = [window(S, t, w) for t in onsets]
        row.append(f"{name} p10/med {q(v,.1)}/{q(v,.5)}")
    print(f"\n{eng} | {why} | {len(times)} triggers, {len(onsets)} onsets")
    print('  load before onset (searches):', ' · '.join(row))
    if eng in ok:
        good = ok[eng]
        print('  load when it answered      :', ' · '.join(f"{n} med/p90 {q([window(S,t,w) for t in good],.5)}/{q([window(S,t,w) for t in good],.9)}" for n, w in W))
