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

EXPECTED_TOTAL=168   # 77 (upstream) + 53 (credential guard fixes: F2 name-axis, multi-line-leaf, anchor-class, map-KEY, URL-userinfo F3, readyz probe-path) - 13 (the assertHAUnlanded block and its ha-gates-derived refusal checks, removed with the acknowledgement) + 33 (HA routes under proxy, auth.proxy.iap.audience, auth.proxy.provider, auth.transport) + 1 (the httpGet path extractor's self-test row) + 1 (HA oidcAudience of only slashes) + 7 (secrets.backend: five refusals, one acceptance, one rendered-shape check) + 9 (agents.imageRegistry for the in-process broker: three refusals, six acceptances).
CHART="${CHART:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
HELM="${HELM:-helm}"
# auth.sessionSecret became REQUIRED in the session-secret phase, and it is here for the same
# reason hub.baseUrl is: scion-hub.assertSessionSecret fails the render without it, so every
# BASE render would return an error string instead of manifests and every check below would
# accuse the chart of a fault it does not have. The chart will not default it - a generated
# secret rotates on every helm upgrade, invalidating every session and the JWT signing key.
BASE=(--set image.repository=r --set agents.imageRegistry=example.invalid/agents --set hub.hubId=ci-minimal --set hub.baseUrl=https://ci-minimal.example.invalid --set auth.sessionSecret=harness-not-a-real-secret --set auth.proxy.iap.audience=/projects/123456789012/locations/us-central1/services/probe-tests)   # hub.baseUrl became REQUIRED in Phase 1; see the arm below.

# A COMPLETE WEB CLIENT CREDENTIAL, for the rows that need auth.mode=oauth to
# render at all. Not folded into BASE, because several rows below exist
# specifically to assert that oauth WITHOUT this is refused - putting it in BASE
# would make those rows unwriteable, and worse, would make them look written.
# It replaces --set auth.acknowledgeOAuthUnlanded=true, which is what these rows
# carried while the credentials had no channel.
# The empty audience undoes BASE's: the chart refuses an audience under oauth,
# where it would be silently discarded.
OAUTH_WEB=(--set auth.proxy.iap.audience= --set auth.oauth.web.google.clientId=rg-web-google-id --set auth.oauth.web.google.clientSecret=rg-web-google-secret)

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

echo "== NAME axis: the separator must not decide whether the guard runs =="
# F2. Every one of these was ACCEPTED, and the word list already contained the
# word that should have caught each. The match was anchored to the hyphen alone,
# so a dotted or camelCase convention walked straight through - and dotted flag
# names are exactly where "password" turns up in the wild. The fix normalises the
# separator; the list is still nine words long. Lengthening it would have been
# the same defect with more entries.
reject "--admin.token"            "names credential material" --set hub.args[0]=--admin.token=hunter2
reject "--admin:token"            "names credential material" --set hub.args[0]=--admin:token=hunter2
reject "--adminToken (camelCase)" "names credential material" --set hub.args[0]=--adminToken=hunter2
reject "--adminAPIKey (acronym)"  "names credential material" --set hub.args[0]=--adminAPIKey=hunter2
reject "--spring.datasource.password" "names credential material" --set hub.args[0]=--spring.datasource.password=hunter2
reject "--auth/token"             "names credential material" --set hub.args[0]=--auth/token=hunter2
reject "--db.credential"          "names credential material" --set hub.args[0]=--db.credential=x
# The camelCase split happens before lowering, so it is destroyed by any caller
# that pre-lowers. It WAS: the flag path passed an already-lowered name while the
# positional path passed a raw one, so --adminToken was rejected as a positional
# and accepted as a flag. Both paths are pinned here; a fix to one is not a fix.
reject "adminToken as a positional" "names credential material" --set hub.args[0]=adminToken=hunter2
# POSITIVE TWINS FOR THE NEW SEPARATORS. The trailing-segment anchor is what makes
# the list usable at all: a flag NAMED for a credential carries one, a flag named
# ABOUT one does not. Matching every segment instead of the last would reject all
# of these, and they are the reason the fix is a better tokeniser and not a
# broader match.
accept "--tokenTtl"               --set hub.args[0]=--tokenTtl=5m
accept "--maxTokens"              --set hub.args[0]=--maxTokens=10
accept "--secret.manager.project" --set hub.args[0]=--secret.manager.project=p
accept "--passwordMinLength"      --set hub.args[0]=--passwordMinLength=8

echo "== credential guard, VALUE axis =="
reject "DSN with userinfo"  "embeds credentials in a URL" --set 'hub.args[0]=--upstream=postgres://scion:hunter2@10.0.0.1/scion'

echo "== F3: a / in the password must not silence the URL guard =="
# EVERY ONE OF THESE RENDERED CLEAN before F3, with the password on argv. The old
# password class was [^/@[:space:]]+, so a slash left the regex no path to the
# terminating @ and the guard went quiet. The control below - the same password
# with the slash removed - was refused, which is what made this anti-correlated
# with the mistake: the operator who percent-encodes correctly was protected and
# the one carrying a raw slash was not.
#
# THESE ROWS ARE THE COST OF ANY FUTURE NARROWING OF THAT PATTERN. The disclosed
# residual below is a standing temptation to tighten it; if you do, run these
# first and count how many go quiet.
reject "pw with a slash"        "embeds credentials in a URL" --set 'hub.args[0]=--upstream=postgres://u:a/b@10.0.0.1/scion'
reject "pw slash, empty user"   "embeds credentials in a URL" --set 'hub.args[0]=--upstream=redis://:S3cr3t/Xy@10.0.0.1:6379'
reject "pw with two slashes"    "embeds credentials in a URL" --set 'hub.args[0]=--upstream=mysql://u:p/w/d@host:3306/db'
# THE COMMA IS ESCAPED BECAUSE --set SPLITS ON IT, and the first version of this
# row proved the point the hard way: helm never planted the value at all, failing
# with `key "h2:27017/db" has no value`. That is a REJECTION, so an exit-code-only
# assertion would have gone green on an arm that never reached the chart.
#
# So this row asserts the REDACTED DSN rather than the message wording. It is the
# stronger claim: it can only appear if the comma survived --set, if the URL guard
# was the guard that fired, and if the redactor rewrote the userinfo. If a future
# helm changes its escaping, this goes red instead of quietly degrading into a
# single-host arm that still passes.
reject "mongodb replica set"    'mongodb://REDACTED@h1:27017,h2:27017/db' --set 'hub.args[0]=--upstream=mongodb://u:p/w@h1:27017\,h2:27017/db'
reject "amqp vhost"             "embeds credentials in a URL" --set 'hub.args[0]=--upstream=amqp://u:a/b@rabbit:5672/vhost'
reject "pw ends in a slash"     "embeds credentials in a URL" --set 'hub.args[0]=--upstream=postgres://u:abc/@10.0.0.1/scion'
reject "pw is a single slash"   "embeds credentials in a URL" --set 'hub.args[0]=--upstream=postgres://u:/@10.0.0.1/scion'

# THE OTHER DIRECTION, WHICH IS THE HALF THAT KEEPS GETTING SKIPPED. Widening the
# password class to admit "/" is only safe if the authority-terminator tail holds
# these three quiet. Without that tail, "user:pass@" matched anywhere inside a
# query string and each of these was refused.
accept "DSN with no userinfo"     --set 'hub.args[0]=--upstream=postgres://10.0.0.1:5432/scion'
accept "email in a query string"  --set 'hub.args[0]=--upstream=https://api.example.com/v1/users?filter=a:b@c.com'
accept "explicit port, @ in query" --set 'hub.args[0]=--upstream=https://example.com:8080/path?e=a@b'

# 🔴 KNOWN FALSE POSITIVE, PINNED DELIBERATELY. THIS ROW ASSERTS A DEFECT.
# A URL with an explicit port AND an @ in its path is refused: the port reads as
# the password and the path segment as the host, and no URI grammar distinguishes
# them from userinfo. The pre-F3 pattern accepted this, so it is a real regression
# on this one shape, taken knowingly - 7 silent credential leaks traded for 1 loud
# refusal of an unusual URL.
#
# DO NOT DELETE THIS ROW TO MAKE A FIX GO GREEN. If you narrow the pattern and
# reclaim this arm, this row goes red - that is the row WORKING. Flip it to
# accept() and delete this comment, having first re-run the seven fire rows above.
reject "KNOWN FP: port + @ in path" "embeds credentials in a URL" --set 'hub.args[0]=--upstream=https://example.com:8080/a@b/c'
reject "ghp_ prefix"        "shape of a credential"       --set 'hub.args[0]=--x=ghp_AAAAAAAAAAAAAAAAAAAA'
reject "sk- prefix"         "shape of a credential"       --set 'hub.args[0]=--x=sk-proj-A1b2C3d4E5f6G7h8I9j0'
# 🔴 THE ROW BELOW WAS AN accept FOR MOST OF TODAY AND THAT WAS MY DEFECT, NOT A
# TRADE. In 215a6f85 I added length floors to the credential alternation, which
# made "sk-9A" render clean, and I INVERTED THIS ROW -- a production reject row
# inherited from main, where its witness read "sk-AAAAAAAAAAAA" -- so that my own
# suite would go green on the change. It did. That is precisely the problem: the
# suite could no longer show me the casualty, because I had deleted the row that
# WAS the casualty. Restored to reject by me, gd-p0-dev, 2026-08-17.
#
# RATIFIED BY gd-em AND gke-deploy-lead AFTER I DISCLOSED IT: NOTHING IN THIS
# BRANCH MAY WEAKEN A PRODUCTION reject ROW, and any diff flipping an
# accept/reject verb must list the row in the PR description with a reason.
reject "short sk- string"   "shape of a credential"       --set 'hub.args[0]=--x=sk-9A'
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

echo "== VALUE axis: ONE SCALAR LEAF CAN BE MULTI-LINE =="
# Every other row on this axis plants a BARE scalar, which sits at the start of
# the string and so satisfies "^" for free. That made the whole axis look anchored
# when it was not: a YAML block scalar - which is what annotations carrying an
# embedded config blob look like - hid any prefixed token behind a leading line.
# Worst on "-----BEGIN ", since PEM is multi-line BY DEFINITION.
#
# These use a values FILE, not --set: --set cannot express a newline in a scalar,
# and a fixture that silently plants one line is a row that measures nothing.
MLTMP="$(mktemp -d)"; trap 'rm -rf "$MLTMP"' EXIT
ml() { # ml <file> <block scalar body>
  { printf 'hub:\n  hubId: fixed-id\n  podAnnotations:\n    example.com/blob: |\n'
    printf '%b\n' "$2"; } > "$MLTMP/$1.yaml"
  # PERTURBATION ASSERT: prove the leaf really is multi-line before reading a result.
  local n; n=$(awk '/^      /{c++} END{print c+0}' "$MLTMP/$1.yaml")
  [ "$n" -ge 2 ] || { echo "FAIL  fixture $1 is not multi-line (${n} line(s)) - row measures nothing"; failed=$((failed + 1)); return 1; }
}
ml pem   "      owner: platform-eng\n      -----BEGIN RSA PRIVATE KEY-----\n      MIIEowIBAAKCAQEA\n      -----END RSA PRIVATE KEY-----" \
  && reject "PEM block on a non-first line"  "shape of a credential" -f "$MLTMP/pem.yaml"
ml tokln "      owner: platform-eng\n      ghp_A1b2C3d4E5f6G7h8\n      tier: gold" \
  && reject "R3: credential alone on LINE 2 of a scalar" "shape of a credential" -f "$MLTMP/tokln.yaml"
# These two are why (?m) alone was not enough: the credential is MID-line, after
# "token: ", so a line-start anchor never reaches it. They need the widened class.
ml tokkv "      owner: platform-eng\n      token: ghp_A1b2C3d4E5f6G7h8\n      tier: gold" \
  && reject "token after 'token: ' in a leaf" "shape of a credential" -f "$MLTMP/tokkv.yaml"
ml pemkv "      owner: platform-eng\n      key: -----BEGIN RSA PRIVATE KEY-----" \
  && reject "PEM after 'key: ' in a leaf"     "shape of a credential" -f "$MLTMP/pemkv.yaml"
# R-b: PEM INDENTED, which is how a PEM actually appears when someone pastes one
# under a key in a block scalar. The extra two spaces survive the block's own
# indent stripping, so the header is preceded by whitespace rather than sitting
# at column 0 - a configuration no other row here plants.
ml pemind "      cert:\n        -----BEGIN RSA PRIVATE KEY-----\n        MIIEowIBAAKCAQEA" \
  && reject "PEM indented under a key"        "shape of a credential" -f "$MLTMP/pemind.yaml"

echo "== ANCHOR CLASS: the credential is not at offset 0, and the leaf is ONE LINE =="
# THE DEFECT WAS NEVER MULTI-LINE. "(^|=)" required the credential at offset 0 or
# immediately after "="; ANY other preceding byte silenced the check. Every row
# above plants a bare scalar or a "--flag=" form, so the whole axis sat on the one
# side of the defect where the anchor happens to hold, and reported green.
#
# These rows are SINGLE-LINE on purpose. A multi-line witness makes this look like
# a narrower bug than it is and invites a line-start fix, which closes one of the
# seven known arms and leaves the likeliest one - a plain "token: <secret>" pair -
# still silent.
sl() { # sl <file> <single-line leaf value>, planted as a QUOTED scalar
  { printf 'hub:\n  hubId: fixed-id\n  podAnnotations:\n'
    printf "    example.com/blob: %s\n" "$2"; } > "$MLTMP/$1.yaml"
  # PERTURBATION ASSERT: one line, and the value really reached the file.
  local n; n=$(awk '/example.com\/blob:/{c++} END{print c+0}' "$MLTMP/$1.yaml")
  [ "$n" -eq 1 ] || { echo "FAIL  fixture $1 did not plant a single leaf (${n})"; failed=$((failed + 1)); return 1; }
}
sl a3   "'token: ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8'" \
  && reject "single-line 'token: <tok>'"      "shape of a credential" -f "$MLTMP/a3.yaml"
sl a4   "'Authorization: Bearer ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8'" \
  && reject "single-line 'Bearer <tok>'"      "shape of a credential" -f "$MLTMP/a4.yaml"
sl a5   "' ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8'" \
  && reject "ONE leading space before a token" "shape of a credential" -f "$MLTMP/a5.yaml"
# A quoted credential inside a JSON blob. The preceding byte is a double quote,
# which is neither "=" nor whitespace nor a colon - the configuration that a
# hand-listed set of separators misses and a negated token class catches.
sl aq   "'{\"tok\":\"ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8\"}'" \
  && reject "token after a double quote"      "shape of a credential" -f "$MLTMP/aq.yaml"

echo "== the walk checks BOTH SIDES of the colon =="
# A credential written as an annotation KEY rendered clean: the walk recursed on
# values and never inspected names. A key reaches exactly the same readers as a
# value.
{ printf 'hub:\n  hubId: fixed-id\n  podAnnotations:\n'
  printf '    ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8: mine\n'; } > "$MLTMP/k1.yaml"
reject "credential as an annotation KEY" "shape of a credential" -f "$MLTMP/k1.yaml"
# AND THE MESSAGE MUST NOT PRINT THE KEY IT CAUGHT. Everywhere else the source
# path interpolates the key so the operator can find the leaf; here the key IS
# the secret, so the path stops at the parent. Asserted, not assumed.
executed=$((executed + 1))
_kout="$(render -f "$MLTMP/k1.yaml")"
case "$_kout" in
  *ghp_A1b2C3d4E5f6G7h8*) echo "FAIL  the key-check message printed the credential it caught"
                          failed=$((failed + 1)) ;;
  *"a map key"*)          echo "ok    key-check message locates the leaf without printing it" ;;
  *) echo "FAIL  key-check message did not name the surface"; failed=$((failed + 1)) ;;
