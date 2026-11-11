#!/usr/bin/env bash
# winsec-guard: static guard for the Windows build's file-protection
# rules.
#
# WHAT IT CHECKS
# Over every non-test Go file of this module that a GOOS=windows build of
# ./cmd/scion compiles (from `go list -deps`, so _unix.go and other
# excluded files are already out):
#   (a) chmod-as-protection  no .Chmod( / os.Chmod / Fchmod. On Windows a
#                            chmod only toggles the read-only attribute and
#                            protects nothing. A hit is accepted only if the
#                            file is a _unix.go file, the line carries
#                            "//winsec:not-protection <reason>", or the file
#                            is listed in hack/winsec-unprotected.txt.
#   (b) string-containment   no filepath.Rel( whose result is checked with
#                            HasPrefix(..., "..") within the next 5 lines,
#                            outside pkg/util/pathcanon. Containment goes
#                            through pathcanon.Within. A hit is accepted only
#                            if the Rel line carries
#                            "//winsec:not-containment <reason>" or the file
#                            is listed in hack/winsec-unprotected.txt.
#   (c) nofollow-zero        no O_NOFOLLOW or openNoFollow constant defined
#                            as 0 in a file built only off unix (see LIMITS).
#   (d) return-nil-check     no "return nil" inside a function whose name
#                            matches check*Private* in a file built only
#                            off unix; "return true, nil" counts too.
#
# LIMITS (read before trusting a green run)
# These are text checks, not type checks. (b) sees a Rel result only when it
# reaches HasPrefix within 5 lines and the ".." prefix is a literal in that
# call (a named constant or a helper hides it). (c) sees only a literal zero
# (0, 00, 0x0, 0o0).
# (d) sees only functions named check*Private*; a privacy check with another
# name is not covered. (c) and (d) apply to files built off unix only when
# that shows in the file name (_windows.go, _other.go) or a //go:build line
# naming windows, !unix or !linux. Review still owns these rules.
#
# LIST
# hack/winsec-unprotected.txt: one file per line, "<path> <status>", where
# status starts with "pending Phase 3" (a client-reachable private-file site
# not yet on privatefs/pathcanon; the keyword is what this script parses) or
# "not protected on Windows: gated" (a server or broker site behind the
# Windows command gate). Every listed path must exist and appear once. The
# list states openly which files are not protected on Windows.
#
# USAGE
#   hack/winsec-guard.sh              run the guard (repo root or any subdir)
#   hack/winsec-guard.sh --self-test  run the checks on built-in fixtures
# GO names the go command (default: go). It is only used for `go list`,
# which does not compile anything.
#
# EXIT: 0 clean; 1 findings; 2 usage, tool or list error.

set -euo pipefail

GO="${GO:-go}"
NOT_PROTECTION_MARK='//winsec:not-protection [^[:space:]]'
NOT_CONTAINMENT_MARK='//winsec:not-containment [^[:space:]]'

die() {
  printf 'winsec-guard: %s\n' "$*" >&2
  exit 2
}

# load_list LISTFILE: validates the list and prints its paths, one per line.
# (Plain bash 3.2: no associative arrays.)
load_list() {
  local list=$1 line path status n=0 seen=$'\n'
  [ -f "$list" ] || die "missing list $list"
  while IFS= read -r line || [ -n "$line" ]; do
    n=$((n + 1))
    case "$line" in '' | '#'*) continue ;; esac
    path=${line%%[[:space:]]*}
    status=${line#"$path"}
    status=${status#"${status%%[![:space:]]*}"}
    case "$status" in
      'pending Phase 3'* | 'not protected on Windows: gated'*) ;;
      *) die "$list:$n: status must start with 'pending Phase 3' or 'not protected on Windows: gated': $line" ;;
    esac
    [ -f "$path" ] || die "$list:$n: no such file: $path"
    case "$seen" in *$'\n'"$path"$'\n'*) die "$list:$n: duplicate entry: $path" ;; esac
    seen="$seen$path"$'\n'
    printf '%s\n' "$path"
  done <"$list"
}

