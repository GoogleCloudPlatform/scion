#!/usr/bin/env bash
# Shared reader for hack/area-dirs.txt, the list of pkg/hub area directories
# that every source-scanning guard must cover (ptone/scion#4275). Source
# it, then:
#
#   # shellcheck source=SCRIPTDIR/lib/area-dirs.sh
#   source "$(dirname "$0")/lib/area-dirs.sh"
#   list="$(read_area_dirs hack/area-dirs.txt)" || exit 4
#   mapfile -t dirs <<<"$list"
#
# (Not mapfile < <(read_area_dirs ...): a process substitution's exit status
# is lost, so a malformed list would read as an empty one.)
#
# It applies the same rules as the Go reader (hack/areadirs): "<kind> <dir>"
# per line, kind area or testsupport, dir a clean slash path inside pkg/hub,
# no duplicates, at least one entry. Blank and # lines are ignored.

# read_area_dirs <file>: prints each listed dir, one per line, in file
# order. On a missing, malformed or empty list it prints the reason on
# stderr, prints nothing on stdout, and returns 1.
read_area_dirs() {
  local file="$1"
  if [[ ! -r "$file" ]]; then
    echo "area-dirs: cannot read $file" >&2
    return 1
  fi
  local out
  if ! out="$(LC_ALL=C awk '
    /^[[:space:]]*$/ || /^[[:space:]]*#/ { next }
    {
      if (NF != 2) { printf "area-dirs: %s:%d: want \"<kind> <dir>\"\n", FILENAME, FNR > "/dev/stderr"; bad = 1; exit 1 }
      if ($1 != "area" && $1 != "testsupport") { printf "area-dirs: %s:%d: unknown kind %s\n", FILENAME, FNR, $1 > "/dev/stderr"; bad = 1; exit 1 }
      d = $2
      n = split(d, seg, "/")
      for (i = 1; i <= n; i++) {
        if (seg[i] == "" || seg[i] == "." || seg[i] == ".." || seg[i] !~ /^[A-Za-z0-9_.-]+$/) {
          printf "area-dirs: %s:%d: directory %s is not a clean slash path inside pkg/hub\n", FILENAME, FNR, d > "/dev/stderr"; bad = 1; exit 1
        }
      }
      if (d in seen) { printf "area-dirs: %s:%d: directory %s listed twice\n", FILENAME, FNR, d > "/dev/stderr"; bad = 1; exit 1 }
      seen[d] = 1
      print d
      count++
    }
    END {
      if (bad) exit 1
      if (count == 0) { printf "area-dirs: %s: no entries\n", FILENAME > "/dev/stderr"; exit 1 }
    }
  ' "$file")"; then
    return 1
  fi
  printf '%s\n' "$out"
}
