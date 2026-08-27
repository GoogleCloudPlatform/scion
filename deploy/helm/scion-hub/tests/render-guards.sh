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

EXPECTED_TOTAL=77   # 63 + 14 from the oauth credential section (Phase 3).
CHART="${CHART:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
HELM="${HELM:-helm}"
# auth.sessionSecret became REQUIRED in the session-secret phase, and it is here for the same
# reason hub.baseUrl is: scion-hub.assertSessionSecret fails the render without it, so every
# BASE render would return an error string instead of manifests and every check below would
# accuse the chart of a fault it does not have. The chart will not default it - a generated
# secret rotates on every helm upgrade, invalidating every session and the JWT signing key.
BASE=(--set image.repository=r --set hub.hubId=ci-minimal --set hub.baseUrl=https://ci-minimal.example.invalid --set auth.sessionSecret=harness-not-a-real-secret)   # hub.baseUrl became REQUIRED in Phase 1; see the arm below.

# A COMPLETE WEB CLIENT CREDENTIAL, for the rows that need auth.mode=oauth to
# render at all. Not folded into BASE, because several rows below exist
# specifically to assert that oauth WITHOUT this is refused - putting it in BASE
# would make those rows unwriteable, and worse, would make them look written.
# It replaces --set auth.acknowledgeOAuthUnlanded=true, which is what these rows
# carried while the credentials had no channel.
OAUTH_WEB=(--set auth.oauth.web.google.clientId=rg-web-google-id --set auth.oauth.web.google.clientSecret=rg-web-google-secret)

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

render() { "$HELM" template t "$CHART" "${BASE[@]}" "$@" 2>&1; }

# reject <label> <substring the message MUST contain> <helm args...>
reject() {
  local label="$1" want="$2"; shift 2
  executed=$((executed + 1))
  local out
  if out="$(render "$@")"; then
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
  local out
  if out="$(render "$@")"; then
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

echo "== the HA-unlanded gate: THREE ROUTES, TRANSCRIBED FROM THE HUB =="
# scion-hub.assertHAUnlanded refuses the shapes this chart can render but cannot
# start. The routes are not this chart's invention: they are isHADeployment
# (cmd/server_foreground.go:927), and the hub's own tripwire test asserts the
# same three at cmd/server_ha_preflight_test.go:248-256 on ab0d227. THE ROUTES
# ARE TRANSCRIBED, SO THEY CAN DRIFT, which is what these rows are for.
#
# EACH NEGATIVE ASSERTS *WHICH* ROUTE FIRED, NOT MERELY THAT SOMETHING DID. The
# three routes overlap in the value space - postgres implies gcs by schema rule,
# and gcs plus proxy is route 3 - so a row that only checked for a refusal would
# stay green with two of the three routes deleted. Row r2 uses auth.mode oauth
# for exactly that reason: it is the only way to reach the postgres route with
# the gcs-plus-proxy route switched off. My first matrix did not do this and
# three of its rows were confounded by pre-existing schema allOf rules doing the
# refusing instead of this guard.
# POSTGRES NOW COSTS FOUR MORE VALUES, AND THAT IS THE CLOUD SQL PHASE, NOT A
# WORKAROUND. --set database.driver=postgres on its own is a hard refusal since
# the proxy landed: the chart reaches Postgres only through the Cloud SQL Auth
# Proxy, so a postgres driver with cloudsql.enabled false renders a DSN pointing
# at a loopback port nothing binds. Every row below that wants to reach an HA
# route THROUGH postgres has to get past that refusal first, or it measures the
# Cloud SQL guard while claiming to measure the HA guard - a green row about the
# wrong subject.
#
# MEASURED, and this is why the list is here rather than inline: with these four
# omitted, r2 and its positive twin both fail with "database.driver is postgres
# but cloudsql.enabled is false" instead of the HA refusal, and the gate-name arm
# below reports 0/7 because the message it reads is the Cloud SQL one.
CLOUDSQL_SET=(
  --set cloudsql.enabled=true
  --set cloudsql.instanceConnectionName=my-project:us-central1:db-1
  --set database.auth=iam
  --set database.name=scion
  --set serviceAccount.gcpServiceAccount=hub@my-project.iam.gserviceaccount.com
)

reject "r1: hub.extraEnv sets K_SERVICE" "hub.extraEnv sets K_SERVICE" \
  --set 'hub.extraEnv[0].name=K_SERVICE' --set 'hub.extraEnv[0].value=svc'
reject "r2: postgres driver, route 3 held off with oauth" "database.driver is postgres" \
  --set database.driver=postgres --set storage.provider=gcs --set storage.bucket=b \
  "${CLOUDSQL_SET[@]}" \
  --set auth.mode=oauth "${OAUTH_WEB[@]}"
reject "r3: gcs storage with proxy auth" "storage.provider is gcs and auth.mode is proxy" \
  --set storage.provider=gcs --set storage.bucket=b

# POSITIVE TWINS, ONE PER ROUTE. The flag is the escape hatch for rendering the
# settings.yaml without installing, so each refusal must be clearable - a guard
# with no way past it is a removed feature, not a warning.
accept "r1 + acknowledgeHAUnlanded" \
  --set 'hub.extraEnv[0].name=K_SERVICE' --set 'hub.extraEnv[0].value=svc' --set acknowledgeHAUnlanded=true
accept "r2 + acknowledgeHAUnlanded" \
  --set database.driver=postgres --set storage.provider=gcs --set storage.bucket=b \
  "${CLOUDSQL_SET[@]}" \
  --set auth.mode=oauth "${OAUTH_WEB[@]}" --set acknowledgeHAUnlanded=true
accept "r3 + acknowledgeHAUnlanded" \
  --set storage.provider=gcs --set storage.bucket=b --set acknowledgeHAUnlanded=true

# THE OVER-TRIGGER DIRECTION, WHICH IS THE HALF A REFUSAL SUITE USUALLY OMITS.
# Demanding an acknowledgement from a deployment that is not HA is a defect of
# the same size as failing to demand one from a deployment that is: it teaches
# operators to set the flag reflexively, and then the flag protects nobody.
# K_SERVICE with an empty value is the near-miss that matters - the hub reads
# os.Getenv != "" (:928), so an empty value is not a route, and a guard written
# on the name alone would refuse it.
accept "K_SERVICE present but with an explicit empty value" \
  --set 'hub.extraEnv[0].name=K_SERVICE' --set 'hub.extraEnv[0].value='
accept "an unrelated extraEnv name"       --set 'hub.extraEnv[0].name=NOT_K_SERVICE' --set 'hub.extraEnv[0].value=svc'
accept "sqlite + local storage + oauth"   --set auth.mode=oauth "${OAUTH_WEB[@]}"
accept "the chart defaults, no route"

# THE GATE LIST IS PART OF THE CONTRACT, SO IT IS ASSERTED RATHER THAN TRUSTED.
# gke-deploy-lead's Critical 1 requires the refusal to name every unlandable
# gate in hub order. The number was reported as five for most of a day because a
# prober stopped at the first gate it could not construct and its extent was
# read as the preflight's extent; then it was reported as eight, because that
# prober supplied a WELL-FORMED IAP audience and never reached the format gate.
# So the list is no longer written here. It is read out of hack/ha-gates.txt,
# which cmd/helm_chart_ha_contract_test.go derives by driving the real
# validateHostedHAPreflight over the chart's own golden settings.yaml. If the
# hub gains or loses a gate, this assertion changes with it and no one edits a
# constant. THE DENOMINATOR IS STILL ASSERTED, not the presence of at least one:
# a message naming three of the gates would satisfy any per-substring loop that
# has no total.
#
# CONTROLS RUN AGAINST THIS DERIVATION on 2026-08-17, all four red:
#
#   seed a KEY the refusal cannot name  -> FAIL 8/9, names it as unnamed
#   delete hack/ha-gates.txt            -> META-FAILURE, exit 2
#   seed an unrecognised PROSE gate     -> META-FAILURE, exit 2
#   point the awk header at nothing     -> META-FAILURE "holds 0 entries"
#
# The last is the apparatus control: an extraction that quietly returns nothing
# is the failure mode that makes _ha_seen equal _ha_total on an empty list.
# 🛑 COMPLETENESS ALONE WAS THE HOLE, AND IT STAYED OPEN BECAUSE IT PASSED.
# Until now this block asserted only that the refusal names EVERY derived gate.
# A refusal naming every derived gate AND SEVERAL THE HUB NO LONGER HAS scored
# a clean pass, because nothing here had an upper bound. That is not
# hypothetical: 1b3c9418 deleted the server.auth.mode=proxy gate and moved the
# seven IAP gates behind `if cfg.Auth.Mode == "proxy"`, and this assertion
# stayed green over a refusal that still named all of them under oauth. A
# one-sided parity check reports "8/8 named" in exactly the voice it would use
# if it were right.
#
# So there are now TWO assertions per arm and TWO arms:
#   completeness  every derived gate appears in the refusal          (was here)
#   exclusivity   the refusal's enumeration names NO key the walk did not (new)
# and the arms are the proxy shape and the oauth shape, which since 1b3c9418
# are different lists. A single-arm guard cannot see a per-mode error at all.
#
# THE PRE-REGISTERED EXTENTS ARE WRITTEN HERE, ABOVE THE RUN, AND NOT READ OFF
# THE OUTPUT AFTERWARDS: proxy >= 3 gates, oauth == 1 gate, proxy > oauth.
# gd-spec-rev's sharpening of gd-em's rule is the reason they are stated rather
# than inspected - "a positive arm you read after the fact is just another
# number on the screen". The oauth arm carries the absolute count per ruling
# (p1) because its one gate is the session secret — Cloud SQL landed in P2 and
# server.database.url is no longer a gate; if that number moves, a phase
# boundary moved and a human is supposed to be interrupted. The proxy arm keeps
# a floor rather than a pin because pinning it is the hand-maintained constant
# this whole block exists to delete.
_ha_gates="$CHART/hack/ha-gates.txt"
if [ ! -s "$_ha_gates" ]; then
  echo "HARNESS ERROR: $_ha_gates is missing or empty, so there is no derived gate list to check the refusal against. Regenerate with: go test ./cmd -run TestHelmChartHAGateWalk -update-chart-contract. NOTHING WAS MEASURED."
  echo "ASSERTIONS_EXECUTED=${executed}"
  exit 2
fi

# Reads one arm's CANON block out of the artifact into globals. Globals rather
# than a return value on purpose: every failure in here is a meta-failure, and
# `exit 2` inside a $( ) subshell would set a status nobody reads instead of
# stopping the run.
_ha_read_canon() {
  local _hdr="$1" _line _canon
  _canon="$(awk -v hdr="$_hdr" '
    index($0, hdr) == 1 { f = 1; next }
    f && $0 == "CANON BEGIN" { c = 1; next }
    c && $0 == "CANON END"   { exit }
    c { print }' "$_ha_gates")"
  # KEY lines name a settings key; PROSE lines are gates that name none, and
  # each one needs a deliberate decision here rather than a silent drop.
  _ha_want=""
  _ha_keys=""
  while IFS= read -r _line; do
    [ -n "$_line" ] || continue
    case "$_line" in
      "KEY   "*)
        _ha_want="$_ha_want${_line#KEY   }"$'\n'
        _ha_keys="$_ha_keys${_line#KEY   }"$'\n' ;;
      "PROSE "*)
        case "$_line" in
          *"durable session/signing secret"*) _ha_want="$_ha_want"'durable session/signing secret'$'\n' ;;
          *"supported IAP audience"*)         : ;;  # reached only by the malformed-audience arms
          *)
            echo "HARNESS ERROR: the derived gate list carries a prose gate this script does not recognise: ${_line#PROSE }"
            echo "               Add it to the case above, or decide in writing that the chart's refusal need not name it. NOTHING WAS MEASURED."
            echo "ASSERTIONS_EXECUTED=${executed}"
            exit 2 ;;
        esac ;;
    esac
  done <<EOF
