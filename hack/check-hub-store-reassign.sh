#!/usr/bin/env bash
# Flags NEW direct reassignments of the hub Server's store field in pkg/hub
# tests (`srv.store = ...`, `f.srv.store = ...`, `f.Server.store = ...`).
#
# Swapping srv.store after setup is safe only while nothing started by New()
# or by the test's setup reads the field from another goroutine. Once setup
# starts such a goroutine, every swap becomes a data race (ptone/scion#3184).
# The supported alternative is the store fault switch in
# pkg/hub/store_fault_helpers_test.go: installStoreFault (or
# testServerWithStoreFault) installs a switch-gated wrapper once, right after
# the server is built, and the test calls Arm() on the returned
# storeFaultSwitch instead of swapping the store.
#
# Existing call sites are baselined per file in
# hack/hub-store-reassign-baseline.txt ("<file> | <count>"). No line numbers:
# entries survive unrelated edits. The gate fails when:
#   - a file has more reassignments than its baseline entry (or has none), or
#   - a file has FEWER than its baseline entry, or an entry matches no file:
#     the baseline is a ratchet, so lower the count (or delete the entry) in
#     the same change that migrates call sites.
#
# Comment lines are ignored. LIMITATIONS: the check is textual and
# line-oriented. It does not see a reassignment through a differently named
# variable (e.g. `x := srv; x.store = ...`) or a multi-value assignment
# (`srv.store, y = ...`).
#
# Severity: FORMATTING-GRADE (test hygiene)
#   Missing rg:          exit 3 (nothing analysed; see hack/lib/require-tool.sh)
#   Scan root missing:   exit 4
#   Violations found:    exit 1
#   Clean:               exit 0
#
# Usage: hack/check-hub-store-reassign.sh [--self-test]
#
# See hack/LINT-CONVENTIONS.md for the conventions this script follows.
set -euo pipefail

cd "$(dirname "$0")/.."

# sort and join must agree on collation.
export LC_ALL=C

name="check-hub-store-reassign"

# shellcheck source=SCRIPTDIR/lib/require-tool.sh
source hack/lib/require-tool.sh
require_tool rg "$name" "ripgrep (rg)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# A reassignment of a `srv` or `Server` value's store field: the name must
# not be the tail of a longer identifier (testSrv), and `==` is a comparison.
pattern='(^|[^A-Za-z0-9_])(srv|Server)\.store[[:space:]]*=([^=]|$)'

helper_hint() {
  echo "Do not reassign srv.store in pkg/hub tests. Use the store fault switch" >&2
  echo "instead: installStoreFault / testServerWithStoreFault in" >&2
  echo "pkg/hub/store_fault_helpers_test.go install a switch-gated wrapper right" >&2
  echo "after the server is built; call Arm() on the returned storeFaultSwitch" >&2
  echo "where the test used to swap the store (ptone/scion#3435)." >&2
}

# scan <root> <baseline> <label>
# Compares per-file counts under <root> (test files only) with <baseline>,
# whose paths are relative to <root>. Returns 0 clean, 1 violations, 4 when
# <root>/pkg/hub is missing.
scan() {
  local root="$1" baseline="$2" label="$3"
  if [[ ! -d "$root/pkg/hub" ]]; then
    echo "$name: scan root $root/pkg/hub is missing — NOTHING WAS ANALYSED" >&2
    return 4
  fi

  local actual="$work/actual" expected="$work/expected"

  # file:line:text -> drop comment lines -> per-file count.
  # rg exits 1 when nothing matches, which is a clean state here.
  (cd "$root" || exit 4
   rg -n --no-heading --color never -e "$pattern" --glob '*_test.go' pkg/hub 2>/dev/null || true) \
    | awk -F: '{ text = $0; sub(/^[^:]*:[^:]*:/, "", text); if (text !~ /^[[:space:]]*\/\//) n[$1]++ }
               END { for (f in n) print f " " n[f] }' \
    | sort >"$actual"

  # Baseline: "<file> | <count>", '#' comments and blank lines ignored.
  awk -F'|' '/^[[:space:]]*(#|$)/ { next }
             { f = $1; c = $2; gsub(/[[:space:]]/, "", f); gsub(/[[:space:]]/, "", c); print f " " c }' \
    "$baseline" | sort >"$expected"

  local over under
  over="$(join -a1 -e0 -o '0,1.2,2.2' "$actual" "$expected" | awk '$2 > $3 { print "  " $1 ": " $2 " (baseline " $3 ")" }')"
  under="$(join -a2 -e0 -o '0,1.2,2.2' "$actual" "$expected" | awk '$2 < $3 { print "  " $1 ": " $2 " (baseline " $3 ")" }')"

  local total
  total="$(awk '{ s += $2 } END { print s + 0 }' "$actual")"
  echo "$name: analysed ${label}, ${total} direct srv.store reassignment(s) in pkg/hub tests" >&2

  local rc=0
  if [[ -n "$over" ]]; then
    echo "$name: new direct srv.store reassignment(s) in pkg/hub tests:" >&2
    echo "$over" >&2
    echo >&2
    helper_hint
    rc=1
  fi
  if [[ -n "$under" ]]; then
    echo "$name: baseline is stale (fewer reassignments than recorded):" >&2
    echo "$under" >&2
    echo "Lower the count, or delete the entry, in $baseline." >&2
    rc=1
  fi
  return "$rc"
}

