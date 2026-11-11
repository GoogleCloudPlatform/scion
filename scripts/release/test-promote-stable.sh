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

# Tests for scripts/release/promote-stable.sh against throwaway local
# repositories: each case has a bare repository as `origin` and a working
# clone. Needs only bash and git; no network, no gh.

set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT="$DIR/promote-stable.sh"
SELECT="$DIR/select-weekly-preview.sh"
CUT="$DIR/cut-preview.sh"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fails=0
fail() {
  echo "FAIL: $*" >&2
  fails=$((fails + 1))
}

g() { git -C "$repo" "$@"; }
r() { git -C "$remote" "$@"; }

commit() {
  g commit -q --allow-empty -m "$1"
  g rev-parse HEAD
}

# new_repo NAME — origin ($remote, bare) and a working repo ($repo) on main
# with commits c1..c4. Nightlies on c2 and c3 (c4 is newer than the newest
# nightly). release/v0.3 at c2 with v0.3.0-preview.1 on c1 and
# v0.3.0-preview.2 on c2. Everything is pushed to origin.
new_repo() {
  remote="$tmp/$1.git"
  repo="$tmp/$1"
  git init -q --bare -b main "$remote"
  git init -q -b main "$repo"
  g config user.email t@example.com
  g config user.name t
  g remote add origin "$remote"
  c1=$(commit c1); c2=$(commit c2); c3=$(commit c3); c4=$(commit c4)
  g tag nightly-20261020 "$c2"
  g tag nightly-20261021 "$c3"
  g tag v0.3.0-preview.1 "$c1"
  g tag v0.3.0-preview.2 "$c2"
  g push -q origin main "$c2:refs/heads/release/v0.3"
  g push -q origin --tags
}

# reject_ref REF — make origin's update hook reject REF. Per-ref rejection
# (unlike pre-receive) lets other refs through unless the push is atomic.
reject_ref() {
  cat > "$remote/hooks/update" <<HOOK
#!/bin/sh
if [ "\$1" = "$1" ]; then echo "rejected \$1" >&2; exit 1; fi
exit 0
HOOK
  chmod +x "$remote/hooks/update"
}

# run ARGS... — run promote-stable.sh in $repo, answering y; sets $status,
# output in $tmp/out.
run() {
  status=0
  (cd "$repo" && printf 'y\n' | "$SCRIPT" "$@") >"$tmp/out" 2>&1 || status=$?
}

want_status() {
  [ "$status" = "$1" ] || fail "$name: exit $status, want $1 ($(tail -1 "$tmp/out"))"
}

# remote_ref REF — commit REF points to on origin, or empty.
remote_ref() { r rev-parse -q --verify "$1^{commit}" 2>/dev/null || true; }

want_remote() { # want_remote REF SHA (empty SHA: must not exist)
  local got
  got=$(remote_ref "$1")
  [ "$got" = "$2" ] || fail "$name: origin $1: got '${got:-<none>}', want '${2:-<none>}'"
}

want_out() { grep -qF -- "$1" "$tmp/out" || fail "$name: output lacks '$1'"; }

# run_select — run select-weekly-preview.sh in $repo after fetching; sets $sel
# (outputs) and $sel_status.
run_select() {
  g fetch -q origin --tags
  sel_status=0
  sel=$(cd "$repo" && env -u GITHUB_OUTPUT -u GITHUB_STEP_SUMMARY "$SELECT" --skip-ci-check 2>"$tmp/sel-err") || sel_status=$?
  [ "$sel_status" = 0 ] || fail "$name: select exit $sel_status ($(tail -1 "$tmp/sel-err"))"
}

want_sel() { # want_sel KEY VALUE
  printf '%s\n' "$sel" | grep -qx "$1=$2" || fail "$name: select $1 is not '$2': $(printf '%s' "$sel" | tr '\n' ' ')"
}

HINT_NOW="dispatch build-preview-weekly.yml by hand"
HINT_LATER="only after the next nightly"

no_next_previews() {
  [ -z "$(r tag -l 'v0.4.*')" ] || fail "$name: a v0.4 tag was created: $(r tag -l 'v0.4.*' | tr '\n' ' ')"
}

# --- Normal: tag and next branch, next branch on the stable commit ---
name=normal; new_repo "$name"
run
want_status 0
want_remote refs/tags/v0.3.0 "$c2"
want_remote refs/heads/release/v0.4 "$c2"
want_out "stable commit"
want_out "$HINT_NOW"
no_next_previews

# --- End to end: straight after promotion, nightly-20261021 (c3) is newer
# than the next branch, so a dispatch tags preview.1 there at once ---
run_select
want_sel selected true; want_sel branch release/v0.4
want_sel tag v0.4.0-preview.1; want_sel sha "$c3"