$_canon
EOF
  _ha_total="$(printf '%s' "$_ha_want" | grep -cE .)"
}

# Cuts the gate enumeration out of the refusal and leaves the harvested keys in
# the global _ha_named. LOCATED BY CONTENT, and the anchors are two fixed
# phrases the message itself carries. Scoping matters: the refusal also says
# "The chart already satisfies server.hub.hub_id", and a key harvest over the
# whole message would read that as a gate and then report an intruder that is
# really a correct sentence.
#
# 🔴 [HISTORY 2026-08-17] A GLOBAL, NOT A RETURN VALUE, AND I LEARNED THAT THE
# EXPENSIVE WAY TWENTY LINES AFTER WRITING THE COMMENT THAT SAYS SO. This
# started out printing to stdout and being read with `_named="$(...)"`, with an
# `exit 2` on the vacuous path - and `exit 2` inside a command substitution
# exits the SUBSHELL. The meta-failure text became the function's return value,
# the run carried on with the error message as its corpus, and the arm reported
# a differ fault instead of the extractor fault that had actually occurred. I
# had documented this exact hazard on _ha_read_canon immediately above and then
# reintroduced it here, which is worth leaving on the record: THE COMMENT DID
# NOT PROTECT THE NEXT FUNCTION. The apparatus control caught it, on its first
# run, by breaking the anchor and reading the message rather than the exit code.
_ha_enumerated_keys() {
  local _msg="$1" _enum
  _enum="$(printf '%s' "$_msg" | tr '\n' ' ' | sed -n 's/.*measured in hub order by walking the real preflight: \(.*\)\. The chart already satisfies.*/\1/p')"
  if [ -z "$_enum" ]; then
    # 🛑 THE EXTRACTOR SHAPE GUARD. If the refusal is reworded and these anchors
    # stop matching, the harvest returns the empty set - which is a SUBSET of
    # every canon list and would make the exclusivity assertion pass on every
    # arm forever. An extractor that cannot extract takes the strict branch.
    echo "HARNESS ERROR: could not cut the gate enumeration out of the HA refusal. The anchors 'measured in hub order by walking the real preflight: ' and '. The chart already satisfies' no longer both appear in it, so the exclusivity check has nothing to check and would pass vacuously. Re-anchor it against the current message in _helpers.tpl. NOTHING WAS MEASURED."
    echo "ASSERTIONS_EXECUTED=${executed}"
    exit 2
  fi
  _ha_named="$(printf '%s' "$_enum" | grep -oE 'server\.[a-z0-9_]+(\.[a-z0-9_]+)*' | sort -u)"
}

