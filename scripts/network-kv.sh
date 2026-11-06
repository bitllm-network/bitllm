#!/bin/sh
# Copies a node's GET /network into Cloudflare KV every two minutes. The
# Worker behind bitllm.org serves it as /network.json for the live map, so
# the machine running this never accepts inbound requests.
#
# Two minutes is 720 writes a day, under the free tier limit of 1,000.
#
#   CLOUDFLARE_ACCOUNT_ID=... CLOUDFLARE_API_TOKEN=... \
#   scripts/network-kv.sh http://127.0.0.1:7778 NAMESPACE_ID
#
# Pass "once" as the third argument to publish one snapshot and exit; a
# failed upload then exits non-zero. That is the mode the scheduled GitHub
# workflow uses, so the map no longer depends on a laptop staying awake.
#
# The token needs Workers KV Storage:Edit on the account. The value holds
# counts per model and per ~156 km cell; it names no peer ids and no
# addresses (see internal/netmap and docs/network-map.md).
#
# The key expires an hour after the last write, so if this stops the site
# shows an empty network rather than an old one as if it were current.
set -eu
GATEWAY="${1:?usage: network-kv.sh GATEWAY_URL NAMESPACE_ID [once]}"
NS="${2:?usage: network-kv.sh GATEWAY_URL NAMESPACE_ID [once]}"
MODE="${3:-loop}"
: "${CLOUDFLARE_ACCOUNT_ID:?set CLOUDFLARE_ACCOUNT_ID}"
: "${CLOUDFLARE_API_TOKEN:?set CLOUDFLARE_API_TOKEN}"
URL="https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID/storage/kv/namespaces/$NS/values/network?expiration_ttl=3600"
TMP=$(mktemp "${TMPDIR:-/tmp}/bitllm-network.XXXXXX")
trap 'rm -f "$TMP"' EXIT

while :; do
  ok=0
  if curl -sf -m 10 "$GATEWAY/network" -o "$TMP" &&
     python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert "cells" in d' "$TMP" 2>/dev/null; then
    # The token goes on curl's stdin as config: printf is a shell builtin, and
    # anything on curl's command line shows in the process list.
    if printf 'header = "Authorization: Bearer %s"\n' "$CLOUDFLARE_API_TOKEN" | curl -K - -sf -m 10 -X PUT "$URL" \
      -H "Content-Type: application/json" \
      --data-binary @"$TMP" > /dev/null; then
      ok=1
    else
      echo "network-kv: upload failed" >&2
    fi
  else
    echo "network-kv: $GATEWAY/network unavailable or invalid" >&2
  fi
  if [ "$MODE" = once ]; then
    test "$ok" = 1 || exit 1
    exit 0
  fi
  sleep 120
done
