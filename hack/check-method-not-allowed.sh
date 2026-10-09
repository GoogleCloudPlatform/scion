#!/usr/bin/env bash
# A 405 response MUST carry an Allow header (RFC 9110 section 15.5.6). This
# script runs two rules:
#
# 1. Bare helper calls (pkg/hub, pkg/runtimebroker). Both packages'
#    MethodNotAllowed helpers set Allow from their variadic allowedMethods
#    argument, so a MethodNotAllowed(w) call with no methods sends a 405
#    without Allow (ptone/scion#2421, PR #1413).
#
# 2. Direct 405 writes (pkg/sciontool, extras/docs-agent,
#    extras/scion-telegram). These packages do not use a helper; they write
#    http.StatusMethodNotAllowed themselves (http.Error, writeError, ...).
#    Every such write must be preceded, within the ALLOW_WINDOW lines above
#    it (or on the same line) and in the same func, by a line that sets the
#    "Allow" header (ptone/scion#2858). Comparisons (== / !=) and comment
#    lines are not writes and are ignored. The pkg/hub and pkg/runtimebroker
#    helper bodies write the status from a variable list, so those packages
#    stay on rule 1.
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
#   No candidates:     exit 4 (rule 1: no MethodNotAllowed call found at all;
#                      rule 2: no StatusMethodNotAllowed write found at all.
#                      Either means the scan roots are wrong, not that the
#                      tree is clean)
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
status_roots=(pkg/sciontool extras/docs-agent extras/scion-telegram)
# Lines above a StatusMethodNotAllowed write searched for an Allow header.
ALLOW_WINDOW=3

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

# scan_status <root>... — print direct 405 writes with no Allow header set in
# the ALLOW_WINDOW lines above (or on the same line) as path:line:text.
# Returns 4 if no non-test Go file under the roots mentions
# StatusMethodNotAllowed.
scan_status() {
  local candidates
  candidates="$(grep -rlF --include='*.go' --exclude='*_test.go' \
    'StatusMethodNotAllowed' "$@" 2>/dev/null || true)"
  if [[ -z "$candidates" ]]; then
    return 4
  fi
  # shellcheck disable=SC2086 # candidates is a newline-separated path list
  awk -v win="$ALLOW_WINDOW" '
    FNR == 1 { last_allow = -1000 }
    /^[ \t]*func / { last_allow = -1000 }  # an Allow in one func cannot cover the next
    /"Allow"/ { last_allow = FNR }
    /StatusMethodNotAllowed/ {
      if ($0 ~ /^[ \t]*\/\//) next          # comment line
      if ($0 ~ /[=!]=/) next                  # comparison, not a write
      if (FNR - last_allow > win) print FILENAME ":" FNR ":" $0
    }
  ' $candidates
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
  trap "rm -rf '$dir'" EXIT

  mkdir -p "$dir/clean" "$dir/bad" "$dir/empty"
  cat >"$dir/clean/h.go" <<'GO'
package h
func MethodNotAllowed(w http.ResponseWriter, allowedMethod string, otherMethods ...string) {}
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

  mkdir -p "$dir/sclean" "$dir/sbad" "$dir/sempty"
  cat >"$dir/sclean/h.go" <<'GO'
package h
func a(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
func b(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET"); writeError(w, http.StatusMethodNotAllowed, "x")
}
func c(code int) bool { return code == http.StatusMethodNotAllowed }
// http.Error(w, "x", http.StatusMethodNotAllowed) in a comment is ignored.
GO
  cat >"$dir/sbad/h.go" <<'GO'
package h
func a(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
func b(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET")
	x := 1
	y := 2
	z := 3
	writeError(w, http.StatusMethodNotAllowed, "too far from Allow")
}
GO
  cat >"$dir/sbad/h2.go" <<'GO'
package h
func c(w http.ResponseWriter) {
	w.WriteHeader(http.StatusMethodNotAllowed) // Allow set in the previous file does not count
}
GO
  cat >"$dir/sbad/h_test.go" <<'GO'
package h
func TestBare(t *testing.T) { http.Error(w, "x", http.StatusMethodNotAllowed) }
GO
  cat >"$dir/sempty/h.go" <<'GO'
package h
GO
  out="$(scan_status "$dir/sclean")" && rc=0 || rc=$?
  n="$(count_lines "$out")"
  if [[ "$rc" -ne 0 || "$n" -ne 0 ]]; then
    echo "self-test FAIL: status clean fixture: rc=$rc count=$n" >&2; failed=1
  fi
  out="$(scan_status "$dir/sbad")" && rc=0 || rc=$?
  n="$(count_lines "$out")"
  if [[ "$rc" -ne 0 || "$n" -ne 3 ]]; then
    echo "self-test FAIL: status bad fixture: rc=$rc count=$n (want 3, test file excluded)" >&2; failed=1
  fi
  out="$(scan_status "$dir/sempty")" && rc=0 || rc=$?
  if [[ "$rc" -ne 4 ]]; then
    echo "self-test FAIL: status empty fixture: rc=$rc (want 4)" >&2; failed=1
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
for r in "${roots[@]}" "${status_roots[@]}"; do
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
status_violations="$(scan_status "${status_roots[@]}")" && rc=0 || rc=$?
if [[ "$rc" -eq 4 ]]; then
  echo "$name: analysed ${sha}, no StatusMethodNotAllowed writes under ${status_roots[*]} — NOTHING WAS ANALYSED (wrong cwd or empty checkout?)" >&2
  exit 4
fi

failed=0
count="$(count_lines "$violations")"
if [[ "$count" -gt 0 ]]; then
  echo "$name: analysed ${sha}, ${count} line(s) with a bare MethodNotAllowed(w) call (no allowed methods):" >&2
  echo "$violations" >&2
  echo >&2
  echo "Pass the methods the handler accepts so the 405 carries an Allow header:" >&2
  echo "  MethodNotAllowed(w, http.MethodGet, http.MethodPost)" >&2
  failed=1
fi
status_count="$(count_lines "$status_violations")"
if [[ "$status_count" -gt 0 ]]; then
  echo "$name: analysed ${sha}, ${status_count} line(s) writing StatusMethodNotAllowed with no Allow header set in the ${ALLOW_WINDOW} line(s) above:" >&2
  echo "$status_violations" >&2
  echo >&2
  echo "Set Allow to the methods the handler accepts before writing the 405:" >&2
  echo '  w.Header().Set("Allow", http.MethodPost)' >&2
  failed=1
fi
if [[ "$failed" -ne 0 ]]; then
  exit 1
fi

echo "$name: analysed ${sha}, no bare MethodNotAllowed(w) calls under ${roots[*]} and no 405 writes without Allow under ${status_roots[*]}" >&2
