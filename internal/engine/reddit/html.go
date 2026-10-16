package reddit

import (
	"fmt"
	"net/url"
	"strings"
)

// Reddit closed its logged-out .json endpoints in 2026: a .json fetch answers
// 403 "blocked by network security", while the HTML page of the same thread
// still loads for a logged-out browser. So the snippets below read the page
// the browser already rendered (shreddit-post / shreddit-comment elements) and
// rebuild the .json wire shape from it, which ParseThread and
// ParseSubredditListing then read unchanged.

// maxExpandRequests caps the "more replies" requests one thread fetch sends,
// whatever expand asks for. Each is one request to Reddit, the same one a
// reader triggers with a click. The page sends them itself, so the quota sees
// them only afterwards: the envelope reports how many went out and the session
// charges them (Session.charge). At 40, Reddit answered 429 after about 2
// large threads (2026-10-15).
const maxExpandRequests = 10

// expandDelayMS spaces those POSTs, so the expansion reads like a person
// clicking through the thread rather than a burst.
const expandDelayMS = 300

// htmlCommon holds the helpers both snippets share. It ends without a return,
// so each snippet appends its own body.
const htmlCommon = `
const A = (e, k) => e.getAttribute(k);
const num = s => parseInt(s || "0", 10) || 0;
const ts = s => s ? Math.floor(Date.parse(s) / 1000) : 0;
const txt = e => e ? e.innerText.trim() : "";
// A GIF or image reply renders as a <figure> with no text. Keep its URL, as
// Reddit's JSON body did, rather than an empty comment.
const body = e => txt(e) || (e ? [...new Set([...e.querySelectorAll("img[src], video[src], a[href]")]
  .map(m => m.getAttribute("src") || m.getAttribute("href")))].join("\n") : "");
const post = p => ({kind: "t3", data: {
  id: (A(p, "id") || "").replace(/^t3_/, ""), title: A(p, "post-title") || "",
  author: A(p, "author") || "[deleted]", subreddit: A(p, "subreddit-name") || "",
  score: num(A(p, "score")), upvote_ratio: parseFloat(A(p, "upvote-ratio") || "0") || 0,
  num_comments: num(A(p, "comment-count")), created_utc: ts(A(p, "created-timestamp")),
  url: A(p, "content-href") || "", permalink: A(p, "permalink") || "",
  selftext: txt(p.querySelector('[slot="text-body"]'))}});
// The page has no post: a thread or subreddit that is gone or refused, a wall,
// or markup this snippet no longer reads. The navigation's own status and the
// page text go back, so Go can tell which (fetchEnvelope.missing).
const missing = () => {
  const nav = performance.getEntriesByType("navigation")[0];
  return JSON.stringify({s: (nav && nav.responseStatus) || 200, nf: 1,
    b: document.body ? document.body.innerText.slice(0, 500) : ""});
};
`

