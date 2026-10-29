# scripts/single-node-vm/tests/test_release_order.sh -- covers
# release-order.sh, the tag ordering behind deploy.sh's downgrade guard.
# It runs on the operator's machine (possibly macOS), so it must not need
# GNU `sort -V`; these tests pin its results and prove it has no external
# dependency.
#
# This file has no shebang -- it is always `source`d, never executed --
# so shellcheck needs an explicit shell directive to know its dialect.
# shellcheck shell=bash

RELEASE_ORDER_SH="${TIER_DIR}/release-order.sh"

# _ro_expect A B WANT -- assert release_order A B prints WANT.
_ro_expect() {
  assert_eq "$3" "$(release_order "$1" "$2")" "release_order $1 $2"
}

test_release_order_pinned_cases() {
  # shellcheck source=scripts/single-node-vm/release-order.sh
  source "$RELEASE_ORDER_SH"
  # Nightlies: by date (and a numbered same-day rebuild after its day).
  _ro_expect nightly-20261001 nightly-20261024 older
  _ro_expect nightly-20261024 nightly-20261001 newer
  _ro_expect nightly-20261024 nightly-20261024 same
  _ro_expect nightly-20261023 nightly-20261023.1 older
  _ro_expect nightly-20261023.2 nightly-20261023.10 older
  # Semver cores compare numerically, component by component.
  _ro_expect v1.9.0 v1.10.0 older
  _ro_expect v1.10.0 v1.9.0 newer
  _ro_expect v0.9.9 v1.0.0 older
  _ro_expect v2.0.0 v10.0.0 older
  # Same core, both releases: the "release outranks a pre-release" branch
  # answers newer before any comparison (unchanged from before).
  _ro_expect 1.2.3 v1.2.3 newer
  # A pre-release is older than its release; pre-releases order by their
  # own components.
  _ro_expect v1.0.0-test v1.0.0 older
  _ro_expect v1.0.0 v1.0.0-rc.1 newer
  _ro_expect v0.3.0-preview.2 v0.3.0-preview.10 older
  _ro_expect v0.3.0-preview.4 v0.3.0-rc.1 older
  _ro_expect v0.3.0-rc.10 v0.3.0-rc.2 newer
  _ro_expect v1.0.0-alpha v1.0.0-alpha.1 older
  _ro_expect v1.0.0-rc.1 v0.9.0 newer
  # Different channels or git builds cannot be ordered.
  _ro_expect v1.0.0 nightly-20261024 unknown
  _ro_expect nightly-20261024 v1.0.0 unknown
  _ro_expect git-abcdef012345 v1.0.0 unknown
  _ro_expect v1.0.0 git-abcdef012345 unknown
}

# Parity with GNU sort -V, the implementation release_order used to call:
# for every ordered pair from a corpus of realistic and tricky keys,
# version_cmp must put first what `sort -V` puts first. Skipped (visibly)
# where sort has no -V, e.g. a macOS developer machine.
test_release_order_matches_gnu_sort_v() {
  if ! printf '1\n2\n' | LC_ALL=C sort -V >/dev/null 2>&1; then
    echo "SKIP [${CURRENT_TEST}]: this sort has no -V to compare against"
    return 0
  fi
  # shellcheck source=scripts/single-node-vm/release-order.sh
  source "$RELEASE_ORDER_SH"
  local keys=(20261023 20261024 20260101 20261023.1 20261023.2 20261023.10 1.0.0 1.0.1 1.2.0 1.10.0
    0.9.9 10.0.0 1.0.0.1 preview.1 preview.2 preview.10 preview.4 rc.1 rc.2 rc.10 alpha alpha.1
    alpha.beta beta beta.2 test test.1 1 01 001 10 9 a b A Z a1 a01 a10 'a~' '~a' x-y x.y x_y
    rc1 rc10 rc-1 preview4 preview.04 dev.0 0 00 1.0 1.0.0-rc ab abc 1a 1a.b 2.tar.gz a.b.c
    x.b 'x.b~' '1.0~rc.tar' 1.0.tar a.tar 'a~.tar' 'a.b~c' a.bc)
  # The last row exercises filevercmp's second pass: keys whose prefixes
  # (file suffix cut) compare equal, so the full strings decide (x.b~
  # before x.b, a.b~c before a.bc).
  local a b first want mismatches="" n=0
  for a in "${keys[@]}"; do
    for b in "${keys[@]}"; do
      [[ "$a" == "$b" ]] && continue
      n=$((n + 1))
      first="$(printf '%s\n%s\n' "$a" "$b" | LC_ALL=C sort -V | head -1)"
      if [[ "$first" == "$a" ]]; then want=-1; else want=1; fi
      version_cmp "$a" "$b"
      [[ "$VERSION_CMP" == "$want" ]] || mismatches="${mismatches} ${a}/${b}"
    done
  done
  assert_eq "" "$mismatches" "version_cmp must order all ${n} pairs exactly as GNU sort -V does"
}

# release_order uses no external command: with PATH holding only a `sort`
# that rejects -V and logs every call (as on macOS, but stricter), every
# result is unchanged and the stub is never called.
test_release_order_needs_no_sort() {
  local stub log out
  stub="$(mktemp -d)"
  log="${stub}/calls.log"
  printf '#!/bin/sh\necho "sort $*" >> "%s"\necho "sort: invalid option -- V" >&2\nexit 2\n' "$log" > "${stub}/sort"
  chmod 0755 "${stub}/sort"
  # shellcheck disable=SC2016 # the script is for the child bash
  out="$(PATH="$stub" "$BASH" -c '
    source "$1"
    release_order nightly-20261001 nightly-20261024
    release_order v0.3.0-preview.2 v0.3.0-preview.10
    release_order v1.10.0 v1.9.0
    release_order v1.0.0-rc.1 v1.0.0
  ' _ "$RELEASE_ORDER_SH" 2>&1)"
  assert_eq $'older\nolder\nnewer\nolder' "$out" "release_order must give the same results with no usable sort on PATH"
  assert_false "$([[ -s "$log" ]] && echo true || echo false)" "release_order must never call sort"
  rm -rf "$stub"
}
