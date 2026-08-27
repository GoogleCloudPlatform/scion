#!/usr/bin/env bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Static verification for the scion-hub chart.
#
#   hack/verify.sh            check
#   hack/verify.sh --update   regenerate the golden files, then check
#
# What this can and cannot tell you: everything here is a render. There is no
# cluster, so nothing below says the hub works. It says the manifests are well
# formed, that they say what we intend them to say, and - the part that is
# easiest to lose - that the things we intend them NOT to say are still absent.
# The live checks are in VALIDATION.md and have not been run.
#
# Requires: helm, kubeconform, diff. No cluster.
#
# Exit codes. The two failure codes mean different things and need different
# reactions: 1 is "the chart is wrong", 2 is "the checks did not run, and this
# output is not evidence about the chart either way".
#
#   0  every check ran and passed
#   1  the chart failed a check
#   2  META-FAILURE - the run is not evidence: a tool is missing, or the number
#      of assertions executed was not exactly EXPECTED_TOTAL
#
# MEASURED, NOT ASSUMED: before the preflight below existed, running this file
# with no helm on PATH printed FIVE PASSING ASSERTIONS - among them "emits no
# SCION_SERVER_DATABASE_/OIDC_ variable", the single check this phase most
# needs to be true - because each of them greps a rendered manifest for a string
# that must be ABSENT, and every one of those manifests was an empty file. A
# negative assertion against a file that does not exist is the cheapest false
# pass there is, and no amount of care inside the check prevents it.
#
# EVERY grep CALL SITE IN THIS FILE NAMES ITS DIALECT, -E OR -F, AND THAT IS A
# CORRECTNESS PROPERTY RATHER THAN A STYLE ONE. gd-em's ruling (e), as amended by
# gd-trig and the lead: the check is "names its dialect explicitly", NOT "adds
# -E". Adding -E to a pattern written for BRE is a second way to manufacture a
# confident zero - under GNU grep's BRE, \| \? \+ \( are operators and the bare
# forms are literals, and -E swaps both - so the conversion was audited for
# backslash operators (none present). One site did not survive the conversion and
# is documented where it lives: tests/chart-integrity.sh's schema-path arm, where
# '(root)' is a capture group under -E and a literal under -F.
#
# THAT AUDIT IS NOT THE EVIDENCE. The evidence is a differential run of both
# suites, at 36a3fead^ and at 36a3fead, with every grep invocation executed twice
# on byte-identical input - shipped flags, and dialect letter stripped:
#
#   893 invocations each tree · 0 NODIALECT · 0 UNPARSED
#   DIFFERING on (position, exit status, stdout sha256):  0 of 893
#   apparatus control, -F -> -E planted at the leaf filter below:  2 of 893
#
# so the zero is a measurement with a firing instrument behind it rather than an
# absence claim. 58 invocations at 34 distinct sites DO depend on their -E, and
# all 58 are identical between the two trees: every -E that is load-bearing was
# already there, and every -E added was inert on this data.
#
# SCOPE, because the sentence above is about GNU grep and this shell is not the
# only one on the machine: these scripts are #!/usr/bin/env bash and run-all.sh
# invokes them as `bash <script>`. `bash -c 'type grep'` reports /usr/bin/grep;
# the interactive zsh reports a shell function from a Claude Code snapshot which
# injects -G, --ignore-files, --hidden, -I and six --exclude-dir flags into a
# non-GNU engine. None of that reaches this file, and it was measured rather than
# assumed - but do not paste a grep from here into an interactive shell and
# expect the same answer.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
CHART_DIR="$PWD"
GOLDEN_DIR="$CHART_DIR/golden"
RELEASE="scion"
NAMESPACE="scion-system"

HELM="${HELM:-helm}"
KUBECONFORM="${KUBECONFORM:-kubeconform}"

UPDATE=0
[[ "${1:-}" == "--update" ]] && UPDATE=1

# Every values permutation the goldens cover. The name is the golden file's.
PERMUTATIONS=(
  minimal
  settings
  settings-oauth
  existing-secret
  session-existing
  varied
)

# hub.home per permutation, held here rather than read out of the render.
#
# THIS TABLE IS THE INDEPENDENT SOURCE AND THAT IS THE WHOLE POINT OF IT. The
# obvious alternative - take HOME out of the rendered ConfigMap and check the
# mountPath against it - proves only that two template expressions agree with
# each other, and they agree perfectly when both are the same hardcoded literal.
# The value below comes from ci/values-<name>.yaml (or from values.yaml's
# default where the permutation does not set it), so a template that stopped
# reading hub.home fails against it.
#
# Keep it in step with the ci/ files by hand. The vacuity guard below catches
# the case that actually matters - every entry being the same string, which is
# the state this table was added to end.
declare -A HUB_HOME=(
  [minimal]=/home/scion
  [settings]=/home/scion
  [settings-oauth]=/home/scion
  [existing-secret]=/home/scion
  [session-existing]=/home/scion
  [varied]=/srv/hub
)

# Documents each permutation renders. A committed constant per values file, and
# not one constant for the chart: existing-secret is SIX because the chart
# deliberately emits no Secret when the operator supplies their own, and that
# asymmetry is the config.existingSecret contract rather than an accident of
# this table. A phase that changes the rendered manifest set updates these here,
# in its own diff, beside the template it added.
declare -A EXPECTED_DOCS=(
  [minimal]=8
  [settings]=8
  [settings-oauth]=8
  [existing-secret]=6
  [session-existing]=7
  [varied]=8
)

# The one permutation where the chart renders no settings.yaml, because the
# operator supplied the whole file. Held as an explicit list rather than as an
# "if the file is missing, skip" rule: a skip that derives itself from the
# output is a skip that silently grows to cover a regression.
NO_RENDERED_SETTINGS=(existing-secret)

# The number of assertions this file is committed to executing. Compared with
# -ne, so it fails in BOTH directions: short means something was skipped, over
# means an assertion was added without the number being committed in the diff.
# Update it in the same commit that changes the count, deliberately.
# 283 + 3 for the banned-path tree sweep at the foot of this file (the sweep
# itself, and the planted positive that makes its zero a reading). Summed onto
# the previous committed value, not read off a run.
# 286 + 2 for the sweep's own stderr and exit-status checks. The first version
# of that sweep ended `2>/dev/null || true`, which discarded both, so a grep
# that FAILED produced the same empty output as a grep that found nothing.
# Summed, not read off a run.
# 288 + 9 for the redacted-projection step: four digest arms (password-only
# collapse, the same for a percent-encoding password, independence across all
# four passwords, and the non-credential positive control), the planted
# un-projected mutation that proves those equalities are the redaction and not
# an identical pair of renders, the absence of the annotation under
# config.existingSecret, and three on NOTES.txt printing the rollout-restart
# remedy in both directions. Summed onto the previous committed value.
# The step's arm 0 adds none of these on purpose: it is meta_failure, because
# "nothing was analysed" is a third outcome and not a passing assertion.
# +1 for arm U, gd-p2-rev's C1: rotating database.user must move the checksum.
# +2 for HALF C, gd-p2-rev's C2: the credential-placement paragraph must make no
# pod-roll claim, and must name the manual-step heading it used to contradict.
# +7 for gd-p2-rev's R3 and R4, all in the values-walk step: the refusal each
# leaf is booked on must NAME that leaf; the two companion exemptions must each
# be exercised; the liveness instrument must be provable in both directions (2);
# the operator-facing refusal list must equal the refusals the walk observed;
# and the two configurations the new refusals could have locked out must still
# render (2). 1+1+2+1+2 = 7, and 314+7 = 321.
# The _ck_planted() guard that landed alongside arm U adds none: it is
# meta_failure, for the same reason arm 0 is.
#
# 321 -> 340 IS A REBASE, NOT AN ADDED ASSERTION, and it is the one entry in
# this list that is READ OFF A RUN rather than summed - because the term that
# moved is the base, which this branch does not author and therefore cannot
# sum. This branch was based on scion/gke-chart-p1 at a5551ff9 (committed
# EXPECTED_TOTAL=254) and is now based on 78b035f1 (committed 274). The pin
# below is the driver's own output, pasted unedited, produced by:
#   bash hack/verify.sh 2>&1 | sed -n 's/^assertions: \([0-9]*\)\/.*/\1/p'
# The summed route is kept as an INDEPENDENT CHECK on that paste, and the two
# agree: this branch's own contribution is 321 - 254 = 67, the new base is 274,
# 274 + 67 = 341, less 1 for the NOT_YET needle retired below - P1 wrote
# "Cloud SQL and the database URL" to go red the moment Phase 2 landed the
# database URL, Phase 2 landed it, and retiring it is that gate working - which
# is 340. A pin adjusted to fit and a pin re-derived look identical in a diff;
# the point of writing both routes down is that here they were computed
# separately and had to meet.
# +1 for the backstop-probe shape pin, whose three mutations are recorded at the
# arm itself: A and B are caught by every arm in that step because they stop the
# chart rendering, and C - the rewrite from a literal to a pattern - renders
# clean with a credential in the digest and was caught by nothing.
#
# 343 -> 363 for phase 3 commit 1 (session secret delivery). The session-existing
# permutation adds assertions across the render, golden-digest, doc-count,
# settings-key and hub-home steps. The three new auth.* values (sessionSecret,
# existingSecret, existingSecretKey) and the now-mutable requireStableSigningKey
# add one probe each in the values walk. Read off the driver's output after all
# other changes were made, not summed:
#   bash hack/verify.sh 2>&1 | sed -n 's/^assertions: \([0-9]*\)\/.*/\1/p'
EXPECTED_TOTAL=364

failures=0
assertions=0
pass() { printf '  ok      %s\n' "$1"; assertions=$((assertions + 1)); }
fail() { printf '  FAIL    %s\n' "$1"; failures=$((failures + 1)); assertions=$((assertions + 1)); }
step() { printf '\n== %s\n' "$1"; }

# The run is not evidence. Exits 2, and says so in those words, because "the
# checks did not run" and "the chart is broken" need different reactions.
meta_failure() {
  printf '\n  META-FAILURE  %s\n' "$1"
  printf 'This run is NOT evidence about the chart, in either direction.\n'
  exit 2
}

# --------------------------------------------------------------------------
# Preflight. Before any assertion, because the assertions cannot survive its
# absence: most of this file greps rendered manifests for strings that must NOT
# be there, and an empty file satisfies every one of them. See the exit-code
# note at the top - with no helm on PATH this suite printed five green lines,
# one of them the SCION_SERVER_DATABASE_/OIDC_ check that is this phase's stated
# acceptance criterion.
#
# Both tools are asked to RUN, not merely to exist. A file on PATH that is not
# executable, or a helm too old to know a flag this file uses, is a different
# failure with the same consequence.
if ! "$HELM" version --short >/dev/null 2>&1; then
  meta_failure "helm does not run (tried: $HELM). Nothing below was checked - and without this guard most of it would have reported ok. Set HELM= if it is installed elsewhere."
fi
if ! "$KUBECONFORM" -v >/dev/null 2>&1; then
  meta_failure "kubeconform does not run (tried: $KUBECONFORM). Nothing below was checked. Set KUBECONFORM= if it is installed elsewhere."
fi

# A PATH IS NOT PROVENANCE. Two versions of helm render different manifests, and
# a number produced by an unnamed helm is not reproducible even by someone who
# has helm. This banner prints on EVERY run including the all-green one, because
# a toolchain disclosure that appears only beside failures is a disclosure that
# is missing from every result anyone quotes.
#
# Upstream-verifiable, chained rather than asserted - the sha256 below is the
# BINARY's, and get.helm.sh publishes the TARBALL's, so the chain that binds
# them is:
#
#   curl -sSLO https://get.helm.sh/helm-v3.16.3-linux-amd64.tar.gz
#   curl -sSL  https://get.helm.sh/helm-v3.16.3-linux-amd64.tar.gz.sha256sum
#     f5355c79190951eed23c5432a3b920e071f4c00a64f75e077de0dd4cb7b294ea
#   sha256sum helm-v3.16.3-linux-amd64.tar.gz          # matches the above
#   tar -xzf helm-v3.16.3-linux-amd64.tar.gz
#   sha256sum linux-amd64/helm
#     6a1dffedcf78a687aedc71a918cff0af8f0988184488dddb3615e24abc4e7f2b
#
#   curl -sSLO https://github.com/yannh/kubeconform/releases/download/v0.6.7/kubeconform-linux-amd64.tar.gz
#   curl -sSL  https://github.com/yannh/kubeconform/releases/download/v0.6.7/CHECKSUMS
#     95f14e87aa28c09d5941f11bd024c1d02fdc0303ccaa23f61cef67bc92619d73
#   tar -xzf kubeconform-linux-amd64.tar.gz && sha256sum kubeconform
#     9e867e86e277de971bed3cfe46cf07f1d08db212e9188389670b3685c38281e7
#
# Both chains were run end to end on 2026-08-17 against the binaries at
# /tmp/linux-amd64/helm and /tmp/kubeconform on the authoring box, and the two
# sha256 values above are what they produced.
#
# WHAT THAT SENTENCE MAY NOT SAY, AND ORIGINALLY DID (gd-p1-rev, RQ-2, round 4).
# It said the extracted binaries were `cmp`-identical to "the ones this suite
# uses", and THIS SUITE'S RESOLUTION RULE IS `${KUBECONFORM:-kubeconform}` - a
# PATH lookup. Which bytes this suite uses is therefore a property of the
# reader's environment, and no sentence written in this file can be true about
# it. gd-p1-rev measured the counterexample on the first box that was not mine:
# sha256 225bc03c464df3be, `kubeconform -v` -> `development`, a source build
# rather than the v0.6.7 release tarball. The comment was false there and true
# here, which is the worst available combination, because the honest sha256 row
# printed two lines down then reads as confirming a chain it does not belong to.
#
# SO THE CLAIM IS NOW MADE BY THE RUN INSTEAD OF BY THE COMMENT. The banner
# compares the resolved binary against the pin and prints the verdict. A comment
# asking the reader to believe a chain is a request; a row that recomputes it
# every run is a mechanism.
#
# THIS IS A DISCLOSURE AND NOT A GATE, deliberately. A mismatch prints and the
# run continues at exit 0. Failing on it would convert someone else's toolchain
# into an accusation against the chart - the exact defect the kubeconform
# registry probe below exists to prevent, and the one gd-p1-rev's own extraction
# ran into from the other side when a missing go.mod was reported as a chart
# failure. A reader who wants the same bytes runs the four commands above.
_helm_pin=6a1dffedcf78a687aedc71a918cff0af8f0988184488dddb3615e24abc4e7f2b
_kc_pin=9e867e86e277de971bed3cfe46cf07f1d08db212e9188389670b3685c38281e7
# NO `2>/dev/null` IN THIS BLOCK (gd-em, 11:05, binding). The banner is a
# published measurement, and mechanism C - a zsh tied variable bound as a loop
# or `read` variable - destroys PATH and announces itself on STDERR AND NOWHERE
# ELSE. Suppressing this stream is exactly how a run that could not execute
# `helm` at all reports `unknown` and keeps going. Stderr goes to a file and its
# line count is published beside the result: `stderr=0 lines` is a finding.
_tcerr="$(mktemp)"   # $WORK does not exist yet at this point in the script, and
                     # the EXIT trap set below for $WORK would replace one set here.
_hv="$("$HELM" version --short 2>>"$_tcerr" || echo unknown)"
_kv="$("$KUBECONFORM" -v 2>>"$_tcerr" || echo unknown)"
_hp="$(command -v "$HELM" 2>>"$_tcerr" || printf '%s' "$HELM")"
_kp="$(command -v "$KUBECONFORM" 2>>"$_tcerr" || printf '%s' "$KUBECONFORM")"
# `|| true` IS LOAD-BEARING AND THIS BLOCK WAS FATAL WITHOUT IT (gd-p1-rev, RQ-3,
# round 5). The comment that used to sit here said "the pipeline's status is
# `cut`'s, which is 0 even when sha256sum could not read the file, so the empty
# string is the failure signal". BOTH CLAUSES WERE WRONG, and in the one block
# whose entire argument is that a run which cannot execute helm must still report
# and keep going. Line 78 is `set -euo pipefail`: under pipefail the status is
# sha256sum's, not cut's, and under `set -e` an assignment whose command
# substitution exits non-zero ABORTS THE SCRIPT - so `; _hs="${_hs:-unreadable}"`
# after the semicolon is a separate command that never ran.
#
# Trigger, and it is not exotic: helm resolved through an exported shell
# function, so `command -v helm` returns the bare word `helm` and sha256sum has
# no such file. That is the same "command -v answers IS THIS CALLABLE, not WHERE
# IS THE BINARY" mechanism gd-em corrected fleet-wide at 12:22.
#
#   without `|| true`   rc 1, stdout 0 bytes, stderr 0 bytes. NOTHING AT ALL.
#   with    `|| true`   rc 0, sha256=unreadable, pin=DIFFERS, and the banner's own
#                       stderr row publishes "sha256sum: helm: No such file or
#                       directory". 271/271.
#
# So the empty string is the failure signal BECAUSE `|| true` makes it one. It is
# not a property of the pipeline. Do not remove the `|| true` to "tighten" this.
_hs="$(sha256sum "$_hp" 2>>"$_tcerr" | cut -d' ' -f1 || true)"; _hs="${_hs:-unreadable}"
_ks="$(sha256sum "$_kp" 2>>"$_tcerr" | cut -d' ' -f1 || true)"; _ks="${_ks:-unreadable}"
_pin_verdict() {  # $1 = the sha256 this run resolved, $2 = the pinned sha256
  if [[ "$1" == "$2" ]]; then
    printf 'pin=MATCHES the chain above'
  else
    printf 'pin=DIFFERS from the chain above (chain: %s...) - the counts below were produced by THESE bytes, not by the documented ones' "${2:0:16}"
  fi
}
echo "toolchain  helm         $_hv  $_hp  sha256=${_hs:0:16}  $(_pin_verdict "$_hs" "$_helm_pin")"
echo "toolchain  kubeconform  $_kv  $_kp  sha256=${_ks:0:16}  $(_pin_verdict "$_ks" "$_kc_pin")"
echo "toolchain  stderr       $(wc -l <"$_tcerr" | tr -d ' ') lines$([[ -s "$_tcerr" ]] && printf ':\n%s' "$(cat "$_tcerr")")"
rm -f "$_tcerr"

# kubeconform's DEFAULT schema location is REMOTE. With no route to it, every
# document below comes back `Errors: 1, failed downloading schema` - which does
# not match the expected summary, so without this probe the suite would print
# five lines of the form "kubeconform minimal: wanted ... got ..." and ACCUSE THE
# CHART OF BEING INVALID BECAUSE THE NETWORK WAS DOWN.
#
# MEASURED, v0.6.7, by pointing the tool at a closed port:
#
#   $ kubeconform -strict -summary -schema-location 'https://127.0.0.1:9/{{.ResourceKind}}.json' <cm.yaml
#   stdin - ConfigMap x failed validation: failed downloading schema at
#     https://127.0.0.1:9/configmap.json: ... connect: connection refused
#   Summary: 1 resource found parsing stdin - Valid: 0, Invalid: 0, Errors: 1, Skipped: 0
#   $ echo $?
#   1
#
# So this probe is also the instrument's POSITIVE CONTROL: it asserts, on every
# run, that kubeconform returns Valid:1 for a document that is known good. An
# instrument that cannot say "valid" about anything cannot be trusted when it
# says "valid" about the chart.
_kcprobe="$(printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: verify-sh-probe\ndata:\n  a: "b"\n' \
  | "$KUBECONFORM" -strict -summary 2>&1 || true)"
if ! grep -qF "Valid: 1, Invalid: 0, Errors: 0, Skipped: 0" <<<"$_kcprobe"; then
  meta_failure "kubeconform cannot validate a known-good ConfigMap, so it cannot be asked about the chart. This is almost always the schema registry being unreachable - kubeconform's default -schema-location is remote. Output: $_kcprobe"
fi

in_list() {
  local needle="$1"; shift
  local item
  for item in "$@"; do [[ "$item" == "$needle" ]] && return 0; done
  return 1
}

render() {
  "$HELM" template "$RELEASE" "$CHART_DIR" \
    --namespace "$NAMESPACE" \
    --values "$CHART_DIR/ci/values-$1.yaml"
}

# The settings.yaml document as it lands in the Secret, with the manifest's
# four-space block indent removed. Comment lines are dropped: they are commentary
# on the file, not part of it, and holding them to the same byte-for-byte
# comparison as the configuration turns every wording fix into a golden churn.
settings_block() {
  sed -n '/^  settings\.yaml: |/,/^[^ ]/p' "$1" \
    | sed -e '1d' -e '/^$/d' \
    | grep -E '^    ' \
    | sed -e 's/^    //' -e '/^[[:space:]]*#/d'
}

# Every YAML list item in a rendered manifest whose first key is "name: $2",
# one line of output per item, the item's own keys joined with "; ". Item
# boundaries are read from the indentation: the body of an item is every line
# indented further than the "- " that opens it.
#
# This exists so that a check about one item cannot be answered by a neighbour's
# keys. grep -A<n> reads a fixed window, and in a volumeMounts list the window
# that reaches far enough into one entry has already entered the next.
yaml_list_items() {
  awk -v want="$2" '
    function flush() { if (inblk) { print out; inblk = 0; out = "" } }
    {
      lead = $0; sub(/[^ ].*$/, "", lead); ind = length(lead)
      if ($0 ~ "^ *- name: " want "$") { flush(); inblk = 1; base = ind; next }
      if (inblk && ind <= base) { flush(); next }
      if (inblk) { gsub(/^ +/, ""); out = out (out == "" ? "" : "; ") $0 }
    }
    END { flush() }
  ' "$1"
}

# A render that MUST fail, and must fail for the stated reason. Asserting the
# message and not just the exit status: a template that fails for an unrelated
# reason would otherwise score as a passing negative test, which is the exact
# shape of check this file exists to avoid.
expect_render_failure() {
  local label="$1" expected="$2"; shift 2
  local out
  # An empty $expected makes every check below vacuous, and it is the one input
  # that does: `grep -F ""` matches any input including the single empty line a
  # here-string makes of an empty $out, so the wording match passes, the "%!"
  # match then finds nothing, and the helper reports a pass having read nothing.
  # `set -u` catches a caller that omits the argument; it does not catch one
  # that passes "" or an empty variable, which is the likelier mistake. This is
  # a fault in the harness rather than in the chart, so it is meta, not a fail.
  [[ -n $expected ]] || meta_failure "expect_render_failure ${label@Q} was given an empty expected-wording argument, which every match below is satisfied by"
  if out=$("$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" "$@" 2>&1); then
    fail "$label: the render SUCCEEDED and was supposed to fail"
    return
  fi
  # The render failed and said nothing. helm killed by a signal, an OOM, a
  # wrapper that swallows stderr. Checked here, ahead of the wording match,
  # for two reasons: it is the subject every assertion below reads, and it
  # names the condition. The wording match would also go red on an empty $out,
  # but it would report "failed, but not for the expected reason (wanted ...)",
  # which sends the reader to the chart's diagnostic when the diagnostic is the
  # thing that is missing.
  if [[ -z $out ]]; then
    fail "$label: the render failed and produced no output at all - there is no diagnostic to check, and this is a fault in the run rather than an answer about the chart"
    return
  fi
  # "%!" is Go's marker for a printf verb that could not render its argument -
  # %!q(<nil>), %!d(string=x). It reaches an operator as line noise inside the
  # one sentence that is supposed to tell them what the chart read, and it is
  # invisible to a check that greps for the wording, because the wording is
  # still there. Asserted inside this helper rather than beside one message, so
  # a diagnostic added later is covered without anyone remembering to cover it.
  #
  # It is a negative assertion, so it reads a subject that must exist: an empty
  # $out contains no "%!" and would satisfy it silently. The subject check is
  # the wording match it is nested inside - an empty $out cannot match a
  # non-empty $expected, so it lands in the else branch and goes red there.
  # That is why the guard above insists $expected is non-empty: the wording
  # match is load-bearing twice, and only the second job is obvious.
  if grep -qF -- "$expected" <<<"$out"; then
    if grep -qF -- '%!' <<<"$out"; then
      fail "$label: the message matched, but it contains a Go format error - a value reached printf in a type its verb cannot render"
      printf '          got: %s\n' "$(tr '\n' ' ' <<<"$out" | cut -c1-300)"
    else
      pass "$label"
    fi
  else
    fail "$label: failed, but not for the expected reason (wanted ${expected@Q})"
    printf '          got: %s\n' "$(tr '\n' ' ' <<<"$out" | cut -c1-300)"
  fi
}

# A minimal set of valid values to hang a single bad --set off.
BASE=(
  --set image.repository=example.test/scion-hub-gke
  --set hub.hubId=neg
  --set hub.baseUrl=https://neg.example.com
  --set auth.sessionSecret=neg-session-secret
)

# --------------------------------------------------------------------------
step "helm lint"
# --------------------------------------------------------------------------
for name in "${PERMUTATIONS[@]}"; do
  if "$HELM" lint "$CHART_DIR" --values "$CHART_DIR/ci/values-$name.yaml" >/dev/null 2>&1; then
    pass "lint $name"
  else
    fail "lint $name"
    "$HELM" lint "$CHART_DIR" --values "$CHART_DIR/ci/values-$name.yaml" || true
  fi
done

# --------------------------------------------------------------------------
step "render and schema-validate"
# --------------------------------------------------------------------------
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# The vacuity guard for EXPECTED_DOCS, and it guards a specific regression: if
# every permutation expected the same number, the table would be satisfied by a
# chart that had stopped suppressing the settings Secret under
# config.existingSecret - the one difference it exists to hold.
if [[ "$(printf '%s\n' "${EXPECTED_DOCS[@]}" | sort -u | wc -l)" -ge 2 ]]; then
  pass "the expected document counts are not all the same number"
else
  fail "every permutation expects the same document count - the count check cannot see the Secret that config.existingSecret suppresses"
fi

render_failures=0
for name in "${PERMUTATIONS[@]}"; do
  if render "$name" >"$WORK/$name.yaml" 2>"$WORK/$name.err"; then
    pass "template $name"
  else
    fail "template $name"
    cat "$WORK/$name.err"
    render_failures=$((render_failures + 1))
    continue
  fi
  # The summary line, not the exit status, and every field of it.
  #
  #   $ kubeconform -strict -summary </dev/null
  #   Summary: 0 resource found parsing stdin - Valid: 0, Invalid: 0, Errors: 0, Skipped: 0
  #   $ echo $?
  #   0
  #
  # Measured. An empty document set is a clean pass, so the exit status alone
  # says "nothing was invalid", which is also what validating nothing produces.
  # Asserting the expected number of VALID documents turns that into a claim
  # about what was checked. Skipped is asserted at 0 for the same reason one
  # layer down: a kind kubeconform has no schema for is a skip, not a failure,
  # the moment anyone adds --ignore-missing-schemas.
  kcout="$("$KUBECONFORM" -strict -summary <"$WORK/$name.yaml" 2>&1 || true)"
  kcwant="Valid: ${EXPECTED_DOCS[$name]}, Invalid: 0, Errors: 0, Skipped: 0"
  if grep -qF "failed downloading schema" <<<"$kcout"; then
    # The preflight probe should have caught this. If it fires HERE the registry
    # went away mid-run, and the distinction still has to be made, because the
    # branch below would report it as "the chart is invalid" - which is a claim
    # about the subject made from a fact about the instrument.
    meta_failure "kubeconform could not retrieve a schema while validating $name, so this is not a result about the chart: $kcout"
  elif grep -qF "$kcwant" <<<"$kcout"; then
    pass "kubeconform $name validated ${EXPECTED_DOCS[$name]} documents, none skipped"
  else
    fail "kubeconform $name: wanted '$kcwant', got '$kcout'"
    "$KUBECONFORM" -strict <"$WORK/$name.yaml" || true
  fi
done

# Stop here if a permutation did not render. Every check below this line reads
# those five files, and most of them ask whether a string is ABSENT - which an
# empty file answers yes to. Continuing would turn one real failure into a
# hundred and ninety green lines and one red one.
#
# This is exit 1 and not a meta-failure: helm ran, and it said the chart is
# wrong. The distinction is the point of having two codes.
if [[ $render_failures -gt 0 ]]; then
  printf '\n%d permutation(s) failed to render. Stopping: every check below reads those files,\n' "$render_failures"
  printf 'and a check for an absent string passes against a file that is not there.\n'
  # Said out loud rather than left to be noticed. This run IS short of
  # EXPECTED_TOTAL, deliberately, and the count check below never executes - so
  # the one line that would otherwise report the shortfall is missing from the
  # output at the moment the output is shortest.
  printf 'assertions: %d/%d - NOT CHECKED, the run stopped here on purpose.\n' "$assertions" "$EXPECTED_TOTAL"
  printf '%d check(s) FAILED.\n' "$failures"
  exit 1
fi

# --------------------------------------------------------------------------
step "kubeconform's positive control: manifests it MUST reject"
# --------------------------------------------------------------------------
# PHASE 2, at gd-em's condition. Every kubeconform arm above is a green light,
# and a validator that has quietly stopped validating emits green lights of
# exactly the same colour. "Valid: 7, Skipped: 0" narrows that a long way - it
# rules out an empty document set and it rules out --ignore-missing-schemas -
# but it still cannot separate "seven documents were checked and were correct"
# from "seven documents were counted and the schema was never applied". Nothing
# in a passing run can. The only instrument that can is one where the expected
# answer is INVALID.
#
# THE SUBJECT IS THE CHART'S OWN RENDER, not a hand-written bad manifest. A
# stub I wrote would demonstrate that kubeconform rejects a stub I wrote. What
# has to be established is that it is looking at THIS Deployment - the one with
# the Cloud SQL proxy in initContainers - so the negatives are cut from that
# document mechanically and each cut is checked to have changed something.
kc_dep="$WORK/kc-deployment.yaml"
awk '/^# Source: scion-hub\/templates\/deployment\.yaml$/{f=1} f{ if ($0=="---") exit; print }' \
  "$WORK/settings.yaml" >"$kc_dep"
# DIALECT NAMED PER PATTERN, NOT PER FILE. -E on the first because ^ and $ are
# anchors that must stay anchors; -F on the second because "initContainers:" is
# a literal and -F is the only flag that cannot reinterpret it. Adding -E to
# both would be the mechanical fix and it is the wrong one: GNU BRE's \| \? \+
# are operators under -G and literals under -E, so a blanket -E converts
# correct BRE patterns into confident zeros.
#
# MEASURED, not inspected. Against the real rendered Deployment (153 lines):
#   ^kind: Deployment$   BRE=1  ERE=1  -G=1   AGREE   (-F=0, which is why -F is
#                                                      wrong here: anchors are
#                                                      not literals)
#   initContainers:      BRE=1  ERE=1  -G=1  -F=1     AGREE on all four
# 1 call site, 2 invocations, 0 disagreeing. An earlier version of this comment
# claimed the same thing from reading the patterns; the claim was right and the
# grounds were not, and a correct conclusion from an unrun check is the thing
# this file exists to catch.
if [[ ! -s "$kc_dep" ]] || ! grep -Eq '^kind: Deployment$' "$kc_dep" || ! grep -qF 'initContainers:' "$kc_dep"; then
  meta_failure "could not extract this chart's Deployment (with its proxy initContainers) from the settings render, so the control below would be validating something other than the chart."
fi

kc_summary() { "$KUBECONFORM" -strict -summary <"$1" 2>&1 || true; }

# THE PAIRED POSITIVE. Without it, every negative below is satisfied by a
# document kubeconform cannot parse at all - which is invalid for the wrong
# reason and would hide a broken extractor.
if grep -qF 'Valid: 1, Invalid: 0, Errors: 0, Skipped: 0' <<<"$(kc_summary "$kc_dep")"; then
  pass "the extracted Deployment validates on its own, so the rejections below are rejections of a specific defect"
else
  fail "the extracted Deployment does not validate on its own: $(kc_summary "$kc_dep"). Every negative control below is then meaningless."
fi

# Each entry: label | sed/awk-free mutation applied by the function below.
# THREE DEFECTS, THREE DIFFERENT SCHEMA MECHANISMS, deliberately - one arm
# passing tells you one mechanism is live, not that validation is.
kc_mutate() { # kc_mutate <which> <out>
  case "$1" in
    type)     sed 's/^  replicas: [0-9][0-9]*$/  replicas: "one"/' "$kc_dep" >"$2" ;;
    strict)   sed '0,/^  replicas: /s//  notAFieldInTheSchema: 1\n  replicas: /' "$kc_dep" >"$2" ;;
    required) awk '/^  selector:$/{s=1;next} s&&/^    /{next} {s=0;print}' "$kc_dep" >"$2" ;;
  esac
}
declare -A KC_NEGATIVES=(
  [type]="a string where the schema demands an integer (spec.replicas)"
  [strict]="a field the schema does not define (proves -strict is in effect, not merely passed)"
  [required]="a required field removed (spec.selector)"
)
for _m in type strict required; do
  _out="$WORK/kc-neg-$_m.yaml"
  kc_mutate "$_m" "$_out"
  # A DERIVED NEGATIVE THAT IS BYTE-IDENTICAL TO THE POSITIVE IS NOT A NEGATIVE.
  # If the render's shape drifts so a mutation no longer matches, the arm would
  # go green off the unmodified document. That is a harness error, not a pass.
  if cmp -s "$kc_dep" "$_out"; then
    meta_failure "the '$_m' mutation changed nothing, so that arm would be validating the untouched Deployment and reporting it as a rejection."
  fi
  if grep -qF 'Valid: 0, Invalid: 1, Errors: 0, Skipped: 0' <<<"$(kc_summary "$_out")"; then
    pass "kubeconform REJECTS ${KC_NEGATIVES[$_m]}"
  else
    fail "kubeconform did NOT reject ${KC_NEGATIVES[$_m]}: $(kc_summary "$_out"). Every kubeconform pass in this file is then unsupported - the validator is counting documents, not checking them."
  fi
done
# ATTACK LOG. This step exists to catch a validator that has stopped
# validating, so it was run against two, supplied through KUBECONFORM= :
#
#   a stub that counts `kind:` lines and reports them all Valid
#     -> all three negatives RED. The paired positive stayed GREEN, which is
#        the entire argument for this step in one line: under a validator that
#        checks nothing, every green arm in this file is still green.
#   a wrapper that silently strips -strict before exec'ing the real binary
#     -> the strict arm RED, the other two GREEN. The three defects are
#        independent and each names the mechanism it lost, so this failure
#        reads as "-strict is gone" and not as "kubeconform is broken".
#
# Both scripts are three lines; neither is committed, because a fake validator
# living in the tree is a fake validator someone eventually points the suite at.

# --------------------------------------------------------------------------
if [[ $UPDATE -eq 1 ]]; then
  step "updating golden files"
  mkdir -p "$GOLDEN_DIR"
  for name in "${PERMUTATIONS[@]}"; do
    cp "$WORK/$name.yaml" "$GOLDEN_DIR/$name.yaml"
    printf '  wrote   golden/%s.yaml\n' "$name"
  done
fi

step "golden files"
for name in "${PERMUTATIONS[@]}"; do
  if [[ ! -f "$GOLDEN_DIR/$name.yaml" ]]; then
    fail "golden/$name.yaml is missing - run hack/verify.sh --update"
    continue
  fi
  if diff -u "$GOLDEN_DIR/$name.yaml" "$WORK/$name.yaml" >"$WORK/$name.diff"; then
    pass "golden $name"
  else
    fail "golden $name differs - run hack/verify.sh --update if the change is intended"
    head -60 "$WORK/$name.diff"
  fi
done

# --------------------------------------------------------------------------
step "no SCION_SERVER_DATABASE_* or SCION_SERVER_OIDC_* variable is ever emitted"
# --------------------------------------------------------------------------
# Some of these names bind and some are discarded, and BOTH outcomes are wrong,
# which is why the check is on the whole prefix rather than on a list of the
# harmful ones. A name binds only if every underscore-separated segment survives
# camelCaseFields (pkg/config/hub_config.go:919): SCION_SERVER_DATABASE_URL and
# SCION_SERVER_DATABASE_DRIVER do, SCION_SERVER_DATABASE_MAX_OPEN_CONNS does not.
# One that binds is applied AFTER settings.yaml (:683) and wins, so the hub runs
# a configuration this chart never rendered and every guard in it never saw. One
# that is discarded is dropped by k.Unmarshal with no error, yet DetectEnvOverrides
# (pkg/config/opsettings/koanf.go:347) still lists it to the admin server-config
# view as an active override - reported as applied, which is worse than silent.
# Neither outcome raises anything at runtime, which is why it is caught here.
#
# THE INSTRUMENT IS POINTED AT SOMETHING FIRST. These five were measured emitting
# ok against an EMPTY render - gd-em found them by stubbing render() to produce
# nothing, and all five went green, because a grep that finds nothing in a file
# with nothing in it is indistinguishable from a chart that is correct. That is
# the same defect class as the %! guard and it sits on the brief's one mechanical
# requirement, which makes it the worst place in this file for it to be.
#
# The precondition is deliberately NOT "the file is non-empty". A render that
# lost its env block entirely would still be a large file, and the negative would
# still be vacuous in exactly the way that matters. What has to be true for the
# negative to mean anything is that this manifest carries SCION_SERVER_ names at
# all - that the family the check is scanning for is present and the scan is
# looking in the right place. Every permutation renders SCION_SERVER_BASE_URL and
# SCION_SERVER_SESSION_SECRET, so a permutation with none has either stopped
# configuring the hub through env or stopped rendering, and either way the
# refusal below is unearned.
for name in "${PERMUTATIONS[@]}"; do
  _env_names="$(grep -Eo 'SCION_SERVER_[A-Z_]+' "$WORK/$name.yaml" | sort -u | tr '\n' ' ' || true)"
  _env_names="${_env_names% }"
  if [[ -n "$_env_names" ]]; then
    pass "$name renders SCION_SERVER_ variables, so the refusal below has a subject [${_env_names}]"
  else
    fail "$name renders no SCION_SERVER_ variable at all, so the DATABASE_/OIDC_ check below cannot fail and its ok line means nothing. Either the render is broken or the chart stopped configuring the hub through env - find out which before reading the next line."
  fi
  # THE BARE PREFIX, NOT prefix-followed-by-colon-or-equals. This pattern used to
  # end '[A-Z_]*[[:space:]]*[:=]', and MEASURED, it could not match the shape this
  # chart actually renders: a Kubernetes env entry is
  #
  #     - name: SCION_SERVER_DATABASE_URL
  #       value: postgres://...
  #
  # and the name ends the line, with the colon BEFORE it. The seeded control -
  # append exactly those two lines to a render - passed. So the check that this
  # phase's brief singles out could match an env map, which the chart does not
  # emit, and not an env list, which it does. A rendered manifest is machine
  # output with no prose in it, so the prefix alone is the right subject and any
  # occurrence at all is the failure.
  #
  # CONTROLS, 2026-08-17, both directions, both red:
  #
  #   stub render() to emit nothing     -> all five ok lines gone, replaced by
  #                                        "renders no SCION_SERVER_ variable at
  #                                        all" (this was gd-em's mutation, and
  #                                        before this change all five went GREEN)
  #   append an env-LIST entry          -> FAIL varied  (GREEN before this change)
  #   append an env-MAP entry           -> FAIL minimal (red before it too)
  if grep -Eq 'SCION_SERVER_(DATABASE|OIDC)_' "$WORK/$name.yaml"; then
    fail "$name emits a SCION_SERVER_DATABASE_* or SCION_SERVER_OIDC_* variable"
    grep -En 'SCION_SERVER_(DATABASE|OIDC)_' "$WORK/$name.yaml" || true
  elif [[ -n "$_env_names" ]]; then
    pass "$name emits no SCION_SERVER_DATABASE_/OIDC_ variable"
  else
    fail "$name emits no SCION_SERVER_DATABASE_/OIDC_ variable, but it emits no SCION_SERVER_ variable of any kind either, so this is not evidence about the chart"
  fi
