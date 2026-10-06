#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Generates the app icon set in web/public/ from the logo SVGs in this
# folder. Re-run after editing any logo*.svg and commit the outputs.
#
# Usage: web/design/app-logo/generate-icons.sh
# Needs a Chromium-based browser (CHROME overrides the binary) and
# python3 (standard library only, used to pack favicon.ico).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
public="$(cd "$here/../../public" && pwd)"
chrome="${CHROME:-$(command -v chromium || command -v chromium-browser || command -v google-chrome || true)}"
if [ -z "$chrome" ]; then
  echo "generate-icons: no Chromium found; set CHROME" >&2
  exit 1
fi
work="$(mktemp -d)"
if [ -z "$work" ] || [ "$work" = "/" ] || [ ! -d "$work" ]; then
  echo "generate-icons: mktemp -d returned an unusable directory: '$work'" >&2
  exit 1
fi
cleanup() {
  # Never touch anything outside a real temp directory.
  if [ -n "$work" ] && [ "$work" != "/" ] && [ -d "$work" ]; then
    rm -f -- "$work"/page.html "$work"/chrome.log "$work"/favicon-*.png
    rmdir -- "$work"
  fi
}
trap cleanup EXIT

# raster <svg> <size> <out.png>: renders the SVG at size x size px with a
# transparent background.
raster() {
  local svg="$1" size="$2" out="$3"
  cat >"$work/page.html" <<EOF
<!doctype html><html><head><style>
html,body{margin:0;background:transparent}
img{display:block;width:${size}px;height:${size}px}
</style></head><body><img src="file://$here/$svg"></body></html>
EOF
  rm -f -- "$out"
  # Chromium is noisy on stderr even on success, so keep its output in a
  # log and show it only when the render fails.
  if ! "$chrome" --headless --no-sandbox --disable-gpu --hide-scrollbars \
    --force-device-scale-factor=1 --default-background-color=00000000 \
    --window-size="$size,$size" --screenshot="$out" \
    "file://$work/page.html" >"$work/chrome.log" 2>&1 || [ ! -s "$out" ]; then
    cat "$work/chrome.log" >&2
    echo "generate-icons: render failed: $out (CHROME=$chrome)" >&2
    exit 1
  fi
  echo "$out"
}

# Full tile with rounded corners for the standard icons.
raster logo.svg 192 "$public/icon-192.png"
raster logo.svg 512 "$public/icon-512.png"

# Full-bleed square, mark inside the central 80% safe zone. iOS applies
# its own corner mask and fills transparency with black, so the
# apple-touch-icon uses the full-bleed variant too.
raster logo-maskable.svg 512 "$public/icon-maskable-512.png"
raster logo-maskable.svg 180 "$public/apple-touch-icon.png"

# Favicons use the simplified small variant (no gradients or midrib,
# thicker stem, lighter edge so the tile shows on dark tab strips).
cp "$here/logo-small.svg" "$public/favicon.svg"
echo "$public/favicon.svg"
for s in 16 32 48; do
  raster logo-small.svg "$s" "$work/favicon-$s.png" >/dev/null
done

# Pack the PNGs into an ICO container (PNG-compressed entries).
python3 - "$public/favicon.ico" "$work"/favicon-16.png "$work"/favicon-32.png "$work"/favicon-48.png <<'PY'
import struct
import sys

out, pngs = sys.argv[1], sys.argv[2:]
blobs = [open(p, "rb").read() for p in pngs]
header = struct.pack("<HHH", 0, 1, len(blobs))
offset = len(header) + 16 * len(blobs)
entries = b""
for blob in blobs:
    w, h = struct.unpack(">II", blob[16:24])
    entries += struct.pack("<BBBBHHII", w % 256, h % 256, 0, 0, 1, 32, len(blob), offset)
    offset += len(blob)
with open(out, "wb") as f:
    f.write(header + entries + b"".join(blobs))
print(out)
PY
