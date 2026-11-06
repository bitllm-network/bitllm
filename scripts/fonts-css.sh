#!/bin/sh
# Writes internal/assets/fonts.css from the font files in bitllm-org/branding.
#   scripts/fonts-css.sh ../branding/fonts
set -eu
dir=${1:?usage: scripts/fonts-css.sh path/to/branding/fonts}
cd "$(dirname "$0")/.."

face() { # family weight style file
  printf '@font-face { font-family: "%s"; font-weight: %s; font-style: %s; src: url("data:font/woff2;base64,%s") format("woff2"); }\n' \
    "$1" "$2" "$3" "$(base64 < "$dir/$4" | tr -d '\n')"
}
{
  face "JetBrains Mono" 400 normal jetbrains-mono-400.woff2
  face "JetBrains Mono" 400 italic jetbrains-mono-400i.woff2
  face "JetBrains Mono" 700 normal jetbrains-mono-700.woff2
  face "Arimo" 400 normal arimo-400.woff2
  face "Arimo" 700 normal arimo-700.woff2
} > internal/assets/fonts.css
