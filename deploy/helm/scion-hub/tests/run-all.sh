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
# THREE COUNTS, COMMITTED, EACH FAILING IN BOTH DIRECTIONS:
#
#   EXPECTED_SCRIPTS     how many scripts must run. Adding a fifth fails here
#                        until someone bumps this number in a diff.
#   the on-disk set      every *.sh beside this file must be enumerated below,
#                        and every name below must exist. This is the half that
#                        catches the original defect - a script added to the
#                        directory but never wired in. A count alone would not:
#                        it is defeated by one script added as another is
#                        dropped.
#   EXPECTED_ASSERTIONS  the sum across all scripts. This duplicates each
#                        script's own EXPECTED_TOTAL on purpose. Without it,
#                        deleting assertions AND lowering that script's total is
#                        green everywhere - the breach produces no symptom that
#                        points at the number.
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

EXPECTED_SCRIPTS=4
EXPECTED_ASSERTIONS=106

# Enumerated by name, not globbed into a loop. A glob would run whatever is
# present and could never notice that something is absent.
SCRIPTS=(
  reserved-flags.sh     # 31 - the reserved-flag lists
  update-strategy.sh    #  4 - the updateStrategy derivation
  render-guards.sh      # 46 - every other render-time refusal
  chart-integrity.sh    # 25 - .helmignore breadth and the packaged file set
)

meta_fail=0
note() { echo "META  $*"; meta_fail=$((meta_fail + 1)); }

# --- count check 1: the enumeration matches its committed size ---------------
if [ "${#SCRIPTS[@]}" -ne "$EXPECTED_SCRIPTS" ]; then
  note "SCRIPTS lists ${#SCRIPTS[@]} entries, EXPECTED_SCRIPTS is ${EXPECTED_SCRIPTS}. Bump the number in the same diff that changes the list."
fi

# --- count check 2: the enumeration matches the directory --------------------
# Both directions. Missing catches a stale entry; unenumerated catches the
# defect this file was written for.
for s in "${SCRIPTS[@]}"; do
  [ -f "${HERE}/${s}" ] || note "enumerated but not present on disk: ${s}"
done
while IFS= read -r found; do
  case " ${SCRIPTS[*]} " in
    *" ${found} "*) ;;
    *) note "present on disk but NOT ENUMERATED, so it would never run: ${found}. Add it to SCRIPTS and bump EXPECTED_SCRIPTS." ;;
  esac
done < <(cd "$HERE" && ls -1 *.sh 2>/dev/null | grep -v '^run-all\.sh$')

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
  case "$rc" in
    0) n="$(printf '%s\n' "$out" | sed -n 's|^PASS \([0-9]*\)/.*|\1|p' | tail -1)"
       if [ -z "$n" ]; then
         note "${s} exited 0 without a parseable 'PASS n/m' line, so its assertion count cannot be summed."
       else
         total_assertions=$((total_assertions + n))
       fi ;;
    1) echo ">>> ${s}: ASSERTION FAILURE (exit 1)"; real_failure=1 ;;
    2) note "${s} exited 2: it did not run its full set." ;;
    *) note "${s} exited ${rc}, which is not part of the contract." ;;
  esac
done

# --- count check 3: every script actually ran --------------------------------
[ "$ran" -eq "$EXPECTED_SCRIPTS" ] || note "ran ${ran} scripts, expected exactly ${EXPECTED_SCRIPTS}."

# --- count check 4: the assertion total ---------------------------------------
if [ "$real_failure" -eq 0 ] && [ "$meta_fail" -eq 0 ] && [ "$total_assertions" -ne "$EXPECTED_ASSERTIONS" ]; then
  note "executed ${total_assertions} assertions across ${ran} scripts, expected exactly ${EXPECTED_ASSERTIONS}. If you added or removed assertions, update EXPECTED_ASSERTIONS here as well as the script's own EXPECTED_TOTAL - that this number is stated twice is the point."
fi

echo
echo "================================================================"
echo "scripts: ${ran}/${EXPECTED_SCRIPTS}   assertions: ${total_assertions}/${EXPECTED_ASSERTIONS}   meta-failures: ${meta_fail}"
if [ "$meta_fail" -ne 0 ]; then
  echo "META-FAILURE: the check set did not run as committed. This is not a passing run."
  exit 2
fi
if [ "$real_failure" -ne 0 ]; then
  echo "FAILED: at least one script reported an assertion failure."
  exit 1
fi
echo "PASS ${total_assertions}/${EXPECTED_ASSERTIONS} assertions across ${ran}/${EXPECTED_SCRIPTS} scripts."