# label / canon header / render args, with the extents pre-registered above.
_ha_arm() {
  local _label="$1" _hdr="$2" _min="$3" _max="$4"; shift 4
  local _out _seen=0 _missing="" _w _re _named _extra
  local _rwant _rclause _rgot _rmiss _rxtra _rctl_before _rctl_after

  _out="$(render "$@" 2>&1)"
  _ha_read_canon "$_hdr"

  # THE PROBE'S OWN CORPUS, ASSERTED. An awk header that matched nothing would
  # leave _ha_want empty, and an empty want list makes _seen equal _total on
  # zero gates and prints ok. That is the exact failure this block is here to
  # catch, reproduced one level up, so it is a meta-failure and not a pass.
  if [ "$_ha_total" -lt "$_min" ] || { [ "$_max" -gt 0 ] && [ "$_ha_total" -ne "$_max" ]; }; then
    echo "HARNESS ERROR: the ${_label} arm's derived gate list holds ${_ha_total} entries; this run was registered in advance for at least ${_min}$([ "$_max" -gt 0 ] && echo " and exactly ${_max}"). Either hack/ha-gates.txt moved and the pre-registered extent above must be re-decided by a human, or the awk header '${_hdr}' no longer matches a block in it. NOTHING WAS MEASURED."
    echo "ASSERTIONS_EXECUTED=${executed}"
    exit 2
  fi

  # --- completeness: every derived gate is named -----------------------------
  executed=$((executed + 1))
  while IFS= read -r _w; do
    [ -n "$_w" ] || continue
    # A bare substring test would count server.auth.transport as present
    # because server.auth.transport.mode is - a prefix passing for its own
    # extension. Match the key followed by something that cannot continue it.
    _re="$(printf '%s' "$_w" | sed 's/[.[\*^$]/\\&/g')"
    if printf '%s' "$_out" | grep -qE "${_re}([^.[:alnum:]_]|\$)"; then
      _seen=$((_seen + 1))
    else
      _missing="$_missing $_w"
    fi
  done <<EOF
$_ha_want
EOF
  if [ "$_seen" -eq "$_ha_total" ]; then
    echo "ok    ${_label}: the HA refusal names all ${_ha_total} unlandable gates the hub refuses on"
  else
    echo "FAIL  ${_label}: the HA refusal names ${_seen}/${_ha_total} unlandable gates; unnamed:${_missing}"
    echo "        got: $(printf '%s' "$_out" | tr '\n' ' ' | cut -c1-300)"
    failed=$((failed + 1))
  fi

  # --- exclusivity: no gate the walk did not derive --------------------------
  executed=$((executed + 1))
  if ! printf '%s' "$_ha_keys" | grep -qE .; then
    # PROSE-ONLY ARM. After Cloud SQL landed, the oauth arm has only the session
    # secret — a PROSE gate — and no KEY entries. A key harvest over a refusal
    # that names no keys returns the empty set, and the empty set is a subset of
    # every canon, so the exclusivity differ would pass vacuously. But the
    # COMPLETENESS check above already verified the refusal's content against the
    # walk, and a PROSE-only arm cannot name a settings key the walk did not
    # derive, so the exclusivity assertion is satisfied by construction. Report
    # it and move on rather than sending the key harvester into a corpus it
    # cannot read.
    echo "ok    ${_label}: the HA refusal's enumeration has no KEY gates on this arm (PROSE-only); exclusivity is satisfied by construction"
  else
    _ha_enumerated_keys "$_out"; _named="$_ha_named"
    # THE HARVEST'S OWN EXTENT, ASSERTED AGAINST AN INDEPENDENTLY-DERIVED FLOOR
    # AND NEVER AGAINST ZERO. Every arm's canon has at least one KEY, so the
    # refusal must name at least one. If the key regex stops matching - a rename
    # from server.* to hub.* would do it - the harvest is empty, the empty set is
    # a subset of every canon, and the exclusivity assertion below passes forever
    # on every arm. That is the same vacuity the extractor shape guard catches one
    # level up, and it needs catching at both levels because the anchors can match
    # while the harvest inside them does not.
    if [ "$(printf '%s\n' "$_named" | grep -cE .)" -lt 1 ]; then
      echo "HARNESS ERROR: the ${_label} arm's refusal enumeration was located but no settings key could be harvested out of it. The exclusivity check would compare the empty set against the canon and pass. NOTHING WAS MEASURED."
      echo "ASSERTIONS_EXECUTED=${executed}"
      exit 2
    fi
    _extra="$(printf '%s\n' "$_named" | grep -E . | comm -23 - <(printf '%s\n' "$_ha_keys" | grep -E . | sort -u) | tr '\n' ' ')"
    # THE POSITIVE CONTROL, IN THE SAME COMMAND, WITH ITS EXPECTED VALUE WRITTEN
    # DOWN BEFORE THE RUN: seeding one key the walk cannot have derived must move
    # the differ's answer by EXACTLY ONE. A comm that silently produced nothing -
    # unsorted input is enough to do that - would report "no intruders" in the
    # same words as a clean arm.
    #
    # 🔴 [HISTORY 2026-08-17] THE EXPECTED VALUE WAS FIRST WRITTEN AS THE ABSOLUTE
    # `1`, WHICH IS ONLY CORRECT WHILE THE SUBJECT IS CLEAN. I found that by
    # planting a real intruder in the oauth branch to check this arm goes red:
    # it did go red, on the CONTROL, reporting "the differ is not reading one of
    # its two inputs" about a differ that was working perfectly and had just found
    # the thing I planted. A CONTROL WHOSE EXPECTED VALUE DEPENDS ON THE SUBJECT
    # BEING CLEAN REPORTS AN APPARATUS FAULT EVERY TIME THE APPARATUS SUCCEEDS -
    # and it fails in the direction of exit 2, "nothing was measured", which is
    # the one outcome that tells a reader to disregard the finding. The delta is
    # the right expectation because it holds either way.
    _ctl_before="$(printf '%s\n' "$_extra" | tr ' ' '\n' | grep -cE .)"
    _ctl_after="$(printf '%s\nserver.zzz.control.probe\n' "$_named" | grep -E . | sort -u | comm -23 - <(printf '%s\n' "$_ha_keys" | grep -E . | sort -u) | grep -cE .)"
    if [ "$_ctl_after" -ne "$((_ctl_before + 1))" ]; then
      echo "HARNESS ERROR: the ${_label} exclusivity differ answered ${_ctl_before} on the real corpus and ${_ctl_after} on the same corpus seeded with one key the walk cannot have derived (server.zzz.control.probe). Seeding one intruder must move it by exactly one; it moved by $((_ctl_after - _ctl_before)). The differ is not reading one of its two inputs. NOTHING WAS MEASURED."
      echo "ASSERTIONS_EXECUTED=${executed}"
      exit 2
    fi
    if [ -z "${_extra// /}" ]; then
      echo "ok    ${_label}: the HA refusal's enumeration names no gate outside the ${_ha_total} the walk derived"
    else
      echo "FAIL  ${_label}: the HA refusal names gates the hub does not have on this arm:${_extra}"
      echo "        this is the 1b3c9418 shape - a refusal that is right about the outcome and wrong about the reason, which sends the operator to configure things that were never going to be checked."
      failed=$((failed + 1))
    fi
  fi

  # --- the removal condition names exactly the phases the walk attributes ----
  #
  # WHY THIS EXISTS, AND IT IS NOT THE SAME CHECK AS THE TWO ABOVE. Those read
  # the GATE LIST. This reads the sentence that tells the operator when the flag
  # goes away, which is a different claim about a different set, and it was
  # wrong on BOTH arms while the gate list was right on both.
  #
  #   oauth: the gate list says "the ingress/IAP phase lands nothing this
  #          release is waiting on" and the removal sentence, forty words later
  #          in the same string, said the flag waits on the ingress/IAP values.
  #          A single message contradicting itself.
  #   proxy: the gate list attributes a gate to the session-secret phase and the
  #          removal sentence named only Cloud SQL and ingress/IAP, so the flag
  #          would have been declared removable with a gate still standing.
  #
  # THE EXPECTED SET IS DERIVED FROM THE WALK, NOT READ OUT OF THE PROSE. If it
  # were harvested from the gate list it would agree with the gate list by
  # construction and could never disagree with it, which is the whole question.
  #
  # THE CLAUSE IS CUT POSITIVELY. Both arms MENTION the ingress/IAP phase - the
  # oauth one to say it does not apply - so a token scan over the whole sentence
  # would score oauth as naming it. Only the text between "when" and "landed" is
  # the list of phases being waited on.
  executed=$((executed + 1))
  _rwant=""
  printf '%s\n' "$_ha_keys" | grep -qx 'server\.database\.url'  && _rwant="${_rwant}Cloud SQL phase|"
  printf '%s\n' "$_ha_want" | grep -q  'durable session'        && _rwant="${_rwant}session-secret phase|"
  printf '%s\n' "$_ha_keys" | grep -qE '^server\.auth\.'        && _rwant="${_rwant}ingress/IAP phase|"
  _rwant="$(printf '%s' "$_rwant" | tr '|' '\n' | grep -E . | sort -u)"
  _rclause="$(printf '%s' "$_out" | tr '\n' ' ' | sed -n 's/.*stops being needed for auth\.mode [a-z]* when \(.*\) ha[sv]e\?\( both\| all\)\? landed.*/\1/p')"
  # THE EXTRACTION'S OWN SHAPE, ASSERTED. An anchor that stops matching yields
  # an empty clause, the empty set names no wrong phase, and the comparison
  # below reports agreement in the same words it would use for a correct
  # sentence.
  # NOTE: the sed pattern accepts "has landed" (single phase), "have both
  # landed" (two phases), and "have all landed" (three or more). After Cloud
  # SQL lands, the oauth arm waits on one phase and uses the singular.
  if [ -z "$_rclause" ] || [ -z "$_rwant" ]; then
    echo "HARNESS ERROR: the ${_label} arm's removal-condition clause cut to '${_rclause}' and its walk-derived phase set to '$(printf '%s' "$_rwant" | tr '\n' ' ')'. Either the refusal no longer says 'stops being needed for auth.mode X when ... have both/all landed', or the walk yielded no phase to attribute. An empty set agrees with every sentence. NOTHING WAS MEASURED."
    echo "ASSERTIONS_EXECUTED=${executed}"
    exit 2
  fi
  _rgot="$(printf '%s' "$_rclause" | grep -oE 'Cloud SQL phase|session-secret phase|ingress/IAP phase' | sort -u)"
  _rmiss="$(printf '%s\n' "$_rgot" | grep -E . | comm -13 - <(printf '%s\n' "$_rwant") | tr '\n' ' ')"
  _rxtra="$(printf '%s\n' "$_rgot" | grep -E . | comm -23 - <(printf '%s\n' "$_rwant") | tr '\n' ' ')"
  # THE POSITIVE CONTROL, DELTA-EXPECTED FOR THE REASON RECORDED ABOVE.
  #
  # 🔴 [HISTORY 2026-08-17] THE COUNT IS TAKEN IN LINES, AND THE FIRST VERSION
  # TOOK IT IN SPACE-SEPARATED TOKENS - `tr ' ' '\n'`, copied from the exclusivity
  # control forty lines up, where the items are settings KEYS and contain no
  # spaces. Phase names do: "ingress/IAP phase" tokenises to two. So on a CLEAN
  # subject the extras set is empty, 0 tokens, delta 1, and the control passed;
  # on a DIRTY subject it counted 2 where the truth was 1, the delta came out 0,
  # and the arm exited 2 "NOTHING WAS MEASURED" instead of exiting 1 with the
  # defect named. MEASURED: reverting the oauth removal sentence to the old
  # shared wording produced exactly that, and the arm that was supposed to catch
  # it reported an apparatus fault about itself instead.
  #
  # This is the SAME defect I recorded above and thought I had designed out. The
  # expected value was already a delta; what stayed coupled to the subject was
  # the COUNTING METHOD, which is only correct while the set it counts is empty.
  # A CONTROL CAN BE SUBJECT-COUPLED THROUGH ITS TOKENISER AND NOT THROUGH ITS
  # EXPECTATION, and a clean tree cannot tell the difference.
  _rctl_before="$(printf '%s\n' "$_rgot" | grep -E . | comm -23 - <(printf '%s\n' "$_rwant") | grep -cE .)"
  _rctl_after="$(printf '%s\nzzz control phase\n' "$_rgot" | grep -E . | sort -u | comm -23 - <(printf '%s\n' "$_rwant") | grep -cE .)"
  if [ "$_rctl_after" -ne "$((_rctl_before + 1))" ]; then
    echo "HARNESS ERROR: the ${_label} removal-condition differ answered ${_rctl_before} on the real clause and ${_rctl_after} with one phase planted that the walk cannot attribute. Planting one must move it by exactly one; it moved by $((_rctl_after - _rctl_before)). NOTHING WAS MEASURED."
    echo "ASSERTIONS_EXECUTED=${executed}"
    exit 2
  fi
  if [ -z "${_rmiss// /}" ] && [ -z "${_rxtra// /}" ]; then
    echo "ok    ${_label}: the removal condition waits on exactly the phases the walk attributes gates to ($(printf '%s' "$_rwant" | tr '\n' ',' | sed 's/,$//'))"
  else
    echo "FAIL  ${_label}: the removal condition and the walk disagree about which phases this flag waits on. Waits on a phase with no gate on this arm:${_rxtra:- none}. Has a gate but is not waited on:${_rmiss:- none}."
    echo "        an operator reading this is told the flag clears at the wrong time - too early leaves a gate standing, too late holds the flag through a phase that cannot affect it."
    failed=$((failed + 1))
  fi
}

