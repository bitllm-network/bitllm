#!/bin/sh
# Publishes site/ to the directory bitllm.org serves. It takes the newest commit
# on main whose `test` workflow passed, so a red push never reaches the site.
# Runs every few minutes next to the web server:
#   scripts/site-sync.sh ~/bitllm-site ~/bitllm-boot/site-sync.sha
# Files that are not in site/, such as network.json, are left alone.
set -eu
DIR="${1:?usage: scripts/site-sync.sh SITE_DIR STATE_FILE}"
STATE="${2:?usage: scripts/site-sync.sh SITE_DIR STATE_FILE}"
REPO=bitllm-org/bitllm
RUNS="https://api.github.com/repos/$REPO/actions/workflows/test.yml/runs?branch=main&event=push&status=success&per_page=1"

sha=$(curl -fsS -m 20 "$RUNS" | jq -r '.workflow_runs[0].head_sha // empty')
case "$sha" in
  *[!0-9a-f]* | "") echo "site-sync: no green commit found" >&2; exit 1 ;;
esac
[ "$(cat "$STATE" 2>/dev/null || true)" = "$sha" ] && exit 0

tmp=$(mktemp -d "$(dirname "$STATE")/.site-sync.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -m 60 -o "$tmp/src.tar.gz" "https://codeload.github.com/$REPO/tar.gz/$sha"
mkdir "$tmp/site"
tar -xzf "$tmp/src.tar.gz" -C "$tmp/site" --strip-components=2 "bitllm-$sha/site"

for f in index.html llms.txt install.sh install.ps1; do
  [ -s "$tmp/site/$f" ] || { echo "site-sync: $f missing at $sha" >&2; exit 1; }
done
sh -n "$tmp/site/install.sh"

for f in "$tmp"/site/*; do
  [ -f "$f" ] && mv "$f" "$DIR/$(basename "$f")"
done
echo "$sha" > "$STATE"
echo "site-sync: published $sha"
