#!/usr/bin/env bash
# Render-time guard assertions for deploy/helm/scion-hub, other than the
# reserved-flag list (tests/reserved-flags.sh) and the updateStrategy
# derivation (tests/update-strategy.sh). Those two are separate because they
# were separately verified; between the three, every render-time refusal in
# _helpers.tpl has at least one case.
#
# FAILS CLOSED, same contract as its two siblings: it asserts the number of
# assertions EXECUTED against a committed total and exits 2 on a short run,
# distinct from 1 for a real failure. "The harness did not run" and "the chart
# is broken" need different reactions from whoever sees red. Absence of a
# failure is not evidence of a check.
#
# NO CI WIRING, deliberately. Phase 6 owns that and may relocate this file.
#
# WHY SO MANY CASES ASSERT THE MESSAGE AND NOT JUST THE EXIT CODE: several of
# these guards are layered, and a layered guard silently becomes a single point
# of failure the moment you assert only the outcome. Where two independent
# layers refuse the same value, there is one case per layer, and the lower layer
# is reached with --skip-schema-validation - which is how an operator reaches it
# too.
set -u

EXPECTED_TOTAL=46
CHART="${CHART:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
HELM="${HELM:-helm}"
BASE=(--set image.repository=r --set hub.hubId=ci-minimal)

# TOOL-PRESENCE ARM. A MISSING TOOLCHAIN MUST NOT BE REPORTED AS A BROKEN CHART.
# Without this every helm invocation fails, every assertion fails, and the output
# accuses the chart of dropping templates when the truth is that helm is not
# installed. Found by the first person to run this suite who was not its author,
# in a container without helm, in four minutes. A mutation suite inherits its
# author's environment, so the environment is the one variable it cannot mutate
# from the inside - the same shape as axis (d), answerable only from outside.
# "Nothing was analysed" is a THIRD outcome, distinct from clean and from failing,
# and it exits 2 with the other harness errors rather than 1.
#
# 🔴 AND THE PREFLIGHT DESTROYED THE MEASUREMENT THAT AUDITS THE PREFLIGHT.
# Adding it was correct and it stays. But the way to check whether an assertion
# in this file is reason-matched or fail-open is to run it with no toolchain and
# read which lines go green - and after this arm exists, there are no lines to
# read: the run reports ASSERTIONS_EXECUTED=0 and exits before any of them. The
# two vacuous greens at :110 and :184 were found that way; :212 survived because
# by then the probe that would have shown it could no longer be run.
# IMPROVING A MEASUREMENT CAN DESTROY ITS DIAGNOSTIC VALUE.
#
# So: AUDIT=1 bypasses the arm and lets the assertions run against a broken or
# absent toolchain, which is the only way to see which of them go green anyway.
# IT CAN NEVER CERTIFY ANYTHING - an audit run exits 2 unconditionally at the
# bottom, whatever the assertions say, so it cannot be pasted into a review as a
# pass and cannot be wired into run-all.sh by accident.
AUDIT="${AUDIT:-0}"
_missing=""
for _t in "$HELM" sha256sum; do command -v "$_t" >/dev/null 2>&1 || _missing="${_missing} ${_t}"; done
if [ -n "$_missing" ] && [ "$AUDIT" != "1" ]; then
  echo "HARNESS ERROR: required tool(s) not on PATH:${_missing}"
  echo "NOTHING WAS ANALYSED. This is not a passing run, and it is NOT a chart failure."
  echo "  To see WHICH assertions go green anyway - the fail-open audit - re-run with AUDIT=1."
  echo "  That mode exits 2 no matter what it prints. It is an instrument, not a verdict."
  echo "ASSERTIONS_EXECUTED=0"
  exit 2
fi
if [ "$AUDIT" = "1" ]; then
  echo "=================================================================="
  echo "AUDIT MODE. The tool-presence arm is BYPASSED. Missing:${_missing:- (none)}"
  echo "Every 'ok' below is a SUSPECT, not a result: it is an assertion that"
  echo "passed without the toolchain it claims to exercise. This run exits 2."
  echo "=================================================================="
fi

executed=0
failed=0

render() { "$HELM" template t "$CHART" "${BASE[@]}" "$@" 2>&1; }

# reject <label> <substring the message MUST contain> <helm args...>
reject() {
  local label="$1" want="$2"; shift 2
  executed=$((executed + 1))
  local out; out="$(render "$@")"
  if [ $? -eq 0 ]; then
    echo "FAIL  rendered but must reject: ${label}"; failed=$((failed + 1)); return
  fi
  case "$out" in
    *'%!'*)
       # gd-p1-dev's guard, adopted: a printf verb mismatch in _helpers.tpl
       # renders %!s(<nil>) inside a message whose wording still matches, so the
       # substring check below would go green on a diagnostic that shows the
       # operator nothing. Checked first because it is the more specific failure.
       echo "FAIL  ${label}: the refusal message could not render its own value (%!)"
       echo "        got:  $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)"
       failed=$((failed + 1)) ;;
    *"$want"*) echo "ok    rejected: ${label}" ;;
    *) echo "FAIL  ${label}: rejected with the WRONG message"
       echo "        want: ${want}"
       echo "        got:  $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)"
       failed=$((failed + 1)) ;;
  esac
}

