#!/usr/bin/env bash
# Reserved-flag assertions for deploy/helm/scion-hub.
#
# Provenance: this is the script that produced 31/31 at 7911e16 and at 721fc77
# in the round-4 mechanical pass. Committed so the numbers are re-runnable by
# someone other than their author. Location is provisional; Phase 6 owns CI
# wiring and may relocate this. Nothing here is wired into CI on purpose.
#
# Adopted from gd-p0-rev-2's handover with FOUR changes, recorded so the
# provenance claim above stays exact:
#   1. CHART now defaults to this script's own parent directory instead of a
#      repo-relative path, so it also works from an unpacked package.
#   2. The short-run guard is an INEQUALITY, not a floor, per gd-em's ruling -
#      a run that executes MORE assertions than committed is the same defect
#      facing the other way. The message at the bottom changed with it.
#   3. A tool-presence arm, and an ASSERTIONS_EXECUTED line for run-all.sh.
#   4. 🔴 reject() NOW ASSERTS THE REFUSAL'S REASON, NOT ITS EXIT STATUS. This
#      is a change to what all 29 reject assertions MEAN, not merely to how they
#      are spelled - see the comment on reject() below for the three measured
#      worlds that satisfied the old form. rev-2 found it in its own helper and
#      specified the fix; the failing-message text is the only thing added.
#
# WHICH PARTS ARE STILL rev-2's: the 31 cases and the count. No case was added,
# removed or repointed at a different flag. What changed is the predicate every
# reject case is judged by, and item 4 is why the earlier version of this header
# - which said "No assertion was altered" and kept saying it after item 2 landed
# - was itself an instance of the defect this file exists to catch.
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
command -v "$HELM" >/dev/null 2>&1 || _missing="${_missing} ${HELM}"
if [ -n "$_missing" ]; then
  echo "HARNESS ERROR: required tool(s) not on PATH:${_missing}"
  echo "NOTHING WAS ANALYSED. This is not a passing run, and it is NOT a chart failure."
  echo "ASSERTIONS_EXECUTED=0"
  exit 2
fi

executed=0
failed=0

