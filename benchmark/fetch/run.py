"""Fetch benchmark runner: latency, content, silent failures and memory.

Runs every URL of a corpus through one or more candidates and scores each
reply against the URL's must-contain phrases, so an empty or wrong page that
comes back as a success is counted, not missed. While a candidate runs, the
memory of the containers named with --container is sampled.

    python3 -I benchmark/fetch/run.py --out /tmp/run \\
        --candidate omnifeed=omnifeed:http://127.0.0.1:8080 \\
        --container c4a

Candidate kinds:
  omnifeed   POST <url>/crawl, the Open WebUI loader route (what ships,
             engines, retries and fallbacks included); no_cache=true
  crawl4ai   POST <url>/crawl with omnifeed's default generic-crawl payload
  firecrawl  POST <url>/v2/scrape (Firecrawl or Firecrawl Local)
  http       one plain GET and Firecrawl Local's needs_browser() heuristic;
             says which pages a fetch without a browser would serve. Its
             "content" is tag-stripped HTML, good for phrase checks only.

Writes <out>/raw.jsonl (one line per call) and <out>/summary.md.
See benchmark/README.md for the suites and how to read the numbers.
"""
import argparse
import concurrent.futures as cf
import html
import json
import os
import re
import statistics
import subprocess
import threading
import time
import urllib.error
import urllib.request

# omnifeed's DefaultCrawl4AIUserAgent (internal/config/config.go).
UA = ("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) "
      "Chrome/156.0.0.0 Safari/537.36")
BLOCK = ["just a moment", "verify you are human", "captcha", "access denied",
         "enable javascript", "attention required", "are you a robot"]
# omnifeed's generic crawl payload with its defaults (internal/engine/crawl4ai).
C4A_PARAMS = {
    "word_count_threshold": 10, "wait_until": "domcontentloaded",
    "delay_before_return_html": 0.1, "page_timeout": 60000,
    "excluded_tags": ["nav", "footer", "header", "form", "aside", "script", "style", "noscript"],
    "remove_consent_popups": True, "exclude_external_links": False,
    "exclude_social_media_links": True, "exclude_external_images": True,
    "wait_for": 'css::root:not(:has(script[src*="/cdn-cgi/challenge-platform/"]'
                '[src*="/orchestrate/"])) :visible',
    "wait_for_timeout": 8000,
    "markdown_generator": {"type": "DefaultMarkdownGenerator", "params": {
        "content_filter": {"type": "PruningContentFilter", "params": {
            "threshold": 0.48, "threshold_type": "fixed",
            "preserve_tags": ["pre", "code", "ul", "ol"],
            "preserve_classes": ["highlight", "chroma", "highlighter-rouge", "codehilite"]}},
        "options": {"ignore_links": False}}},
}


def post(url, body, token, timeout):
    headers = {"content-type": "application/json"}
    if token:
        headers["authorization"] = "Bearer " + token
    req = urllib.request.Request(url, json.dumps(body).encode(), headers)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        return {"error": f"http {e.code}: {e.read()[:200]!r}"}
    except Exception as e:  # noqa: BLE001 - a failure is a result
        return {"error": str(e)}


def call_omnifeed(base, url, token):
    j = post(base + "/crawl?no_cache=true", {"urls": [url]}, token, 180)
    if isinstance(j, dict):
        return "", j.get("error", "bad reply"), {}
    doc = j[0] if j else {}
    meta = doc.get("metadata") or {}
    if meta.get("error") == "true":
        return "", meta.get("error_code", "error"), meta
    return doc.get("page_content") or "", "", meta


def call_crawl4ai(base, url, token):
    body = {"urls": [url],
            "crawler_config": {"type": "CrawlerRunConfig", "params": C4A_PARAMS},
            "browser_config": {"type": "BrowserConfig",
                               "params": {"user_agent": UA, "enable_stealth": True}}}
    j = post(base + "/crawl", body, token, 180)
    res = (j.get("results") or [{}])[0]
    md = res.get("markdown") or {}
    text = md if isinstance(md, str) else (
        (md.get("fit_markdown") or "").strip() or md.get("raw_markdown") or "")
    return text, j.get("error") or j.get("detail") or res.get("error_message") or "", {}


def call_firecrawl(base, url, token):
    j = post(base + "/v2/scrape", {"url": url, "formats": ["markdown"]}, token, 180)
    d = j.get("data") or {}
    return d.get("markdown") or "", j.get("error") or "", {"warning": d.get("warning") or ""}