// threadHTMLJS returns the snippet that turns the rendered thread page into
// the [postListing, commentListing] pair a .json thread fetch returned. It
// expands collapsed branches by POSTing the page's own more-comments forms,
// until the page holds limit comments, maxReq requests are spent, or no branch
// is left. Branches it did not open come back as "more" gaps with no child IDs,
// so the engine reports them without trying /api/morechildren. The envelope's
// n is the requests sent.
func threadHTMLJS(limit, maxReq int) string {
	return htmlCommon + fmt.Sprintf(`
const LIMIT = %d, MAX_REQ = %d, DELAY = %d;
const p = document.querySelector("shreddit-post");
if (!p) return missing();
const postID = A(p, "id");
const seen = new Set(), things = [];
const take = root => root.querySelectorAll("shreddit-comment").forEach(c => {
  const id = A(c, "thingid");
  if (!id || seen.has(id)) return;
  seen.add(id);
  things.push({kind: "t1", data: {
    id: id.replace(/^t1_/, ""), parent_id: A(c, "parentid") || postID, link_id: postID,
    author: A(c, "author") || "[deleted]", score: num(A(c, "score")),
    body: body(document.getElementById(id + "-comment-rtjson-content")),
    depth: num(A(c, "depth")), created_utc: ts(A(c, "created")), replies: ""}});
});
take(document);
const csrf = (document.cookie.match(/(?:^|; )csrf_token=([^;]+)/) || [])[1] || "";
// Two kinds of collapsed branch: a more-comments form (POSTed for the next
// replies), and, below depth 4, a "more replies" link to the branch's own
// permalink page (fetched like a reader following it). Each fetched fragment
// or page brings new copies of branches already seen, so a branch is known by
// what it loads: the comment a link opens, or a form's URL and cursor. Keyed by
// element instead, one branch was fetched again and again until Reddit refused.
const keyOf = e => {
  if (e.tagName === "A") {
    const m = (A(e, "href") || "").match(/\/comment\/([a-z0-9]+)/i);
    return "c:" + (m ? m[1] : A(e, "href"));
  }
  return "f:" + A(e, "src") + "|" + [...e.querySelectorAll("input[name]")].map(i => i.name + "=" + i.value).join("&");
};
const done = new Set();
const pending = () => {
  const keys = new Set();
  return [...document.querySelectorAll(
    'faceplate-partial[src*="/more-comments/"], a[slot="more-comments-permalink"]')]
    .filter(e => {
      const k = keyOf(e);
      if (done.has(k) || keys.has(k)) return false;
      keys.add(k);
      return true;
    });
};
let sent = 0, limited = null;
for (let next = pending(); !limited && next.length && sent < MAX_REQ && things.length < LIMIT; next = pending()) {
  for (const part of next) {
    if (sent >= MAX_REQ || things.length >= LIMIT) break;
    done.add(keyOf(part));
    if (sent > 0) await new Promise(r => setTimeout(r, DELAY));
    sent++;
    let r, html;
    try {
      if (part.tagName === "A") {
        r = await fetch(A(part, "href"));
      } else {
        const form = new URLSearchParams();
        part.querySelectorAll("input[name]").forEach(i => form.set(i.name, i.value));
        if (csrf) form.set("csrf_token", csrf);
        r = await fetch(A(part, "src"), {method: "POST", body: form,
          headers: {"Content-Type": "application/x-www-form-urlencoded"}});
      }
      if (r.status === 200) html = await r.text();
    } catch (e) {
      // A dropped request must not throw away the thread already read.
      limited = {xs: -1};
      break;
    }
    // A branch that is gone (404) is skipped: it is not a refusal.
    if (r.status === 404) continue;
    // A refused expansion keeps what the page already gave: stop expanding,
    // report the rest as gaps, and say why (xs, the refused status) along
    // with Reddit's back-off, which holds the method.
    if (r.status !== 200) {
      limited = {ra: r.headers.get("retry-after"), rs: r.headers.get("x-ratelimit-reset"),
        rm: r.headers.get("x-ratelimit-remaining"), xs: r.status};
      break;
    }
    // Rendered off-screen inside the live page, so innerText keeps the
    // paragraph breaks a detached DOMParser document would lose.
    const box = document.createElement("div");
    box.style.cssText = "position:absolute;left:-99999px;top:0;width:800px";
    box.innerHTML = html;
    document.body.appendChild(box);
    take(box);
  }
}
// A permalink page numbers depth from its own focus comment, so rebuild every
// depth from the parent chain instead of trusting the attribute.
const depthOf = new Map();
for (const t of things) {
  const p = t.data.parent_id.replace(/^t1_/, "");
  t.data.depth = depthOf.has(p) ? depthOf.get(p) + 1 : 0;
  depthOf.set(t.data.id, t.data.depth);
}
const gaps = pending().map(e => {
  const u = new URL(A(e, "src") || A(e, "href"), location.href);
  const parent = e.closest("shreddit-comment");
  return {kind: "more", data: {count: num(u.searchParams.get("comments-remaining")),
    parent_id: parent ? A(parent, "thingid") : postID,
    depth: parent ? (depthOf.get(A(parent, "thingid").replace(/^t1_/, "")) ?? 0) + 1 : 0,
    children: []}};
});
return JSON.stringify({s: 200, ...limited, n: sent, b: JSON.stringify([
  {kind: "Listing", data: {children: [post(p)]}},
  {kind: "Listing", data: {children: things.concat(gaps)}}])});
`, limit, maxReq, expandDelayMS)
}

