#!/usr/bin/env bash
# Sequential TinyFish Search run, 4s apart (free tier is 30 req/min).
set -euo pipefail
out=raw/tinyfish; mkdir -p "$out"
while IFS=$'\t' read -r id q; do
  curl -s -m 30 -G -H @/tmp/tf_hdr --data-urlencode "query=$q" \
    -w '\n%{http_code} %{time_total}\n' https://api.search.tinyfish.ai > "$out/$id.raw"
  tail -1 "$out/$id.raw" | sed "s/^/$id /"
  sleep 4
done < queries.txt
