#!/bin/sh
# Writes internal/assets/fonts.css from the font files in bitllm-org/branding,
# with their licenses in a comment: the OFL requires each copy to carry them,
# and this file is what the repository, the binary and the page all contain.
#   scripts/fonts-css.sh ../branding/fonts
set -eu
dir=$(cd "${1:?usage: scripts/fonts-css.sh path/to/branding/fonts}" && pwd)
cd "$(dirname "$0")/.."

for f in jetbrains-mono-400.woff2 jetbrains-mono-400i.woff2 jetbrains-mono-700.woff2 \
  arimo-400.woff2 arimo-700.woff2 OFL-JetBrainsMono.txt OFL-Arimo.txt; do
  [ -s "$dir/$f" ] || { echo "fonts-css: $dir/$f is missing or empty" >&2; exit 1; }
done

face() { # family weight style file
  data=$(base64 < "$dir/$4" | tr -d '\n')
  printf '@font-face { font-family: "%s"; font-weight: %s; font-style: %s; src: url("data:font/woff2;base64,%s") format("woff2"); }\n' \
    "$1" "$2" "$3" "$data"
}
out=internal/assets/fonts.css
{
  printf '/* The dashboard bundles JetBrains Mono and Arimo. Their licenses follow.\n\n'
  cat "$dir/OFL-JetBrainsMono.txt"
  printf '\n'
  cat "$dir/OFL-Arimo.txt"
  printf '*/\n'
  face "JetBrains Mono" 400 normal jetbrains-mono-400.woff2
  face "JetBrains Mono" 400 italic jetbrains-mono-400i.woff2
  face "JetBrains Mono" 700 normal jetbrains-mono-700.woff2
  face "Arimo" 400 normal arimo-400.woff2
  face "Arimo" 700 normal arimo-700.woff2
} > "$out.tmp"
mv "$out.tmp" "$out"
