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

# check-image-size.sh — report how much bigger a candidate image is than a
# baseline, and fail when the growth exceeds a budget.
#
# Usage: check-image-size.sh [--budget-mib N] <baseline-image> <candidate-image>
#
# Both images must be in the local engine's store: docker, or podman when
# docker is absent (override with IMAGE_TOOL=docker|podman). What is measured
# is the image's uncompressed size, read per engine:
#
#   docker  the sum of its uncompressed layer sizes (docker history
#           --human=false). Not `image inspect`'s .Size: with docker's
#           containerd image store it depends on how the image entered the
#           store (larger than the layer sum for a local build, about the
#           compressed size for a pull), and it overstated core-base's
#           growth by about 40%.
#   podman  `podman image inspect` .Size, the unpacked size in podman's own
#           store. `podman history` always prints .Size human-formatted,
#           whatever --human says (cmd/podman/images/history.go,
#           historyReporter.Size, v5.8.9), so it cannot be summed.
#
# The budget is in MiB and defaults to IMAGE_SIZE_BUDGET_MIB, else
# DEFAULT_BUDGET_MIB below. A candidate smaller than the baseline always
# passes.
#
# DEFAULT_BUDGET_MIB is the measured growth of core-base from the Playwright
# WebKit and Firefox system libraries (install-core-toolchain.sh
# playwright-deps, playwright-core 1.62.1's Debian 13 list): 471.8 MiB of
# docker layer sum for core-base (Debian 13, amd64) built on 2026-10-31,
# almost all of it the ~470 MiB playwright-deps layer, plus about 15%
# headroom for package updates. Raising it is a deliberate decision about
# image weight; do it in the same change that adds the weight, and say why.
#
# Exit status: 0 within budget, 1 over budget, 2 usage or inspection error.

set -euo pipefail

DEFAULT_BUDGET_MIB=550

usage() {
  echo "usage: $(basename "$0") [--budget-mib N]" \
    "<baseline-image> <candidate-image>" >&2
  exit 2
}

budget="${IMAGE_SIZE_BUDGET_MIB:-$DEFAULT_BUDGET_MIB}"
args=()
while [ $# -gt 0 ]; do
  case "$1" in
  --budget-mib)
    [ $# -ge 2 ] || usage
    budget="$2"
    shift 2
    ;;
  --budget-mib=*)
    budget="${1#*=}"
    shift
    ;;
  -h | --help) usage ;;
  -*)
    echo "unknown flag: $1" >&2
    usage
    ;;
  *)
    args+=("$1")
    shift
    ;;
  esac
done
[ "${#args[@]}" -eq 2 ] || usage
baseline="${args[0]}"
candidate="${args[1]}"

# At most 6 digits, so budget * 1048576 below cannot overflow.
case "$budget" in
'' | *[!0-9]*)
  echo "budget must be a whole number of MiB, got '$budget'" >&2
  exit 2
  ;;
esac
if [ "${#budget}" -gt 6 ]; then
  echo "budget must be at most 6 digits of MiB, got '$budget'" >&2
  exit 2
fi
# Base 10 explicitly: bash arithmetic reads a leading zero ("08") as octal.
budget=$((10#$budget))

tool="${IMAGE_TOOL:-}"
if [ -z "$tool" ]; then
  if command -v docker >/dev/null 2>&1; then
    tool=docker
  elif command -v podman >/dev/null 2>&1; then
    tool=podman
  else
    echo "neither docker nor podman found; set IMAGE_TOOL" >&2
    exit 2
  fi
fi
case "$tool" in
docker) measure="sum of uncompressed layer sizes (docker history)" ;;
podman) measure="unpacked image size (podman image inspect)" ;;
*)
  echo "IMAGE_TOOL must be docker or podman, got '$tool'" >&2
  exit 2
  ;;
esac

errf="$(mktemp)" || {
  echo "cannot create a temp file" >&2
  exit 2
}
trap 'rm -f "$errf"' EXIT

# image_size <ref> — the image's uncompressed size in bytes (see the header).
# The engine's stderr is kept apart from the sizes being parsed, and shown
# only when the engine fails.
image_size() {
  local out line hint total=0 seen=0
  if [ "$tool" = podman ]; then
    if ! out="$(podman image inspect --format '{{.Size}}' "$1" 2>"$errf")"; then
      echo "cannot inspect $1 with podman: $(cat "$errf")" >&2
      exit 2
    fi
  elif ! out="$(docker history --human=false --format '{{.Size}}' "$1" \
    2>"$errf")"; then
    echo "cannot read the layers of $1 with docker: $(cat "$errf")" >&2
    exit 2
  fi
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    case "$line" in
    *[!0-9]*)
      hint=""
      [ "$tool" = docker ] &&
        hint=" (if docker is podman's docker shim, set IMAGE_TOOL=podman)"
      echo "unexpected size for $1 from $tool: '$line'$hint" >&2
      exit 2
      ;;
    esac
    total=$((total + 10#$line))
    seen=1
  done <<<"$out"
  if [ "$seen" -eq 0 ]; then
    echo "no size reported for $1 by $tool" >&2
    exit 2
  fi
  printf '%s\n' "$total"
}

mib() { awk -v b="$1" 'BEGIN { printf "%.1f", b / 1048576 }'; }

base_bytes="$(image_size "$baseline")"
cand_bytes="$(image_size "$candidate")"
delta_bytes=$((cand_bytes - base_bytes))
budget_bytes=$((budget * 1048576))

echo "${measure}:"
printf 'baseline   %10s MiB  %s\n' "$(mib "$base_bytes")" "$baseline"
printf 'candidate  %10s MiB  %s\n' "$(mib "$cand_bytes")" "$candidate"
printf 'delta      %10s MiB  (budget %s MiB)\n' "$(mib "$delta_bytes")" \
  "$budget"

if [ "$delta_bytes" -gt "$budget_bytes" ]; then
  echo "FAIL: $candidate grew by $(mib "$delta_bytes") MiB over $baseline," \
    "above the $budget MiB budget" >&2
  exit 1
fi
echo "ok: within budget"