# accept <label> <helm args...>
accept() {
  local label="$1"; shift
  executed=$((executed + 1))
  local out; out="$(render "$@")"
  if [ $? -eq 0 ]; then
    echo "ok    accepted: ${label}"
  else
    echo "FAIL  rejected but must accept: ${label}"
    echo "        $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)"
    failed=$((failed + 1))
  fi
}

echo "== credential guard, NAME axis =="
# The round-1 secret check inspected values only, so --admin-token=hunter2 was
# accepted: the value "hunter2" has no credential shape. The name axis is what
# catches it, and this row is the reason the axis exists.
reject "--admin-token=hunter2"    "names credential material" --set hub.args[0]=--admin-token=hunter2
reject "--api-key"                "names credential material" --set hub.args[0]=--api-key=abc
reject "--gh-pat"                 "names credential material" --set hub.args[0]=--gh-pat=abc
reject "--private-key"            "names credential material" --set hub.args[0]=--private-key=abc
reject "--upstream-password"      "names credential material" --set hub.args[0]=--upstream-password=abc
reject "--x-credential"           "names credential material" --set hub.args[0]=--x-credential=abc

echo "== credential guard, VALUE axis =="
reject "DSN with userinfo"  "embeds credentials in a URL" --set 'hub.args[0]=--upstream=postgres://scion:hunter2@10.0.0.1/scion'
reject "ghp_ prefix"        "shape of a credential"       --set 'hub.args[0]=--x=ghp_AAAAAAAAAAAAAAAAAAAA'
reject "sk- prefix"         "shape of a credential"       --set 'hub.args[0]=--x=sk-AAAAAAAAAAAA'
reject "AKIA prefix"        "shape of a credential"       --set 'hub.args[0]=--x=AKIAABCDEFGH1234'
# ORDERING PIN. Both messages are rejections, and asserting WHICH one fires is
# what keeps a guard from taking credit for a catch it did not make. This row
# was "whitespace guard wins" until the credential check moved from a list of
# named subtrees to a walk of the whole of .Values: the walk applies the VALUE
# axis to hub.args before the argv guard runs at all, so the credential axis now
# genuinely makes this catch rather than inheriting it.
#
# Nothing is given up by retargeting it. The walk shadows the VALUE axis ONLY -
# measured, the whitespace and name axes still answer from the argv guard at
# deployment.yaml, which is what the two whitespace rows and the underscore rows
# above pin. Whitespace-on-argv reachability is therefore still asserted; it is
# just no longer asserted through a PEM.
#
# One cost, recorded because it will confuse someone: helm cites the template it
# was rendering when the walk failed, which is serviceaccount.yaml, not the
# Deployment that owns argv. The message names values.hub.args[0] so the PATH is
# right, but the FILE in the citation is not where the operator should look.
reject "PEM header (value axis wins)"       "shape of a credential" --set 'hub.args[0]=--x=-----BEGIN RSA PRIVATE KEY-----'
# Keep both PEM rows. They no longer differ in which guard catches them, but
# they still differ in shape, and the helper is shared: Phase 1 and Phase 3 call
# it on environment values where a multi-line PEM is legal and this is the catch.
reject "PEM in a positional"                "shape of a credential" --set 'hub.args[0]=x=-----BEGIN RSA PRIVATE KEY-----'

echo "== the failure message must not print what it caught =="
# TWO CONDITIONS, AND THE FIRST ONE IS THE POINT. This was a bare negative -
# "the output does not contain hunter2" - which an EMPTY output satisfies
# perfectly. With no helm the render produced nothing, grep found nothing, and
# this printed "ok credential redacted", certifying a redaction guard that had
# not been consulted. Same family as a reject() satisfied by a missing binary,
# found by applying rev-2's rule to my own file rather than to theirs.
# So: establish that the guard actually fired, THEN assert the absence.
executed=$((executed + 1))
_out="$(render --set 'hub.args[0]=--upstream=postgres://scion:hunter2@10.0.0.1/scion')"
case "$_out" in
  *"embeds credentials in a URL"*)
    case "$_out" in
      *hunter2*) echo "FAIL  the credential guard leaked the password into its own error message"; failed=$((failed + 1)) ;;
      *) echo "ok    credential redacted in the failure message" ;;
    esac ;;
  *) echo "FAIL  credential redaction: the guard did not fire, so redaction was never tested"
     echo "        got: $(printf '%s' "$_out" | tr '\n' ' ' | cut -c1-160)"
     failed=$((failed + 1)) ;;
