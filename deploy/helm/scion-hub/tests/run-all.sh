#!/usr/bin/env bash
#
# run-all.sh -- the meta-check. IT EXISTS BECAUSE RULE 9 DOES NOT COMPOSE.
#
# Each script beside this one fails closed on its own execution: it counts the
# assertions it ran and refuses to report success on a short run. That contract
# is airtight and it is also silent about a script that was never invoked. A
# script's fail-closed guarantee is a claim about ITS OWN run; none of them can
# make a claim about a run that did not happen. So a set of individually
# non-vacuous checks is vacuous AT THE SET LEVEL, and the stronger each script's
# internal contract is, the more confident a runner is that green means covered.
#
# That is not hypothetical. The same defect exists one repo directory over -
# several hack/check-*.sh scripts with nothing asserting they are all wired - and
# it was reproduced HERE, inside the remedy for the finding it came from, within
# an hour, by three authors each applying the fail-closed rule correctly and
# individually. This file is the generalisation: WHERE A CHECK COUNTS ANYTHING,
# COMMIT THE NUMBER AND FAIL ON INEQUALITY - AND THAT INCLUDES COUNTING THE
# CHECKS.
#
# FOUR COUNTS, COMMITTED, EACH FAILING IN BOTH DIRECTIONS:
#
#   EXPECTED_SCRIPTS     how many scripts must run. Adding a fifth fails here
#                        until someone bumps this number in a diff.
#   the on-disk set      every FILE beside this one, at any depth, must be
#                        enumerated below - either as a script to run or as a
#                        named exception - and every name below must exist. This
#                        is the half that catches the original defect: a script
#                        added to the directory but never wired in.
#   EXPECTED_FILES       how many files this directory holds. The set comparison
#                        alone is defeated by one file added as another is
#                        dropped; this is not.
#   EXPECTED_ASSERTIONS  the sum across all scripts. This duplicates each
#                        script's own EXPECTED_TOTAL on purpose. Without it,
#                        deleting assertions AND lowering that script's total is
#                        green everywhere - the breach produces no symptom that
#                        points at the number.
#
# 🔴 [HISTORY 2026-08-17] WHAT EXPECTED_ASSERTIONS DOES NOT TELL YOU, STATED
# HERE BECAUSE IT USED TO AND NO LONGER DOES. The total counts assertions EXECUTED, not assertions that
# meant anything. Those differ: an assertion that shells out to a missing binary
# and reads the non-zero exit as proof still executes. [HISTORY 2026-08-17,
# NUMERALS ARE PERIOD-ACCURATE AND NOT CURRENT: this file now carries 127
# assertions, not 106] Before the tool-presence
# arms below, a run with no helm reported 0/106 - visibly alarming, and load-
# bearing by accident, because the number was a pass-count masquerading as a
# coverage-count. Counting executions is the correct meaning for rule 9, and it
# converts that alarm into a serene 106/106. THE SIGNAL DID NOT SURVIVE IN THIS
# NUMBER; IT MOVED. It is now carried by the tool-presence pre-flights (here and
# in each script) and by every assertion matching the REASON for a refusal
# rather than its exit status. Do not weaken either one on the theory that the
# count would catch it. It would not.
#
# MUTATION TABLE. Every row EXECUTED, none inferred, and every row asserts the
# WHOLE summary line - exit code AND scripts AND assertions AND meta-failures -
# not the field the mutation was aimed at. That rule is not stylistic. The first
# version of this file [HISTORY 2026-08-17] shipped with the count check short-circuited once any
# script failed, and MM6 - the mutation that triggers exactly that condition -
# had already printed "assertions: 60/106" on a run recorded as green - a
# PERIOD-ACCURATE numeral, not a current one; the denominator is 127 today -
# because
# what was asserted about MM6 was its exit code. THE DEFECT WAS IN THE SUITE'S
# OWN OUTPUT AT THE MOMENT IT WAS DECLARED PASSING. A mutation that checks one
# field of a multi-field output has tested one field; the rest is decoration you
# have trained yourself to skim.
#
# RE-DERIVE, DO NOT EDIT: bash hack/run-all-mutations.sh. The rows below were
# printed by that script and pasted; it asserts it produced exactly ten of them.
# The numbers here were 107 for a while after the total moved to 127, because
# the original driver lived in a scratch directory and was thrown away - a
# measurement whose apparatus is not shipped decays into a quotation, and this
# table is where that was demonstrated rather than argued.
#
#   MEASURED 2026-08-17T08:53:04Z, helm v3.16.3+gcfd0749
#   MM0    clean                                  exit 0  4/4   127/127   meta 0
#   MM1    unenumerated script on disk            exit 2  4/4   127/127   meta 2
#   MM2    EXPECTED_SCRIPTS=5                     exit 2  4/5   127/127   meta 2
#   MM3    EXPECTED_ASSERTIONS=128                exit 2  4/4   127/128   meta 1
#   MM4    enumerated script missing              exit 2  3/4   123/127   meta 4
#   MM5    assertion dropped + own total lowered  exit 2  4/4   126/127   meta 1
#   MM6    a real assertion failure               exit 1  4/4   127/127   meta 0
#   MM7    helm absent from PATH                  exit 2  0/4   0/127     meta 1
#   MM8    a script emits no count line           exit 2  4/4   123/127   meta 2
#   MM9    named exception missing from disk      exit 2  4/4   127/127   meta 2
#
# MM5 is why EXPECTED_ASSERTIONS duplicates each script's own total: the mutated
# script alone reports PASS 3/3 and exits 0. MM6 and MM7 are the pair that keeps
# 1 and 2 distinct - "the chart is broken" and "the checks did not run" must not
# collapse into each other, and MM7 is the direction that collapses toward the
# alarming and wholly wrong reading. MM8 is the one that would catch a future
# script that forgot the count line, since "unknown" must not be read as zero.
# MM9 covers the named exceptions: an exception that stops existing must not
# quietly become an exception to nothing.
#
# MM4 is meta 4, not meta 2, and the extra two are worth reading rather than
# rounding off: removing one script trips the missing-file check, the
# unenumerated-set check, EXPECTED_FILES and the assertion total. FOUR
# INDEPENDENT COUNTS FIRING ON ONE MUTATION IS THE INTENDED BEHAVIOUR, not
# double-reporting - each is answering a different question about the same
# breach, and a fix that silenced three of them would leave the fourth as the
# only witness.
#
# The mutation driver is not in this directory: it copies the tree and edits the
# copies, so it is not an assertion script and enumerating it here would be a
# category error. It is hack/run-all-mutations.sh - IN THE REPOSITORY, which it
# was not, and its absence is reported by the hack/ gate near the bottom of this
# file rather than left to be noticed.
#
# NO CI WIRING, deliberately, same as the scripts it runs. Phase 6 owns that.
#
# CONTRACT:
#   exit 0 -- every script ran, every assertion passed, all three counts agree
#   exit 1 -- a script reported a real assertion failure
#   exit 2 -- a meta-failure: a script was missing, unenumerated, un-run, or a
#             count disagreed. Distinct from 1 because "the checks did not all
#             run" and "the chart is broken" need different reactions.
set -u -o pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

