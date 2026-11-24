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

# Tests for scripts/release/check-downgrade-floor.sh against synthetic floor
# files and throwaway local repositories. Needs only bash and git; no network.

set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/check-downgrade-floor.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.com
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.com

fails=0
fail() {
  echo "FAIL: $*" >&2
  fails=$((fails + 1))
}

write_floor_file() {
  local path="$1"
  local value="$2"
  cat > "$path" <<EOF
package version

const DowngradeFloor = "${value}"
EOF
}

expect_pass() {
  local desc="$1"
  shift
  local out
  if out=$("$SCRIPT" "$@" 2>&1); then
    echo "ok: ${desc}"
  else
    fail "${desc}: expected exit 0, got non-zero: ${out}"
  fi
}

expect_fail() {
  local desc="$1"
  local want_substr="$2"
  shift 2
  local rc=0
  local out
  out=$("$SCRIPT" "$@" 2>&1) || rc=$?
  if [[ $rc -ne 1 ]]; then
    fail "${desc}: expected exit 1, got ${rc}: ${out}"
    return
  fi
  if [[ -n "$want_substr" && "$out" != *"$want_substr"* ]]; then
    fail "${desc}: output missing '${want_substr}': ${out}"
    return
  fi
  echo "ok: ${desc}"
}

expect_usage_error() {
  local desc="$1"
  shift
  local rc=0
  local out
  out=$("$SCRIPT" "$@" 2>&1) || rc=$?
  if [[ $rc -eq 2 ]]; then
    echo "ok: ${desc}"
  else
    fail "${desc}: expected exit 2, got ${rc}: ${out}"
  fi
}

# --- Regression: v0.4.0-preview.3 (ptone/scion#8854) ----------------------
# Published 2026-11-20T15:18Z with floor nightly-20261121 -> must be rejected;
# with floor nightly-20261120 -> must be allowed.

floor_late="$tmp/floor_20261121.go"
floor_same="$tmp/floor_20261120.go"
floor_earlier="$tmp/floor_20261119.go"
write_floor_file "$floor_late" "nightly-20261121"
write_floor_file "$floor_same" "nightly-20261120"
write_floor_file "$floor_earlier" "nightly-20261119"

expect_fail "v0.4.0-preview.3 late floor (2026-11-20T15:18Z vs nightly-20261121)" \
  "dated after the release" \
  --floor-file "$floor_late" --date "2026-11-20T15:18Z" "v0.4.0-preview.3"

expect_fail "v0.4.0-preview.3 late floor with RFC3339 seconds" \
  "nightly-20261120" \
  --floor-file "$floor_late" --date "2026-11-20T15:18:38Z" "v0.4.0-preview.3"

expect_pass "v0.4.0-preview.3 matching floor (2026-11-20T15:18Z vs nightly-20261120)" \
  --floor-file "$floor_same" --date "2026-11-20T15:18Z" "v0.4.0-preview.3"

expect_pass "v0.4.0-preview.3 earlier floor (2026-11-20 vs nightly-20261119)" \
  --floor-file "$floor_earlier" --date "2026-11-20" "v0.4.0-preview.3"

# --- Stable and RC cuts ----------------------------------------------------

expect_fail "stable v0.4.0 late floor" \
  "dated after the release" \
  --floor-file "$floor_late" --date "2026-11-20" "v0.4.0"

expect_pass "stable v0.4.0 matching floor" \
  --floor-file "$floor_same" --date "20261120" "v0.4.0"

expect_fail "rc v0.4.0-rc.1 late floor" \
  "dated after the release" \
  --floor-file "$floor_late" --date "2026-11-20" "v0.4.0-rc.1"

expect_pass "rc v0.4.0-rc.1 matching floor" \
  --floor-file "$floor_same" --date "2026-11-20" "v0.4.0-rc.1"

expect_pass "refs/tags/ prefix stripped" \
  --floor-file "$floor_same" --date "2026-11-20" "refs/tags/v0.4.0"

# --- Nightly cuts ----------------------------------------------------------

expect_fail "nightly-20261120 with floor nightly-20261121" \
  "dated after the release" \
  --floor-file "$floor_late" "nightly-20261120"

expect_fail "nightly-20261120 ignores later --date because BelowFloor compares tag dates" \
  "dated after the release" \
  --floor-file "$floor_late" --date "2026-11-25" "nightly-20261120"

expect_pass "nightly-20261120 with floor nightly-20261120" \
  --floor-file "$floor_same" "nightly-20261120"

