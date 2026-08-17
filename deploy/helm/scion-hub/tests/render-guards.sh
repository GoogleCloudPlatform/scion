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
# A PEM header contains spaces, so the whitespace guard reaches it FIRST. Both
# are rejections; asserting which message fires keeps the ordering honest rather
# than letting the credential axis take credit for a catch it did not make.
reject "PEM header (whitespace guard wins)" "contains whitespace" --set 'hub.args[0]=--x=-----BEGIN RSA PRIVATE KEY-----'
# The PEM alternative is only reachable on argv through a non-flag-shaped entry.
# Do not delete it as dead: the helper is shared, and Phase 1 and Phase 3 call it
# on environment values where a multi-line PEM is legal and this is the catch.
reject "PEM in a positional"                "shape of a credential" --set 'hub.args[0]=x=-----BEGIN RSA PRIVATE KEY-----'

echo "== the failure message must not print what it caught =="
executed=$((executed + 1))
if render --set 'hub.args[0]=--upstream=postgres://scion:hunter2@10.0.0.1/scion' | grep -q 'hunter2'; then
  echo "FAIL  the credential guard leaked the password into its own error message"; failed=$((failed + 1))
else
  echo "ok    credential redacted in the failure message"
fi

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
executed=$((executed + 1))
if [ "$(render | sha256sum)" = "$(render --is-upgrade | sha256sum)" ]; then
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
