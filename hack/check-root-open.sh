#!/usr/bin/env bash
# Flags direct (*os.Root).Open, (*os.Root).OpenFile, and (*os.Root).Create
# calls outside an explicit allowlist.
#
# Use the shared helper in pkg/util/rootopen (OpenAt, OpenRegularForRead,
# OpenFileForWrite, OpenDir) so relative path opens under an *os.Root enforce
# no-follow on every component and non-blocking leaf opens in the kernel.
#
# Every justified exception is listed in hack/root-open-allowlist.txt as:
#   <file> | <function> | <count> | <reason>
# <function> is the enclosing top-level function (a call inside a closure
# counts for its enclosing function; "-" for a call outside any function).
# No line numbers: entries survive unrelated edits. The gate fails when:
#   - a (file, function) pair has more calls than its entry (or has none), or
#   - a pair has FEWER calls than its entry, or an entry matches nothing:
#     lower the count (or delete the entry) in the same change that migrates
#     call sites.
#
# What counts: every selector call .Open(, .OpenFile(, or .Create( in a
# non-test .go file under pkg/ or cmd/ that references os.Root or os.OpenRoot,
# excluding non-os.Root calls (os.Open/OpenFile/Create, agenthome.Open,
# resolved.Open/Resolved.Open/cfg.Open, zero-arg .Open(), context-bearing
# service .OpenFile(ctx,...)/.Create(ctx,...), and FileSystem.Open).
#
# LIMITATIONS: the check is textual and line-oriented.
#   - A call in a file that receives *os.Root through a type alias without
#     naming os.Root or os.OpenRoot is not scanned.
#   - A reference inside a /* */ block comment is counted.
#   - The trailing-comment strip cuts at the first whitespace followed by //.
#   - The enclosing function is taken from the last top-level func header;
#     a column-one closing brace (or a one-line function) ends it.
#
# Severity: SECURITY-GRADE (root open confinement)
#   Missing rg:          exit 3 (nothing analysed; see hack/lib/require-tool.sh)
#   Scan root missing:   exit 4
#   Allowlist missing:   exit 4
#   No call found:       exit 4 (the helper itself always opens ".")
#   Violations found:    exit 1
#   Clean:               exit 0
#
# Usage: hack/check-root-open.sh [--self-test]
#
# See hack/LINT-CONVENTIONS.md for the conventions this script follows.
set -euo pipefail

cd "$(dirname "$0")/.."

# sort and join must agree on collation.
export LC_ALL=C

name="check-root-open"

# shellcheck source=SCRIPTDIR/lib/require-tool.sh
source hack/lib/require-tool.sh
require_tool rg "$name" "ripgrep (rg)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

root_ref_pattern='os\.Root\b|os\.OpenRoot\b'

helper_hint() {
  echo "Do not call (*os.Root).Open, OpenFile, or Create directly on untrusted" >&2
  echo "paths. Use pkg/util/rootopen (OpenRegularForRead, OpenFileForWrite," >&2
  echo "OpenDir, or OpenAt) instead. If a direct call is genuinely safe (for" >&2
  echo "example, opening \".\" on an already-verified root or a non-unix" >&2
  echo "fallback), add an entry with a one-line reason to" >&2
  echo "hack/root-open-allowlist.txt." >&2
}