_ha_arm "proxy" "===== settings.yaml [audience well-formed" 3 0 \
  --set database.driver=postgres --set storage.provider=gcs --set storage.bucket=b \
  "${CLOUDSQL_SET[@]}"
_ha_proxy_total="$_ha_total"

_ha_arm "oauth" "===== settings-oauth.yaml [audience well-formed" 1 1 \
  --set database.driver=postgres --set storage.provider=gcs --set storage.bucket=b \
  "${CLOUDSQL_SET[@]}" \
  --set auth.mode=oauth "${OAUTH_WEB[@]}"
_ha_oauth_total="$_ha_total"

# --- the differential itself -------------------------------------------------
# Two arms that agree prove nothing about a per-mode error; they are also what a
# chart with one hard-coded list produces. This asserts the arms DISAGREE, which
# is the whole content of 1b3c9418 as it reaches this chart.
executed=$((executed + 1))
if [ "$_ha_proxy_total" -gt "$_ha_oauth_total" ]; then
  echo "ok    the proxy arm walks more gates than the oauth arm (${_ha_proxy_total} > ${_ha_oauth_total}), so the two refusals are not one list rendered twice"
else
  echo "FAIL  the proxy and oauth arms derived ${_ha_proxy_total} and ${_ha_oauth_total} gates. Since 1b3c9418 the hub's IAP gates run only under auth.mode=proxy, so proxy must be the longer list. Equal counts mean the two arms are reading the same canon block and the per-mode branch in scion-hub.assertHAUnlanded is untested."
  failed=$((failed + 1))
