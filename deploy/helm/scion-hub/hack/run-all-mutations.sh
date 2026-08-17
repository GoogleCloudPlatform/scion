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

# THE ARM MUST BE A REPOSITORY, NOT A CHART DIRECTORY, AND THAT COST A WHOLE TABLE.
#
# This driver used to `cp -a "$SRC/." "$d/"` - the chart alone, into a bare
# mktemp dir - and run tests/run-all.sh from there. That stopped being a faithful
# copy of the subject at 7bb90dd9, when run-all.sh grew a dependency that reaches
# OUT of the chart:
#
#   run-all.sh:355  _contract_test="${HERE}/../../../../cmd/helm_chart_ha_contract_test.go"
#
# From a chart copied to /tmp/tmp.XXXX, that path resolves above /tmp and does not
# exist, so EVERY arm - including the clean one - picked up one extra
# meta-failure that had nothing to do with its mutation:
#
#   committed        re-derived under the old driver
#   MM0 exit 0 meta 0    ->   exit 2 meta 1     the CLEAN arm was red
#   MM6 exit 1 meta 0    ->   exit 2 meta 1     see below
#   every other row      ->   meta +1
#
# MM6 is the row that matters. It is the only arm that induces a REAL CHART
# FAULT, and its whole purpose is to show exit 1 (the chart is wrong) is
# distinguishable from exit 2 (the run is not evidence). The stray meta collapsed
# 1 into 2 and destroyed the distinction the row exists to demonstrate.
#
# So the arm is now a copy of the REPOSITORY with the chart inside it at its real
# path, which is what run-all.sh is written against. .git is excluded: it is 79M,
# nothing in the suite reads it, and verify-failopen.sh already falls back to the
# real repo for `git archive`, exactly as it did under the old driver.
#
# THE WORKING TREE IS STILL NEVER MUTATED. Everything under $d is a real copy,
# not a symlink - a symlinked cmd/ or go.mod would put a `go test` write one flag
# away from the developer's checkout, and this script's contract is that it edits
# only copies.
REPO_ROOT="$(cd "$SRC/../../.." && pwd)"
CHART_REL="${SRC#"$REPO_ROOT"/}"

for t in helm python3 mktemp cp go tar; do
  command -v "$t" >/dev/null 2>&1 || { echo "HARNESS ERROR: $t is not on PATH. NOTHING WAS MEASURED."; exit 2; }
done
[ -f "$SRC/tests/run-all.sh" ] || { echo "HARNESS ERROR: $SRC/tests/run-all.sh does not exist. NOTHING WAS MEASURED."; exit 2; }

# The mirror is only faithful if these are inside it. Asserted against the source
# tree BEFORE any arm runs, so a layout change is a harness error naming the file
# rather than ten rows quietly carrying an extra meta-failure.
[ "$CHART_REL" != "$SRC" ] || { echo "HARNESS ERROR: $SRC is not under $REPO_ROOT, so the chart's path inside the mirror cannot be derived. NOTHING WAS MEASURED."; exit 2; }
for f in go.mod cmd/helm_chart_ha_contract_test.go; do
  [ -e "$REPO_ROOT/$f" ] || { echo "HARNESS ERROR: $REPO_ROOT/$f is absent, so every arm would report its absence as a fact about the chart. NOTHING WAS MEASURED."; exit 2; }
done

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

# Path + content of every file in the chart copy. Sorted, so the comparison is
# order-independent, and content-keyed: a mutation that rewrites a file to the
# same bytes correctly counts as NO CHANGE, and a mutation that only adds or only
# deletes a file is caught by the path half. stderr is deliberately NOT
# suppressed - a manifest that could not read part of the tree must be loud,
# because its failure mode is "two identical manifests" and that reads as "the
# mutation did nothing", which is a meta-failure this driver is about to raise.
#
# NO EXCLUDE LIST. The driver's own scratch - the PATH farm and the two manifests
# - lives in $d and not in $chart, so there is nothing to filter, and a filter
# that never matches is a claim nothing tests. If that ever changes the file
# count below stops matching the source chart and the arm meta-fails by name.
_chart_manifest() { find "$1" -type f -exec sha256sum {} + | sort -k2; }