esac
# R2 PAIRING (gd-p3-rev, ratified by gd-em 14:20Z at CRITICAL). The key-position
# reject above only means something beside a VALUE-position row planted in THE
# SAME MAP. Without it, "the key axis fires" and "this fixture shape fires for
# some other reason" are the same observation. Same annotation map, same
# credential, other side of the colon.
{ printf 'hub:\n  hubId: fixed-id\n  podAnnotations:\n'
  printf '    example.com/tok: ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8\n'; } > "$MLTMP/k2.yaml"
reject "same credential in VALUE position" "shape of a credential" -f "$MLTMP/k2.yaml"
# ⚠ THE PATH IN THESE MESSAGES DOES NOT ROUND-TRIP, and that is known and not
# fixed here (gd-em 14:27Z §5). "values.hub.nodeSelector.cloud.google.com/pool"
# is ambiguous between a dotted KEY and four nested maps, and dotted keys are
# ordinary in this chart -- "cloud.google.com/gke-spot" is in its own values.
# Still far better than no path. Not a defect this commit undertakes to repair.
# KEYS GET THE VALUE AXIS, NOT THE NAME AXIS, AND THIS IS THE ROW THAT PINS IT.
# The name axis hard-fails on an underscore because pflag rejects one in a flag
# name. Annotation keys are not flags, and underscores are legal in them, so
# routing keys through the name axis would reject an ordinary label.
accept "ordinary key with an underscore" \
  --set 'hub.podAnnotations.app_version=1.2'

