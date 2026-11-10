#!/usr/bin/env bash
# Flag type assertions of an ent client's driver to *entsql.Driver.
#
# The hub store's ent driver may be wrapped by the transaction diagnostics
# (pkg/observability/txdiag.EntDriver), and an assertion
# `x.(*entsql.Driver)` fails on the wrapper: a checked assertion silently
# takes its !ok branch (for example skipping the SQLite FK reconcile, or a
# store reporting "not backed by a *sql.DB"), and an unchecked one panics.
# Code that needs the *sql.DB of an ent client calls entc.SQLDriver or
# entc.SQLDB, which unwrap. The only allowed assertion is the one inside
# entc.SQLDriver itself (pkg/ent/entc/client.go, func SQLDriver).
#
# Scans non-test AND _test.go files under pkg, cmd and internal. Comment lines are
# ignored.
#
# Severity: FORMATTING-GRADE exit-code handling.
#   Missing tool:      exit 3 (nothing analysed; see hack/lib/require-tool.sh)
#   Missing scan root: exit 4 (a root directory does not exist)
#   No candidates:     exit 0 (no assertion anywhere is a clean state)
#   Violations found:  exit 1 (file:line list on stderr)
#   Clean:             exit 0
#
# Usage: hack/check-entsql-driver-assert.sh [--self-test]
#
# See hack/LINT-CONVENTIONS.md for the conventions this script follows.
set -euo pipefail

cd "$(dirname "$0")/.."

# shellcheck source=SCRIPTDIR/lib/require-tool.sh
source hack/lib/require-tool.sh
require_tool grep check-entsql-driver-assert
require_tool awk check-entsql-driver-assert

name="check-entsql-driver-assert"
roots=(pkg cmd internal)
pattern='.(*entsql.Driver)'

# Allowed: file path + enclosing function (never line numbers).
allowed_file="pkg/ent/entc/client.go"
allowed_func="SQLDriver"

provenance() {
  local sha
  if ! sha="$(git rev-parse --short HEAD 2>/dev/null)"; then
    printf '(git unavailable or not a worktree)'
    return
  fi
  if [[ -n "$(git status --porcelain 2>/dev/null)" ]]; then
    printf '%s-dirty' "$sha"
  else
    printf '%s' "$sha"
  fi
}

# enclosing_func <file> <line> — print the name of the top-level func
# enclosing <line>, or nothing.
enclosing_func() {
  awk -v target="$2" '
    NR > target { exit }
    /^func / {
      s = $0
      sub(/^func[ \t]+(\([^)]*\)[ \t]*)?/, "", s)
      sub(/[(\[].*/, "", s)
      fn = s
    }
    END { print fn }
  ' "$1"
}

# scan <root>... — print violations as path:line:text on stdout.
scan() {
  local root hits path line text
  for root in "$@"; do
    if [[ ! -d "$root" ]]; then
      echo "${name}: scan root ${root} does not exist — NOTHING WAS ANALYSED" >&2
      return 4
    fi
  done
  hits="$(grep -rnF --include='*.go' "$pattern" "$@" 2>/dev/null || true)"
  [[ -z "$hits" ]] && return 0
  while IFS= read -r hit; do
    path="${hit%%:*}"
    line="${hit#*:}"; line="${line%%:*}"
    text="${hit#*:*:}"
    # Comment lines are not assertions.
    if [[ "$text" =~ ^[[:space:]]*// ]]; then
      continue
    fi
    if [[ "$path" == "$allowed_file" || "$path" == */"$allowed_file" ]] &&
      [[ "$(enclosing_func "$path" "$line")" == "$allowed_func" ]]; then
      continue
    fi
    printf '%s\n' "$hit"
  done <<<"$hits"
}

self_test() {
  local tmp rc out
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' RETURN
  mkdir -p "$tmp/pkg/ent/entc" "$tmp/pkg/store" "$tmp/cmd"
  cat >"$tmp/pkg/ent/entc/client.go" <<'EOF'
package entc

func SQLDriver(d dialect.Driver) (*entsql.Driver, bool) {
	if sd, ok := d.(*entsql.Driver); ok {
		return sd, true
	}
	return nil, false
}

func other(d dialect.Driver) {
	_ = d.(*entsql.Driver)
}
EOF
  cat >"$tmp/pkg/store/clean.go" <<'EOF'
package store

// A comment may mention x.(*entsql.Driver) without matching.
func f(c *ent.Client) { _, _ = entc.SQLDriver(c.Driver()) }
EOF
  cat >"$tmp/pkg/store/bad.go" <<'EOF'
package store

func g(c *ent.Client) { drv, ok := c.Driver().(*entsql.Driver); _, _ = drv, ok }
EOF
  cat >"$tmp/cmd/bad_test.go" <<'EOF'
package cmd

func h(c *ent.Client) { _ = c.Driver().(*entsql.Driver).DB() }
EOF
  out="$(cd "$tmp" && scan pkg cmd)"
  rc=$?
  local fail=0
  # expect <desc> <command...>: the check passes when the command succeeds.
  # expect_not <desc> <command...>: it passes when the command fails.
  expect() {
    local desc="$1"
    shift
    if "$@"; then echo "  ok: ${desc}"; else echo "  FAIL: ${desc}"; fail=1; fi
  }
  expect_not() {
    local desc="$1"
    shift
    if "$@"; then echo "  FAIL: ${desc}"; fail=1; else echo "  ok: ${desc}"; fi
  }
  expect "scan returns 0 (violations are printed, not returned)" test "$rc" -eq 0
  expect_not "allowed SQLDriver body is not flagged" grep -q "client.go:4:" <<<"$out"
  expect "another func in the allowed file is flagged" grep -q "client.go:11:" <<<"$out"
  expect_not "comment line is not flagged" grep -q "clean.go" <<<"$out"
  expect "non-test violation is flagged" grep -q "pkg/store/bad.go:3:" <<<"$out"
  expect "_test.go violation is flagged" grep -q "cmd/bad_test.go:3:" <<<"$out"
  expect "exactly 3 violations" test "$(grep -c . <<<"$out")" -eq 3
  rm -rf "$tmp/cmd"
  set +e
  (cd "$tmp" && scan pkg cmd >/dev/null 2>&1)
  rc=$?
  set -e
  expect "missing scan root exits 4" test "$rc" -eq 4
  if [[ $fail -ne 0 ]]; then
    echo "${name}: self-test FAILED"
    return 1
  fi
  echo "${name}: self-test passed"
}

if [[ "${1:-}" == "--self-test" ]]; then
  self_test
  exit $?
fi

echo "${name}: $(provenance)"
set +e
violations="$(scan "${roots[@]}")"
rc=$?
set -e
if [[ $rc -ne 0 ]]; then
  exit "$rc"
fi
if [[ -n "$violations" ]]; then
  echo "${name}: type assertion to *entsql.Driver (use entc.SQLDriver or entc.SQLDB):" >&2
  printf '%s\n' "$violations" >&2
  exit 1
fi
echo "${name}: OK"