# count_calls <root>
# Prints "<file>|<function> <count>" for every (file, enclosing function)
# with at least one counted call under <root>/{pkg,cmd}, paths relative to <root>.
count_calls() {
  local root="$1"
  local roots=()
  [[ -d "$root/pkg" ]] && roots+=("pkg")
  [[ -d "$root/cmd" ]] && roots+=("cmd")
  if [[ ${#roots[@]} -eq 0 ]]; then
    return 0
  fi

  (cd "$root" || exit 4
   rg -l --color never -e "$root_ref_pattern" --glob '*.go' --glob '!*_test.go' "${roots[@]}" 2>/dev/null || true) \
    | sed 's#^\./##' | sort | while IFS= read -r f; do
        awk -v file="$f" '
          /^func / {
            s = substr($0, 6)
            if (substr(s, 1, 1) == "(") { sub(/^\([^)]*\)[[:space:]]*/, "", s) }
            sub(/[[:space:](\[].*$/, "", s)
            fn = s
            oneline = ($0 ~ /}[[:space:]]*$/)
          }
          /^[[:space:]]*\/\// { next }
          {
            line = $0
            sub(/[[:space:]]+\/\/.*$/, "", line)
            while (match(line, /[A-Za-z0-9_]+\.(Open|OpenFile|Create)\(/)) {
              tok = substr(line, RSTART, RLENGTH)
              after = substr(line, RSTART + RLENGTH)
              line = substr(line, RSTART + RLENGTH)
              if (tok ~ /^os\.(Open|OpenFile|Create)\($/) { continue }
              if (tok == "unix.Open(") { continue }
              if (tok == "agenthome.Open(") { continue }
              if (tok ~ /^([Rr]esolved|cfg)\.Open\($/) { continue }
              if (tok == "FileSystem.Open(") { continue }
              if (tok ~ /\.Open\($/ && after ~ /^\)/) { continue }
              if (tok ~ /\.(OpenFile|Create)\($/ && after ~ /^(ctx|createCtx)[,)]/) { continue }
              n[(fn == "" ? "-" : fn)]++
            }
          }
          /^}/ || oneline { fn = ""; oneline = 0 }
          END { for (k in n) print file "|" k " " n[k] }
        ' "$root/$f"
      done | sort
}

# scan <root> <allowlist> <label>
# Compares per-(file, function) counts under <root> with <allowlist>, whose
# paths are relative to <root>. Returns 0 clean, 1 violations, 4 when
# <root>/pkg is missing, allowlist is missing, or nothing was counted.
scan() {
  local root="$1" allowlist="$2" label="$3"
  if [[ ! -d "$root/pkg" ]]; then
    echo "$name: scan root $root/pkg is missing — NOTHING WAS ANALYSED" >&2
    return 4
  fi
  if [[ ! -f "$allowlist" ]]; then
    echo "$name: allowlist $allowlist is missing — NOTHING WAS ANALYSED" >&2
    return 4
  fi

  local actual="$work/actual" expected="$work/expected"
  count_calls "$root" >"$actual"
  if [[ ! -s "$actual" ]]; then
    echo "$name: no (*os.Root).Open/OpenFile/Create call found under $root — NOTHING WAS ANALYSED" >&2
    return 4
  fi

  # Allowlist: "<file> | <function> | <count> | <reason>", '#' comments and
  # blank lines ignored. A malformed line is a parse error (failing closed).
  local parsed="$work/parsed"
  if ! awk -F'|' '{ sub(/\r$/, "") }
             /^[[:space:]]*(#|$)/ { next }
             { f = $1; fn = $2; c = $3; r = $4
               gsub(/[[:space:]]/, "", f)
               gsub(/[[:space:]]/, "", fn)
               gsub(/[[:space:]]/, "", c)
               gsub(/^[[:space:]]+|[[:space:]]+$/, "", r)
               if (NF != 4 || f == "" || fn == "" || c !~ /^[0-9]+$/ || r == "") {
                 printf "%s: allowlist parse error at line %d: want \"<file> | <function> | <count> | <reason>\", got: %s\n", name, NR, $0 > "/dev/stderr"
                 bad = 1; next
               }
               print f "|" fn " " c }
             END { exit bad }' name="$name" \
    "$allowlist" >"$parsed"; then
    echo "Fix the malformed line(s) in $allowlist." >&2
    return 1
  fi
  sort "$parsed" >"$expected"

  local over under
  over="$(join -a1 -e0 -o '0,1.2,2.2' "$actual" "$expected" | awk '$2 > $3 { print "  " $1 ": " $2 " (allowlist " $3 ")" }')"
  under="$(join -a2 -e0 -o '0,1.2,2.2' "$actual" "$expected" | awk '$2 < $3 { print "  " $1 ": " $2 " (allowlist " $3 ")" }')"

  local total
  total="$(awk '{ s += $2 } END { print s + 0 }' "$actual")"
  echo "$name: analysed ${label}, ${total} direct (*os.Root).Open/OpenFile/Create call(s)" >&2

  local rc=0
  if [[ -n "$over" ]]; then
    echo "$name: direct (*os.Root).Open/OpenFile/Create call(s) outside the allowlist (file|function):" >&2
    echo "$over" >&2
    echo >&2
    helper_hint
    rc=1
  fi
  if [[ -n "$under" ]]; then
    echo "$name: allowlist is stale (fewer calls than recorded, file|function):" >&2
    echo "$under" >&2
    echo "Lower the count, or delete the entry, in $allowlist." >&2
    rc=1
  fi
  return "$rc"
}

self_test() {
  local dir="$work/fixture" failed=0 rc out
  mkdir -p "$dir/pkg/util/rootopen" "$dir/pkg/hub"

  cat >"$dir/pkg/util/rootopen/rootopen_unix.go" <<'EOF'
package rootopen
import "os"
func OpenAt(dir *os.Root, name string) (*os.File, error) {
	return dir.Open(".")
}
EOF
  cat >"$dir/pkg/hub/ok.go" <<'EOF'
package hub
import "os"
func safe(r *os.Root) {
	// r.Open("commented") is ignored
	_ = 1 // r.OpenFile("trailing", 0, 0) is ignored
	_, _ = os.Open("plain")
	_, _ = os.OpenFile("plain", 0, 0)
	_, _ = os.Create("plain")
	_, _ = resolved.Open(false)
	_, _ = fh.Open()
	_, _ = svc.Create(ctx, req)
}
EOF
  printf 'package hub\nimport "os"\nfunc t(r *os.Root) { _, _ = r.Open("x") }\n' >"$dir/pkg/hub/ok_test.go"
  cat >"$dir/allow" <<'EOF'
# fixture
pkg/util/rootopen/rootopen_unix.go | OpenAt | 1 | helper opens root fd itself
EOF

  rc=0; scan "$dir" "$dir/allow" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 0 ]] || { echo "self-test FAIL: allowlisted clean tree: rc=$rc (want 0)" >&2; failed=1; }

  printf 'func badOpen(r *os.Root) { _, _ = r.Open("x") }\n' >>"$dir/pkg/hub/ok.go"
  rc=0; out="$(scan "$dir" "$dir/allow" fixture 2>&1)" || rc=$?
  [[ $rc -eq 1 && "$out" == *"pkg/hub/ok.go|badOpen: 1 (allowlist 0)"* ]] ||
    { echo "self-test FAIL: raw root.Open violation: rc=$rc (want 1): $out" >&2; failed=1; }

  cat >"$dir/pkg/hub/bad_file.go" <<'EOF'
package hub
import "os"
func badOpenFile(r *os.Root) {
	_, _ = r.OpenFile("x", os.O_RDONLY, 0)
	_, _ = r.Create("y")
}
EOF
  rc=0; out="$(scan "$dir" "$dir/allow" fixture 2>&1)" || rc=$?
  [[ $rc -eq 1 && "$out" == *"pkg/hub/bad_file.go|badOpenFile: 2 (allowlist 0)"* ]] ||
    { echo "self-test FAIL: raw root.OpenFile/Create violation: rc=$rc (want 1): $out" >&2; failed=1; }
  rm "$dir/pkg/hub/bad_file.go"

  cat >"$dir/allow" <<'EOF'
pkg/util/rootopen/rootopen_unix.go | OpenAt | 1 | helper opens root fd itself
pkg/hub/ok.go | badOpen | 2 | stale count
EOF
  rc=0; out="$(scan "$dir" "$dir/allow" fixture 2>&1)" || rc=$?
  [[ $rc -eq 1 && "$out" == *"allowlist is stale"* ]] ||
    { echo "self-test FAIL: stale count: rc=$rc (want 1): $out" >&2; failed=1; }

  cat >"$dir/allow.bad" <<'EOF'
pkg/util/rootopen/rootopen_unix.go | OpenAt | 1
EOF
  rc=0; out="$(scan "$dir" "$dir/allow.bad" fixture 2>&1)" || rc=$?
  [[ $rc -eq 1 && "$out" == *"allowlist parse error at line 1"* ]] ||
    { echo "self-test FAIL: missing reason parse error: rc=$rc (want 1): $out" >&2; failed=1; }

  rc=0; scan "$dir/missing" "$dir/allow" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 4 ]] || { echo "self-test FAIL: missing root: rc=$rc (want 4)" >&2; failed=1; }

  mkdir -p "$dir/empty/pkg"
  rc=0; scan "$dir/empty" "$dir/allow" fixture 2>/dev/null || rc=$?
  [[ $rc -eq 4 ]] || { echo "self-test FAIL: empty scan: rc=$rc (want 4)" >&2; failed=1; }

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
scan . hack/root-open-allowlist.txt "$sha" || rc=$?
if [[ $rc -eq 0 ]]; then
  echo "$name: no (*os.Root).Open/OpenFile/Create call outside the allowlist"
fi
exit "$rc"
