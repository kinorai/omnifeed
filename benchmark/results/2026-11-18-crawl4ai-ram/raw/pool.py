"""crawl4ai browser pool: how many Chromes, how much memory, per request kind.

usage: python3 -I pool.py <container> <variant> <out.jsonl>
variant "prod": omnifeed's body as shipped (user_agent + enable_stealth).
variant "aligned": the body repeats config.yml's kwargs, so it can match the
permanent browser.
"""
import json
import subprocess
import sys
import time
import urllib.request

CTR, VARIANT, OUT = sys.argv[1], sys.argv[2], sys.argv[3]
LABEL = sys.argv[4] if len(sys.argv) > 4 else VARIANT
BASE = "http://127.0.0.1:11298"
UA = ("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) "
      "Chrome/156.0.0.0 Safari/537.36")
PARAMS = {
    "word_count_threshold": 10, "wait_until": "domcontentloaded",
    "delay_before_return_html": 0.1, "page_timeout": 60000,
    "excluded_tags": ["nav", "footer", "header", "form", "aside", "script", "style", "noscript"],
    "remove_consent_popups": True, "exclude_social_media_links": True,
    "exclude_external_images": True,
}
URLS = ["https://kubernetes.io/docs/concepts/workloads/pods/",
        "https://docs.netbird.io/how-to/getting-started",
        "https://cloudnative-pg.io/documentation/current/",
        "https://www.home-assistant.io/integrations/mqtt/",
        "https://go.dev/doc/effective_go"]


def browser(alt):
    if VARIANT == "prod":
        bp = {"enable_stealth": True}
        if not alt:
            bp["user_agent"] = UA
        return bp
    bp = {"headless": True, "text_mode": True, "enable_stealth": True}
    if not alt:
        bp["user_agent"] = UA
    return bp


def post(path, body):
    req = urllib.request.Request(BASE + path, json.dumps(body).encode(),
                                 {"content-type": "application/json",
                                  "authorization": "Bearer benchtoken"})
    t = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=120) as r:
            j = json.load(r)
    except urllib.error.HTTPError as e:
        j = {"error": f"http {e.code}: {e.read()[:200]!r}"}
    except Exception as e:  # noqa: BLE001 - a failure is a result
        j = {"error": str(e)}
    return j, round(time.monotonic() - t, 2)


def crawl(url, alt=False):
    j, s = post("/crawl", {"urls": [url],
                           "crawler_config": {"type": "CrawlerRunConfig", "params": PARAMS},
                           "browser_config": {"type": "BrowserConfig", "params": browser(alt)}})
    res = (j.get("results") or [{}])[0]
    md = res.get("markdown") or {}
    text = md if isinstance(md, str) else (md.get("raw_markdown") or "")
    return {"s": s, "chars": len(text), "err": (j.get("error") or j.get("detail") or "")}


def execute_js(url):
    j, s = post("/execute_js", {"url": url, "scripts": ["document.title"]})
    return {"s": s, "ok": bool(j.get("success", "results" in j)), "err": str(j.get("error") or j.get("detail") or "")[:200]}


def sh(*cmd):
    return subprocess.run(["docker", "exec", CTR, *cmd], capture_output=True, text=True).stdout


def measure(step):
    stat = dict(line.split() for line in sh("cat", "/sys/fs/cgroup/memory.stat").splitlines())
    current = int(sh("cat", "/sys/fs/cgroup/memory.current").strip())
    ws = (current - int(stat["inactive_file"])) / 1048576
    procs = [line.split(None, 1) for line in sh("ps", "-eo", "rss=,args=").splitlines() if line.strip()]
    chrome = [(int(r), a) for r, a in procs if "chrom" in a.lower()]
    mains = [a for _, a in chrome if "--type=" not in a]
    py = sum(int(r) for r, a in procs if "python" in a or "gunicorn" in a) / 1024
    rec = {"variant": LABEL, "step": step, "t": time.strftime("%H:%M:%S"),
           "ws_mib": round(ws), "chrome_browsers": len(mains),
           "chrome_procs": len(chrome), "chrome_rss_mib": round(sum(r for r, _ in chrome) / 1024),
           "python_rss_mib": round(py)}
    print(json.dumps(rec), flush=True)
    with open(OUT, "a") as f:
        f.write(json.dumps(rec) + "\n")
    return rec


def log(step, data):
    rec = {"variant": LABEL, "step": step, **data}
    print(json.dumps(rec), flush=True)
    with open(OUT, "a") as f:
        f.write(json.dumps(rec) + "\n")


def main():
    time.sleep(20)
    measure("boot")
    for u in URLS:
        log("crawl " + u, crawl(u))
    measure("after 5 generic crawls")
    log("crawl alt-fingerprint", crawl(URLS[0], alt=True))
    measure("after alt-fingerprint crawl")
    log("execute_js", execute_js("https://example.com/"))
    measure("after execute_js")
    for u in URLS[:2]:
        log("crawl again " + u, crawl(u))
    measure("after 2 more crawls")
    # config sets idle_ttl_sec 60: cold browsers close after 60 s idle,
    # hot after 120 s; the janitor wakes every 60 s.
    time.sleep(200)
    measure("after 200 s idle")


if __name__ == "__main__":
    main()