fi

echo "== oauth mode requires a complete web client credential =="
# THIS SECTION REPLACES auth.acknowledgeOAuthUnlanded, and the replacement is not
# like-for-like. The acknowledgement asked the operator to CONFIRM the deployment
# would be unusable; these rows assert the chart REFUSES to render it. An
# acknowledgement is satisfiable by anyone in a hurry, and the thing it guarded
# against - a hub that starts, binds, passes /readyz and refuses every login - is
# not a thing an operator should be able to opt into by typing true.
#
# TWO LAYERS, so two rows per case, per this file's convention. The schema layer
# is what an operator hits; the template layer is what they hit with
# --skip-schema-validation, and it is the one that must hold, because config.extra
# reaches the settings document without passing through the schema at all.
#
# THE SCHEMA ROWS DISCRIMINATE, and that is asserted rather than assumed: the
# id-only row demands the message name clientSecret and the secret-only row
# demands clientId. A schema that listed both halves whatever was missing would
# satisfy a looser pair of substrings while telling the operator nothing.
reject "oauth, no credentials, schema layer"  "clientId" \
  --set auth.mode=oauth
reject "oauth, clientId only, schema layer"   "clientSecret" \
  --set auth.mode=oauth --set auth.oauth.web.google.clientId=rg-id
reject "oauth, clientSecret only, schema layer" "clientId" \
  --set auth.mode=oauth --set auth.oauth.web.google.clientSecret=rg-sec

