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

# Tests for scripts/release/select-weekly-preview.sh against throwaway local
# repositories. Needs only bash and git; no network, no gh.

set -euo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/select-weekly-preview.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fails=0
fail() {
  echo "FAIL: $*" >&2
  fails=$((fails + 1))
}

# Fake CI lookup: prints the conclusion recorded for a SHA in $CI_FILE.
cat > "$tmp/ci.sh" <<'EOF'
#!/usr/bin/env bash
line=$(grep "^$1 " "$CI_FILE" || true)
echo "${line#* }"
EOF
chmod +x "$tmp/ci.sh"
export CI_FILE="$tmp/ci-status"

g() { git -C "$repo" "$@"; }

commit() {
  g commit -q --allow-empty -m "$1"
  g rev-parse HEAD
}

# new_repo NAME — main with commits c1..c4, nightlies on c2..c4, and
# origin/release/v0.3 at c1 with v0.3.0-preview.1..2 on it.
new_repo() {
  repo="$tmp/$1"
  git init -q -b main "$repo"
  g config user.email t@example.com
  g config user.name t
  c1=$(commit c1); c2=$(commit c2); c3=$(commit c3); c4=$(commit c4)
  g tag nightly-20261020 "$c2"
  g tag nightly-20261021 "$c3"
  g tag nightly-20261022 "$c4"
  g update-ref refs/remotes/origin/release/v0.3 "$c1"
  g tag v0.3.0-preview.1 "$c1"
  g tag v0.3.0-preview.2 "$c1"
  : > "$CI_FILE"
}

ci() { echo "$1 $2" >> "$CI_FILE"; }

# run ARGS... — run the script in $repo; sets $out (outputs), $status.
run() {
  status=0
  out=$(cd "$repo" && env -u GITHUB_OUTPUT -u GITHUB_STEP_SUMMARY \
    "$SCRIPT" --ci-status-cmd "$tmp/ci.sh" "$@" 2>"$tmp/stderr") || status=$?
}

want() { # want KEY VALUE
  local got
  got=$(printf '%s\n' "$out" | sed -n "s/^$1=//p" | tail -1)
  [ "$got" = "$2" ] || fail "$name: $1: got '$got', want '$2'"
}

want_status() {
  [ "$status" = "$1" ] || fail "$name: exit $status, want $1 ($(tail -1 "$tmp/stderr"))"
}

# --- Newest nightly passes CI ---
name=newest-passes; new_repo "$name"
ci "$c4" success; ci "$c3" success
run
want_status 0; want selected true; want nightly nightly-20261022; want sha "$c4"
want tag v0.3.0-preview.3; want branch release/v0.3

# --- Newest fails, walk back to the newest passing one ---
name=walk-back; new_repo "$name"
ci "$c4" failure; ci "$c3" pending; ci "$c2" success
run
want_status 0; want selected true; want nightly nightly-20261020; want sha "$c2"

# --- No nightly newer than the branch passes ---
name=none-pass; new_repo "$name"
ci "$c4" failure; ci "$c3" failure; ci "$c2" missing
run
want_status 0; want selected false
grep -q "passed" "$tmp/stderr" || fail "$name: no explanation in output"

# --- --skip-ci-check takes the newest ---
name=skip-ci; new_repo "$name"
ci "$c4" failure
run --skip-ci-check
want_status 0; want nightly nightly-20261022

# --- Branch already at the newest nightly ---
name=up-to-date; new_repo "$name"
g update-ref refs/remotes/origin/release/v0.3 "$c4"
run
want_status 0; want selected false
grep -q "No new nightly since last preview" "$tmp/stderr" || fail "$name: missing skip message"

# --- A passing nightly older than the branch head is never picked ---
name=old-pass; new_repo "$name"
g update-ref refs/remotes/origin/release/v0.3 "$c3"
ci "$c4" failure; ci "$c2" success
run
want_status 0; want selected false

# --- Override that is already on the branch: skip ---
name=override-old; new_repo "$name"
g update-ref refs/remotes/origin/release/v0.3 "$c3"
run --nightly-tag nightly-20261020
want_status 0; want selected false