expect_pass "nightly-20261120.1 same-day rebuild with floor nightly-20261120" \
  --floor-file "$floor_same" "nightly-20261120.1"

floor_same_seq="$tmp/floor_20261120_1.go"
write_floor_file "$floor_same_seq" "nightly-20261120.1"
expect_fail "nightly-20261120 with floor nightly-20261120.1" \
  "sorts after" \
  --floor-file "$floor_same_seq" "nightly-20261120"

# --- Empty and malformed floors --------------------------------------------

floor_empty="$tmp/floor_empty.go"
write_floor_file "$floor_empty" ""
expect_pass "empty floor allows any release" \
  --floor-file "$floor_empty" --date "2026-11-20" "v0.4.0-preview.3"

floor_non_nightly="$tmp/floor_non_nightly.go"
write_floor_file "$floor_non_nightly" "v0.4.0-preview.1"
expect_fail "non-nightly floor rejected" \
  "must be a nightly-YYYYMMDD tag" \
  --floor-file "$floor_non_nightly" --date "2026-11-20" "v0.4.0-preview.3"

floor_bad_date="$tmp/floor_bad_date.go"
write_floor_file "$floor_bad_date" "nightly-20260230"
expect_fail "impossible calendar date in floor rejected" \
  "not a valid calendar date" \
  --floor-file "$floor_bad_date" --date "2026-11-20" "v0.4.0-preview.3"

floor_missing_const="$tmp/floor_missing_const.go"
cat > "$floor_missing_const" <<'EOF'
package version
// const DowngradeFloor = "nightly-20261120"
EOF
expect_fail "commented-out or missing const rejected" \
  "Could not find" \
  --floor-file "$floor_missing_const" --date "2026-11-20" "v0.4.0-preview.3"

expect_fail "missing floor file rejected" \
  "not found" \
  --floor-file "$tmp/does-not-exist.go" --date "2026-11-20" "v0.4.0-preview.3"

# --- Typed const and inline comment ----------------------------------------

floor_typed="$tmp/floor_typed.go"
cat > "$floor_typed" <<'EOF'
package version

const DowngradeFloor string = "nightly-20261120" // inline comment
EOF
expect_pass "typed const with inline comment parsed" \
  --floor-file "$floor_typed" --date "2026-11-20" "v0.4.0-preview.3"

# --- --ref mode against a local git repo -----------------------------------

repo="$tmp/repo"
git init -q "$repo"
mkdir -p "$repo/pkg/version"
write_floor_file "$repo/pkg/version/downgrade_floor.go" "nightly-20261121"
git -C "$repo" add pkg/version/downgrade_floor.go
git -C "$repo" commit -q -m "late floor"
sha_late=$(git -C "$repo" rev-parse HEAD)

write_floor_file "$repo/pkg/version/downgrade_floor.go" "nightly-20261120"
git -C "$repo" add pkg/version/downgrade_floor.go
git -C "$repo" commit -q -m "fixed floor"
sha_fixed=$(git -C "$repo" rev-parse HEAD)

if (cd "$repo" && "$SCRIPT" --ref "$sha_late" --date "2026-11-20T15:18Z" "v0.4.0-preview.3" >/dev/null 2>&1); then
  fail "--ref late commit should fail"
else
  echo "ok: --ref late commit rejected"
fi

if (cd "$repo" && "$SCRIPT" --ref "$sha_fixed" --date "2026-11-20T15:18Z" "v0.4.0-preview.3" >/dev/null 2>&1); then
  echo "ok: --ref fixed commit allowed"
else
  fail "--ref fixed commit should pass"
fi

# --- Usage errors (exit 2) -------------------------------------------------

expect_usage_error "missing tag"
expect_usage_error "invalid tag 'main'" --floor-file "$floor_same" "main"
expect_usage_error "invalid tag 'nightly-20260230'" --floor-file "$floor_same" "nightly-20260230"
expect_usage_error "invalid --date calendar date" --floor-file "$floor_same" --date "2026-02-30" "v0.4.0"
expect_usage_error "non-UTC --date offset rejected" --floor-file "$floor_same" --date "2026-11-20T15:18:00+05:00" "v0.4.0"
expect_usage_error "unknown option rejected" --bogus "v0.4.0"

if [[ $fails -ne 0 ]]; then
  echo "$fails test(s) failed" >&2
  exit 1
fi
echo "all tests passed"