reject "oauth, no credentials, template layer" "no complete OAuth web client credential is present" \
  --skip-schema-validation --set auth.mode=oauth
reject "oauth, clientId only, template layer"  "google (has client_id, missing client_secret)" \
  --skip-schema-validation --set auth.mode=oauth --set auth.oauth.web.google.clientId=rg-id
reject "oauth, clientSecret only, template layer" "google (has client_secret, missing client_id)" \
  --skip-schema-validation --set auth.mode=oauth --set auth.oauth.web.google.clientSecret=rg-sec

# CLI CREDENTIALS DO NOT SUBSTITUTE FOR WEB ONES, and this row is the reason the
# guard walks the web subtree specifically instead of asking "is server.oauth
# non-empty". The hub keys its login check by client type (pkg/hub/oauth.go:194),
# so a complete cli credential renders, validates, looks like configuration in
# the Secret, and satisfies no browser login. A guard written on presence rather
# than on client type passes this input.
reject "complete cli credential does not satisfy oauth mode" "no complete OAuth web client credential is present" \
  --set auth.mode=oauth \
  --set config.extra.server.oauth.cli.google.client_id=rg-cli-id \
  --set config.extra.server.oauth.cli.google.client_secret=rg-cli-secret

# THE SPELLING GUARD, WHICH IS THE ONE THAT CAUGHT ME. settings.yaml binds
# client_id/client_secret (V1OAuthProviderConfig, pkg/config/settings_v1.go:635);
# clientId/clientSecret is the SCION_SERVER_* environment mapper's spelling
# (pkg/config/hub_config.go:334). Measured both directions in
# harness/zz_p3_oauth_settings_probe_test.go: snake_case binds, camelCase leaves
# the field empty with no error from yaml.v3. I wrote the positive case in
# camelCase and expected it to pass.
#
# The first row carries a COMPLETE google credential as well, so the missing- and
# incomplete-credential guards above cannot be what refuses it. Without that, the
# row would go green on the wrong refusal and the spelling guard could be deleted
# without turning anything red.
reject "camelCase via config.extra, oauth mode" "server.oauth.web.github.clientId" \
  --set auth.mode=oauth "${OAUTH_WEB[@]}" \
  --set config.extra.server.oauth.web.github.clientId=rg-camel
