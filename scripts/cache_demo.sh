#!/usr/bin/env bash
# FR-C1 live demo: explicit context caching through the gateway on real Vertex.
# Vertex requires >= 1024 prefix tokens for explicit caching; the pilot's agent
# prompts are ~400 tokens and Vertex counts only `contents` toward the
# minimum, so in the web UI they correctly report BYPASS with Vertex's reason.
# This sends a long operating manual as the first turns with breakpoint
# after_turn:2: request 1 = MISS (cache created),
# request 2 = HIT (Vertex reports cachedContentTokenCount).
set -euo pipefail
cd "$(dirname "$0")/.."
GW=${GW:-http://127.0.0.1:8081}
python3 scripts/cache_demo_body.py > /tmp/va_cache_body.json
for i in 1 2; do
  echo "--- request $i"
  curl -s -D /tmp/va_h.$i -o /tmp/va_b.$i "$GW/v1/responses" \
    -H 'X-VA-Cache-Breakpoint: after_turn:2' -H 'X-VA-Cache-TTL: 300s' -d @/tmp/va_cache_body.json
  grep -i '^x-va-cache' /tmp/va_h.$i
  python3 -c 'import json,sys;u=json.load(open(sys.argv[1]))["usage"];print("input_tokens",u["input_tokens"],"cached_tokens",u["input_tokens_details"]["cached_tokens"])' /tmp/va_b.$i
done
echo "--- gateway cache stats"
curl -s "$GW/v1/cache/stats" | python3 -c 'import json,sys;d=json.load(sys.stdin);d.pop("last_bypass_reason",None);print(json.dumps(d,indent=2))'