# reject <flag> -- a flag the chart must refuse, AND THE REASON IS ASSERTED.
#
# 🔴 THIS USED TO READ `if helm template ...; then FAIL; else ok; fi` - any
# non-zero exit taken as proof of refusal. Three unrelated worlds produce a
# non-zero exit from a chart that was never consulted, and all three were
# measured, not imagined:
#
#   1. helm is not installed. "command not found" is non-zero. Twenty-nine of
#      the thirty-one assertions here printed "ok" on a machine with no helm,
#      declaring the reserved-flag mechanism verified. Only the two accept()
#      twins went red, and they exist solely because a reviewer insisted on a
#      positive twin in an earlier round.
#   2. the chart cannot render for an unrelated reason. Phase 1 adds a required
#      value; until BASE supplies it the render fails at schema validation and
#      all twenty-nine go green again - WITH HELM PRESENT, so no tool-presence
#      guard catches this one.
#   3. any future rename, syntax error or bad CHART path.
#
# Matching the message defeats all three at once, and matching THIS message -
# which names the specific flag - defeats a fourth: the wrong reservation firing
# and taking credit for the catch. Matching "execution error at" would not,
# because world 2 produces one too.
#
# The general rule, which cost four people most of a day to converge on: A CLAIM
# MUST BE CHECKED AT ITS MECHANISM, NOT AT ITS OUTCOME, BECAUSE THE OUTCOME IS
# WHAT A BROKEN WORLD ALSO PRODUCES. A negative assertion that checks the reason
# degrades closed for free; one that checks only the outcome degrades open.
reject() {
  executed=$((executed + 1))
  # LOWERCASED, because the guard reports the CANONICAL reservation it matched
  # rather than echoing the operator's spelling: --CONFIG and --Global are here
  # precisely to prove the match is case-insensitive, and both are refused with
  # "-config" and "-global" in the message. Discovered by this very assertion on
  # its first run - the old exit-status form could not have told the difference,
  # which is a small demonstration of the point of the change.
  local out want naive lower norm
  # NORMALISE THE WAY THE HELPER DOES: strip leading dashes, strip from '='.
  # Callers below pass bare names, so this is a no-op today and is here so that
  # a later caller passing "--base-url=x" cannot silently produce a matcher that
  # never fires. A no-op guard against a caller that does not exist yet would
  # normally fail axis (d); it earns its place because the FAILURE MODE IS A
  # SILENT NON-MATCH that the counter-form below would report as a broken world
  # rather than as a bad argument.
  norm="${1#"${1%%[!-]*}"}"
  norm="${norm%%=*}"
  lower="$(printf '%s' "$norm" | tr '[:upper:]' '[:lower:]')"
  # ONE DASH, AND THE TRAILING COLON IS LOAD-BEARING - DO NOT DROP EITHER.
  #
  # One dash: _helpers.tpl strips both leading dashes and any =value before
  # interpolating, so the message for --dev-auth reads "-dev-auth". Matching
  # "--dev-auth:" finds nothing and turns all 29 red. That failure is fail-CLOSED
  # and therefore safe, but it is a trap in the other direction: 29 reds invite
  # loosening the matcher until they pass, and the nearest loosening is
  # "execution error at", which is exactly the string a schema rejection also
  # produces. Normalise; never loosen.
  #
  # The colon: without it, want="-c" MATCHES a message reading "-config:".
  # THREE such pairs are live, all three inside $neverPassed, all three measured:
  #
  #     sends -config   matcher -c   with colon: no match   without: MATCHES
  #     sends -grove    matcher -g   with colon: no match   without: MATCHES
  #     sends -profile  matcher -p   with colon: no match   without: MATCHES
  #     sends -c        matcher -c   with colon: MATCHES    without: MATCHES  <- control
  #
  # An unanchored matcher lets the guard fire for the WRONG reservation and
  # still print ok. That is a FALSE GREEN, strictly worse than the all-red
  # failure a wrong dash-count produces, because nothing draws attention to it.
  # The last row is the control: it is the one that must match either way, and
  # without it "with colon: no match" on the first three rows is equally
  # consistent with a matcher that never matches anything.
  #
  # All three pairs are already explicit cases - c, config, g, grove, p and
  # profile each appear in the $neverPassed loop below and each asserts its own
  # name - so a misattribution in either direction fails one of them. No case
  # was added for them and EXPECTED_TOTAL is unchanged at 31.
  want="hub.args may not contain -${lower}:"
  # COUNTER-FORM. A negative grep's control is the counter-form of the same
  # grep: when the toolchain works and the guard fires, exactly one of these two
  # must appear. Both absent means a broken world rather than a refusal - which
  # is what an exit-status check could never distinguish - and both present, or
  # the naive form alone, means the message format moved under us.
  naive="hub.args may not contain --${lower}:"
  if out="$("$HELM" template t "$CHART" "${BASE[@]}" --set-json "hub.args=[\"--$1\"]" 2>&1)"; then
    echo "FAIL  accepted but must reject: --$1"; failed=$((failed + 1)); return
  fi
  # gd-p1-dev's guard, checked first because it is the more specific failure: a
  # diagnostic that cannot render its own value. A printf verb mismatch in the
  # helper yields %!s(<nil>) INSIDE a message whose surrounding wording still
  # matches, so the reason-match below would pass while the operator is shown
  # nothing usable.
  case "$out" in
    *'%!'*)
       echo "FAIL  --$1: the refusal message could not render its own value (%!)"
       echo "        got: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
       failed=$((failed + 1)); return ;;
  esac

  local m_want=0 m_naive=0
  case "$out" in *"$want"*)  m_want=1 ;;  esac
  case "$out" in *"$naive"*) m_naive=1 ;; esac

  # rev-3's COUNTER-FORM CONTROL. A bare `grep -q` for the normalised form is a
  # positive assertion, but the thing it certifies - "the guard refused this
  # specific flag" - is only established if the OTHER spelling is absent. Running
  # both against the same captured output makes the arithmetic total the
  # assertion, and each sum means something different:
  #
  #   1  exactly one form present  -> the guard fired, spelled as we expect
  #   0  neither present           -> NOT a refusal by this guard. A broken world:
  #                                   schema rejection, bad CHART path, rename.
  #                                   This is the world an exit-status check
  #                                   reported as ok 29 times with no helm.
  #   2  both present              -> the message contains both spellings, so the
  #                                   matcher is no longer discriminating and the
  #                                   colon anchor above may be doing nothing.
  #
  # The sum is checked with -ne against 1, both directions, per the same rule the
  # assertion counts follow: where a check counts anything, commit the number.
  if [ $((m_want + m_naive)) -eq 1 ] && [ "$m_want" -eq 1 ]; then
    echo "ok    rejected: --$1"
  elif [ $((m_want + m_naive)) -eq 0 ]; then
    echo "FAIL  --$1: refused, but NOT by the reserved-flag guard - NEITHER spelling present."
    echo "        This is a broken world, not a refusal: no helm, a schema rejection,"
    echo "        a renamed helper, or a bad CHART path all land here."
    echo "        want message containing: ${want}"
    echo "        got: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
    failed=$((failed + 1))
  else
    echo "FAIL  --$1: counter-form control violated (normalised=${m_want} naive=${m_naive})."
    echo "        Exactly one spelling must appear. Both, or only the double-dash form,"
    echo "        means the helper's message format moved and this matcher no longer"
    echo "        discriminates between reservations."
    echo "        got: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
    failed=$((failed + 1))
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
