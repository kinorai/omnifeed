package reddit

import (
	"fmt"
	"strings"
)

// Reddit closed its logged-out .json endpoints in 2026: a .json fetch answers
// 403 "blocked by network security", while the HTML page of the same thread
// still loads for a logged-out browser. So the snippets below read the page
// the browser already rendered (shreddit-post / shreddit-comment elements) and
// rebuild the .json wire shape from it, which ParseThread and
// ParseSubredditListing then read unchanged.

// maxExpandRequests caps the "more replies" POSTs one thread fetch sends. Each
// is one request to Reddit, the same one a reader triggers with a click.
const maxExpandRequests = 40

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
const notFound = () => JSON.stringify({s: 200, b: document.body ? document.body.innerText.slice(0, 500) : ""});
`

// threadHTMLJS returns the snippet that turns the rendered thread page into
// the [postListing, commentListing] pair a .json thread fetch returned. It
// expands collapsed branches by POSTing the page's own more-comments forms,
// until the page holds limit comments, the request cap is spent, or no branch
// is left. Branches it did not open come back as "more" gaps with no child IDs,
// so the engine reports them without trying /api/morechildren.
func threadHTMLJS(limit int) string {
	return htmlCommon + fmt.Sprintf(`
const LIMIT = %d, MAX_REQ = %d, DELAY = %d;
const p = document.querySelector("shreddit-post");
if (!p) return notFound();
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
const done = new Set();
// Two kinds of collapsed branch: a more-comments form (POSTed for the next
// replies), and, below depth 4, a "more replies" link to the branch's own
// permalink page (fetched like a reader following it).
const pending = () => [...document.querySelectorAll(
  'faceplate-partial[src*="/more-comments/"], a[slot="more-comments-permalink"]')]
  .filter(e => !done.has(e));
let sent = 0, limited = null;
for (let next = pending(); !limited && next.length && sent < MAX_REQ && things.length < LIMIT; next = pending()) {
  for (const part of next) {
    if (sent >= MAX_REQ || things.length >= LIMIT) break;
    done.add(part);
    if (sent > 0) await new Promise(r => setTimeout(r, DELAY));
    sent++;
    let r;
    if (part.tagName === "A") {
      r = await fetch(A(part, "href"));
    } else {
      const form = new URLSearchParams();
      part.querySelectorAll("input[name]").forEach(i => form.set(i.name, i.value));
      if (csrf) form.set("csrf_token", csrf);
      r = await fetch(A(part, "src"), {method: "POST", body: form,
        headers: {"Content-Type": "application/x-www-form-urlencoded"}});
    }
    // A refused expansion keeps what the page already gave: stop expanding,
    // report the rest as gaps, and pass Reddit's back-off on to the limiter.
    if (r.status !== 200) {
      limited = {ra: r.headers.get("retry-after"), rs: r.headers.get("x-ratelimit-reset"),
        rm: r.status === 429 ? "0" : r.headers.get("x-ratelimit-remaining")};
      break;
    }
    // Rendered off-screen inside the live page, so innerText keeps the
    // paragraph breaks a detached DOMParser document would lose.
    const box = document.createElement("div");
    box.style.cssText = "position:absolute;left:-99999px;top:0;width:800px";
    box.innerHTML = await r.text();
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
return JSON.stringify({s: 200, ...limited, b: JSON.stringify([
  {kind: "Listing", data: {children: [post(p)]}},
  {kind: "Listing", data: {children: things.concat(gaps)}}])});
`, limit, maxExpandRequests, expandDelayMS)
}

// listingHTMLJS returns the snippet that turns a rendered subreddit page into
// the Listing a .json listing fetch returned. It reads only the posts the
// first page renders (about 25): more would need the feed's scroll loader.
func listingHTMLJS(limit int) string {
	return htmlCommon + fmt.Sprintf(`
const ps = [...document.querySelectorAll("shreddit-post")].slice(0, %d);
if (!ps.length) return notFound();
return JSON.stringify({s: 200, b: JSON.stringify({kind: "Listing", data: {children: ps.map(post)}})});
`, limit)
}

// threadPageURL is the thread page, sorted the way the caller asked. The page
// takes the same sort names as the .json endpoint (top, new, confidence, …).
func threadPageURL(permalink, sort string) string {
	return redditOrigin + strings.TrimSuffix(permalink, "/") + "/?sort=" + sort
}