# --- End to end: with a later nightly the weekly selection takes it ---
g commit -q --allow-empty -m c5
c5=$(g rev-parse HEAD)
g tag nightly-20261023 "$c5"
g push -q origin main nightly-20261023
run_select
want_sel selected true; want_sel branch release/v0.4
want_sel tag v0.4.0-preview.1; want_sel sha "$c5"

# --- cut-preview.sh after the promotion targets the line after next ---
cut_out=$(cd "$repo" && "$CUT" --dry-run 2>&1) || fail "$name-cut: cut-preview.sh --dry-run failed: $cut_out"
printf '%s\n' "$cut_out" | grep -q "release/v0.5" \
  || fail "$name-cut: cut-preview.sh does not target release/v0.5: $cut_out"

# --- Stable commit on main but past the newest nightly: base is the nightly ---
name=past-nightly; new_repo "$name"
g tag v0.3.0-preview.3 "$c4"
g push -q origin "$c4:refs/heads/release/v0.3" v0.3.0-preview.3
run
want_status 0
want_remote refs/tags/v0.3.0 "$c4"
want_remote refs/heads/release/v0.4 "$c3"
want_out "nightly-20261021"
want_out "$HINT_LATER"
run_select
want_sel selected false

# --- Stable commit is the newest nightly: no nightly is newer than the next
# branch, so the weekly selection tags nothing until the next nightly ---
name=stable-is-newest-nightly; new_repo "$name"
g tag v0.3.0-preview.3 "$c3"
g push -q origin "$c3:refs/heads/release/v0.3" v0.3.0-preview.3
run
want_status 0
want_remote refs/heads/release/v0.4 "$c3"
want_out "stable commit"
want_out "$HINT_LATER"
grep -qF "$HINT_NOW" "$tmp/out" && fail "$name: immediate-dispatch hint printed"
run_select
want_sel selected false

# --- Cherry-picked release branch: base is the newest nightly on main ---
name=cherry-pick; new_repo "$name"
g checkout -q -b rel "$c2"
pick=$(commit cherry-pick)
g tag v0.3.0-preview.3 "$pick"
g checkout -q main
g push -q origin "$pick:refs/heads/release/v0.3" v0.3.0-preview.3
# A nightly that is not on main must not be picked.
g tag nightly-20261030 "$pick"
g push -q origin nightly-20261030
run
want_status 0
want_remote refs/tags/v0.3.0 "$pick"
want_remote refs/heads/release/v0.4 "$c3"
want_out "nightly-20261021, the newest nightly on main"
want_out "$HINT_LATER"
grep -qF "$HINT_NOW" "$tmp/out" && fail "$name: immediate-dispatch hint printed"
no_next_previews

# End to end: straight after promotion the weekly selection tags nothing
# (nightly-20261030 is not on main, but it is newer by name; drop it so the
# selection sees only main's nightlies, as in the real repository).
g tag -d nightly-20261030 >/dev/null
g push -q origin :refs/tags/nightly-20261030
run_select
want_sel selected false

# After the next nightly it takes that one.
g commit -q --allow-empty -m c5
c5=$(g rev-parse HEAD)
g tag nightly-20261031 "$c5"
g push -q origin main nightly-20261031
run_select
want_sel selected true; want_sel tag v0.4.0-preview.1; want_sel sha "$c5"

# --- Next branch already exists on origin: refuse, push nothing ---
name=next-exists; new_repo "$name"
g push -q origin "$c1:refs/heads/release/v0.4"
run release/v0.3
want_status 1
want_out "release/v0.4' already exists on origin"
want_remote refs/tags/v0.3.0 ""
want_remote refs/heads/release/v0.4 "$c1"

# --- Next branch on origin but not in the local remote-tracking refs ---
name=next-exists-untracked; new_repo "$name"
git clone -q --bare "$remote" "$tmp/other.git"
git -C "$tmp/other.git" push -q origin "$c1:refs/heads/release/v0.4"
rm -rf "$tmp/other.git"
g config remote.origin.fetch '+refs/heads/main:refs/remotes/origin/main'
g config --add remote.origin.fetch '+refs/heads/release/v0.3:refs/remotes/origin/release/v0.3'
run release/v0.3
want_status 1
want_out "release/v0.4' already exists on origin"
want_remote refs/tags/v0.3.0 ""

# --- Next branch appears on origin after the check, as a fast-forward of the
# base: the lease refuses it and nothing is pushed ---
name=next-appears-before-push; new_repo "$name"
mkdir -p "$tmp/shim"
real_git=$(command -v git)
cat > "$tmp/shim/git" <<SHIM
#!/usr/bin/env bash
if [ "\${1:-}" = push ]; then
  "$real_git" -C "$remote" update-ref refs/heads/release/v0.4 "$c1"