echo "== THE READINESS PATH IS /readyz, AND NOTHING ELSE =="
# R6 (gd-p3-rev's sweep, made gd-em's own at 14:20Z). "/readyz" appeared ZERO
# times across all six committed test scripts while two probes in deployment.yaml
# depend on it. It is a hard constraint on this project -- the path is /readyz,
# NOT /api/v1/readyz and NOT the legacy health-z path -- and until the rows below it was protected
# by nothing at all. A constraint that lives only in a brief is not a constraint.
#
# TWO ASSERTIONS, POSITIVE AND EXHAUSTIVE, BECAUSE EITHER ALONE IS WEAK. The
# count row alone passes if a second probe is added pointing somewhere wrong and
# a correct one is deleted. The no-other-path row alone passes VACUOUSLY if the
# probes stop rendering altogether -- zero paths is zero wrong paths. Together
# they pin "exactly two, and both of them /readyz".
# STRUCTURAL, NOT BY VALUE SHAPE. Only a `path:` that is a direct child of an
# `httpGet:` mapping is a probe (or lifecycle-hook) path, so that is the only
# `path:` this section reads. Volume `items` entries (the settings Secret
# projects `path: settings.yaml`) and hostPath volumes are skipped because they
# are not under httpGet, whatever their value looks like. Both quote styles are
# unwrapped, and a relative value is read with a leading slash added, because
# Kubernetes does not require one on an httpGet path and the kubelet adds it
# when it builds the URL. Flow-style `httpGet: {path: ...}` is not parsed; the
# chart renders block style, and the self-test row below pins what is parsed.
_httpget_paths() {
  awk '
    function ind(s) { match(s, /^ */); return RLENGTH }
    /^[[:space:]]*$/ { next }
    inblk && ind($0) <= hind { inblk = 0 }
    inblk && $0 ~ /^[[:space:]]*(- )?path:/ {
      v = $0
      sub(/^[[:space:]]*(- )?path:[[:space:]]*/, "", v)
      sub(/[[:space:]]+#.*$/, "", v); sub(/[[:space:]]+$/, "", v)
      if (v ~ /^".*"$/ || v ~ /^\047.*\047$/) v = substr(v, 2, length(v) - 2)
      if (substr(v, 1, 1) != "/") v = "/" v
      print v
    }
    /^[[:space:]]*(- )?httpGet:[[:space:]]*$/ { inblk = 1; hind = ind($0) }
  '
}
# THE EXTRACTOR'S OWN CHECK. A synthetic manifest with one probe path in each
# form the extractor must read, and two non-probe `path:` lines it must skip.
executed=$((executed + 1))
_synthetic='      livenessProbe:
        httpGet:
          path: "/dq"
          port: http
      startupProbe:
        httpGet:
          path: \047/sq\047
      readinessProbe:
        httpGet:
          path: relative
          httpHeaders:
            - name: X
              value: y
      volumes:
        - name: s
          secret:
            items:
              - key: settings.yaml
                path: settings.yaml
        - name: h
          hostPath:
            path: /var/data'
_synthetic="$(printf '%b' "$_synthetic")"
_synthgot="$(printf '%s\n' "$_synthetic" | _httpget_paths | tr '\n' ' ')"
if [ "$_synthgot" = "/dq /sq /relative " ]; then
  echo "ok    the httpGet path extractor reads double-quoted, single-quoted and relative paths and skips volume paths"
else
  echo "FAIL  the httpGet path extractor read [${_synthgot}] from the synthetic manifest; expected [/dq /sq /relative ]"
  failed=$((failed + 1))
fi
executed=$((executed + 1))
_probeout="$("$HELM" template t "$CHART" "${BASE[@]}" -s templates/deployment.yaml 2>&1)"
_nready=$(printf '%s\n' "$_probeout" | _httpget_paths | grep -cx '/readyz' || true)
if [ "$_nready" -eq 2 ]; then
  echo "ok    both probes point at /readyz (exactly 2)"
else
  echo "FAIL  expected exactly 2 httpGet paths of /readyz in the Deployment, got ${_nready}"
  failed=$((failed + 1))
fi
# Whole chart, not just the Deployment: an httpGet path introduced in any other
# template is in scope for this constraint too.
executed=$((executed + 1))
_nother=$(render | _httpget_paths | grep -cvx '/readyz' || true)
if [ "$_nother" -eq 0 ]; then
  echo "ok    no probe path other than /readyz renders anywhere in the chart"
else
  echo "FAIL  ${_nother} probe path(s) other than /readyz render:"
  render | _httpget_paths | grep -vx '/readyz' | sed 's/^/        /'
  failed=$((failed + 1))
fi

echo "== THE FALSE-POSITIVE SET: prose this pattern REFUSES, pinned as its cost =="
# 🔴 REWRITTEN BY ME, gd-p0-dev, 2026-08-17, REPLACING A COMMENT I ALSO WROTE
# EARLIER TODAY (215a6f85) THAT SAID THE OPPOSITE. The old block introduced these
# as negative controls proving that length floors kept ordinary English
# renderable. The floors are gone -- no floor can separate a 40-character real
# key from a 40-character legitimate string, which is a closed-form result and
# not a tuning failure -- so these values are REFUSED now, and the rows say so.
#
# FLIP, DO NOT DELETE. Every reject row below is a KNOWN FALSE POSITIVE. If you
# narrow the pattern and reclaim one, its row goes red: THAT IS THE ROW WORKING,
# and the repair is to flip it back to accept() with a note, NEVER to delete it.
# A deleted FP row is a cost that has stopped being counted.
#
# FRAME, BECAUSE SIX IS NOT A COVERAGE FIGURE: these are 6 rows from a corpus I
# built by hand. The production aperture is 32 distinct legitimate values over 19
# rendered surfaces (gd-p3-rev, 14:08Z, measured with real helm). THIS IS A
# SAMPLE AND MUST NOT BE READ AS THE FALSE-POSITIVE SET. It exists so the cost is
# visible in the suite at all, not so that it is quantified here.
#
# AND THE COST DID NOT RISE. Against the pattern shipping in main the FP count is
# IDENTICAL -- 608/1368, 32 distinct, before and after -- because dropping "(?i)"
# removes two while the anchor fix adds two. Membership changes; size does not.
# The two rows immediately below are the OUT side of that swap, and they are what
# make it a swap rather than an assertion.
sl h1 "'SK-Hynix-pool'"   && accept "SK-Hynix-pool (an FP under (?i), reclaimed)"  -f "$MLTMP/h1.yaml"
sl h2 "'akia12345678'"    && accept "akia12345678 (an FP under (?i), reclaimed)"   -f "$MLTMP/h2.yaml"
# The IN side. Both are the ANCHOR's price, NOT the prefix list's -- the prefixes
# below are byte-identical to main's. Anyone reaching for the alternation to
# reclaim these is editing the wrong half of the expression.
sl p1 "'sk-learn pipeline'"       && reject "KNOWN FP, FLIP DONT DELETE: prose 'sk-learn pipeline'" "shape of a credential" -f "$MLTMP/p1.yaml"
sl p2 "'xoxb-team'"               && reject "KNOWN FP, FLIP DONT DELETE: prose 'xoxb-team'"         "shape of a credential" -f "$MLTMP/p2.yaml"
sl p3 "'my sk-8 skateboard deck'" && reject "KNOWN FP, FLIP DONT DELETE: prose 'sk-8 skateboard'"   "shape of a credential" -f "$MLTMP/p3.yaml"
sl p4 "'avoid xoxb-style naming'" && reject "KNOWN FP, FLIP DONT DELETE: prose 'xoxb-style naming'" "shape of a credential" -f "$MLTMP/p4.yaml"
sl p5 "'/opt/sk-tools/bin'"       && reject "KNOWN FP, FLIP DONT DELETE: path '/opt/sk-tools/bin'"  "shape of a credential" -f "$MLTMP/p5.yaml"
ml prose "      note: the sk-8 connector is documented\n      tier: gold" \
  && reject "KNOWN FP, FLIP DONT DELETE: prose 'sk-8' in a leaf" "shape of a credential" -f "$MLTMP/prose.yaml"
# 🛑 THIS ONE DID NOT FLIP, AND IT IS THE ONLY ROW IN THE BLOCK THAT
# DISCRIMINATES. "task-force" contains "sk-f", but the byte in front of it is
# "a", which IS in the token class, so the anchor never engages. Measured, not
# reasoned: of the seven prose rows this block used to carry, six refuse and this
# one still renders.
# IF THIS ROW EVER GOES RED, THE ANCHOR CLASS HAS BEEN WIDENED INTO A PLAIN
# SUBSTRING SEARCH and every English word containing a prefix becomes a refusal.
# The six rows above cannot detect that -- they are red either way.
ml task  "      note: owned by a task-force team\n      tier: gold" \
  && accept "mid-word sk- in 'task-force' is NOT matched" -f "$MLTMP/task.yaml"
ml clean "      owner: platform-eng\n      tier: gold" \
  && accept "credential-free multi-line leaf"   -f "$MLTMP/clean.yaml"

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

# THE SAME ASSERTION WITH A SLASH IN THE PASSWORD, AND IT IS NOT A DUPLICATE.
# Detection and redaction are TWO DIFFERENT REGEXES in _helpers.tpl. The detector
# carries an authority-terminator tail; the redactor deliberately does not, which
# makes the redactor a strict SUPERSET of the detector. That relationship is the
# only thing guaranteeing every caught string is redactable.
#
# If someone edits one pattern and not the other - the obvious maintenance
# mistake, since they sit on adjacent lines and look interchangeable -
# regexReplaceAll matches nothing, returns the value UNCHANGED, and %q prints the
# whole DSN INCLUDING THE PASSWORD into the CI log. The failure is invisible from
# the exit code: the guard still refuses, still exits 1, still prints a
# correct-sounding message. Only the password's presence in it gives it away.
#
# The slash matters: it is the character the detector was widened for, so this is
# the arm that goes red first if the two patterns drift apart.
executed=$((executed + 1))
_out="$(render --set 'hub.args[0]=--upstream=postgres://scion:S3cr3t/Xy@10.0.0.1/scion')"
case "$_out" in
  *"embeds credentials in a URL"*)
    case "$_out" in
      *"S3cr3t/Xy"*) echo "FAIL  slash-bearing password LEAKED into the guard's own error message"
                     echo "        the detector and the redactor have drifted apart in _helpers.tpl"
                     failed=$((failed + 1)) ;;
      *) echo "ok    slash-bearing credential redacted in the failure message" ;;
    esac ;;
  *) echo "FAIL  slash redaction: the guard did not fire, so redaction was never tested"
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
# THE EMBEDDED-WHITESPACE GUARD IS GATED ON hasPrefix "-" (_helpers.tpl, the
# "contains whitespace" refusal). A POSITIONAL carrying whitespace is not covered
# by it at all. This row pins that boundary so nobody reads the row above as
# "argv rejects whitespace" - it rejects whitespace in FLAGS.
accept "positional with spaces (whitespace guard is flag-only)" \
  --set 'hub.args[0]=plain positional with spaces'

