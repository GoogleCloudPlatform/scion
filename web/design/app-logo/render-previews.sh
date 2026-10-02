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

# Renders a PNG preview sheet for each logo direction in this folder.
# Each sheet shows 16 px (plus a 6x zoom of the 16 px raster), 32, 180
# and 512 px on light and dark backgrounds.
#
# Usage: web/design/app-logo/render-previews.sh [output-dir]
# Needs a Chromium-based browser; set CHROME to override the binary.
# Output defaults to /tmp/app-logo-previews (PNGs are not committed).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
out="${1:-/tmp/app-logo-previews}"
chrome="${CHROME:-$(command -v chromium || command -v chromium-browser || command -v google-chrome)}"
mkdir -p "$out"

render() {
  local svg="$1" title="$2"
  local name="${svg%.svg}"
  local query
  query="s=${svg}&t=$(python3 -c 'import sys, urllib.parse; print(urllib.parse.quote(sys.argv[1]))' "$title")"
  "$chrome" --headless --no-sandbox --disable-gpu --hide-scrollbars \
    --virtual-time-budget=2000 --window-size=1100,1200 \
    --screenshot="$out/$name.png" "file://$here/preview-sheet.html?$query" 2>/dev/null
  echo "$out/$name.png"
}

render direction-a.svg "Direction A: green tile, white two-leaf sprout"
render direction-b.svg "Direction B: Scion blue tile, green sprout growing from a branch node"
render direction-b-small.svg "Direction B, 16/32 px variant: no ring, thicker stem, flat leaves"
render direction-c.svg "Direction C: slate tile, single bold leaf on a curved stem"