self_test() {
  local dir="$work/fixture" failed=0 rc
  mkdir -p "$dir/pkg/hub"

  cat >"$dir/pkg/hub/a_test.go" <<'EOF'
package hub
func a() {
	srv.store = wrapped
	f.srv.store = &failing{}
	f.Server.store=x
	// srv.store = commented out, ignored
	if srv.store == nil {}
	testSrv.store = ignored
	srv.storeX = ignored
}
EOF
  cat >"$dir/pkg/hub/a.go" <<'EOF'
package hub
func prod() { srv.store = notATestFile }
EOF
  printf '# fixture\npkg/hub/a_test.go | 3\n' >"$dir/baseline"

  rc=0; scan "$dir" "$dir/baseline" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 0 ]] || { echo "self-test FAIL: baseline match: rc=$rc (want 0)" >&2; failed=1; }

  echo '	g.srv.store = added' >>"$dir/pkg/hub/a_test.go"
  rc=0; scan "$dir" "$dir/baseline" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 1 ]] || { echo "self-test FAIL: new reassignment: rc=$rc (want 1)" >&2; failed=1; }

  printf 'package hub\nfunc b() { srv.store = s }\n' >"$dir/pkg/hub/b_test.go"
  printf 'pkg/hub/a_test.go | 4\n' >"$dir/baseline"
  rc=0; scan "$dir" "$dir/baseline" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 1 ]] || { echo "self-test FAIL: unbaselined file: rc=$rc (want 1)" >&2; failed=1; }

  printf 'pkg/hub/a_test.go | 4\npkg/hub/b_test.go | 2\n' >"$dir/baseline"
  rc=0; scan "$dir" "$dir/baseline" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 1 ]] || { echo "self-test FAIL: stale count: rc=$rc (want 1)" >&2; failed=1; }

  printf 'pkg/hub/a_test.go | 4\npkg/hub/b_test.go | 1\npkg/hub/gone_test.go | 1\n' >"$dir/baseline"
  rc=0; scan "$dir" "$dir/baseline" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 1 ]] || { echo "self-test FAIL: stale entry: rc=$rc (want 1)" >&2; failed=1; }

  printf 'pkg/hub/a_test.go | 4\npkg/hub/b_test.go | 1\n' >"$dir/baseline"
  rc=0; scan "$dir" "$dir/baseline" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 0 ]] || { echo "self-test FAIL: updated baseline: rc=$rc (want 0)" >&2; failed=1; }

  rc=0; scan "$dir/missing" "$dir/baseline" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 4 ]] || { echo "self-test FAIL: missing root: rc=$rc (want 4)" >&2; failed=1; }

  if [[ $failed -ne 0 ]]; then
    return 1
  fi
  echo "$name: self-test passed" >&2
}

if [[ "${1:-}" == "--self-test" ]]; then
  self_test
  exit $?
fi

sha="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
if [[ "$sha" != "unknown" ]] && [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi

rc=0
scan . hack/hub-store-reassign-baseline.txt "$sha" || rc=$?
if [[ $rc -eq 0 ]]; then
  echo "$name: no new reassignments"
fi
exit "$rc"