rows=0
mm0_rc=""; mm0_meta=""
row() { # row <label> <mutation function name>
  local label="$1" fn="$2"
  local d; d="$(mktemp -d)"
  # The mirror: the repository minus .git, with the chart at its real path.
  tar -C "$REPO_ROOT" --exclude=./.git -cf - . 2>/dev/null | tar -C "$d" -xf - || {
    echo "HARNESS ERROR: could not mirror $REPO_ROOT into $d for the $label arm. NOTHING WAS MEASURED."; exit 2; }
  local chart="$d/$CHART_REL"
  [ -f "$chart/tests/run-all.sh" ] || {
    echo "HARNESS ERROR: the $label arm's mirror has no $CHART_REL/tests/run-all.sh, so the copy is not the subject. NOTHING WAS MEASURED."; exit 2; }
  # A CONTROL MUST ASSERT THAT IT PERTURBED THE SUBJECT BEFORE IT REPORTS WHAT
  # THE SUBJECT DID. My rule, adopted project-wide by gd-em at 12:17, and it was
  # NOT IN THIS DRIVER until gd-p3-dev found the same hole in P3's copy of it:
  # two of their ten seds targeted literals that had since changed on disk, so
  # the mutations MATCHED NOTHING, the driver printed "10/10 rows produced" and
  # exited 0, and MM3's row came out byte-identical to the clean MM0 row but for
  # its label. A no-op mutation produces a plausible row, not a missing one.
  #
  # All six of this driver's sed targets do currently exist - I checked each
  # before writing this - so no row here is presently a no-op. That is a fact
  # about today's file contents and not a property of the driver, which is
  # exactly the argument I made about RQ-3, so it gets an assertion rather than
  # my word. The chart is small; a manifest is cheaper than a second copy.
  #
  # TWO ARMS HERE DO NOT TOUCH THE TREE AND THEY DECLARE IT: MM0 applies no
  # mutation at all (it is the fidelity control) and MM7 mutates PATH, not the
  # chart. Both set expect_change=0, and for those two the assertion runs in the
  # OTHER direction - an unexpected write is just as much a meta-failure as an
  # absent one. `expect_change=0` is therefore a claim, not an opt-out.
  local _pre="$d/.manifest-pre" _post="$d/.manifest-post"
  _chart_manifest "$chart" >"$_pre" || {
    echo "HARNESS ERROR: could not manifest the $label arm's chart copy before mutating it. NOTHING WAS MEASURED."; exit 2; }
  # EXTENT BEFORE DIFFERENCE. Two empty manifests compare equal, which would read
  # as "the mutation did nothing" for a mutating arm and, worse, as a clean pass
  # for MM0 and MM7. Pinned against an independently-derived expectation - the
  # source tree's own file count - and never against zero.
  local _pre_n _src_n
  _pre_n="$(wc -l <"$_pre")"
  _src_n="$(_chart_manifest "$SRC" | wc -l)"
  if [ "$_pre_n" -ne "$_src_n" ]; then
    echo "HARNESS ERROR: the $label arm's chart copy manifests $_pre_n files where the source chart has $_src_n. The copy is not the subject. NOTHING WAS MEASURED."; exit 2
  fi
  local strip_helm=0 expect_change=1
  "$fn" "$chart"    # a mutation may set strip_helm=1 and/or expect_change=0
  _chart_manifest "$chart" >"$_post" || {
    echo "HARNESS ERROR: could not manifest the $label arm's chart copy after mutating it. NOTHING WAS MEASURED."; exit 2; }
  local _changed=1; cmp -s "$_pre" "$_post" && _changed=0
  if [ "$_changed" -ne "$expect_change" ]; then
    if [ "$expect_change" = "1" ]; then
      echo "HARNESS ERROR: the $label mutation CHANGED NOTHING. Its target has probably moved or been renamed, so the row below would report a clean run wearing $label's label - which is the defect this table exists to detect. NOTHING WAS MEASURED."
    else
      # `diff`, NOT `comm`. comm requires both inputs sorted by the WHOLE line and
      # the manifest is sorted by PATH (-k2), so comm printed three "input is not
      # in sorted order" warnings to stderr and then dumped nearly the entire
      # manifest as the "difference" - eighteen paths to report one added file.
      # Measured on the control arm below before this line was written.
      local _delta
      _delta="$(diff "$_pre" "$_post" | sed -n "s|^[<>] [0-9a-f]* *${chart}/||p" | sort -u | tr '\n' ' ')"
      echo "HARNESS ERROR: the $label arm is supposed to leave the chart untouched and it modified: ${_delta}- NOTHING WAS MEASURED."
    fi
    exit 2
  fi
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
    # Positive: EVERY OTHER tool must still be reachable. Without this, an
    # over-broad PATH makes run-all.sh report git's or kubeconform's absence as
    # a fact about the chart - the exact defect this replaced.
    #
    # DERIVED AS A SET DIFFERENCE, NOT A HAND-WRITTEN LIST. This control used to
    # read `for _t in git kubeconform bash sed diff python3`. An anti-join
    # against the tools the suite actually invokes put 27 outside that list,
    # INCLUDING grep - the single most-invoked tool here, and the one whose
    # silent absence would turn every assertion in run-all.sh into a fake chart
    # failure, which is precisely what these two controls exist to prevent. A
    # hand-picked tool list is the same defect as a hand-transcribed flag
    # enumeration; it goes stale silently and its gaps are invisible.
    #
    # The farm's contents are fully determined: every executable basename on the
    # caller's absolute PATH entries, minus the dropped tool. So the honest
    # question is not "are these six present" but "does the farm differ from the
    # real PATH by exactly {helm}". That is complete by construction and cannot
    # go out of date when the suite starts using a new tool.
    _want="$d/.farm-want"; _have="$d/.farm-have"
    { oldifs="$IFS"; IFS=:; set -- $PATH; IFS="$oldifs"
      for _dir in "$@"; do
        case "$_dir" in /*) ;; *) continue ;; esac
        [ -d "$_dir" ] || continue
        for _f in "$_dir"/*; do [ -x "$_f" ] && printf '%s\n' "${_f##*/}"; done
      done; } | sort -u | grep -vx helm > "$_want"
    ls -1 "$farm" 2>/dev/null | sort -u > "$_have"
    # Extent before the difference, so an empty diff cannot come from an empty scan.
    if [ ! -s "$_want" ]; then
      echo "HARNESS ERROR: the expected-tool set for the $label arm is EMPTY, so the comparison below would pass on anything. NOTHING WAS MEASURED."; exit 2
    fi
    _absent="$(comm -23 "$_want" "$_have" | tr '\n' ' ' | sed 's/  *$//')"
    if [ -n "$_absent" ]; then
      echo "HARNESS ERROR: the $label arm should differ from the real PATH by exactly {helm}, but the farm is also missing: ${_absent}. Its result would be about those tools and not about helm. NOTHING WAS MEASURED."; exit 2
    fi
    out="$(PATH="$farm" bash "$chart/tests/run-all.sh" 2>&1)"; rc=$?
  else
    out="$(bash "$chart/tests/run-all.sh" 2>&1)"; rc=$?
  fi
  local summary s a m
  summary="$(printf '%s\n' "$out" | sed -n 's/^scripts: \(.*\)$/\1/p' | tail -1)"
  s="$(printf '%s' "$summary" | sed -n 's/^\([0-9]*\/[0-9]*\).*/\1/p')"
  a="$(printf '%s' "$summary" | sed -n 's/.*assertions: \([0-9]*\/[0-9]*\).*/\1/p')"
  m="$(printf '%s' "$summary" | sed -n 's/.*meta-failures: \([0-9]*\).*/\1/p')"
  # A GUARD ON AN EXTRACTOR MUST DEMAND A VALUE OF THE RIGHT SHAPE (gd-p2-dev's
  # rule; raised against this line by gd-p1-rev in round 6, and I agree with it).
  # These three used to print `${s:-?}` `${a:-?}` `${m:-?}`, so a summary line the
  # seds could not parse rendered as `?` IN THE TABLE, and the driver counted the
  # row and exited 0 anyway. That is MM0-printed-not-asserted one layer down: the
  # failure is visible only to a human who happens to look at the column. All ten
  # rows do produce a parseable summary today - including MM7, which reports
  # 0/4 0/127 because run-all.sh survives helm's absence - so demanding the shape
  # costs nothing and a `?` becomes an exit 2 instead of a character in a table.
  local _bad=""
  case "$s" in ''|*[!0-9/]*) _bad="scripts=${s:-<empty>}" ;; esac
  case "$a" in ''|*[!0-9/]*) _bad="$_bad assertions=${a:-<empty>}" ;; esac
  case "$m" in ''|*[!0-9]*)  _bad="$_bad meta=${m:-<empty>}" ;; esac
  if [ -n "$_bad" ]; then
    echo "HARNESS ERROR: the $label arm's summary line could not be parsed into the three numbers this table reports: ${_bad}. The summary read: '${summary:-<no scripts: line at all>}'. A row with an unparsed field is not a measurement, so it is not being printed as one. NOTHING WAS MEASURED."
    exit 2
  fi
  printf '#   %-6s %-38s exit %s  %-5s %-9s meta %s\n' \
    "$label" "$MM_DESC" "$rc" "$s" "$a" "$m"
  [ "$label" = "MM0" ] && { mm0_rc="$rc"; mm0_meta="${m:-?}"; }
  rows=$((rows + 1))
  rm -rf "$d"
}