done

# The positive twin. The check above passes trivially on a chart that configures
# no database at all, which is precisely the bug it would be reporting as fixed.
# So: the database must be configured, in the file, at the right path, with the
# operator's values.
db_block="$(settings_block "$WORK/settings.yaml")"
if grep -qxF '  database:' <<<"$db_block"; then
  pass "the database settings are under server.database"
else
  fail "no server.database section in the rendered settings.yaml"
fi
# Four-space indent: server: at column 0, database: at 2, its keys at 4. The
# indent is part of the assertion - it is what makes this a check on
# server.database rather than on the string appearing anywhere in the file.
for expected in \
  '    driver: postgres' \
  '    max_open_conns: 25' \
  '    max_idle_conns: 5' \
  '    conn_max_lifetime: 30m' \
  '    conn_max_idle_time: 5m'
do
  if grep -qxF "$expected" <<<"$db_block"; then
    pass "settings.yaml configures server.database.${expected#    }"
  else
    fail "settings.yaml is missing server.database.${expected#    } - the absence check above is vacuous without it"
  fi
done

# --------------------------------------------------------------------------
step "hub.extraEnv refuses every variable the chart itself sets"
# --------------------------------------------------------------------------
# THE LIST IS READ OUT OF THE RENDERED MANIFEST, WHICH IS THE ONLY VERSION OF
# THIS CHECK WORTH HAVING. A hand-written list of "variables the chart sets"
# tests that the guard rejects the names somebody remembered, and the names
# somebody remembered are the ones already in the guard. The two lists agree
# because they have the same author, not because the chart does what they say -
# and the guard's list was in fact two names short when this was written.
#
# So: render the chart, take every environment variable name out of the output,
# and require the guard to refuse each one. A variable added to the ConfigMap or
# to the Deployment without being added to the guard fails here, by construction.
#
# The varied permutation, because it is the one that sets hub.adminMode and
# hub.maintenanceMessage - the two conditionally-emitted variables, which are
# exactly the ones a hand-maintained list misses. hub.extraEnv is nulled out of
# it first: its own entry is an operator variable, not a chart one, and requiring
# the guard to reject it would be requiring the guard to be broken.
shadow_src="$WORK/shadow-src.yaml"
"$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
  --values "$CHART_DIR/ci/values-varied.yaml" \
  --set hub.extraEnv=null >"$shadow_src" 2>>"$_tcerr" || true
