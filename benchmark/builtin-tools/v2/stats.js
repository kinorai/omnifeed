// Shared, deterministic stats for every site version. Same numbers everywhere.
const BENCH = (() => {
  const USE = o => o === 'success' || o === 'partial';
  const pct = (a, p) => { if (!a.length) return NaN; const s = [...a].sort((x, y) => x - y); const i = (s.length - 1) * p / 100, lo = Math.floor(i), hi = Math.ceil(i); return s[lo] + (s[hi] - s[lo]) * (i - lo); };
  const host = u => new URL(u).hostname.replace(/^www\./, '');
  function compute(D) {
    const S = D.sites, Q = D.searches;
    const usable = t => S.filter(s => USE(s[t].outcome)).length;
    const onlyO = S.filter(s => USE(s.omnifeed.outcome) && !USE(s.builtin.outcome));
    const onlyB = S.filter(s => USE(s.builtin.outcome) && !USE(s.omnifeed.outcome));
    const neither = S.filter(s => !USE(s.builtin.outcome) && !USE(s.omnifeed.outcome));
    const both = S.filter(s => USE(s.builtin.outcome) && USE(s.omnifeed.outcome));
    const cats = [...new Set(S.map(s => s.cat))].map(c => { const a = S.filter(s => s.cat === c); return { cat: c, n: a.length, b: a.filter(s => USE(s.builtin.outcome)).length, o: a.filter(s => USE(s.omnifeed.outcome)).length, sites: a }; }).sort((x, y) => y.n - x.n);
    const win = k => Q.filter(q => new RegExp('^' + k).test(q.winner)).length;
    const lat = {
      sB: Q.flatMap(q => q.builtin.ms), sO: Q.flatMap(q => q.omnifeed.ms),
      fB: S.map(s => s.builtin.ms), fO: S.map(s => s.omnifeed.ms),
    };
    const route = s => { const R = { success: 3, partial: 2 }, rb = R[s.builtin.outcome] || 0, ro = R[s.omnifeed.outcome] || 0;
      if (!rb && !ro) return 'neither'; if (rb && !ro) return 'builtin'; if (ro && !rb) return 'omnifeed'; if (rb !== ro) return rb > ro ? 'builtin' : 'omnifeed'; return s.builtin.ms <= s.omnifeed.ms ? 'builtin' : 'omnifeed'; };
    const fallbackFixed = S.filter(s => USE(s.omnifeed.outcome) || USE(s.builtin.outcome)).length;
    const fallbackToday = S.filter(s => USE(s.omnifeed.outcome) || (s.omnifeed.outcome !== 'softblock' && USE(s.builtin.outcome))).length;
    return {
      S, Q, n: S.length, nq: Q.length, usableB: usable('builtin'), usableO: usable('omnifeed'),
      onlyO, onlyB, neither, both, cats, lat,
      searchWins: { b: win('builtin'), o: win('omnifeed'), t: win('tie') },
      refused: S.filter(s => s.builtin.outcome === 'refused'), soft: S.filter(s => s.omnifeed.outcome === 'softblock'),
      news: cats.find(c => c.cat === 'News'), route, fallbackFixed, fallbackToday,
    };
  }
  return { USE, pct, host, compute, load: () => fetch('data.json').then(r => r.json()).then(D => ({ D, ...compute(D) })) };
})();