# AND IT IS NOT GATED ON auth.mode. A misspelled credential is inert in proxy
# mode too - it just is not load-bearing there yet, which is exactly the state in
# which a wrong spelling gets committed and survives until the mode changes.
reject "camelCase via config.extra, proxy mode" "server.oauth.web.github.clientId" \
  --set auth.mode=proxy \
  --set config.extra.server.oauth.web.github.clientId=rg-camel

accept "oauth with a complete google web credential" --set auth.mode=oauth "${OAUTH_WEB[@]}"
accept "oauth with a complete github web credential" --set auth.mode=oauth \
  --set auth.oauth.web.github.clientId=rg-gh-id --set auth.oauth.web.github.clientSecret=rg-gh-secret
# config.extra IS A FIRST-CLASS WAY TO MEET THE REQUIREMENT, not a bypass of it.
# The guard reads the rendered document, so an operator who supplies
# server.oauth.web themselves has supplied it, and a guard that insisted on
# auth.oauth.web specifically would refuse a correct deployment.
accept "credentials supplied through config.extra in snake_case" --set auth.mode=oauth \
  --set config.extra.server.oauth.web.google.client_id=rg-extra-id \
  --set config.extra.server.oauth.web.google.client_secret=rg-extra-secret
# WITH AN EXTERNAL SETTINGS SECRET THE CHART RENDERS NO SETTINGS DOCUMENT, so
# there is nothing to inspect and nothing to refuse. Asserting this keeps the
# guard from growing into a claim about a file the chart cannot see.
accept "oauth with no credentials but an external settings Secret" \
  --set auth.mode=oauth --set config.existingSecret=operator-owned