MM_DESC=""
# MM0 IS THE FIDELITY CONTROL AND ITS "MUTATION" IS THE ABSENCE OF ONE. Declaring
# expect_change=0 does not exempt it from the applied-mutation control; it turns
# that control around, so an MM0 that DID modify the tree is a meta-failure too.
mm0() { expect_change=0; }
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
# MM7 perturbs PATH, not the chart, so the chart must come out byte-identical -
# and that is asserted rather than assumed. The PATH perturbation has its own two
# controls below (helm gone; every other tool still reachable).
mm7() { strip_helm=1; expect_change=0; }
mm8() { sed -i '/^echo "ASSERTIONS_EXECUTED=\${executed}"$/d' "$1/tests/update-strategy.sh"; }
mm9() { rm -f "$1/tests/verify-failopen.sh"; }

echo "# MEASURED $(date -u +%Y-%m-%dT%H:%M:%SZ) against $SRC"
# `helm version`, not `"$(command -v helm)" version`. gd-em's 12:22 correction:
# command -v answers IS THIS NAME CALLABLE, not WHERE IS THE BINARY, so for a
# function or alias it returns the bare word and the outer expansion then tries
# to execute it as a path. Cosmetic here - it is a provenance banner - but it is
# the same construct that aborted verify.sh's toolchain banner, so it goes.
echo "# helm $(helm version --short 2>/dev/null || echo unknown)"
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

