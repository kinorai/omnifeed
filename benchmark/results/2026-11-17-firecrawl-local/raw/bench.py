"""Firecrawl Local vs crawl4ai: latency, content quality, memory.

usage: python3 -I bench.py <out.json>
"""
import concurrent.futures as cf
import json
import subprocess
import sys
import threading
import time
import urllib.request

C4A = "http://127.0.0.1:11299/crawl"
FC = "http://127.0.0.1:3399/v2/scrape"

# url, kind, phrases that must be in the main content (lowercase)
URLS = [
    ("https://en.wikipedia.org/wiki/Rust_(programming_language)", "static", ["graydon hoare", "borrow"]),
    ("https://docs.python.org/3/library/asyncio-task.html", "docs", ["taskgroup", "asyncio.gather"]),
    ("https://kubernetes.io/docs/concepts/workloads/pods/", "docs", ["smallest deployable units"]),
    ("https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Status/429", "docs", ["too many requests", "retry-after"]),
    ("https://docs.rs/tokio/latest/tokio/", "docs", ["runtime", "spawn"]),
    ("https://go.dev/doc/effective_go", "docs", ["gofmt", "goroutine"]),
    ("https://github.com/firecrawl/firecrawl", "app", ["firecrawl", "agpl"]),
    ("https://react.dev/learn", "spa", ["usestate", "component"]),
    ("https://quotes.toscrape.com/js/", "js-only", ["albert einstein", "the world as we have created it"]),
    ("https://arxiv.org/abs/1706.03762", "static", ["attention is all you need", "transformer"]),
    ("https://arxiv.org/pdf/1706.03762", "pdf", ["scaled dot-product attention", "multi-head"]),
    ("https://news.ycombinator.com/", "listing", ["points", "comments"]),
    ("https://lwn.net/", "listing", ["lwn"]),
    ("https://blog.cloudflare.com/", "listing", ["cloudflare"]),
    ("https://vercel.com/blog", "spa", ["vercel"]),
    ("https://www.npmjs.com/package/react", "walled", ["react is a javascript library"]),
    ("https://stackoverflow.com/questions/11227809", "walled", ["branch prediction"]),
    ("https://www.bbc.com/news", "news", ["bbc"]),
    ("https://www.lemonde.fr/", "news", ["le monde"]),
    ("https://www.theverge.com/", "news", ["the verge"]),
    ("https://www.lequipe.fr/", "walled", ["football"]),
    ("https://www.postgresql.org/docs/current/sql-select.html", "docs", ["select", "where condition"]),
]

BLOCK = ["just a moment", "verify you are human", "captcha", "access denied",
         "enable javascript", "attention required", "are you a robot"]

C4A_PARAMS = {
    "word_count_threshold": 10, "wait_until": "domcontentloaded",
    "delay_before_return_html": 0.1, "page_timeout": 60000,
    "excluded_tags": ["nav", "footer", "header", "form", "aside", "script", "style", "noscript"],
    "remove_consent_popups": True, "exclude_external_links": False,
    "exclude_social_media_links": True, "exclude_external_images": True,
    "wait_for": 'css::root:not(:has(script[src*="/cdn-cgi/challenge-platform/"][src*="/orchestrate/"])) :visible',
    "wait_for_timeout": 8000,
    "markdown_generator": {"type": "DefaultMarkdownGenerator", "params": {
        "content_filter": {"type": "PruningContentFilter", "params": {
            "threshold": 0.48, "threshold_type": "fixed",
            "preserve_tags": ["pre", "code", "ul", "ol"],
            "preserve_classes": ["highlight", "chroma", "highlighter-rouge", "codehilite"]}},
        "options": {"ignore_links": False}}},
}
UA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"


def post(url, body, headers, timeout=120):
    req = urllib.request.Request(url, json.dumps(body).encode(), {"content-type": "application/json", **headers})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return json.load(r)
    except urllib.error.HTTPError as e:
        try:
            return json.load(e)
        except Exception:
            return {"error": f"http {e.code}"}
    except Exception as e:
        return {"error": str(e)}