# THE POSITIVE TWIN OF THE SPELLING GUARD: the chart's own render must land on
# the side of the guard it enforces. A chart that refused camelCase from
# config.extra while emitting camelCase itself would pass every row above -
# the guard runs on the merged document, so it would refuse its own output, and
# the accept rows would be the ones to fail... unless the guard were ever
# narrowed to config.extra only. This asserts the emitted spelling directly.
executed=$((executed + 1))
_oa="$(render --set auth.mode=oauth "${OAUTH_WEB[@]}")"
if [ -z "$_oa" ] || ! printf '%s\n' "$_oa" | grep -q '^kind: Secret$'; then
  echo "FAIL  rendered oauth credentials: no Secret in the output, so nothing was inspected"
  failed=$((failed + 1))
elif printf '%s\n' "$_oa" | grep -qE '^ +client(Id|Secret):'; then
  echo "FAIL  rendered oauth credentials: the chart emits camelCase, which binds nothing in settings.yaml"
  failed=$((failed + 1))
elif printf '%s\n' "$_oa" | grep -q '^ *client_id: rg-web-google-id$' \
  && printf '%s\n' "$_oa" | grep -q '^ *client_secret: rg-web-google-secret$'; then
  echo "ok    the chart emits client_id/client_secret, the spelling settings.yaml binds"
else
  echo "FAIL  rendered oauth credentials: neither spelling reached the settings document"
  echo "        got: $(printf '%s' "$_oa" | grep -c .) lines, no client_id/client_secret"
  failed=$((failed + 1))
fi
unset _oa

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
elif ! printf '%s\n' "$_a" | grep -qE '^kind: Deployment$' \
  || ! printf '%s\n' "$_b" | grep -qE '^kind: Deployment$'; then
  echo "FAIL  install/upgrade comparison: helm exited 0 but the output carries no Deployment, so the compared strings are not the manifest"
  failed=$((failed + 1))
elif [ "$(printf '%s' "$_a" | sha256sum)" = "$(printf '%s' "$_b" | sha256sum)" ]; then
  echo "ok    render is identical for install and upgrade"
else
  echo "FAIL  render differs between install and upgrade"; failed=$((failed + 1))
fi
for rel in t other-release t2; do
  executed=$((executed + 1))
  got="$("$HELM" template "$rel" "$CHART" "${BASE[@]}" | grep -oE 'scion\.io/hub-id: .*' | sort -u)"
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