esac

echo "== underscore reachability =="
# Not input validation. The name pattern needs a hyphen or start-of-string
# before "secret", so SESSION_SECRET passed through unchanged matches NOTHING -
# the guard would render, read as applied, and miss precisely the value it was
# added for. These rows assert it fails loudly instead.
reject "--session_secret" "contains an underscore" --set hub.args[0]=--session_secret=x
reject "--api_key"        "Translate"              --set hub.args[0]=--api_key=x
reject "--pod_namespace"  "contains an underscore" --set hub.args[0]=--pod_namespace=x

echo "== whitespace on argv =="
reject "leading space"  "leading or trailing whitespace" --set 'hub.args[0]= --verbose'
reject "embedded space" "contains whitespace"            --set 'hub.args[0]=--log-level debug'

echo "== POSITIVE TWINS: the false-positive baseline =="
# Without these the suite passes by refusing everything. Each is a near-miss
# chosen to sit just inside a guard's boundary: -token-ttl and -max-tokens
# contain "token", -secret-manager-project contains "secret",
# -password-min-length contains "password", -keycloak-realm is auth-adjacent.
accept "--token-ttl"              --set hub.args[0]=--token-ttl=5m
accept "--max-tokens"             --set hub.args[0]=--max-tokens=100
accept "--secret-manager-project" --set hub.args[0]=--secret-manager-project=p
accept "--keycloak-realm"         --set hub.args[0]=--keycloak-realm=r
accept "--password-min-length"    --set hub.args[0]=--password-min-length=8
accept "--enable-debug"           --set hub.args[0]=--enable-debug
accept "a plain positional"       --set hub.args[0]=extra
accept "no extra args at all"

echo "== hub.hubId =="
reject "hubId empty"      "hubId" --set hub.hubId=""
reject "hubId whitespace" "hubId" --set 'hub.hubId= h '

echo "== two-layer guards: ONE CASE PER LAYER =="
# uid/gid 0 and an empty image.repository are each refused twice, independently.
# The schema fires first; --skip-schema-validation removes that layer and proves
# the helper stands alone. Asserting only the outcome would let either layer be
# deleted with the row still green - which is how a real two-layer guard once got
# "corrected" into a schema-only one here.
reject "runAsUser 0, schema layer"   "greater than or equal to 1" --set hub.securityContext.runAsUser=0
reject "runAsGroup 0, schema layer"  "greater than or equal to 1" --set hub.securityContext.runAsGroup=0
reject "runAsUser 0, helper layer"   "runAsUser may not be 0"     --skip-schema-validation --set hub.securityContext.runAsUser=0
reject "runAsGroup 0, helper layer"  "runAsGroup may not be 0"    --skip-schema-validation --set hub.securityContext.runAsGroup=0
reject "image.repository empty, schema layer" "String length must be greater than or equal to 1" --set image.repository=""
reject "image.repository empty, helper layer" "image.repository is required" --skip-schema-validation --set image.repository=""

echo "== startup budget: A PRODUCT, WHICH NO PER-FIELD SCHEMA BOUND CAN EXPRESS =="
# Every one of these rendered clean with the schema FULLY ACTIVE before the
# assertion existed. There is no --skip-schema-validation row because there is
# nothing in the schema to skip - that absence is the finding, not an omission.
reject "periodSeconds=1 gives 60s"  "the startup budget is too short" --set probes.startup.periodSeconds=1
reject "2 x 60 gives 120s"          "= 120s"                          --set probes.startup.periodSeconds=2
reject "startup off, liveness on"   "holds the liveness probe off"    --set probes.startup.enabled=false --set probes.liveness.enabled=true
accept "default 5 x 60 = 300s"
accept "startup off AND liveness off"  --set probes.startup.enabled=false
# FIXTURE FOR A REMOVED SCHEMA BOUND. failureThreshold's minimum went from 60 to
# 1 because the old bound rejected SAFE configurations (10 x 30 = 300s) while
# admitting unsafe ones (1 x 60 = 60s) - wrong, not merely weak. Both rows below
# set failureThreshold BELOW 60, which the old bound refused outright, and they
# differ only in the period: the pair can pass only if the guard reads the product.
reject "threshold 30, product 150s" "the startup budget is too short" --set probes.startup.failureThreshold=30
accept "threshold 30, product 300s" --set probes.startup.failureThreshold=30 --set probes.startup.periodSeconds=10
reject "sub-60 threshold is the HELPER's refusal, not a shadow of the old bound" \
       "the startup budget is too short" --skip-schema-validation --set probes.startup.failureThreshold=30