echo "== argv, ANCHOR CLASS, attributed BY MESSAGE TEXT =="
# WHY ATTRIBUTION AND NOT PASS/FAIL. Several prefixed argv forms are refused today
# by the WHITESPACE guard rather than the credential guard - a different guard,
# with a different purpose, that the operator satisfies by not typing a space. A
# harness recording only "did it fail" books those as credential coverage and
# concludes argv is guarded. These rows name the message, so the whitespace axis
# cannot take credit for a catch the value axis did not make.
#
# THE CLEANEST DISCRIMINATOR IS A POSITIONAL, AND THE REASON IS NOT WHAT IT LOOKS
# LIKE. "cert:-----BEGIN RSA PRIVATE KEY-----" was reported as escaping because it
# contains no whitespace. It contains four spaces. It escapes because it does not
# begin with "-", so the whitespace guard above never examines it, which leaves
# the value axis as the only thing standing between a PEM and the manifest.
reject "positional 'cert:<PEM>' (value axis, not whitespace)" \
  "shape of a credential" --set 'hub.args[0]=cert:-----BEGIN RSA PRIVATE KEY-----'
reject "positional 'Authorization: Bearer <tok>'" \
  "shape of a credential" --set 'hub.args[0]=Authorization: Bearer ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8'

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

echo "== HA routes under auth.mode proxy: the chart renders what the preflight reads =="
# The routes are isHADeployment's (cmd/server_foreground.go), carried in
# scion-hub.haRoutes; the hub's own tripwire test (cmd/server_ha_preflight_test.go)
# asserts the same three. Under auth.mode proxy the preflight then checks the
# IAP audience's shape and the transport block, and the chart refuses an HA
# render that would fail it. Under oauth those gates are not reached.
#
# EACH NEGATIVE ASSERTS WHICH ROUTE FIRED. The routes overlap - postgres implies
# gcs by schema rule, and gcs plus proxy is route 3 - so each row names its own.
#
# POSTGRES NEEDS THE CLOUD SQL VALUES. --set database.driver=postgres on its own
# is refused because the chart reaches Postgres only through the Cloud SQL Auth
# Proxy; without these the rows below would measure that refusal instead.
CLOUDSQL_SET=(
  --set cloudsql.enabled=true
  --set cloudsql.instanceConnectionName=my-project:us-central1:db-1
  --set database.auth=iam
  --set database.name=scion
  --set serviceAccount.gcpServiceAccount=hub@my-project.iam.gserviceaccount.com
)
# The transport block the proxy preflight requires. Probe strings, not credentials.
TRANSPORT=(
  --set auth.transport.mode=iap
  --set auth.transport.oidcAudience=probe-rg-oauth-client.apps.googleusercontent.com
  --set auth.transport.platformAuthSa=probe-rg@probe-project.iam.gserviceaccount.com
)
GCS=(--set storage.provider=gcs --set storage.bucket=b)
PG=(--set database.driver=postgres "${GCS[@]}" "${CLOUDSQL_SET[@]}")