# run_checks FILELIST LISTFILE: prints one finding per line; returns 1 if any.
run_checks() {
  local files=$1 list=$2 listed f found=0 exempt
  listed=$'\n'$(load_list "$list")$'\n' || exit 2

  while IFS= read -r f; do
    [ -n "$f" ] || continue
    [ -f "$f" ] || die "file from go list is missing: $f"
    exempt=
    case "$listed" in *$'\n'"$f"$'\n'*) exempt=1 ;; esac

    # (a) chmod-as-protection
    if [ -z "$exempt" ] && [[ "$f" != *_unix.go ]]; then
      while IFS= read -r hit; do
        [ -n "$hit" ] || continue
        printf '%s:%s: chmod-as-protection: Chmod protects nothing on Windows; use privatefs, or mark the line %s\n' \
          "$f" "${hit%%:*}" "'//winsec:not-protection <reason>'"
        found=1
      done < <(grep -nE '\.Chmod\(|os\.Chmod\b|Fchmod' "$f" |
        grep -vE '^[0-9]+:[[:space:]]*//' |
        grep -v -- "$NOT_PROTECTION_MARK" || true)
    fi

    # (b) string-containment
    if [ -z "$exempt" ] && [[ "$f" != pkg/util/pathcanon/* ]]; then
      while IFS= read -r ln; do
        [ -n "$ln" ] || continue
        printf '%s:%s: string-containment: filepath.Rel + HasPrefix("..") decides containment on strings; use pathcanon.Within, or mark the Rel line %s\n' \
          "$f" "$ln" "'//winsec:not-containment <reason>'"
        found=1
      done < <(awk -v mark="$NOT_CONTAINMENT_MARK" '
        /filepath\.Rel\(/ && $0 !~ /^[[:space:]]*\/\// {
          if ($0 ~ mark) { pending = 0; next }
          pending = NR
          if ($0 ~ /HasPrefix\(.*"\.\./) { print NR; pending = 0 }
          next
        }
        pending && NR - pending <= 5 && /HasPrefix\(.*"\.\./ { print pending; pending = 0 }
      ' "$f")
    fi

    # (c) and (d) apply to files built only off unix: by file name, or by a
    # //go:build line that names windows or negates unix or linux.
    case "$f" in
      *_windows.go | *_other.go) ;;
      *)
        grep -m1 -E '^//go:build .*(windows|!unix|!linux)' "$f" >/dev/null || continue
        ;;
    esac

    # (c) nofollow-zero
    while IFS= read -r hit; do
      [ -n "$hit" ] || continue
      printf '%s:%s: nofollow-zero: a no-follow flag defined as 0 silently follows links\n' "$f" "${hit%%:*}"
      found=1
    done < <(grep -nE '(O_NOFOLLOW|openNoFollow)[[:space:]]*([A-Za-z0-9_]+[[:space:]]*)?=[[:space:]]*0([xXoO]?0*)?[[:space:]]*($|//|\))' "$f" || true)

    # (d) return-nil-check
    while IFS= read -r ln; do
      [ -n "$ln" ] || continue
      printf '%s:%s: return-nil-check: a privacy check that returns nil on this platform is POSIX reasoning behind a build tag\n' "$f" "$ln"
      found=1
    done < <(awk '
      # A return whose last result is nil: "return nil", "return true, nil".
      BEGIN { ret = "return[[:space:]]+([^/]*,[[:space:]]*)?nil[[:space:]]*($|//|})" }
      /^func / {
        name = $0
        sub(/^func (\([^)]*\) )?/, "", name)
        sub(/\(.*/, "", name)
        infn = (tolower(name) ~ /^check.*private/)
        if (infn && $0 ~ /\{.*\}[[:space:]]*$/) {
          if ($0 ~ ret) print NR
          infn = 0
        }
        next
      }
      /^}/ { infn = 0; next }
      infn && $0 ~ ret { print NR }
    ' "$f")
  done <"$files"
  return "$found"
}

