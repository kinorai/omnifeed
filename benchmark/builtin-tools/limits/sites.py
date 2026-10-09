import json, bisect, collections, re
from datetime import datetime, timezone
from urllib.parse import urlparse
ts = lambda s: datetime.fromisoformat(s[:26].rstrip('Z')).timestamp()
BLOCK = ('bot_block', 'captcha', 'http_403', 'http_429')
rows = []
for l in open('calls.jsonl'):
    d = json.loads(l)
    if d.get('tool') != 'fetch_url' or not d.get('args.url'): continue
    host = (urlparse(d['args.url']).hostname or '').removeprefix('www.')
    err = d.get('err') or ''
    kind = 'ok' if not err else ('block' if err.split(':')[0].split(' ')[0] in BLOCK else 'other')  # 'captcha (marker=...)' too
    rows.append((ts(d['_time']), host, kind, err[:70]))
rows.sort()
per = collections.defaultdict(list)
for t, h, k, e in rows: per[h].append((t, k, e))
print(f"fetches with URL: {len(rows)}  distinct hosts: {len(per)}  blocked: {sum(k=='block' for _,_,k,_ in rows)}")
def prior(times, t, w): return bisect.bisect_left(times, t) - bisect.bisect_left(times, t - w)
# every block: same-host fetches in the prior hour / day
pb = [(prior([t for t, _, _ in per[h]], t, 3600), prior([t for t, _, _ in per[h]], t, 86400)) for t, h, k, _ in rows if k == 'block']
print(f"blocks with no same-host fetch in the prior hour: {sum(a == 0 for a, _ in pb)}/{len(pb)}; first visit in 24h: {sum(d == 0 for _, d in pb)}/{len(pb)}")
out = []
for h, ev in per.items():
    T = [t for t, _, _ in ev]
    blocks = [(t, e) for t, k, e in ev if k == 'block']
    if not blocks: continue
    first_visit_blocks = sum(1 for t, _ in blocks if prior(T, t, 86400) == 0)
    p1h = sorted(prior(T, t, 3600) for t, _ in blocks)
    oks = [t for t, k, _ in ev if k == 'ok']
    ok1h = sorted(prior(T, t, 3600) for t in oks)
    out.append((len(blocks), h, len(ev), len(oks), first_visit_blocks, p1h[len(p1h)//2], max(ok1h) if ok1h else None, collections.Counter(e.split(':')[0] for _, e in blocks).most_common(2)))
out.sort(reverse=True)
print(f"{'host':32} fetches ok blocks first-visit-blocks | same-host fetches in prior 1h: at block (median) / max while ok")
for b, h, n, ok, fv, p, okmax, kinds in out[:40]:
    print(f"{h[:32]:32} {n:5} {ok:4} {b:4} {fv:4}  | {p:3} / {okmax}  {kinds}")

print("\nproven-safe pace per host (max same-host fetches observed in a window; blocks inside that busiest window)")
print(f"{'host':28} {'fetches':>7} {'blocks':>6} | max/1m  max/10m  max/1h  max/24h | blocks at >=50% of max/1h")
res = []
for h, ev in per.items():
    if len(ev) < 15: continue
    T = [t for t, _, _ in ev]
    mx = {}
    for name, w in (('1m', 60), ('10m', 600), ('1h', 3600), ('24h', 86400)):
        mx[name] = max(bisect.bisect_right(T, t) - bisect.bisect_left(T, t - w) for t in T)
    b = [t for t, k, _ in ev if k == 'block']
    hot = sum(1 for t in b if (bisect.bisect_right(T, t) - bisect.bisect_left(T, t - 3600)) >= mx['1h'] / 2)
    res.append((len(ev), h, len(b), mx, hot))
for n, h, b, mx, hot in sorted(res, reverse=True)[:25]:
    print(f"{h[:28]:28} {n:7} {b:6} | {mx['1m']:6} {mx['10m']:7} {mx['1h']:7} {mx['24h']:7} | {hot}")
# reddit detail: block contexts
T = [t for t, _, _ in per['reddit.com']]
print('\nreddit blocks:')
for t, k, e in per['reddit.com']:
    if k == 'block':
        print(' ', datetime.fromtimestamp(t, timezone.utc).strftime('%Y-%m-%d %H:%M'), 'prior 1m/10m/1h/24h:', [bisect.bisect_left(T, t) - bisect.bisect_left(T, t - w) for w in (60, 600, 3600, 86400)], e[:60])