reject "r1: hub.extraEnv sets K_SERVICE, proxy, no transport" "hub.extraEnv sets K_SERVICE" \
  --set 'hub.extraEnv[0].name=K_SERVICE' --set 'hub.extraEnv[0].value=svc'
reject "r2: postgres driver, proxy, no transport" "database.driver is postgres" \
  "${PG[@]}"
reject "r3: gcs storage with proxy auth, no transport" "storage.provider is gcs and auth.mode is proxy" \
  "${GCS[@]}"
reject "an HA proxy refusal names the transport mode it needs" "must be iap - set auth.transport.mode: iap" \
  "${GCS[@]}"
# The preflight reads oidc_audience as TrimRight(TrimSpace(x), "/"), so a value
# of only slashes is empty to it. The render-time check must strip them too.
reject "HA, oidcAudience of only slashes" "oidc_audience is empty once surrounding whitespace and trailing slashes are removed" \
  "${GCS[@]}" --set auth.transport.mode=iap --set-string 'auth.transport.oidcAudience=//' \
  --set auth.transport.platformAuthSa=probe-rg@probe-project.iam.gserviceaccount.com

# POSITIVE TWINS, ONE PER ROUTE: each refusal clears once the values the
# preflight reads are supplied, and by nothing else.
accept "r1 + transport" \
  --set 'hub.extraEnv[0].name=K_SERVICE' --set 'hub.extraEnv[0].value=svc' "${TRANSPORT[@]}"