fi
exec "$real_git" "\$@"
SHIM
chmod +x "$tmp/shim/git"
status=0
(cd "$repo" && printf 'y\n' | PATH="$tmp/shim:$PATH" "$SCRIPT") >"$tmp/out" 2>&1 || status=$?
rm -rf "$tmp/shim"
want_status 1
want_remote refs/tags/v0.3.0 ""
want_remote refs/heads/release/v0.4 "$c1"

# --- Multi-digit minor: release/v0.9 promotes to v0.9.0 and creates
# release/v0.10; auto-detect ignores names that are not release/vX.Y ---
name=multi-digit; new_repo "$name"
g tag v0.9.0-preview.1 "$c2"
g push -q origin "$c2:refs/heads/release/v0.9" "$c2:refs/heads/release/v0.9-old" v0.9.0-preview.1
g push -q origin :refs/heads/release/v0.3
run
want_status 0
want_out "Auto-detected release branch: release/v0.9"
want_remote refs/tags/v0.9.0 "$c2"
want_remote refs/heads/release/v0.10 "$c2"
want_remote refs/heads/release/v0.1 ""

# --- Major version other than 0: release/v1.2 promotes to v1.2.0 and
# creates release/v1.3 ---
name=major-one; new_repo "$name"
g tag v1.2.0-preview.4 "$c2"
g push -q origin "$c2:refs/heads/release/v1.2" v1.2.0-preview.4
run
want_status 0
want_out "Auto-detected release branch: release/v1.2"
want_remote refs/tags/v1.2.0 "$c2"
want_remote refs/heads/release/v1.3 "$c2"
want_remote refs/tags/v0.3.0 ""
want_remote refs/heads/release/v0.4 ""

# --- Bad branch names: refused with the release/vX.Y message, also when the
# branch does not exist ---
name=bad-name; new_repo "$name"
g push -q origin "$c2:refs/heads/release/v0.3-old"
run release/v0.3-old
want_status 1
want_out "'release/v0.3-old' is not a release/vX.Y branch"
run release/foo
want_status 1
want_out "'release/foo' is not a release/vX.Y branch"
want_remote refs/tags/v0.3.0 ""

# --- --no-next-branch: tag only ---
name=no-next; new_repo "$name"
run --no-next-branch
want_status 0
want_remote refs/tags/v0.3.0 "$c2"
want_remote refs/heads/release/v0.4 ""
want_out "not created: --no-next-branch"

# --- --dry-run: prints both refs, pushes nothing ---
name=dry-run; new_repo "$name"
run --dry-run
want_status 0
want_out "Stable tag:      v0.3.0"
want_out "Commit:          $c2"
want_out "Next branch:     release/v0.4"
want_out "Next base:       $c2"
want_out "No changes were made."
want_remote refs/tags/v0.3.0 ""
want_remote refs/heads/release/v0.4 ""
g rev-parse -q --verify refs/tags/v0.3.0 >/dev/null && fail "$name: local tag created"

# --- Atomic: tag rejected, so the next branch is not created ---
name=atomic-tag-rejected; new_repo "$name"
reject_ref refs/tags/v0.3.0
run
want_status 1
want_remote refs/tags/v0.3.0 ""
want_remote refs/heads/release/v0.4 ""
g rev-parse -q --verify refs/tags/v0.3.0 >/dev/null && fail "$name: local tag left behind"

# --- Atomic: next branch rejected, so the tag is not created ---
name=atomic-branch-rejected; new_repo "$name"
reject_ref refs/heads/release/v0.4
run
want_status 1
want_remote refs/tags/v0.3.0 ""
want_remote refs/heads/release/v0.4 ""
# After the failure, a rerun succeeds once the remote accepts.
rm "$remote/hooks/update"
run
want_status 0
want_remote refs/tags/v0.3.0 "$c2"
want_remote refs/heads/release/v0.4 "$c2"

# --- No nightly on main: refuse, push nothing ---
name=no-nightly; new_repo "$name"
g tag -d nightly-20261020 nightly-20261021 >/dev/null
g push -q origin :refs/tags/nightly-20261020 :refs/tags/nightly-20261021
run
want_status 1
want_out "no nightly-YYYYMMDD tag is reachable from origin/main"
want_remote refs/tags/v0.3.0 ""

# --- Declining the prompt pushes nothing ---
name=declined; new_repo "$name"
status=0
(cd "$repo" && printf 'n\n' | "$SCRIPT") >"$tmp/out" 2>&1 || status=$?
want_status 1
want_remote refs/tags/v0.3.0 ""
want_remote refs/heads/release/v0.4 ""

if [ "$fails" -ne 0 ]; then
  echo "$fails failure(s)" >&2
  exit 1
fi
echo "ok"