EXPECTED_SCRIPTS=5
EXPECTED_ASSERTIONS=179   # 35 chart-integrity + 63 render-guards + 31 reserved-flags + 4 update-strategy + 46 rbac-collision.
EXPECTED_FILES=7        # SCRIPTS + NOT_RUN_HERE + NOT_EXECUTABLE + this file.
# 🛑 [HISTORY 2026-08-17] EXPECTED_FILES SHIPPED WRONG FOR AN HOUR BECAUSE A
# CONFLICT RESOLUTION TOOK BOTH LINES AS A UNIT. The rebase onto main deleted
# tests/stale-claim-triage.md; NOT_EXECUTABLE was emptied to match, so the
# set-difference check went on passing clean, and only the COUNT was left
# stale at 7. The two constants above came out of the same conflict hunk and
# answer to different facts - one was right and one was not, and taking a
# hunk takes both without being asked which. Both are re-derived here:
#   ls -1 deploy/helm/scion-hub/tests | wc -l                          -> 6
#   for s in chart-integrity render-guards reserved-flags update-strategy; \
#     do bash tests/$s.sh; done, reading each script's own total       -> 35 61 31 4
# I told two agents that EXPECTED_ASSERTIONS was certain to move and it did
# not; the pin that had silently moved was the other one on the same line.