self_test() {
  local dir rc=0
  dir=$(mktemp -d) || die "mktemp failed"
  (
    cd "$dir" || exit 2
    mkdir -p p
    cat >p/bad.go <<'EOF'
package p
func a() { _ = f.Chmod(0o600) }
var chmod = os.Chmod
func b() {
	rel, err := filepath.Rel(root, p)
	if err != nil || strings.HasPrefix(rel, "..") {
	}
}
func c() {
	rel, _ := filepath.Rel(root, p)
	if strings.HasPrefix(filepath.ToSlash(rel), "../") {
	}
}
EOF
    cat >p/ok.go <<'EOF'
package p
func a() { _ = os.Chmod(p, 0o755) } //winsec:not-protection mode 0755 is not a privacy mode
func b() {
	rel, _ := filepath.Rel(home, p) //winsec:not-containment display only
	if !strings.HasPrefix(rel, "..") {
	}
	// os.Chmod in a comment is fine
}
EOF
    cat >p/listed.go <<'EOF'
package p
func a() { _ = f.Chmod(0o600) }
EOF
    cat >p/x_unix.go <<'EOF'
package p
func a() { _ = f.Chmod(0o600) }
EOF
    cat >p/bad_other.go <<'EOF'
package p
const openNoFollow = 0
func checkPrivateFile(f *os.File) error {
	return nil
}
func checkPrivateOwner(f *os.File) (bool, error) {
	return true, nil
}
func checkPrivateMode() (bool, error) { return false, nil }
EOF
    cat >p/nounix.go <<'EOF'
//go:build !unix

package p
const O_NOFOLLOW = 0
const openNoFollow = 0x0
const keep = 0x20000
EOF
    cat >p/ok_other.go <<'EOF'
package p
const openNoFollow = 0x20000
func checkPrivateFile(f *os.File) error {
	return errors.ErrUnsupported
}
func helper() error { return nil }
func checkPrivateErr() (bool, error) { return false, errors.ErrUnsupported }
EOF
    cat >p/portable.go <<'EOF'
package p
func checkPrivateRecord(f *os.File) error {
	return nil
}
EOF
    printf 'p/listed.go not protected on Windows: gated (fixture)\n' >list.txt
    printf '%s\n' p/bad.go p/ok.go p/listed.go p/x_unix.go p/bad_other.go p/ok_other.go p/nounix.go p/portable.go >files.txt
    set +e
    local out
    out=$(run_checks files.txt list.txt)
    rc=$?
    set -e
    want=$'p/bad.go:2: chmod-as-protection\np/bad.go:3: chmod-as-protection\np/bad.go:5: string-containment\np/bad.go:10: string-containment\np/bad_other.go:2: nofollow-zero\np/bad_other.go:4: return-nil-check\np/bad_other.go:7: return-nil-check\np/bad_other.go:9: return-nil-check\np/nounix.go:4: nofollow-zero\np/nounix.go:5: nofollow-zero'
    got=$(printf '%s\n' "$out" | sed -E 's/^([^:]+:[0-9]+: [a-z-]+):.*/\1/')
    if [ "$rc" != 1 ] || [ "$got" != "$want" ]; then
      printf 'self-test FAILED (rc=%s)\nwant:\n%s\ngot:\n%s\n' "$rc" "$want" "$got" >&2
      exit 1
    fi
    # List errors exit 2, on a file set that is otherwise clean.
    printf 'p/ok.go\n' >clean.txt
    run_checks clean.txt list.txt >/dev/null || { echo 'self-test FAILED: clean set rejected' >&2; exit 1; }
    for bad in 'p/missing.go not protected on Windows: gated' 'p/listed.go because' \
      $'p/listed.go pending Phase 3\np/listed.go pending Phase 3'; do
      printf '%s\n' "$bad" >badlist.txt
      set +e
      (run_checks clean.txt badlist.txt >/dev/null 2>&1)
      rc=$?
      set -e
      if [ "$rc" != 2 ]; then
        printf 'self-test FAILED: list %q gave rc=%s, want 2\n' "$bad" "$rc" >&2
        exit 1
      fi
    done
  ) || rc=$?
  rm -rf -- "$dir"
  [ "$rc" = 0 ] || exit 1
  echo 'winsec-guard: self-test ok'
}

main() {
  case "${1:-}" in
    --self-test) self_test; return ;;
    '') ;;
    *) die "usage: $0 [--self-test]" ;;
  esac
  local root rc
  root=$(git rev-parse --show-toplevel) || die "not in a git checkout"
  # The physical path, so go list's directories carry the same prefix.
  root=$(cd "$root" && pwd -P) || die "cannot resolve $root"
  cd "$root"
  files=$(mktemp) || die "mktemp failed"
  trap 'rm -f -- "${files:-}" "${files:-}.raw"' EXIT
  # shellcheck disable=SC2016 # a go template, not a shell expansion
  GOOS=windows GOARCH=amd64 "$GO" list -deps \
    -f '{{if and (not .Standard) .Module}}{{if .Module.Main}}{{$d := .Dir}}{{range .GoFiles}}{{$d}}/{{.}}{{"\n"}}{{end}}{{end}}{{end}}' \
    ./cmd/scion >"$files.raw" || die "go list failed"
  local line
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    case "$line" in
      "$root"/*) printf '%s\n' "${line#"$root/"}" ;;
      *) die "go list reported a file outside $root: $line" ;;
    esac
  done <"$files.raw" | sort -u >"$files"
  rm -f -- "$files.raw"
  [ -s "$files" ] || die "go list returned no files"
  rc=0
  run_checks "$files" hack/winsec-unprotected.txt || rc=$?
  if [ "$rc" = 0 ]; then
    printf 'winsec-guard: ok (%s files)\n' "$(wc -l <"$files" | tr -d ' ')"
  else
    printf 'winsec-guard: findings above; see the header of %s\n' "$0" >&2
  fi
  return "$rc"
}

main "$@"
