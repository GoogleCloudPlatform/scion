#!/usr/bin/env bash
# R3-1 cases: the updateStrategy refusal was removed; these pin what replaced it.
#
# Provenance: produced 4/4 at 721fc77. At 7911e16 case 3 failed by design
# (exit 1 at deployment.yaml:27:17) - that is the pre-fix baseline.
#
# FAILS CLOSED, same contract as reserved-flags.sh.
#
# Adopted from gd-p0-rev-2's handover with three changes: CHART defaults to this
# script's own parent directory rather than a repo-relative path; the short-run
# guard is an INEQUALITY rather than a floor, per gd-em's ruling, and its message
# changed with it; and a tool-presence arm plus an ASSERTIONS_EXECUTED line were
# added. No assertion was altered, added or removed - the 4 cases are rev-2's.
set -u

EXPECTED_TOTAL=4
CHART="${CHART:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
HELM="${HELM:-helm}"
# auth.sessionSecret became REQUIRED in the session-secret phase, and it is here for the same
# reason hub.baseUrl is: scion-hub.assertSessionSecret fails the render without it, so every
# BASE render would return an error string instead of manifests and every check below would
# accuse the chart of a fault it does not have. The chart will not default it - a generated
# secret rotates on every helm upgrade, invalidating every session and the JWT signing key.
BASE=(--set image.repository=r --set hub.hubId=h --set hub.baseUrl=https://h.example.invalid --set auth.sessionSecret=harness-not-a-real-secret)   # hub.baseUrl became REQUIRED in Phase 1; see the arm below.

# TOOL-PRESENCE ARM. A MISSING TOOLCHAIN MUST NOT BE REPORTED AS A BROKEN CHART.
# Without this every helm invocation fails, every assertion fails, and the output
# accuses the chart of dropping templates when the truth is that helm is not
# installed. Found by the first person to run this suite who was not its author,
# in a container without helm, in four minutes. A mutation suite inherits its
# author's environment, so the environment is the one variable it cannot mutate
# from the inside - the same shape as axis (d), answerable only from outside.
# "Nothing was analysed" is a THIRD outcome, distinct from clean and from failing,
# and it exits 2 with the other harness errors rather than 1.
_missing=""
for _t in "$HELM" awk; do command -v "$_t" >/dev/null 2>&1 || _missing="${_missing} ${_t}"; done
if [ -n "$_missing" ]; then
  echo "HARNESS ERROR: required tool(s) not on PATH:${_missing}"
  echo "NOTHING WAS ANALYSED. This is not a passing run, and it is NOT a chart failure."
  echo "ASSERTIONS_EXECUTED=0"
  exit 2
fi

# BASE-VIABILITY ARM, AND IT IS THE SAME ARGUMENT AS THE TOOL-PRESENCE ARM ABOVE
# ONE STEP IN. A missing toolchain makes every render fail; so does a BASE that
# no longer satisfies the chart's required values, and the output is worse,
# because it is not empty - it is every assertion confidently blaming the guard
# it was aimed at. MEASURED: Phase 1 made hub.baseUrl required, and this suite
# emitted 77 failures reading "refused, but NOT by the reserved-flag guard" and
# "rejected with the WRONG message". Every one of those sentences was false. The
# guards were fine; BASE was.
#
# A REQUIRED VALUE ADDED BY A LATER PHASE INVALIDATES EVERY BASE IN THIS SUITE AT
# ONCE, AND NOTHING ABOUT IT IS SPECIFIC TO hub.baseUrl. Adding the flag fixes
# today; this arm is what makes the NEXT one arrive as one honest line instead of
# seventy-seven misleading ones. Exit 2, not 1: an unrenderable BASE means the run
# is not evidence about the chart, in either direction.
if ! _bv="$("$HELM" template t "$CHART" "${BASE[@]}" 2>&1)" || [ -z "$_bv" ]; then
  echo "HARNESS ERROR: BASE alone does not render, so no assertion below tests what it says it tests."
  echo "  This is almost always a newly-REQUIRED value that BASE does not set."
  printf '%s\n' "$_bv" | sed 's/^/  | /' | head -5
  echo "NOTHING WAS ANALYSED. This is not a passing run, and it is NOT a chart failure."
  echo "ASSERTIONS_EXECUTED=0"
  exit 2
fi
unset _bv

executed=0
failed=0

strategy_is() {  # <desc> <expected-type> <extra helm args...>
  local desc="$1" want="$2"; shift 2
  executed=$((executed + 1))
  local out got
  if ! out="$("$HELM" template t "$CHART" "${BASE[@]}" "$@" 2>&1)"; then
    echo "FAIL  ${desc}: render failed"; failed=$((failed + 1)); return
  fi
  got="$(printf '%s\n' "$out" | awk '/^  strategy:/{getline; print $2; exit}')"
  if [ "$got" != "$want" ]; then
    echo "FAIL  ${desc}: strategy type is '${got}', want '${want}'"; failed=$((failed + 1)); return
  fi
  # RollingUpdate must carry maxUnavailable: 0. Asserting the type alone would
  # pass a fix that deleted the fail AND broke the derivation.
  if [ "$want" = "RollingUpdate" ] && ! printf '%s\n' "$out" | grep -qF 'maxUnavailable: 0'; then
    echo "FAIL  ${desc}: RollingUpdate without maxUnavailable: 0"; failed=$((failed + 1)); return
  fi
  echo "ok    ${desc}: ${got}"
}

# 1-2. The default derivation, which R3-1 deliberately KEPT.
strategy_is "default at replicaCount=1" Recreate      --set replicaCount=1
strategy_is "default at replicaCount=2" RollingUpdate --set replicaCount=2
# 3. POSITIVE TWIN for the removed refusal. Previously exit 1. Must now RENDER
#    and emit RollingUpdate + maxUnavailable: 0 - not merely stop erroring.
strategy_is "explicit RollingUpdate at replicaCount=1" RollingUpdate \
  --set updateStrategy.type=RollingUpdate --set replicaCount=1
# 4. The explicit-override path is not collateral damage.
strategy_is "explicit Recreate at replicaCount=2" Recreate \
  --set updateStrategy.type=Recreate --set replicaCount=2

echo "---"
echo "executed=${executed} expected=${EXPECTED_TOTAL} failed=${failed}"
# Emitted unconditionally, on every exit path, so run-all.sh can sum what
# actually ran even when this script is reporting a failure. The count check must
# not be silenced by the outcome it is meant to qualify.
echo "ASSERTIONS_EXECUTED=${executed}"

if [ "$executed" -ne "$EXPECTED_TOTAL" ]; then
  # INEQUALITY, NOT A FLOOR. A short run is a failed run; a LONG run means
  # assertions were added without committing the number, which is the same
  # defect facing the other way. Where a check counts anything, the number is
  # committed and both directions fail.
  echo "HARNESS ERROR: executed ${executed} assertions, expected exactly ${EXPECTED_TOTAL}."
  exit 2
fi
[ "$failed" -eq 0 ] || exit 1
echo "PASS ${executed}/${EXPECTED_TOTAL}"