# Enumerated by name, not globbed into a loop. A glob would run whatever is
# present and could never notice that something is absent.
SCRIPTS=(
  reserved-flags.sh     # 31 - the reserved-flag lists
  update-strategy.sh    #  4 - the updateStrategy derivation
  render-guards.sh      # 57 - every other render-time refusal, incl. the HA-unlanded gate
  chart-integrity.sh    # 35 - .helmignore breadth, the packaged file set, base-url, signing key
  rbac-collision.sh     # 46 - the cluster-scoped RBAC name, and two pinned residuals
)

# 🛑 rbac-collision.sh IS NOT AN "ALL GREEN MEANS ALL FIXED" SCRIPT, AND THE LINE
# THAT WIRES IT IN IS THE WORST PLACE TO LEAVE THAT UNSAID. Several of its
# forty-six assertions PIN DEFECTS THAT ARE STILL PRESENT - arm (o) for C4 and
# arm (q) for C5, two live Criticals this change does NOT fix. They are written
# to PASS WHILE THE DEFECT EXISTS and to go RED when it is fixed.
#
# So a green run of that script means "the chart names objects as measured", NOT
# "the cluster-scoped naming is sound". Whoever fixes C4 or C5 will see red here
# and must DELETE the corresponding arm in that same diff, not repair it.
#
# This note is duplicated from that script's own header on purpose. A reader who
# trusts the single summary line at the bottom of this file is exactly the reader
# who will never open the header, and the summary line cannot carry a caveat.

# NAMED EXCEPTIONS. Present in this directory, deliberately NOT run from here and
# deliberately NOT part of EXPECTED_ASSERTIONS. Stated as a list rather than left
# to fall out of a pattern: an unstated exclusion is how a file ends up in a
# directory that everything claims to cover and nothing runs.
NOT_RUN_HERE=(
  # verify-failopen.sh INVOKES this script - twice, once with a stripped PATH -
  # so running it from here would recurse. It is the verifier for this runner's
  # own fail-closed behaviour and its subject is therefore the runner, not the
  # chart; its steps are not chart assertions and must not be summed into the
  # chart assertion total. Run it directly: bash tests/verify-failopen.sh <sha>.
  verify-failopen.sh
)

# NOT A SCRIPT AT ALL. Enumerated for the same reason as NOT_RUN_HERE: the file
# scan below counts EVERY file in this directory, so anything unlisted is
# reported as un-covered. Listing it here is the statement that it is data, not
# an executable, and that no assertion total is expected from it.
NOT_EXECUTABLE=(
)

REQUIRED_TOOLS=("${HELM:-helm}" tar mktemp awk sha256sum)

meta_fail=0
note() { echo "META  $*"; meta_fail=$((meta_fail + 1)); }

# ONE SUMMARY EMITTER, USED BY EVERY EXIT PATH INCLUDING THE EARLY ONES. Four
# fields on one line, always all four, because a mutation that checks the field
# it was aimed at has tested one field and skimmed the rest. Every exit from
# this script prints scripts, assertions and meta-failures, so no outcome can be
# reported by a subset of the numbers that describe it.
#
# 🔴 [HISTORY 2026-08-17] AND UNTIL TODAY IT DID NOT PRINT real_failure, WHICH IS
# THE ONE FIELD THAT SEPARATES "NOTHING RAN" FROM "SOMETHING WENT RED". The
# paragraph above is the standard this line was failing: a run with a stale
# gate list and a wrong EXPECTED_FILES printed
#
#     scripts: 4/4   assertions: 127/127   meta-failures: 1
#
# and I read my own summary and reported ONE defect to two agents, because the
# second one had no field to appear in. gd-p1-rev found it by planting
# EXPECTED_FILES=6 and watching rc go 2 -> 1 with the gate walk still red. TWO
# INDEPENDENT DEFECTS, TWO DIFFERENT EXIT CODES, ONE VISIBLE NUMBER.
summary() {  # <ran> <assertions>
  echo
  echo "================================================================"
  echo "scripts: ${1}/${EXPECTED_SCRIPTS}   assertions: ${2}/${EXPECTED_ASSERTIONS}   meta-failures: ${meta_fail}   scripts-red: ${real_failure:-0}"
}