def c4a(url):
    body = {"urls": [url], "crawler_config": {"type": "CrawlerRunConfig", "params": C4A_PARAMS},
            "browser_config": {"type": "BrowserConfig", "params": {"user_agent": UA, "enable_stealth": True}}}
    if url.endswith("/pdf/1706.03762"):
        body = {"urls": [url], "crawler_config": {"type": "CrawlerRunConfig", "params": {
            "page_timeout": 60000, "scraping_strategy": {"type": "PDFContentScrapingStrategy", "params": {}}}}}
    j = post(C4A, body, {"authorization": "Bearer benchtoken"})
    res = (j.get("results") or [{}])[0]
    md = res.get("markdown") or {}
    if isinstance(md, str):
        text = md
    else:
        text = (md.get("fit_markdown") or "").strip() or md.get("raw_markdown") or ""
    return text, res.get("status_code"), j.get("error") or res.get("error_message") or ""


def fc(url):
    j = post(FC, {"url": url, "formats": ["markdown"]}, {"authorization": "Bearer benchtoken"})
    d = j.get("data") or {}
    meta = d.get("metadata") or {}
    warn = d.get("warning") or j.get("error") or ""
    return d.get("markdown") or "", meta.get("statusCode"), warn


def score(text, phrases):
    low = text.lower()
    hit = sum(p in low for p in phrases)
    blocked = len(text) < 3000 and any(b in low for b in BLOCK)
    return hit, blocked


def run(name, fn, url, phrases):
    t = time.monotonic()
    text, status, err = fn(url)
    dt = time.monotonic() - t
    hit, blocked = score(text, phrases)
    return {"tool": name, "url": url, "s": round(dt, 2), "chars": len(text), "status": status,
            "hit": hit, "need": len(phrases), "blocked": blocked, "err": str(err)[:200],
            "head": text[:400]}


class MemSampler(threading.Thread):
    def __init__(self, container):
        super().__init__(daemon=True)
        self.c, self.peak, self.samples, self.stop = container, 0, [], False

    def mem(self):
        out = subprocess.run(["docker", "stats", "--no-stream", "--format", "{{.MemUsage}}", self.c],
                             capture_output=True, text=True).stdout.split("/")[0].strip()
        num = float("".join(ch for ch in out if ch.isdigit() or ch == "."))
        unit = out.lstrip("0123456789.")
        return num * {"KiB": 1 / 1024, "MiB": 1, "GiB": 1024, "B": 1 / 1048576}.get(unit, 1)

    def run(self):
        while not self.stop:
            m = self.mem()
            self.samples.append(m)
            self.peak = max(self.peak, m)


def main():
    out = {"results": [], "mem": {}, "throughput": {}}
    tools = {"crawl4ai": (c4a, "c4a-bench"), "firecrawl": (fc, "fc-bench")}
    for name, (fn, ctr) in tools.items():
        s = MemSampler(ctr)
        idle = s.mem()
        s.start()
        # pass 1 sequential (includes cold start), pass 2 sequential warm
        for p in (1, 2):
            for url, kind, phrases in URLS:
                r = run(name, fn, url, phrases)
                r.update(kind=kind, pass_=p)
                out["results"].append(r)
                print(name, p, r["s"], r["chars"], r["hit"], "/", r["need"], "B" if r["blocked"] else "", url, flush=True)
        seq_peak = s.peak
        # concurrency 8 over the whole set
        t = time.monotonic()
        with cf.ThreadPoolExecutor(8) as ex:
            conc = list(ex.map(lambda u: run(name, fn, u[0], u[2]), URLS))
        out["throughput"][name] = {"wall_s": round(time.monotonic() - t, 1),
                                   "ok": sum(r["hit"] == r["need"] for r in conc)}
        s.stop = True
        s.join()
        time.sleep(20)
        out["mem"][name] = {"idle_mib": round(idle, 1), "peak_seq_mib": round(seq_peak, 1),
                            "peak_c8_mib": round(s.peak, 1), "after_mib": round(s.mem(), 1)}
        print(name, out["mem"][name], out["throughput"][name], flush=True)
    json.dump(out, open(sys.argv[1], "w"), indent=1)


if __name__ == "__main__":
    main()
