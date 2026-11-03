#!/bin/sh
# Copies a node's GET /network into a static network.json for the website,
# once a minute. bitllm.org runs this next to the bootstrap node, so the map
# on the landing page reads a plain file and no node API is exposed publicly.
#   scripts/network-json.sh http://127.0.0.1:7779 ~/bitllm-site
# The file only holds counts per model and per ~156 km cell (see network.go);
# it names no peer ids and no addresses.
set -eu
GATEWAY="${1:?usage: scripts/network-json.sh GATEWAY_URL SITE_DIR}"
DIR="${2:?usage: scripts/network-json.sh GATEWAY_URL SITE_DIR}"
while :; do
  if curl -sf -m 10 "$GATEWAY/network" -o "$DIR/.network.json.tmp" &&
     python3 -c 'import json,sys; d=json.load(open(sys.argv[1])); assert "cells" in d' "$DIR/.network.json.tmp" 2>/dev/null; then
    mv "$DIR/.network.json.tmp" "$DIR/network.json"
  fi
  sleep 60
done