# --- pre-flight: the toolchain ------------------------------------------------
# THE HARNESS MUST NOT BLAME THE CHART FOR ITS OWN MISSING TOOLS. Run without
# helm, this suite [HISTORY 2026-08-17] previously reported "render is MISSING kind: Service" and
# "package is MISSING scion-hub/Chart.yaml" - every one a false accusation - and
# exited 1, the code reserved for "the chart is broken". Checked here as well as
# inside each script so that the set-level run says it once and clearly, and so
# a script invoked on its own still fails closed.
missing=""
for t in "${REQUIRED_TOOLS[@]}"; do
  command -v "$t" >/dev/null 2>&1 || missing="${missing} ${t}"
done
if [ -n "$missing" ]; then
  # note(), NOT a bare echo, and the SAME summary line as every other exit path.
  # [HISTORY 2026-08-17] The first version of this arm printed its own message and exited without a
  # summary, so the catastrophic-shortfall run reported no meta-failure count at
  # all - reproducing, on the repair, the precise surface being repaired:
  # a total of 0 sitting beside a meta-failure count that does not say 1.
  note "required tool(s) not on PATH:${missing}"
  summary 0 0
  echo "NOTHING WAS ANALYSED. This is NOT a passing run, and it is NOT a chart"
  echo "failure - without these tools the harness can make no claim about the"
  echo "chart either way. Install them, or read this as 'unknown', never 'clean'."
  exit 2
fi

# --- count check 1: the enumeration matches its committed size ---------------
if [ "${#SCRIPTS[@]}" -ne "$EXPECTED_SCRIPTS" ]; then
  note "SCRIPTS lists ${#SCRIPTS[@]} entries, EXPECTED_SCRIPTS is ${EXPECTED_SCRIPTS}. Bump the number in the same diff that changes the list."
fi

# --- count check 2: the enumeration matches the directory --------------------
# Both directions. Missing catches a stale entry; unenumerated catches the
# defect this file was written for.
#
# EVERY REGULAR FILE AT ANY DEPTH, NOT `ls -1 *.sh`. The previous version defined
# "present" with a glob at depth 1, so it caught an unenumerated `orphan.sh` and
# was blind to `orphan.bash`, an extensionless `orphan`, and
# `helpers/orphan.sh` - three of the four naming shapes. This file's own argument
# eight lines up is that "a glob would run whatever is present and could never
# notice that something is absent", and the half meant to enforce it was itself a
# glob, enforcing a `.sh` convention that is stated nowhere. Committing the
# convention in prose and asserting it would have been the weaker fix: a
# convention asserted in prose is the thing this entire round was about.
#
# So the rule is now positive and total: every file here is either run and
# counted (SCRIPTS), or named as a deliberate exception (NOT_RUN_HERE), or this
# runner. A file in none of those three fails the run whatever it is called and
# wherever it sits. That includes a stray editor backup or a downloaded binary,
# and that is intended - the answer is one line in a list, and the alternative is
# a directory nobody can make a statement about.
#
# NOT_EXECUTABLE is currently empty, and the +/- expansions below are a
# prophylactic for that, not a fix for an observed failure: on the bash this
# suite has been run against (5.2.15) an empty array under `set -u` expands
# without error at both sites, and that was measured. Older bash is reported to
# treat it as unbound; that is documentation, not something measured here, and
# no interpreter available to this project can exhibit it. The guarded forms are
# output-identical to the unguarded ones whether the array is empty or not, so
# they cost nothing and remove the question.
for s in "${SCRIPTS[@]}" "${NOT_RUN_HERE[@]}" ${NOT_EXECUTABLE[@]+"${NOT_EXECUTABLE[@]}"}; do
  [ -f "${HERE}/${s}" ] || note "enumerated but not present on disk: ${s}"
