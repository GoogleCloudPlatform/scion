#!/usr/bin/env bash
# Per-call-site scan-coverage guard for the source-scanning tests of pkg/hub.
#
# Many pkg/hub tests parse Go sources (go/parser, go/ast, go/packages) and
# enumerate files to guard call sites, literals or registries. When an area
# moves out of package hub (hack/pkgmove), such a test silently stops
# scanning the moved code unless it already reads the new directory. pkgmove
# asks each scanning test file to declare its coverage with a
# "pkgmove:scan-covers" marker, but it checks per FILE: once one scan in a
# file is marked, a second unmarked scan in the same file passes unseen.
# This guard checks every enumeration call instead, so a new scan needs its
# claim when it is written, not at the next move.
#
# Scope: pkg/hub/*_test.go files that import go/parser, go/ast or
# golang.org/x/tools/go/packages. In them, every enumeration call
#   filepath.Glob/Walk/WalkDir, os.ReadDir/Getwd, ioutil.ReadDir,
#   parser.ParseDir, packages.Load, fs.WalkDir/Glob/ReadDir
# must have, within the $window lines above it and in the same top-level
# function, one of:
#   - a claim that holds for every pkg/hub area, followed by a reason line:
#       // pkgmove:scan-covers pkg/hub/...
#       // <why: e.g. recursive walk of pkg/hub, no SkipDir for a subdirectory>
#     (also accepted: ./pkg/hub/..., pkg/..., ./... and ...);
#   - the list claim, followed by a reason line, in a function that uses one
#     of the guardScanSubdirs helpers (guardScanDirs, mustGuardScanDirs,
#     readGuardScanDirs, globGuardScanDirs, parseNonTestGoFiles):
#       // pkgmove:scan-covers guardScanSubdirs
#       // <why>
#   - an exemption with its reason on the same line, for a call that does
#     not read pkg/hub sources (another package's directory, the module
#     root lookup):
#       // scan-coverage:exempt <why>
# Calls inside the guardScanSubdirs helpers themselves (helpers below) are
# exempt. A per-area marker (pkg/hub/apierr) is not a claim here: it would
# need a new marker at every area move, which is what this guard prevents.
#
# Fix a finding by routing the scan through globGuardScanDirs or
# readGuardScanDirs (pkg/hub/ast_test_helpers_test.go) and adding the list
# claim, by adding a pkg/hub/... claim to a walk that really recurses into
# every pkg/hub subdirectory, or by exempting a call that reads no pkg/hub
# sources. See hack/pkgmove/README.md "Declaring scan coverage".
#
# LIMITATIONS: the check is textual and line-oriented.
#   - Comment lines and trailing comments are skipped; single-line string
#     literals are blanked, but text inside a multi-line raw string is read
#     as code.
#   - The enclosing function is taken from the last top-level func header;
#     a column-one closing brace ends it.
#   - It checks that a claim exists, not that it is true: the reviewer still
#     checks the walk (pkgmove echoes each claim in its report).
#   - A call through a func value (read := os.ReadDir) is not a call site;
#     the function that assigns it is expected to carry a claim anyway.
#
# Severity: guard-grade (an empty scan fails)
#   Scan root missing:            exit 4
#   No candidate file or call:    exit 4
#   Violations found:             exit 1
#   Clean:                        exit 0
#
# Usage: hack/check-scan-coverage.sh [--self-test]
#
# See hack/LINT-CONVENTIONS.md for the conventions this script follows.
set -euo pipefail

cd "$(dirname "$0")/.."

export LC_ALL=C

name="check-scan-coverage"
window=15