accept "r2 + transport" "${PG[@]}" "${TRANSPORT[@]}"
accept "r3 + transport" "${GCS[@]}" "${TRANSPORT[@]}"
# Under oauth the IAP gates sit inside `if cfg.Auth.Mode == "proxy"`, so an HA
# oauth shape needs none of them.
accept "postgres + gcs under oauth, no IAP or transport values" \
  "${PG[@]}" --set auth.mode=oauth "${OAUTH_WEB[@]}"
reject "cloudrun_invoker transport on an HA proxy shape" "must be iap" \
  "${GCS[@]}" --set auth.transport.mode=cloudrun_invoker \
  --set auth.transport.platformAuthSa=probe-rg@probe-project.iam.gserviceaccount.com

# THE OVER-TRIGGER DIRECTION. A deployment that is not HA must not be asked for
# transport. K_SERVICE with an empty value is not a route: the hub reads
# os.Getenv != "", so a guard written on the name alone would refuse it.
accept "K_SERVICE present but with an explicit empty value" \
  --set 'hub.extraEnv[0].name=K_SERVICE' --set 'hub.extraEnv[0].value='
accept "an unrelated extraEnv name"       --set 'hub.extraEnv[0].name=NOT_K_SERVICE' --set 'hub.extraEnv[0].value=svc'
accept "sqlite + local storage + oauth"   --set auth.mode=oauth "${OAUTH_WEB[@]}"
accept "the chart defaults, no route"

echo "== auth.proxy.iap.audience =="
# Required on every proxy render: initHubServer and initWebServer both refuse
# iap with no audience. Its SHAPE is checked only on HA shapes, as the hub
# checks it only in validateHostedHAPreflight. Two layers per case, because
# config.extra and --skip-schema-validation reach the template without the schema.
reject "proxy with no audience, schema layer"   "auth.proxy.iap.audience" \
  --set auth.proxy.iap.audience=
reject "proxy with no audience, template layer" "no IAP audience is set" \
  --skip-schema-validation --set auth.proxy.iap.audience=