done
known=" ${SCRIPTS[*]} ${NOT_RUN_HERE[*]} ${NOT_EXECUTABLE[*]-} run-all.sh "
on_disk=0
while IFS= read -r found; do
  on_disk=$((on_disk + 1))
  case "$known" in
    *" ${found} "*) ;;
    *) note "present on disk but NOT ENUMERATED, so nothing here makes any claim about it: ${found}. Add it to SCRIPTS (run and counted) or to NOT_RUN_HERE (a named exception), and update EXPECTED_SCRIPTS/EXPECTED_FILES to match." ;;
  esac
done < <(cd "$HERE" && find . -type f | sed 's|^\./||' | sort)

# --- count check 2b: the directory is the size we committed -------------------
# Distinct from 2. The set comparison above is defeated by one file added as
# another is dropped; this is not.
[ "$on_disk" -eq "$EXPECTED_FILES" ] || note "this directory holds ${on_disk} files, EXPECTED_FILES is ${EXPECTED_FILES}."

# --- run them ----------------------------------------------------------------
ran=0
total_assertions=0
real_failure=0

for s in "${SCRIPTS[@]}"; do
  [ -f "${HERE}/${s}" ] || continue
  echo
  echo "################ ${s} ################"
  # Invoked through bash rather than executed, because helm package does not
  # preserve the executable bit and a chmod lost in a tarball must not silently
  # skip a script.
  out="$(bash "${HERE}/${s}" 2>&1)"; rc=$?
  printf '%s\n' "$out"
  ran=$((ran + 1))

  # SUMMED FROM A LINE EVERY SCRIPT EMITS ON EVERY EXIT PATH, NOT FROM ITS PASS
  # LINE. Parsing "PASS n/m" only counted scripts that succeeded, so the moment
  # anything failed the total collapsed and the inequality below stopped
  # applying - the count check went quiet exactly when it was most needed. A run
  # reporting 0 of 106 assertions with zero meta-failures is the shape that bug
  # produced, and it is the shape this line prevents.
  n="$(printf '%s\n' "$out" | sed -n 's|^ASSERTIONS_EXECUTED=\([0-9]*\)$|\1|p' | tail -1)"
  if [ -z "$n" ]; then
    note "${s} emitted no ASSERTIONS_EXECUTED line, so what it ran cannot be counted. Treated as a meta-failure rather than as zero, because 'unknown' and 'none' are different."
  else
    total_assertions=$((total_assertions + n))
  fi

  case "$rc" in
    0) ;;
    1) echo ">>> ${s}: ASSERTION FAILURE (exit 1)"; real_failure=1 ;;
    2) note "${s} exited 2: it did not run its full set, or its toolchain was incomplete." ;;
    *) note "${s} exited ${rc}, which is not part of the contract." ;;
  esac
done

# --- count check 3: every script actually ran --------------------------------
[ "$ran" -eq "$EXPECTED_SCRIPTS" ] || note "ran ${ran} scripts, expected exactly ${EXPECTED_SCRIPTS}."