# Helpers whose enumeration calls are the guardScanSubdirs mechanism itself:
# "<file>:<function>", or "<file>:*" for every function of the file.
helpers=(
  # guardScanDirs, readGuardScanDirs, globGuardScanDirs and findHubDir.
  "ast_test_helpers_test.go:*"
  # Reads one directory; callers pass the guardScanDirs entries.
  "permission_helpers_test.go:parseNonTestGoFiles"
)

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# scan <root>: checks <root>/pkg/hub. Prints findings on stderr. Returns 0
# clean, 1 violations, 4 when nothing was analysed.
scan() {
  local root="$1" hub="$1/pkg/hub"
  if [[ ! -d "$hub" ]]; then
    echo "$name: scan root $hub is missing — NOTHING WAS ANALYSED" >&2
    return 4
  fi
  local files=() f
  while IFS= read -r f; do
    files+=("$f")
  done < <(cd "$hub" && grep -lE '^[[:space:]]*(import[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*[[:space:]]+)?"(go/parser|go/ast|golang.org/x/tools/go/packages)"' -- *_test.go 2>/dev/null | sort || true)
  if [[ ${#files[@]} -eq 0 ]]; then
    echo "$name: no pkg/hub test file imports go/parser, go/ast or go/packages under $hub — NOTHING WAS ANALYSED" >&2
    return 4
  fi

  local out="$work/out" calls="$work/calls"
  : >"$out"
  : >"$calls"
  local helper_list
  helper_list="$(printf '%s\n' "${helpers[@]}")"
  for f in "${files[@]}"; do
    awk -v file="$f" -v window="$window" -v helpers="$helper_list" -v calls="$calls" '
      function header(line,    s) {
        s = substr(line, 6)
        if (substr(s, 1, 1) == "(") { sub(/^\([^)]*\)[[:space:]]*/, "", s) }
        sub(/[[:space:](\[].*$/, "", s)
        return s
      }
      BEGIN {
        n = split(helpers, hl, "\n")
        for (i = 1; i <= n; i++) {
          if (hl[i] == "") continue
          split(hl[i], parts, ":")
          if (parts[1] == file) helperfn[parts[2]] = 1
        }
        enum = "(^|[^A-Za-z0-9_.])(filepath[.](Glob|Walk|WalkDir)|os[.](ReadDir|Getwd)|ioutil[.]ReadDir|parser[.]ParseDir|packages[.]Load|fs[.](WalkDir|Glob|ReadDir))[(]"
        anyclaim = "^[[:space:]]*// pkgmove:scan-covers ([.]/)?(pkg/hub/[.][.][.]|pkg/[.][.][.]|[.][.][.])[[:space:]]*$"
        listclaim = "^[[:space:]]*// pkgmove:scan-covers guardScanSubdirs[[:space:]]*$"
        exempt = "^[[:space:]]*// scan-coverage:exempt[[:space:]]+[^[:space:]]"
        uses = "(^|[^A-Za-z0-9_])(guardScanDirs|mustGuardScanDirs|readGuardScanDirs|globGuardScanDirs|parseNonTestGoFiles)([^A-Za-z0-9_]|$)"
      }
      # First pass: the functions that use a guardScanSubdirs helper.
      NR == FNR {
        if ($0 ~ /^func /) fn1 = header($0)
        if ($0 !~ /^[[:space:]]*\/\// && $0 ~ uses) usesHelper[fn1 == "" ? "-" : fn1] = 1
        if ($0 ~ /^}/) fn1 = ""
        next
      }
      # Second pass: claims and calls.
      FNR == 1 { fn = ""; anyAt = -1; listAt = -1; exemptAt = -1; pending = "" }
      /^func / { fn = header($0); anyAt = -1; listAt = -1; exemptAt = -1; pending = "" }
      {
        # A claim counts once its next line is a reason comment.
        if (pending != "") {
          if ($0 ~ /^[[:space:]]*\/\/[[:space:]]*[^[:space:]]/ && $0 !~ /pkgmove:scan-covers/) {
            if (pending == "any") anyAt = FNR - 1; else listAt = FNR - 1
          }
          pending = ""
        }
        if ($0 ~ anyclaim) { pending = "any"; next }
        if ($0 ~ listclaim) { pending = "list"; next }
        if ($0 ~ exempt) { exemptAt = FNR; next }
        if ($0 ~ /^[[:space:]]*\/\//) next
        line = $0
        gsub(/"([^"\\]|\\.)*"/, "\"\"", line)
        gsub(/`[^`]*`/, "``", line)
        sub(/[[:space:]]+\/\/.*$/, "", line)
        while (match(line, enum)) {
          call = substr(line, RSTART, RLENGTH)
          line = substr(line, RSTART + RLENGTH)
          sub(/^[^A-Za-z]/, "", call)
          sub(/[(]$/, "", call)
          f = (fn == "" ? "-" : fn)
          print file ":" FNR >> calls
          if (helperfn["*"] || helperfn[f]) continue
          if (anyAt >= 0 && FNR - anyAt <= window) continue
          if (exemptAt >= 0 && FNR - exemptAt <= window) continue
          if (listAt >= 0 && FNR - listAt <= window) {
            if (usesHelper[f]) continue
            printf "pkg/hub/%s:%d: %s in %s: the guardScanSubdirs claim at line %d holds only for a function that reads through the guardScanSubdirs helpers, and %s uses none\n", file, FNR, call, f, listAt, f
            continue
          }
          printf "pkg/hub/%s:%d: %s in %s has no covering claim in the %d lines above it\n", file, FNR, call, f, window
        }
      }
      /^}/ { fn = ""; anyAt = -1; listAt = -1; exemptAt = -1 }
    ' "$hub/$f" "$hub/$f" >>"$out"
  done

  local ncalls nfind
  ncalls="$(wc -l <"$calls" | tr -d ' ')"
  nfind="$(wc -l <"$out" | tr -d ' ')"
  if [[ "$ncalls" -eq 0 ]]; then
    echo "$name: no enumeration call found in ${#files[@]} candidate file(s) under $hub — NOTHING WAS ANALYSED" >&2
    return 4
  fi
  echo "$name: analysed ${#files[@]} source-scanning test file(s), $ncalls enumeration call(s) under $hub" >&2
  if [[ "$nfind" -gt 0 ]]; then
    echo "$name: $nfind enumeration call(s) without a covering claim:" >&2
    sed 's/^/  /' "$out" >&2
    echo >&2
    echo "Route the scan through globGuardScanDirs or readGuardScanDirs (pkg/hub/ast_test_helpers_test.go)" >&2
    echo "and add \"// pkgmove:scan-covers guardScanSubdirs\" plus a reason line; or, for a walk that" >&2
    echo "recurses into every pkg/hub subdirectory, \"// pkgmove:scan-covers pkg/hub/...\" plus a reason line;" >&2
    echo "or, for a call that reads no pkg/hub sources, \"// scan-coverage:exempt <reason>\"." >&2
    echo "Put the claim in the same function, at most $window lines above the call." >&2
    return 1
  fi
  return 0
}

self_test() {
  local dir="$work/fixture" hub failed=0 rc out
  hub="$dir/pkg/hub"
  mkdir -p "$hub"

  # Clean: every verdict that passes.
  cat >"$hub/clean_test.go" <<'EOF'
package hub

import (
	"go/parser"
	"os"
	"path/filepath"
)

func walkAll() {
	// pkgmove:scan-covers pkg/hub/...
	// recursive walk of pkg/hub, no SkipDir.
	_ = filepath.WalkDir(".", nil)
	_ = parser.ParseFile
}

func viaList() {
	// pkgmove:scan-covers guardScanSubdirs
	// scanned through guardScanSubdirs; other directories alone.
	for _, dir := range []string{".", "../store"} {
		if dir == "." {
			_, _ = globGuardScanDirs(dir, "*.go")
		} else {
			_, _ = filepath.Glob(filepath.Join(dir, "*.go"))
		}
	}
}

func viaListValue() {
	read := os.ReadDir
	// pkgmove:scan-covers guardScanSubdirs
	// scanned through guardScanSubdirs.
	read = readGuardScanDirs
	_, _ = os.ReadDir("../store")
	_ = read
}

func store() {
	// scan-coverage:exempt reads ../store only.
	_, _ = os.ReadDir("../store")
	// filepath.Walk(".", nil) in a comment is not a call
	_ = "os.ReadDir(" // nor is a string
	_ = `filepath.Glob(` + "x" // nor a raw string
}

func moduleWide() {
	// pkgmove:scan-covers ./...
	// walks the whole module.
	_ = filepath.Walk("../..", nil)
}

func atWindow() {
	// pkgmove:scan-covers pkg/hub/...
	// recursive walk of pkg/hub.
	_ = 1
	_ = 2
	_ = 3
	_ = 4
	_ = 5
	_ = 6
	_ = 7
	_ = 8
	_ = 9
	_ = 10
	_ = 11
	_ = 12
	_ = 13
	_ = filepath.Walk(".", nil) // 15 lines below the claim
}
EOF
  cat >"$hub/ast_test_helpers_test.go" <<'EOF'
package hub

import "go/ast"

func globGuardScanDirs(base, pattern string) ([]string, error) { return filepath.Glob(pattern) }
func findHubDir() { _, _ = os.Getwd() }
EOF
  cat >"$hub/permission_helpers_test.go" <<'EOF'
package hub

import "go/ast"

func parseNonTestGoFiles(dir string) { _, _ = os.ReadDir(dir) }
EOF
  # Not candidates: no parser import, and a non-test file.
  printf 'package hub\n\nimport "os"\n\nfunc f() { _, _ = os.ReadDir(".") }\n' >"$hub/noparser_test.go"
  printf 'package hub\n\nimport "go/parser"\n\nfunc f() { _, _ = os.ReadDir(".") }\n' >"$hub/prod.go"

  rc=0; out="$(scan "$dir" 2>&1)" || rc=$?
  [[ $rc -eq 0 ]] || { echo "self-test FAIL: clean fixture: rc=$rc (want 0)" >&2; echo "$out" >&2; failed=1; }

  # Violations, one per verdict. The second scan of this file is the
  # per-file gap: the first one is claimed, and that must not cover it.
  cat >"$hub/bad_test.go" <<'EOF'
package hub

import (
	"go/parser"
	"os"
	"path/filepath"
)

func marked() {
	// pkgmove:scan-covers pkg/hub/...
	// recursive walk of pkg/hub.
	_ = filepath.WalkDir(".", nil)
}

func unmarked() {
	_ = filepath.WalkDir(".", nil)
}

func noReason() {
	// pkgmove:scan-covers pkg/hub/...
	_, _ = filepath.Glob("*.go")
}

func perArea() {
	// pkgmove:scan-covers pkg/hub/apierr
	// recursive walk already includes pkg/hub/apierr.
	_, _ = os.ReadDir(".")
}

func directive() {
	//pkgmove:scan-covers pkg/hub/...
	// walks pkg/hub.
	_, _ = os.ReadDir(".")
}

func listWithoutHelper() {
	// pkgmove:scan-covers guardScanSubdirs
	// claims the list but reads only pkg/hub.
	_, _ = filepath.Glob("*.go")
}

func exemptNoReason() {
	// scan-coverage:exempt
	_, _ = os.Getwd()
}

func tooFar() {
	// pkgmove:scan-covers pkg/hub/...
	// recursive walk of pkg/hub.
	_ = 1
	_ = 2
	_ = 3
	_ = 4
	_ = 5
	_ = 6
	_ = 7
	_ = 8
	_ = 9
	_ = 10
	_ = 11
	_ = 12
	_ = 13
	_ = 14
	_ = filepath.Walk(".", nil) // 16 lines below the claim
}

// pkgmove:scan-covers pkg/hub/...
// a claim above the function header does not count.
func claimOutside() {
	_ = parser.ParseDir
	_, _ = parser.ParseDir(nil, ".", nil, 0)
}
EOF
  rc=0; out="$(scan "$dir" 2>&1)" || rc=$?
  local want
  for want in \
    "pkg/hub/bad_test.go:16: filepath.WalkDir in unmarked has no covering claim" \
    "pkg/hub/bad_test.go:21: filepath.Glob in noReason has no covering claim" \
    "pkg/hub/bad_test.go:27: os.ReadDir in perArea has no covering claim" \
    "pkg/hub/bad_test.go:33: os.ReadDir in directive has no covering claim" \
    "pkg/hub/bad_test.go:39: filepath.Glob in listWithoutHelper: the guardScanSubdirs claim at line 37 holds only for a function that reads through the guardScanSubdirs helpers" \
    "pkg/hub/bad_test.go:44: os.Getwd in exemptNoReason has no covering claim" \
    "pkg/hub/bad_test.go:64: filepath.Walk in tooFar has no covering claim" \
    "pkg/hub/bad_test.go:71: parser.ParseDir in claimOutside has no covering claim" \
    "8 enumeration call(s) without a covering claim"; do
    [[ "$out" == *"$want"* ]] || { echo "self-test FAIL: missing \"$want\"" >&2; failed=1; }
  done
  [[ $rc -eq 1 ]] || { echo "self-test FAIL: violations: rc=$rc (want 1)" >&2; failed=1; }
  [[ $failed -eq 0 ]] || echo "$out" >&2
  [[ "$out" != *"bad_test.go:12:"* ]] || { echo "self-test FAIL: the claimed walk in marked was reported" >&2; failed=1; }
  rm "$hub/bad_test.go"

  # Nothing analysed.
  rm "$hub"/*_test.go
  rc=0; scan "$dir" 2>/dev/null || rc=$?
  [[ $rc -eq 4 ]] || { echo "self-test FAIL: no candidate file: rc=$rc (want 4)" >&2; failed=1; }
  printf 'package hub\n\nimport "go/ast"\n\nvar _ ast.Node\n' >"$hub/x_test.go"
  rc=0; scan "$dir" 2>/dev/null || rc=$?
  [[ $rc -eq 4 ]] || { echo "self-test FAIL: no enumeration call: rc=$rc (want 4)" >&2; failed=1; }
  rc=0; scan "$work/missing" 2>/dev/null || rc=$?
  [[ $rc -eq 4 ]] || { echo "self-test FAIL: missing root: rc=$rc (want 4)" >&2; failed=1; }

  if [[ $failed -ne 0 ]]; then
    echo "$name: self-test FAILED" >&2
    return 1
  fi
  echo "$name: self-test passed" >&2
  return 0
}

if [[ "${1:-}" == "--self-test" ]]; then
  self_test
  exit $?
fi

sha="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
if [[ "$sha" != "unknown" && -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi
echo "$name: analysed ${sha}" >&2
rc=0
scan "." || rc=$?
exit "$rc"
