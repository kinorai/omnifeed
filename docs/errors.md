# Error codes

When a fetch or search fails, omnifeed reports one stable **error code** on every
front-end. Clients should branch on the code, never on the message text: the
message is for humans and changes wording between releases.

## Codes

| Code | Meaning | Retryable | What to do |
| --- | --- | --- | --- |
| `rate_limited` | The site (or an API it sits behind) answered HTTP 429. | yes | Back off, then retry (no `retry_after_s`: the upstream's `Retry-After` is not forwarded). |
| `blocked` | The site refused the request: HTTP 403, or a bot wall without a clean status. | no | Use another URL or source. |
| `captcha` | A CAPTCHA or human-verification challenge was served in place of the page. | no | Use another URL or source. |
| `timeout` | The fetch ran out of time (omnifeed's budget, a navigation timeout, or crawl4ai's own time limit / HTTP 504). | yes | Retry once; a page that times out twice is likely to keep doing so. |
| `thin_content` | The page rendered, but with too little usable content (a JS-only shell, a PDF, a near-empty page). | no | Use another URL; for feeds try `scan_full_page`. |
| `not_found` | The page does not exist: HTTP 404/410, or a "page not found" page served with a success status (soft 404). | no | Check the URL; search for the current location. |
| `upstream_error` | crawl4ai, SearXNG or a source API failed, or the failure could not be classified. | see the `retryable` flag | Retry when `retryable` is true. |
| `quota_exhausted` | omnifeed's own per-host pacing quota is spent; nothing was sent. | yes | Wait `retry_after_s`, then retry. |
| `invalid_request` | The request itself is wrong: a malformed URL, a non-http(s) scheme, or a private address. | no | Fix the request. |

## Mapping from failure reasons

The `reason` label on omnifeed's metrics and logs (the internal `FailureKind`) is
finer-grained than the codes. Each reason maps to exactly one code:

| Reason (`FailureKind`) | Code | Retryable |
| --- | --- | --- |
| `http_429` | `rate_limited` | yes |
| `http_403` | `blocked` | no |
| `bot_block` | `blocked` | no |
| `captcha` | `captcha` | no |
| `timeout` | `timeout` | yes |
| `canceled` | `timeout` | yes |
| `thin_content` | `thin_content` | no |
| `not_found` | `not_found` | no |
| `site_error` | `upstream_error` | yes (the requested site answered 5xx; not an omnifeed or crawl4ai fault) |
| `upstream_error` | `upstream_error` | yes |
| `upstream_rejected` | `upstream_error` | no (already retried once; usually a per-page verdict) |
| `bad_response` | `upstream_error` | yes |
| `error` | `upstream_error` | no |
| `quota_exhausted` | `quota_exhausted` | yes |
| (URL rejected before any fetch) | `invalid_request` | no |

### How crawl4ai failures are classified

From crawl4ai 0.9.3 on, `/crawl` answers HTTP 200 with a top-level
`success: true` even when the page failed; the verdict is in
`results[0].success` and `results[0].error_message`. omnifeed treats
`results[0].success: false` as a failure and reads the message by stable
patterns, most specific first:

1. crawl4ai's content gate (`Structural:`, `minimal_text`, `no <body>`) → `thin_content`
2. `exceeded the time limit`, `timeout`, `timed out`, `net::ERR_TIMED_OUT` → `timeout`
3. `captcha`, `Just a moment`, `Turnstile`, `verify you are human` → `captcha`
4. `HTTP 429`, `Too Many Requests`, `rate limit` → `http_429`
5. `HTTP 403`, `403 Forbidden` → `http_403`
6. `anti-bot`, `blocked`, or a named WAF (Cloudflare, Akamai, PerimeterX, DataDome, Incapsula/Imperva) → `bot_block`
7. anything else (HTTP 5xx, other `net::ERR_*`, crashes) → `upstream_error`

When the message names nothing but the result's own `status_code` is 403 or 429,
that status decides. The raw message is kept in the error detail, flattened and
cut to 300 characters. A crawl4ai 5xx whose body has a FastAPI `detail` string
is classified from that string; an HTTP 504 is always `timeout`; the scrubbed
HTTP 500 of crawl4ai 0.9.2+ (`{"error":"Internal server error","correlation_id":…}`)
stays `upstream_rejected`.

## Where the code appears

### MCP (`tools/call`)

A tool that ran and failed returns a **result** with `isError: true`, not a
JSON-RPC error:

```json
{
  "jsonrpc": "2.0",
  "id": 7,
  "result": {
    "content": [
      {"type": "text", "text": "fetch_url failed: quota_exhausted: pacing quota exhausted; retry in 90s [quota_exhausted] Retryable after 90s."},
      {"type": "text", "text": "{\"code\":\"quota_exhausted\",\"retry_after_s\":90,\"retryable\":true,\"url\":\"https://example.com/a\"}"}
    ],
    "structuredContent": {
      "code": "quota_exhausted",
      "retryable": true,
      "retry_after_s": 90,
      "url": "https://example.com/a"
    },
    "isError": true
  }
}
```

- `content[0]` is a short human-readable sentence: what failed, why, and whether to retry.
- `structuredContent` always has `code` and `retryable`; `retry_after_s` (today only
  on `quota_exhausted`) and `upstream_status` appear only when known; `url` appears when the call had a `url`
  argument (`fetch_url`).
- `content[1]` repeats `structuredContent` as JSON text, as the MCP spec recommends for
  clients that predate `structuredContent` (added in protocol 2025-06-18). Older clients
  ignore the unknown field.
- Modern-era (2026-07-28) requests get the usual `resultType` and `_meta` on this result too.

JSON-RPC errors are kept for protocol-level problems only: malformed params and a
missing required argument (`-32602`), an unknown tool (`-32602`), an unknown method
(`-32601`), version/header mismatches (`-32020`, `-32022`). Authentication fails at the
HTTP layer with `401`.

Before this change, tool failures were JSON-RPC errors with code `-32603` and the
human sentence as `message`. Clients that relied on that must also accept the
`isError` result.

### Open WebUI loader (`POST /crawl`)

A failed URL still comes back as a document whose `page_content` starts with
`Error crawling URL:`. Its `metadata` carries `error: "true"`, `error_code`,
`retryable` (`"true"`/`"false"`) and, when known, `retry_after_s`.

### REST search (`POST /search`)

An upstream failure is HTTP 502 with
`{"error": "search upstream failed: …", "code": "…", "retryable": …}` plus
`retry_after_s` / `upstream_status` when known. Request errors (400, 401, 405) keep
their `{"error": "…"}` body.