// The listing snippet's timings, in ms. It checks the feed every listingPollMS,
// scrolls to the bottom after listingScrollAfterMS and again after twice that
// with no new post, and stops after listingStallMS with none (the sub has no
// more), or after listingMaxMS in all. A page with no feed element after
// listingNoFeedMS (a wall, a gone or refused sub) is read as is.
const (
	listingPollMS        = 250
	listingScrollAfterMS = 1000
	listingStallMS       = 3000
	listingMaxMS         = 15000
	listingNoFeedMS      = 1000
)

// listingHTMLJS returns the snippet that turns a rendered subreddit page into
// the Listing a .json listing fetch returned. The compact view (FetchListing)
// holds about 9 posts when the page reports loaded and 34 within 3s; each
// scroll to the bottom then loads 25 more. Every batch, the first one
// included, is a fetch of /svc/shreddit/community-more-posts/: one request to
// Reddit that the page sends itself, like the thread snippet's expansions: the
// envelope's n reports them so the session charges them to the quota. 100
// posts cost 1+4 requests; the default 25 needs no scroll.
// The page drops the posts scrolled past, so each post is read as soon as it
// shows up. A batch Reddit refused, or the listingMaxMS cap, comes back as the
// envelope's xs (status, or -1), so the listing is reported partial. Measured
// 2026-10-16, top-of-week: 100 posts in about 11s per call, a sub's whole
// 42-post week in 8s, 25 posts in 4s.
func listingHTMLJS(limit int) string {
	return htmlCommon + fmt.Sprintf(`
const LIMIT = %d;
const POLL = %d, SCROLL_AFTER = %d, STALL = %d, MAX = %d, NO_FEED = %d;
const read = new WeakSet(), ids = new Set(), got = [];
const grab = () => {
  for (const p of document.querySelectorAll("shreddit-post")) {
    if (read.has(p)) continue;
    read.add(p);
    const t = post(p);
    // A post with no id is kept (Go reports the listing partial, or a layout
    // change when none has one); one already read is not.
    if (t.data.id && ids.has(t.data.id)) continue;
    if (t.data.id) ids.add(t.data.id);
    got.push(t);
  }
  return got.length;
};
// The status of the first feed batch Reddit refused, 0 while none was. An entry
// is recorded once its response is in; 0 there is unknown, not a refusal.
const batches = () => performance.getEntriesByType("resource").filter(e =>
  e.name.includes("/svc/shreddit/community-more-posts/"));
const refused = () => {
  const e = batches().find(e => e.responseStatus > 0 && e.responseStatus !== 200);
  return e ? e.responseStatus : 0;
};
let xs = 0;
for (let waited = 0, last = grab(), still = 0; last < LIMIT; ) {
  if (waited >= MAX) { xs = -1; break; }
  await new Promise(r => setTimeout(r, POLL));
  waited += POLL;
  const n = grab();
  still = n === last ? still + POLL : 0;
  last = n;
  if ((xs = refused())) break;
  if (!n && waited >= NO_FEED && !document.querySelector("shreddit-feed")) break;
  if (still >= STALL) break;
  if (n && still > 0 && still %% SCROLL_AFTER === 0) window.scrollTo(0, document.body.scrollHeight);
}
const ps = got.slice(0, LIMIT);
if (!ps.length) return missing();
return JSON.stringify({s: 200, xs, n: batches().length, b: JSON.stringify({kind: "Listing", data: {children: ps}})});
`, limit, listingPollMS, listingScrollAfterMS, listingStallMS, listingMaxMS, listingNoFeedMS)
}

// threadPageURL is the thread page, sorted the way the caller asked. The page
// takes the same sort names as the .json endpoint (top, new, confidence, …).
func threadPageURL(permalink, sort string) string {
	return redditOrigin + strings.TrimSuffix(permalink, "/") + "/?sort=" + url.QueryEscape(sort)
}