reject "whitespace-only audience, template layer" "no IAP audience is set" \
  --skip-schema-validation --set 'auth.proxy.iap.audience=  '
accept "HA, backendServices audience" \
  "${GCS[@]}" "${TRANSPORT[@]}" --set auth.proxy.iap.audience=/projects/123456789012/global/backendServices/5555555555555555555
accept "HA, audience with a trailing slash (the hub trims it)" \
  "${GCS[@]}" "${TRANSPORT[@]}" --set auth.proxy.iap.audience=/projects/123456789012/global/backendServices/5555555555555555555/
reject "HA, malformed audience, schema layer" "auth.proxy.iap.audience" \
  "${GCS[@]}" "${TRANSPORT[@]}" --set auth.proxy.iap.audience=my-iap-audience
reject "HA, malformed audience, template layer" "isSupportedIAPAudience" \
  --skip-schema-validation "${GCS[@]}" "${TRANSPORT[@]}" --set auth.proxy.iap.audience=my-iap-audience
reject "HA, audience with an empty segment, template layer" "isSupportedIAPAudience" \
  --skip-schema-validation "${GCS[@]}" "${TRANSPORT[@]}" --set auth.proxy.iap.audience=/projects//global/backendServices/1
accept "non-HA, malformed audience (the hub checks the shape only on HA)" \
  --set auth.proxy.iap.audience=my-iap-audience
reject "audience set under auth.mode oauth" "but auth.mode is" \
  --set auth.mode=oauth "${OAUTH_WEB[@]}" --set auth.proxy.iap.audience=/projects/123456789012/global/backendServices/5555555555555555555
reject "config.extra overwrites the audience the chart writes" "config.extra overwrites" \
  --set config.extra.server.auth.proxy.iap.audience=/projects/123456789012/global/backendServices/6666666666666666666

echo "== auth.proxy.provider =="
reject "jwt provider, schema layer"   "auth.proxy.provider" --set auth.proxy.provider=jwt
reject "jwt provider, template layer" "config.existingSecret" --skip-schema-validation --set auth.proxy.provider=jwt
reject "header provider, template layer" "initWebServer" --skip-schema-validation --set auth.proxy.provider=header

echo "== auth.transport =="
reject "oidcAudience with no transport mode" "set while auth.transport.mode is" \
  --set auth.transport.oidcAudience=probe-rg-oauth-client.apps.googleusercontent.com
reject "platformAuthSa with mode none" "set while auth.transport.mode is" \
  --set auth.transport.mode=none --set auth.transport.platformAuthSa=probe-rg@probe-project.iam.gserviceaccount.com
reject "unknown transport mode, schema layer" "auth.transport.mode" --set auth.transport.mode=bogus
reject "unknown transport mode, template layer" "must be one of" --skip-schema-validation --set auth.transport.mode=bogus
reject "iap transport with no oidcAudience" "requires auth.transport.oidcAudience" \
  --set auth.transport.mode=iap --set auth.transport.platformAuthSa=probe-rg@probe-project.iam.gserviceaccount.com
reject "cloudrun_invoker with no platformAuthSa" "requires auth.transport.platformAuthSa" \
  --set auth.transport.mode=cloudrun_invoker
accept "cloudrun_invoker with platformAuthSa, non-HA" \
  --set auth.transport.mode=cloudrun_invoker --set auth.transport.platformAuthSa=probe-rg@probe-project.iam.gserviceaccount.com
accept "transport mode none, nothing else" --set auth.transport.mode=none
reject "config.extra camelCase transport key" "That spelling binds nothing" \
  --set config.extra.server.auth.transport.oidcAudience=probe-rg-oauth-client.apps.googleusercontent.com
reject "config.extra camelCase platformAuthSA" "That spelling binds nothing" \
  --set config.extra.server.auth.transport.platformAuthSA=probe-rg@probe-project.iam.gserviceaccount.com

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
  --set auth.mode=oauth --set auth.proxy.iap.audience=
reject "oauth, clientId only, schema layer"   "clientSecret" \
  --set auth.mode=oauth --set auth.proxy.iap.audience= --set auth.oauth.web.google.clientId=rg-id
reject "oauth, clientSecret only, schema layer" "clientId" \
  --set auth.mode=oauth --set auth.proxy.iap.audience= --set auth.oauth.web.google.clientSecret=rg-sec

reject "oauth, no credentials, template layer" "no complete OAuth web client credential is present" \
  --skip-schema-validation --set auth.mode=oauth --set auth.proxy.iap.audience=
reject "oauth, clientId only, template layer"  "google (has client_id, missing client_secret)" \
  --skip-schema-validation --set auth.mode=oauth --set auth.proxy.iap.audience= --set auth.oauth.web.google.clientId=rg-id
reject "oauth, clientSecret only, template layer" "google (has client_secret, missing client_id)" \
  --skip-schema-validation --set auth.mode=oauth --set auth.proxy.iap.audience= --set auth.oauth.web.google.clientSecret=rg-sec

# CLI CREDENTIALS DO NOT SUBSTITUTE FOR WEB ONES, and this row is the reason the
# guard walks the web subtree specifically instead of asking "is server.oauth
# non-empty". The hub keys its login check by client type (pkg/hub/oauth.go:194),
# so a complete cli credential renders, validates, looks like configuration in
# the Secret, and satisfies no browser login. A guard written on presence rather
# than on client type passes this input.
reject "complete cli credential does not satisfy oauth mode" "no complete OAuth web client credential is present" \
  --set auth.mode=oauth --set auth.proxy.iap.audience= \
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
accept "oauth with a complete github web credential" --set auth.mode=oauth --set auth.proxy.iap.audience= \
  --set auth.oauth.web.github.clientId=rg-gh-id --set auth.oauth.web.github.clientSecret=rg-gh-secret
