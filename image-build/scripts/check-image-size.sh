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
# Both images must be in the local engine's store (docker, or podman when
# docker is absent; override with IMAGE_TOOL). Sizes are the engine's
# uncompressed image size. The budget is in MiB and defaults to
# IMAGE_SIZE_BUDGET_MIB, else DEFAULT_BUDGET_MIB below. A candidate smaller
# than the baseline always passes.
#
# DEFAULT_BUDGET_MIB covers core-base growth from the Playwright WebKit and
# Firefox system libraries (install-core-toolchain.sh playwright-deps).
# Raising it is a deliberate decision about image weight; do it in the same
# change that adds the weight, and say why.
#
# Exit status: 0 within budget, 1 over budget, 2 usage or inspection error.

set -euo pipefail

DEFAULT_BUDGET_MIB=500

usage() {
  echo "usage: $(basename "$0") [--budget-mib N] <baseline-image> <candidate-image>" >&2
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

case "$budget" in
'' | *[!0-9]*)
  echo "budget must be a whole number of MiB, got '$budget'" >&2
  exit 2
  ;;
esac
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

# image_size <ref> — uncompressed size in bytes.
image_size() {
  local out
  if ! out="$("$tool" image inspect --format '{{.Size}}' "$1" 2>&1)"; then
    echo "cannot inspect $1 with $tool: $out" >&2
    exit 2
  fi
  case "$out" in
  '' | *[!0-9]*)
    echo "unexpected size for $1 from $tool: '$out'" >&2
    exit 2
    ;;
  esac
  printf '%s\n' "$out"
}

mib() { awk -v b="$1" 'BEGIN { printf "%.1f", b / 1048576 }'; }

base_bytes="$(image_size "$baseline")"
cand_bytes="$(image_size "$candidate")"
delta_bytes=$((cand_bytes - base_bytes))
budget_bytes=$((budget * 1048576))

printf 'baseline   %10s MiB  %s\n' "$(mib "$base_bytes")" "$baseline"
printf 'candidate  %10s MiB  %s\n' "$(mib "$cand_bytes")" "$candidate"
printf 'delta      %10s MiB  (budget %s MiB)\n' "$(mib "$delta_bytes")" "$budget"

if [ "$delta_bytes" -gt "$budget_bytes" ]; then
  echo "FAIL: $candidate grew by $(mib "$delta_bytes") MiB over $baseline, above the $budget MiB budget" >&2
  exit 1
fi
echo "ok: within budget"
