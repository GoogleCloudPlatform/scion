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

# Tests for scripts/release/select-previous-release-tag.sh against throwaway
# local repositories. Needs only bash and git; no network.

set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/select-previous-release-tag.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Commit identity for the throwaway repositories, without writing git config.
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com

fails=0
fail() {
  echo "FAIL: $*" >&2
  fails=$((fails + 1))
}

# new_repo NAME TAG... — a repository with one commit carrying every TAG.
new_repo() {
  repo="$tmp/$1"
  shift
  git init -q "$repo"
  git -C "$repo" commit -q --allow-empty -m c1
  local t
  for t in "$@"; do
    git -C "$repo" tag "$t"
  done
}

# expect TAG WANT — the script, run in $repo, prints WANT (empty: nothing).
expect() {
  local got
  if ! got=$(cd "$repo" && "$SCRIPT" "$1"); then
    fail "$1: exited non-zero"
    return
  fi
  if [[ "$got" == "$2" ]]; then
    echo "ok: $1 -> ${2:-<none>}"
  else
    fail "$1: got '${got}', want '${2}'"
  fi
}

# expect_usage_error TAG — the script exits 2 for TAG.
expect_usage_error() {
  local rc=0
  (cd "$repo" && "$SCRIPT" "$1" >/dev/null 2>&1) || rc=$?
  if [[ $rc -eq 2 ]]; then
    echo "ok: $1 -> exit 2"
  else
    fail "$1: exit $rc, want 2"
  fi
}

previews=()
for n in 1 2 3 4 5 6 7 8 9 10; do
  previews+=("v0.3.0-preview.$n")
done

# Previews 1..10, two rc tags, the stable release, the next line's first
# preview, and nightlies (which never count). Tag creation order is shuffled
# relative to version order on purpose: only version order may matter.
new_repo full nightly-20261029 v0.3.0-rc.2 "${previews[@]}" v0.3.0-rc.1 \
  nightly-20261105 v0.3.0 v0.4.0-preview.1 v0.3.0-preview.x v0.3.0-preview.01

expect v0.3.0-preview.2 v0.3.0-preview.1
expect v0.3.0-preview.9 v0.3.0-preview.8
expect v0.3.0-preview.10 v0.3.0-preview.9
expect v0.3.0-preview.11 v0.3.0-preview.10 # TAG need not exist yet
expect v0.3.0-rc.1 v0.3.0-preview.10
expect v0.3.0-rc.2 v0.3.0-rc.1
expect v0.3.0 v0.3.0-preview.1             # no earlier stable: first preview
expect v0.4.0-preview.1 v0.3.0-rc.2        # previous preview-channel tag
expect v0.3.0-preview.1 ''                 # first preview: nothing earlier
expect_usage_error nightly-20261105
expect_usage_error v0.3.0-preview.x
expect_usage_error v0.3.0-preview.01
expect_usage_error main

# A later stable release starts from the previous stable, not a preview.
new_repo stable2 v0.3.0-preview.1 v0.3.0-preview.2 v0.3.0 v0.3.1 \
  v0.4.0-preview.1 v0.4.0-rc.1 v0.10.0
expect v0.3.1 v0.3.0
expect v0.4.0 v0.3.1
expect v0.10.0 v0.3.1
expect v0.4.0-rc.1 v0.4.0-preview.1

# No release tags at all, only nightlies.
new_repo empty nightly-20261029 nightly-20261030
expect v0.3.0-preview.1 ''
expect v0.3.0 ''

# A stable tag whose only previews come later in version order.
new_repo later v0.4.0-preview.1
expect v0.3.0 ''

if [[ $fails -ne 0 ]]; then
  echo "$fails test(s) failed" >&2
  exit 1
fi
echo "all tests passed"