# --- gate 5: hack/verify.sh, WHICH THIS FILE USED TO OMIT ----------------------
# 🔴 [HISTORY 2026-08-17] AND THE OMISSION SHIPPED. Commit eec9df03 landed on the work branch with
# five of the five golden files stale, because their content changed and nothing
# in this run compares them. I ran run-all.sh, read PASS, and pushed. The goldens
# are hack/verify.sh's business and hack/ is not tests/, so the set this runner
# guards stopped at the directory it lives in.
#
# THAT IS THIS FILE'S OWN THESIS, TURNED AROUND ON IT. The header above says a
# set of individually non-vacuous checks is vacuous at the set level, and then
# bounds the set at the directory the author happened to be standing in - which
# is gd-em's rule 46, in the file written to prevent exactly this class. The
# stronger this runner's internal contract got, the more confidently its PASS
# line was read as "the chart is verified", which is a claim it was never making.
#
# NOT SUMMED INTO EXPECTED_ASSERTIONS, and that is deliberate rather than
# convenient: EXPECTED_ASSERTIONS is cross-checked against each script's own
# EXPECTED_TOTAL, and hack/verify.sh is not one of the enumerated scripts. It
# gets its own committed number, failing in both directions like the others.
# Phase 6 owns CI wiring and may move this; it must not delete it without
# replacing the coverage.
EXPECTED_VERIFY_ASSERTIONS=278
# hack/ IS OUTSIDE THIS DIRECTORY, SO THE FILE SCAN BELOW CANNOT SEE IT, AND
# NAMING ITS CONTENTS HERE IS THE ONLY THING THAT MAKES THEM DISCOVERABLE. Two
# files, both stated: verify.sh, gated below, and run-all-mutations.sh, which is
# the driver that produces the MM table at the top of this file. That driver is
# not run from here - it copies this tree ten times and each copy runs this
# script, so it would recurse and it takes minutes - but its ABSENCE is reported,
# because the MM table was already stale once and the reason was that its
# apparatus lived in a scratch directory and was thrown away.
_mm_driver="${HERE}/../hack/run-all-mutations.sh"
[ -f "$_mm_driver" ] || note "hack/run-all-mutations.sh is missing: the MM table at the top of this file can no longer be re-derived, so its numbers are now quotations."
_verify="${HERE}/../hack/verify.sh"
if [ ! -f "$_verify" ]; then
  note "hack/verify.sh is missing, so the golden files and the settings-leaf probe were not checked at all."
else
  _vout="$(bash "$_verify" 2>&1)"; _vrc=$?
  _vn="$(printf '%s\n' "$_vout" | sed -n 's/^[[:space:]]*assertions: \([0-9]*\)\/.*/\1/p' | tail -1)"
  if [ -z "$_vn" ]; then
    note "hack/verify.sh emitted no assertion count, so its ${EXPECTED_VERIFY_ASSERTIONS} checks cannot be shown to have run."
  elif [ "$_vn" -ne "$EXPECTED_VERIFY_ASSERTIONS" ]; then
    note "hack/verify.sh executed ${_vn} assertions, expected exactly ${EXPECTED_VERIFY_ASSERTIONS}. Update this number and its EXPECTED_TOTAL in the same diff."
  fi
  if [ "$_vrc" -ne 0 ]; then
    echo ">>> hack/verify.sh: FAILED (exit ${_vrc})"
    printf '%s\n' "$_vout" | grep -E '^[[:space:]]*(FAIL|META-FAILURE)' | sed 's/^/    /'
    real_failure=1
  else
    echo ">>> hack/verify.sh: ok (${_vn} assertions, goldens current)"
  fi
fi

# --- the derivation this suite's gate list depends on -------------------------
# cmd/helm_chart_ha_contract_test.go IS OUTSIDE THIS DIRECTORY AND OUTSIDE THE
# CHART, so neither the file scan above nor hack/verify.sh's chart-tree walk can
# see it, and naming it here is the only thing that makes it discoverable.
#
# WHY IT MATTERS MORE THAN THE OTHER OUTSIDE FILES. render-guards.sh and
# verify.sh no longer carry the HA gate list; they read it out of
# hack/ha-gates.txt, which that Go test produces. If the test were deleted, the
# artifact would sit there unchanged and every assertion built on it would stay
# green forever - a derived enumeration whose producer is gone is worse than the
# literal it replaced, because it still looks derived. So the producer is
# enumerated, its test functions are counted in both directions, and it is RUN.
#
# A MISSING go IS A META-FAILURE, NOT A SKIP. tests/ and hack/ are both
# .helmignored, so this suite only ever runs against a repository checkout, and a
# checkout of this repository has Go. "go not found" here means the run cannot
# make the claim, which is the definition of a meta-failure in this harness.
# CONTROLS RUN AGAINST THIS SECTION on 2026-08-17, all four red:
#
#   move the test file away          -> META, "it is what DERIVES ..."
#   add a third func Test            -> META, 3 declared vs EXPECTED 2
#   drift a gate name in the artifact-> the Go test FAILS here, and verify.sh too
#   move the artifact away           -> verify.sh META exit 2, 4 meta-failures
EXPECTED_CONTRACT_TESTS=2   # TestHelmChartHAGateWalk + TestHelmChartSigningKeyContract.
_contract_test="${HERE}/../../../../cmd/helm_chart_ha_contract_test.go"
_gates_artifact="${HERE}/../hack/ha-gates.txt"
if [ ! -f "$_contract_test" ]; then
  note "cmd/helm_chart_ha_contract_test.go is missing. It is what DERIVES hack/ha-gates.txt, and the HA gate assertions in render-guards.sh and hack/verify.sh read that artifact - so without it they check a frozen file against itself and cannot go red. Restore it, or delete the artifact and put the gate list back under a guard that can fail."