# --- Override bypasses the CI guard ---
name=override; new_repo "$name"
ci "$c4" success; ci "$c3" failure
run --nightly-tag nightly-20261021
want_status 0; want selected true; want nightly nightly-20261021; want sha "$c3"

# --- Override validation ---
name=override-bad-format; new_repo "$name"
run --nightly-tag v0.3.0-preview.1
want_status 1
name=override-missing; new_repo "$name"
run --nightly-tag nightly-20990101
want_status 1

# --- Diverged branch (cherry-pick) fails, never force-pushes ---
name=diverged; new_repo "$name"
g checkout -q -b rel "$c1"
pick=$(commit cherry-pick)
g checkout -q main
g update-ref refs/remotes/origin/release/v0.3 "$pick"
ci "$c4" success
run
want_status 1; want selected false
grep -q "::error::" "$tmp/stderr" || fail "$name: no ::error:: annotation"

# --- Version sort picks release/v0.10 over release/v0.9, numeric N ---
name=version-sort; new_repo "$name"
g update-ref refs/remotes/origin/release/v0.9 "$c1"
g update-ref refs/remotes/origin/release/v0.10 "$c1"
g update-ref refs/remotes/origin/release/v0.10-old "$c1"
for n in 1 2 9 10; do g tag "v0.10.0-preview.$n" "$c1"; done
g tag v0.1.0-preview.50 "$c1"   # other line, must not count
ci "$c4" success
run
want_status 0; want branch release/v0.10; want tag v0.10.0-preview.11

# --- First preview of a line ---
name=first-preview; new_repo "$name"
g update-ref refs/remotes/origin/release/v0.4 "$c1"
ci "$c4" success
run
want_status 0; want branch release/v0.4; want tag v0.4.0-preview.1

# --- Line already promoted to stable ---
name=promoted; new_repo "$name"
g tag v0.3.0 "$c1"
ci "$c4" success
run
want_status 1

# --- Highest patch wins even with a lower N; non-matching tags ignored ---
name=mixed-bases; new_repo "$name"
ci "$c4" success
g tag v0.3.0-preview.8 "$c1"
g tag v0.3.1-preview.7 "$c1"
g tag v0.3.0-preview.x "$c1"
g tag v0.3.0-preview.8-rc "$c1"
run
want_status 0; want tag v0.3.1-preview.8

# --- Patch compared numerically: v0.3.10 beats v0.3.9 ---
name=patch-numeric; new_repo "$name"
ci "$c4" success
g tag v0.3.9-preview.20 "$c1"
g tag v0.3.10-preview.3 "$c1"
run
want_status 0; want tag v0.3.10-preview.4

# --- Highest patch already promoted to stable ---
name=highest-patch-promoted; new_repo "$name"
ci "$c4" success
g tag v0.3.1-preview.7 "$c1"
g tag v0.3.1 "$c1"
run
want_status 1; want selected false
grep -q "v0.3.1 has already been promoted" "$tmp/stderr" || fail "$name: missing promoted message"

# --- No release branch ---
name=no-branch; new_repo "$name"
g update-ref -d refs/remotes/origin/release/v0.3
run
want_status 1

# --- Results go to GITHUB_OUTPUT and GITHUB_STEP_SUMMARY when set ---
name=gha-files; new_repo "$name"
ci "$c4" success
(cd "$repo" && GITHUB_OUTPUT="$tmp/gho" GITHUB_STEP_SUMMARY="$tmp/ghs" \
  "$SCRIPT" --ci-status-cmd "$tmp/ci.sh" >/dev/null 2>&1) || fail "$name: non-zero exit"
grep -qx "tag=v0.3.0-preview.3" "$tmp/gho" || fail "$name: tag not in GITHUB_OUTPUT"
grep -q "nightly-20261022" "$tmp/ghs" || fail "$name: nightly not in step summary"

if [ "$fails" -ne 0 ]; then
  echo "$fails failure(s)" >&2
  exit 1
fi
echo "ok"
