#!/usr/bin/env bash
#
# run-all-mutations.sh -- the driver behind the MM table in tests/run-all.sh.
#
# WHY IT IS IN THE REPOSITORY. The table was measured once, by a driver that
# lived in a scratch directory, and then the numbers in the table went stale the
# first time the assertion total moved - 107 in the comment, 127 on disk - with
# nothing able to notice, because the instrument had been thrown away. A
# measurement whose apparatus is not shipped degrades into a quotation. This
# file is that apparatus, and it re-derives every row rather than checking them.
#
# It COPIES the tree and edits the copies. It never mutates the working tree.
#
# Usage: hack/run-all-mutations.sh [chart-dir]
# exit 0 -- every row was produced, and the row count matches EXPECTED_ROWS
# exit 2 -- a required tool is absent, or fewer/more rows than committed
#
# It prints the table. It does not compare against the comment in run-all.sh:
# the comment is prose for a reader and the numbers there are updated by hand
# from this output, in the diff that moves them. What this script guarantees is
# that producing the numbers costs one command.
set -u -o pipefail

EXPECTED_ROWS=10
SRC="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

for t in helm python3 mktemp cp; do
  command -v "$t" >/dev/null 2>&1 || { echo "HARNESS ERROR: $t is not on PATH. NOTHING WAS MEASURED."; exit 2; }
done
[ -f "$SRC/tests/run-all.sh" ] || { echo "HARNESS ERROR: $SRC/tests/run-all.sh does not exist. NOTHING WAS MEASURED."; exit 2; }

# A PATH that lacks EXACTLY ONE TOOL and nothing else.
#
# MEASURED, AND IT WAS WRONG HERE. This row used to run the suite under
# PATH=/usr/bin:/bin and call the result "helm absent from PATH". On this box
# that PATH also removes git (/usr/local/bin/git) and kubeconform, and /bin is a
# symlink to usr/bin, so it is one directory written twice. PATH surgery cannot
# construct an absent-tool arm on such a box: there is no prefix of PATH that
# drops one tool and keeps the others. The row measured three absences and was
# labelled with one. See gke-deploy-lead's broadcast: AN INSTRUMENT THAT DOES
# NOT CHECK FOR ITS OWN TOOLS REPORTS THEIR ABSENCE AS A FACT ABOUT THE SUBJECT.
#
# A symlink farm can. Every executable reachable from the caller's PATH is
# linked into one directory by ABSOLUTE path - not by `command -v` output, which
# resolves against the PATH being mutated - except the one being removed.
path_farm_without() { # path_farm_without <toolname> <destdir>
  local drop="$1" dest="$2" dir f base
  mkdir -p "$dest" || return 1
  local oldifs="$IFS"; IFS=:
  set -- $PATH
  IFS="$oldifs"
  for dir in "$@"; do
    case "$dir" in /*) ;; *) continue ;; esac   # relative PATH entries resolve elsewhere
    [ -d "$dir" ] || continue
    for f in "$dir"/*; do
      [ -x "$f" ] || continue
      base="${f##*/}"
      [ "$base" = "$drop" ] && continue
      [ -e "$dest/$base" ] && continue          # first match wins, as PATH order does
      ln -s "$f" "$dest/$base" 2>/dev/null
    done
  done
}

