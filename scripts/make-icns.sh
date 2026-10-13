#!/usr/bin/env bash
# Rebuild client/cmd/tacit/macos/tacit.icns from client/cmd/tacit/reviewui/icon.svg.
#
# The .icns is committed, so a Mac build needs nothing from this script; run it
# after the SVG changes. It renders the SVG once at 1024 px with headless
# Chrome (ImageMagick's own SVG renderer drops the gradients, the filter and
# the rotations), scales that down for each size, and packs the PNGs into an
# icns container (a header plus typed PNG chunks, written here with plain
# Python). Linux or macOS; needs Chrome or Chromium, ImageMagick (magick or
# convert) and python3.
#
# Usage: scripts/make-icns.sh [OUT]   (default client/cmd/tacit/macos/tacit.icns)
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
svg="$root/client/cmd/tacit/reviewui/icon.svg"
out="${1:-$root/client/cmd/tacit/macos/tacit.icns}"

chrome="${CHROME:-}"
if [[ -z "$chrome" ]]; then
  for c in google-chrome chromium chromium-browser "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"; do
    if command -v "$c" >/dev/null 2>&1; then
      chrome="$c"
      break
    fi
  done
fi
[[ -n "$chrome" ]] || { echo "make-icns: needs Chrome or Chromium (set CHROME=...)" >&2; exit 1; }
if command -v magick >/dev/null 2>&1; then
  im=(magick)
elif command -v convert >/dev/null 2>&1; then
  im=(convert)
else
  echo "make-icns: needs ImageMagick" >&2
  exit 1
fi

cache="${XDG_CACHE_HOME:-$HOME/.cache}"
mkdir -p "$cache"
work=$(mktemp -d "$cache/tacit-icns.XXXXXX")
trap 'rm -rf "$work"' EXIT
cp "$svg" "$work/icon.svg"
printf '<!doctype html><html><body style="margin:0;background:transparent"><img src="icon.svg" width="1024" height="1024" style="display:block"></body></html>' >"$work/wrap.html"
"$chrome" --headless=new --disable-gpu --no-sandbox --user-data-dir="$work/profile" \
  --hide-scrollbars --force-device-scale-factor=1 --default-background-color=00000000 \
  --window-size=1024,1024 --screenshot="$work/1024.png" "file://$work/wrap.html" >/dev/null 2>&1
for n in 16 32 64 128 256 512; do
  "${im[@]}" "$work/1024.png" -filter Lanczos -resize "${n}x${n}" -strip "PNG32:$work/$n.png"
done

python3 - "$work" "$out" <<'PY'
import struct, sys
work, out = sys.argv[1], sys.argv[2]
# OSType -> pixel size of its PNG (the @2x types reuse the larger render).
types = [("icp4", 16), ("icp5", 32), ("icp6", 64), ("ic07", 128), ("ic08", 256),
         ("ic09", 512), ("ic10", 1024), ("ic11", 32), ("ic12", 64), ("ic13", 256), ("ic14", 512)]
chunks = b""
for t, n in types:
    data = open(f"{work}/{n}.png", "rb").read()
    chunks += t.encode() + struct.pack(">I", len(data) + 8) + data
open(out, "wb").write(b"icns" + struct.pack(">I", len(chunks) + 8) + chunks)
PY
echo "wrote $out"
