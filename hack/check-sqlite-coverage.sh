#!/usr/bin/env bash
# Checks that every package with a `//go:build !no_sqlite` test file is
# tested with SQLite enabled by a blocking CI job (ptone/scion#1088).
#
# Build & Test runs `make test-fast-ci`, which uses -tags no_sqlite, so it
# never compiles those test files. They run only where a job tests the
# package untagged; the package patterns those jobs cover are SQLITE_CI_PKGS
# in the Makefile (printed by `make -s sqlite-ci-pkgs`). Six packages were
# once left out and ran only in the non-blocking Full Test Suite. This
# check fails when a package like that appears again.
#
# A package is a directory holding a *_test.go file whose //go:build line
# contains !no_sqlite. Directories under testdata/, vendor/ or
# node_modules/, and nested Go modules (a directory below the root with its
# own go.mod, e.g. extras/*), are not part of the main module's ./... and
# are skipped. A pattern ending in /... covers its directory and every
# directory below it; any other pattern covers exactly its directory.
#
# Usage:
#   hack/check-sqlite-coverage.sh               # patterns from make -s sqlite-ci-pkgs
#   hack/check-sqlite-coverage.sh PATTERN...    # explicit patterns (./dir or ./dir/...)
#   hack/check-sqlite-coverage.sh --self-test   # run the fixture tests
#
# Severity: SECURITY-GRADE no-candidates behaviour (a scan that finds no
# !no_sqlite test file at all means the scan is broken, not that the tree is
# clean).
#
# EXIT CODES (see hack/LINT-CONVENTIONS.md)
#   0  every package is covered
#   1  one or more packages are not covered (list on stderr)
#   3  a required tool is missing (make, when no patterns are given)
#   4  no !no_sqlite test file found, or no patterns
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR/.."

CHECK=check-sqlite-coverage

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

# sqlite_test_dirs ROOT: print every directory (relative, as ./dir) under
# ROOT that holds a !no_sqlite test file, one per line, sorted.
sqlite_test_dirs() {
  local root="$1" modules
  # Nested module roots, as ./dir/ prefixes. vendor, node_modules
  # (web/node_modules exists in Build & Test) and .git are not walked.
  modules="$(cd "$root" && find . \( -name vendor -o -name node_modules \
    -o -name .git \) -prune -o -name go.mod -not -path ./go.mod -print \
    | sed -e 's|/go.mod$|/|')"
  # Pattern uses POSIX ERE (-E); no GNU-only \| or \b. --exclude-dir is
  # in both GNU and BSD grep.
  (cd "$root" && grep -rlE --include='*_test.go' --exclude-dir=vendor \
    --exclude-dir=node_modules --exclude-dir=.git \
    -e '^//go:build .*!no_sqlite([^A-Za-z0-9_]|$)' . || true) \
    | grep -vE '/(testdata|vendor|node_modules)/' \
    | sed -e 's|/[^/]*$||' \
    | sort -u \
    | while IFS= read -r dir; do
        skip=0
        if [[ -n "$modules" ]]; then
          while IFS= read -r mod; do
            case "$dir/" in "$mod"*) skip=1 ;; esac
          done <<<"$modules"
        fi
        [[ "$skip" -eq 1 ]] || printf '%s\n' "$dir"
      done
}