elif [ ! -f "$_gates_artifact" ]; then
  note "hack/ha-gates.txt is missing though its producer is present. Regenerate: go test ./cmd -run TestHelmChartHAGateWalk -update-chart-contract"
else
  _ct="$(grep -cE '^func Test' "$_contract_test")"
  if [ "$_ct" -ne "$EXPECTED_CONTRACT_TESTS" ]; then
    note "cmd/helm_chart_ha_contract_test.go declares ${_ct} test functions, EXPECTED_CONTRACT_TESTS is ${EXPECTED_CONTRACT_TESTS}. Bump the number in the same diff that adds or removes one, so that neither direction is silent."
  fi
  if ! command -v go >/dev/null 2>&1; then
    note "go is not on PATH, so the derivation behind hack/ha-gates.txt was not re-run and the artifact this suite reads is unverified."
  else
    # From the repository root, because ./cmd is a Go package path and not a
    # file path. -count=1 defeats the test cache: a cached PASS is a statement
    # about an earlier tree.
    _gout="$( (cd "${HERE}/../../../.." && go test ./cmd -run TestHelmChart -count=1) 2>&1 )"; _grc=$?
    # A GO RUN THAT NEVER REACHED A TEST IS AN INSTRUMENT FAULT, NOT A CHART
    # FAULT - the symmetric twin of the kubeconform-registry probe in verify.sh,
    # and filed by gd-p1-rev (round 4, item 6) after walking into it: they
    # extracted deploy/ and cmd/ without go.mod and got `FAILED (exit 1)` with
    # no reason at all, because the filter below matches none of the lines the
    # toolchain emits when the package cannot be built.
    #
    # MEASURED, three arms, [HISTORY 2026-08-17] all three previously reported as "the chart is wrong":
    #   gate name drifted, a genuine test failure  -> --- FAIL, --- committed,
    #                                                 --- derived now. 🔴 [HISTORY
    #                                                 2026-08-17] THIS ROW WAS
    #                                                 LABELLED "POSITIVE CONTROL:
    #                                                 the filter works for the
    #                                                 case it was written for."
    #                                                 IT WAS FALSE. See below.
    #   syntax error planted                       -> [setup failed]; the
    #                                                 compiler's own
    #                                                 `cmd/...go:564:16: expected
    #                                                 ')'` line IS DROPPED
    #   no go.mod in the tree                      -> ZERO lines survive
    #
    # The empty-filter limb is the one that catches the third arm, and it is why
    # the test is on the FILTER'S OUTPUT rather than on a list of toolchain
    # phrases: a build failure this list does not enumerate still produces no
    # diagnostic, and a hand-listed set of compiler messages is the enumeration
    # that quietly stops being complete.
    #
    # 🛑 [HISTORY 2026-08-17] AND THAT SENTENCE CONDEMNED THE FILTER ONE LINE
    # BELOW IT, WHICH NOBODY NOTICED FOR A DAY. `^ *---|^(ok|FAIL)` IS a
    # hand-listed enumeration that quietly stopped being complete - and it
    # stopped being complete for the ASSERTION-FAILURE limb, the one limb that
    # carries every real finding this test will ever produce. gd-p1-rev ran the
    # case for real on 2026-08-17 and measured it: of 342 lines, SIX survived.
    # What the operator saw was
    #
    #     --- FAIL: TestHelmChartHAGateWalk
    #             --- committed
    #             --- derived now
    #
    # two section headers with nothing between them, which reads as "the two
    # sides agree" - the exact opposite of what happened. Eaten by name: the
    # test's own sentence "ha-gates.txt is stale. The hub's preflight no longer
    # produces the committed gate list", both TERMINATED lines carrying the gate
    # counts, and all four "===== settings-oauth.yaml" arm headers.
    #
    # 🛑 THE CONTROL ABOVE ASSERTED THAT THE THREE MARKERS SURVIVED. IT NEVER
    # ASSERTED THAT ANYTHING SURVIVED BETWEEN THEM. A control that checks the
    # frame arrived has not checked that the contents did, and it passes loudest
    # in exactly the case the filter destroys.
    #
    # So all three limbs now print $_gout WHOLE. An allow-list that stops being
    # complete prints nothing; that is the failure mode that got us here, and
    # there is no reason this limb should be the one that keeps it.
    _gfiltered="$(printf '%s\n' "$_gout" | grep -E 'HARNESS ERROR|VACUOUS|^ *---|^(ok|FAIL)')" || true
    if [ "$_grc" -ne 0 ] && { printf '%s\n' "$_gout" | grep -qE '\[setup failed\]|\[build failed\]|^go: ' || [ -z "$_gfiltered" ]; }; then
      note "go test ./cmd exited ${_grc} without running a test - the package did not build or the module was not found. This is a fault in the instrument, not a finding about the chart, so it is a meta-failure and NOT a chart failure. Full output follows."
      printf '%s\n' "$_gout" | sed 's/^/    /'
    elif [ "$_grc" -ne 0 ]; then
      echo ">>> cmd/helm_chart_ha_contract_test.go: FAILED (exit ${_grc}). Full output follows; it is not filtered, because the filter that used to stand here reduced this case to two empty section headers."
      printf '%s\n' "$_gout" | sed 's/^/    /'
      real_failure=1
    else
      echo ">>> cmd/helm_chart_ha_contract_test.go: ok (${_ct} tests; hack/ha-gates.txt re-derived and unchanged)"
    fi
  fi
