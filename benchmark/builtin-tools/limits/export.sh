#!/bin/bash
# Export the rate-limit datasets from VictoriaLogs. VL=<VictoriaLogs URL>, NS=<LogsQL stream filter>.
# The exports hold the URLs and queries your clients sent: keep them out of git.
set -euo pipefail
cd "$(dirname "$0")"
VL=${VL:-http://localhost:9428}
NS=${NS:-'kubernetes.pod_namespace:omnifeed'}  # LogsQL filter selecting omnifeed's pods
q() { curl -sf "$VL/select/logsql/query" --data-urlencode "query=$1" > "$2.tmp" && mv "$2.tmp" "$2"; wc -l "$2"; }

q "_time:120d $NS (\"mcp tool call completed\" OR \"mcp tool call failed\") | fields _time, _msg, tool, args.url, args.query, args.site, chars, duration_ms, err, kubernetes.pod_name" calls.jsonl
q "_time:120d $NS \"searxng returned partial results\" | fields _time, results, unresponsive_engines, kubernetes.pod_name" partial.jsonl
q "_time:120d $NS \"search audit\" | fields _time, query_id, duration_ms, engine_rows, site, site_scoped, time_range, total" audit.jsonl
q "_time:120d $NS \"search engine unresponsive\" | fields _time, engine, reason, reason_class, query_id" unresponsive.jsonl