rows=0
row() { # row <label> <mutation function name>
  local label="$1" fn="$2"
  local d; d="$(mktemp -d)"
  cp -a "$SRC/." "$d/"
  local strip_helm=0
  "$fn" "$d"    # a mutation may set strip_helm=1
  local out rc
  if [ "$strip_helm" = "1" ]; then
    local farm="$d/.pathfarm"
    path_farm_without helm "$farm" || {
      echo "HARNESS ERROR: could not build the single-tool-absent PATH. NOTHING WAS MEASURED."; exit 2; }
    # TWO CONTROLS, because either one alone passes for the wrong arm.
    # Negative: the tool under test must really be gone, or the row measures a
    # clean run wearing MM7's label.
    if ( PATH="$farm"; hash -r 2>/dev/null; command -v helm >/dev/null 2>&1 ); then
      echo "HARNESS ERROR: the $label arm still resolves helm, so it is not the arm it claims to be. NOTHING WAS MEASURED."; exit 2
    fi
    # Positive: EVERY OTHER tool the suite needs must still run. Without this,
    # an over-broad PATH makes run-all.sh report git's or kubeconform's absence
    # as a fact about the chart - the exact defect this replaced.
    for _t in git kubeconform bash sed diff python3; do
      ( PATH="$farm"; hash -r 2>/dev/null; command -v "$_t" >/dev/null 2>&1 ) || {
        echo "HARNESS ERROR: the $label arm also removed ${_t}, so its result would be about ${_t} and not about helm. NOTHING WAS MEASURED."; exit 2; }
    done
    out="$(PATH="$farm" bash "$d/tests/run-all.sh" 2>&1)"; rc=$?
  else
    out="$(bash "$d/tests/run-all.sh" 2>&1)"; rc=$?
  fi
  local summary s a m
  summary="$(printf '%s\n' "$out" | sed -n 's/^scripts: \(.*\)$/\1/p' | tail -1)"
  s="$(printf '%s' "$summary" | sed -n 's/^\([0-9]*\/[0-9]*\).*/\1/p')"
  a="$(printf '%s' "$summary" | sed -n 's/.*assertions: \([0-9]*\/[0-9]*\).*/\1/p')"
  m="$(printf '%s' "$summary" | sed -n 's/.*meta-failures: \([0-9]*\).*/\1/p')"
  printf '#   %-6s %-38s exit %s  %-5s %-9s meta %s\n' \
    "$label" "$MM_DESC" "$rc" "${s:-?}" "${a:-?}" "${m:-?}"
  rows=$((rows + 1))
  rm -rf "$d"
}

MM_DESC=""
mm0() { :; }
mm1() { printf '#!/usr/bin/env bash\nexit 0\n' >"$1/tests/zz-unenumerated.sh"; }
mm2() { sed -i 's/^EXPECTED_SCRIPTS=4$/EXPECTED_SCRIPTS=5/' "$1/tests/run-all.sh"; }
mm3() { sed -i 's/^EXPECTED_ASSERTIONS=127/EXPECTED_ASSERTIONS=128/' "$1/tests/run-all.sh"; }
mm4() { rm -f "$1/tests/update-strategy.sh"; }
# MM5: drop assertions AND lower that script's own total, which is green
# everywhere except against run-all.sh's duplicate of the number.
mm5() {
  sed -i '/^reject "--gh-pat"/d' "$1/tests/render-guards.sh"
  sed -i 's/^EXPECTED_TOTAL=57$/EXPECTED_TOTAL=56/' "$1/tests/render-guards.sh"
}
# MM6: a real chart failure, induced in the chart rather than in the harness.
mm6() { sed -i 's/runAsUser may not be 0/runAsUser must not be 0/' "$1/templates/_helpers.tpl"; }
mm7() { strip_helm=1; }
mm8() { sed -i '/^echo "ASSERTIONS_EXECUTED=\${executed}"$/d' "$1/tests/update-strategy.sh"; }
mm9() { rm -f "$1/tests/verify-failopen.sh"; }

echo "# MEASURED $(date -u +%Y-%m-%dT%H:%M:%SZ) against $SRC"
echo "# helm $("$(command -v helm)" version --short 2>/dev/null || echo unknown)"
MM_DESC="clean";                                 row MM0 mm0
MM_DESC="unenumerated script on disk";           row MM1 mm1
MM_DESC="EXPECTED_SCRIPTS=5";                    row MM2 mm2
MM_DESC="EXPECTED_ASSERTIONS=128";               row MM3 mm3
MM_DESC="enumerated script missing";             row MM4 mm4
MM_DESC="assertion dropped + own total lowered"; row MM5 mm5
MM_DESC="a real assertion failure";              row MM6 mm6
MM_DESC="helm absent from PATH";                 row MM7 mm7
MM_DESC="a script emits no count line";          row MM8 mm8
MM_DESC="named exception missing from disk";     row MM9 mm9

# THE DENOMINATOR, ASSERTED. A driver that produced three rows and exited 0
# would let someone paste three rows into the table and call it re-measured.
if [ "$rows" -ne "$EXPECTED_ROWS" ]; then
  echo "HARNESS ERROR: produced ${rows} rows, committed to exactly ${EXPECTED_ROWS}."
  exit 2
fi
echo "# ${rows}/${EXPECTED_ROWS} rows produced."
