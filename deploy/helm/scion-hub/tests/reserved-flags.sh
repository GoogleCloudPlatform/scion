#!/usr/bin/env bash
# Reserved-flag assertions for deploy/helm/scion-hub.
#
# Provenance: this is the script that produced 31/31 at 7911e16 and at 721fc77
# in the round-4 mechanical pass. Committed so the numbers are re-runnable by
# someone other than their author. Location is provisional; Phase 6 owns CI
# wiring and may relocate this. Nothing here is wired into CI on purpose.
#
# Adopted from gd-p0-rev-2's handover with ONE change, recorded so the provenance
# claim above stays exact: CHART now defaults to this script's own parent
# directory instead of a repo-relative path, so it also works from an unpacked
# package. No assertion, count or message was altered.
#
# FAILS CLOSED. Rule 9 applied to the tooling itself: it asserts the number of
# assertions EXECUTED, not merely the absence of a failure. A run that executes
# zero cases exits non-zero. The Phase 7 stage guard printed seventeen oks over
# an empty table and exited 0; this must not be able to do that.
set -u

EXPECTED_TOTAL=31          # 29 must-reject + 2 must-accept. Update deliberately.
CHART="${CHART:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
HELM="${HELM:-helm}"
BASE=(--set image.repository=r --set hub.hubId=h)

executed=0
failed=0

reject() {  # a flag the chart must refuse
  executed=$((executed + 1))
  if "$HELM" template t "$CHART" "${BASE[@]}" --set-json "hub.args=[\"--$1\"]" >/dev/null 2>&1; then
    echo "FAIL  accepted but must reject: --$1"; failed=$((failed + 1))
  else
    echo "ok    rejected: --$1"
  fi
}

accept() {  # POSITIVE TWIN: a benign flag the chart must still allow.
  executed=$((executed + 1))
  if "$HELM" template t "$CHART" "${BASE[@]}" --set-json "hub.args=[$1]" >/dev/null 2>&1; then
    echo "ok    accepted: $1"
  else
    echo "FAIL  rejected but must accept: $1"; failed=$((failed + 1))
  fi
}

# $setByChart - the chart renders these; pflag is last-wins.
for f in foreground hosted host web-port enable-hub enable-runtime-broker \
         enable-web auto-provide global; do reject "$f"; done
# $neverPassed - config selection.
for f in config c project g grove profile p; do reject "$f"; done
# $aliasOrIgnored - not the lever they appear to be.
for f in production port; do reject "$f"; done
# $ownedByConfig - delivered through another channel.
for f in admin-emails base-url db storage-bucket storage-dir; do reject "$f"; done
# $unsafeToPass - weaken auth or expose credentials.
for f in session-secret dev-auth enable-test-login web-assets-dir; do reject "$f"; done
# Case-insensitivity of the reserved match (pflag itself is case-SENSITIVE).
for f in CONFIG Global; do reject "$f"; done

# Positive twins. Without these the suite passes by refusing everything, which
# is the shape that let --admin-token=hunter2 through in round 1.
accept '"--log-level","debug"'
accept '"--verbose"'

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
