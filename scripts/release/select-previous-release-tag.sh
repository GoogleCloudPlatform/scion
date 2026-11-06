#!/bin/bash
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

# scripts/release/select-previous-release-tag.sh — Previous release tag in the same channel
#
# Read-only. Prints the release tag that the release notes for TAG start from,
# chosen from the tags in the current repository by version order:
#
#   preview  vX.Y.Z-preview.N or vX.Y.Z-rc.N: the previous preview or rc tag.
#   stable   vX.Y.Z: the previous stable tag. If there is none, the first
#            preview tag (vX.Y.Z-preview.N) before TAG.
#
# Version order compares X, Y, Z, then preview before rc before stable, then N,
# so v0.3.0-preview.10 follows v0.3.0-preview.9 and v0.3.0 follows its rc tags.
# Nightly tags are not handled: their release notes keep GitHub's default.
#
# Usage: ./scripts/release/select-previous-release-tag.sh TAG
#
# Prints the tag and exits 0. Prints nothing and exits 0 when no earlier tag
# exists in the channel. Exits 2 when TAG is not a preview, rc or stable tag.

set -euo pipefail

STABLE_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
PRERELEASE_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-(preview|rc)\.(0|[1-9][0-9]*)$'

usage() {
  echo "Usage: $0 TAG" >&2
  exit 2
}

[[ $# -eq 1 ]] || usage
TAG="$1"

# sort_key TAG — prints "X Y Z RANK N TAG" for a release tag, nothing otherwise.
# RANK orders the kinds of tag that share X.Y.Z: preview 0, rc 1, stable 2.
sort_key() {
  if [[ "$1" =~ $STABLE_RE ]]; then
    echo "${BASH_REMATCH[1]} ${BASH_REMATCH[2]} ${BASH_REMATCH[3]} 2 0 $1"
  elif [[ "$1" =~ $PRERELEASE_RE ]]; then
    local rank=0
    [[ "${BASH_REMATCH[4]}" == rc ]] && rank=1
    echo "${BASH_REMATCH[1]} ${BASH_REMATCH[2]} ${BASH_REMATCH[3]} $rank ${BASH_REMATCH[5]} $1"
  fi
}

# sorted_before PATTERN — release tags matching the extended regex PATTERN
# (excluding TAG) plus TAG itself, in version order, cut off after TAG.
sorted_before() {
  local t
  {
    sort_key "$TAG"
    git tag --list 'v*' | while IFS= read -r t; do
      if [[ "$t" != "$TAG" && "$t" =~ $1 ]]; then
        sort_key "$t"
      fi
    done
  } | sort -k1,1n -k2,2n -k3,3n -k4,4n -k5,5n \
    | awk -v tag="$TAG" '$6 == tag { done = 1 } !done { print $6 }'
}

if [[ "$TAG" =~ $PRERELEASE_RE ]]; then
  sorted_before "$PRERELEASE_RE" | tail -n 1
elif [[ "$TAG" =~ $STABLE_RE ]]; then
  prev=$(sorted_before "$STABLE_RE" | tail -n 1)
  if [[ -z "$prev" ]]; then
    # No earlier stable: start from the oldest preview. The oldest v* tag in
    # the repository is v0.3.0-preview.1, the agreed baseline for the first
    # stable release; every later stable starts from the previous stable.
    # sed (not head) reads all its input, so nothing upstream gets SIGPIPE
    # under pipefail.
    prev=$(sorted_before '^v[0-9]+\.[0-9]+\.[0-9]+-preview\.[0-9]+$' | sed -n 1p)
  fi
  [[ -z "$prev" ]] || echo "$prev"
else
  echo "error: '$TAG' is not a preview, rc or stable release tag" >&2
  exit 2
fi
