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
# Scope: the *_test.go files of pkg/hub and of every pkg/hub area directory
# listed in hack/area-dirs.txt (one level deep each) that import go/parser,
# go/ast or golang.org/x/tools/go/packages. Each move PR adds its directory to
# that list, so a scanning test moved with an area stays checked
# (ptone/scion#4275). In them, every enumeration call
#   filepath.Glob/Walk/WalkDir, os.ReadDir/Getwd, ioutil.ReadDir,
#   parser.ParseDir, packages.Load, fs.WalkDir/Glob/ReadDir
# must have, within the $window lines above it and in the same top-level
# function, one of:
#   - a claim that holds for every pkg/hub area, followed by a reason line:
#       // pkgmove:scan-covers pkg/hub/...
#       // <why: e.g. recursive walk of pkg/hub, no SkipDir for a subdirectory>
#     (also accepted: ./pkg/hub/..., pkg/..., ./... and ...),
#     but not in a function that uses a guardScanSubdirs helper (they read
#     each listed directory one level deep, so the claim would be false);
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
#   Scan root or a listed area
#   directory missing, or
#   hack/area-dirs.txt malformed: exit 4
#   No candidate file or call:    exit 4
#   Unknown argument, --root
#   without a directory, or
#   --root with --self-test:      exit 5
#   Violations found:             exit 1
#   Clean:                        exit 0
#
# Usage: hack/check-scan-coverage.sh [--self-test | --root <dir>]
#   --root <dir> scans <dir> (its pkg/hub and its hack/area-dirs.txt) instead
#   of the repository; the self-test uses it to run the real main path over a
#   fixture tree. A relative <dir> resolves against the repository root (the
#   script cds there first).
#
# See hack/LINT-CONVENTIONS.md for the conventions this script follows.
set -euo pipefail

self="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
repo="$(dirname "$(dirname "$self")")"
cd "$repo"

# shellcheck source=SCRIPTDIR/lib/area-dirs.sh
source "$repo/hack/lib/area-dirs.sh"

export LC_ALL=C

name="check-scan-coverage"
window=15

# parse_args <args...>: sets mode and root; on a usage error sets usage_err
# and returns 5. The self-test calls it in a subshell, so each refusal has a
# check that fails if the refusal is removed.
parse_args() {
  mode=scan
  root=""
  usage_err=""
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --self-test) mode="self-test" ;;
      --root)
        if [[ $# -lt 2 || -z "$2" ]]; then
          usage_err="--root needs a directory"
          return 5
        fi
        root="$2"
        shift
        ;;
      *)
        usage_err="unknown argument $1"
        return 5
        ;;
    esac
    shift
  done
  if [[ "$mode" == self-test && -n "$root" ]]; then
    usage_err="--root does not combine with --self-test"
    return 5
  fi
  return 0
}
if ! parse_args "$@"; then
  echo "$name: $usage_err — NOTHING WAS ANALYSED" >&2
  exit 5
fi

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

# candidate_files <dir>: the *_test.go files in <dir> that import go/parser,
# go/ast or golang.org/x/tools/go/packages, sorted. Prints nothing (and
# succeeds) when there are none or <dir> cannot be entered. Runs in a
# subshell, so the cd does not leak.
candidate_files() (
  if ! cd "$1" 2>/dev/null; then
    exit 0
  fi
  if ! grep -lE '^[[:space:]]*(import[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*[[:space:]]+)?"(go/parser|go/ast|golang.org/x/tools/go/packages)"' -- *_test.go 2>/dev/null | sort; then
    exit 0
  fi
)

