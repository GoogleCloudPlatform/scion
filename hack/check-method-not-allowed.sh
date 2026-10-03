#!/usr/bin/env bash
# Flags bare MethodNotAllowed(w) calls in pkg/hub and pkg/runtimebroker. A 405
# response MUST carry an Allow header (RFC 9110 section 15.5.6); both
# packages' MethodNotAllowed helpers set it from their variadic
# allowedMethods argument, so a call with no methods sends a 405 without
# Allow (ptone/scion#2421, PR #1413).
#
# Matches "MethodNotAllowed(w)" anywhere on a line, not only at line end, so
# `return MethodNotAllowed(w) // ...` or a call inside a larger expression is
# still caught. Test files are excluded: they may exercise the bare call on
# purpose.
#
# Severity: SECURITY-GRADE exit-code handling (fail-closed). This is a
# correctness check, not a security one, but it is blocking in CI, so an
# empty scan must not read as clean.
#   Missing tool:      exit 3 (nothing analysed; see hack/lib/require-tool.sh)
#   Missing scan root: exit 4 (a root directory does not exist, e.g. it was
#                      renamed; checked before scanning so a partial scan
#                      cannot read as clean)
#   No candidates:     exit 4 (no MethodNotAllowed call found at all, which
#                      means the scan roots are wrong, not that the tree is
#                      clean)
#   Violations found:  exit 1 (file:line list on stderr)
#   Clean:             exit 0
#
# Usage: hack/check-method-not-allowed.sh [--self-test]
#
# See hack/LINT-CONVENTIONS.md for the conventions this script follows.
set -euo pipefail

cd "$(dirname "$0")/.."

# shellcheck source=SCRIPTDIR/lib/require-tool.sh
source hack/lib/require-tool.sh
require_tool grep check-method-not-allowed

name="check-method-not-allowed"
roots=(pkg/hub pkg/runtimebroker)

# scan <root>... — print bare-call violations as path:line:text on stdout.
# Returns 4 if no non-test Go file under the roots calls MethodNotAllowed.
scan() {
  local candidates
  candidates="$(grep -rlF --include='*.go' --exclude='*_test.go' \
    'MethodNotAllowed(' "$@" 2>/dev/null || true)"
  if [[ -z "$candidates" ]]; then
    return 4
  fi
  # The helper definitions take (w http.ResponseWriter, ...), so the fixed
  # string never matches them; no definition filter is needed.
  # shellcheck disable=SC2086 # candidates is a newline-separated path list
  grep -nF 'MethodNotAllowed(w)' $candidates /dev/null || true
}

count_lines() {
  # grep -c prints exactly one integer (0 when empty) and exits 1 on zero
  # matches, so `|| true` here cannot append a second "0" line.
  printf '%s' "$1" | grep -c . || true
}

self_test() {
  local dir rc out n
  dir="$(mktemp -d)"
  # shellcheck disable=SC2064 # expand $dir now
  trap "rm -rf '$dir'" RETURN

  mkdir -p "$dir/clean" "$dir/bad" "$dir/empty"
  cat >"$dir/clean/h.go" <<'GO'
package h
func MethodNotAllowed(w http.ResponseWriter, allowedMethods ...string) {}
func a(w http.ResponseWriter) { MethodNotAllowed(w, http.MethodGet) }
GO
  cat >"$dir/bad/h.go" <<'GO'
package h
func a(w http.ResponseWriter) { MethodNotAllowed(w) }
func b(w http.ResponseWriter) {
	MethodNotAllowed(w) // mid-line: must still be caught
	MethodNotAllowed(w)
}
GO
  cat >"$dir/bad/h_test.go" <<'GO'
package h
func TestBare(t *testing.T) { MethodNotAllowed(w) }
GO
  cat >"$dir/empty/h.go" <<'GO'
package h
GO

  local failed=0
  out="$(scan "$dir/clean")" && rc=0 || rc=$?
  n="$(count_lines "$out")"
  if [[ "$rc" -ne 0 || "$n" -ne 0 ]]; then
    echo "self-test FAIL: clean fixture: rc=$rc count=$n" >&2; failed=1
  fi
  out="$(scan "$dir/bad")" && rc=0 || rc=$?
  n="$(count_lines "$out")"
  if [[ "$rc" -ne 0 || "$n" -ne 3 ]]; then
    echo "self-test FAIL: bad fixture: rc=$rc count=$n (want 3, test file excluded)" >&2; failed=1
  fi
  out="$(scan "$dir/empty")" && rc=0 || rc=$?
  if [[ "$rc" -ne 4 ]]; then
    echo "self-test FAIL: empty fixture: rc=$rc (want 4)" >&2; failed=1
  fi
  if [[ "$failed" -ne 0 ]]; then
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

# grep -r below swallows "No such file or directory", so a renamed root would
# silently shrink the scan. Refuse to scan at all if any root is missing.
for r in "${roots[@]}"; do
  if [[ ! -d "$r" ]]; then
    echo "$name: analysed ${sha}, scan root $r missing — NOTHING WAS ANALYSED (root renamed or moved? update roots in $0)" >&2
    exit 4
  fi
done

violations="$(scan "${roots[@]}")" && rc=0 || rc=$?
if [[ "$rc" -eq 4 ]]; then
  echo "$name: analysed ${sha}, no MethodNotAllowed calls under ${roots[*]} — NOTHING WAS ANALYSED (wrong cwd or empty checkout?)" >&2
  exit 4
fi

count="$(count_lines "$violations")"
if [[ "$count" -gt 0 ]]; then
  echo "$name: analysed ${sha}, ${count} line(s) with a bare MethodNotAllowed(w) call (no allowed methods):" >&2
  echo "$violations" >&2
  echo >&2
  echo "Pass the methods the handler accepts so the 405 carries an Allow header:" >&2
  echo "  MethodNotAllowed(w, http.MethodGet, http.MethodPost)" >&2
  exit 1
fi

echo "$name: analysed ${sha}, no bare MethodNotAllowed(w) calls under ${roots[*]}" >&2
