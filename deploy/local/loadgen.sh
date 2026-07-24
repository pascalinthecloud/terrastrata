#!/usr/bin/env bash
# Generate realistic mirror traffic against a running terrastrata, so the Grafana
# dashboard has something to show. Hits the same endpoints terraform does:
# versions index -> archives index -> provider zip. Talks to the plaintext mirror
# directly, so it needs no TLS or terraform.
#
#   ./loadgen.sh                                  # 120s, 4 workers, default providers
#   DURATION=600 CONCURRENCY=8 ./loadgen.sh       # heavier, longer
#   DOWNLOAD_ZIPS=0 ./loadgen.sh                  # metadata only (no zip bytes)
#   ./loadgen.sh hashicorp/azurerm hashicorp/aws  # custom providers (bigger = more cache)
set -euo pipefail

MIRROR="${MIRROR:-http://localhost:8080}"
HOST="${HOST:-registry.terraform.io}"
PLATFORM="${PLATFORM:-linux_amd64}"
DURATION="${DURATION:-120}"
CONCURRENCY="${CONCURRENCY:-4}"
DOWNLOAD_ZIPS="${DOWNLOAD_ZIPS:-1}"

if [ "$#" -gt 0 ]; then
  PROVIDERS=("$@")
else
  PROVIDERS=(hashicorp/null hashicorp/random hashicorp/local hashicorp/tls)
fi

command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 1; }

deadline=$(( $(date +%s) + DURATION ))

# One worker: until the deadline, pick a random provider and walk
# index.json -> <version>.json -> (optional) zip, exactly as terraform would.
worker() {
  while [ "$(date +%s)" -lt "$deadline" ]; do
    p="${PROVIDERS[$((RANDOM % ${#PROVIDERS[@]}))]}"
    ns="${p%%/*}"; type="${p##*/}"
    base="$MIRROR/$HOST/$ns/$type"

    index="$(curl -fsS "$base/index.json" 2>/dev/null)" || { sleep 0.2; continue; }
    ver="$(printf '%s' "$index" | python3 -c \
      'import sys,json,random;v=list(json.load(sys.stdin).get("versions",{}));print(random.choice(v) if v else "")')"
    [ -n "$ver" ] || continue

    archives="$(curl -fsS "$base/$ver.json" 2>/dev/null)" || continue

    if [ "$DOWNLOAD_ZIPS" = "1" ]; then
      url="$(printf '%s' "$archives" | python3 -c \
        'import sys,json;a=json.load(sys.stdin).get("archives",{}).get("'"$PLATFORM"'");print(a["url"] if a else "")')"
      [ -n "$url" ] && curl -fsS -o /dev/null "$base/$url" 2>/dev/null || true
    fi
  done
}

echo "load: ${#PROVIDERS[@]} providers, ${CONCURRENCY} workers, ${DURATION}s, zips=${DOWNLOAD_ZIPS} -> $MIRROR"
for _ in $(seq "$CONCURRENCY"); do worker & done
wait
echo "done"