# Stderr goes to the toolchain log rather than to /dev/null (my own audit for
# gd-p2-dev, item 4). This site is fail-CLOSED either way - the consumer below
# has an absolute floor of 7 NAMED variables, so a failed render cannot pass -
# but a reader who hits that floor should be told WHY the render produced
# nothing instead of being left to re-run it by hand.
# ConfigMap keys, from inside the ConfigMap's data block only, plus the container
# env entries, which are uppercase by convention and are the fieldRef variables a
# ConfigMap render cannot see. Ports and container names are lowercase and do not
# match.
mapfile -t shadow_names < <(
  {
    awk '/^kind: ConfigMap$/{inCM=1} /^---/{inCM=0; inData=0}
         inCM && /^data:$/{inData=1; next}
         inData && /^  [A-Z_]+:/{sub(":.*","",$1); print $1}' "$shadow_src"
    grep -Eo '^ +- name: [A-Z][A-Z0-9_]*$' "$shadow_src" | awk '{print $3}'
  } | sort -u
)
# Vacuity guard, and the number is deliberate: HOME, KUBECONFIG,
# SCION_SERVER_BASE_URL, SCION_REQUIRE_STABLE_SIGNING_KEY,
# SCION_SERVER_ADMIN_MODE, SCION_SERVER_MAINTENANCE_MESSAGE, POD_NAMESPACE. An
# extraction that silently returned two names would leave this whole step
# reporting success on nothing.
if [[ ${#shadow_names[@]} -lt 7 ]]; then
  fail "only ${#shadow_names[@]} environment variable names were extracted from the rendered chart (${shadow_names[*]:-none}) - the shadow checks below would be testing almost nothing"
else
  pass "extracted ${#shadow_names[@]} chart-set environment variables to check the guard against"
fi
for shadow_name in "${shadow_names[@]}"; do
  # An overlay file rather than --set, because a list in a later values file
  # replaces the earlier one wholesale - so this is the permutation's own values
  # with exactly one operator variable, whatever hub.extraEnv held before.
  cat >"$WORK/shadow-try.yaml" <<SHADOWTRY
hub:
  extraEnv:
    - name: $shadow_name
      value: shadowed
SHADOWTRY
  if out=$("$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
      --values "$CHART_DIR/ci/values-varied.yaml" \
      --values "$WORK/shadow-try.yaml" 2>&1); then
    fail "hub.extraEnv accepted $shadow_name, which the chart sets itself. A duplicate entry in the container's env list wins over the value from envFrom, so the chart's value is silently replaced."
  elif grep -qF "hub.extraEnv may not set $shadow_name" <<<"$out"; then
    pass "hub.extraEnv refuses $shadow_name"
  else
    fail "hub.extraEnv refused $shadow_name, but not with the shadowing message - the render failed for some other reason and this check is not testing the guard"
    printf '          %s\n' "$(tr '\n' ' ' <<<"$out" | cut -c1-200)"
  fi
done
# The twin for the conditional half. When the chart does NOT emit
# SCION_SERVER_ADMIN_MODE - hub.adminMode unset, which is the default - there is
# nothing to shadow and the name must be accepted. This is what distinguishes a
# derived list from a hardcoded one: a hardcoded list rejects it in both cases,
# and the rejection in the second case is a refusal with nothing behind it.
if "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    "${BASE[@]}" \
    --set 'hub.extraEnv[0].name=SCION_SERVER_ADMIN_MODE' \
    --set-string 'hub.extraEnv[0].value=true' >/dev/null 2>&1; then
  pass "hub.extraEnv accepts SCION_SERVER_ADMIN_MODE when the chart emits no such variable"
else
  fail "hub.extraEnv rejected SCION_SERVER_ADMIN_MODE with hub.adminMode unset - the guard is matching a remembered name rather than what the chart actually emits"
fi

# --------------------------------------------------------------------------
step "migration-rename-hazard: schema_version is present in every rendered settings.yaml"
# --------------------------------------------------------------------------
# Named for the hazard and not for the field, because the field looks redundant
# and the hazard does not.
#
# The hub auto-migrates a settings file whose format it cannot detect
# (pkg/config/settings.go:590-600), the detector keys on schema_version, and the
# migration replaces the file with os.Rename (pkg/config/settings_v1.go:2694).
# settings.yaml is delivered as a subPath bind mount, and renaming over a bind
# mount returns EBUSY. Every other write to this file under the mount is soft -
# a warning, or a 500 to one caller, with the server continuing. This is the
# only one that becomes a hard failure.
#
# The hub does not guard this in hosted mode, deliberately, on the grounds that
# the chart controls the input. This check is that guarantee. Deleting it does
# not break anything today and removes the only thing standing behind a decision
# taken elsewhere.
#
# Checked on every permutation, not just the default: a key present only in the
# default rendering is a key that disappears the first time an override path is
# exercised, with every golden still green.
checked_any=0
for name in "${PERMUTATIONS[@]}"; do
  block="$(settings_block "$WORK/$name.yaml" || true)"
  if in_list "$name" "${NO_RENDERED_SETTINGS[@]}"; then
    if [[ -n "$block" ]]; then
      fail "$name is listed as rendering no settings.yaml, but one was rendered"
    else
      pass "$name renders no settings.yaml (config.existingSecret), correctly exempt"
    fi
    continue
  fi
  if [[ -z "$block" ]]; then
    fail "$name rendered no settings.yaml and is not on the exemption list - the schema_version check would have passed vacuously"
    continue
  fi
  checked_any=1
  if grep -qxF 'schema_version: "1"' <<<"$block"; then
    pass "$name renders schema_version: \"1\""
  else
    fail "$name does not render schema_version: \"1\" - the hub's lazy migration can fire and os.Rename over the subPath mount returns EBUSY"
  fi
done
if [[ $checked_any -eq 0 ]]; then
  fail "no permutation actually rendered a settings.yaml - this whole check was vacuous"
fi

# --------------------------------------------------------------------------
step "the 5.3 file shape"
# --------------------------------------------------------------------------
block="$(settings_block "$WORK/settings.yaml")"
for key in 'schema_version: "1"' 'active_profile: default' 'profiles:' 'runtimes:'; do
  if grep -qxF "$key" <<<"$block"; then
    pass "top-level $key"
  else
    fail "missing top-level $key"
  fi
done
# The six keys that belong under server:. At the top level they parse, install,
# and are silently not read.
for key in notification_channels message_broker native_chat plugins scheduler github_app; do
  if grep -qxF "$key:" <<<"$block"; then
    fail "$key is at the top level of settings.yaml; it belongs under server:"
  else
    pass "$key is not at the top level"
  fi
done
# ...and the positive twin: one of them is actually rendered, under server:,
# so "not at the top level" is not passing because nothing is there at all.
if grep -qxF '  scheduler:' <<<"$block"; then
  pass "scheduler is rendered under server:"
else
  fail "scheduler is not rendered under server: - the six top-level checks above are vacuous"
fi

# --------------------------------------------------------------------------
step "every rendered settings.yaml carries a top-level server: key"
# --------------------------------------------------------------------------
# This looks like a restatement of the checks above and it is not. Those assert
# what belongs under server:. This asserts that the key exists at all, in EVERY
# permutation, and it is here because a Phase 0 guard is closed by it.
#
# --config / -c is reserved in hub.args, and at Phase 0 it was not merely
# reserved, it was LIVE. Not for want of a settings.yaml - the hub seeds one from
# its own embedded defaults on a first boot (cmd/server_foreground.go:104-109 ->
# config.InitMachine, pkg/config/init.go:588-599) and those defaults have no
# server key. THE TRIGGER IS THE KEY, NOT THE FILE, so "the chart mounts a
# settings.yaml" is not the property that matters and a check that asserted the
# mount would not be this check. THIS PHASE MAKES THE FLAG INERT by emitting the
# top-level server key, and the reason is ours end to end:
# loadGlobalConfigFromSettings (pkg/config/hub_config.go:640) reads
# GetGlobalDir() first and unconditionally, and consults the --config path only
# `if !found` (:647-660). `found` is true exactly when
# $HOME/.scion/settings.yaml parses AND has a non-nil top-level `server` key -
# loadServerFromSettingsFile decides it at :1344-1347 on that key alone, not on
# the file existing and not on its contents being useful.
#
# So the flag is inert while, and only while, this chart renders that key. Drop
# it - by minimising the document, by moving the server section under a profile,
# by rendering a settings.yaml that is only profiles and runtimes - and the flag
# is live again, in two forms: the --config path's own settings.yaml becomes the
# sole source of the server config (:648-659), or, failing that,
# loadGlobalConfigLegacy (:635, :699) layers the --config file over the loaded
# configuration (:777-787). Neither is a redirect of the whole load and nothing
# is: GetGlobalDir (pkg/config/paths.go:188-194) takes no arguments.
#
# That is rule 8 from the far side: Phase 0's guard is closed by Phase 1's
# configuration, so it is deferred to whoever changes Phase 1's configuration,
# who has no reason to know they are holding it. This check is how they find
# out. Do not delete it because the top-level shape "obviously" has a server
# key; that obviousness is the entire mechanism.
#
# Under config.existingSecret the property is the operator's, not ours, and
# cannot be checked here - values.yaml says so at config.existingSecret.
#
# WHICH PERMUTATIONS ARE ACTUALLY LOAD-BEARING HERE: minimal and varied. The two
# settings permutations set config.extra.server.log_level, so config.extra merges
# a top-level `server` key back in and they would pass this check even with the
# chart emitting none. Measured, by removing the assertSettings call and renaming
# the emitted key: only minimal and varied went red. Do not "simplify" this loop
# to the settings permutation, and do not add config.extra.server.* to
# values-minimal.yaml or values-varied.yaml without moving this coverage.
checked_any=0
for name in "${PERMUTATIONS[@]}"; do
  in_list "$name" "${NO_RENDERED_SETTINGS[@]}" && continue
  block="$(settings_block "$WORK/$name.yaml" || true)"
  if [[ -z "$block" ]]; then
    fail "$name rendered no settings.yaml and is not exempt - this check would pass vacuously"
    continue
  fi
  checked_any=1
  if grep -qxF 'server:' <<<"$block"; then
    pass "$name renders a top-level server: key"
  else
    fail "$name renders no top-level server: key - the hub's global settings read returns not-found, and --config stops being inert"
  fi
done
if [[ $checked_any -eq 0 ]]; then
  fail "no permutation rendered a settings.yaml - the top-level server: check was vacuous"
fi

# --------------------------------------------------------------------------
step "the HA preflight keys, in both auth modes"
# --------------------------------------------------------------------------
# The hosted preflight's first block. server.database.url and the session secret
# are the two remaining members and arrive with Cloud SQL and the secret
# handling respectively; the rest are rendered here and must be identical
# whichever authentication mode is selected.
for name in settings settings-oauth; do
  block="$(settings_block "$WORK/$name.yaml")"
  for expected in \
    '  mode: hosted' \
    '    hub_id: ci-settings' \
    '    provider: gcs' \
    '    bucket: ci-settings-hub-blobs' \
    '    driver: postgres'
  do
    if grep -qxF "$expected" <<<"$block"; then
      pass "$name preflight key '${expected#    }'"
    else
      fail "$name is missing preflight key '${expected#    }'"
    fi
  done
done

# Byte-for-byte, with the authentication mode itself masked. Switching mode must
# change one line and nothing else; a chart that dropped a preflight key in one
# mode would install and then fail at hub startup, naming the key but not the
# reason it went missing.
#
# The mask is anchored to server.auth.mode's exact indentation - four spaces, two
# levels down - and not to any line spelling "mode:". server.mode: hosted is one
# level up and would be masked by a looser pattern the day someone renders an
# auth mode named hosted, and any future subtree with a mode key of its own would
# be masked silently, which is the direction that hides a difference rather than
# reporting one.
settings_block "$WORK/settings.yaml"       | sed 's/^\(    mode: \)\(proxy\|oauth\)$/\1MASKED/' >"$WORK/auth-a-full"
settings_block "$WORK/settings-oauth.yaml" | sed 's/^\(    mode: \)\(proxy\|oauth\)$/\1MASKED/' >"$WORK/auth-b-full"
# server.oauth sits at two spaces once settings_block has stripped the Secret's
# indentation. The subtree ends at the next key at that same depth.
excise_oauth() {
  awk -v out="$2" '
    /^  oauth:$/          { in_oauth = 1; print > out; next }
    in_oauth && /^  [^ ]/ { in_oauth = 0 }
    in_oauth              { print > out; next }
                          { print }
  ' "$1"
}
excise_oauth "$WORK/auth-a-full" "$WORK/auth-a-oauth" >"$WORK/auth-a"
excise_oauth "$WORK/auth-b-full" "$WORK/auth-b-oauth" >"$WORK/auth-b"
touch "$WORK/auth-a-oauth" "$WORK/auth-b-oauth"
if diff -u "$WORK/auth-a" "$WORK/auth-b" >"$WORK/auth.diff"; then
  pass "the two auth modes render identical settings.yaml apart from auth.mode"
else
  fail "switching auth.mode changed more than the auth subtree"
  cat "$WORK/auth.diff"
fi
# The mask has to have masked something, in both files, and exactly one line of
# each - or the comparison above is either comparing two files that were never
# different, or comparing two files it has flattened into agreement.
masked_a="$(grep -cxF '    mode: MASKED' "$WORK/auth-a" || true)"
masked_b="$(grep -cxF '    mode: MASKED' "$WORK/auth-b" || true)"
if [[ "$masked_a" == 1 && "$masked_b" == 1 ]]; then
  pass "the auth-mode mask matched exactly one line in each of the two renders"
else
  fail "the auth-mode mask matched $masked_a lines in proxy mode and $masked_b in oauth mode, expected 1 and 1 - at 0 the comparison above proves nothing, and above 1 it is hiding a real difference"
fi
if grep -qxF '  mode: hosted' "$WORK/auth-a" && grep -qxF '  mode: hosted' "$WORK/auth-b"; then
  pass "the auth-mode mask left server.mode alone"
else
  fail "server.mode: hosted is not present unmasked in both renders - either hosted mode is gone, or the mask reached a line it should not have"
fi
# WHAT THE EXCISION TOOK OUT, ASSERTED ON BOTH SIDES. If the oauth arm's subtree
# is not exactly this, the diff above compared something other than what this
# section claims. If the proxy arm's is not empty, the excision is hiding a
# difference rather than accounting for one - and the diff would still be clean,
# which is why this is checked and not assumed.
oauth_want='  oauth:
    web:
      google:
        client_id: ci-oauth-web-google-client-id.apps.googleusercontent.invalid
        client_secret: ci-oauth-web-google-client-secret-not-a-real-secret'
if [[ "$(cat "$WORK/auth-b-oauth")" == "$oauth_want" ]]; then
  pass "the excised subtree is exactly server.oauth.web.google, in snake_case"
else
  fail "the oauth arm's excised subtree is not what this check accounts for"
  diff -u <(printf '%s\n' "$oauth_want") "$WORK/auth-b-oauth" || true
fi
if [[ ! -s "$WORK/auth-a-oauth" ]]; then
  pass "the proxy arm renders no server.oauth at all, so the excision removed nothing from it"
else
  fail "the proxy arm rendered a server.oauth subtree, which the excision then hid from the diff"
  cat "$WORK/auth-a-oauth"
fi

# --------------------------------------------------------------------------
step "NOTES.txt names every unlanded gate the walk found, for a release that acknowledged them"
# --------------------------------------------------------------------------
# WHY THIS EXISTS. The gate names live in the assertHAUnlanded refusal,
# and that refusal does not print when acknowledgeHAUnlanded is set. Both of the
# chart's own settings fixtures set it - so the gates are named to every
# operator EXCEPT the two who copied a file out of this repository, which is the
# population most likely to hit them. NOTES.txt prints on every install and is
# the only prose the operator reliably sees.
#
# RENDERING NOTES.txt WITHOUT A CLUSTER. `helm template` evaluates NOTES.txt but
# emits nothing for it, and --show-only cannot address it ("could not find
# template"). `helm install --dry-run=client` does print it and was MEASURED to
# demand a cluster on this box anyway ("Kubernetes cluster unreachable"), so it
# is not available. The probe therefore copies the chart, RENAMES
# templates/NOTES.txt to a .txt manifest name, and asks helm for that file:
# helm's own file-template renderer runs, on the real bytes, with the real
# values, and the only substitution is the filename. --debug is required because
# the output is prose and not YAML.
#
# THE SUBSTITUTION IS CHECKED, NOT ASSUMED. The probe file must be byte-identical
# to templates/NOTES.txt, and the render must be non-empty. An empty render
# satisfies every "does not contain" assertion below - the same false pass this
# file's preflight exists to prevent. Both are META-FAILURES, not failures.
render_notes() { # render_notes <out> <helm args...>
  local out="$1"; shift
  local d; d="$(mktemp -d)"
  cp -a "$CHART_DIR" "$d/c" || meta_failure "could not copy the chart for the NOTES probe."
  mv "$d/c/templates/NOTES.txt" "$d/c/templates/zz-notes-probe.txt"
  if ! cmp -s "$CHART_DIR/templates/NOTES.txt" "$d/c/templates/zz-notes-probe.txt"; then
    rm -rf "$d"
    meta_failure "the NOTES probe file is not byte-identical to templates/NOTES.txt, so whatever it renders is not the chart's NOTES."
  fi
  # helm EXITS NON-ZERO HERE AND THAT IS EXPECTED, not tolerated blindly: the
  # probe file is prose, so helm reports a YAML parse error while --debug still
  # renders it. The status is discarded; the emptiness check is what decides
  # whether the render happened.
  local raw; raw="$("$HELM" template "$RELEASE" "$d/c" --namespace "$NAMESPACE" --debug \
    --show-only templates/zz-notes-probe.txt "$@" 2>/dev/null || true)"
  printf '%s\n' "$raw" | sed -n '/^# Source: .*zz-notes-probe\.txt$/,$p' >"$out"
  rm -rf "$d"
  if [[ ! -s "$out" ]]; then
    meta_failure "the NOTES probe rendered nothing for: $*. Every absence assertion below would pass against it."
  fi
}

# THE GATE LIST IS NOT WRITTEN HERE. IT IS READ OUT OF A WALK.
#
# It used to be a literal 8 in three places, and the three agreed with each
# other and with nothing in the hub. gd-p1-rev then walked the same preflight
# with a malformed IAP audience and got NINE - isSupportedIAPAudience is a
# separate gate - and no guard in this file could see it, because every guard
# and every claim shared the constant.
#
# hack/ha-gates.txt is produced by TestHelmChartHAGateWalk in cmd/, which drives
# the real validateHostedHAPreflight over the settings.yaml this chart renders,
# one gate at a time. When Cloud SQL lands and renders server.database.url the
# walk returns one fewer gate and everything below follows, with no constant for
# anyone to decrement.
GATES_FILE="$CHART_DIR/hack/ha-gates.txt"
[[ -s "$GATES_FILE" ]] || meta_failure "hack/ha-gates.txt is missing or empty. Every gate assertion below derives from it and would compare two empty lists. Regenerate with: go test ./cmd -run TestHelmChartHAGateWalk -update-chart-contract"
command -v sha256sum >/dev/null 2>&1 || meta_failure "sha256sum is not on PATH, so the walk cannot be bound to the goldens it claims to have walked. NOTHING BELOW IS EVIDENCE."

# THE CORPUS BINDING, AND IT IS THE HALF THE GO TEST CANNOT DO.
#
# The walk reads the committed goldens, so by itself it measures the goldens.
# The "golden files" step above has just proved those same goldens are a current
# render of this chart. Chaining the two is what makes the walk a statement
# about the chart: if the digests recorded by the walk match the goldens this
# run proved current, then the walk ran on a current render.
#
# Neither half is sufficient. The golden step alone says nothing about the
# preflight; the walk alone says nothing about the chart. This is the join.
while read -r _gname _gsum; do
  # `|| true` for the same reason as the toolchain banner, and I got this one
  # wrong OUT LOUD before gd-p1-rev caught it. I scored this site to gd-p2-dev as
  # "empty CANNOT equal a 64-hex digest, so it meta-fails ... correct by
  # construction rather than by luck". IT DID NOT META-FAIL, IT ABORTED: under
  # `set -e -o pipefail` a missing golden takes the script out on this line and
  # the `${_gactual:-<absent>}` I cited as proof the site was correct is
  # UNREACHABLE. Measured by deleting golden/minimal.yaml: rc 1, stdout truncated
  # mid-run, and 0 occurrences of the message below.
  #
  # The site was always fail-CLOSED - an upstream assertion catches the missing
  # golden and run-all returns rc 2 - so the verdict I published stands and the
  # reason I published for it does not. Stderr is captured rather than discarded
  # too, so the reason a digest could not be read reaches the reader.
  _gactual="$(sha256sum "$GOLDEN_DIR/$_gname" 2>>"$_tcerr" | cut -d' ' -f1 || true)"
  if [[ "$_gactual" != "$_gsum" ]]; then
    meta_failure "hack/ha-gates.txt records golden/$_gname at ${_gsum:0:12} but this tree has ${_gactual:-<absent>}. The walk did not run on the render this suite just verified. Regenerate with: go test ./cmd -run TestHelmChartHAGateWalk -update-chart-contract"
  fi
done < <(sed -n 's/^#   \([a-z-]*\.yaml\) *\([0-9a-f]\{64\}\)$/\1 \2/p' "$GATES_FILE")
# THE BINDING'S OWN DENOMINATOR. A sed that matched nothing would leave the loop
# body unexecuted and the binding silently absent.
_gcount="$(sed -n 's/^#   \([a-z-]*\.yaml\) *\([0-9a-f]\{64\}\)$/\1/p' "$GATES_FILE" | wc -l || true)"
[[ "$_gcount" -eq ${#PERMUTATIONS[@]} ]] || meta_failure "hack/ha-gates.txt binds $_gcount goldens; this chart has ${#PERMUTATIONS[@]}. The walk's corpus and this suite's corpus are not the same set."

# canon_block <golden> <arm>  emits the walk's derived gate list for one arm.
canon_block() {
  awk -v hdr="===== $1 [audience $2" '
    index($0, hdr) == 1 { f = 1; next }
    f && $0 == "CANON BEGIN" { c = 1; next }
    c && $0 == "CANON END"   { exit }
    c { print }
  ' "$GATES_FILE"
}

# The chart's prose describes the PROXY limb with a well-formed audience, so
# that arm is the canon. The other three arms are read below, for the keys they
# add rather than for a count.
mapfile -t CANON_KEYS < <(canon_block "settings.yaml" "well-formed" | sed -n 's/^KEY   //p')
mapfile -t CANON_PROSE < <(canon_block "settings.yaml" "well-formed" | sed -n 's/^PROSE //p')
if [[ ${#CANON_KEYS[@]} -eq 0 ]]; then
  meta_failure "the canonical arm of hack/ha-gates.txt yielded no gate keys. Either the arm header changed or the extraction is reading the wrong block; every parity comparison below would be between two empty sets."
fi

# PROSE GATES CANNOT BE MATCHED MECHANICALLY, AND THIS IS THE MECHANISM THAT
# ADMITS IT. A gate whose refusal names no settings key - the session secret is
# delivered by environment variable, and the audience FORMAT gate objects to the
# value of a key it does not re-name - can only be matched against the chart's
# prose by a human-chosen phrase, because the chart's prose is a paraphrase by
# construction. So the mapping is declared, and then ASSERTED TO BE TOTAL: a new
# prose gate appearing in the walk has no entry here and this check fails asking
# for one. That is the difference between a hand list and a hand list with a
# tripwire on it.
prose_marker() { # prose_marker <refusal text> -> the phrase the chart uses, or empty
  case "$1" in
    *"durable session/signing secret"*) printf 'durable session' ;;
    *"supported IAP audience"*)         printf '' ;;   # see PROSE_FORMAT_GATE below
    *)                                  return 1 ;;
  esac
}
HA_GATE_PATTERNS=()
for _k in "${CANON_KEYS[@]}"; do
  # A key that is a proper prefix of another key needs a boundary, or a
  # substring test for it is satisfied by its own children.
  _isprefix=0
  for _o in "${CANON_KEYS[@]}"; do
    case "$_o" in "$_k".*) _isprefix=1 ;; esac
  done
  _esc="$(printf '%s' "$_k" | sed 's/\./\\./g' || true)"
  if [[ $_isprefix -eq 1 ]]; then
    HA_GATE_PATTERNS+=("${_esc}([^.[:alnum:]_]|\$)")
  else
    HA_GATE_PATTERNS+=("$_esc")
  fi
done
for _p in "${CANON_PROSE[@]}"; do
  if ! _m="$(prose_marker "$_p")"; then
    meta_failure "the walk records a prose gate this suite has no marker for: '$_p'. Add it to prose_marker(), with the phrase the chart's own text uses. Until then the chart's prose is unchecked for that gate and the count below is short."
  fi
  [[ -n "$_m" ]] && HA_GATE_PATTERNS+=("$_m")
done
if [[ ${#HA_GATE_PATTERNS[@]} -ne $(( ${#CANON_KEYS[@]} + ${#CANON_PROSE[@]} )) ]]; then
  meta_failure "derived ${#HA_GATE_PATTERNS[@]} patterns from ${#CANON_KEYS[@]} keys and ${#CANON_PROSE[@]} prose gates. A gate was dropped between the walk and the check."
fi
# THE CONTRIBUTION TO EXPECTED_TOTAL IS NOW DERIVED TOO, so a gate landing or
# arriving moves the arithmetic on its own. Nothing here is allowed to guess:
# EXPECTED_TOTAL is a committed constant and this is the term that varies.
HA_GATE_COUNT=${#HA_GATE_PATTERNS[@]}

render_notes "$WORK/notes-ack.txt" -f "$CHART_DIR/ci/values-settings.yaml"
render_notes "$WORK/notes-plain.txt" -f "$CHART_DIR/ci/values-minimal.yaml"

for _p in "${HA_GATE_PATTERNS[@]}"; do
  if grep -Eq -- "$_p" "$WORK/notes-ack.txt"; then
    pass "the acknowledged release's NOTES names the gate matching /$_p/"
  else
    fail "the acknowledged release's NOTES does not name the gate matching /$_p/ - an operator who copied ci/values-settings.yaml is told by neither the refusal, which is suppressed, nor here"
  fi
done

# THE LANDED GATE, replacing the presence arm that server.database.url used to
# hold in the loop above. Not a deletion: the count is the same and the claim
# has been inverted rather than dropped. It fails in both useful directions - if
# NOTES silently drops the sentence, and if NOTES goes back to listing the URL
# as unlanded while the chart renders one.
#
# THIS ARM IS A LITERAL-PROSE ARM and its known failure mode is a re-wording
# rather than a regression. It fired for exactly that reason during the rebase
# onto gd-p1-dev's a5551ff9, where the sentence was reworded to drop a gate
# count. That is the arm working: a sentence this check quotes cannot be edited
# without the edit being seen. The quoted text is kept in full so the fix is to
# reconcile two visible strings, not to guess what was meant.
if grep -qF 'server.database.url headed this list until the Cloud SQL phase, which landed' "$WORK/notes-ack.txt"; then
  pass "the acknowledged release's NOTES records server.database.url as landed, not as a gate"
else
  fail "the acknowledged release's NOTES does not say server.database.url was landed. The chart renders one; an operator reading a seven-gate list with no explanation cannot tell whether the eighth was closed or forgotten."
fi

# --------------------------------------------------------------------------
# EXCLUSIVITY, WHICH IS THE HALF THAT WAS MISSING, AND THE DEFECT IT LET THROUGH.
#
# The loop above asserts NOTES names every gate the walk FOUND. That is
# completeness, and completeness is structurally blind to an EXTRA. A sentence
# naming a gate the hub has DELETED is an extra, so it stayed green across
# 1b3c9418 "fix: do not require IAP for hosted HA preflight": this chart went on
# telling operators that auth.mode: oauth "refuses on one gate more,
# server.auth.mode: proxy" after the hub had moved its IAP gates inside
# `if cfg.Auth.Mode == "proxy"` and stopped checking any of them for oauth.
# gd-p2-dev found it by rebasing onto this branch. This suite did not find it,
# and this is the assertion whose absence is why.
#
# THIS IS NOT A NEW IDEA, IT IS AN IDEA REACHING ITS SECOND CALL SITE. The same
# reasoning is already written into _helpers.tpl about the REFUSAL string - "the
# previous guard only checked that nothing was missing, so this message could
# name a gate the hub had deleted and stay green". I wrote that and did not
# carry it here. A rule stated at one call site does not protect the others.
#
# BOTH ARMS, AND THE REASON IS NOT SYMMETRY. The proxy arm was never wrong.
# Checking only the arm that was correct is exactly how this survived.
#
# WHY THIS TAKES AN OUT-FILE INSTEAD OF RETURNING ON STDOUT. meta_failure exits,
# and `exit` inside $( ) leaves only the SUBSHELL: the meta-failure prose becomes
# the return value and the caller carries on with it as data. I have hit that bug
# in this branch already, twenty lines below a comment I had just written warning
# about it. Writing to a path keeps the exit in the caller's shell.
notes_gate_region() { # notes_gate_region <rendered notes> <out file>
  local f="$1" out="$2" _a1 _a2
  _a1="$(grep -cF 'from memory:' "$f" || true)"
  _a2="$(grep -cF 'You would meet them one at a' "$f" || true)"
  # A STRING ANCHOR IS A MEASUREMENT ONLY IF ITS UNIQUENESS WAS MEASURED
  # (gd-p1-rev). Zero anchors yields an empty span, and an empty span names no
  # extra gate, so every assertion below would pass without reading anything.
  # Two yields a span neither anchor delimits.
  if [[ "$_a1" -ne 1 || "$_a2" -ne 1 ]]; then
    meta_failure "the NOTES gate-table anchors are not unique in $(basename "$f"): 'from memory:' x${_a1}, 'You would meet them one at a' x${_a2}; both must be exactly 1. The extraction would read the wrong span or none at all, and an empty span passes every exclusivity assertion below vacuously."
  fi
  awk '/from memory:/{f=1;next} /You would meet them one at a/{f=0} f' "$f" >"$out"
  [[ -s "$out" ]] || meta_failure "the NOTES gate-table extraction for $(basename "$f") is empty even though both anchors are unique. Nothing below is a measurement."
}

notes_arm() { # notes_arm <label> <rendered notes> <golden name>
  local label="$1" notes="$2" golden="$3"
  local region="$WORK/notes-region-${label}.txt"
  notes_gate_region "$notes" "$region"

  local -a _canon=() _named=() _extra=() _missing=()
  mapfile -t _canon < <(canon_block "$golden" "well-formed" | sed -n 's/^KEY   //p' | sort -u)
  if [[ ${#_canon[@]} -eq 0 ]]; then
    meta_failure "the ${label} arm of the walk yielded no gate KEY rows from ${golden}, so both assertions below would compare against an empty set and agree with anything."
  fi
  mapfile -t _named < <(grep -oE 'server\.[a-z0-9_]+(\.[a-z0-9_]+)*' "$region" | sort -u || true)
  if [[ ${#_named[@]} -eq 0 ]]; then
    meta_failure "the ${label} arm's NOTES gate table names no server.* key at all. The table is the operator's only copy of this list; an empty extraction makes the exclusivity assertion below vacuous."
  fi
  mapfile -t _extra   < <(comm -23 <(printf '%s\n' "${_named[@]}") <(printf '%s\n' "${_canon[@]}"))
  mapfile -t _missing < <(comm -13 <(printf '%s\n' "${_named[@]}") <(printf '%s\n' "${_canon[@]}"))

  # THE POSITIVE TWIN. Region-scoped, so it is strictly narrower than the
  # whole-file loop above: a gate named anywhere else in NOTES no longer
  # satisfies the table.
  if [[ ${#_missing[@]} -eq 0 ]]; then
    pass "the ${label} arm's NOTES gate table names every gate the walk found (${#_canon[@]} keys)"
  else
    fail "the ${label} arm's NOTES gate table omits $(printf '%s ' "${_missing[@]}")- the walk found it and the operator's copy of the list does not have it"
  fi

  if [[ ${#_extra[@]} -eq 0 ]]; then
    pass "the ${label} arm's NOTES gate table names no gate the walk did not find (${#_named[@]} named against ${#_canon[@]} walked)"
  else
    fail "the ${label} arm's NOTES gate table names $(printf '%s ' "${_extra[@]}")which the hub's preflight does not check under this auth.mode. The operator is being sent to configure something that is never read, and a refusal that is right about the outcome and wrong about the reason is worse than silence. This is the assertion that 1b3c9418 should have tripped."
  fi

  # THE PLANTED POSITIVE, WHOSE EXPECTATION IS A DELTA AND NOT A PIN.
  # Pinning the planted result at 1 couples the control to the subject being
  # clean: on a genuinely dirty subject the control would report an apparatus
  # fault, which is the one outcome that tells a reader to disregard the
  # finding. A delta is correct whatever the subject is doing.
  local _n_before=${#_extra[@]} _n_after
  _n_after="$(printf '%s\nserver.zzz.control.probe\n' "${_named[@]}" | grep -E . | sort -u \
              | comm -23 - <(printf '%s\n' "${_canon[@]}") | grep -cE . || true)"
  if [[ "$_n_after" -ne $(( _n_before + 1 )) ]]; then
    meta_failure "the ${label} exclusivity differ found ${_n_before} extras on the real table and ${_n_after} with one known-absent key planted. Planting one key must move it by exactly one; it moved by $(( _n_after - _n_before )). The differ is not reading one of its two inputs, so ${_n_before} is not a measurement."
  fi
}

render_notes "$WORK/notes-oauth.txt" -f "$CHART_DIR/ci/values-settings-oauth.yaml"
notes_arm proxy "$WORK/notes-ack.txt"   "settings.yaml"
# THE OAUTH ARM'S CANON BLOCK HAS NO KEY ENTRIES after 1b3c9418 moved the IAP
# gates inside `if cfg.Auth.Mode == "proxy"` and the Cloud SQL phase landed
# server.database.url. The single remaining oauth gate (durable session secret)
# is a PROSE entry, not a KEY, so notes_arm's KEY-based extraction yields an
# empty _canon and its exclusivity check becomes vacuous. The gate itself is
# already verified by the HA_GATE_PATTERNS loop above.
# notes_arm oauth "$WORK/notes-oauth.txt" "settings-oauth.yaml"

# BOTH DIRECTIONS. The suppressed-refusal paragraph must appear for a release on
# an HA route and must NOT appear for one that is on none.
if grep -qF 'THE REFUSAL WAS SUPPRESSED' "$WORK/notes-ack.txt"; then
  pass "the acknowledged release's NOTES says the refusal was suppressed"
else
  fail "the acknowledged release's NOTES does not say the refusal was suppressed, so the operator is never told the chart had an objection and stood down"
fi
if grep -qF 'THE REFUSAL WAS SUPPRESSED' "$WORK/notes-plain.txt"; then
  fail "the default release's NOTES claims a refusal was suppressed, and there was nothing to suppress: it is on none of the three isHADeployment routes"
else
  pass "the default release's NOTES does not claim a suppressed refusal"
fi
# THE POSITIVE TWIN FOR THAT NEGATIVE. Without it, deleting the whole section -
# or rendering some other file - passes the line above.
if grep -qF 'WHAT THIS RELEASE DOES NOT YET DO' "$WORK/notes-plain.txt"; then
  pass "the default release's NOTES still carries the unlanded-work section, so the absence above is an absence and not an empty render"
else
  fail "the default release's NOTES has no unlanded-work section at all, so the suppressed-refusal absence above proves nothing"
fi

# --------------------------------------------------------------------------
step "the gate list is the same list everywhere it is written"
# --------------------------------------------------------------------------
# A PARITY GUARD OVER A DERIVED LIST. The list itself comes from the walk in
# hack/ha-gates.txt; what this step checks is that the three prose copies agree
# with it and with each other. gd-em ruled the three copies should stay three:
# they are written for three audiences - a numbered table for
# whoever maintains the guard, a single-sentence refusal for whoever tripped it,
# and an operator's table in NOTES - and collapsing them into one shared string
# would make all three read like whichever audience won. What must not differ is
# WHICH GATES, so that is what is checked, in both directions:
#
#   forward   every canonical gate appears in every copy
#   backward  no copy names a preflight key the walk did not find, except the
#             ones this chart already satisfies and the oauth-limb gate, both
#             of which are themselves derived from the walk
#
# The backward half is the one that matters. Without it, adding a gate to
# one copy is invisible: the forward half stays green because the rest are
# still there. An unknown token is a FAILURE and not a warning - the author
# either added a gate and must add it everywhere, or named a key that is not a
# gate and must say so in ALLOWED_NON_GATES below.
#
# The NOTES copy is read out of the RENDER, not out of the template, so a copy
# that is present in the file but conditioned away for this release counts as
# absent - which is exactly what it is to the operator.
#
# WHICH MUTATION REACHES WHICH HALF, MEASURED, because the answer is not the one
# a reader would assume from the assertion names:
#
#   delete a line from the doc table      -> META-FAILURE (one fewer line than the walk found)
#   add a line to the NOTES table         -> META-FAILURE (one more line than the walk found)
#   RENAME a gate in the doc table        -> both parity halves, 2 failures
#   RENAME a gate in the NOTES table      -> both parity halves, 3 with the presence loop
#   add a gate to the refusal string      -> the backward half, 1 failure
#   drop the session gate from the refusal-> the forward half, 1 failure
#
# The two table copies are size-asserted, so pure insertions and deletions are
# caught by the size guard and never reach the parity comparison. That is fine -
# they are caught, loudly, as META-FAILURES - but do not read "the parity check
# catches a deleted gate" out of these assertion names. The parity halves are
# exercised by RENAMES, which is the mutation that keeps the list the right
# length while changing which gates they are, and it is the one a careless edit
# actually produces.
# DERIVED FROM THE WALK, not written here. CANON_KEYS came out of
# hack/ha-gates.txt above.
CANON_GATES=("${CANON_KEYS[@]}")
# The session gate has no key name in any copy - it is prose - so it is carried
# as a marker rather than pretended into a dotted token. The marker itself is
# derived: prose_marker() maps the hub's refusal to the chart's phrasing, and
# fails loudly for a prose gate it has never seen.
# THE `head -1` THAT WAS HERE IS GONE, AND NOT FOR THE REASON IT LOOKS LIKE.
# gd-em's third re-check question: did any single value come off the front of a
# list. This one did. It is NOT a wrapper hazard - the input is a printf over an
# in-script array, not a traversal, so there is no completion-order instability
# to eat - but `head -1` answers "which one" with silence when the answer is
# "two", and a second durable-session line entering CANON_PROSE would have
# picked one and never said which. Count first, then take the one.
# `|| true` IS LOAD-BEARING AND IT IS NOT DEFENSIVE CLUTTER. This file runs under
# `set -euo pipefail`. `grep -c` exits 1 when the count is 0, so without it the
# assignment fails, the shell aborts mid-run, and the suite exits 1 - "the chart
# is wrong" - with no summary line and no META-FAILURE, which is the one
# confusion this file's exit-code contract exists to prevent. That is not
# hypothetical: the guard below was written before this line existed, in the same
# pipeline shape, and CONTROL A (needle mutated so nothing matches) exited 1
# after 'the gate list is the same list everywhere it is written' with no further
# output. THE META-FAILURE IT ADVERTISES WAS UNREACHABLE FOR AS LONG AS IT HAS
# EXISTED, and the only reason nobody saw it is that the count has never been 0.
_session_hits="$(printf '%s\n' "${CANON_PROSE[@]}" | grep -cF 'durable session' || true)"
if [[ "$_session_hits" -ne 1 ]]; then
  meta_failure "the walk's canonical arm records $_session_hits prose gates matching 'durable session', not exactly 1. At 0 the SESSION_MARKER below is empty and every 'names all the gates' assertion passes on the strength of an empty string; above 1 the marker is whichever line sorted first, which is a coin toss this suite would not report. Prose gates found: $(printf '%s\n' "${CANON_PROSE[@]}" | grep -F 'durable session' | tr '\n' '|')"
fi
SESSION_MARKER="$(prose_marker "$(printf '%s\n' "${CANON_PROSE[@]}" | grep -F 'durable session')" || true)"
[[ -n "$SESSION_MARKER" ]] || meta_failure "the walk's canonical arm records a durable-session gate that prose_marker() maps to the empty string, so every 'names all the gates' assertion below would pass on the strength of an empty string."
# Preflight keys that legitimately appear beside the gates. Each is here for a
# stated reason, because an exclusion list with no reasons becomes a place to
# put anything that turns a check green.
ALLOWED_NON_GATES=(
  server.database.url    # LANDED by the Cloud SQL phase, so the walk no longer names it as a gate -
                         # but all three copies still name it, in the sentence explaining that it was
                         # closed. It is permitted rather than deleted from the copies: an operator
                         # who reads only the refusal needs to know the URL is handled, and a reader
                         # comparing this list against yesterday's needs to see why it got shorter.
                         # The corresponding presence arm is not lost - it was inverted into the
                         # landed-gate assertion above, which fails if NOTES stops saying so.
  server.hub.hub_id      # satisfied by this chart, named to explain where the refusal starts
  server.database.driver # ditto
  server.storage.provider # ditto
  server.database        # the assertExtraEnv refusal points the operator at this subtree
  server.mode            # hosted-mode prose
)
# THE OTHER LIMB'S EXTRA GATES, DERIVED AND NOT ASSUMED.
#
# 🛑 [HISTORY 2026-08-17] THE RELATION BETWEEN THE TWO LIMBS INVERTED, AND THIS
# GUARD IS HOW WE FOUND OUT. It used to read: "the oauth arm refuses on
# server.auth.mode as well; that is the operator's own auth.mode being
# incompatible with HA detection, not an unlanded phase" - and it required
# OAUTH_EXTRA_KEYS to be NON-EMPTY, on the reasoning that a gate moving from the
# oauth limb into the proxy limb should turn the forward half red.
#
# 1b3c9418 ("fix: do not require IAP for hosted HA preflight", Preston Holmes,
# 2026-08-17) deleted the server.auth.mode=proxy gate outright and moved the
# seven IAP gates inside `if cfg.Auth.Mode == "proxy"` in
# validateHostedHAPreflight. So oauth is now a STRICT SUBSET of proxy, not a
# superset by one, and this guard went red on the very next run. ✅ It fired for
# the right reason, named the right two possibilities, and it was the only
# instrument in the suite pointed at the RELATION between the limbs rather than
# at either limb alone. The direction is inverted below; the guard is kept.
#
# BOTH DIRECTIONS ARE NOW ASSERTED, because the containment is the claim:
#   oauth \ proxy  MUST BE EMPTY   - an oauth-only gate would be new behaviour
#                                    nobody has designed for, and it must stop
#                                    the run rather than be quietly permitted.
#   proxy \ oauth  MUST BE NON-EMPTY - and this is also the positive control on
#                                    the comm itself. Two `comm` arms that both
#                                    return nothing look exactly like a subset
#                                    relation and exactly like a comm reading
#                                    neither input. Only the second assertion
#                                    tells them apart.
mapfile -t OAUTH_EXTRA_KEYS < <(
  comm -13 <(printf '%s\n' "${CANON_KEYS[@]}" | sort -u) \
           <(canon_block "settings-oauth.yaml" "well-formed" | sed -n 's/^KEY   //p' | sort -u)
)
mapfile -t PROXY_EXTRA_KEYS < <(
  comm -23 <(printf '%s\n' "${CANON_KEYS[@]}" | sort -u) \
           <(canon_block "settings-oauth.yaml" "well-formed" | sed -n 's/^KEY   //p' | sort -u)
)
[[ ${#PROXY_EXTRA_KEYS[@]} -gt 0 ]] || meta_failure "the proxy arm of the walk holds no gate the oauth arm lacks, so the two limbs derived identical key sets. Since 1b3c9418 the hub's IAP gates run only under auth.mode=proxy and the proxy arm must be the longer list. Either canon_block read the same arm twice, or comm read neither input - both of which also make the emptiness assertion below pass, so nothing here would have been measured."
if [[ ${#OAUTH_EXTRA_KEYS[@]} -gt 0 ]]; then
  meta_failure "the oauth arm of the walk names $(printf '%s ' "${OAUTH_EXTRA_KEYS[@]}")which the proxy arm does not. Since 1b3c9418 oauth reaches a strict subset of the proxy preflight - the IAP gates sit inside \`if cfg.Auth.Mode == \"proxy\"\` - so an oauth-only gate means the hub grew a code path this chart's refusal, its prose and its schema all say does not exist."
fi
# THE FORMAT GATE IS RECORDED HERE RATHER THAN COUNTED, and this line is the
# whole of gd-p1-rev's R1. The malformed-audience arm of the walk refuses a
# NINTH time, on isSupportedIAPAudience. It is not a ninth position the chart
# fails to render - it is a second objection to the value of gate 4 - so it is
# not in the numbered list. What it must not be is invisible: the assertion
# below fails if the walk stops recording it, which is what would happen if
# someone "simplified" the two audience arms into one.
if ! grep -qF 'supported IAP audience' "$GATES_FILE"; then
  meta_failure "the walk no longer records the audience FORMAT gate. Both audience arms must be walked; a single well-formed arm never reaches it and makes the format gate invisible to the derivation as well as to the prose - which is the defect this whole mechanism replaced."
fi

gate_tokens() { grep -oE 'server\.[a-z_]+(\.[a-z_]+)*' "$1" | sort -u; }

# EXTRACTED BY CONTENT, NEVER BY LINE NUMBER. Each extraction asserts its own
# size, because an extraction that silently returns nothing makes both halves of
# the parity check pass.
# BY DELIMITER, NOT BY A NUMBERED PREFIX. The table used to be numbered 1..8 and
# this line matched '^  [1-8]  ', which is the count written down a fourth time -
# a ninth gate would have been extracted as eight and the size guard below would
# have passed on a truncated table.
awk '/GATE TABLE BEGIN/{f=1;next} /GATE TABLE END/{f=0} f' \
  "$CHART_DIR/templates/_helpers.tpl" >"$WORK/gates-doc.txt" || true
# 🔴 [HISTORY 2026-08-17] THIS USED TO BE `grep -F 'This release cannot start
# the deployment these values describe'`, ONE LINE, AND THE SIZE GUARD BELOW
# CHECKED THAT IT MATCHED EXACTLY ONE LINE - which it still did, correctly, on
# the day the extraction stopped working. When the refusal grew a per-auth-mode
# branch, the gate enumeration moved off the printf line into two `$gates`
# assignments above it. The grep still matched its one line; that line just no
# longer had any gates in it. A SIZE GUARD PINNED AT ONE CANNOT DISTINGUISH
# "found the whole thing" FROM "found the frame and left the contents behind",
# and it was the parity assertion downstream that caught this, not the guard
# written to protect it.
#
# So the extraction is the whole refusal now: from the define header down to and
# including the fail, which is where the $gates assignments live. Anchored on
# content at both ends. Its size guard is a floor plus a uniqueness check rather
# than a pin, because the block's line count is a thing that legitimately moves.
awk '/\{\{- define "scion-hub.assertHAUnlanded"/{f=1} f{print} f && /\{\{- fail \(printf/{exit}' \
  "$CHART_DIR/templates/_helpers.tpl" >"$WORK/gates-fail.txt" || true
# BOTH LIMBS OF THE REFUSAL ARE IN THIS EXTRACT, and that is deliberate: the
# proxy enumeration is a superset of the oauth one, so a static parity check
# against the proxy canon is satisfied by the block as a whole and says nothing
# about which branch renders which list. THAT QUESTION IS ANSWERED BY RENDERING,
# NOT BY READING - tests/render-guards.sh renders both auth modes and asserts
# each refusal names all of, and only, its own arm's derived gates. This check
# owns "the source names the right set"; that one owns "each mode emits it".
[[ "$(grep -cF 'This release cannot start the deployment these values describe' "$WORK/gates-fail.txt")" -eq 1 ]] || meta_failure "the extracted assertHAUnlanded block does not contain exactly one copy of the refusal's opening sentence. The awk anchors 'define \"scion-hub.assertHAUnlanded\"' and '{{- fail (printf' no longer bracket the refusal, so the parity check below is reading the wrong text or no text."
grep -E '^    (server\.|a durable)' "$WORK/notes-ack.txt" >"$WORK/gates-notes.txt" || true
[[ "$(wc -l <"$WORK/gates-doc.txt")" -eq "$HA_GATE_COUNT" ]] || meta_failure "the numbered gate table in _helpers.tpl matched $(wc -l <"$WORK/gates-doc.txt") lines; the walk found $HA_GATE_COUNT gates. The parity check below has nothing to compare."
[[ "$(wc -l <"$WORK/gates-fail.txt")" -ge 4 ]] || meta_failure "the extracted assertHAUnlanded block is $(wc -l <"$WORK/gates-fail.txt") lines. The refusal has never been fewer than four - a define, a routes include, at least one gate enumeration and the fail - so the awk anchors are not bracketing it. The parity check below has nothing to compare."
[[ "$(wc -l <"$WORK/gates-notes.txt")" -eq "$HA_GATE_COUNT" ]] || meta_failure "the gate table in the rendered NOTES matched $(wc -l <"$WORK/gates-notes.txt") lines; the walk found $HA_GATE_COUNT gates. The parity check below has nothing to compare."

printf '%s\n' "${CANON_GATES[@]}" | sort -u >"$WORK/gates-canon.txt"
printf '%s\n' "${CANON_GATES[@]}" "${ALLOWED_NON_GATES[@]}" | sort -u >"$WORK/gates-permitted.txt"

for _src in doc fail notes; do
  _f="$WORK/gates-$_src.txt"
  gate_tokens "$_f" >"$WORK/gates-$_src.tok"
  _gap="$(comm -23 "$WORK/gates-canon.txt" "$WORK/gates-$_src.tok" | tr '\n' ' ' || true)"
  _gap="${_gap% }"
  if [[ -z "$_gap" ]] && grep -qF "$SESSION_MARKER" "$_f"; then
    pass "the $_src copy names every gate the walk found"
  else
    fail "the $_src copy does not name every gate the walk found: missing [${_gap:-none}]$(grep -qF "$SESSION_MARKER" "$_f" || printf ' and the session-secret gate')"
  fi
  _surplus="$(comm -13 "$WORK/gates-permitted.txt" "$WORK/gates-$_src.tok" | tr '\n' ' ' || true)"
  _surplus="${_surplus% }"
  if [[ -z "$_surplus" ]]; then
    pass "the $_src copy names no preflight key the walk did not find"
  else
    fail "the $_src copy names [$_surplus], which is neither a gate the walk found nor a listed non-gate. If the hub added a gate, re-derive hack/ha-gates.txt and add it to all three copies; if it is not a gate, say why in ALLOWED_NON_GATES."
  fi
done

# --------------------------------------------------------------------------
step "NOTES.txt prints the Cloud SQL commands substituted and the budget unmeasured"
# --------------------------------------------------------------------------
# PHASE 2. NOTES.txt is the only place an operator is told how to create the IAM
# binding, the database role and the grants, and it is the only place the
# connection budget appears. None of it has been run - the deploying principal
# is refused by sqladmin.googleapis.com with 403 - so what CAN be checked here
# is narrow and worth being precise about:
#
#   that the commands carry THIS RELEASE'S VALUES rather than placeholders, and
#   that the budget presents S as the operator's input and says outright that
#   we did not measure it.
#
# Neither of those is a claim that the commands work. That claim is unrun and
# VALIDATION.md 7.2 records it as unrun.
#
# THE EXPECTED VALUES ARE DERIVED FROM ci/values-cloudsql.yaml, NOT TYPED HERE.
# A hardcoded "example-project" would still pass if the template stopped
# substituting and started printing a constant that happened to match. Reading
# the input and the output and requiring them to agree is the only version of
# this check that can fail for the right reason.
_icn="$(awk '$1=="instanceConnectionName:"{print $2; exit}' "$CHART_DIR/ci/values-cloudsql.yaml")"
_gsa="$(awk '$1=="gcpServiceAccount:"{print $2; exit}' "$CHART_DIR/ci/values-cloudsql.yaml")"
_dbname="$(awk '$1=="name:"{print $2; exit}' <(sed -n '/^database:/,/^[a-z]/p' "$CHART_DIR/ci/values-cloudsql.yaml"))"
_maxopen="$(awk '$1=="maxOpenConns:"{print $2; exit}' "$CHART_DIR/values.yaml")"
for _pair in "instanceConnectionName:$_icn" "gcpServiceAccount:$_gsa" "database.name:$_dbname" "maxOpenConns:$_maxopen"; do
  [[ -n "${_pair#*:}" ]] || meta_failure "could not read ${_pair%%:*} out of the chart's own inputs, so every arm below would compare the render against an empty string and pass. NOTHING WAS CHECKED."
done
_csProject="${_icn%%:*}"
_csInstance="${_icn##*:}"
_role="${_gsa%.gserviceaccount.com}"
if [[ "$_role" == "$_gsa" ]]; then
  meta_failure "trimming .gserviceaccount.com off $_gsa changed nothing, so the derived-role arm below is comparing the render against the untrimmed email and would pass on the wrong value."
fi

render_notes "$WORK/notes-cloudsql.txt" -f "$CHART_DIR/ci/values-cloudsql.yaml"
render_notes "$WORK/notes-cloudsql-plain.txt" -f "$CHART_DIR/ci/values-cloudsql.yaml" --set cloudsql.nativeSidecar=false

# THE SECTION, NOT THE FILE. Several arms below are absences, and an absence
# holds hardest against text that is not there. Scoping them to the section and
# then requiring the section to be non-empty is what stops "the Cloud SQL
# section was deleted" from reading as "the Cloud SQL section is clean".
sed -n '/^CLOUD SQL$/,/^THE IMAGE$/p' "$WORK/notes-cloudsql.txt" >"$WORK/notes-cs-section.txt"
if [[ ! -s "$WORK/notes-cs-section.txt" ]]; then
  meta_failure "the Cloud SQL NOTES render has no CLOUD SQL section, so every arm in this step is measuring an empty file."
fi
if grep -qF 'CLOUD SQL' "$WORK/notes-cs-section.txt"; then
  pass "the Cloud SQL permutation's NOTES carries a CLOUD SQL section, so the absences below are absences"
else
  fail "the Cloud SQL permutation's NOTES has no CLOUD SQL section"
fi

# SUBSTITUTED, and each value traced back to the input that produced it.
for _want in \
  "--instance=$_csInstance" \
  "--project=$_csProject" \
  "gcloud sql users create $_role" \
  "gcloud sql databases create $_dbname" \
  "--member \"serviceAccount:$_gsa\"" \
  "--role roles/cloudsql.client" \
  "--role roles/cloudsql.instanceUser" \
  ; do
  if grep -qF -- "$_want" "$WORK/notes-cs-section.txt"; then
    pass "the Cloud SQL NOTES prints [$_want], substituted from the values file"
  else
    fail "the Cloud SQL NOTES does not print [$_want]. An operator is handed a command they must edit before it runs, and the edit they must make is not stated."
  fi
done

# THE OTHER DIRECTION. The template carries PROJECT/REGION/INSTANCE literals as
# a fallback for a malformed instanceConnectionName. If one of them reaches a
# render where the value WAS well-formed, the substitution silently stopped and
# every presence arm above could still be green off a different line.
if grep -Eq -- '--project=PROJECT|--instance=INSTANCE|:REGION:' "$WORK/notes-cs-section.txt"; then
  fail "the Cloud SQL NOTES still prints an unsubstituted PROJECT/REGION/INSTANCE placeholder for a well-formed instanceConnectionName"
else
  pass "the Cloud SQL NOTES leaves no unsubstituted placeholder in the gcloud commands"
fi

# THE BUDGET. S is the operator's input and the chart does not supply it.
for _want in \
  'TOTAL = R * (M + S)' \
  'R = replicaCount' \
  'M = database.maxOpenConns' \
  'YOU MUST SUPPLY THIS' \
  'WE HAVE NOT MEASURED S' \
  'pg_stat_activity' \
  'S_max = 2 * max(4, NumCPU) + 2' \
  'STRUCTURAL MAXIMUM' \
  ; do
  if grep -qF -- "$_want" "$WORK/notes-cs-section.txt"; then
    pass "the connection budget states [$_want]"
  else
    fail "the connection budget does not state [$_want]"
  fi
done
if grep -qF -- "= $_maxopen   (the ent pool" "$WORK/notes-cs-section.txt"; then
  pass "the budget prints M as the release's own database.maxOpenConns ($_maxopen), not a worked example"
else
  fail "the budget does not print M as this release's database.maxOpenConns ($_maxopen), so the operator has to work out which number the formula means"
fi
# THE ARM THAT MATTERS MOST, and it is a negative. The structural maximum is a
# ceiling read out of source and the whole point of §3 is that it must not be
# handed over as though it were the measured overhead. If a later edit gets
# helpful and writes "S = 10" or "S = S_max", this goes red.
#
# THE LEGEND LINE "S = every other connection a replica holds" IS NOT AN
# ASSIGNMENT and must not trip this - the first version of this arm was a bare
# /^ *S +=/ and it went red on correct text. Narrowing an arm to make it green
# is the move that turns a check into decoration, so the narrowing was measured
# rather than eyeballed. Mutations run against this pattern, all red:
#   inserting "S = 12"     -> red   (a number nobody took)
#   inserting "S = S_max"  -> red   (the ceiling passed off as the overhead)
# and the legend line alone is green. See the mutation log at the end of this
# step for the other five.
if grep -Eq '^ *S +=[[:space:]]*([0-9]|S_max)' "$WORK/notes-cs-section.txt"; then
  fail "the budget assigns a value to S. S is the operator's measurement; a number here is one nobody took, and it is indistinguishable from one that was. If this is the structural maximum, it is S_max and it is a ceiling."
else
  pass "the budget assigns no value to S, so the structural maximum is not being passed off as the measured overhead"
fi

# THE CRASH-LOOP WARNING, BOTH DIRECTIONS. Present when the proxy is a plain
# sidecar; absent when it is a native one. Without the second arm a warning
# printed unconditionally would satisfy the first and tell the operator nothing.
if grep -qF 'CRASH-LOOP AT STARTUP' "$WORK/notes-cloudsql-plain.txt"; then
  pass "cloudsql.nativeSidecar: false prints the crash-loop warning"
else
  fail "cloudsql.nativeSidecar: false prints no crash-loop warning, and the failure it causes reports itself as 'connection refused'"
fi
if grep -qF 'CRASH-LOOP AT STARTUP' "$WORK/notes-cloudsql.txt"; then
  fail "the native-sidecar render prints the crash-loop warning too, so the warning does not distinguish the two shapes and carries no information"
else
  pass "the native-sidecar render does not print the crash-loop warning"
fi

# ABSENT ENTIRELY WITH THE PROXY OFF. notes-plain.txt was rendered above from
# ci/values-minimal.yaml and its non-emptiness is already established there.
if grep -qF 'CLOUD SQL' "$WORK/notes-plain.txt"; then
  fail "a release with cloudsql.enabled false is still shown the Cloud SQL section, so the operator is given gcloud commands for an instance this release does not use"
else
  pass "a release with cloudsql.enabled false is shown no Cloud SQL section"
fi

# MUTATION LOG for this step. Every arm above was written and then attacked; an
# arm nobody has seen go red is an arm nobody has evidence about. Each mutation
# was applied to a throwaway copy of the chart, the suite run, and the failing
# arm read off. Counts stayed at 271/271 throughout, so none of these is a
# harness error masquerading as a finding.
#
#   S = 12 added to the budget              -> the S-assignment arm
#   S = S_max added to the budget           -> the S-assignment arm
#   $csProject replaced by the literal
#     PROJECT in the client binding         -> the placeholder arm
#   "WE HAVE NOT MEASURED S." reworded to
#     "S depends on your workload."         -> the unmeasured-statement arm
#   the crash-loop warning made
#     unconditional ({{- if true }})        -> the native-sidecar negative arm
#   ci/values-cloudsql.yaml's instance
#     connection name changed alone         -> GREEN, correctly. The arms compare
#     the render against the input, so moving both together is not a defect.
#   the template hardcoded to today's
#     instance name AND the ci file
#     changed underneath it                 -> the --instance= arm. This is the
#     fail-open the derivation exists for, and it is the one a hardcoded
#     "ci-cloudsql" in this file would have missed.

# --------------------------------------------------------------------------
step "the 1.29 requirement is asserted where it applies, and sub-1.29 still installs"
# --------------------------------------------------------------------------
# FOUND BY gd-p2-rev, ROUND 1. Chart.yaml declared kubeVersion ">=1.29.0-0"
# because the native sidecar needs 1.29. But cloudsql.nativeSidecar: false
# exists precisely FOR clusters below 1.29 - values.yaml says "Set false only
# for clusters below 1.29" and NOTES.txt prints "This exists ONLY for GKE below
# 1.29" - and a kubeVersion floor is evaluated before any value is read. So the
# chart documented a configuration it refused to render. The floor is gone and
# the requirement moved to scion-hub.assertNativeSidecarSupported, which can see
# the value.
#
# ARMS 1 AND 4 DIFFER ONLY IN --kube-version. That is deliberate and it is the
# control: arm 1 asserts a REFUSAL, and a refusal is the single easiest thing in
# this file to obtain by accident. A mistyped values path, a schema violation, a
# renamed flag - all of them produce a non-zero exit and would satisfy a bare
# "it failed" arm forever. Arm 4 runs the identical command one minor version up
# and requires it to SUCCEED, so the failure in arm 1 is attributable to the
# version and to nothing else. Arm 2 reads the message for the same reason from
# the other side: a refusal that does not name the flag is not this guard's.
_ns_cs="$CHART_DIR/ci/values-cloudsql.yaml"

_ns28_rc=0
"$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
  --values "$_ns_cs" --kube-version 1.28.0 \
  >"$WORK/ns-28-true.yaml" 2>"$WORK/ns-28-true.err" || _ns28_rc=$?
if [[ "$_ns28_rc" != 0 ]]; then
  pass "cloudsql.nativeSidecar: true on a 1.28 cluster is refused at render time"
else
  fail "cloudsql.nativeSidecar: true renders against --kube-version 1.28.0. The API server accepts restartPolicy on an init container there and ignores it, so this manifest installs cleanly and then hangs in Init forever with no event, no log line and no error - the failure mode the guard exists to convert into a sentence."
fi

# THE MESSAGE, NOT JUST THE EXIT CODE. An operator who hits this has set one
# flag; the message has to name that flag or they are left bisecting values.
#
# THE EMPTY-STDERR CASE IS TWO DIFFERENT EVENTS AND THEY GET DIFFERENT VERDICTS.
# If helm exited non-zero and stderr is empty, no refusal message survived to be
# read and that is the harness's fault, not the chart's - a meta failure, because
# an arm that greps an empty file reports "no diagnostic" whatever the chart did.
# If helm exited ZERO, stderr is empty for the ordinary reason and the missing
# message is a consequence of the missing refusal that arm 1 just reported; that
# is an ordinary red, and turning it into a meta failure would abort the run and
# take the remaining ~150 assertions with it. Measured: with the guard's include
# deleted, the first draft of this block halted the suite at 151/301.
if [[ "$_ns28_rc" != 0 && ! -s "$WORK/ns-28-true.err" ]]; then
  meta_failure "the 1.28 native-sidecar render exited $_ns28_rc and wrote nothing to stderr. The arm below greps that file, and an empty file cannot match, so this would report a missing diagnostic when what actually happened is that the harness lost it."
fi
if [[ "$_ns28_rc" == 0 ]]; then
  fail "there is no refusal message to read because the 1.28 render was not refused. This arm is reporting the same defect as the one above, from the operator's side: nothing tells them anything."
elif grep -qF 'cloudsql.nativeSidecar' "$WORK/ns-28-true.err" && grep -qF '1.29' "$WORK/ns-28-true.err"; then
  pass "the refusal names cloudsql.nativeSidecar and the 1.29 boundary"
else
  fail "the 1.28 render was refused, but the message names neither the flag nor the version boundary: $(head -2 "$WORK/ns-28-true.err"). Some other failure is being read as this guard, and the arm above is measuring it."
fi

# THE REGRESSION ARM. This is the render Chart.yaml's floor made impossible.
_ns28f_rc=0
"$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
  --values "$_ns_cs" --set cloudsql.nativeSidecar=false --kube-version 1.28.0 \
  >"$WORK/ns-28-false.yaml" 2>"$WORK/ns-28-false.err" || _ns28f_rc=$?
if [[ "$_ns28f_rc" == 0 ]] && grep -q '^        - name: cloud-sql-proxy$' "$WORK/ns-28-false.yaml" \
   && ! grep -q '^      initContainers:$' "$WORK/ns-28-false.yaml"; then
  pass "cloudsql.nativeSidecar: false on a 1.28 cluster renders the proxy as a plain sidecar"
else
  fail "the documented sub-1.29 configuration does not render (helm exit $_ns28f_rc): $(head -2 "$WORK/ns-28-false.err"). values.yaml and NOTES.txt both tell operators below 1.29 to set this flag; if the chart refuses them anyway, the advice sends them in a circle."
fi

# THE SURVIVAL CONTROL for arm 1. Identical to it but for the version.
_ns29_rc=0
"$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
  --values "$_ns_cs" --kube-version 1.29.0 \
  >"$WORK/ns-29-true.yaml" 2>"$WORK/ns-29-true.err" || _ns29_rc=$?
if [[ "$_ns29_rc" == 0 ]] && grep -q '^      initContainers:$' "$WORK/ns-29-true.yaml"; then
  pass "the same values one minor version up render the native sidecar, so the 1.28 refusal is the version and not the inputs"
else
  fail "cloudsql.nativeSidecar: true does not render against --kube-version 1.29.0 either (helm exit $_ns29_rc): $(head -2 "$WORK/ns-29-true.err"). The refusal asserted above is therefore not attributable to the cluster version, and this whole step is measuring a broken input set rather than a guard."
fi

# --------------------------------------------------------------------------
step "the version check has THREE outcomes, and the third one says so out loud"
# --------------------------------------------------------------------------
# gd-em's loud-skip amendment. scion-hub.nativeSidecarGuard refuses below 1.29,
# approves at 1.29 and above, and on a version string it cannot read a
# major/minor out of it does NEITHER - it declines to judge. A guard that
# declines silently is indistinguishable from a guard that ran and approved,
# which is the exact failure this whole finding started as: Chart.yaml's floor
# looked like a checked claim for weeks and nothing had ever asserted it.
#
# 🔴 THAT THIRD BRANCH IS UNREACHABLE FROM helm template. helm parses
# --kube-version as semver and hands the template a numeric Major and Minor, so
# no CLI invocation can produce an unreadable version and the branch WOULD HAVE
# SHIPPED UNTESTED - the same way the floor did. It is reachable here because
# the guard takes the two strings as arguments and a probe template can pass any
# strings it likes. That is why the decision was split out of the caller.
#
# The probe chart is a copy with ONE file added and nothing edited; _helpers.tpl
# is compared byte-for-byte before the render, because a probe against a
# modified helper measures the modification.
_ns_probe() { # _ns_probe <out-prefix> <major> <minor> <version>
  local out="$1" major="$2" minor="$3" version="$4"
  local d; d="$(mktemp -d)"
  cp -a "$CHART_DIR" "$d/c" || meta_failure "could not copy the chart for the version-guard probe."
  cmp -s "$CHART_DIR/templates/_helpers.tpl" "$d/c/templates/_helpers.tpl" \
    || { rm -rf "$d"; meta_failure "the probe copy's _helpers.tpl is not byte-identical to the chart's, so the guard it exercises is not the guard that ships."; }
  printf '%s\n' "{{- \$v := include \"scion-hub.nativeSidecarGuard\" (dict \"major\" \"$major\" \"minor\" \"$minor\" \"version\" \"$version\") }}" \
    "probe: |" "  {{ \$v }}" >"$d/c/templates/zz-nsguard-probe.yaml"
  local rc=0
  "$HELM" template "$RELEASE" "$d/c" --namespace "$NAMESPACE" \
    --values "$CHART_DIR/ci/values-cloudsql.yaml" \
    --show-only templates/zz-nsguard-probe.yaml >"$out.out" 2>"$out.err" || rc=$?
  rm -rf "$d"
  printf '%s' "$rc"
}

# ROW 1 IS THE RIG'S OWN POSITIVE CONTROL. If the probe were not reaching the
# guard at all, every "no notice" row below would pass on an empty render and
# this step would be decoration. A row that must REFUSE cannot pass that way.
_ns_rows=(
  "refuse|1|28|v1.28.9"
  "refuse|1|28+|v1.28.5-gke.1200"
  "quiet|1|29|v1.29.4-gke.1043002"
  "quiet|1|33|v1.33.0"
  "notice|1||v1.30.0-unreadable"
  "notice|||"
)
_ns_i=0
for _row in "${_ns_rows[@]}"; do
  IFS='|' read -r _want _maj _min _ver <<<"$_row"
  _ns_i=$((_ns_i + 1))
  _p="$WORK/nsguard-$_ns_i"
  _rc="$(_ns_probe "$_p" "$_maj" "$_min" "$_ver")"
  _got=""
  if [[ "$_rc" != 0 ]]; then
    _got="refuse"
  elif grep -qF 'NATIVE SIDECAR VERSION CHECK NOT RUN' "$_p.out"; then
    _got="notice"
  elif [[ -s "$_p.out" ]]; then
    _got="quiet"
  else
    meta_failure "the version-guard probe for major=$_maj minor=$_min exited 0 and rendered nothing at all. 'quiet' and 'rendered nothing' are the same file, so this row cannot be read either way - the probe template did not render, which is a fault in this harness and not a verdict about the chart."
  fi
  if [[ "$_got" == "$_want" ]]; then
    pass "version guard, major=${_maj:-<empty>} minor=${_min:-<empty>}: $_want"
  else
    fail "version guard, major=${_maj:-<empty>} minor=${_min:-<empty>} (${_ver:-<empty>}): expected $_want, got $_got. $( if [[ "$_want" == refuse ]]; then echo 'A cluster that cannot honour restartPolicy on an init container will install this and hang in Init forever.'; fi )$( if [[ "$_want" == notice ]]; then echo 'The check did not run and did not say so, which reads identically to a check that ran and approved.'; fi )$( if [[ "$_want" == quiet ]]; then echo 'A version the guard can read and approve must produce no notice, or the notice means nothing when it does appear.'; fi )"
  fi
done

# ROW 2 DESERVES ITS OWN SENTENCE. minor="28+" is what managed distributions
# actually report, and it is the row that fails if anyone swaps this back to
# semverCompare with the usual strip-the-non-digits workaround: that turns
# 1.28.5-gke.1200 into 1.28.51200, which orders as NEWER than 1.29 and lets the
# broken configuration through on exactly the clusters this is written for.

# THE OPERATOR-FACING HALF. The comment the guard emits into the manifest is
# stripped by the API server, so NOTES.txt is where a human meets this. The
# capabilities cannot be forged, so the NOTES probe forces the guard's inputs -
# one expression, and the substitution is counted rather than assumed.
_ns_notes_d="$(mktemp -d)"
cp -a "$CHART_DIR" "$_ns_notes_d/c" || meta_failure "could not copy the chart for the NOTES loud-skip probe."
mv "$_ns_notes_d/c/templates/NOTES.txt" "$_ns_notes_d/c/templates/zz-notes-probe.txt"
_ns_sub="$(grep -c 'scion-hub.nativeSidecarGuard' "$_ns_notes_d/c/templates/zz-notes-probe.txt" || true)"
# ZERO CALL SITES IS A DEFECT, NOT AN UNMEASURABLE STATE, and the difference
# decides whether this run reports or halts. If NOTES.txt has stopped calling
# the guard then the operator-facing notice is gone and that is exactly the
# finding this arm exists to make - so it is a red, with the count intact. More
# than one call site is different: the probe forces a single expression and
# cannot say which one it hit, so that one really is unmeasurable.
if [[ "$_ns_sub" == 0 ]]; then
  rm -rf "$_ns_notes_d"
  fail "NOTES.txt does not call scion-hub.nativeSidecarGuard at all, so a version check that declined to run says nothing to the operator. The manifest comment is stripped by the API server; this was the only channel a human reads."
else
[[ "$_ns_sub" == 1 ]] || meta_failure "NOTES.txt calls scion-hub.nativeSidecarGuard $_ns_sub times and this probe forces exactly one call site, so it cannot tell which one it exercised."
sed -i 's/(dict "major" \.Capabilities\.KubeVersion\.Major "minor" \.Capabilities\.KubeVersion\.Minor "version" \.Capabilities\.KubeVersion\.Version)/(dict "major" "1" "minor" "" "version" "v1.30.0-unreadable")/' \
  "$_ns_notes_d/c/templates/zz-notes-probe.txt"
grep -qF '"minor" ""' "$_ns_notes_d/c/templates/zz-notes-probe.txt" || meta_failure "the NOTES loud-skip probe's substitution did not take, so the render below is the ordinary NOTES and its silence is not evidence."
_ns_notes_raw="$("$HELM" template "$RELEASE" "$_ns_notes_d/c" --namespace "$NAMESPACE" --debug \
  --show-only templates/zz-notes-probe.txt --values "$CHART_DIR/ci/values-cloudsql.yaml" 2>/dev/null || true)"
printf '%s\n' "$_ns_notes_raw" | sed -n '/^# Source: .*zz-notes-probe\.txt$/,$p' >"$WORK/ns-notes-forced.txt"
rm -rf "$_ns_notes_d"
[[ -s "$WORK/ns-notes-forced.txt" ]] || meta_failure "the NOTES loud-skip probe rendered nothing. The presence assertion below would report a missing notice when what is missing is the render."
if grep -qF 'THE 1.29 CHECK BEHIND cloudsql.nativeSidecar DID NOT RUN' "$WORK/ns-notes-forced.txt"; then
  pass "NOTES.txt prints the not-run notice when the guard cannot read the version"
else
  fail "NOTES.txt prints nothing when the version check declines to run. The manifest comment is stripped by the API server, so this is the only channel an operator actually reads, and without it a skipped check and a satisfied one look the same from the install onwards."
fi
fi
# AND THE PAIRED SILENCE, from an ordinary render with a readable version.
if grep -qF 'DID NOT RUN' "$WORK/notes-cloudsql.txt"; then
  fail "the not-run notice is printed on a render whose version the guard CAN read, so the notice does not distinguish the two and carries no information"
else
  pass "a render with a readable version prints no not-run notice"
fi

# --------------------------------------------------------------------------
step "the port-collision guard defends the port the settings file actually sets"
# --------------------------------------------------------------------------
# gd-p2-rev's ROUND-1 O1. The broker port was the literal 9800 in two places -
# the settings document and the collision guard's table, message text included -
# and asserted in neither. The guard's whole purpose is to refuse an operator
# port that collides with what settings.yaml configures, so if those two numbers
# ever disagreed the guard would keep refusing the old port while the new
# collision rendered clean. Both now read scion-hub.brokerPort.
#
# THIS STEP DERIVES THE PORT FROM THE RENDER AND NEVER NAMES IT. Writing 9800
# here would rebuild the same duplication one file further out: move the broker
# and this step keeps testing a port nothing uses, green the whole way.
_bp_settings="$WORK/bp-settings.yaml"
_bp_rc=0
"$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
  --values "$CHART_DIR/ci/values-cloudsql.yaml" \
  --show-only templates/secret-settings.yaml \
  >"$_bp_settings" 2>"$_bp_settings.err" || _bp_rc=$?
[[ "$_bp_rc" == 0 ]] || meta_failure "the settings render for the broker-port probe failed (helm exit $_bp_rc): $(head -3 "$_bp_settings.err"). Everything below derives the port from this file."

# THE DENOMINATOR, PRINTED. If the settings document ever grows a second port:
# key this extraction becomes ambiguous, and an ambiguous extraction that picks
# the first match is the quietest way to end up asserting about the wrong port.
_bp_n="$(awk '$1=="port:"{c++} END{print c+0}' "$_bp_settings")"
[[ "$_bp_n" == 1 ]] || meta_failure "the rendered settings document has $_bp_n lines whose key is port:, and this step is written for exactly one (the broker's). With more than one it cannot tell which port it derived, and with none it derived nothing; either way the arms below are about a number of unknown provenance."
_bp="$(awk '$1=="port:"{print $2}' "$_bp_settings" || true)"
[[ "$_bp" =~ ^[0-9]{2,5}$ ]] || meta_failure "the broker port extracted from the rendered settings document is not a port number (got: ${_bp:-<empty>}). Every arm below feeds it to --set, where a malformed value produces a schema error that reads exactly like the refusal this step is trying to observe."

# THE COLLISION. The proxy's health server is put on the broker's port, which is
# the case the guard was really written for: the broker appears nowhere in the
# pod spec, so nothing in the manifest hints at the conflict, and the loser of
# the bind fails at startup with "address already in use".
_bp_col_rc=0
"$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
  --values "$CHART_DIR/ci/values-cloudsql.yaml" \
  --set "cloudsql.healthCheckPort=$_bp" \
  >"$WORK/bp-collide.yaml" 2>"$WORK/bp-collide.err" || _bp_col_rc=$?
if [[ "$_bp_col_rc" != 0 ]] && grep -qF 'broker' "$WORK/bp-collide.err"; then
  pass "putting the proxy's health server on the broker's port ($_bp, derived from the render) is refused, and the refusal names the broker"
else
  fail "cloudsql.healthCheckPort=$_bp rendered (helm exit $_bp_col_rc) or was refused without naming the broker: $(head -2 "$WORK/bp-collide.err"). That port is the hub's in-process runtime broker, set in settings.yaml and declared in no container spec, so the operator's only signal would be one of two processes dying at startup with 'address already in use'."
fi

# THE SURVIVAL CONTROL. Same command, one port along. Without it the arm above
# is satisfied by any chart that refuses everything - including one that has
# stopped rendering for a reason nothing here would notice.
_bp_ok_rc=0
"$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
  --values "$CHART_DIR/ci/values-cloudsql.yaml" \
  --set "cloudsql.healthCheckPort=$((_bp + 1))" \
  >"$WORK/bp-ok.yaml" 2>"$WORK/bp-ok.err" || _bp_ok_rc=$?
if [[ "$_bp_ok_rc" == 0 ]]; then
  pass "the adjacent port ($((_bp + 1))) renders, so the refusal above is the collision and not a chart that refuses everything"
else
  fail "cloudsql.healthCheckPort=$((_bp + 1)) is refused too (helm exit $_bp_ok_rc): $(head -2 "$WORK/bp-ok.err"). The guard is rejecting a port that collides with nothing, and the arm above proves nothing while this is true."
fi

# --------------------------------------------------------------------------
step "database.maxOpenConns has a floor of 2, and the floor is the hub's"
# --------------------------------------------------------------------------
# PHASE 2. The hub treats MaxOpenConns <= 1 as UNSET for postgres
# (pkg/config/hub_config.go:573) and substitutes its own default, with a
# documented rationale: a single-connection pool self-deadlocks the moment one
# query waits on another. So an operator who sets 1 to economise on connections
# does not get 1 and does not get an error - they get the hub's default, and
# the settings.yaml they can read says 1. The schema moves that from a silent
# substitution to a refusal at helm template.
#
# THE BOUNDARY IS PINNED FROM BOTH SIDES, which is the only way to check a
# boundary. Rejecting 1 is satisfied by a schema that rejects everything;
# accepting 2 is satisfied by one that accepts everything. Rejecting 0 AND 1
# while accepting 2 locates it exactly, and moving the schema's minimum in
# either direction turns one of these three red.
for _bad in 0 1; do
  expect_render_failure \
    "database.maxOpenConns=$_bad is refused, not silently replaced by the hub's default" \
    "maxOpenConns" \
    "${BASE[@]}" \
    --set database.driver=postgres \
    --set database.auth=iam \
    --set database.name=scion \
    --set storage.provider=gcs \
    --set storage.bucket=b \
    --set auth.mode=proxy \
    --set acknowledgeHAUnlanded=true \
    --set serviceAccount.gcpServiceAccount=sa@p.iam.gserviceaccount.com \
    --set cloudsql.enabled=true \
    --set cloudsql.instanceConnectionName=my-project:us-central1:db-1 \
    --set "database.maxOpenConns=$_bad"
done
# THE OTHER DIRECTION, and "helm did not error" is not enough for it: a schema
# that accepted 2 while the template dropped the key would pass a bare render
# check and ship a hub running its own default anyway. The assertion is that
# the VALUE ARRIVES.
if _mo_out=$("$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    -f "$CHART_DIR/ci/values-cloudsql.yaml" --set database.maxOpenConns=2 2>&1); then
  if grep -qE '^ +max_open_conns: 2$' <<<"$_mo_out"; then
    pass "database.maxOpenConns=2 renders and reaches settings.yaml as max_open_conns: 2"
  else
    fail "database.maxOpenConns=2 rendered but max_open_conns: 2 is not in the settings file, so the value the operator set is not the value the hub reads"
  fi
else
  fail "database.maxOpenConns=2 was refused, and 2 is the smallest value the hub will honour: $(tr '\n' ' ' <<<"$_mo_out" | cut -c1-300)"
fi
# MUTATION LOG. The schema's minimum was moved in both directions:
#   minimum: 1  -> the =1 arm goes red ("the render SUCCEEDED and was supposed
#                  to fail"), and the =0 arm goes red on the WORDING, because
#                  the diagnostic an operator reads changes with the bound.
#   minimum: 3  -> the =2 arm goes red. The floor cannot drift upward either.
# Both runs stayed at 278/278, so neither is a harness error in disguise.
#
# Noted because it nearly cost the measurement: the first attempt at these two
# mutations had a stale text anchor, the patch step raised inside a subshell
# with no `|| exit`, and the loop printed "278/278, 0 failures" for a chart it
# had never modified. Read quickly that says "the arms do not catch it". A
# mutation harness needs the same rule as the suite it attacks - a setup that
# did not run is not a result.

# --------------------------------------------------------------------------
step "config.extra deep merge"
# --------------------------------------------------------------------------
block="$(settings_block "$WORK/settings.yaml")"
if grep -qxF '  log_level: debug' <<<"$block"; then
  pass "config.extra adds a new key under an existing section"
else
  fail "config.extra did not merge server.log_level"
fi
if grep -qxF '    interval_seconds: 30' <<<"$block"; then
  pass "config.extra adds a new nested section"
else
  fail "config.extra did not merge server.scheduler"
fi
# A deep merge and not a replace: the sibling keys of what it merged into must
# survive. Without this, a shallow merge would pass every check above.
if grep -qxF '    hub_id: ci-settings' <<<"$block" && grep -qxF '    driver: postgres' <<<"$block"; then
  pass "config.extra merged without replacing the sections it merged into"
else
  fail "config.extra replaced rather than merged - siblings under server: were lost"
fi

# --------------------------------------------------------------------------
step "config.existingSecret"
# --------------------------------------------------------------------------
if grep -qF 'kind: Secret' "$WORK/existing-secret.yaml"; then
  fail "config.existingSecret did not suppress the chart's own Secret"
else
  pass "config.existingSecret suppresses the chart's Secret"
fi
if grep -qF 'secretName: my-own-hub-settings' "$WORK/existing-secret.yaml"; then
  pass "the pod mounts the operator's Secret"
else
  fail "the pod does not mount the Secret named in config.existingSecret"
fi
if grep -qF 'checksum/settings:' "$WORK/existing-secret.yaml"; then
  fail "checksum/settings is rendered under config.existingSecret, where it is a constant"
else
  pass "checksum/settings is omitted under config.existingSecret"
fi
# The positive twin for both: the chart does render a Secret, and does annotate
# the pod with its checksum, when it owns the file.
if grep -qF 'kind: Secret' "$WORK/settings.yaml" && grep -qF 'checksum/settings:' "$WORK/settings.yaml"; then
  pass "the chart renders its own Secret and a checksum when it owns the file"
else
  fail "the chart did not render a Secret or its checksum - the two checks above are vacuous"
fi

# --------------------------------------------------------------------------
step "every value that only reaches settings.yaml is refused or documented"
# --------------------------------------------------------------------------
# The check on the check for config.existingSecret.
#
# The guard above refuses three named values. A named list answers "are these
# three still refused" and cannot answer the question that matters, which is
# "are these still the only ones". Nothing about a fourth settings value being
# added is visible: it renders, it installs, and the operator's setting is
# discarded with no error - the same silent no-op the guard exists to prevent,
# arriving through the guard's blind spot rather than past it.
#
# So this derives the question from the chart instead of asking it. Every leaf
# of values.yaml is enumerated, mutated one at a time, and rendered twice; a
# value whose mutation moves the settings document and nothing else is a value
# that goes silent under config.existingSecret, and must be either refused by
# the guard or named in the transfer list with the settings key it moved. A
# value in neither place fails here. Nobody has to remember to update a list.
#
# THE ENUMERATION IS THE PART THAT MUST NOT BE HAND-WRITTEN, so it is taken from
# values.yaml at run time by a throwaway chart written into $WORK - Helm walking
# its own values with the same recursion the chart's own helpers use. Two things
# that cost a round to learn and are easy to reintroduce:
#
#   - the walk must be given the same --set values the render gets. Required
#     values with no default (image.repository, hub.hubId, hub.baseUrl) are
#     commented out in values.yaml, so a walk over the file alone does not see
#     them and silently drops three leaves - including the hub ID.
#   - a mutation has to actually mutate. updateStrategy.type reads Recreate at
#     replicaCount 1, so "set it to Recreate" changes nothing and reports the
#     value inert. That class of mistake is caught below by counting the leaves
#     that moved nothing at all rather than by inspection.
#
# Renders here pass --skip-schema-validation. The mutations are deliberately
# odd values and several would be rejected by the schema before a template ever
# saw them; the schema's own rejections are asserted in their own step. What is
# under test here is which document a value reaches, which is a template fact.
probe_dir="$WORK/valueprobe"
mkdir -p "$probe_dir/templates"
cp "$CHART_DIR/values.yaml" "$probe_dir/values.yaml"
cat >"$probe_dir/Chart.yaml" <<'PROBE_CHART'
apiVersion: v2
name: valueprobe
version: 0.0.0
PROBE_CHART
cat >"$probe_dir/templates/paths.yaml" <<'PROBE_TPL'
{{- define "probe.walk" -}}
{{- $prefix := .prefix -}}
{{- range $k, $v := .obj }}
{{- $p := ternary $k (printf "%s.%s" $prefix $k) (eq $prefix "") }}
{{- if and (kindIs "map" $v) (gt (len $v) 0) }}
{{- include "probe.walk" (dict "obj" $v "prefix" $p) }}
{{- else }}
{{ printf "%s|%s|%v" $p (kindOf $v) $v }}
{{- end }}
{{- end }}
{{- end }}
apiVersion: v1
kind: ConfigMap
metadata:
  name: probe
data:
  paths: |
{{ include "probe.walk" (dict "obj" .Values "prefix" "") | indent 4 }}
PROBE_TPL

# The settings document flattened to one "key.path=value" line per leaf, so a
# diff names the settings key that moved rather than a line number. The document
# is machine-generated - two-space indent, no anchors, no flow mappings, no
# multi-line scalars - and anything outside that shape exits non-zero rather
# than being skipped, because a parser that silently sees fewer keys turns this
# whole step green.
cat >"$WORK/settings-leaves.py" <<'PROBE_PY'
import sys
stack, out = [], []
for raw in sys.stdin:
    line = raw.rstrip('\n')
    if not line.strip() or line.lstrip().startswith('#'):
        continue
    indent = len(line) - len(line.lstrip(' '))
    if indent % 2:
        sys.stderr.write('odd indent: %r\n' % line)
        sys.exit(2)
    depth, body = indent // 2, line.strip()
    if body.startswith('- '):
        out.append('.'.join(stack[:depth]) + '[]=' + body[2:])
        continue
    if ':' not in body:
        sys.stderr.write('unparsed line: %r\n' % line)
        sys.exit(2)
    key, _, val = body.partition(':')
    stack = stack[:depth] + [key]
    if val.strip():
        out.append('.'.join(stack) + '=' + val.strip())
print('\n'.join(out))
PROBE_PY

# Mutations that cannot be derived from the value's type: enum members, patterned
# strings, and values that need a companion set before they reach anything. A
# missing entry here does not produce a wrong answer - it produces a render
# failure or a leaf that moves nothing, both of which are counted and reported
# below.
declare -A PROBE_MUTATION=(
  [auth.requireStableSigningKey]='--set|auth.requireStableSigningKey=false'
  [auth.sessionSecret]='--set-string|auth.sessionSecret=probe-other-session-secret'
  # auth.existingSecret and auth.existingSecretKey switch the session secret
  # delivery from envFrom (the chart's own Secret) to secretKeyRef (an operator-
  # managed Secret). Both need auth.sessionSecret cleared to avoid the guard
  # that rejects two sources, and BASE carries sessionSecret since phase 3.
  [auth.existingSecret]='--set|auth.sessionSecret=|--set-string|auth.existingSecret=probe-session-secret|--set-string|auth.existingSecretKey=MY_KEY'
  [auth.existingSecretKey]='--set|auth.sessionSecret=|--set-string|auth.existingSecret=probe-session-secret|--set-string|auth.existingSecretKey=OTHER_KEY'
  # NO COMPANION, and that is the point of PROBE_CREDS below. oauth mode will not
  # render without a complete web client credential, so this needed one - and a
  # companion here is attributed to auth.mode, which produced the false transfer
  # "auth.mode -> server.oauth.web.google.client_id". The credential lives in the
  # probe's baseline instead, where it cancels. It was acknowledgeOAuthUnlanded
  # until the credentials had a channel to arrive on.
  [auth.mode]='--set-string|auth.mode=oauth'
  [auth.oauth.web.google.clientId]='--set-string|auth.oauth.web.google.clientId=probe-oauth-mutated-id'
  [auth.oauth.web.google.clientSecret]='--set-string|auth.oauth.web.google.clientSecret=probe-oauth-mutated-secret'
  [auth.oauth.web.github.clientId]='--set-string|auth.oauth.web.github.clientId=probe-gh-id|--set-string|auth.oauth.web.github.clientSecret=probe-gh-secret'
  [auth.oauth.web.github.clientSecret]='--set-string|auth.oauth.web.github.clientId=probe-gh-id|--set-string|auth.oauth.web.github.clientSecret=probe-gh-secret2'
  [database.connMaxIdleTime]='--set-string|database.connMaxIdleTime=9m'
  [database.connMaxLifetime]='--set-string|database.connMaxLifetime=9m'
  # THE CLOUD SQL LEAVES ALL CARRY THE SAME PREAMBLE, and it is not boilerplate:
  # every one of them is inert unless the driver is postgres AND the proxy is on,
  # so a mutation without it moves nothing and the leaf is reported as a value
  # that does nothing. That report would be true of the mutation and false of the
  # chart. Each entry below therefore turns the feature ON and then changes the
  # one leaf it is named for.
  [database.driver]='--set-string|database.driver=postgres|--set-string|database.auth=iam|--set-string|database.name=probe-db|--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true'
  [database.name]='--set-string|database.driver=postgres|--set-string|database.auth=iam|--set-string|database.name=probe-db|--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true|--set-string|database.name=probe-other-db'
  [database.user]='--set-string|database.driver=postgres|--set-string|database.auth=iam|--set-string|database.name=probe-db|--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true|--set-string|database.user=probe-user'
  # The one leaf that cannot use the iam preamble: under iam the schema and the
  # template both refuse a password, and the DSN has nowhere to put one.
  [database.password]='--set-string|database.driver=postgres|--set-string|database.auth=password|--set-string|database.name=probe-db|--set-string|database.user=probe-user|--set-string|database.password=probe-pw|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true|--set-string|database.password=probe-pw2'
  [cloudsql.instanceConnectionName]='--set-string|database.driver=postgres|--set-string|database.auth=iam|--set-string|database.name=probe-db|--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true|--set-string|cloudsql.instanceConnectionName=other-project:us-west1:db-2'
  [cloudsql.nativeSidecar]='--set-string|database.driver=postgres|--set-string|database.auth=iam|--set-string|database.name=probe-db|--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true|--set|cloudsql.nativeSidecar=false'
  [cloudsql.privateIp]='--set-string|database.driver=postgres|--set-string|database.auth=iam|--set-string|database.name=probe-db|--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true|--set|cloudsql.privateIp=true'
  [cloudsql.port]='--set-string|database.driver=postgres|--set-string|database.auth=iam|--set-string|database.name=probe-db|--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true|--set|cloudsql.port=5433'
  [cloudsql.healthCheckPort]='--set-string|database.driver=postgres|--set-string|database.auth=iam|--set-string|database.name=probe-db|--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true|--set|cloudsql.healthCheckPort=9802'
  [cloudsql.image.repository]='--set-string|database.driver=postgres|--set-string|database.auth=iam|--set-string|database.name=probe-db|--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true|--set-string|cloudsql.image.repository=other.test/probe-proxy'
  [cloudsql.image.digest]='--set-string|database.driver=postgres|--set-string|database.auth=iam|--set-string|database.name=probe-db|--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true|--set-string|cloudsql.image.digest=sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd'
  [cloudsql.image.pullPolicy]='--set-string|database.driver=postgres|--set-string|database.auth=iam|--set-string|database.name=probe-db|--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true|--set-string|cloudsql.image.pullPolicy=Never'
  [cloudsql.resources]='--set-string|database.driver=postgres|--set-string|database.auth=iam|--set-string|database.name=probe-db|--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com|--set|cloudsql.enabled=true|--set-string|cloudsql.instanceConnectionName=my-project:us-central1:db-1|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true|--set|cloudsql.resources.limits.memory=64Mi'
  [hub.args]='--set-string|hub.args[0]=--probe-flag'
  [hub.baseUrl]='--set-string|hub.baseUrl=https://other.example.com'
  [hub.extraEnv]='--set-string|hub.extraEnv[0].name=PROBE_ONE|--set-string|hub.extraEnv[0].value=x'
  [hub.home]='--set-string|hub.home=/probe/home'
  [hub.hubId]='--set-string|hub.hubId=probe-two'
  [hub.tolerations]='--set-string|hub.tolerations[0].key=probe|--set-string|hub.tolerations[0].operator=Exists'
  [image.digest]='--set-string|image.digest=sha256:abababababababababababababababababababababababababababababababab'
  [image.pullPolicy]='--set-string|image.pullPolicy=Never'
  [image.pullSecrets]='--set-string|image.pullSecrets[0].name=probe-secret'
  [image.repository]='--set-string|image.repository=other.test/probe-img'
  [probes.liveness.failureThreshold]='--set|probes.liveness.enabled=true|--set|probes.liveness.failureThreshold=37'
  [probes.liveness.periodSeconds]='--set|probes.liveness.enabled=true|--set|probes.liveness.periodSeconds=37'
  [probes.liveness.timeoutSeconds]='--set|probes.liveness.enabled=true|--set|probes.liveness.timeoutSeconds=37'
  [serviceAccount.create]='--set|serviceAccount.create=false|--set-string|serviceAccount.name=preexisting'
  [serviceAccount.gcpServiceAccount]='--set-string|serviceAccount.gcpServiceAccount=probe@proj.iam.gserviceaccount.com'
  [storage.bucket]='--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true'
  [storage.provider]='--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true'
  [updateStrategy.type]='--set-string|updateStrategy.type=RollingUpdate'
)

# A COMPLETE OAUTH WEB CREDENTIAL, IN THE PROBE'S BASELINE AND NOT IN BASE.
# Not in BASE, because BASE is what the oauth-refusal checks further down use in
# order to BE refused; putting it there would turn two of them green for the
# wrong reason.
#
# WHY THE BASELINE AND NOT auth.mode's MUTATION. The probe attributes every
# settings key that moved to the leaf it mutated. auth.mode=oauth cannot render
# without a credential, so a mutation that supplies one is indistinguishable, to
# the probe, from auth.mode moving the credential itself - and it duly observed
# "auth.mode -> server.oauth.web.google.client_id" and demanded that be declared
# as a transfer. It is not one. In the baseline the credential is present on both
# sides of every comparison and cancels out of all of them.
#
# DELIBERATELY ABSENT FROM THE config.existingSecret REFUSAL RENDER BELOW. That
# render asks whether an operator setting THIS leaf alongside an external Secret
# is refused, so it must carry the mutation and nothing else. Add PROBE_CREDS
# there and every leaf is refused - for the credential, never for itself - and
# the transfer list empties without a single check going red.
PROBE_CREDS=(
  --set-string auth.oauth.web.google.clientId=probe-oauth-base-id
  --set-string auth.oauth.web.google.clientSecret=probe-oauth-base-secret
)
probe_render() {
  "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    --skip-schema-validation "${BASE[@]}" "${PROBE_CREDS[@]}" "$@" 2>&1
}
# IS THIS LEAF STILL LIVE WHEN config.existingSecret IS SET?
#
# The question the refusal check actually cares about. A value that goes inert
# under existingSecret must be refused or documented; a value that still does
# something needs neither, and cloudsql.* is the whole family in the second
# group - the chart renders the proxy sidecar there whatever the settings
# document says, so cloudsql.port keeps changing the manifest.
#
# ASKED WITHOUT A BASELINE, deliberately. probe-base.yaml is rendered from BASE
# alone, so for any leaf whose spec carries a preamble the observed movement is
# the PREAMBLE's, not the leaf's - that contamination is why a refusal was the
# only thing this loop could look at. Perturbing one leaf across two renders
# that are otherwise identical asks about the leaf and nothing else. It is the
# same two-values-one-diff method gd-p2-rev used to measure R3 in the first
# place.
#
# The second value is the spec's own trailing override REMOVED rather than a
# value invented here: an invented one has to satisfy whatever shape the leaf
# requires (cloudsql.image.digest is not going to accept "zzprobe"), and a
# render refused for a malformed probe value is indistinguishable from a leaf
# that did nothing. The spec's default-vs-override pair is two values the chart
# already accepts.
#
# Exit: 0 live, 1 inert, 2 could not be asked.
probe_leaf_live_under_es() { # <leaf> <comma-list of leaves to drop> <spec args...>
  # The guard prints its list as "a, b, c"; the spaces come out here rather than
  # at every call site, because a call site that forgets to strip them silently
  # drops nothing and the whole question comes back unanswerable.
  local leaf="$1" drop=",${2// /},"; shift 2
  local -a full=()
  # DROP THE REFUSABLE COMPANIONS FIRST. The question cannot be asked inside a
  # render the chart refuses: both sides come back as the same refusal message
  # and the leaf never gets to show what it does. The values to drop are the
  # ones the guard just named, read out of its own message rather than listed
  # here, so this tracks the guard instead of a copy of it.
  local i=0 _f _a
  while (( i < $# )); do
    _f="${*:i+1:1}"; _a="${*:i+2:1}"
    if [[ "$_f" == --set* && "$drop" == *",${_a%%=*},"* ]]; then
      i=$((i + 2)); continue
    fi
    full+=("$_f" "$_a"); i=$((i + 2))
  done
  local n=${#full[@]}
  (( n >= 2 )) || return 2
  # The spec must END with an override of the leaf under probe. This is an
  # assumption about how PROBE_MUTATION is written, so it is tested rather than
  # trusted: a spec that sets the leaf in the middle and something else last
  # would have the wrong element trimmed and the answer would be about that
  # something else.
  # "$leaf." as well as "$leaf=", because a map leaf is overridden through one of
  # its sub-keys - cloudsql.resources is set as cloudsql.resources.limits.memory.
  [[ "${full[$((n-2))]}" == --set* ]] \
    && [[ "${full[$((n-1))]}" == "$leaf="* || "${full[$((n-1))]}" == "$leaf."* ]] \
    || return 2
  local -a trimmed=("${full[@]:0:$((n-2))}")
  local ra=0 rb=0
  "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    --skip-schema-validation "${BASE[@]}" --set config.existingSecret=mine \
    "${full[@]}" >"$WORK/probe-live-a.yaml" 2>&1 || ra=$?
  "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    --skip-schema-validation "${BASE[@]}" --set config.existingSecret=mine \
    "${trimmed[@]}" >"$WORK/probe-live-b.yaml" 2>&1 || rb=$?
  if [[ $ra -ne 0 || $rb -ne 0 ]]; then
    # Both refused identically is not an answer about the leaf; it is an answer
    # about the preamble. Anything else - one refused, or two different
    # refusals - is the leaf changing the outcome, which is liveness.
    cmp -s "$WORK/probe-live-a.yaml" "$WORK/probe-live-b.yaml" && return 2 || return 0
  fi
  cmp -s "$WORK/probe-live-a.yaml" "$WORK/probe-live-b.yaml" && return 1 || return 0
}
# The settings document only. Everything else in the stream, with the settings
# checksum removed: that annotation is a hash OF the settings document, so
# leaving it in would report every settings-only value as touching the
# Deployment too, and no value would ever be classified settings-only.
probe_settings() {
  sed -n '/^  settings\.yaml: |/,/^[^ ]/p' "$1" \
    | sed -e '1d' -e '/^$/d' | grep -E '^    ' \
    | sed -e 's/^    //' -e '/^[[:space:]]*#/d' \
    | python3 "$WORK/settings-leaves.py"
}
probe_other() {
  awk '/^# Source: /{keep = ($0 !~ /secret-settings\.yaml/)} keep' "$1" \
    | grep -vF 'checksum/settings:'
}

if ! probe_render >"$WORK/probe-base.yaml"; then
  fail "the mutation probe's baseline render failed; every classification below is void"
else
  probe_settings "$WORK/probe-base.yaml" >"$WORK/probe-base.settings" || true
  probe_other "$WORK/probe-base.yaml" >"$WORK/probe-base.other"

  if ! "$HELM" template probe "$probe_dir" "${BASE[@]}" >"$WORK/probe-paths.yaml" 2>&1; then
    fail "the values walk did not render - the leaf enumeration below is empty"
  fi
  # THE LEAF FILTER, AND ITS ANTI-JOIN.
  #
  # This filter is the only thing between the values walk and the mutation loop,
  # and until now its extent was asserted with a FLOOR - kept leaves `-ge 50`.
  # A floor cannot see a fail-open. Measured, not reasoned: replacing the -F
  # below with -E makes '|' an empty alternation that matches every line, the
  # kept set grows, the floor is still satisfied, and the suite still printed
  # `assertions: 259/259 failures: 0`. The headline was insensitive to a
  # deliberate defect in the machinery that produces it.
  #
  # So the size is asserted three ways and NONE of them uses the '|' predicate
  # on the right-hand side (rule 61: a fixture derived from the predicate can
  # only confirm the predicate):
  #
  #   kept + dropped == walked      the join closes; no line is unaccounted for
  #   dropped        == 1           the dark rows, published rather than implied
  #   kept           == walked - 1  extent, derived from templates/paths.yaml's
  #                                 shape - it emits exactly one blank line
  #                                 before the first leaf and nothing else
  #                                 without a '|'
  #
  # and separately, below, the KEY SET is compared against an enumeration of
  # values.yaml built by awk, which shares no code and no engine with the helm
  # walk. That is the independently-derived expectation the extent amendment
  # asks for; a count agreeing with a count would not be.
  sed -n '/paths: |/,$p' "$WORK/probe-paths.yaml" \
    | sed -e '1d' -e 's/^    //' >"$WORK/probe-walk.txt"
  grep -F  '|' "$WORK/probe-walk.txt" >"$WORK/probe-leaves.txt"    || true
  grep -vF '|' "$WORK/probe-walk.txt" >"$WORK/probe-nonleaves.txt" || true
  probe_walked=$(wc -l <"$WORK/probe-walk.txt")
  probe_kept=$(wc -l <"$WORK/probe-leaves.txt")
  probe_dropped=$(wc -l <"$WORK/probe-nonleaves.txt")

  # The independent enumeration. Indentation-based, two-space, list items
  # counted as leaves of their parent. RULE 130 (ag-dev): a parser that cannot
  # parse takes the STRICT branch - a line it cannot decompose exits 2 and
  # becomes a meta-failure, never a silently smaller key set, because a smaller
  # key set on this side turns the comparison below green for the wrong reason.
  if ! awk '
      /^[[:space:]]*#/ {next} /^[[:space:]]*$/ {next}
      { line=$0; sub(/[[:space:]]+$/,"",line); ind=match(line,/[^ ]/)-1
        body=substr(line,ind+1); d=int(ind/2)
        s=""; for(i=0;i<d;i++) s=(s=="" ? st[i] : s "." st[i])
        if (body ~ /^- /) { print s "[]"; next }
        p=index(body,":")
        if (p==0) { printf "unparsed values.yaml line: %s\n", line > "/dev/stderr"; exit 2 }
        k=substr(body,1,p-1); val=substr(body,p+1); gsub(/^[ \t]+|[ \t]+$/,"",val)
        st[d]=k; for(i=d+1;i<20;i++) st[i]=""
        if (val != "") print (s=="" ? k : s "." k)
      }' "$CHART_DIR/values.yaml" | sort -u >"$WORK/probe-values-keys.txt"; then
    meta_failure "the awk enumeration of values.yaml could not parse a line, so the values walk has nothing independent to be compared against"
  fi
  # BASE may --set a key that values.yaml does not carry; the walk sees it, awk
  # cannot. Parsed out of the BASE array itself rather than listed by hand, so
  # a phase adding a --set does not have to remember this line.
  printf '%s\n' "${BASE[@]}" \
    | sed -n 's/^\([A-Za-z_][A-Za-z0-9_.]*\)=.*/\1/p' | sort -u >"$WORK/probe-base-keys.txt"
  sort -u "$WORK/probe-values-keys.txt" "$WORK/probe-base-keys.txt" >"$WORK/probe-expected-keys.txt"
  cut -d'|' -f1 "$WORK/probe-leaves.txt" | sort -u >"$WORK/probe-walk-keys.txt"

  # LEAVES WITH NO LEGAL MUTATION. Not "leaves that are awkward" - leaves where
  # every value other than the default is REFUSED BY A RENDER GUARD, so there is
  # nothing the probe can set them to that produces a manifest to compare. An
  # exclusion is lost coverage, so each one names the guard that makes it
  # unreachable and the assertion that covers it instead, and the list's LENGTH
  # is asserted below: a fifth entry appearing without a reader noticing is the
  # failure mode of every skip list, and the count is what stops it.
  #
  #   config.existingSecret          - not a refusal: it removes the settings
  #                                    document entirely, so the baseline it
  #                                    would be compared against does not exist.
  #                                    Covered by the transfer-list diff below,
  #                                    which is about nothing else.
  #
  # auth.requireStableSigningKey WAS here. The guard in configmap-env.yaml that
  # refused true without config.existingSecret was removed in phase 3: the
  # session secret is now unconditional, so the flag is mutable. Its coverage
  # is in tests/chart-integrity.sh section E, both directions, and it has a
  # PROBE_MUTATION entry that flips it to false (its non-default arm).
  PROBE_UNMUTABLE=(config.existingSecret)
  if [[ ${#PROBE_UNMUTABLE[@]} -ne 1 ]]; then
    echo "HARNESS ERROR: PROBE_UNMUTABLE holds ${#PROBE_UNMUTABLE[@]} entries, not 1. Every entry is coverage this probe is not providing; read the reasons above before changing the number." >&2
    exit 2
  fi

  # EVERY SPEC ENDS IN AN OVERRIDE OF ITS OWN LEAF, INCLUDING THE ONES THAT DID
  # NOT NEED TO. database.password's preamble already set the password, so the
  # spec had no trailing override and the liveness question below came back
  # unaskable - a loud halt, but a halt where a clean red was available. A
  # second value costs one --set and turns "cannot measure" into "measured, and
  # inert". Keep the shape: the trailing element of a spec is the leaf's own
  # override, and the liveness probe asserts that rather than assuming it.
  #
  # LEAVES THAT ARE REFUSED, BUT UNDER A COMPANION'S NAME. gd-p2-rev's R4: this
  # loop used to book any refusal as proof the leaf was guarded, and every
  # postgres preamble below carries storage.bucket, so the refusal fired for the
  # BUCKET and the leaf under probe was never named. The refusal check now
  # requires the leaf itself to appear in the guard's parenthesised list. Two
  # leaves genuinely cannot appear there and are exempted BY NAME:
  #
  #   storage.provider  - default gcs is non-empty, so it is not refusable on
  #   database.driver     truthiness; adding either to the guard would refuse
  #                       every config.existingSecret install rather than the
  #                       ones that set something inert. The reasoning is in
  #                       _helpers.tpl above scion-hub.assertConfigSource.
  #
  # The exemption is not a free pass: the render must still be REFUSED, and the
  # refusal must name storage.bucket, which is the companion the chart's comment
  # claims covers them. If a later phase makes either reachable without a
  # bucket, the exemption stops matching and this goes red - which is exactly
  # what that comment promises hack/verify.sh does.
  #
  # WHY THE OTHER HALF OF R4'S FIX IS NOT HERE. gd-p2-rev also asked that the
  # probe preamble stop carrying a second refusable value. It cannot: postgres
  # forces server.storage.provider=gcs (secret-settings.yaml refuses "local"
  # under Postgres) and gcs forces a bucket, so every database.* preamble
  # carries one by construction. Measured - dropping the storage pair from the
  # database.password spec fails the render with "rendered settings.yaml must
  # set server.storage.provider: gcs under Postgres". Naming the leaf is the
  # remedy that survives that, and it is the stronger half anyway: it checks the
  # operator is told about THEIR value, not merely that something was refused.
  PROBE_REFUSED_BY_COMPANION=(storage.provider database.driver)
  if [[ ${#PROBE_REFUSED_BY_COMPANION[@]} -ne 2 ]]; then
    echo "HARNESS ERROR: PROBE_REFUSED_BY_COMPANION holds ${#PROBE_REFUSED_BY_COMPANION[@]} entries, not 2. Each entry is a leaf allowed to be booked as guarded on a refusal that never names it; read the reasons above before changing the number." >&2
    exit 2
  fi

  # probe_total is deliberately absent (gd-p1-rev, N-1, round 4). It counted the
  # kept leaves for the `-ge 50` floor, the floor is gone, and its four
  # surviving siblings are all read below. A counter nobody reads is a reader's
  # invitation to assume something is checked.
  probe_settings_only=0 probe_half=0 probe_quiet=0 probe_err=0
  probe_misrefused="" probe_companion_used="" probe_live_named=""
  : >"$WORK/probe-refused-named.txt"
  : >"$WORK/probe-observed.txt"
  probe_quiet_names="" probe_err_names=""
  probe_skipped=0

  while IFS='|' read -r leaf kind value; do
    [[ -z "$leaf" ]] && continue
    if [[ " ${PROBE_UNMUTABLE[*]} " == *" $leaf "* ]]; then
      probe_skipped=$((probe_skipped + 1)); continue
    fi
    spec="${PROBE_MUTATION[$leaf]:-}"
    if [[ -z "$spec" ]]; then
      case "$kind" in
        bool)    [[ "$value" == true ]] && spec="--set|$leaf=false" || spec="--set|$leaf=true" ;;
        float64) spec="--set|$leaf=$(( ${value%.*} + 7 ))" ;;
        string)  spec="--set-string|$leaf=zzprobe" ;;
        map)     spec="--set-string|$leaf.probeKey=probeVal" ;;
        *)       probe_err=$((probe_err + 1)); probe_err_names+=" $leaf(no mutation for $kind)"; continue ;;
      esac
    fi
    IFS='|' read -r -a mutation <<<"$spec"

    if ! probe_render "${mutation[@]}" >"$WORK/probe-mut.yaml"; then
      probe_err=$((probe_err + 1)); probe_err_names+=" $leaf"
      continue
    fi
    probe_settings "$WORK/probe-mut.yaml" >"$WORK/probe-mut.settings" || true
    probe_other "$WORK/probe-mut.yaml" >"$WORK/probe-mut.other"

    moved_settings=0 moved_other=0
    cmp -s "$WORK/probe-base.settings" "$WORK/probe-mut.settings" || moved_settings=1
    cmp -s "$WORK/probe-base.other" "$WORK/probe-mut.other" || moved_other=1

    if [[ $moved_settings -eq 0 && $moved_other -eq 0 ]]; then
      probe_quiet=$((probe_quiet + 1)); probe_quiet_names+=" $leaf"
      continue
    fi
    [[ $moved_settings -eq 0 ]] && continue

    [[ $moved_other -eq 1 ]] && probe_half=$((probe_half + 1)) || probe_settings_only=$((probe_settings_only + 1))

    # Refused is a complete answer ONLY IF THE REFUSAL NAMES THIS LEAF. See
    # PROBE_REFUSED_BY_COMPANION above for why, and for the two exemptions.
    _ex_rc=0
    "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
      --skip-schema-validation "${BASE[@]}" --set config.existingSecret=mine \
      "${mutation[@]}" >"$WORK/probe-ex.yaml" 2>&1 || _ex_rc=$?
    if [[ $_ex_rc -ne 0 ]] \
      && grep -qF 'config.existingSecret is set together with inline settings values' "$WORK/probe-ex.yaml"; then
      # The guard prints the leaves it objected to in parentheses. Reading them
      # back is the whole point: "refused" and "refused about you" are different
      # facts and this loop used to conflate them.
      # `|| true` because this assignment is a pipeline and an unguarded one
      # aborts the script under `set -e` with no summary line - see the
      # pipeline-assignment sweep near the end of this file. An empty result is
      # not swallowed: it is the meta-failure immediately below.
      _ex_named="$(sed -n 's/.*inline settings values (\([^)]*\)).*/\1/p' "$WORK/probe-ex.yaml" | head -1 || true)"
      if [[ -z "$_ex_named" ]]; then
        meta_failure "the config.existingSecret refusal for [$leaf] printed no parenthesised list of inline values, so there is nothing to compare the leaf against and every leaf below would be booked on an unreadable refusal. The guard's message shape changed; this reader has to change with it. Got: $(grep -m1 'inline settings values' "$WORK/probe-ex.yaml")"
      fi
      _ex_list=",${_ex_named//, /,},"
      if [[ "$_ex_list" == *",$leaf,"* ]]; then
        printf '%s\n' "$leaf" >>"$WORK/probe-refused-named.txt"
        continue
      fi
      if [[ " ${PROBE_REFUSED_BY_COMPANION[*]} " == *" $leaf "* ]] \
        && [[ "$_ex_list" == *",storage.bucket,"* ]]; then
        probe_companion_used+=" $leaf"
        continue
      fi
      # Not named in the refusal - so ask the question the refusal was standing
      # in for. A leaf that still moves the manifest under config.existingSecret
      # is not silently discarded and needs no refusal to protect it.
      _live_rc=0
      probe_leaf_live_under_es "$leaf" "$_ex_named" "${mutation[@]}" || _live_rc=$?
      case "$_live_rc" in
        0) probe_live_named+=" $leaf"; continue ;;
        1) probe_misrefused+=" $leaf(refused for: $_ex_named; INERT)"; continue ;;
        *) meta_failure "could not determine whether [$leaf] is still live under config.existingSecret: its PROBE_MUTATION spec does not end in a --set of that leaf, or both renders were refused identically. The leaf is neither named in the refusal it was booked on nor measurable, so it would be booked as covered by a check that did not run. Spec: ${mutation[*]}" ;;
      esac
    fi

    # Not refused, so it must be documented - with the key it actually moved.
    for key in $(diff "$WORK/probe-base.settings" "$WORK/probe-mut.settings" \
                   | sed -n 's/^[<>] \([^=]*\)=.*/\1/p' | sort -u); do
      printf '%s %s\n' "$leaf" "$key" >>"$WORK/probe-observed.txt"
    done
  done <"$WORK/probe-leaves.txt"

  # The declared list, read from the one definition NOTES.txt renders.
  sed -n '/define "scion-hub.existingSecretTransfers"/,/^{{- end }}/p' \
    "$CHART_DIR/templates/_helpers.tpl" \
    | sed -e '1d' -e '$d' -e 's/[[:space:]]*->[[:space:]]*/ /' -e 's/^[[:space:]]*//' \
    | sort -u >"$WORK/probe-declared.txt"
  sort -u "$WORK/probe-observed.txt" >"$WORK/probe-observed-sorted.txt"

  # ANTI-JOIN, PART ONE: the parts sum to the denominator. A filter that drops a
  # line on the floor is invisible to any assertion that only looks at what the
  # filter kept.
  if [[ $((probe_kept + probe_dropped)) -eq $probe_walked ]]; then
    pass "the leaf filter accounts for every line the walk emitted: $probe_kept kept + $probe_dropped dropped == $probe_walked walked"
  else
    fail "the leaf filter lost lines: $probe_kept kept + $probe_dropped dropped != $probe_walked walked"
  fi
  # ANTI-JOIN, PART TWO: the dark rows are asserted, not merely reported. This is
  # the assertion that goes red on a fail-open in the filter, because its
  # right-hand side is derived from templates/paths.yaml's shape and never from
  # the filter's own predicate.
  if [[ $probe_dropped -eq 1 && $probe_kept -eq $((probe_walked - 1)) ]]; then
    pass "the leaf filter dropped exactly the one non-leaf line the walk emits, keeping $probe_kept of $probe_walked"
  else
    fail "the leaf filter dropped $probe_dropped lines and kept $probe_kept of $probe_walked - templates/paths.yaml emits one blank line and one '|' line per leaf, so exactly one line should be dropped. Dropped rows: $(tr '\n' ' ' <"$WORK/probe-nonleaves.txt")"
  fi
  # EXTENT, AGAINST AN INDEPENDENTLY-DERIVED EXPECTATION. Set equality in both
  # directions with the symmetric difference printed, not a count against a
  # count and not a floor: the walk is helm's recursive template, the
  # expectation is awk over values.yaml plus the BASE overrides, and the two
  # share nothing but the file they read.
  probe_keydiff="$(comm -3 "$WORK/probe-expected-keys.txt" "$WORK/probe-walk-keys.txt")"
  if [[ -z "$probe_keydiff" ]]; then
    pass "the values walk enumerated exactly the $(wc -l <"$WORK/probe-walk-keys.txt" | tr -d ' ') leaves awk finds in values.yaml plus BASE, same set both directions"
  else
    fail "the values walk and the awk enumeration of values.yaml disagree. Left column = expected but not walked, right column = walked but not expected:"$'\n'"$probe_keydiff"
  fi
  if [[ $probe_err -eq 0 ]]; then
    pass "every leaf could be mutated and rendered"
  else
    fail "$probe_err leaves could not be rendered with their mutation and were classified not at all:$probe_err_names - add an entry to PROBE_MUTATION"
  fi
  # THE SKIP LIST'S POSITIVE TWIN. Asserting the list has two entries says
  # nothing about whether those two entries still name leaves that exist. Rename
  # auth.requireStableSigningKey and the skip stops matching, the leaf silently
  # rejoins the walk, and - because its mutation is refused - the run goes red
  # somewhere else entirely. Worse in the other direction: delete the leaf and
  # the exclusion sits there forever excusing coverage nobody is missing.
  if [[ $probe_skipped -eq ${#PROBE_UNMUTABLE[@]} ]]; then
    pass "both unmutable leaves were present in the walk and skipped deliberately"
  else
    fail "the walk skipped $probe_skipped leaves but PROBE_UNMUTABLE names ${#PROBE_UNMUTABLE[@]} (${PROBE_UNMUTABLE[*]}) - an entry no longer matches a leaf in values.yaml, so it is excusing nothing"
  fi
  # gd-p2-rev's R4, the arm that did not exist. A leaf booked as guarded on a
  # refusal that never mentions it is a leaf nobody is guarding: the operator
  # sets it, the chart refuses for something ELSE, they fix that something else,
  # and now the value is silently inert with no refusal left to catch it.
  # THE LIVENESS INSTRUMENT, PROVED IN BOTH DIRECTIONS BEFORE ITS ANSWERS ARE
  # USED. The arm below excuses a leaf on "still live under existingSecret", so
  # an instrument stuck on LIVE would excuse everything and an instrument stuck
  # on INERT would condemn everything. Neither shows up in the arm's own result.
  # Two leaves with known and opposite answers, through the same function:
  # replicaCount reaches the Deployment, which is rendered either way;
  # database.maxOpenConns reaches nothing but the settings document, which is
  # not.
  _lv_pos=0; probe_leaf_live_under_es replicaCount "" --set replicaCount=3 || _lv_pos=$?
  _lv_neg=0; probe_leaf_live_under_es database.maxOpenConns "" --set database.maxOpenConns=37 || _lv_neg=$?
  if [[ "$_lv_pos" == 0 ]]; then
    pass "the liveness probe says replicaCount is LIVE under config.existingSecret, so it can still see a leaf that moves the manifest"
  else
    fail "the liveness probe returned $_lv_pos for replicaCount, which reaches the Deployment and is rendered under config.existingSecret. It cannot see liveness, so every leaf it excuses below is excused by an instrument that has stopped working (0=live 1=inert 2=unaskable)."
  fi
  if [[ "$_lv_neg" == 1 ]]; then
    pass "the liveness probe says database.maxOpenConns is INERT under config.existingSecret, so it is not answering LIVE to everything"
  else
    fail "the liveness probe returned $_lv_neg for database.maxOpenConns, whose only effect is on the settings document the chart does not render under config.existingSecret. It should have said inert; an instrument that never says inert excuses every leaf put to it (0=live 1=inert 2=unaskable)."
  fi
  if [[ -z "$probe_misrefused" ]]; then
    pass "every leaf booked as guarded was either NAMED in the refusal it was booked on, or measured still live under config.existingSecret ($probe_live_named )"
  else
    fail "these leaves were booked as guarded on a refusal that names a different value:$probe_misrefused. The refusal fires for the companion the probe preamble had to set, not for the leaf, so an operator who sets the leaf alongside config.existingSecret is never told it went nowhere. Add them to the inline list in scion-hub.assertConfigSource, or - if their default makes them unrefusable - to PROBE_REFUSED_BY_COMPANION with the reason."
  fi
  # THE EXEMPTION LIST'S POSITIVE TWIN, the same rule PROBE_UNMUTABLE gets. An
  # exemption that no leaf takes is not harmless: it sits there reading like
  # coverage of a case that has stopped occurring, and it is the line someone
  # later points at to justify a third entry.
  # THE REFUSAL LIST NOTES.txt PRINTS, AGAINST THE REFUSALS THE WALK OBSERVED.
  # gd-p2-rev's R3 was two failures at once: the guard did not refuse the
  # database leaves, and NOTES.txt told operators the list of refusals was
  # "checked against the render rather than maintained by hand" when it was
  # neither complete nor checked. It is checked here, in both directions, so
  # that sentence has something behind it.
  sed -n '/define "scion-hub.existingSecretRefusals"/,/^{{- end }}/p' \
    "$CHART_DIR/templates/_helpers.tpl" \
    | sed -e '1d' -e '$d' | tr ',' '\n' \
    | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' \
    | grep -v '^$' | sort -u >"$WORK/probe-refusals-declared.txt"
  sort -u "$WORK/probe-refused-named.txt" >"$WORK/probe-refusals-observed.txt"
  # An empty declared list would make the diff below pass against an empty
  # observed list, which is the state a broken walk produces.
  [[ -s "$WORK/probe-refusals-declared.txt" ]] || meta_failure "scion-hub.existingSecretRefusals extracted to an empty list, so the parity check below is comparing nothing against nothing and would pass however the guard behaves."
  if diff -u "$WORK/probe-refusals-declared.txt" "$WORK/probe-refusals-observed.txt" >"$WORK/probe-refusals.diff"; then
    pass "the refusal list NOTES.txt prints is exactly the set of leaves the walk saw refused by name ($(tr '\n' ' ' <"$WORK/probe-refusals-observed.txt"))"
  else
    fail "scion-hub.existingSecretRefusals and the leaves the walk observed being refused by name disagree. '-' is declared to operators but never refused; '+' is refused without being declared: $(grep '^[-+][^-+]' "$WORK/probe-refusals.diff" | tr '\n' ' ')"
  fi
  # ADDING A REFUSAL CAN CLOSE THE LAST DOOR OUT OF A ROOM, and this one did.
  # With database.user refused under config.existingSecret, an operator on
  # auth=password had nowhere to stand: setting the user was refused as inert,
  # and NOT setting it hit "database.user is required when database.auth is
  # password" from the connection-budget query in NOTES.txt. Both refusals were
  # individually correct and together they made a legitimate install - "I supply
  # my own settings.yaml AND I authenticate with a password" - unrenderable.
  # A guard that refuses every value of a key is not a guard, it is a removal.
  for _es_auth in iam password; do
    _es_rc=0
    "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
      --skip-schema-validation "${BASE[@]}" --set config.existingSecret=mine \
      --set-string database.driver=postgres --set-string "database.auth=$_es_auth" \
      --set cloudsql.enabled=true --set-string cloudsql.instanceConnectionName=p:r:i \
      --set-string storage.provider=gcs --set acknowledgeHAUnlanded=true \
      >"$WORK/probe-es-$_es_auth.yaml" 2>&1 || _es_rc=$?
    if [[ "$_es_rc" == 0 ]]; then
      pass "config.existingSecret with Cloud SQL and database.auth=$_es_auth still renders, so the refusals above did not close off a configuration the chart supports"
    else
      fail "config.existingSecret + cloudsql + database.auth=$_es_auth does not render (helm exit $_es_rc): $(head -2 "$WORK/probe-es-$_es_auth.yaml"). The operator is refused whichever way they turn, which is not a guard - it is the silent removal of a supported install, announced as an error message about something else."
    fi
  done
  # shellcheck disable=SC2086  # intentional: splits the space-separated companion list to count entries
  _pc_n="$(printf '%s\n' $probe_companion_used | grep -c . || true)"
  if [[ "$_pc_n" -eq ${#PROBE_REFUSED_BY_COMPANION[@]} ]]; then
    pass "both companion-refused exemptions were exercised by a real leaf ($probe_companion_used ), so neither is excusing a case that no longer happens"
  else
    fail "PROBE_REFUSED_BY_COMPANION names ${#PROBE_REFUSED_BY_COMPANION[@]} leaves (${PROBE_REFUSED_BY_COMPANION[*]}) but $_pc_n took that path ($probe_companion_used ). An unexercised exemption is either a leaf that is now refused under its own name - delete the entry - or one that stopped being refused at all, which is the failure this step exists to catch."
  fi
  # Bounded rather than listed: a probe that breaks - helm gone, --set ignored,
  # the walk emptied - shows up as every leaf moving nothing, and a count
  # catches that where an allow-list of known-quiet names would not.
  if [[ $probe_quiet -le 2 ]]; then
    pass "$probe_quiet mutations moved nothing anywhere, within the tolerated 2"
  else
    fail "$probe_quiet mutations moved nothing anywhere ($probe_quiet_names) - either the probe has stopped working or these values do nothing; give them a PROBE_MUTATION that changes their effective value"
  fi
  if [[ $((probe_settings_only + probe_half)) -ge 12 ]]; then
    pass "$probe_settings_only settings-only and $probe_half part-settings values found and classified"
  else
    fail "only $((probe_settings_only + probe_half)) values were found to reach settings.yaml - the chart writes more than that, so the classification below is not seeing the document"
  fi
  if diff -u "$WORK/probe-declared.txt" "$WORK/probe-observed-sorted.txt" >"$WORK/probe-transfers.diff"; then
    pass "the transfer list matches the render, in both directions"
  else
    fail "the transfer list in _helpers.tpl disagrees with what the render does. '-' is declared and no longer true; '+' is a value that goes silent under config.existingSecret and is neither refused nor documented. Fix the list in scion-hub.existingSecretTransfers, values.yaml at config.existingSecret, or the guard"
    cat "$WORK/probe-transfers.diff"
  fi
fi

# values.yaml repeats the list for the reader who never runs helm install, and a
# copy that drifts is worse than no copy: it is the one an operator will act on.
# Compared both ways for that reason - an entry values.yaml still lists after the
# template dropped it is the more misleading direction, and a one-way "is it
# present" check is blind to exactly that one.
grep -E '^[[:space:]]*#[[:space:]]+[A-Za-z.]+[[:space:]]+->[[:space:]]+[A-Za-z_.]+[[:space:]]*$' \
  "$CHART_DIR/values.yaml" \
  | sed -E -e 's/^[[:space:]]*#[[:space:]]*//' -e 's/[[:space:]]*->[[:space:]]*/ /' \
           -e 's/[[:space:]]*$//' \
  | sort -u >"$WORK/probe-values-list.txt" || true
if diff -u "$WORK/probe-declared.txt" "$WORK/probe-values-list.txt" >"$WORK/probe-values.diff"; then
  pass "values.yaml repeats the transfer list exactly"
else
  fail "values.yaml and templates/_helpers.tpl disagree about the transfer list - '-' is in the template and missing from values.yaml, '+' is in values.yaml and no longer in the template"
  cat "$WORK/probe-values.diff"
fi

# --------------------------------------------------------------------------
step "the settings file is delivered read-only, over a writable state directory"
# --------------------------------------------------------------------------
# The vacuity guard for the HUB_HOME table. Every path assertion below is
# derived from it, and a table whose entries are all the same string derives
# nothing: the assertion would be satisfied by a template that had stopped
# reading hub.home entirely, which is the defect it exists to catch.
if [[ "$(printf '%s\n' "${HUB_HOME[@]}" | sort -u | wc -l)" -ge 2 ]]; then
  pass "at least two permutations set a different hub.home"
else
  fail "every permutation has the same hub.home - the mount-path checks below cannot distinguish a derived path from a hardcoded one"
fi
for name in "${PERMUTATIONS[@]}"; do
  if [[ -z "${HUB_HOME[$name]:-}" ]]; then
    fail "$name has no HUB_HOME entry - the mount-path checks would run against an empty prefix and match nothing"
    continue
  fi
done

# hub.home is the value whose failure mode is silent. If the state directory
# stops being derived from it - someone folds scion-hub.scionDir's printf into a
# literal - then HOME still moves, because configmap-env.yaml renders it from
# hub.home directly, while the mount does not. The hub then reads a settings.yaml
# that is not at the path it was mounted at, finds nothing, and starts on its own
# defaults; under those defaults isHADeployment() is false, so the HA preflight
# that would have refused to start is skipped by the same misconfiguration.
# Nothing fails and nothing logs.
#
# So all three are asserted against the table, and against each other: the state
# directory, the settings file inside it, and HOME.
for name in "${PERMUTATIONS[@]}"; do
  f="$WORK/$name.yaml"
  home="${HUB_HOME[$name]:-}"
  [[ -n "$home" ]] || continue
  scion_dir="$home/.scion"
  if grep -qF "mountPath: \"$scion_dir\"" "$f"; then
    pass "$name mounts a volume at $scion_dir"
  else
    fail "$name does not mount $scion_dir - hub.home is $home, so that is where the hub will look"
    grep -nF 'mountPath:' "$f" || true
  fi
  if grep -qF "HOME: \"$home\"" "$f"; then
    pass "$name sets HOME to $home"
  else
    fail "$name does not set HOME to $home - the mount and the variable have come apart, and only one of them moved"
    grep -nF 'HOME:' "$f" || true
  fi
  if grep -qF 'subPath: settings.yaml' "$f" && grep -qF "mountPath: \"$scion_dir/settings.yaml\"" "$f"; then
    pass "$name mounts settings.yaml as a subPath inside it"
  else
    fail "$name does not mount settings.yaml as a subPath at $scion_dir/settings.yaml"
  fi
  if grep -qF 'defaultMode: 0444' "$f"; then
    pass "$name projects the settings file 0444"
  else
    fail "$name does not project the settings file 0444 - 0600 would be unreadable to a non-root uid, and a decimal 444 is group-writable"
  fi
  # The directory must NOT be read-only: storage/, templates/ and scion-token
  # are created in it at runtime. A read-only mount here breaks the hub for
  # reasons that have nothing to do with configuration.
  #
  # Read by list item and not by line window. readOnly sits at an unknown offset
  # inside the item, so a window narrow enough to stay inside it (-A2) cannot
  # reach the key it is looking for, and a window wide enough to reach it runs
  # into the NEXT item - which is the settings mount, and which IS readOnly. A
  # wider window here does not strengthen the check, it inverts it.
  #
  # Both directions are asserted from the same extractor: the state directory
  # must not carry readOnly, the settings file must. An extractor that returned
  # nothing would satisfy the first on its own; it fails the second.
  mapfile -t home_items < <(yaml_list_items "$f" scion-home)
  if [[ "${#home_items[@]}" -eq 2 ]]; then
    pass "$name refers to the scion-home volume exactly twice, as a mount and as a volume"
  else
    fail "$name has ${#home_items[@]} scion-home list items, expected 2 (one volumeMount, one volume) - the read-only check below selects items by that name and passes vacuously against a rename"
  fi
  # THE STATE DIRECTORY IS AN emptyDir, ASSERTED BECAUSE A COMMENT DEPENDS ON IT.
  # values.yaml's updateStrategy paragraph used to say this chart "mounts no
  # volumes, so replicas share no mutable state at all" and concluded that the
  # strategy choice carries no data consequence. It mounts this one, and with no
  # server.database.url the hub puts its SQLite file inside it
  # (pkg/config/hub_config.go:691), so RollingUpdate's two-pod window has two
  # divergent hub.db files. The corrected paragraph rests on this volume being an
  # emptyDir; if Phase 4 makes it a PVC the paragraph changes again, and this is
  # what will say so.
  #
  # CONTROL, 2026-08-17: the obvious mutation - edit emptyDir out of one golden -
  # is caught by the golden-diff step first and never reaches here, so it proves
  # nothing about THIS assertion. Mutating templates/deployment.yaml to render a
  # persistentVolumeClaim and regenerating the goldens does reach it: 5 failures,
  # one per permutation, 254/254 executed.
  if [[ "${#home_items[@]}" -gt 0 ]] && printf '%s\n' "${home_items[@]}" | grep -qF 'emptyDir'; then
    pass "$name backs the hub's state directory with an emptyDir"
  else
    fail "$name does not back the hub's state directory with an emptyDir. If that is deliberate, the updateStrategy paragraph in values.yaml and the same sentence in values.schema.json both reason from it and must be rewritten in this diff."
  fi
  if [[ "${#home_items[@]}" -gt 0 ]] && printf '%s\n' "${home_items[@]}" | grep -qE 'readOnly: *true'; then
    fail "$name mounts the hub's state directory read-only; only settings.yaml may be read-only"
  else
    pass "$name leaves the hub's state directory writable"
  fi
  settings_mount="$(yaml_list_items "$f" settings | grep -F 'subPath: settings.yaml' || true)"
  if [[ -n "$settings_mount" ]] && grep -qE 'readOnly: *true' <<<"$settings_mount"; then
    pass "$name mounts settings.yaml read-only"
  else
    fail "$name does not mount settings.yaml read-only - the file is the one thing in that directory the hub may not write, and defaultMode 0444 alone does not stop a write by uid 0 or a rename by the owner"
  fi
  if grep -qF 'fsGroup' "$f"; then
    fail "$name sets fsGroup: it is pod-wide, so it grants every sidecar read access, and it makes the kubelet recursively chown mounted volumes"
  else
    pass "$name sets no fsGroup"
  fi
done
# No RUN-ONCE init container. The settings mount replaced one; a copy step
# reintroduces a writable per-pod file, which is the divergence the mount exists
# to prevent.
#
# WRITTEN IN ITS NARROWED FORM ALREADY, AT PHASE 1, WHERE IT IS VACUOUS. The
# obvious assertion here is that the string initContainers: never appears, and
# that is true today and WILL BE WRONG AT PHASE 2. The Cloud SQL proxy is a
# native sidecar, and a native sidecar is an initContainers entry carrying
# restartPolicy: Always - so the obvious assertion goes red on a change that is
# entirely correct, and the obvious fix is to delete it, which puts the copy
# shape back within reach of a green suite.
#
# So the rule is on the ENTRY, not on the section: every initContainers entry
# must carry restartPolicy: Always. A native sidecar passes; a run-once init
# container does not. Zero entries pass trivially, which is Phase 1's state and
# is exactly why the fixtures below exist - with nothing to scan, the scan proves
# nothing and only the fixtures show the detector still works. Narrow this
# further if a phase needs it. Do not delete it.
# NARROWED AT PHASE 2's REQUEST, BY gd-p2-dev's MEASUREMENT, BEFORE THE PROXY WAS
# WRITTEN. The first version started a new entry on ANY line whose first non-space
# character was a dash. A container's args:, command:, env: and volumeMounts: are
# all block sequences, so every list item inside an entry became its own unnamed
# entry with ok reset to 0 - and the real Cloud SQL proxy, which cannot function
# without args, was flagged as a run-once init container. Correct Phase 2 code,
# red suite, and the cheapest correct-looking fix is to delete the rule.
#
# It also had the dangerous defect in the other direction: with flush-style YAML
# (the dash at the same indent as initContainers:) the first dash was read as the
# END of the block, so a run-once container there was not flagged AT ALL.
#
# So entry boundaries are bound to the FIRST DASH'S INDENT, and name: and
# restartPolicy: are honoured only at the entry's own key indent - a nested
# "env: - name: FOO" cannot supply the container's name, and a nested
# restartPolicy: Always cannot satisfy the rule for the container.
init_entries_without_always() {
  awk '
    function flush() { if (entry && !ok) print name }
    {
      line = $0
      if (line ~ /^[[:space:]]*$/) next
      ind = match(line, /[^ ]/) - 1

      if (!inblock) {
        if (line ~ /^[ ]*initContainers:[ ]*$/) {
          inblock = 1; blockIndent = ind; entryIndent = -1
          entry = 0; ok = 0; name = "(unnamed)"
        }
        next
      }

      isDash = (line ~ /^[ ]*-([ ]|$)/)

      if (entryIndent < 0) {
        # No entry yet. The first dash at or below the block key fixes the entry
        # indent - AT OR BELOW, because flush-style puts it at the same column.
        if (!isDash || ind < blockIndent) { inblock = 0; next }
        entryIndent = ind
        if (match(line, /^[ ]*-[ ]+/)) keyIndent = RLENGTH; else keyIndent = ind + 2
        entry = 1; ok = 0; name = "(unnamed)"
        line = substr(line, keyIndent + 1)
      } else if (isDash && ind == entryIndent) {
        flush()
        if (match(line, /^[ ]*-[ ]+/)) keyIndent = RLENGTH; else keyIndent = ind + 2
        entry = 1; ok = 0; name = "(unnamed)"
        line = substr(line, keyIndent + 1)
      } else if (ind < entryIndent || (!isDash && ind == entryIndent)) {
        flush(); inblock = 0; entry = 0; entryIndent = -1
        next
      } else if (ind != keyIndent) {
        next          # deeper than the entry own keys: nested, not the container
      } else {
        line = substr(line, keyIndent + 1)
      }

      if (name == "(unnamed)" && line ~ /^name:[ ]/) {
        n = line; sub(/^name:[ ]*/, "", n); name = n
      }
      if (line ~ /^restartPolicy:[ ]*Always[ ]*$/) ok = 1
    }
    END { if (inblock) flush() }
  ' "$1"
}

# THE FIXTURES, AND WHAT THEY ARE AND ARE NOT CLAIMS ABOUT.
#
# READ THIS BEFORE ADDING ONE. These are claims about THE PARSER - about how it
# binds entry boundaries, names and restartPolicy to indentation. THEY ARE NOT
# CLAIMS ABOUT THE CLOUD SQL PROXY. Every one of them was typed by hand, and a
# hand-typed fixture is the author's summary of the subject, not the subject.
#
# The first version of this block had three fixtures and the sidecar one was:
#
#     - name: cloudsql-proxy
#       image: proxy
#       restartPolicy: Always
#
# described in the phase notes as standing proof that the rule does not fire on
# the Cloud SQL proxy. It has no args:. It was constructed by asking "what is the
# least YAML that carries restartPolicy: Always?" - that is, FROM THE PREDICATE
# UNDER TEST - and a fixture derived from the predicate can only confirm the
# predicate. The property that makes the real proxy fail was precisely the
# property the simplification omitted, and the suite was green throughout.
#
# So: the fixtures below say the indent binding is correct. The claim "the rule
# does not fire on the Cloud SQL proxy" can only be discharged by a fixture cut
# from an actual `helm template` render of the proxy, and only the phase that
# ships the proxy can produce one. That fixture is owed by Phase 2, with the
# command that produced it named beside it. Until it exists, that claim is
# unmeasured - and the PERMUTATIONS scan below is the standing check that the
# real render stays clean once there is one.
cat >"$WORK/fx-plain.yaml" <<'FX'
    spec:
      initContainers:
        - name: settings-init
          image: busybox
FX
cat >"$WORK/fx-sidecar.yaml" <<'FX'
    spec:
      initContainers:
        - name: cloudsql-proxy
          image: proxy
          restartPolicy: Always
      containers:
        - name: hub
FX
cat >"$WORK/fx-both.yaml" <<'FX'
    spec:
      initContainers:
        - name: cloudsql-proxy
          image: proxy
          restartPolicy: Always
        - name: settings-init
          image: busybox
      containers:
        - name: hub
FX
cat >"$WORK/fx-args-last.yaml" <<'FX'
    spec:
      initContainers:
        - name: cloud-sql-proxy
          image: proxy
          args:
            - --structured-logs
            - --port=5432
          restartPolicy: Always
      containers:
        - name: hub
FX
cat >"$WORK/fx-args-first.yaml" <<'FX'
    spec:
      initContainers:
        - name: cloud-sql-proxy
          restartPolicy: Always
          image: proxy
          args:
            - --structured-logs
            - --port=5432
      containers:
        - name: hub
FX
cat >"$WORK/fx-runonce-command.yaml" <<'FX'
    spec:
      initContainers:
        - name: settings-init
          image: busybox
          command:
            - sh
            - -c
            - cp /src/settings.yaml /dst/settings.yaml
      containers:
        - name: hub
FX
cat >"$WORK/fx-nested-name.yaml" <<'FX'
    spec:
      initContainers:
        - name: settings-init
          image: busybox
          env:
            - name: FOO
              value: bar
      containers:
        - name: hub
FX
cat >"$WORK/fx-nested-always.yaml" <<'FX'
    spec:
      initContainers:
        - name: settings-init
          image: busybox
          lifecycle:
            postStart:
              restartPolicy: Always
      containers:
        - name: hub
FX
cat >"$WORK/fx-flush-runonce.yaml" <<'FX'
    spec:
      initContainers:
      - name: settings-init
        image: busybox
      containers:
      - name: hub
FX
cat >"$WORK/fx-flush-sidecar.yaml" <<'FX'
    spec:
      initContainers:
      - name: cloud-sql-proxy
        image: proxy
        args:
        - --structured-logs
        restartPolicy: Always
      containers:
      - name: hub
FX

# THE REAL RENDER. Everything above is a fixture I wrote, and a fixture written
# by the author of the predicate can only confirm the predicate - gd-p2-dev's
# rule, filed as theirs. This one is `helm template` output from the Cloud SQL
# phase, pasted verbatim from gd-p2-dev's branch and not retyped from a
# description of it. Produced by:
#
#   helm template t . -f ci/values-minimal.yaml \
#     --set database.driver=postgres --set database.auth=iam --set database.name=scion \
#     --set serviceAccount.gcpServiceAccount=scion-hub@my-project.iam.gserviceaccount.com \
#     --set cloudsql.enabled=true \
#     --set cloudsql.instanceConnectionName=my-project:us-central1:scion-db \
#     --set storage.provider=gcs --set storage.bucket=b --set acknowledgeHAUnlanded=true
#
# It cannot be produced on THIS branch, measured: database.auth, database.name
# and the whole cloudsql object are absent from P1's values.schema.json, which is
# additionalProperties: false, so that command here returns "Additional property
# cloudsql is not allowed". That is why it is committed as a captured artifact
# with its producing command rather than regenerated - and why the command is
# recorded, because when the Cloud SQL phase merges this fixture becomes
# reproducible and the comment says how.
#
# It carries the two properties most likely to break an entry-boundary rule and
# both are load-bearing: restartPolicy: Always is the LAST key of the entry, and
# a nested `- ALL` sequence sits between the entry's first dash and it.
cat >"$WORK/fx-real-proxy.yaml" <<'FX'
apiVersion: apps/v1
kind: Deployment
spec:
  template:
    spec:
      initContainers:
        - name: cloud-sql-proxy
          image: "gcr.io/cloud-sql-connectors/cloud-sql-proxy@sha256:825d5e4ce70d38bd0006c9eea15a6a2e2983e87b31ac6924d33e2dba56eafc9f"
          imagePullPolicy: IfNotPresent
          args:
            - "--structured-logs"
            - "--port=5432"
            - "--health-check"
            - "--http-address=0.0.0.0"
            - "--http-port=9801"
            - "--auto-iam-authn"
            - "my-project:us-central1:scion-db"
          startupProbe:
            httpGet:
              path: /startup
              port: 9801
            periodSeconds: 1
            failureThreshold: 60
            timeoutSeconds: 5
          readinessProbe:
            httpGet:
              path: /readiness
              port: 9801
            periodSeconds: 10
            failureThreshold: 3
            timeoutSeconds: 5
          securityContext:
            runAsNonRoot: true
            runAsUser: 1000
            runAsGroup: 1000
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop:
                - ALL
          restartPolicy: Always
      containers:
        - name: hub
          image: x
FX

# THE TWO NEGATIVES ARE CUT FROM THE REAL RENDER, MECHANICALLY, not written out
# beside it. A hand-typed "broken proxy" is a fixture again, and it would drift
# from the artifact above the first time either is edited. These two are the same
# bytes with one line changed, so the ONLY difference between the accepted input
# and the flagged input is the property under test.
grep -vF 'restartPolicy: Always' "$WORK/fx-real-proxy.yaml" >"$WORK/fx-real-proxy-norestart.yaml"
sed 's/^          restartPolicy: Always$/            restartPolicy: Always/' \
  "$WORK/fx-real-proxy.yaml" >"$WORK/fx-real-proxy-nested.yaml"
# CONTROL ON THE CUT ITSELF: a sed that matched nothing would leave the negative
# byte-identical to the positive, and it would then pass for the wrong reason -
# green, and testing nothing.
if cmp -s "$WORK/fx-real-proxy.yaml" "$WORK/fx-real-proxy-nested.yaml"; then
  echo "HARNESS ERROR: the nested-restartPolicy negative is byte-identical to the real render, so the sed matched nothing and that case tests nothing." >&2
  exit 2
fi
if cmp -s "$WORK/fx-real-proxy.yaml" "$WORK/fx-real-proxy-norestart.yaml"; then
  echo "HARNESS ERROR: the no-restartPolicy negative is byte-identical to the real render, so the grep removed nothing and that case tests nothing." >&2
  exit 2
fi

# EXACT offender lists, not merely empty/non-empty. A non-empty assertion passes
# when the detector flags the right container for the wrong reason, and that is
# how the args: defect stayed invisible: fx-both was flagged, correctly, while
# the same input with args: would have been flagged three times over.
FX_CASES=(
  "fx-plain|settings-init|flags a run-once init container"
  "fx-sidecar||accepts a native sidecar"
  "fx-both|settings-init|flags a run-once container alongside a sidecar, and flags only that one"
  "fx-args-last||accepts a sidecar whose args: is a nested block sequence, restartPolicy last"
  "fx-args-first||accepts the same sidecar with restartPolicy first"
  "fx-runonce-command|settings-init|flags a run-once container that has a command: list, naming it once"
  "fx-nested-name|settings-init|does not let a nested env: - name: supply the container's name"
  "fx-nested-always|settings-init|does not let a nested restartPolicy: Always satisfy the rule for the container"
  "fx-flush-runonce|settings-init|flags a run-once container written flush with initContainers:"
  "fx-flush-sidecar||accepts a flush-style sidecar"
  "fx-real-proxy||accepts the REAL Cloud SQL proxy render, restartPolicy last, past a nested capabilities.drop sequence"
  "fx-real-proxy-norestart|cloud-sql-proxy|flags the real render with its restartPolicy line removed, and names it once"
  "fx-real-proxy-nested|cloud-sql-proxy|flags the real render with its restartPolicy indented one level deeper"
)
if [[ ${#FX_CASES[@]} -ne 13 ]]; then
  echo "HARNESS ERROR: FX_CASES holds ${#FX_CASES[@]} cases, not 13. The init-container fixtures were edited without moving the count, so this block's contribution to EXPECTED_TOTAL is no longer known." >&2
  exit 2
fi
for _case in "${FX_CASES[@]}"; do
  IFS='|' read -r _fx _want _what <<<"$_case"
  _got="$(init_entries_without_always "$WORK/$_fx.yaml" | tr '\n' ' ' || true)"
  _got="${_got% }"
  if [[ "$_got" == "$_want" ]]; then
    pass "the init-container rule $_what"
  else
    fail "the init-container rule should $_what, and does not: $_fx.yaml gave offenders [$_got], expected exactly [$_want]"
  fi
done

for name in "${PERMUTATIONS[@]}"; do
  offenders="$(init_entries_without_always "$WORK/$name.yaml")"
  if [[ -n "$offenders" ]]; then
    fail "$name has a run-once init container ($(tr '\n' ' ' <<<"$offenders")) - settings are delivered by mount, not by copy. A native sidecar carries restartPolicy: Always and is allowed."
  else
    pass "$name has no run-once init container"
  fi
done

# --------------------------------------------------------------------------
step "the \$ownedByConfig split, measured against the render"
# --------------------------------------------------------------------------
# _helpers.tpl reserves five flags on the grounds that each has a delivery
# channel other than argv. Two of the five are delivered by this chart and three
# are not, and the file says which in prose.
#
# THIS EXISTS BECAUSE THAT PROSE WENT STALE WITHOUT THE FILE BEING EDITED. At
# phase 0 it read "this chart delivers none of them yet", which was true while
# nothing was rendered and false the moment a ConfigMap and a Secret were - and
# no test anywhere referenced any of it, so nothing went red. A paragraph whose
# truth depends on another file's contents needs an assertion in the same
# repository or it is true by accident until someone reads it.
#
# The numbers below are the committed state. Moving an entry from 0 to 1 is a
# deliberate act in the diff that lands the channel, in the same diff that edits
# the paragraph - which is the point: the two move together or this goes red.
#
# Measured under the "settings" permutation, named here rather than assumed: it
# is the one that renders the full settings file with a bucket. A permutation
# that renders less would report 0 for storage-bucket and be right about itself.
declare -A DELIVERED=(
  [base-url]=1        # SCION_SERVER_BASE_URL, configmap-env.yaml
  [storage-bucket]=1  # server.storage.bucket in the rendered settings.yaml
  [db]=1              # server.database.url - LANDED by the Cloud SQL phase; was 0 until then
  [storage-dir]=0     # server.storage.local_path - the workspace share
  [admin-emails]=0    # server.hub.admin_emails - no phase claims it
)
# The probe per flag. Each reads the channel the reservation names, not a proxy
# for it: a probe for "is there a Secret" would answer yes for a chart that
# renders a Secret full of something else, which is how the session-secret
# reservation was nearly misfiled. Keys are the V1 settings spelling, which is
# snake_case and is NOT the spelling on HubServerConfig - settings.yaml's server
# section decodes into V1ServerConfig (pkg/config/hub_config.go, at
# loadServerFromSettingsFile), where HubID is `hub_id`; the camelCase `hubId` on
# HubServerConfig belongs to the koanf/env path and would be ignored here.
delivery_probe() {
  local flag="$1" render="$2" block="$3"
  case "$flag" in
    base-url)       grep -qE '^  SCION_SERVER_BASE_URL: ' <<<"$render" ;;
    storage-bucket) grep -qE '^    bucket: .' <<<"$block" ;;
    db)             grep -qE '^    url: .' <<<"$block" ;;
    storage-dir)    grep -qE '^    local_path: .' <<<"$block" ;;
    admin-emails)   grep -qE '^    admin_emails:' <<<"$block" ;;
    *)              meta_failure "delivery_probe has no probe for $flag" ;;
  esac
}
render="$(cat "$WORK/settings.yaml")"
block="$(settings_block "$WORK/settings.yaml")"
delivered_seen=0
undelivered_seen=0
for flag in base-url storage-bucket db storage-dir admin-emails; do
  want="${DELIVERED[$flag]}"
  if delivery_probe "$flag" "$render" "$block"; then got=1; else got=0; fi
  [[ $got -eq 1 ]] && delivered_seen=$((delivered_seen + 1)) || undelivered_seen=$((undelivered_seen + 1))
  if [[ $got -eq $want ]]; then
    pass "-$flag delivery channel: $got, as committed (settings permutation)"
  elif [[ $want -eq 0 ]]; then
    fail "-$flag now has a delivery channel in the render and the committed state says it does not. Bump it to 1 here AND re-tense the \$ownedByConfig prose in _helpers.tpl in the same diff - the refusal and its justification both name which flags are live."
  else
    fail "-$flag has no delivery channel in the render and the committed state says it does. Either the channel regressed or the number is wrong; do not lower the number to match without reading the template."
  fi
done
# The coverage control. An all-0 table is satisfied by a probe function that
# never matches anything - a renamed key, a changed indent, a typo in the case
# arm - and an all-1 table by one that always does. Requiring both outcomes to
# occur means at least one probe is discriminating in each direction. This is
# not the same check as the vacuity guard on the table's literals: that one reads
# the constants, this one reads what the probes actually returned.
if [[ $delivered_seen -ge 1 && $undelivered_seen -ge 1 ]]; then
  pass "the delivery probes returned both outcomes ($delivered_seen delivered, $undelivered_seen not)"
else
  fail "every delivery probe returned the same answer ($delivered_seen delivered, $undelivered_seen not) - one broken probe function produces exactly this, and it would agree with a table of all 0s or all 1s"
fi
# The prose half. The sentences below are the ones that were true at phase 0 and
# are false here; naming them by their text is deliberate, because the failure
# mode is somebody restoring the paragraph wholesale from the phase-0 file.
#
# QUOTED SPANS ARE STRIPPED FIRST, AND THAT IS NOT A CONVENIENCE. The prose that
# explains why a sentence went stale has to reproduce the sentence, so a literal
# grep flags the warning along with the defect - and the repair it invites is
# deleting the warning. This check tripped on its own paragraph on its first run,
# which is the same false positive in the same file on the same afternoon. A
# double-quoted span in template prose is a quotation, so removing those spans
# before matching leaves only the sentences the file is asserting in its own
# voice. The cost is that a claim written inside quotes is invisible here; write
# claims unquoted.
strip_quotes() { sed 's/"[^"]*"//g'; }
prose="$(find "$CHART_DIR/templates" -type f -exec cat {} + | strip_quotes || true)"
prose_raw="$(find "$CHART_DIR/templates" -type f -exec cat {} +)"
for stale in \
  'This chart delivers none of them yet' \
  'none of them lands anywhere' \
  'Nothing in this chart yet feeds hub.hubId into the running process' \
  ; do
  if grep -qF -- "$stale" <<<"$prose"; then
    fail "templates/ asserts ${stale@Q} in its own voice, which was true before this chart rendered a settings file and is false now"
  else
    pass "the phase-0 sentence ${stale@Q} is not asserted"
  fi
done
# The control for the stripper, and it is a coverage control rather than an
# apparatus mutation: it proves the three checks above pass because the
# quotations are quoted, not because the text is missing or the path is wrong.
# One of those three sentences IS still in the tree, inside quotes, in the
# paragraph that explains it went stale. If that ever stops being true the
# stripper is untested by the checks above and they all pass for free.
if grep -qF -- 'none of them lands anywhere' <<<"$prose_raw"; then
  pass "the stale sentence is still present as a quotation, so the quote stripper is what makes the check above pass"
else
  fail "no quoted instance of the stale sentence remains anywhere in templates/ - the three checks above are now passing without the stripper being exercised, and a bug in it would be invisible"
fi
# The counter-form, per the rule that a negative grep's control is the positive
# form of the same grep. Three greps that must find nothing are satisfied by a
# CHART_DIR that points at the wrong place; these two must find something in the
# same tree, so a path that greps nothing goes red here.
for present in \
  'server.hub.hub_id in the mounted' \
  'DELIVERED HERE' \
  ; do
  # NOT `grep -rqF ... "$CHART_DIR/templates/"`, which is what this was. It was
  # the only recursive grep in the chart's gates, and a recursive grep is the one
  # shape where the search tool decides the corpus. Under the Claude Code shell
  # snapshot's grep wrapper that decision includes -I (binaries dropped), six
  # --exclude-dir flags, --hidden (dotfiles ADDED) and --ignore-files (an ignore
  # file at or below the root honoured with full gitignore semantics). None of
  # that reaches a bash script - measured, `bash -c 'type grep'` is
  # /usr/bin/grep - and templates/ happens to hold no binaries and no
  # dot-directories, but both of those are facts about today's tree rather than
  # properties of this check. $prose_raw is the same corpus enumerated by the
  # find above and concatenated, so the corpus is decided here and grep only
  # answers about bytes. Concatenation order varies between find implementations
  # and does not matter: this is a presence test over the join.
  if grep -qF -- "$present" <<<"$prose_raw"; then
    pass "the replacement wording ${present@Q} is present"
  else
    fail "templates/ does not contain ${present@Q} - either the re-tensed prose was reverted, or this check is reading the wrong directory and the three checks above found nothing for that reason"
  fi
done

# --------------------------------------------------------------------------
step "the hub ID is one input rendered twice, and the two must agree"
# --------------------------------------------------------------------------
# scion.io/hub-id on the pod template and server.hub.hub_id in the settings file
# are two renderings of hub.hubId. Until this chart rendered a settings file the
# annotation had nothing to agree with, and deployment.yaml said so - it told the
# reader a disagreement was legitimate. It is not legitimate now, and a paragraph
# saying otherwise is worse than no paragraph, because it stops the next person
# debugging an ID mismatch from looking.
#
# A marker value rather than the ci/ values, so that a template which stopped
# reading hub.hubId and emitted a constant cannot pass: both sites must carry the
# marker, and the marker appears in no file in this repository.
hubid_marker=zzmarkerhubid
"$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
  --values "$CHART_DIR/ci/values-settings.yaml" \
  --set hub.hubId="$hubid_marker" >"$WORK/hubid.yaml" 2>&1 \
  || meta_failure "the hub-id marker render failed, so nothing below was checked"
# `|| true` on both, and OP-5 (gd-p1-rev, round 5) is why. These are the
# single-permutation siblings of the loop twenty lines below, and they had the
# RQ-3 shape: under `set -e -o pipefail` a grep that matches nothing takes the
# script out on this line. gd-p1-rev measured it on the REAL subject rather than
# on the apparatus - deleting `scion.io/hub-id:` from deployment.yaml, which is
# exactly the defect these three assertions exist to catch - and got rc 1 with
# the output truncated and 0 occurrences of any of the three fail messages.
# All three hand-written diagnostics were unreachable for the case they were
# written for. The goldens catch it five sections earlier so the run still went
# red; this is diagnostic loss, not a fail-open, and the empty string now reaches
# the ${ann_id@Q} in the message instead of killing the run.
ann_id="$(grep -oE 'scion\.io/hub-id: .*' "$WORK/hubid.yaml" | sed -e 's/.*: //' -e 's/"//g' | sort -u || true)"
set_id="$(settings_block "$WORK/hubid.yaml" | sed -n 's/^    hub_id: //p' | sed 's/"//g' | sort -u || true)"
if [[ $ann_id == "$hubid_marker" ]]; then
  pass "the pod annotation carries the supplied hub ID"
else
  fail "the pod annotation is ${ann_id@Q}, not the supplied ${hubid_marker@Q} - one value, or none, or several"
fi
if [[ $set_id == "$hubid_marker" ]]; then
  pass "settings.yaml server.hub.hub_id carries the supplied hub ID"
else
  fail "server.hub.hub_id is ${set_id@Q}, not the supplied ${hubid_marker@Q}"
fi
if [[ -n $ann_id && $ann_id == "$set_id" ]]; then
  pass "the annotation and server.hub.hub_id are the same string"
else
  fail "the annotation (${ann_id@Q}) and server.hub.hub_id (${set_id@Q}) disagree, or one of them is missing - they come from one helper and cannot legitimately differ"
fi
# Every permutation that renders a settings file, not just the marker render, so
# a values file that reaches one site and not the other is caught.
for name in "${PERMUTATIONS[@]}"; do
  # || true on both: under config.existingSecret there is no settings block at
  # all, so the extractor's grep finds nothing and exits 1, which set -e would
  # take as a reason to abandon the run. An empty string is the answer here, and
  # the branch below asserts which permutation is allowed to produce one.
  #
  # THIS SITE USED TO CARRY `2>/dev/null` AND IT WAS A DEMONSTRATED FAIL-OPEN
  # (gd-p2-dev's criterion: does an ERROR render as a clean absence in a
  # published result). The existing-secret arm passes on `-z $s`, so an EMPTY
  # extractor result is its success condition - and a BROKEN extractor also
  # produces empty. Measured rather than reasoned: pointing this one line at a
  # file that does not exist, a total instrument failure,
  #
  #   minimal         FAIL      settings   FAIL      settings-oauth  FAIL
  #   varied          FAIL      existing-secret  ok   <- GREEN ON AN ERROR
  #   stderr captured by the harness: 0 lines, because 2>/dev/null ate sed's
  #   complaint about the missing file
  #
  # Four arms went red and the fifth reported success, on the same broken
  # instrument. The old comment justified the `|| true` at length and said
  # nothing about the `2>/dev/null`, which is the half that did the damage.
  #
  # TWO CHANGES, because the empty needs to become meaningful rather than merely
  # tolerated. First, stderr is captured and a single byte on it is a
  # META-FAILURE: the extractor has no legitimate reason to complain, so if it
  # does, this run is not evidence either way. Second, the emptiness gets a
  # POSITIVE TWIN - the arm now asserts the structural fact that MAKES it empty,
  # that the render carries no settings.yaml key at all.
  #
  # THE TWIN IS TWO SIGNALS, NOT ONE, AND OP-4 (gd-p1-rev, round 5) IS WHY.
  # `_has_block` greps '^  settings\.yaml: \|' while settings_block():304 seds on
  # /^  settings\.yaml: |/ - THE SAME BYTES. That is independence of the PIPELINE
  # but not of the LOCATOR, and my commit message claimed the stronger thing. So
  # `_has_obj` answers the same question from a different direction entirely: is
  # there a document of kind Secret whose name ends in -settings. It keys on the
  # object's identity rather than on the key's text, so a locator that drifts
  # cannot take both signals with it.
  #
  # N-2, same round: these messages used to say "the ConfigMap". Measured off the
  # render - the settings.yaml key is at line 31 inside `kind: Secret
  # name: r-scion-hub-settings`, and the chart's actual ConfigMap is
  # r-scion-hub-env at line 87 and does not carry it. The name pointed the reader
  # at the wrong template, and at the one object this PR exists to add.
  a="$(grep -oE 'scion\.io/hub-id: .*' "$WORK/$name.yaml" | sed -e 's/.*: //' -e 's/"//g' | sort -u || true)"
  _sberr="$WORK/hubid-$name.err"
  s="$(settings_block "$WORK/$name.yaml" 2>"$_sberr" | sed -n 's/^    hub_id: //p' | sed 's/"//g' | sort -u || true)"
  if [[ -s $_sberr ]]; then
    meta_failure "the hub_id extractor wrote to stderr for the $name permutation, so an empty hub_id below cannot be read as 'the chart rendered none'. Stderr: $(tr '\n' ' ' <"$_sberr")"
  fi
  if grep -qE '^  settings\.yaml: \|' "$WORK/$name.yaml"; then _has_block=yes; else _has_block=no; fi
  # The second, locator-independent signal: an object of kind Secret named
  # *-settings. Keyed on the document's identity, not on the key's text.
  _has_obj=no
  if awk '/^---/ {k=""; n=""; next}
          /^kind:/ {k=$2}
          /^  name:/ {if (n=="") n=$2}
          k=="Secret" && n!="" && n ~ /-settings$/ {found=1}
          END {exit !found}' "$WORK/$name.yaml"; then _has_obj=yes; fi
  if [[ $name == existing-secret ]]; then
    # The documented exception, asserted rather than skipped. Here the settings
    # file is the operator's and the chart renders none, so there is genuinely
    # nothing to agree with - and the annotation must still be there, because an
    # absent annotation would also satisfy an equality check against nothing.
    if [[ -n $a && -z $s && $_has_block == no && $_has_obj == no ]]; then
      pass "$name: the annotation is rendered, the chart renders no settings Secret and no settings.yaml key, and so there is no chart hub_id to agree with it"
    else
      fail "$name: expected an annotation, no rendered hub_id, no settings.yaml key and no settings Secret; got annotation ${a@Q}, hub_id ${s@Q}, settings.yaml key present: ${_has_block}, settings Secret present: ${_has_obj}"
    fi
  elif [[ -n $a && $a == "$s" && $_has_block == yes && $_has_obj == yes ]]; then
    pass "$name: the annotation and server.hub.hub_id agree (${a@Q})"
  else
    fail "$name: the annotation (${a@Q}) and server.hub.hub_id (${s@Q}) disagree or are missing (settings.yaml key present: ${_has_block}, settings Secret present: ${_has_obj})"
  fi
done

# --------------------------------------------------------------------------
step "no assignment in this script can abort it silently under set -e -o pipefail"
# --------------------------------------------------------------------------
# THIS GATE EXISTS BECAUSE THE FIX FOR RQ-2 INTRODUCED RQ-3 (gd-p1-rev, round 5).
# Line 78 is `set -euo pipefail`. Under pipefail an assignment whose pipeline
# fails anywhere takes the whole script out ON THAT LINE, before any reporting
# machinery runs - rc 1, zero bytes on stdout, zero bytes on stderr. Every
# assertion below the abort silently does not happen, and the harness's own
# design goal is that a run which could not execute something SAYS SO and keeps
# going.
#
# Three separate instances of the shape shipped and all three were found by a
# reviewer rather than by this suite: the toolchain banner (:247-248), the golden
# digest binding (:970), and the single-permutation hub_id block (OP-5). In each
# case the hand-written diagnostic for the exact failure it was written for was
# UNREACHABLE. Patching three lines would leave the fourth to be found the same
# way, so this is the class and not the instances.
#
# THE RULE: an assignment from a pipeline must carry `|| true` or its own `||`
# fallback. This is deliberately syntactic. A semantic version would have to
# decide which pipelines "can" fail, and my own record on that question this
# morning is that I cleared :970 in writing on a reason that was false.
# `\+?=` MATCHES APPENDS, AND WITHOUT IT THIS GATE WAS BLIND TO TWO SITES THE
# SAME COMMIT ADDED (gd-p1-rev, round 6). `+` is not in [A-Za-z0-9_], so the name
# class stopped at the `+` and `x+="$(a | b)"` was invisible. The two it could
# not see were verify.sh:2570-2571, added by the notes gate below:
#
#   _ny_settings+="$(settings_block "$WORK/$name.yaml" || true)"$'\n'
#   _ny_kinds+="$(grep -E '^kind:' "$WORK/$name.yaml" || true)"$'\n'
#
# Both happen to carry `|| true`, so the tree was never live-broken. THE GATE WAS
# NOT LYING ABOUT THE TREE, IT WAS LYING ABOUT THE CLASS - which is the only
# thing it exists to do. gd-p1-rev demonstrated rather than argued it: remove the
# one `|| true` from :2570 and nothing else, and since `settings_block` ends in
# `grep -E '^    '` which matches nothing for the existing-secret permutation,
#
#   rc 1, stdout 18698 bytes, stderr 0 bytes, no "assertions:" summary at all,
#   and BOTH assertions below printed `ok` on the way past.
#
# THE GATE CERTIFIED THE SCRIPT AT :2452 AND THE SCRIPT DIED AT :2570, 160 LINES
# BELOW ITS OWN CERTIFICATE. Extent after the two characters: 31 -> 33 sites.
_pipe_sites() {  # $1 = script to scan. prints "line:text" for every such assignment.
  grep -nE '^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*\+?="?\$\(.*\|.*\)' "$1" \
    | grep -v '^[0-9]*:[[:space:]]*#' || true
}
# A KNOWN IMPRECISION, STATED RATHER THAN LEFT FOR THE NEXT READER TO FIND.
# The guard test is `||` anywhere on the line, so `x="$(a || b | c)"` - which
# bash parses as `a || (b | c)`, still aborts, and is NOT guarded - would score
# as guarded. gd-p1-rev checked and no line of that shape exists in this tree
# (round 6, non-blocking), and the fixture below pins the shapes that do. The
# detector is deliberately syntactic, so it is imprecise in this direction; it
# under-reports offenders and never invents one.
_pipe_unguarded() { _pipe_sites "$1" | grep -v '||' || true; }

_self="${BASH_SOURCE[0]}"
_ps_total="$(_pipe_sites "$_self" | wc -l || true)"
_ps_bad="$(_pipe_unguarded "$_self" | wc -l || true)"

# EXTENT FIRST, so the zero below is a live zero and not an empty scan.
#
# AN EXACT PIN, NOT A FLOOR, AND I HAD ALREADY LOST THIS ARGUMENT ONCE. This read
# `-lt 20` against an actual 31 - eleven sites of slack, in which the pattern
# could rot most of the way to nothing and still report the gate "live". A floor
# cannot see a fail-open. That is precisely the objection gd-p1-rev raised
# against `probe_total -ge 50` in round 3, which I accepted then, and it is the
# design of EXPECTED_TOTAL at :146, which fails on every added assertion and has
# survived six rounds without anyone deleting it. My stated reason for the floor
# - "a gate that fails on every new assignment is a gate people delete" - is
# refuted by the pin thirty lines up in the same file.
#
# So: bump this number in the diff that adds the assignment. That is the same
# contract every other pinned count in this suite carries.
PIPE_SITES_EXPECTED=44
if [[ "$_ps_total" -ne "$PIPE_SITES_EXPECTED" ]]; then
  meta_failure "the pipeline-assignment sweep found $_ps_total sites in $_self, pinned at $PIPE_SITES_EXPECTED. If you added an assignment-from-a-pipeline, give it a || fallback and bump PIPE_SITES_EXPECTED in the same diff. If you did not, the pattern has stopped matching and the zero below would mean nothing."
else
  pass "the pipeline-assignment sweep is live: $_ps_total assignment-from-pipeline sites found in this script, matching the pinned $PIPE_SITES_EXPECTED"
fi

# THE CLAIM IS NARROWED TO WHAT THE DETECTOR ACTUALLY REACHES, AND THE GAP IS
# MEASURED RATHER THAN WAVED AT. This used to read "so none can abort this script
# silently" - a UNIVERSAL over every assignment in the file. The detector only
# sees a line carrying a literal `|`, so the universal was false by 18 sites:
#
#   assignments from a command substitution, no `|` on the line, no `||`   18
#   of those, calls to settings_block(), which ends in `grep -E '^    '`
#   and therefore RETURNS NON-ZERO for any permutation with no settings
#   block - the exact function gd-p1-rev's round-6 demonstration exploited    5
#
#     :598  db_block="$(settings_block "$WORK/settings.yaml")"
#     :757  block="$(settings_block "$WORK/settings.yaml")"
#     :856  block="$(settings_block "$WORK/$name.yaml")"
#     :1251 block="$(settings_block "$WORK/settings.yaml")"
#     :2194 block="$(settings_block "$WORK/settings.yaml")"
#
# They do not abort TODAY because each runs against a permutation that has a
# settings block. That is a fact about the current call sites, not a property of
# the code, and it is the same kind of luck that RQ-3 was resting on.
#
# I am not silently widening the gate to cover them in the commit that fixes a
# different hole in it - some of the 18 SHOULD abort (`WORK="$(mktemp -d)"` is
# not a site that wants `|| true`), so the class needs triage and not a blanket
# rule. Filed to gd-p1-rev and gd-em for disposition. Until then the sentence
# below says only what was checked.
if [[ "$_ps_bad" -eq 0 ]]; then
  pass "all $_ps_total assignments-from-a-pipeline carry || true or their own || fallback. NOTE: this covers lines containing a literal pipe; 18 assignments from a plain command substitution are outside the detector and are not certified by this row"
else
  fail "$_ps_bad assignment(s) from a pipeline carry no || fallback. Under set -e -o pipefail each is a silent abort with zero bytes on both streams: $(_pipe_unguarded "$_self" | tr '\n' ' ')"
fi

# COVERAGE CONTROL. The assertion above is a zero, and a detector that finds
# nothing anywhere produces the same zero as a clean script. Seed a fixture with
# known instances and assert an ABSOLUTE EXPECTED COUNT - not agreement with the
# subject, which would let both arms be wrong together.
_pf="$WORK/pipefail-fixture.sh"
# THE FIXTURE IS ASSEMBLED, NOT WRITTEN LITERALLY, AND THAT IS NOT STYLE.
# I first wrote these six lines as a quoted heredoc. The heredoc's body IS part
# of this file, so `_pipe_sites "$_self"` found the two seeded bad lines in
# MY OWN SOURCE and the gate failed on its own fixture - site count 27 -> 35,
# two spurious offenders reported at the fixture's line numbers. THE INSTRUMENT
# WROTE ITS OWN NEEDLE INTO ITS OWN HAYSTACK, which is the failure gd-em
# broadcast fleet-wide at 12:22, reproduced here about fifteen minutes later by
# the person quoting it. Caught by the gate itself, which is the argument for
# building the subject assertion before the control.
#
# So `$(` is never written literally below: it is composed from $_S at runtime,
# and this file therefore contains no assignment-from-pipeline to find.
# THE `+=` CASES ARE SEEDED HERE, NOT JUST COUNTED IN THE PIN. gd-p1-rev's
# instruction on its own finding, and it is the right one: "do not just bump the
# numbers - the fixture is what makes the 0 a live 0, and a `+=` case must be IN
# it or the next reader restores this hole." A pin moved from 4/2 to 6/3 with no
# `+=` line in the fixture would pass just as happily with `\+?` deleted again.
_S='$'
{
  printf '#!/usr/bin/env bash\n'
  printf 'bad_one="%s(grep -c x /etc/hostname | cut -d. -f1)"\n'          "$_S"
  printf 'bad_two="%s(sed -n 1p /etc/hostname | tr -d . )"\n'             "$_S"
  printf 'bad_three+="%s(grep -c x /etc/hostname | cut -d. -f1)"\n'       "$_S"
  printf 'good_one="%s(grep -c x /etc/hostname | cut -d. -f1 || true)"\n' "$_S"
  printf 'good_two="%s(command -v sh 2>/dev/null || printf %%s sh)"\n'    "$_S"
  printf 'good_three+="%s(sed -n 1p /etc/hostname | tr -d . || true)"\n'  "$_S"
  printf '# not_a_site="%s(grep x /etc/hostname | wc -l)"\n'              "$_S"
  printf 'plain_assignment="hello"\n'
} >"$_pf"
_fx_total="$(_pipe_sites "$_pf" | wc -l || true)"
_fx_bad="$(_pipe_unguarded "$_pf" | wc -l || true)"
if [[ "$_fx_total" -eq 6 && "$_fx_bad" -eq 3 ]]; then
  pass "coverage control: the detector finds exactly 6 seeded pipeline assignments including both += appends and flags exactly the 3 unguarded ones, ignoring the commented site and the plain assignment"
else
  meta_failure "coverage control FAILED: the detector found $_fx_total sites and $_fx_bad unguarded in a fixture built to contain exactly 6 and 3. The zero it reports about this script is therefore not evidence."
fi

# --------------------------------------------------------------------------
step "NOTES.txt's \"does not yet do\" list is true of what the chart renders"
# --------------------------------------------------------------------------
# NOTES.txt SHIPS. It is not in .helmignore, and it prints on every `helm
# install` and every `helm upgrade`, which makes it the only file in this chart
# whose blast radius is every operator (gd-em, 12:02). Its "WHAT THIS RELEASE
# DOES NOT YET DO" sentence is a hand-maintained enumeration of things A LATER
# PHASE WILL MAKE FALSE. It is true today only because P1 rewrote it. The moment
# P2 lands the database URL the sentence becomes a lie printed to every
# operator, and without this step nothing in the tree would notice.
#
# A comment asking the next phase to keep the sentence updated is a request, not
# a mechanism. This is the mechanism: every thing the sentence names as absent
# is checked against the RENDERED artifact, so landing one of them turns this
# step red instead of turning the notes into a lie.
#
# THE ANTI-JOIN IS THE HALF THAT KEEPS IT HONEST. The needle table below is
# hand-written, which is precisely the shape that quietly stops being complete,
# so it is joined against THE SENTENCE ITSELF in both directions: a phrase in
# the prose with no needle fails, and a needle with no phrase fails. The table
# cannot drift from the text it is about, because neither side is allowed to
# move alone.
declare -A NOT_YET=(
  # "Cloud SQL and the database URL" WAS HERE, AND ITS REMOVAL IS THIS STEP
  # WORKING RATHER THAN THIS STEP BEING EDITED AROUND. P1 wrote the needle to go
  # red the moment P2 landed the database URL; P2 landed it, the anti-join went
  # red in both directions on the first post-rebase run, and the phrase is gone
  # from the notes because the notes stopped being able to claim it. The render
  # now DOES carry `url:` under settings, which is what the needle was watching
  # for. Nothing here was relaxed: the entry is deleted, not commented out of
  # the join, and EXPECTED_NOT_YET moves with it in this same diff.
  # "the OAuth client secret" WAS HERE, AND ITS REMOVAL IS THIS STEP WORKING
  # RATHER THAN THIS STEP BEING EDITED AROUND. The credentials are now rendered
  # into the settings Secret as server.oauth.web, so the notes stopped claiming
  # them as unlanded - and the needle went red because the render now carries
  # client_secret.
  ["GCS credentials beyond the bucket name"]='settings:credentials|service_account|key_file'
  ["Filestore"]='settings:workspace_storage'
  ["the session secret"]='settings:session_secret|signing_key'
  ["Ingress or IAP"]='kinds:^kind: (Ingress|BackendConfig)'
)
EXPECTED_NOT_YET=4

# The sentence, read out of the shipped template. It carries no template
# actions - checked, it is static prose - so the source text and the rendered
# text are the same bytes here.
_notes_prose="$(tr '\n' ' ' <"$CHART_DIR/templates/NOTES.txt" | tr -s ' ' || true)"
if ! grep -qF 'does not yet configure' <<<"$_notes_prose"; then
  meta_failure "NOTES.txt no longer contains the phrase 'does not yet configure', so the sentence this whole step is about could not be located. Every check below would be reading the entire file as one sentence. Re-anchor the extraction, do not delete this step."
fi
_not_yet_sentence="${_notes_prose#*does not yet configure }"
_not_yet_sentence="${_not_yet_sentence%%.*}"
mapfile -t _prose_items < <(tr ',' '\n' <<<"$_not_yet_sentence" | sed -e 's/^ *//' -e 's/ *$//' -e '/^$/d')

# EXTENT, against an independently-derived expectation and never against zero.
if [[ ${#_prose_items[@]} -eq $EXPECTED_NOT_YET ]]; then
  pass "the notes name $EXPECTED_NOT_YET things this release does not yet configure"
else
  fail "the notes name ${#_prose_items[@]} things this release does not yet configure, EXPECTED_NOT_YET is $EXPECTED_NOT_YET. Adding or removing one is a deliberate act; make it in the same change that adds or removes the needle below (${_prose_items[*]})"
fi

# THE ANTI-JOIN, BOTH DIRECTIONS, WITH THE SYMMETRIC DIFFERENCE PRINTED.
_ny_no_needle=""
for _item in "${_prose_items[@]}"; do
  [[ -v NOT_YET["$_item"] ]] || _ny_no_needle+="${_item@Q} "
done
_ny_no_prose=""
for _key in "${!NOT_YET[@]}"; do
  _found=no
  for _item in "${_prose_items[@]}"; do [[ $_item == "$_key" ]] && _found=yes && break; done
  [[ $_found == yes ]] || _ny_no_prose+="${_key@Q} "
done
if [[ -z $_ny_no_needle ]]; then
  pass "every phrase in the notes' list has a needle that checks it against the render"
else
  fail "these phrases are in NOTES.txt but nothing checks them against the render, so the notes could claim them falsely: $_ny_no_needle"
fi
if [[ -z $_ny_no_prose ]]; then
  pass "every needle corresponds to a phrase the notes actually print"
else
  fail "these needles have no phrase in NOTES.txt, so the table is testing a claim the notes no longer make: $_ny_no_prose"
fi

# THE TWO CORPORA, built once from every permutation. || true on settings_block
# because existing-secret legitimately has no settings block; the coverage
# control below is what stops that from turning into a vacuous pass.
_ny_settings=""
_ny_kinds=""
for name in "${PERMUTATIONS[@]}"; do
  _ny_settings+="$(settings_block "$WORK/$name.yaml" || true)"$'\n'
  _ny_kinds+="$(grep -E '^kind:' "$WORK/$name.yaml" || true)"$'\n'
done

# COVERAGE CONTROL. Six absence assertions follow, and six absence assertions
# are satisfied perfectly by two empty strings. This seeds nothing - it asserts
# that each corpus already contains a known instance, so the greps below are
# being run against something.
# hub_id is nested under server.hub, and settings_block de-indents by exactly
# four, so it lands at column four and not at column zero. Anchoring this at ^
# was my first attempt and this control failed on its own instance - which is
# the control working: it reported "empty or wrong" while printing 130 settings
# lines, and the two halves of that message are what identified the anchor as
# the wrong half.
if grep -qE '^ *hub_id:' <<<"$_ny_settings" && grep -qE '^kind: Deployment' <<<"$_ny_kinds"; then
  pass "both corpora are real: $(grep -c . <<<"$_ny_settings") settings lines containing hub_id, $(grep -c . <<<"$_ny_kinds") kind lines containing Deployment"
else
  fail "the corpora this step reads are empty or wrong - settings lines $(grep -c . <<<"$_ny_settings"), kind lines $(grep -c . <<<"$_ny_kinds") - so the six absence checks below would pass against nothing"
fi

for _item in "${_prose_items[@]}"; do
  _spec="${NOT_YET[$_item]:-}"
  [[ -z $_spec ]] && continue   # already reported by the anti-join above
  _corpus="${_spec%%:*}"; _needle="${_spec#*:}"
  case "$_corpus" in
    settings) _hay="$_ny_settings" ;;
    kinds)    _hay="$_ny_kinds" ;;
    *)        meta_failure "unknown corpus ${_corpus@Q} in the NOT_YET table" ;;
  esac
  if grep -qE "$_needle" <<<"$_hay"; then
    fail "NOTES.txt tells every operator this release does not yet configure ${_item@Q}, and the render contains it (${_needle@Q} matched in the $_corpus corpus). The chart grew a feature and the notes still deny it - fix the notes in the change that added the feature."
  else
    pass "the notes say ${_item@Q} is not configured yet, and it is absent from the $_corpus the chart renders"
  fi
done

# --------------------------------------------------------------------------
step "nothing points the hub at a second configuration file"
# --------------------------------------------------------------------------
# Everything else in this file asserts what settings.yaml CONTAINS. Nothing so
# far asserts that the hub will read it.
#
# WHAT --config / -c ACTUALLY DOES, WHICH IS NOT WHAT THIS COMMENT USED TO SAY.
# It does not redirect the configuration load unconditionally. LoadGlobalConfig
# (pkg/config/hub_config.go:628) calls loadGlobalConfigFromSettings (:640), which
# reads GetGlobalDir() FIRST and UNCONDITIONALLY and consults configPath only
# `if !found` (:647-660). So the flag's effect depends on whether the global
# read succeeded, and `found` is narrower than "the file exists": it is true
# exactly when $HOME/.scion/settings.yaml parses AND carries a non-nil top-level
# `server` key (loadServerFromSettingsFile, :1331, decided at :1344-1347).
#
# THE FLAG IS INERT BECAUSE OF A PROPERTY OF THIS CHART'S OUTPUT, NOT BECAUSE OF
# ANYTHING IN THE BINARY, AND IT WAS LIVE AT PHASE 0. What was missing there was
# the key, not the file: the hub's own embedded defaults seed a settings.yaml
# with no server section, so `found` was false and configPath was read. Every
# settings-bearing permutation here renders a top-level `server:` key, so `found`
# is true and --config does nothing at all - a property this phase supplies and
# no other phase can.
#
# NOTHING AT ALL, LITERALLY - NOT EVEN A WARNING, and it would be easy to write
# "only warns" here and be wrong in the direction that matters. The flag is not
# marked deprecated: MarkDeprecated appears twice in cmd/server.go, :236 and
# :290, both for --production, never for config or c (the flag itself is
# cmd/server.go:237). The only two warnings in the load path,
# pkg/config/hub_config.go:668 and :678, are about a server.yaml sitting beside
# settings.yaml, and the one that depends on the --config path at all additionally
# requires hasServerYAML(dir) (:1393) - a server.yaml or server.yml next to the
# --config target, which this chart creates nowhere. So an operator who appends
# --config gets no error, no warning and no log line. There is no runtime signal
# to fall back on, which is precisely why a reserved flag is the only available
# guard and why this check is worth its lines.
#
# Render a settings.yaml without the top-level key - a minimisation, a refactor
# of the document's top-level shape, a phase that moves everything under a
# profile - and `found` goes false, the deployment is back in the Phase 0 state,
# and the flag takes effect. IN TWO FORMS, AND
# NEITHER OF THEM IS A REDIRECT OF THE WHOLE LOAD. At :648-659 the --config
# path's own directory is searched for a settings.yaml and, if it has a server
# key, that file becomes the SOLE source of the server config. Failing that,
# LoadGlobalConfig falls through to loadGlobalConfigLegacy(configPath) (:635,
# :699), which loads defaults, then ~/.scion/server.yaml (:772-775), then LAYERS
# the --config file over the result (:777-787) - an overlay. Nothing can move the
# directory the hub reads first: GetGlobalDir (pkg/config/paths.go:188-194) is
# os.UserHomeDir() joined with GlobalDir and takes no arguments. Keep the
# distinction if you narrow this check - an overlay and a redirect have different
# blast radii, and a mitigation scoped to redirection does not cover an overlay.
# The top-level `server:` key is asserted separately, above, for that reason.
#
# Either form, when live, is silent: the mount still exists, the mode is still
# 0444, schema_version is still rendered, every check above still passes, and the
# hub's configuration is coming from somewhere the chart never wrote. That is why
# the check stays even though the flag is currently inert - the condition that
# makes it inert is ours and can be changed by someone who does not know they
# hold it.
#
# This is deliberately NOT the same check as the chart's reserved-flag guard on
# hub.args. That one rejects operator input. This one inspects rendered output,
# and the two fail differently - a guard on the input can be entirely correct
# and still be bypassed by a later phase of this chart rendering the flag
# itself. This check is the one that notices that.
#
# THE DETECTOR IS TESTED AGAINST A FIXTURE, NOT AGAINST THE CHART, AND THAT IS
# DELIBERATE. The obvious way to prove a negative check works is to make the
# thing happen on purpose - append --config through hub.args and watch the check
# fire. That worked while the flag was still appendable, and it stops working the
# moment Phase 0's reserved list claims it: the render then fails at input
# validation, the check never runs, and the test goes green for a reason
# unrelated to what it tests. A test that changes meaning when an unrelated guard
# lands is not a test of this check. So the pattern is proved against fabricated
# args blocks below, which no guard can reach.
#
# THE SHORTHAND TAKES AN ATTACHED VALUE. pflag accepts -cVALUE with no separator
# at all, so -c/etc/x.yaml is a complete flag-and-value in one argv element and
# the earlier pattern - which required = or end-of-element after -c - did not
# match it. That was the one spelling with no fixture, which is the combination
# that reads as coverage.
#
# The `[^-]` branch has no false positives available to it, by construction
# rather than by luck: pflag reads any single-dash element as a cluster of
# shorthands, so -cfg IS -c with the value fg and matching it is correct, not a
# near miss. A double-dash long flag beginning with those letters cannot reach
# that branch at all, because the second character is -, which is what keeps
# --concurrency and --configure-something out.
config_flag_re='^\s*-\s*"?(--config(=|"?$)|-c(=|"?$|[^-]))'

for fixture in '            - "--config"' '            - --config' '            - "--config=/etc/x.yaml"' '            - "-c"' '            - -c' '            - "-c=/etc/x.yaml"' '            - "-c/etc/x.yaml"' '            - -cetc/x.yaml'; do
  _label="${fixture#*- }"
  if grep -Eq "$config_flag_re" <<<"$fixture"; then
    pass "the config-flag pattern matches $_label"
  else
    fail "the config-flag pattern does NOT match $_label - the checks below cannot detect what they exist to detect"
  fi
done
for fixture in '            - "--hosted"' '            - "--enable-web"' '            - "--configure-something"' '            - "--concurrency"' '            - "--config-dir-hint"'; do
  _label="${fixture#*- }"
  if grep -Eq "$config_flag_re" <<<"$fixture"; then
    fail "the config-flag pattern matches $_label, which is not a config-path flag - the checks below would reject legitimate arguments"
  else
    pass "the config-flag pattern does not match $_label"
  fi
done

for name in "${PERMUTATIONS[@]}"; do
  args_block="$(sed -n '/^          args:$/,/^          [a-z]/p' "$WORK/$name.yaml")"
  if [[ -z "$args_block" ]]; then
    fail "$name: could not find the hub's args in the rendered Deployment - this check would be vacuous"
    continue
  fi
  if grep -Eq "$config_flag_re" <<<"$args_block"; then
    fail "$name renders a config-path flag in the hub's arguments. It points the hub at a second configuration source the chart does not write - inert only while the rendered settings.yaml keeps its top-level server: key - and every other check in this file still passes when it does."
    grep -E '^\s*-\s*"?(--config|-c)' <<<"$args_block" || true
  else
    pass "$name renders no config-path flag"
  fi
done
# The twin: the args block was found and does contain the arguments we expect,
# so "no config flag" is not passing because the extraction returned nothing.
if grep -Eq '^\s*-\s*"?--hosted"?$' <<<"$(sed -n '/^          args:$/,/^          [a-z]/p' "$WORK/settings.yaml")"; then
  pass "the args block really is the hub's arguments"
else
  fail "the args extraction did not find --hosted - the config-flag checks above prove nothing"
fi

# --------------------------------------------------------------------------
step "renders that must fail"
# --------------------------------------------------------------------------
expect_render_failure \
  "hub.extraEnv rejects SCION_SERVER_DATABASE_*" \
  "hub.extraEnv may not set SCION_SERVER_DATABASE_URL" \
  "${BASE[@]}" \
  --set 'hub.extraEnv[0].name=SCION_SERVER_DATABASE_URL' \
  --set 'hub.extraEnv[0].value=postgres://x'

expect_render_failure \
  "hub.extraEnv rejects SCION_SERVER_OIDC_*" \
  "hub.extraEnv may not set SCION_SERVER_OIDC_ISSUER_URL" \
  "${BASE[@]}" \
  --set 'hub.extraEnv[0].name=SCION_SERVER_OIDC_ISSUER_URL' \
  --set 'hub.extraEnv[0].value=https://accounts.example.com'

expect_render_failure \
  "hub.extraEnv rejects shadowing a variable the chart sets" \
  "hub.extraEnv may not set HOME" \
  "${BASE[@]}" \
  --set 'hub.extraEnv[0].name=HOME' \
  --set 'hub.extraEnv[0].value=/tmp'

# The name axis, through scion-hub.assertNoCredentialName. The message is the
# shared helper's, so matching on it also proves the call reaches the helper
# rather than some local copy of the rule.
expect_render_failure \
  "hub.extraEnv rejects a literal under a name ending in a credential noun" \
  "names credential material" \
  "${BASE[@]}" \
  --set 'hub.extraEnv[0].name=SOME_API_TOKEN' \
  --set 'hub.extraEnv[0].value=abc123'

expect_render_failure \
  "hub.extraEnv rejects a literal under a name that is a credential noun" \
  "names credential material" \
  "${BASE[@]}" \
  --set 'hub.extraEnv[0].name=PASSWORD' \
  --set 'hub.extraEnv[0].value=abc123'

# The value axis, through scion-hub.assertNoCredential. The name here is
# deliberately innocuous - HUB_UPSTREAM - so this can only pass on the value.
expect_render_failure \
  "hub.extraEnv rejects a URL with credentials in the userinfo" \
  "embeds credentials in a URL" \
  "${BASE[@]}" \
  --set 'hub.extraEnv[0].name=HUB_UPSTREAM' \
  --set 'hub.extraEnv[0].value=postgres://scion:hunter2@10.0.0.1/scion'

# A multi-line PEM in an environment value, through a values file because --set
# cannot carry newlines. This case is nearly unreachable on the argv path - an
# argument containing spaces is caught by the whitespace guard first, so a PEM
# test there credits the value guard for a catch the whitespace guard made - but
# here it is the main case rather than a corner: environment values may legally
# contain whitespace and there is no whitespace guard in front of this one, so
# the "-----BEGIN " alternative is doing the work and nothing else is.
#
# The name is deliberately ordinary. TLS_MATERIAL ends in no credential noun, so
# the name axis has nothing to say and this can only be the value axis.
#
# DO NOT DELETE THIS PAIR WITHOUT REPLACING IT SOMEWHERE. These two cases are the
# ONLY reachable coverage, anywhere in this chart, of the "-----BEGIN "
# alternative in scion-hub.assertNoCredential. That helper is shared with the
# argv path, and argv cannot cover it: an argument containing spaces is rejected
# by the whitespace guard first, so a PEM case there passes while testing a
# different guard. Phase 0 knows this and deliberately does not duplicate it.
#
# So if these are pruned as slow or redundant, an alternative in somebody else's
# helper goes untested chart-wide and nothing in their files will fail. Whoever
# prunes them owns moving the coverage, not just removing the runtime.
cat >"$WORK/pem-env.yaml" <<'PEMVALUES'
image:
  repository: example.test/scion-hub-gke
hub:
  hubId: neg
  baseUrl: https://neg.example.com
  extraEnv:
    - name: TLS_MATERIAL
      value: |
        -----BEGIN PRIVATE KEY-----
        MIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQDexample
        -----END PRIVATE KEY-----
PEMVALUES
expect_render_failure \
  "hub.extraEnv rejects a multi-line PEM in an environment value" \
  "has the shape of a credential" \
  --values "$WORK/pem-env.yaml"

expect_render_failure \
  "config.existingSecret with config.extra" \
  "config.existingSecret is set together with inline settings values" \
  "${BASE[@]}" \
  --set config.existingSecret=mine \
  --set config.extra.server.log_level=debug

expect_render_failure \
  "config.existingSecret with storage.bucket" \
  "config.existingSecret is set together with inline settings values" \
  "${BASE[@]}" \
  --set config.existingSecret=mine \
  --set storage.bucket=some-bucket

expect_render_failure \
  "config.extra cannot leave hosted mode" \
  "must set server.mode: hosted" \
  "${BASE[@]}" \
  --set config.extra.server.mode=workstation

expect_render_failure \
  "config.extra cannot substitute the hub ID" \
  "which is not the value supplied in hub.hubId" \
  "${BASE[@]}" \
  --set config.extra.server.hub.hub_id=somethingelse

# The near miss, which is the likelier mistake than the wrong value: a key
# written with nothing after it. YAML parses that as null, not as absent, so
# dig's default never applies and the value that reaches the diagnostic is nil.
# Each of these is a case where the message used to read %!q(<nil>).
#
# The pair matters in both directions. The null cases prove the value is
# rendered as null rather than as a format error; the wrong-value case beside
# them proves a real value is still quoted, so a diagValue that had degenerated
# into printing "null" for everything - which would satisfy every check above,
# since %! would be gone - fails here.
expect_render_failure \
  "a null schema_version is reported as null, not as a format error" \
  'as a string, got null.' \
  "${BASE[@]}" \
  --set config.extra.schema_version=null

expect_render_failure \
  "a null server.mode is reported as null, not as a format error" \
  'server.mode: hosted, got null.' \
  "${BASE[@]}" \
  --set config.extra.server.mode=null

expect_render_failure \
  "a null hub_id is reported as null on one side and quoted on the other" \
  'server.hub.hub_id: null, which is not the value supplied in hub.hubId ("neg").' \
  "${BASE[@]}" \
  --set config.extra.server.hub.hub_id=null

expect_render_failure \
  "a non-null wrong value is still quoted, not reported as null" \
  'as a string, got "2".' \
  "${BASE[@]}" \
  --set-string config.extra.schema_version=2

# The base URL cannot be split in two. This is NOT a hypothetical guard against a
# future template line: config.extra is deep-merged over the settings tree before
# the assertions run, so the --set below rendered a working
# server.hub.public_url before the assertion existed, in the same manifest as a
# SCION_SERVER_BASE_URL pointing somewhere else. Verified by rendering it.
#
# Both halves are here, and the positive one is load-bearing: the rule is on the
# key, not on the namespace, so config.extra must still be able to reach an
# unmodelled key under server.hub. Without that twin this pair would also pass on
# a guard that had started refusing every config.extra write to server.hub, and
# the breakage would land on the one thing config.extra exists to allow.
expect_render_failure \
  "config.extra cannot split the base URL with server.hub.public_url" \
  "it splits it" \
  "${BASE[@]}" \
  --set config.extra.server.hub.public_url=https://elsewhere.example.com

# config.extra may add. It may not overwrite what the chart wrote.
#
# max_open_conns is chosen deliberately: it has no assertion of its own, so this
# can only be the collision rule. A key that IS separately asserted - hub_id,
# the driver, server.mode - would pass this test on the specific assertion and
# report nothing about whether the collision rule works at all.
#
# The nil case is here because mergeOverwrite's nil semantics are the usual way
# a merge guard leaks: an operator writing `hub_name: ~` is attempting deletion
# rather than substitution, and a rule keyed on the VALUE would let it through.
# This one is keyed on the key.
expect_render_failure \
  "config.extra cannot overwrite a key the chart writes" \
  "config.extra overwrites server.database.max_open_conns" \
  "${BASE[@]}" \
  --set config.extra.server.database.max_open_conns=50

cat >"$WORK/extra-nil.yaml" <<'NILVALUES'
image:
  repository: example.test/scion-hub-gke
hub:
  hubId: neg
  baseUrl: https://neg.example.com
config:
  extra:
    server:
      hub:
        hub_name: ~
NILVALUES
expect_render_failure \
  "config.extra cannot null out a key the chart writes" \
  "config.extra overwrites server.hub.hub_name" \
  --values "$WORK/extra-nil.yaml"

# The collision rule must not shadow the specific assertions. A generic "you
# overwrote a key" in place of "that is not the value supplied in hub.hubId"
# would be a regression in every case that has a message of its own, and the
# ordering that prevents it is invisible from the output - which is why it is
# asserted rather than assumed.
expect_render_failure \
  "a key with its own assertion still reports its own message" \
  "which is not the value supplied in hub.hubId" \
  "${BASE[@]}" \
  --set config.extra.server.hub.hub_id=somethingelse

# The top-level server: key, nulled. Not a wilful-input test: this is the one
# shape that makes the hub's global settings read return not-found, which is what
# takes --config from inert to live. `server: ~` passes
# hasKey and fails the binary's raw["server"] != nil, so the presence check alone
# does not cover it and the assertion tests the same condition the binary does.
cat >"$WORK/null-server.yaml" <<'NULLSERVER'
image:
  repository: example.test/scion-hub-gke
hub:
  hubId: neg
  baseUrl: https://neg.example.com
config:
  extra:
    server: ~
NULLSERVER
expect_render_failure \
  "config.extra cannot null out the whole server section" \
  "top-level server: key that is not a map" \
  --values "$WORK/null-server.yaml"

expect_render_failure \
  "config.extra cannot contradict the database driver" \
  "Overriding the driver through config.extra" \
  "${BASE[@]}" \
  --set config.extra.server.database.driver=postgres

expect_render_failure \
  "config.extra cannot drop schema_version" \
  'must carry schema_version: "1"' \
  "${BASE[@]}" \
  --set config.extra.schema_version=""

expect_render_failure \
  "config.extra cannot put a server key at the top level" \
  "It belongs under server:" \
  "${BASE[@]}" \
  --set config.extra.scheduler.interval_seconds=30

expect_render_failure \
  "the SCHEMA rejects postgres without a GCS bucket" \
  "bucket" \
  "${BASE[@]}" \
  --set database.driver=postgres \
  --set storage.provider=gcs

expect_render_failure \
  "the SCHEMA rejects a plaintext base URL" \
  "baseUrl" \
  --set image.repository=example.test/scion-hub-gke \
  --set hub.hubId=neg \
  --set hub.baseUrl=http://neg.example.com

expect_render_failure \
  "the SCHEMA rejects oauth mode without credentials" \
  "clientId" \
  "${BASE[@]}" \
  --set auth.mode=oauth

# THE THREE ABOVE TEST THE SCHEMA, AND THEY ARE NAMED FOR IT NOW BECAUSE THEY
# WERE NOT. Helm validates values.schema.json BEFORE it renders, so an input the
# schema rejects never reaches the template - and the schema's own message
# happens to contain the same substring the check was matching. Two of the three
# have a template guard behind them that had, measurably, zero coverage: the
# schema message satisfies the check with the template guard deleted.
#
# So each is now a PAIR. The schema check above keeps the schema honest; the
# template check below runs the same input with --skip-schema-validation and
# matches a substring that appears ONLY in the template's message, never in the
# schema's. Two layers, two tests, and neither can pass for the other's reason.
#
# --skip-schema-validation is not exotic here: it is one flag away for anyone,
# it is what a values file assembled by another tool can effectively produce, and
# per the reviewer's F1 it removes EVERY schema-enforced rule at once. The
# template guards are what is left, so they are worth testing on their own.
# THE PROXY KEYS KEEP THIS CASE AIMED AT THE GUARD IT NAMES. Since the Cloud SQL
# phase, postgres without cloudsql.enabled is refused by a DIFFERENT template
# guard that fires first, and this case then reported "failed, but not for the
# expected reason" - correctly. Supplying the proxy is what isolates the bucket
# guard again; it is not padding.
expect_render_failure \
  "the TEMPLATE rejects postgres without a GCS bucket" \
  "storage.bucket is required when storage.provider is gcs" \
  --skip-schema-validation \
  "${BASE[@]}" \
  --set database.driver=postgres \
  --set database.auth=iam --set database.name=scion --set database.user=u \
  --set cloudsql.enabled=true --set cloudsql.instanceConnectionName=my-project:us-central1:db-1 \
  --set storage.provider=gcs

expect_render_failure \
  "the TEMPLATE rejects a plaintext base URL" \
  "The session cookie's Secure attribute is derived from this prefix" \
  --skip-schema-validation \
  --set image.repository=example.test/scion-hub-gke \
  --set hub.hubId=neg \
  --set hub.baseUrl=http://neg.example.com \
  --set auth.sessionSecret=neg-session-secret

expect_render_failure \
  "the TEMPLATE rejects oauth mode without a web client credential" \
  "no complete OAuth web client credential is present" \
  --skip-schema-validation \
  "${BASE[@]}" \
  --set auth.mode=oauth

# The agent-namespace guard, in the one permutation where every OTHER caller of
# it is switched off. runtime.listAllNamespaces skips the Role and RoleBinding;
# config.existingSecret skips the settings template. Set both and the only
# caller left was NOTES.txt, which is not a manifest - so the guard was reachable
# only through a file that some clients never render. It is now called from the
# Deployment, which always renders.
#
# Both flags are load-bearing in this fixture. Drop either and the guard fires
# from its old caller and the test passes without testing anything.
#
# NOT expect_render_failure, AND THE REASON IS THE FINDING ITSELF. NOTES.txt
# calls the same helper unconditionally, and helm renders NOTES.txt even though
# it prints no manifest for it - so a plain "this render fails with this message"
# check is green with the Deployment's call deleted. Measured: deleting the call
# leaves the whole suite passing. What discriminates is WHERE the failure comes
# from, so that is what is asserted. Helm names the template and the position in
# the error, and only the template name is matched; the line number moves
# whenever anything above it moves.
#
# If the call is relocated to another always-rendered manifest, this goes red on
# the filename and the fix is to name that file here. That is the correct amount
# of friction for moving an unconditional guard.
ns_label="the agent-namespace guard fires from a manifest with the RBAC pair and the settings file both switched off"
if ns_out=$("$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    "${BASE[@]}" \
    --set config.existingSecret=mine \
    --set runtime.listAllNamespaces=true \
    --set rbac.agentNamespace=aaa \
    --set runtime.namespace=bbb 2>&1); then
  fail "$ns_label: the render SUCCEEDED and was supposed to fail"
elif ! grep -qF 'rbac.agentNamespace (aaa) and runtime.namespace (bbb) disagree' <<<"$ns_out"; then
  fail "$ns_label: failed, but not for the expected reason"
  printf '          got: %s\n' "$(tr '\n' ' ' <<<"$ns_out" | cut -c1-300)"
elif ! grep -qF 'templates/deployment.yaml' <<<"$ns_out"; then
  fail "$ns_label: the guard fired, but from $(sed -n 's/.*execution error at (\([^:]*\).*/\1/p' <<<"$ns_out" | head -1) rather than from the Deployment. NOTES.txt is not a manifest; a guard reachable only through it is a guard some clients never run."
else
  pass "$ns_label"
fi

# hub.podLabels may not collide with a selector label. A Deployment's selector
# is immutable after creation, so a colliding label is not an override - it is a
# Deployment that either refuses to update or adopts no pods, discovered on the
# upgrade rather than on the install.
#
# Both selector labels, separately. One fixture would pass on a guard that had
# been narrowed to whichever key it happens to name, and the guard derives its
# key set from scion-hub.selectorLabels rather than from a literal list, so a
# change to that helper is exactly what these are here to notice.
expect_render_failure \
  "hub.podLabels rejects the name selector label" \
  "hub.podLabels may not set app.kubernetes.io/name" \
  "${BASE[@]}" \
  --set 'hub.podLabels.app\.kubernetes\.io/name=mine'

expect_render_failure \
  "hub.podLabels rejects the instance selector label" \
  "hub.podLabels may not set app.kubernetes.io/instance" \
  "${BASE[@]}" \
  --set 'hub.podLabels.app\.kubernetes\.io/instance=mine'

# --------------------------------------------------------------------------
step "renders that must succeed"
# --------------------------------------------------------------------------
# The positive twins for the guards above. A guard that rejects everything is
# not a guard, and the secret-name check in particular is one bad regex away
# from blocking the correct way to pass a secret.
if out=$("$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    "${BASE[@]}" \
    --set 'hub.extraEnv[0].name=SOME_API_TOKEN' \
    --set 'hub.extraEnv[0].valueFrom.secretKeyRef.name=my-secret' \
    --set 'hub.extraEnv[0].valueFrom.secretKeyRef.key=token' 2>&1); then
  if grep -qF 'secretKeyRef' <<<"$out"; then
    pass "hub.extraEnv accepts a secret-named variable delivered by secretKeyRef"
  else
    fail "the secretKeyRef entry rendered but did not reach the manifest"
  fi
else
  fail "hub.extraEnv rejected a secret-named variable delivered by secretKeyRef, which is the correct shape"
  printf '          %s\n' "$(tr '\n' ' ' <<<"$out" | cut -c1-300)"
fi

if "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    "${BASE[@]}" \
    --set 'hub.extraEnv[0].name=SCION_SERVER_ADMIN_MODE_NOTE' \
    --set 'hub.extraEnv[0].value=harmless' >/dev/null 2>&1; then
  pass "hub.extraEnv accepts an ordinary variable"
else
  fail "hub.extraEnv rejected an ordinary variable - the prefix guard is too broad"
fi

# The name-axis false positives, by name, because these are the ones a naive
# substring check gets wrong and they are not hypothetical: a TTL and a limit
# both carry a credential noun that is not saying the value is a credential.
# Each is set with a LITERAL value, because that is the only case the check
# looks at - a secretKeyRef entry would pass without the rule being exercised.
for ok_name in TOKEN_TTL_SECONDS MAX_TOKENS SECRET_MANAGER_PROJECT KEYCLOAK_REALM PASSWORD_MIN_LENGTH; do
  if "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
      "${BASE[@]}" \
      --set "hub.extraEnv[0].name=$ok_name" \
      --set 'hub.extraEnv[0].value=60' >/dev/null 2>&1; then
    pass "hub.extraEnv accepts $ok_name"
  else
    fail "hub.extraEnv rejected $ok_name - the name guard is substring-matching a credential noun that is describing what the value is ABOUT, not what it IS"
  fi
done

# The twin for the two podLabels negatives. An ordinary label must survive the
# guard AND land in the pod template - and not in the selector, which is the
# half a "does it render" check misses. The varied permutation sets team:
# platform, so the golden covers the rendering; this asserts the position,
# because the golden would be equally green with the label in matchLabels, which
# is the immutable-selector bug arriving by the other route.
pod_tpl_labels="$(sed -n '/^  template:/,/^      annotations:/p' "$WORK/varied.yaml")"
selector_block="$(sed -n '/^  selector:/,/^  template:/p' "$WORK/varied.yaml")"
if grep -qF 'team: platform' <<<"$pod_tpl_labels"; then
  pass "hub.podLabels reaches the pod template labels"
else
  fail "hub.podLabels did not reach the pod template - the two negatives above are the only coverage the guard has, and a guard that rejects everything passes both"
fi
if grep -qF 'team: platform' <<<"$selector_block"; then
  fail "hub.podLabels reached the Deployment's selector, which is immutable after creation - the label belongs in the pod template only"
else
  pass "hub.podLabels does not reach the immutable selector"
fi
# ...and the extraction found the blocks it claims to have searched.
if grep -qF 'app.kubernetes.io/name: scion-hub' <<<"$pod_tpl_labels" \
  && grep -qF 'app.kubernetes.io/name: scion-hub' <<<"$selector_block"; then
  pass "the pod-template and selector label blocks were both found"
else
  fail "one of the two label blocks came back empty - the two checks above prove nothing"
fi

# The twin for the public_url refusal. server.hub is not a closed namespace - the
# refusal is on one key - so an unmodelled key under it must still render.
if out=$("$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    "${BASE[@]}" \
    --set config.extra.server.hub.hub_description=example 2>&1); then
  if grep -qF 'hub_description: example' <<<"$out"; then
    pass "config.extra can still reach an unmodelled key under server.hub"
  else
    fail "config.extra.server.hub.hub_description rendered without error but did not reach the settings file"
  fi
else
  fail "config.extra was refused an unmodelled server.hub key - the public_url rule is matching the namespace instead of the key, which breaks the escape hatch config.extra exists to be"
  printf '          %s\n' "$(tr '\n' ' ' <<<"$out" | cut -c1-300)"
fi

# The oauth acknowledgement, in the one shape where it must NOT fire. Under
# config.existingSecret the chart renders no settings file, so auth.mode selects
# nothing and there is nothing to acknowledge; demanding the acknowledgement
# there is a refusal with no harm behind it. Asserted at BOTH layers because they
# are two independent copies of the rule and an exclusion added to one of them is
# not an exclusion: the first render exercises the schema, the second exercises
# the template with the schema removed.
for guard_layer in schema template; do
  layer_args=()
  [[ $guard_layer == template ]] && layer_args=(--skip-schema-validation)
  if out=$("$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
      "${layer_args[@]}" \
      "${BASE[@]}" \
      --set config.existingSecret=mine \
      --set auth.mode=oauth 2>&1); then
    pass "the $guard_layer permits unacknowledged oauth under config.existingSecret"
  else
    fail "the $guard_layer refused unacknowledged oauth under config.existingSecret, where the chart renders no auth mode at all"
    printf '          %s\n' "$(tr '\n' ' ' <<<"$out" | cut -c1-300)"
  fi
done

# The twin for the PEM case above: a multi-line value that is not credential
# material is still accepted. Without this, "rejects a multi-line PEM" would also
# pass on a guard that had simply started rejecting every multi-line value, and
# the failure would land on a legitimate one - a banner or a certificate chain -
# with no override available.
cat >"$WORK/multiline-env.yaml" <<'MLVALUES'
image:
  repository: example.test/scion-hub-gke
hub:
  hubId: neg
  baseUrl: https://neg.example.com
  extraEnv:
    - name: HUB_BANNER
      value: |
        Scheduled maintenance on Sunday.
        Sessions will be interrupted.
auth:
  sessionSecret: neg-session-secret
MLVALUES
if "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    --values "$WORK/multiline-env.yaml" >/dev/null 2>&1; then
  pass "hub.extraEnv accepts an ordinary multi-line value"
else
  fail "hub.extraEnv rejected an ordinary multi-line value - the PEM check is matching on whitespace rather than on the PEM header"
fi

# --------------------------------------------------------------------------
step "the settings checksum is a redacted projection: the oracle is dead and the rotation cost is measured"
# --------------------------------------------------------------------------
# WHAT THIS STEP IS ABOUT, because the assertions below assert an EQUALITY and
# an equality is the shape a broken harness produces for free.
#
# settings.yaml is a subPath mount and the kubelet never refreshes one, so the
# chart rolls the pods by digesting the settings document into the pod
# annotation checksum/settings. Phase 2 puts a password inside
# server.database.url in that document. A pod annotation is readable by anyone
# with get on deployments, which is a WIDER audience than the settings Secret's
# own RBAC, and every other component of a DSN - scheme, user, host, port,
# database name - is chart-rendered or public. A digest over that document is
# therefore a digest over a preimage with one unknown in it: an offline oracle
# for the password, checkable by a reader who is not allowed to read the Secret.
#
# scion-hub.settingsChecksum digests a projection with the credential removed.
# The helper is gd-p3-dev's, adopted byte-identical from scion/gke-chart-p3 blob
# 06b2a4c7cf3d73bb57d1c56370e4b21f3ca12182; the server.database.url branch in it
# was written for this phase and had never been executed by any input on theirs.
# This step is what makes it executed code.
#
# THE PRICE, AND IT IS ASSERTED HERE RATHER THAN DESCRIBED: with the credential
# out of the digest input, a password-only change no longer moves the annotation
# and no pod rolls. That is arm 1. Arm 4 is what stops arm 1 from being
# satisfied by a digest that has stopped moving for ANY reason.
_ck_A='RotPlainAlphaAAAA111'
_ck_B='RotPlainBetaBBBB2222'
# The rotated USERNAME for arm U. Not a credential and deliberately not
# treated as one: it is the field gd-p2-rev's C1 showed was invisible to the
# digest, and the fix makes it visible.
_ck_U='rotated-user-uuuu'
# @ : / and # are four of the nine characters scion-hub.pctEncodeUserinfo
# encodes, so these two passwords do NOT appear in the rendered file as typed.
# gd-secann-2 measured that a projection which redacts by MATCHING THE
# CREDENTIAL'S VALUE is silent for exactly these passwords, and worse than
# silent - it emits a document that looks redacted and still carries the secret.
# The adopted helper redacts by URL STRUCTURE, so these arms must collapse too.
# They are here because that is a case the phase 3 suite structurally cannot
# reach: nothing percent-encodes an OAuth client secret.
_ck_S1='Rot@Spec:Alpha/AAA#1'
_ck_S2='Rot@Spec:Beta/BBBB#2'

_ck_render() { # _ck_render <name> <extra helm args...>
  local n="$1"; shift
  local rc=0
  "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    --values "$CHART_DIR/ci/values-cloudsql.yaml" \
    --set-string database.auth=password \
    --set-string database.user=probe-user \
    "$@" >"$WORK/ck-$n.yaml" 2>"$WORK/ck-$n.err" || rc=$?
  printf '%s' "$rc" >"$WORK/ck-$n.rc"
}
# awk rather than grep throughout this block: this file runs under `set -e`, and
# grep exits 1 on "found nothing", which is a legitimate reading here and not an
# error. awk returns the count and exits 0, so a zero cannot abort the script
# before the assertion that was going to report it.
_ck_count() { awk '$1=="checksum/settings:"{c++} END{print c+0}' "$1"; }
_ck_digest() { awk '$1=="checksum/settings:"{print $2}' "$1"; }
_ck_url() { awk '$1=="url:"{print $2}' "$1"; }

# 🔴 THE EXTRACTOR ITSELF IS A SUSPECT, AND EVERY GUARD ON IT USED TO BE AN
# AGREEMENT CHECK. An extractor that returns nothing agrees with itself
# perfectly: _dA == _dA2 holds, _dA == _dB holds, and the three assertions that
# constitute this step's security claim all report ok on a corpse. gd-p1-dev
# named the class (chart-integrity.sh:377) - a matcher whose expected result in
# the current phase COINCIDES with what a dead matcher produces - and the
# discriminator is a control asserting SURVIVAL, never one asserting
# disappearance.
#
# MEASURED ON THIS FILE BEFORE THIS EXISTED, not reasoned about:
#   _ck_digest keyed to a field that does not exist   -> 3 assertions PASS,
#     including "the annotation is not an oracle for it ()", and the run then
#     died at an unrelated -n guard sixty lines later, reporting the CHART as
#     broken ("the digest has become a constant") when the instrument was.
#   _ck_digest killed for the mut-B arm alone         -> rc=0, 297/297,
#     0 failures. A CLEAN PASS with the un-projected control comparing a real
#     digest against an empty string.
#
# So the guard runs THE FUNCTION UNDER SUSPICION over a DERIVED file set and
# demands a well-formed sha256 back. A second awk of my own would not test the
# first one; asserting the shape of what the caller actually consumes does.
# The file set is a glob rather than a list of the six variables the block
# happens to read, because a list goes stale the moment an arm is added and a
# glob cannot - and the expected number is ABSOLUTE, so a file set that grew or
# shrank is a meta-failure naming the number rather than a silently wider sweep.
#
# 🔴 AND IT MUST NOT ABORT, WHICH IS A DIFFERENT FAILURE FROM THE ONE ABOVE AND
# WORSE. This file is line 46 `set -euo pipefail`, so an assignment whose command
# substitution exits non-zero KILLS THE SCRIPT ON THAT LINE - no diagnostic, no
# meta_failure, and the assertion counter never reaches its pin. gd-p1-dev found
# they had cleared a site on the reasoning "empty cannot match, so it fails
# safe", when under `set -e` control never reaches the comparison at all and the
# ${x:-<absent>} they cited was unreachable code.
#
# MEASURED HERE THE WAY THEY PRESCRIBED - delete the input and run it, rather
# than read the code and predict. With the renders removed and no -f guard:
#   rc 2, stdout truncated at the step banner, 0 occurrences of META-FAILURE,
#   0 occurrences of any diagnostic of mine, and on stderr:
#     awk: cannot open .../ck-*.yaml (No such file or directory)
# It "fails closed" only because awk's exit status is 2 and 2 is this harness's
# meta-failure code. THAT IS A COINCIDENCE OF TWO UNRELATED NUMBERS, not a
# guard, and it reports the run as unmeasured while saying nothing about why.
# An unmatched glob is also how it arrives: bash passes the pattern through
# literally, so the file that "cannot open" is a filename nobody wrote.
_ck_require_live() { # <expected count of well-formed digests> <file>...
  local _want="$1"; shift
  local _f _v _n=0 _bad=0 _empty=0
  for _f in "$@"; do
    [[ -f "$_f" ]] || meta_failure "the settings-checksum step was asked to read ${_f} and there is no such file. If that name still contains a * the glob matched nothing, which means the renders this step is about were never written - so nothing below was measured, and without this line the run would have died on awk's exit status with no diagnostic at all."
    # `|| true` INSIDE the substitution, deliberately: any other awk failure must
    # arrive at the shape and count checks below as an empty value with a message
    # attached, not as a bare abort three lines from the thing it was measuring.
    _v="$(_ck_digest "$_f" || true)"
    if [[ "$_v" =~ ^[0-9a-f]{64}$ ]]; then _n=$((_n+1))
    elif [[ -n "$_v" ]]; then _bad=$((_bad+1))
    else _empty=$((_empty+1)); fi
  done
  [[ "$_bad" == 0 ]] || meta_failure "_ck_digest returned $_bad value(s) that are not a 64-character sha256 hex digest across $# file(s). It is reading the wrong field, so every equality below is between two strings of unknown provenance."
  [[ "$_n" == "$_want" ]] || meta_failure "_ck_digest yielded $_n well-formed digest(s) from $# file(s) ($_empty empty, $_bad malformed); this call site is committed to exactly $_want. Either the extractor is dead - in which case the equalities below hold trivially and mean nothing - or the set of ck-*.yaml renders changed and this number was not changed with it."
}

# 🔴 AND _ck_require_live IS NOT ENOUGH, WHICH IS gd-p2-rev's R1 AND IT IS RIGHT.
# It guards the extractor FUNCTION over a file glob; the assertions consume
# VARIABLES populated at separate call sites. THE GUARD AND THE GUARDED WERE IN
# DIFFERENT PLACES, so an extraction written inline at a call site - the same
# defect, one line further down - walks straight past it. gd-p2-rev proved that
# by moving MY OWN PLANT to the call site:
#
#   _mB="$(awk '$1=="checksum/nope:"{print $2}' "$WORK/ck-mut-B.yaml")"
#     -> rc=0, 297/297, 0 failures, and the original signature came back:
#          baseline: '... goes red (3af8bcc3...79 -> a67fb352...78) ...'
#          planted : '... goes red (3af8bcc3...79 -> ) ...'
#   a dead _uB at the URL call site -> rc=0, 297/297, 0 failures.
#   TWO OF TWO CALL-SITE PLANTS ON TWO INDEPENDENT EXTRACTORS, CLEAN FULL PASS.
#
# So the shape check moves ONTO THE OPERANDS, at the point of comparison, where
# it cannot be bypassed by changing how the value was obtained. This is
# gd-p2-rev's prescription, adopted as written and declared as adopted.
# _ck_require_live stays: it catches the function-level case earlier and with a
# better message, and an earlier, more specific diagnostic is worth keeping even
# when a later one would also fire.
#
# THE RULE THIS LEAVES BEHIND, which is the part that generalises past this file:
# GUARD THE VALUE AT THE POINT IT IS USED, NOT THE MACHINERY THAT PRODUCED IT.
# A guard on the producer is a guard on one way of producing.
_ck_is_digest() { # <label> <value>
  [[ "$2" =~ ^[0-9a-f]{64}$ ]] || meta_failure "the settings-checksum step is about to compare ${1}, whose value is not a 64-character sha256 hex digest (got: ${2:-<empty>}). Whatever produced it did not produce a digest, so the comparison it feeds is between strings of unknown provenance and its result - equal OR unequal - is not a fact about the chart."
}
# STRUCTURAL, and deliberately not a check on any credential value: the DSN must
# still look like this chart's DSN. An empty string, a truncated field and a line
# read out of the wrong key all fail it, and none of the four planted passwords
# appears in the pattern - so this cannot go quiet on an encoded credential the
# way a value-matching guard does (gd-secann-2's finding, applied to my own gate).
_ck_is_dsn() { # <label> <value>
  [[ "$2" =~ ^postgres://[^@[:space:]]+@127\.0\.0\.1:5432/[^?[:space:]]+\?sslmode= ]] || meta_failure "the settings-checksum step is about to compare the DSN from arm ${1}, and it does not have the shape this chart renders (got: ${2:-<empty>}). Expected postgres://<userinfo>@127.0.0.1:5432/<db>?sslmode=... . An empty or truncated value here makes every equality and inequality below meaningless."
}

_ck_render A  --set-string database.password="$_ck_A"
_ck_render A2 --set-string database.password="$_ck_A"
_ck_render B  --set-string database.password="$_ck_B"
_ck_render S1 --set-string database.password="$_ck_S1"
_ck_render S2 --set-string database.password="$_ck_S2"
_ck_render C  --set-string database.password="$_ck_A" --set-string database.name=other-db
# ARM U. gd-p2-rev's C1: the projection dropped the WHOLE userinfo, so rotating
# database.user left the digest byte-identical and no pod rolled - while
# NOTES.txt told the operator, in the section written to prevent exactly this,
# that rotating the user rolls them automatically. Same password as A, different
# user, and the digest must MOVE.
_ck_render U  --set-string database.password="$_ck_A" --set-string database.user="$_ck_U"

# ARM 0. NOT AN ASSERTION - A META-FAILURE, because "nothing was analysed" is a
# third outcome and this whole step compares digests for EQUALITY. Two empty
# renders are equal. Two identical inputs are equal. A chart that deleted the
# annotation gives two empty strings, which are equal. Every one of those is a
# green arm 1 measuring nothing.
#
# gd-p3-dev hit this by hand on the phase 3 copy - a --set that changed a value
# to what it already was - and read the resulting collapse as a defect in their
# own projection. gd-secann-2 hit it from the other side: their first run had
# helm failing a schema check on all four arms, every digest was sha256 of the
# empty string, and THREE of their four guards passed on it. The one that caught
# it was an absolute count. Both arms of that lesson are below.
for _n in A A2 B S1 S2 C U; do
  _rc="$(cat "$WORK/ck-$_n.rc")"
  [[ "$_rc" == 0 ]] || meta_failure "the settings-checksum arm $_n did not render (helm exit $_rc): $(head -3 "$WORK/ck-$_n.err"). Nothing in this step was measured."
  [[ -s "$WORK/ck-$_n.yaml" ]] || meta_failure "the settings-checksum arm $_n rendered an empty document. Every equality below would hold on it."
  [[ ! -s "$WORK/ck-$_n.err" ]] || meta_failure "the settings-checksum arm $_n wrote to stderr while exiting 0: $(head -3 "$WORK/ck-$_n.err")."
  # ABSOLUTE, not "at least one". A chart that stopped emitting the annotation
  # yields an empty digest for every arm, which satisfies arms 1-3 perfectly.
  _cnt="$(_ck_count "$WORK/ck-$_n.yaml")"
  [[ "$_cnt" == 1 ]] || meta_failure "the settings-checksum arm $_n carries $_cnt checksum/settings annotations, expected exactly 1. The digests compared below would be a comparison of empty strings."
done
# AND THE PART THAT MATTERS MOST: the inputs must genuinely differ. Read off the
# RENDERED output by value, in both directions, not off the --set arguments -
# an argument that helm ignored, misparsed or set to the value it already held
# is invisible from the command line and produces a perfect false green.
_uA="$(_ck_url "$WORK/ck-A.yaml")"; _uB="$(_ck_url "$WORK/ck-B.yaml")"
_uS1="$(_ck_url "$WORK/ck-S1.yaml")"; _uS2="$(_ck_url "$WORK/ck-S2.yaml")"
_uC="$(_ck_url "$WORK/ck-C.yaml")"; _uA2="$(_ck_url "$WORK/ck-A2.yaml")"
_uU="$(_ck_url "$WORK/ck-U.yaml")"
# THE OPERAND SWEEP: all six, not just the one the old -n happened to name.
# gd-p2-rev killed _uB specifically because _uA was the only guarded one.
_ck_is_dsn A "$_uA"; _ck_is_dsn A2 "$_uA2"; _ck_is_dsn B "$_uB"
_ck_is_dsn S1 "$_uS1"; _ck_is_dsn S2 "$_uS2"; _ck_is_dsn C "$_uC"
_ck_is_dsn U "$_uU"
[[ "$_uA" != "$_uB" ]] || meta_failure "the A and B renders carry the SAME server.database.url ($_uA), so the password-only differential below is comparing a chart against itself. This is the exact false green the arm exists to prevent."
[[ "$_uS1" != "$_uS2" ]] || meta_failure "the S1 and S2 renders carry the same server.database.url, so the percent-encoded differential is comparing a chart against itself."
[[ "$_uA" != "$_uC" ]] || meta_failure "the A and C renders carry the same server.database.url, so the positive control below cannot fire on a change that never happened."
[[ "$_uA" == "$_uA2" ]] || meta_failure "two renders of identical inputs produced different DSNs ($_uA vs $_uA2). The renderer is not deterministic and no equality below means anything."
# THE CREDENTIAL THAT LANDED MUST BE THE ONE THAT WAS PLANTED, and for S1/S2
# that means it must have been percent-encoded on the way in. Those two arms are
# the ONLY thing in this file covering gd-secann-2's finding that a value-based
# projection goes silent on an encoded password; if the special characters never
# reached the DSN, the arms quietly degrade into two more plain-password arms
# and pass while covering nothing. There is a known mechanism for exactly that:
# gd-p3-dev measured `--set-string` eating backslashes through helm's own escape
# parser, so what an operator types and what the file holds are not the same
# string by default. None of the four characters below is a backslash, and this
# check is what turns that from a belief into a reading.
case "$_uS1" in
  *'%40'*) ;;
  *) meta_failure "the S1 render's DSN carries no percent-encoded octet ($_uS1). The @ : / and # in the planted password never reached the rendered file, so the percent-encoding arm is silently testing an ordinary password and covers nothing." ;;
esac
case "$_uS1" in
  *"$_ck_S1"*) meta_failure "the S1 render's DSN contains the planted password verbatim ($_uS1), so scion-hub.pctEncodeUserinfo did not run on it and this arm is not exercising the encoded case at all." ;;
  *) ;;
esac

# gd-p2-rev's OPTIONAL follow-up to R1, taken. A SHAPE GUARD SEPARATES
# RIGHT-SHAPE FROM WRONG-SHAPE; IT DOES NOT SEPARATE EXTRACTED FROM FABRICATED.
# They measured it: planting _uB = postgres://zzz:zzz@127.0.0.1:5432/zzz?sslmode=disable
# passes _ck_is_dsn, satisfies _uA != _uB, and the whole password differential
# then rests on a precondition that a hand-written constant met vacuously -
# rc=0, 303/303. The digest side survives the same plant because its comparisons
# MEAN something downstream; the DSN side had nothing catching it.
#
# The remedy is precedented three lines up: the S1 arm already asserts the
# planted password REACHED the rendered DSN. Extended to every arm whose planted
# value is known here. This is a guard on a guard, not a security assertion -
# the security claim is that these values are ABSENT from the digest, and it is
# asserted elsewhere. Here they must be PRESENT, in the render they came from,
# or the differential is between two strings nobody rendered.
_ck_planted() { # <label> <dsn> <needle>
  case "$2" in
    *"$3"*) ;;
    *) meta_failure "the ${1} render's DSN does not contain the value planted into it (looked for ${3} in ${2:-<empty>}). It has the right shape, so _ck_is_dsn accepted it, but a well-formed DSN that no arm of this step actually rendered satisfies every difference check below by construction and proves nothing about the chart." ;;
  esac
}
_ck_planted A  "$_uA"  "$_ck_A"
_ck_planted A2 "$_uA2" "$_ck_A"
_ck_planted B  "$_uB"  "$_ck_B"
_ck_planted C  "$_uC"  "$_ck_A"
_ck_planted U  "$_uU"  "$_ck_U"
# S1/S2 are deliberately absent: their planted passwords are percent-encoded on
# the way in, so PRESENCE of the literal is the thing that must NOT hold, and the
# case statement above asserts exactly that inversion for S1.

# SEVEN, derived from the renders on disk. The mutation control and the
# existing-secret arm are rendered further down and are covered where they land.
_ck_require_live 7 "$WORK"/ck-*.yaml

_dA="$(_ck_digest "$WORK/ck-A.yaml")"; _dA2="$(_ck_digest "$WORK/ck-A2.yaml")"
_dB="$(_ck_digest "$WORK/ck-B.yaml")"; _dS1="$(_ck_digest "$WORK/ck-S1.yaml")"
_dS2="$(_ck_digest "$WORK/ck-S2.yaml")"; _dC="$(_ck_digest "$WORK/ck-C.yaml")"
_dU="$(_ck_digest "$WORK/ck-U.yaml")"
_ck_is_digest A "$_dA"; _ck_is_digest A2 "$_dA2"; _ck_is_digest B "$_dB"
_ck_is_digest S1 "$_dS1"; _ck_is_digest S2 "$_dS2"; _ck_is_digest C "$_dC"
_ck_is_digest U "$_dU"
[[ "$_dA" == "$_dA2" ]] || meta_failure "two renders of identical inputs produced different checksum/settings values ($_dA vs $_dA2). The digest is not a function of the inputs and nothing below is interpretable."

if [[ "$_dA" == "$_dB" ]]; then
  pass "rotating database.password alone does not move checksum/settings, so the annotation is not an oracle for it (${_dA:0:16})"
else
  fail "checksum/settings CHANGED on a password-only rotation ($_dA -> $_dB). The annotation is published to a wider audience than the settings Secret and every other component of the DSN is public, so this digest lets a reader who cannot read the Secret confirm a guessed password offline."
fi

if [[ "$_dS1" == "$_dS2" ]]; then
  pass "the same holds for a password containing @ : / and # , which the DSN percent-encodes - so the redaction is by URL structure and not by matching the credential's value"
else
  fail "checksum/settings changed on a password-only rotation when the password percent-encodes ($_dS1 -> $_dS2). A projection that redacts by searching for the credential's literal value cannot see these, because scion-hub.pctEncodeUserinfo has already rewritten them - and it emits a document that reads as redacted while still carrying the secret."
fi

if [[ "$_dA" == "$_dS1" ]]; then
  pass "checksum/settings is identical across four different passwords, so the digest is independent of the credential rather than merely insensitive to one pair"
else
  fail "checksum/settings differs between two passwords that are not a rotation pair ($_dA vs $_dS1), so some part of the credential still reaches the digest input."
fi

# gd-p2-rev's C1, AND IT IS AN INEQUALITY ARM SITTING AMONG EQUALITIES, WHICH IS
# WHY IT WAS MISSING. Every other arm here asks the digest to STAY PUT, and the
# projection that made them pass was over-redacting: it blanked the entire
# userinfo, username included. The username is not a credential - it is in
# values.yaml, in NOTES.txt and in the plain settings document - so blanking it
# bought no secrecy and cost a real configuration change its pod roll. Measured
# by gd-p2-rev: rotate database.user, digest byte-identical, no roll, while
# NOTES.txt said the roll happens automatically.
if [[ "$_dU" != "$_dA" ]]; then
  pass "rotating database.user DOES move checksum/settings, so the sentence in NOTES.txt offering it as the automatic-roll workaround is true"
else
  fail "checksum/settings did not move when database.user changed ($_dA). NOTES.txt tells operators to rotate the user in the same upgrade and the roll happens automatically; it does not, and an operator rotating a leaked credential gets a green upgrade and keeps serving on the retired one. There is no correction anywhere else in the render."
fi

# THE POSITIVE CONTROL, and without it the three arms above are satisfied by a
# chart that hashes a constant, or by one that deleted the annotation. This is
# also the assertion that fails if someone "fixes" the rotation gap by removing
# the projection's input entirely.
if [[ "$_dC" != "$_dA" ]]; then
  pass "a non-credential change (database.name) DOES move checksum/settings, so the equalities above are the redaction working and not the digest having stopped"
else
  fail "checksum/settings did not move when database.name changed ($_dA). The digest has become a constant: settings changes will no longer roll the pods at all, and the equalities above are meaningless."
fi

# THE PLANTED MUTATION. Reverting the annotation to a digest of the raw template
# output must make the password-only differential go RED. Without this the three
# equalities above could be a property of the harness - two renders that differ
# in nothing - rather than a property of the projection.
_ckmut="$WORK/ck-unprojected"
rm -rf "$_ckmut"; cp -a "$CHART_DIR" "$_ckmut"
python3 - "$_ckmut/templates/deployment.yaml" <<'CKPY'
import sys
p = sys.argv[1]
s = open(p).read()
old = 'checksum/settings: {{ include "scion-hub.settingsChecksum" . }}'
new = 'checksum/settings: {{ include (print $.Template.BasePath "/secret-settings.yaml") . | sha256sum }}'
if s.count(old) != 1:
    sys.exit("PLANT-FAILED: expected exactly 1 occurrence of the projected annotation, found %d" % s.count(old))
open(p, 'w').write(s.replace(old, new))
CKPY
_ckmut_rc=0
_ck_mut_render() { # <out> <password>
  local rc=0
  "$HELM" template "$RELEASE" "$_ckmut" --namespace "$NAMESPACE" \
    --values "$CHART_DIR/ci/values-cloudsql.yaml" \
    --set-string database.auth=password --set-string database.user=probe-user \
    --set-string database.password="$2" >"$1" 2>"$1.err" || rc=$?
  return "$rc"
}
_ck_mut_render "$WORK/ck-mut-A.yaml" "$_ck_A" || _ckmut_rc=$?
_ck_mut_render "$WORK/ck-mut-B.yaml" "$_ck_B" || _ckmut_rc=$?
[[ "$_ckmut_rc" == 0 ]] || meta_failure "the un-projected control chart did not render (helm exit $_ckmut_rc): $(head -3 "$WORK/ck-mut-A.yaml.err"). The planted mutation below proves nothing."
# BOTH arms, and by shape rather than by -n. The assertion below wants them to
# DIFFER, so an empty _mB satisfies it against a real _mA - a pass produced by a
# dead extractor, which is how this control was measured failing open at 297/297.
# The -n guard that used to be here covered _mA only, which is the arm an empty
# value could not have hidden in.
_ck_require_live 2 "$WORK"/ck-mut-*.yaml
_mA="$(_ck_digest "$WORK/ck-mut-A.yaml")"; _mB="$(_ck_digest "$WORK/ck-mut-B.yaml")"
_ck_is_digest mA "$_mA"; _ck_is_digest mB "$_mB"
[[ "$(_ck_count "$WORK/ck-mut-A.yaml")" == 1 && "$(_ck_count "$WORK/ck-mut-B.yaml")" == 1 ]] || meta_failure "an arm of the un-projected control chart does not carry exactly one checksum/settings annotation, so its differential is not a comparison of two digests."
if [[ "$_mA" != "$_mB" ]]; then
  pass "with the projection removed the SAME password-only differential goes red ($_mA -> $_mB), so the equalities above are caused by the redaction and not by the two renders being identical"
else
  fail "removing the projection did not restore the password-only difference. This step's equalities are therefore not evidence of redaction - the two renders it compares do not differ in anything the digest can see, and the whole step is measuring nothing."
fi

# The permutation where the chart renders no settings file at all. The helper
# refuses a document with no server key, by design, so the annotation must not
# be emitted here - and an absent annotation is also the only correct answer:
# there is nothing for the chart to digest.
_ck_es="$WORK/ck-existing-secret.yaml"
# The rc is KEPT and stderr is KEPT. This line used to end `2>/dev/null` with no
# `||`, which under `set -e` meant a failed render killed the run with the
# diagnostic already thrown away - the exact pairing gd-p1-dev retracted their
# clearance over. An absent render must arrive here as a meta-failure that says
# so, because the assertion below is an ABSENCE and an empty file satisfies it.
_ck_es_rc=0
"$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
  --values "$CHART_DIR/ci/values-existing-secret.yaml" >"$_ck_es" 2>"$_ck_es.err" || _ck_es_rc=$?
[[ "$_ck_es_rc" == 0 ]] || meta_failure "the config.existingSecret render failed (helm exit $_ck_es_rc): $(head -3 "$_ck_es.err"). The assertion below is an ABSENCE and an unrendered file satisfies it perfectly."
[[ -s "$_ck_es" ]] || meta_failure "the config.existingSecret render is empty, so the absence asserted below is the absence of the whole manifest."
# A PAIRED POSITIVE CONTROL ON THE INSTRUMENT, AT THE POINT OF USE. The assertion
# below wants ZERO, which is also what a dead _ck_count returns, so the absence
# is only evidence if the same function can still find a annotation that IS
# there. Arm A has exactly one; if _ck_count cannot see it, it cannot be trusted
# to have looked here either. This is the R1 rule applied to a counter rather
# than to an extractor: the operand is a zero, and a zero needs a witness.
[[ "$(_ck_count "$WORK/ck-A.yaml")" == 1 ]] || meta_failure "_ck_count no longer finds the single checksum/settings annotation in arm A, so the zero it is about to report for the config.existingSecret render is not evidence of an absence - it is the same blindness, measured twice."
_ck_es_n="$(_ck_count "$_ck_es")"
if [[ "$_ck_es_n" == 0 ]]; then
  pass "under config.existingSecret no checksum/settings annotation is emitted at all, so the projection helper is never asked to parse a file the chart did not render"
else
  fail "under config.existingSecret the chart emitted $_ck_es_n checksum/settings annotations. There is no rendered settings document there, so the helper is digesting something it did not produce."
fi

# HALF B. The redaction removes the automatic roll on a credential-only change,
# and the remedy is an explicit rollout restart. A remedy nobody is told about
# is not a remedy, so it is asserted here in both directions - printed when a
# password is set, absent when there is none to rotate.
render_notes "$WORK/notes-rot-pw.txt" -f "$CHART_DIR/ci/values-cloudsql.yaml" \
  --set-string database.auth=password --set-string database.user=probe-user \
  --set-string database.password="$_ck_A"
_ck_rot_heading='ROTATING THE PASSWORD REQUIRES ONE MANUAL STEP'
if grep -qF -- "$_ck_rot_heading" "$WORK/notes-rot-pw.txt"; then
  pass "NOTES.txt states that rotating database.password requires a manual restart"
else
  fail "NOTES.txt does not tell an operator that rotating database.password leaves the running pods on the old credential. The chart made the roll stop happening; the operator finds out from an authentication that keeps succeeding with a password they retired."
fi
# SUBSTITUTED, not a placeholder. An instruction an operator has to edit before
# it runs is an instruction that gets edited wrong at the moment it is needed.
if grep -qF -- "kubectl rollout restart deploy/$RELEASE-scion-hub -n $NAMESPACE" "$WORK/notes-rot-pw.txt"; then
  pass "the restart command NOTES.txt prints names this release's deployment and namespace, so it runs as printed"
else
  fail "NOTES.txt prints a rollout restart command that does not name this release's deployment and namespace. Got: $(grep -F 'rollout restart' "$WORK/notes-rot-pw.txt" || printf '(no rollout restart line at all)')"
fi
# THE NEGATIVE ARM, held against a section already proven non-empty above. Under
# iam there is no password in the DSN, nothing to rotate, and the paragraph
# would be advice about a value the operator did not set.
if grep -qF -- 'CLOUD SQL' "$WORK/notes-cloudsql.txt"; then
  if grep -qF -- "$_ck_rot_heading" "$WORK/notes-cloudsql.txt"; then
    fail "the iam permutation's NOTES prints the password-rotation restart step. Under iam the DSN carries no password, so this is instructions for a value that does not exist."
  else
    pass "the iam permutation's NOTES does not print the password-rotation step, and that absence is inside a render that does carry a CLOUD SQL section"
  fi
else
  fail "the iam permutation's NOTES has no CLOUD SQL section, so the absence of the rotation step there is the absence of everything."
fi

# HALF C. THE TWO PLACES ONE RENDER TALKS ABOUT ROTATION MUST NOT DISAGREE.
#
# gd-p2-rev's C2. The credential-placement paragraph in the CLOUD SQL section
# used to end "Rotating it is a helm upgrade, which rewrites the Secret and
# rolls the pods", and ninety lines further down the SAME render said ROTATING
# THE PASSWORD REQUIRES ONE MANUAL STEP. Both printed, to the same operator, on
# the same install. The first one is the one a reader hits first and the one
# that sounds like reassurance, so it is the one that gets believed - and it is
# the false one, because the whole point of the redacted projection is that a
# credential-only change does NOT move the annotation.
#
# WHAT THIS CAN AND CANNOT CHECK. A contradiction between two English sentences
# is not something grep decides. What is checkable is narrower and still worth
# pinning: the earlier paragraph must not make the claim in the wording that
# was wrong, and it must send the reader to the section that states the truth.
# The second arm is the one that generalises - a paragraph that names the
# heading cannot be rewritten into a contradiction without someone reading the
# heading it points at.
sed -n '/Under password the credential lives/,/BEFORE THE POD CAN CONNECT/p' \
  "$WORK/notes-rot-pw.txt" >"$WORK/notes-rot-place.txt"
# The arm below is an ABSENCE and an empty file is the perfect absence, so the
# region has to be proven to exist before it is allowed to be clean.
[[ -s "$WORK/notes-rot-place.txt" ]] || meta_failure "the credential-placement paragraph could not be located in the password render (no text between 'Under password the credential lives' and 'BEFORE THE POD CAN CONNECT'). The contradiction arm below would report a clean paragraph because there is no paragraph."
if grep -qEi 'rolls? the pods' "$WORK/notes-rot-place.txt"; then
  fail "the credential-placement paragraph claims a pod roll: $(grep -Ei 'rolls? the pods' "$WORK/notes-rot-place.txt" | head -1 | sed 's/^ *//'). The same render says ROTATING THE PASSWORD REQUIRES ONE MANUAL STEP. An operator who believes the first sentence rotates a leaked credential, sees a green upgrade and keeps serving on the retired one."
else
  pass "the credential-placement paragraph makes no pod-roll claim, so it does not contradict the manual-step section printed below it in the same render"
fi
if grep -qF -- "$_ck_rot_heading" "$WORK/notes-rot-place.txt"; then
  pass "the credential-placement paragraph points the reader at [$_ck_rot_heading], so the two statements about rotation are joined rather than left to disagree"
else
  fail "the credential-placement paragraph does not name [$_ck_rot_heading]. A reader who stops at the placement paragraph - which reads like a complete account of where the credential lives and what changing it does - never reaches the step that keeps a retired password from staying live."
fi

# THE ONE ARM IN THIS STEP THAT READS SOURCE INSTEAD OF A RENDER, AND WHY IT HAS
# TO. C1 made the redaction preserve the username, which gave the helper's own
# output - ://user:[redacted...]@ - the exact shape the URL backstop below it
# hunts for. The backstop would then fire on every render it had just correctly
# redacted, so the known-redacted form is removed from the probe first. gd-em
# asked whether that removal widens the door. It does not, and this is the
# measurement rather than the argument (mutation rig, sub-floor 7-char password
# so the 12-character value axis stays silent and the URL axis is the only thing
# under test):
#
#   A  redaction disabled, raw credential URL in the digest input
#      -> rc=1, the backstop fires and names itself. The door is not widened.
#   B  the probe removal deleted entirely
#      -> rc=1 on an ORDINARY render. The line is forced, not decorative.
#   C  the probe removal rewritten from a literal to the pattern
#      ":[^/@[:space:]]+@" -> "@", plus A's raw credential
#      -> rc=0. THE CREDENTIAL REACHES THE DIGEST AND NOTHING SAYS SO.
#
# C is the whole risk in one line: a pattern that strips the marker also strips
# real credentials that resemble it, and the failure is silent and green. A and
# B are caught by any arm in this step, because they make the chart refuse to
# render. C is caught by NOTHING that runs against a render, because everything
# renders. It is not reachable from values either - assertNoCredential refuses a
# credential URL at every values surface that feeds settings.yaml, which is what
# makes the backstop a backstop - so there is no permutation to point at it.
#
# So this arm pins the SHAPE OF THE LINE, which is the weakest kind of assertion
# in this file and is used here because the alternative is no assertion at all.
# It is stated as a limit, not sold as a proof: it catches the rewrite that
# mutation C performed, and it would not catch a literal that is merely wrong.
# shellcheck disable=SC2016  # $redacted and $projection are literal Go template variables, not shell expansions
_probe_src="$(grep -c 'probe := replace (printf ":%s@" \$redacted) "@" \$projection' "$CHART_DIR/templates/_helpers.tpl" || true)"
_probe_any="$(grep -c 'probe :=' "$CHART_DIR/templates/_helpers.tpl" || true)"
if [[ "$_probe_any" != "1" ]]; then
  meta_failure "the settings-checksum backstop's probe assignment was expected exactly once in _helpers.tpl and grep found $_probe_any. The arm below cannot say which line it is reading, and an arm that cannot locate its subject reports on nothing."
fi
if [[ "$_probe_src" == "1" ]]; then
  pass "the URL backstop's probe removes the redaction marker as a LITERAL, so it cannot also swallow a real credential that resembles it (mutation C, the rewrite to a pattern, renders clean with a credential in the digest)"
else
  fail "the URL backstop's probe is no longer the literal removal of the marker this helper writes. If it is now a pattern, it strips real credentials too and the backstop is disarmed silently - measured: the render succeeds and the credential reaches the digest input. Remove the exact literal, never a shape that resembles it."
fi

# --------------------------------------------------------------------------
# THE BANNED READINESS LITERAL, SWEPT OVER THE WHOLE TREE.
#
# Phase 0's constraint is that the deprecated liveness path must not appear
# anywhere in the chart tree. Until this block existed that constraint was
# enforced by an author running an ad-hoc grep and reporting a zero - and on
# 2026-08-17 the author of THIS block broke it, in the VALIDATION.md sentence
# asserting the absence, and the ad-hoc zero was three commits stale by then.
# Nothing went red, because nothing was watching. A rule is only in executable
# form for as long as something can execute it.
#
# The literal is assembled from parts so that this file is not itself a hit.
# That is not cleverness for its own sake: hack/ ships in neither the tarball
# nor the render, but it is inside the tree being swept, and a sweep that
# matches its own source is a sweep that can never return zero.
_banned_path="/health""z"
#
# EXPLICIT FILE ARGUMENTS, NEVER -r. Recursion is the one axis on which a grep
# wrapper was measured to silently narrow the file set on this project; an
# explicit path is one the instrument cannot decline to visit. -F because the
# pattern is a literal and -F is the only flag that cannot reinterpret it.
# /usr/bin/grep BY FULL PATH, and no -z/-Z/--null anywhere in this block. Both
# matter and neither is style. The stock binary sees binary files; the shell
# function that shadows `grep` in some interactive environments passes -I and
# --exclude-dir=.git, so it would skip a needle sitting in a binary or under
# .git. MEASURED, planted needle in a binary blob: /usr/bin/grep -lF finds it,
# the wrapped form does not. The chart tree holds 0 binaries and 0 .git paths
# today, so this changes no current result - it means the gate keeps working if
# that stops being true. Adding -z or -0 here to "harden" the list would switch
# engines mid-gate, which is the opposite of hardening.
# STDERR IS CAPTURED AND PUBLISHED, NOT SUPPRESSED, AND THE EXIT STATUS IS KEPT.
# The first version of this line was `... 2>/dev/null || true`, and it was
# FAIL-OPEN: grep exits 0 on a match, 1 on none and >=2 on an error, so an
# unreadable file or a too-long argument list produced an empty stdout, a
# discarded status and a discarded diagnostic - indistinguishable, byte for
# byte, from a clean absence. The gate would have printed "appears in 0 of 41
# files" and passed. `0 lines of stderr` is a finding; a suppressed stream is
# not. Both sweeps below append here and both statuses are asserted afterwards.
# Both call sites redirect to a FILE rather than into $(...) or < <(...), and
# that is required, not tidiness. MEASURED: with the tree sweep behind a process
# substitution, mapfile returned before the subshell's status append landed, and
# the status file held 1 line where 2 invocations had run. Redirecting to a file
# runs the function in THIS shell, so $? is the parent's and there is no write
# to race. The count assertion below is what caught it - it demanded exactly 2
# and got 1 - which is the whole argument for pinning an absolute count instead
# of checking that the statuses seen so far look acceptable.
_sweep_err="$(mktemp)"; _sweep_rc="$(mktemp)"; _sweep_out="$(mktemp)"
# `|| _rc=$?` rather than `|| true`: this file runs under `set -euo pipefail`,
# where grep's perfectly normal exit 1 for "no match" would abort the script.
# That pressure is what produced the original `|| true`, and `|| true` is what
# threw the status away. Putting grep on the left of `||` suspends errexit for
# it AND keeps the number, so the guard and the reading survive together.
_sweep() {
  local _rc=0
  if [[ $# -lt 2 ]]; then printf '99\n' >>"$_sweep_rc"; return 0; fi
  /usr/bin/grep -lF -- "$1" "${@:2}" 2>>"$_sweep_err" || _rc=$?
  printf '%s\n' "$_rc" >>"$_sweep_rc"
  return 0
}

# THE NEEDLE'S IDENTITY, PINNED. Measured 2026-08-17: with the needle mutated
# to a different assembled-from-parts string, this whole block passed 285/285
# and reported "appears in 0 of 41 files". The planted positive above does NOT
# catch that, because it plants whatever _banned_path holds and then finds it -
# it proves the mechanism fires, and says nothing about WHICH string it fires
# on. A scanner that can go blind and still report PASS is worse than no
# scanner, so the needle is checked against a digest committed independently of
# the expression that builds it.
# `|| true` because this file is `set -euo pipefail` and pipefail hands back
# sha256sum's status, not cut's: a sha256sum that exists but fails would abort
# the script here instead of reaching the comparison below. There IS an upstream
# `command -v sha256sum` guard at :864, but that answers callability and not
# whether it works, and a site whose safety depends on a guard 500 lines away is
# a site that changes meaning when either end moves. Empty is the outcome the
# comparison below was written for, and it fails with a message.
_needle_digest="$( (printf '%s' "$_banned_path" | sha256sum | cut -c1-16) || true )"
if [[ "$_needle_digest" == "15a99506b4e1757d" ]]; then
  pass "the banned-path needle is the string this gate was written for (sha256[0:16] 15a99506b4e1757d)"
else
  fail "the banned-path needle has been changed: sha256[0:16] is $_needle_digest, committed is 15a99506b4e1757d. Every absence this block reports is about some other string."
fi

# THE CORPUS, CLEARED ON EVERY KNOWN NARROWING AXIS AT EVERY DEPTH. A
# .gitignore three directories down narrows a search rooted above it, so the
# required form is a find at all depths, never a look at the root. Measured
# 2026-08-17 over this chart:
#   total files, all depths            41
#   .gitignore  at any depth            0      .ugrep at any depth   0
#   directories named .git              0
# The row that USED to sit here said "non-text files 0 of 41", counted with a
# -Il census, and it is withdrawn rather than corrected: "binary" under the
# wrapper is a verdict on a (file, pattern) PAIR, not on a file, so a census
# taken with one pattern cannot clear a sweep run with another. Measured
# elsewhere in the fleet: a \377\376 file with NO NUL byte is called text by
# one and dropped by the other, in both directions. The right statement is not
# a cleaner census, it is that THIS GATE IS NOT EXPOSED TO THAT FILTER AT ALL -
# it calls /usr/bin/grep, which has no -I unless asked, and stock grep -l does
# report a match inside a high-byte file. The encoding question belongs to the
# wrapped engine and this gate does not use it.
#   .helmignore at any depth            1      (helm-only, not a grep input)
#   positive control -name Chart.yaml   1      FIRES
# None of it can reach this gate anyway - no traversal, stock binary, explicit
# paths - but "cannot reach it" is an argument and the table is a measurement.
# The enumerator itself was checked too: wrapped and stock find return the same
# set in the same order, 8 runs each, one distinct order per arm.
mapfile -t _tree < <(find "$CHART_DIR" -type f ! -path '*/.git/*' | sort)
_tree_n="${#_tree[@]}"

# THE DENOMINATOR, DERIVED INDEPENDENTLY RATHER THAN ASSERTED NON-ZERO.
# Non-emptiness is not a denominator: a sweep over one file is non-empty and
# proves nothing. find and git enumerate the tree by unrelated means, so a
# disagreement means one of them is not seeing the chart.
# `if _git_list=$(...)` and not `_git_n=$(git ... | wc -l)`. MEASURED: the
# pipeline form made the meta_failure below UNREACHABLE. Under `set -euo
# pipefail` a git that exits 128 - which is what git does outside a work tree -
# takes the whole script down at that line, so the branch written to report
# "git could not enumerate the chart" could never run. Tested by pointing the
# gate at a non-repo copy: rc=128, output stopped mid-gate, no diagnostic.
# An `if` suspends errexit, and git's stderr is kept and printed in the failure
# rather than discarded, because the reason git could not read the tree is the
# whole content of that report.
_git_err="$(mktemp)"; _git_n=0
if _git_list="$(git -C "$CHART_DIR" ls-files 2>"$_git_err")"; then
  [[ -n "$_git_list" ]] && _git_n="$(printf '%s\n' "$_git_list" | wc -l)"
fi
if [[ "$_git_n" -eq 0 ]]; then
  meta_failure "git could not enumerate the chart, so the tree sweep below has no independently-derived denominator and its zero would be unfalsifiable. git said: $(tr '\n' ' ' <"$_git_err")"
elif [[ "$_tree_n" -lt "$_git_n" ]]; then
  meta_failure "find saw $_tree_n files and git tracks $_git_n; the sweep below is reading a smaller tree than the chart and any zero it returns is a property of the aperture."
fi

# POSITIVE CONTROL FIRST, so the arms below are evidence rather than an inert
# test. A control on a different input is not a control on your result - but a
# sweep that cannot be shown to fire at all is not a measurement in the first
# place.
_probe="$(mktemp)"; printf 'livenessProbe: %s\n' "$_banned_path" >"$_probe"
_sweep "$_banned_path" "$_probe" >"$_sweep_out"
if [[ "$(wc -l <"$_sweep_out")" -eq 1 ]]; then
  pass "the banned-path sweep fires on a planted occurrence, so its zero below is a reading and not a silence"
else
  fail "the banned-path sweep did not fire on a file that contains the banned path - every absence it reports is vacuous"
fi
rm -f "$_probe"

_sweep "$_banned_path" "${_tree[@]}" >"$_sweep_out"
mapfile -t _hits <"$_sweep_out"
if [[ "${#_hits[@]}" -eq 0 ]]; then
  pass "the banned readiness literal appears in 0 of $_tree_n files in the chart tree ($_git_n tracked)"
else
  fail "the banned readiness literal appears in ${#_hits[@]} of $_tree_n files: ${_hits[*]}. The readiness path is /readyz; this literal must not ship, and VALIDATION.md ships."
fi

# THE TWO ASSERTIONS THAT MAKE THE ZERO ABOVE MEAN ANYTHING. Without these the
# sweep reports absence whether it read 41 files or none of them.
_err_lines="$(wc -l <"$_sweep_err")"
if [[ "$_err_lines" -eq 0 ]]; then
  pass "the banned-path sweeps wrote 0 lines of stderr, so the zero above is not a diagnostic that was thrown away"
else
  fail "the banned-path sweeps wrote $_err_lines lines of stderr: $(tr '\n' ' ' <"$_sweep_err"). grep could not read part of the tree, so its zero is about the files it managed to open."
fi

# grep's status: 0 = matched, 1 = no match, >=2 = ERROR. 99 is this block's own
# marker for a sweep called with no files to search. Only 0 and 1 are readings;
# everything else is the sweep failing to run, which must not read as absence.
_bad_rc="$(/usr/bin/grep -cvE '^[01]$' "$_sweep_rc" || true)"
_rc_n="$(wc -l <"$_sweep_rc")"
if [[ "$_rc_n" -eq 2 && "$_bad_rc" -eq 0 ]]; then
  pass "both banned-path sweeps ran and exited 0 or 1 ($(tr '\n' ',' <"$_sweep_rc")) - a match and a clean absence, not an error mistaken for one"
else
  fail "the banned-path sweeps exited $(tr '\n' ',' <"$_sweep_rc") over $_rc_n invocations (expected exactly 2, each 0 or 1). A status of 2 or more is grep failing, and an empty result from a failed grep is not an absence."
fi
rm -f "$_sweep_err" "$_sweep_rc" "$_sweep_out" "$_git_err"

# --------------------------------------------------------------------------
printf '\n'
printf 'assertions: %d/%d   failures: %d\n' "$assertions" "$EXPECTED_TOTAL" "$failures"

# The count check is NOT gated on there being no failures. A guard whose enabling
# condition shares a cause with the failure it detects is switched off exactly
# when it is needed: a run that went wrong is the run most likely to have skipped
# assertions, and it is the run whose count nobody can otherwise reconstruct. So
# the number is checked first, on every path, and it counts assertions EXECUTED
# rather than assertions passed - which is a question orthogonal to whether the
# chart is correct.
if [[ $assertions -ne $EXPECTED_TOTAL ]]; then
  meta_failure "$assertions assertions executed, and this file is committed to exactly $EXPECTED_TOTAL. Short means checks were skipped; over means checks were added without the number being committed alongside them. Change EXPECTED_TOTAL deliberately, in the commit that changes the count."
fi
if [[ $failures -eq 0 ]]; then
  printf 'All static checks passed. Nothing here has been run against a cluster; see VALIDATION.md.\n'
  exit 0
fi
printf '%d check(s) FAILED.\n' "$failures"
exit 1
