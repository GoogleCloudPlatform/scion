#!/usr/bin/env bash
# R3-1 cases: the updateStrategy refusal was removed; these pin what replaced it.
#
# Provenance: produced 4/4 at 721fc77. At 7911e16 case 3 failed by design
# (exit 1 at deployment.yaml:27:17) - that is the pre-fix baseline.
#
# FAILS CLOSED, same contract as reserved-flags.sh.
#
# Adopted from gd-p0-rev-2's handover with one change: CHART defaults to this
# script's own parent directory rather than a repo-relative path. No assertion,
# count or message was altered.
set -u

EXPECTED_TOTAL=4
CHART="${CHART:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
HELM="${HELM:-helm}"
BASE=(--set image.repository=r --set hub.hubId=h)

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
  if [ "$want" = "RollingUpdate" ] && ! printf '%s\n' "$out" | grep -q 'maxUnavailable: 0'; then
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