echo "== hub identity is stable across upgrade and independent of the release name =="
# hub.hubId must be used verbatim and must never be derived from anything Helm
# regenerates. A chart that interpolated .Release.Name or .Release.Revision would
# pass every case above and still re-scope the hub's storage on upgrade.
# ESTABLISH THAT THERE IS A RENDER BEFORE COMPARING RENDERS. Comparing two
# hashes is a bare negative wearing a positive's clothes: two failed renders
# agree, and this printed "ok render is identical for install and upgrade" on a
# machine where nothing rendered at all. It is the strongest false pass in this
# file, because the assertion it fakes - that hub identity survives an upgrade -
# is the one the whole hubId design exists for.
#
# 🔴 THE FIRST FIX FOR THAT WAS INCOMPLETE AND SHIPPED, AND THE REASON IS WORTH
# MORE THAN THE FIX. It tested `[ -z "$_a" ]`. But the defect was never "the
# output was empty" - it was "nothing rendered, and the check could not tell."
# Emptiness was the symptom in the world it was found in: no helm, so nothing
# on stdout. `render()` ends `2>&1`, so when helm IS present and the render
# fails, the output is not empty - it is the error message, and the two error
# messages are identical. Measured at 8cc8d9b with `hub.baseUrl` added to the
# schema's required list (helm v3.16.3+gcfd0749, /tmp/linux-amd64/helm):
#
#   install rc=1 bytes=127   upgrade rc=1 bytes=127   -z fires? NO   shas equal? YES
#   -> "ok    render is identical for install and upgrade"
#
# Found by gd-p0-rev-2 and gd-p0-rev-3 independently, from different containers,
# byte-identical. THE REMEDY ARRIVED CARRYING THE DEFECT IT WAS WRITTEN TO
# REMOVE, because it was written against the description of the bug rather than
# against its mechanism. So this now gates on what is actually claimed - that
# helm SUCCEEDED and produced the manifest whose identity is under test - and
# keeps `-z` as well, which costs nothing and is not the load-bearing arm.
executed=$((executed + 1))
_a="$(render)"; _rc_a=$?
_b="$(render --is-upgrade)"; _rc_b=$?
if [ "$_rc_a" -ne 0 ] || [ "$_rc_b" -ne 0 ]; then
  echo "FAIL  install/upgrade comparison: helm EXITED NON-ZERO (install=${_rc_a} upgrade=${_rc_b}), so nothing was compared"
  echo "        got: $(printf '%s' "$_a" | tr '\n' ' ' | cut -c1-160)"
  failed=$((failed + 1))
elif [ -z "$_a" ] || [ -z "$_b" ]; then
  echo "FAIL  install/upgrade comparison: one or both renders were EMPTY, so nothing was compared"
  failed=$((failed + 1))
elif ! printf '%s\n' "$_a" | grep -q '^kind: Deployment$' \
  || ! printf '%s\n' "$_b" | grep -q '^kind: Deployment$'; then
  echo "FAIL  install/upgrade comparison: helm exited 0 but the output carries no Deployment, so the compared strings are not the manifest"
  failed=$((failed + 1))
elif [ "$(printf '%s' "$_a" | sha256sum)" = "$(printf '%s' "$_b" | sha256sum)" ]; then
  echo "ok    render is identical for install and upgrade"
else
  echo "FAIL  render differs between install and upgrade"; failed=$((failed + 1))
fi
for rel in t other-release t2; do
  executed=$((executed + 1))
  got="$("$HELM" template "$rel" "$CHART" "${BASE[@]}" | grep -o 'scion.io/hub-id: .*' | sort -u)"
  if [ "$got" = 'scion.io/hub-id: "ci-minimal"' ]; then
    echo "ok    hub-id verbatim under release name '${rel}'"
  else
    echo "FAIL  hub-id under release '${rel}': ${got}"; failed=$((failed + 1))
  fi
done

echo "---"
echo "executed=${executed} expected=${EXPECTED_TOTAL} failed=${failed}"
# Emitted unconditionally, on every exit path, so run-all.sh can sum what
# actually ran even when this script is reporting a failure. The count check must
# not be silenced by the outcome it is meant to qualify.
echo "ASSERTIONS_EXECUTED=${executed}"

# THE AUDIT EXIT, PLACED BEFORE EVERY OTHER VERDICT SO NOTHING CAN OVERTAKE IT.
# An audit run is an observation of a deliberately broken world. It has no
# passing form, so it does not get one.
if [ "$AUDIT" = "1" ]; then
  echo "AUDIT MODE: ${executed} assertions ran, ${failed} failed, so $((executed - failed)) went GREEN WITHOUT A WORKING TOOLCHAIN."
  echo "Each of those is a candidate fail-open assertion. This is not a pass."
  exit 2
fi

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