fi

# --- count check 4: the assertion total ---------------------------------------
# UNCONDITIONAL. [HISTORY 2026-08-17] This condition used to read
#
#   real_failure -eq 0 && meta_fail -eq 0 && total_assertions -ne EXPECTED
#
# and both conjuncts are now deleted. The reason they were there was sound: when
# the total was parsed from each script's "PASS n/m" line, only scripts that
# SUCCEEDED contributed, so a red run was legitimately short and the note would
# have been spurious. THAT REASON STOPS APPLYING once the total is summed from
# ASSERTIONS_EXECUTED, which every script emits whether it passes or fails. The
# number now answers "did every assertion RUN", which is orthogonal to whether
# they passed - and that is the question rule 9 says the number is for. So this
# is a correct deletion following a change of premise, not a loosening.
#
# What the conjuncts cost while they were there: a single red assertion in
# update-strategy.sh printed "assertions: 102/106  meta-failures: 0" with helm
# fully present - a four-assertion shortfall on screen and nothing flagging it -
# because a failing script contributed 0 rather than n. The guard was switched
# off by the condition that made it necessary, which is the same family as a
# floor that passes an over-long run and as an assertion satisfied by its
# toolchain being absent.
if [ "$total_assertions" -ne "$EXPECTED_ASSERTIONS" ]; then
  note "executed ${total_assertions} assertions across ${ran} scripts, expected exactly ${EXPECTED_ASSERTIONS}. If you added or removed assertions, update EXPECTED_ASSERTIONS here as well as the script's own EXPECTED_TOTAL - that this number is stated twice is the point."
fi

summary "$ran" "$total_assertions"
if [ "$meta_fail" -ne 0 ]; then
  echo "META-FAILURE: the check set did not run as committed. This is not a passing run."
  exit 2
fi
if [ "$real_failure" -ne 0 ]; then
  echo "FAILED: at least one script reported an assertion failure."
  exit 1
fi
echo "PASS ${total_assertions}/${EXPECTED_ASSERTIONS} assertions across ${ran}/${EXPECTED_SCRIPTS} scripts."