# covered DIR PATTERN...: succeed if a pattern covers DIR.
covered() {
  local dir="$1" p base
  shift
  for p in "$@"; do
    case "$p" in ./*) ;; *) p="./$p" ;; esac
    if [[ "$p" == */... ]]; then
      base="${p%/...}"
      if [[ "$dir" == "$base" || "$dir" == "$base"/* ]]; then
        return 0
      fi
    elif [[ "$dir" == "${p%/}" ]]; then
      return 0
    fi
  done
  return 1
}

# scan ROOT PATTERN...: print uncovered directories to stderr. Returns 0
# (all covered), 1 (uncovered) or 4 (nothing to check).
scan() {
  local root="$1" dirs dir missing="" n=0
  shift
  if [[ $# -eq 0 ]]; then
    echo "$CHECK: no package patterns -- NOTHING WAS ANALYSED" >&2
    return 4
  fi
  dirs="$(sqlite_test_dirs "$root")"
  if [[ -z "$dirs" ]]; then
    echo "$CHECK: no *_test.go file with a !no_sqlite build constraint found -- NOTHING WAS ANALYSED (wrong cwd or broken scan)" >&2
    return 4
  fi
  while IFS= read -r dir; do
    n=$((n + 1))
    if ! covered "$dir" "$@"; then
      missing="${missing}  ${dir}"$'\n'
    fi
  done <<<"$dirs"
  if [[ -n "$missing" ]]; then
    {
      echo "$CHECK: these packages have //go:build !no_sqlite test files but no blocking CI job tests them with SQLite:"
      printf '%s' "$missing"
      echo "Add them to SQLITE_NONHUB_PKGS in the Makefile (run by the SQLite Tests (non-hub) job)."
    } >&2
    return 1
  fi
  echo "$CHECK: all $n package(s) with !no_sqlite test files are covered"
}

self_test() {
  local tmp rc fails=0
  tmp="$(mktemp -d)"
  # shellcheck disable=SC2064 # expand $tmp now
  trap "rm -rf '$tmp'" EXIT

  mk() { # mk FILE BUILDLINE
    mkdir -p "$tmp/$(dirname "$1")"
    printf '%s\n\npackage x\n' "$2" >"$tmp/$1"
  }
  mk go.mod 'module example.com/x'
  mk pkg/a/a_test.go '//go:build !no_sqlite'
  mk pkg/a/sub/s_test.go '//go:build !no_sqlite && unix'
  mk pkg/b/b_test.go '//go:build volume_test && !no_sqlite'
  mk cmd/c_test.go '//go:build !no_sqlite'
  mk cmd/sub/d_test.go '//go:build !no_sqlite'
  mk pkg/nosql/n_test.go '//go:build no_sqlite'
  mk pkg/other/o_test.go '//go:build !no_sqlite_extra'
  mk pkg/prod/p.go '//go:build !no_sqlite'
  mk pkg/a/testdata/t/t_test.go '//go:build !no_sqlite'
  mk web/node_modules/m/m_test.go '//go:build !no_sqlite'
  mk web/node_modules/m/go.mod 'module example.com/m'
  mk pkg/vendor/v/v_test.go '//go:build !no_sqlite'
  mk extras/mod/go.mod 'module example.com/mod'
  mk extras/mod/e_test.go '//go:build !no_sqlite'

  expect() { # expect WANT_RC WANT_STDERR_SUBSTRING|"" PATTERN...
    local want="$1" sub="$2" out
    shift 2
    rc=0
    out="$(scan "$tmp" "$@" 2>&1)" || rc=$?
    if [[ "$rc" -ne "$want" ]] || { [[ -n "$sub" ]] && [[ "$out" != *"$sub"* ]]; }; then
      echo "$CHECK self-test FAIL: patterns [$*]: want rc $want${sub:+ and '$sub'}, got rc $rc: $out" >&2
      fails=$((fails + 1))
    fi
  }
  # All covered: /... covers the dir and subdirs; an exact pattern only its dir.
  expect 0 "all 5 package(s)" ./pkg/a/... ./pkg/b ./cmd/...
  # Exact ./cmd does not cover cmd/sub.
  expect 1 "./cmd/sub" ./pkg/a/... ./pkg/b ./cmd
  # ./pkg/a (exact) leaves pkg/a/sub uncovered.
  expect 1 "./pkg/a/sub" ./pkg/a ./pkg/b ./cmd/...
  # A missing package is reported; a prefix that is not a path boundary does not cover.
  expect 1 "./pkg/b" ./pkg/a/... ./pkg/bb/... ./cmd/...
  # Patterns without ./ and with a trailing slash still match.
  expect 0 "" pkg/a/... pkg/b/ cmd/...
  # No patterns: nothing analysed.
  expect 4 "NOTHING WAS ANALYSED"
  # Nested modules, testdata, non-test files, no_sqlite and !no_sqlite_extra are ignored.
  expect 1 "./pkg/a" ./pkg/b ./cmd/...
  out="$(scan "$tmp" ./nothing 2>&1 || true)"
  for bad in extras/mod pkg/nosql pkg/other pkg/prod testdata node_modules vendor; do
    if [[ "$out" == *"$bad"* ]]; then
      echo "$CHECK self-test FAIL: $bad must not be a candidate: $out" >&2
      fails=$((fails + 1))
    fi
  done
  # An empty tree: nothing analysed.
  rm -rf "${tmp:?}"/pkg "${tmp:?}"/cmd
  expect 4 "NOTHING WAS ANALYSED" ./...

  if [[ "$fails" -ne 0 ]]; then
    echo "$CHECK self-test: $fails failure(s)" >&2
    return 1
  fi
  echo "$CHECK self-test: ok"
}

if [[ "${1:-}" == "--self-test" ]]; then
  self_test
  exit $?
fi

echo "$CHECK: analysed $(provenance)" >&2
if [[ $# -gt 0 ]]; then
  patterns=("$@")
else
  # shellcheck source=SCRIPTDIR/lib/require-tool.sh
  source "$SCRIPT_DIR/lib/require-tool.sh"
  require_tool make "$CHECK"
  patterns=()
  while IFS= read -r p; do
    [[ -n "$p" ]] && patterns+=("$p")
  done < <(make -s --no-print-directory sqlite-ci-pkgs)
  if [[ ${#patterns[@]} -eq 0 ]]; then
    echo "$CHECK: make -s sqlite-ci-pkgs printed no patterns -- NOTHING WAS ANALYSED" >&2
    exit 4
  fi
fi
rc=0
scan . "${patterns[@]}" || rc=$?
exit "$rc"