# THE CLEAN ARM IS THE FIDELITY CONTROL, AND IT IS NOW ASSERTED RATHER THAN
# PRINTED. MM0 applies no mutation, so its only job is to prove the copied arm
# reproduces the subject. If it does not, every OTHER row is measuring the
# difference between its mutation AND the copy's own defects, and the table is
# worthless in a way that is invisible to a reader scanning nine plausible rows.
#
# This is not hypothetical. MM0 sat at exit 2 meta 1 through every run between
# 7bb90dd9 and this commit, printed in the output the whole time, and nobody read
# it - because a printed row is something a human has to notice and a failed
# assertion is something the exit code carries. Shipping the apparatus is
# necessary and was not sufficient; the driver has to grade its own baseline.
if [ "$mm0_rc" != "0" ] || [ "$mm0_meta" != "0" ]; then
  echo "HARNESS ERROR: the CLEAN arm MM0 returned exit ${mm0_rc:-?} with ${mm0_meta:-?} meta-failure(s), and a clean copy of the tree must return exit 0 with 0. The mutation arms below are therefore measuring their mutation PLUS whatever is wrong with the copy, so none of these rows describes the suite. Re-run tests/run-all.sh in the working tree: if it is green there, the defect is in this driver's mirror and not in the chart. NOTHING ABOVE IS A MEASUREMENT."
  exit 2
fi

# THE DENOMINATOR, ASSERTED. A driver that produced three rows and exited 0
# would let someone paste three rows into the table and call it re-measured.
if [ "$rows" -ne "$EXPECTED_ROWS" ]; then
  echo "HARNESS ERROR: produced ${rows} rows, committed to exactly ${EXPECTED_ROWS}."
  exit 2
fi
echo "# ${rows}/${EXPECTED_ROWS} rows produced."