# scan <root> [<dir>...]: checks <root>/pkg/hub and each <dir>, a slash
# path relative to pkg/hub (the hack/area-dirs.txt entries). Files are named
# by their path relative to pkg/hub, so a file in an area directory is never
# taken for the pkg/hub file of the same name. Prints findings on stderr.
# Returns 0 clean, 1 violations, 4 when nothing was analysed or a listed
# directory is missing.
scan() {
  local hub="$1/pkg/hub"
  shift
  if [[ ! -d "$hub" ]]; then
    echo "$name: scan root $hub is missing — NOTHING WAS ANALYSED" >&2
    return 4
  fi
  local files=() f d
  while IFS= read -r f; do
    files+=("$f")
  done < <(candidate_files "$hub")
  for d in "$@"; do
    if [[ ! -d "$hub/$d" ]]; then
      echo "$name: listed area directory $hub/$d is missing — NOTHING WAS ANALYSED" >&2
      return 4
    fi
    while IFS= read -r f; do
      files+=("$d/$f")
    done < <(candidate_files "$hub/$d")
  done
  if [[ ${#files[@]} -eq 0 ]]; then
    echo "$name: no pkg/hub test file imports go/parser, go/ast or go/packages under $hub or its $# listed area dir(s) — NOTHING WAS ANALYSED" >&2
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
          if (anyAt >= 0 && FNR - anyAt <= window) {
            if (!usesHelper[f]) continue
            printf "pkg/hub/%s:%d: %s in %s: the pkg/hub-wide claim at line %d does not hold for a function that reads through the guardScanSubdirs helpers (they read each listed directory one level deep); use the guardScanSubdirs claim\n", file, FNR, call, f, anyAt
            continue
          }
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

# reader_self_test: hack/lib/area-dirs.sh's read_area_dirs rejects the same
# inputs as the Go reader's TestParseRejects (hack/areadirs) and accepts the
# same valid list. One known divergence, which fails closed: a CRLF list is
# accepted by the Go reader (it trims the line) and rejected here (the "\r"
# fails the segment rule).
reader_self_test() {
  local failed=0 f out rc n=0
  local -a cases=(
    "empty|# only a comment\n\n"
    "one field|area\n"
    "extra field|area maintenance apierr\n"
    "unknown kind|leaf apierr\n"
    "absolute|area /pkg/hub/x\n"
    "dot|area .\n"
    "parent|area ../store\n"
    "unclean|area authz//kernel\n"
    "trailing sep|area authz/\n"
    'backslash|area authz\\kernel\n'
    "duplicate dir|area maintenance\ntestsupport maintenance\n"
  )
  f="$work/reader-case.txt"
  local c label src
  for c in "${cases[@]}"; do
    label="${c%%|*}"; src="${c#*|}"
    # shellcheck disable=SC2059 # the case text carries its own escapes
    printf "$src" >"$f"
    rc=0; out="$(read_area_dirs "$f" 2>/dev/null)" || rc=$?
    if [[ $rc -ne 1 || -n "$out" ]]; then
      echo "self-test FAIL: reader case \"$label\": rc=$rc stdout=[$out] (want rc 1, empty stdout)" >&2
      failed=1
    fi
    n=$((n + 1))
  done
  [[ $n -eq 11 ]] || { echo "self-test FAIL: reader ran $n reject cases (want 11)" >&2; failed=1; }
  printf '# comment\n\narea  maintenance\n  testsupport\tinternal/hubtestutil  \n' >"$f"
  rc=0; out="$(read_area_dirs "$f" 2>&1)" || rc=$?
  if [[ $rc -ne 0 || "$out" != $'maintenance\ninternal/hubtestutil' ]]; then
    echo "self-test FAIL: reader accept case: rc=$rc stdout=[$out]" >&2
    failed=1
  fi
  return "$failed"
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

func anyWithHelper() {
	// pkgmove:scan-covers pkg/hub/...
	// claims a recursive walk, but reads through the helpers.
	files, _ := globGuardScanDirs(".", "*.go")
	_, _ = filepath.Glob("../store/*.go")
	_ = files
}

func listNoReason() {
	// pkgmove:scan-covers guardScanSubdirs
	_, _ = readGuardScanDirs(".")
	_, _ = os.ReadDir("../store")
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
    "pkg/hub/bad_test.go:78: filepath.Glob in anyWithHelper: the pkg/hub-wide claim at line 75 does not hold for a function that reads through the guardScanSubdirs helpers" \
    "pkg/hub/bad_test.go:85: os.ReadDir in listNoReason has no covering claim" \
    "10 enumeration call(s) without a covering claim"; do
    [[ "$out" == *"$want"* ]] || { echo "self-test FAIL: missing \"$want\"" >&2; failed=1; }
  done
  [[ $rc -eq 1 ]] || { echo "self-test FAIL: violations: rc=$rc (want 1)" >&2; failed=1; }
  [[ $failed -eq 0 ]] || echo "$out" >&2
  [[ "$out" != *"bad_test.go:12:"* ]] || { echo "self-test FAIL: the claimed walk in marked was reported" >&2; failed=1; }
  rm "$hub/bad_test.go"

  # Area directories (ptone/scion#4275): the MAIN path must read the fixture
  # tree's own hack/area-dirs.txt and scan each listed directory (areas and
  # testsupport alike). An unclaimed scanning test planted in every listed
  # directory must be flagged, and a claimed call beside it must not.
  # pkg/hub/authz is a temp-only extra entry: a later move adds it to the
  # real list. A file in an area directory that shares a helper file's name
  # gets no helper exemption. The run goes through "$self --root", so the
  # wiring from the list to scan() is what is tested.
  local list areas=() a
  if ! list="$(read_area_dirs "$repo/hack/area-dirs.txt")"; then
    echo "self-test FAIL: hack/area-dirs.txt is unreadable or malformed" >&2
    return 1
  fi
  mapfile -t areas <<<"$list"
  areas+=(authz)
  mkdir -p "$dir/hack"
  : >"$dir/hack/area-dirs.txt"
  for a in "${areas[@]}"; do
    printf 'area %s\n' "$a" >>"$dir/hack/area-dirs.txt"
    mkdir -p "$hub/$a"
    cat >"$hub/$a/zz_planted_test.go" <<'EOF'
package planted

import (
	"go/parser"
	"os"
)

func unclaimed() {
	_, _ = os.ReadDir(".")
	_ = parser.ParseFile
}

func claimed() {
	// scan-coverage:exempt reads this temporary directory only.
	_, _ = os.ReadDir(".")
}
EOF
  done
  printf 'package planted\n\nimport "go/ast"\n\nfunc g() { _, _ = os.Getwd() }\n' >"$hub/${areas[0]}/ast_test_helpers_test.go"
  rc=0; out="$("$self" --root "$dir" 2>&1)" || rc=$?
  local area_failed=0
  for a in "${areas[@]}"; do
    want="pkg/hub/$a/zz_planted_test.go:9: os.ReadDir in unclaimed has no covering claim"
    [[ "$out" == *"$want"* ]] || { echo "self-test FAIL: area $a: missing \"$want\"" >&2; area_failed=1; }
    [[ "$out" != *"pkg/hub/$a/zz_planted_test.go:15:"* ]] || { echo "self-test FAIL: area $a: the claimed call was reported" >&2; area_failed=1; }
  done
  want="pkg/hub/${areas[0]}/ast_test_helpers_test.go:5: os.Getwd in g has no covering claim"
  [[ "$out" == *"$want"* ]] || { echo "self-test FAIL: missing \"$want\"" >&2; area_failed=1; }
  want="$((${#areas[@]} + 1)) enumeration call(s) without a covering claim"
  [[ "$out" == *"$want"* ]] || { echo "self-test FAIL: missing \"$want\"" >&2; area_failed=1; }
  [[ $rc -eq 1 ]] || { echo "self-test FAIL: area directories: rc=$rc (want 1)" >&2; area_failed=1; }
  [[ $area_failed -eq 0 ]] || { echo "$out" >&2; failed=1; }
  # The same main path refuses a listed directory that is missing, and a
  # malformed list.
  cp "$dir/hack/area-dirs.txt" "$work/list.ok"
  printf 'area no/such/area\n' >>"$dir/hack/area-dirs.txt"
  rc=0; "$self" --root "$dir" >/dev/null 2>&1 || rc=$?
  [[ $rc -eq 4 ]] || { echo "self-test FAIL: missing listed area directory: rc=$rc (want 4)" >&2; failed=1; }
  printf 'area\n' >"$dir/hack/area-dirs.txt"
  rc=0; "$self" --root "$dir" >/dev/null 2>&1 || rc=$?
  [[ $rc -eq 4 ]] || { echo "self-test FAIL: malformed list: rc=$rc (want 4)" >&2; failed=1; }
  cp "$work/list.ok" "$dir/hack/area-dirs.txt"
  for a in "${areas[@]}"; do
    rm -r "${hub:?}/${a%%/*}" 2>/dev/null || true
  done
  rm -r "${dir:?}/hack"

  reader_self_test || failed=1

  # Usage refusals (exit 5): end to end for the two that cannot recurse, and
  # through parse_args in a subshell for every refusal, with a control.
  local u
  for u in --bogus --root; do
    rc=0; "$self" "$u" >/dev/null 2>&1 || rc=$?
    [[ $rc -eq 5 ]] || { echo "self-test FAIL: '$u' exited $rc (want 5)" >&2; failed=1; }
  done
  rc=0; ( parse_args --bogus ) >/dev/null 2>&1 || rc=$?
  [[ $rc -eq 5 ]] || { echo "self-test FAIL: parse_args --bogus returned $rc (want 5)" >&2; failed=1; }
  rc=0; ( parse_args --root ) >/dev/null 2>&1 || rc=$?
  [[ $rc -eq 5 ]] || { echo "self-test FAIL: parse_args --root returned $rc (want 5)" >&2; failed=1; }
  rc=0; ( parse_args --self-test --root /x ) >/dev/null 2>&1 || rc=$?
  [[ $rc -eq 5 ]] || { echo "self-test FAIL: parse_args --self-test --root returned $rc (want 5)" >&2; failed=1; }
  rc=0; ( parse_args --root /x --self-test ) >/dev/null 2>&1 || rc=$?
  [[ $rc -eq 5 ]] || { echo "self-test FAIL: parse_args --root --self-test returned $rc (want 5)" >&2; failed=1; }
  rc=0; ( parse_args --root /x ) >/dev/null 2>&1 || rc=$?
  [[ $rc -eq 0 ]] || { echo "self-test FAIL: parse_args --root /x returned $rc (want 0)" >&2; failed=1; }

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

if [[ "$mode" == self-test ]]; then
  self_test
  exit $?
fi

if [[ -n "$root" ]] && ! cd "$root" 2>/dev/null; then
  echo "$name: scan root $root is missing — NOTHING WAS ANALYSED" >&2
  exit 4
fi
sha="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"
if [[ "$sha" != "unknown" && -n "$(git status --porcelain 2>/dev/null)" ]]; then
  sha="${sha}-dirty"
fi
echo "$name: analysed ${sha}" >&2
if ! list="$(read_area_dirs hack/area-dirs.txt)"; then
  echo "$name: hack/area-dirs.txt is unreadable or malformed — NOTHING WAS ANALYSED" >&2
  exit 4
fi
mapfile -t areas <<<"$list"
rc=0
scan "." "${areas[@]}" || rc=$?
exit "$rc"