MOUNT_POINTS = ['id="root"', 'id="app"', 'id="__next"', 'id="__nuxt"', 'id="svelte"',
                'id="___gatsby"', "data-reactroot", "ng-app", "ng-view", "ng-version",
                "<app-root", 'id="q-app"', 'id="elm"']
ASKS_FOR_JS = ["enable javascript", "requires javascript", "javascript is disabled",
               "javascript is required", "need to enable javascript"]


def needs_browser(raw_html, text):
    """Port of needs_browser() from Firecrawl Local 0.1.0 (src/browser.rs)."""
    visible = len(re.sub(r"\s", "", text))
    if visible >= 1500:
        return False
    head = raw_html[:200_000].lower()
    if "<script" not in head:
        return False
    if any(s in head for s in ASKS_FOR_JS) or any(m in head for m in MOUNT_POINTS):
        return True
    ratio = visible / max(len(raw_html), 1)
    if ratio < 0.01 and len(raw_html) > 20_000:
        return True
    if visible >= 300:
        return False
    return visible < 60 or ratio < 0.03


def call_http(_base, url, _token):
    req = urllib.request.Request(url, headers={"user-agent": UA, "accept": "text/html,*/*"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            ctype = r.headers.get("content-type", "")
            raw = r.read(10 << 20).decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return "", f"http {e.code}", {"serves": False}
    except Exception as e:  # noqa: BLE001
        return "", str(e), {"serves": False}
    if "html" not in ctype:
        return "", f"content-type {ctype}", {"serves": False}
    body = re.sub(r"(?is)<(script|style|noscript|template|svg|head)\b.*?</\1>", " ", raw)
    text = html.unescape(re.sub(r"<[^>]+>", " ", body))
    text = re.sub(r"\s+", " ", text).strip()
    return text, "", {"serves": not needs_browser(raw, text)}


KINDS = {"omnifeed": call_omnifeed, "crawl4ai": call_crawl4ai,
         "firecrawl": call_firecrawl, "http": call_http}


def plain(text):
    """Markdown or tag-stripped HTML down to comparable words: link text
    without its target, no emphasis or table marks, one space."""
    text = re.sub(r"!?\[([^\]]*)\]\([^)]*\)", r"\1", text)
    text = re.sub(r"[*_`#>|]", "", text)
    return re.sub(r"\s+", " ", text).lower()


def grade(text, err, must):
    """ok / partial / miss / empty / blocked / error. miss and empty are
    silent failures: the call succeeded and the content is not there."""
    if err:
        return "error"
    low = plain(text)
    if len(text) < 3000 and any(b in low for b in BLOCK):
        return "blocked"
    if len(text.strip()) < 200:
        return "empty"
    hit = sum(plain(p).strip() in low for p in must)
    if hit == len(must):
        return "ok"
    return "partial" if hit else "miss"


class Sampler(threading.Thread):
    """Samples a container's working set (cgroup memory.current minus
    inactive_file, what the kubelet reports) and its Chrome browser count."""

    def __init__(self, container):
        super().__init__(daemon=True)
        self.c, self.samples, self.done = container, [], threading.Event()

    def _sh(self, *cmd):
        return subprocess.run(["docker", "exec", self.c, *cmd],
                              capture_output=True, text=True).stdout

    def measure(self):
        stat = dict(ln.split() for ln in self._sh("cat", "/sys/fs/cgroup/memory.stat").splitlines())
        cur = int(self._sh("cat", "/sys/fs/cgroup/memory.current").strip() or 0)
        ps = self._sh("ps", "-eo", "args=").splitlines()
        browsers = sum(1 for a in ps if "chrom" in a.lower() and "--type=" not in a)
        return {"ws_mib": round((cur - int(stat.get("inactive_file", 0))) / 1048576),
                "chrome_browsers": browsers}

    def run(self):
        while not self.done.wait(1):
            self.samples.append(self.measure())


def run_one(name, kind, base, token, item, pass_no):
    t = time.monotonic()
    text, err, meta = KINDS[kind](base, item["url"], token)
    s = round(time.monotonic() - t, 2)
    rec = {"candidate": name, "pass": pass_no, "url": item["url"], "cat": item["cat"],
           "s": s, "chars": len(text), "tokens": len(text) // 4,
           "grade": grade(text, err, item["must"]), "err": str(err)[:200],
           "engine": meta.get("engine", ""), "head": text[:300]}
    if kind == "http":
        rec["serves"] = meta["serves"] and rec["grade"] == "ok"
        rec["serves_but_missing"] = meta["serves"] and rec["grade"] != "ok"
    return rec


def pct(xs, q):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(round(q * (len(xs) - 1))))] if xs else 0


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", default=os.path.join(os.path.dirname(__file__), "..", "corpus", "fetch.jsonl"))
    ap.add_argument("--out", required=True)
    ap.add_argument("--candidate", action="append", required=True, help="name=kind:base_url")
    ap.add_argument("--container", action="append", default=[], help="docker container to sample")
    ap.add_argument("--token", default=os.environ.get("BENCH_TOKEN", ""))
    ap.add_argument("--passes", type=int, default=1)
    ap.add_argument("--concurrency", type=int, default=0, help="extra pass at this concurrency")
    ap.add_argument("--settle", type=int, default=0, help="seconds idle after the run, then sample again")
    a = ap.parse_args()

    corpus = [json.loads(ln) for ln in open(a.corpus) if ln.strip()]
    os.makedirs(a.out, exist_ok=True)
    raw = open(os.path.join(a.out, "raw.jsonl"), "w")
    summary = ["# Fetch benchmark", "", f"corpus: {len(corpus)} URLs, passes: {a.passes}"
               + (f", concurrency pass: {a.concurrency}" if a.concurrency else ""), ""]
    for spec in a.candidate:
        name, rest = spec.split("=", 1)
        kind, base = rest.split(":", 1)
        samplers = {c: Sampler(c) for c in a.container}
        idle = {c: s.measure() for c, s in samplers.items()}
        for s in samplers.values():
            s.start()
        recs = []
        for p in range(1, a.passes + 1):
            for item in corpus:
                r = run_one(name, kind, base, a.token, item, p)
                recs.append(r)
                raw.write(json.dumps(r) + "\n")
                raw.flush()
                print(name, p, r["grade"], r["s"], r["chars"], r["url"], flush=True)
        wall = None
        if a.concurrency:
            t = time.monotonic()
            with cf.ThreadPoolExecutor(a.concurrency) as ex:
                conc = list(ex.map(lambda it: run_one(name, kind, base, a.token, it, "c"), corpus))
            wall = round(time.monotonic() - t, 1)
            for r in conc:
                raw.write(json.dumps(r) + "\n")
        for s in samplers.values():
            s.done.set()
            s.join()
        time.sleep(a.settle)
        after = {c: s.measure() for c, s in samplers.items()}

        grades = {g: sum(r["grade"] == g for r in recs) for g in
                  ("ok", "partial", "miss", "empty", "blocked", "error")}
        secs = [r["s"] for r in recs]
        summary += [f"## {name} ({kind})", "",
                    f"- grades: {grades} (silent failures = miss + empty: "
                    f"{grades['miss'] + grades['empty']})",
                    f"- seconds p50 {statistics.median(secs):.2f}, p95 {pct(secs, 0.95):.2f}",
                    f"- tokens total {sum(r['tokens'] for r in recs)}, "
                    f"median {statistics.median([r['tokens'] for r in recs])}"]
        if wall is not None:
            summary.append(f"- concurrency {a.concurrency}: wall {wall} s, "
                           f"ok {sum(r['grade'] == 'ok' for r in conc)}/{len(conc)}")
        if kind == "http":
            n = sum(r["serves"] for r in recs)
            bad = sum(r["serves_but_missing"] for r in recs)
            summary.append(f"- served without a browser: {n}/{len(recs)}; heuristic said "
                           f"serve but phrases missing: {bad}")
        for c, s in samplers.items():
            peak = max([x["ws_mib"] for x in s.samples] or [0])
            peak_b = max([x["chrome_browsers"] for x in s.samples] or [0])
            summary.append(f"- {c}: idle {idle[c]['ws_mib']} MiB / {idle[c]['chrome_browsers']} Chrome, "
                           f"peak {peak} MiB / {peak_b} Chrome, after {a.settle} s "
                           f"{after[c]['ws_mib']} MiB / {after[c]['chrome_browsers']} Chrome")
        bad = [r for r in recs if r["grade"] != "ok"]
        if bad:
            summary += ["", "| url | grade | s | chars | err |", "|---|---|---|---|---|"]
            summary += [f"| {r['url']} | {r['grade']} | {r['s']} | {r['chars']} | {r['err'][:60]} |"
                        for r in bad]
        summary.append("")
    with open(os.path.join(a.out, "summary.md"), "w") as f:
        f.write("\n".join(summary))
    print("\n".join(summary))


if __name__ == "__main__":
    main()