# config.extra IS A FIRST-CLASS WAY TO MEET THE REQUIREMENT, not a bypass of it.
# The guard reads the rendered document, so an operator who supplies
# server.oauth.web themselves has supplied it, and a guard that insisted on
# auth.oauth.web specifically would refuse a correct deployment.
accept "credentials supplied through config.extra in snake_case" --set auth.mode=oauth --set auth.proxy.iap.audience= \
  --set config.extra.server.oauth.web.google.client_id=rg-extra-id \
  --set config.extra.server.oauth.web.google.client_secret=rg-extra-secret
# WITH AN EXTERNAL SETTINGS SECRET THE CHART RENDERS NO SETTINGS DOCUMENT, so
# there is nothing to inspect and nothing to refuse. Asserting this keeps the
# guard from growing into a claim about a file the chart cannot see.
accept "oauth with no credentials but an external settings Secret" \
  --set auth.mode=oauth --set auth.proxy.iap.audience= --set agents.imageRegistry= --set config.existingSecret=operator-owned

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

echo "== agents.imageRegistry is required for the in-process broker =="
# The chart always renders --enable-runtime-broker, and the hub then refuses to
# start without a registry (requireImageRegistryForBroker,
# cmd/server_foreground.go). The hub's sources, each accepted here: the
# SCION_IMAGE_REGISTRY and SCION_MAINTENANCE_IMAGE_REGISTRY environment
# variables, and image_registry resolved for the active profile (profile level,
# else top level). Not checked under config.existingSecret. BASE carries a
# registry, so each row clears it first.
reject "no registry anywhere" "agents.imageRegistry is required" --set agents.imageRegistry=
reject "registry only on a profile that is not active" "agents.imageRegistry is required" \
  --set agents.imageRegistry= --set config.extra.profiles.other.image_registry=example.invalid/agents
reject "SCION_IMAGE_REGISTRY with an empty value" "agents.imageRegistry is required" \
  --set agents.imageRegistry= --set 'hub.extraEnv[0].name=SCION_IMAGE_REGISTRY' --set-string 'hub.extraEnv[0].value='
accept "agents.imageRegistry set"
accept "top-level image_registry through config.extra" \
  --set agents.imageRegistry= --set config.extra.image_registry=example.invalid/agents
accept "active-profile image_registry through config.extra" \
  --set agents.imageRegistry= --set config.extra.profiles.default.image_registry=example.invalid/agents
accept "SCION_IMAGE_REGISTRY through hub.extraEnv" \
  --set agents.imageRegistry= --set 'hub.extraEnv[0].name=SCION_IMAGE_REGISTRY' --set 'hub.extraEnv[0].value=example.invalid/agents'
accept "SCION_MAINTENANCE_IMAGE_REGISTRY through hub.extraEnv valueFrom" \
  --set agents.imageRegistry= --set 'hub.extraEnv[0].name=SCION_MAINTENANCE_IMAGE_REGISTRY' \
  --set 'hub.extraEnv[0].valueFrom.configMapKeyRef.name=registry' --set 'hub.extraEnv[0].valueFrom.configMapKeyRef.key=prefix'
accept "no registry under config.existingSecret" \
  --set agents.imageRegistry= --set auth.proxy.iap.audience= --set config.existingSecret=operator-owned

echo "== secrets.backend =="
# local renders nothing; gcpsm renders server.secrets and needs a project. The
# hub only WARNS when gcpsm has no project (NewGCPBackend's error is logged by
# cmd/server_foreground.go and the hub runs with no secret backend), so the
# chart refuses it. gcpsm values under local reach nothing and are refused too.
reject "gcpsm with no projectId" "requires secrets.gcpsm.projectId" --set secrets.backend=gcpsm
reject "projectId under backend local" "set while secrets.backend is local" \
  --set secrets.gcpsm.projectId=rg-project
reject "replicationLocations under backend local" "set while secrets.backend is local" \
  --set 'secrets.gcpsm.replicationLocations={us-central1}'
reject "unknown backend, schema layer" "secrets.backend" --set secrets.backend=vault
reject "gcpsm alongside config.existingSecret" "secrets.backend" \
  --set config.existingSecret=rg-settings --set auth.proxy.iap.audience= \
  --set secrets.backend=gcpsm --set secrets.gcpsm.projectId=rg-project
accept "gcpsm with projectId" --set secrets.backend=gcpsm --set secrets.gcpsm.projectId=rg-project
# The rendered shape: snake_case keys V1SecretsConfig binds, under server.secrets,
# and absent entirely under the default backend.
executed=$((executed + 1))
_sb="$(render --set secrets.backend=gcpsm --set secrets.gcpsm.projectId=rg-project \
  --set 'secrets.gcpsm.replicationLocations={us-central1,us-east1}' --show-only templates/secret-settings.yaml)"
_sd="$(render --show-only templates/secret-settings.yaml)"
if ! printf '%s\n' "$_sb" | grep -q '^kind: Secret$' || ! printf '%s\n' "$_sd" | grep -q '^kind: Secret$'; then
  echo "FAIL  server.secrets shape: a render produced no settings Secret, so nothing was inspected"
  failed=$((failed + 1))
elif printf '%s\n' "$_sb" | grep -qE '^      secrets:$' \
  && printf '%s\n' "$_sb" | grep -qE '^        backend: gcpsm$' \
  && printf '%s\n' "$_sb" | grep -qE '^        gcp_project_id: rg-project$' \
  && printf '%s\n' "$_sb" | grep -A2 -E '^        gcp_replication_locations:$' | grep -qE '^        - us-east1$' \
  && ! printf '%s\n' "$_sd" | grep -qE '^      secrets:$'; then
  echo "ok    gcpsm renders server.secrets.{backend,gcp_project_id,gcp_replication_locations}; local renders no server.secrets"
else
  echo "FAIL  server.secrets shape: gcpsm did not render the snake_case keys, or local rendered a secrets block"
  failed=$((failed + 1))
fi
unset _sb _sd

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
