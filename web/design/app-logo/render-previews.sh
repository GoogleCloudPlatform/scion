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

# Renders a PNG preview sheet for each logo SVG in this folder.
# Each sheet shows 16 px (plus a 6x zoom of the 16 px raster), 32, 180
# and 512 px on light and dark backgrounds.
#
# Usage: web/design/app-logo/render-previews.sh [output-dir]
# Needs a Chromium-based browser; set CHROME to override the binary.
# Output defaults to /tmp/app-logo-previews (PNGs are not committed).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
out="${1:-/tmp/app-logo-previews}"
chrome="${CHROME:-$(command -v chromium || command -v chromium-browser || command -v google-chrome || true)}"
if [ -z "$chrome" ]; then
  echo "render-previews: no Chromium found; set CHROME" >&2
  exit 1
fi
mkdir -p "$out"
# One Chromium log shared by all renders, removed on any exit.
log="$(mktemp)"
trap 'rm -f -- "$log"' EXIT

render() {
  local svg="$1" title="$2"
  local name="${svg%.svg}"
  local query
  query="s=${svg}&t=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1]))' "$title")"
  rm -f -- "$out/$name.png"
  # Chromium is noisy on stderr even on success; show it only on failure.
  if ! "$chrome" --headless --no-sandbox --disable-gpu --hide-scrollbars \
    --virtual-time-budget=2000 --window-size=1100,1200 \
    --screenshot="$out/$name.png" "file://$here/preview-sheet.html?$query" \
    >"$log" 2>&1 || [ ! -s "$out/$name.png" ]; then
    cat "$log" >&2
    echo "render-previews: render failed: $out/$name.png (CHROME=$chrome)" >&2
    exit 1
  fi
  echo "$out/$name.png"
}

render logo.svg "logo.svg: app icon tile (direction C)"
render logo-small.svg "logo-small.svg: 16-48 px variant used for the favicons"
render logo-maskable.svg "logo-maskable.svg: full-bleed maskable variant"
