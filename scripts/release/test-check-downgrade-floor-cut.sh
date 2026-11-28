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
# Tests for scripts/release/check-downgrade-floor-cut.sh against throwaway
# local repositories. Needs only bash and git; no network.

set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/check-downgrade-floor-cut.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

fails=0
fail() {
  echo "FAIL: $*" >&2
  fails=$((fails + 1))
}

repo="$tmp/repo"

# new_repo FLOOR [CRON] — a repository whose HEAD^1 has floor FLOOR.
new_repo() {
  rm -rf "$repo"
  mkdir -p "$repo/pkg/version" "$repo/.github/workflows"
  git -C "$repo" init -q -b main
  write_floor "$1"
  write_cron "${2:-0 3 * * *}"
  git -C "$repo" add -A
  git -C "$repo" commit -q -m base
  # Pull-request commits go on a branch; main stays at the base.
  git -C "$repo" checkout -q -b pr
}

write_floor() {
  printf 'package version\n\nconst DowngradeFloor = "%s"\n' "$1" > "$repo/pkg/version/downgrade_floor.go"
}

write_cron() {
  printf "on:\n  schedule:\n    - cron: '%s'   # daily\n" "$1" > "$repo/.github/workflows/build-nightly.yml"
}

# commit_floor FLOOR [COMMITTER_DATE] — HEAD sets the floor to FLOOR.
commit_floor() {
  write_floor "$1"
  git -C "$repo" add -A
  GIT_COMMITTER_DATE="${2:-2026-11-27T12:00:00Z}" git -C "$repo" commit -q -m floor
}

run() {
  (cd "$repo" && "$SCRIPT" "$@")
}

expect_rc() {
  local want="$1" desc="$2" substr="$3"
  shift 3
  local rc=0 out
  out=$(run "$@" 2>&1) || rc=$?
  if [ "$rc" -ne "$want" ]; then
    fail "${desc}: expected exit ${want}, got ${rc}: ${out}"
    return
  fi
  if [ -n "$substr" ] && [[ "$out" != *"$substr"* ]]; then
    fail "${desc}: output missing '${substr}': ${out}"
    return
  fi
  echo "ok: ${desc}"
}

# --- Regression: GoogleCloudPlatform/scion#5703 (ptone/scion#9831) -----------
# Landed 2026-11-27T23:02Z with nightly-20261129; the first nightly to contain
# it was nightly-20261128.
new_repo nightly-20261128
commit_floor nightly-20261129 2026-11-27T23:02:38Z
expect_rc 1 "#5703: next-day floor after landing before the cut" "later than" --landed
expect_rc 1 "#5703 as a pull request at 22:00Z" "set it to nightly-20261128" --at 2026-11-27T22:00:00Z
new_repo nightly-20261126
commit_floor nightly-20261128 2026-11-27T23:02:38Z
expect_rc 0 "#5703 fixed: floor is the next cut" "passed" --landed

# --- Unchanged, removed, emptied -----------------------------------------------
new_repo nightly-20261120
git -C "$repo" commit -q --allow-empty -m other
expect_rc 0 "floor unchanged" "unchanged" --landed --at 2026-11-27T12:00:00Z
new_repo nightly-20261120
commit_floor ""
expect_rc 0 "floor emptied" "removed or emptied" --landed
new_repo nightly-20261120
git -C "$repo" rm -q pkg/version/downgrade_floor.go
git -C "$repo" commit -q -m rm
expect_rc 0 "floor file removed" "removed or emptied" --landed --at 2026-11-27T12:00:00Z

# --- Cut boundaries (cron 0 3 * * *, grace 60) -----------------------------------
new_repo nightly-20261101
commit_floor nightly-20261128
expect_rc 0 "before the cut: same day" "passed" --at 2026-11-28T02:59:59Z
expect_rc 0 "exactly at the cut: same day" "passed" --at 2026-11-28T03:00:00Z
expect_rc 0 "inside grace: same day accepted" "passed" --at 2026-11-28T03:30:00Z
expect_rc 1 "after grace: same day too early" "earlier than" --at 2026-11-28T04:01:00Z
expect_rc 1 "a day early" "earlier than" --at 2026-11-28T12:00:00Z
expect_rc 1 "pull request two days ahead" "later than" --at 2026-11-26T12:00:00Z
expect_rc 1 "grace 0: same-day window closed" "earlier than" --at 2026-11-28T03:01:00Z --grace-minutes 0 --head HEAD --base HEAD^1 --floor-file pkg/version/downgrade_floor.go
new_repo nightly-20261101
commit_floor nightly-20261129
expect_rc 0 "inside grace: next day accepted" "passed" --at 2026-11-28T03:30:00Z
expect_rc 0 "after the cut: next day" "passed" --at 2026-11-28T12:00:00Z
expect_rc 1 "before the cut: next day too late" "later than" --at 2026-11-28T02:00:00Z
expect_rc 0 "grace 0: next day right after the cut" "passed" --at 2026-11-28T03:01:00Z --grace-minutes 0

# --- Month, year and leap-day rollover --------------------------------------------
new_repo nightly-20261101
commit_floor nightly-20270101
expect_rc 0 "year rollover" "passed" --at 2026-12-31T12:00:00Z
new_repo nightly-20271101
commit_floor nightly-20280229
expect_rc 0 "leap day" "passed" --at 2028-02-28T12:00:00Z
new_repo nightly-20271101
commit_floor nightly-20280301
expect_rc 0 "after leap day" "passed" --at 2028-02-29T12:00:00Z

# --- Grace window crossing midnight (cron 30 23 * * *) ------------------------------
new_repo nightly-20261101 "30 23 * * *"
commit_floor nightly-20261127
expect_rc 0 "midnight-crossing grace: previous day's cut" "passed" --at 2026-11-28T00:15:00Z
expect_rc 1 "midnight-crossing grace closed: previous day too early" "earlier than" --at 2026-11-28T00:31:00Z --grace-minutes 30 --base HEAD^1 --head HEAD --workflow .github/workflows/build-nightly.yml --floor-file pkg/version/downgrade_floor.go --landed
new_repo nightly-20261101 "30 23 * * *"
commit_floor nightly-20261128
expect_rc 0 "midnight-crossing grace: same day's cut" "passed" --at 2026-11-28T00:15:00Z

# --- Cron read from --head (DST edit in the same commit) ---------------------------
new_repo nightly-20261101
write_cron "0 2 * * *"
commit_floor nightly-20261128
expect_rc 0 "cron edited with the floor: new cut applies" "02:00 UTC" --at 2026-11-28T02:30:00Z --grace-minutes 60
expect_rc 1 "cron edited with the floor: after new grace" "earlier than" --at 2026-11-28T03:01:00Z

# --- Fail closed ---------------------------------------------------------------------
new_repo nightly-20261101
printf "on:\n  schedule:\n    - cron: '0 3 * * *'\n    - cron: '0 15 * * *'\n" > "$repo/.github/workflows/build-nightly.yml"
commit_floor nightly-20261128
expect_rc 1 "two crons" "exactly one cron" --at 2026-11-27T12:00:00Z
new_repo nightly-20261101
write_cron "0 3 * * 1-5"
commit_floor nightly-20261128
expect_rc 1 "weekday cron" "not a single daily" --at 2026-11-27T12:00:00Z
new_repo nightly-20261101
commit_floor v0.4.0
expect_rc 1 "floor not a nightly tag" "must be a nightly" --at 2026-11-27T12:00:00Z
new_repo nightly-20261101
printf 'package version\n\nconst DowngradeFloor = "nightly-20261128"\nconst DowngradeFloor = "nightly-20261129"\n' > "$repo/pkg/version/downgrade_floor.go"
git -C "$repo" commit -q -am dup
expect_rc 1 "duplicate declarations" "exactly one" --at 2026-11-27T12:00:00Z
rm -rf "$repo"; mkdir -p "$repo"; git -C "$repo" init -q -b main; git -C "$repo" commit -q --allow-empty -m only
expect_rc 1 "no parent commit" "fetch at least two commits" --landed --at 2026-11-27T12:00:00Z

# --- Local pre-merge run: floor bump below a review fixup (rm-floor-rev r1) -----
new_repo nightly-20261101
commit_floor nightly-20261205
git -C "$repo" commit -q --allow-empty -m "address review"
expect_rc 1 "fixup on top: merge-base with main still sees the bump" "set it to nightly-20261129" --at 2026-11-28T12:00:00Z
expect_rc 0 "fixup on top: --landed compares HEAD^1 only" "unchanged" --landed --at 2026-11-28T12:00:00Z
expect_rc 1 "missing main ref" "pass --main REF" --main nosuch --at 2026-11-28T12:00:00Z
git -C "$repo" branch -q upstream-main main
expect_rc 1 "--main names another ref" "set it to nightly-20261129" --main upstream-main --at 2026-11-28T12:00:00Z

# --- Exact boundaries ----------------------------------------------------------
new_repo nightly-20261101
commit_floor nightly-20261128
expect_rc 0 "exactly at the cut, grace 0: same day" "passed" --at 2026-11-28T03:00:00Z --grace-minutes 0
expect_rc 0 "exactly at cut+grace: same day still accepted" "passed" --at 2026-11-28T04:00:00Z
new_repo nightly-20261101
commit_floor nightly-20261129
expect_rc 1 "exactly at the cut, grace 0: next day too late" "later than" --at 2026-11-28T03:00:00Z --grace-minutes 0

# --- Usage errors ----------------------------------------------------------------------
new_repo nightly-20261101
commit_floor nightly-20261128
expect_rc 2 "bad --at" "" --at 2026-11-27
expect_rc 2 "impossible date" "" --at 2026-02-30T12:00:00Z
expect_rc 2 "bad grace" "" --grace-minutes x
expect_rc 2 "unknown flag" "" --frobnicate
expect_rc 2 "missing value" "" --at

if [ "$fails" -gt 0 ]; then
  echo "${fails} test(s) failed" >&2
  exit 1
fi
echo "All check-downgrade-floor-cut.sh tests passed."
