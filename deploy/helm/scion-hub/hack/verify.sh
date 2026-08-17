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
  [varied]=/srv/hub
)

# Documents each permutation renders. A committed constant per values file, and
# not one constant for the chart: existing-secret is SIX because the chart
# deliberately emits no Secret when the operator supplies their own, and that
# asymmetry is the config.existingSecret contract rather than an accident of
# this table. A phase that changes the rendered manifest set updates these here,
# in its own diff, beside the template it added.
declare -A EXPECTED_DOCS=(
  [minimal]=7
  [settings]=7
  [settings-oauth]=7
  [existing-secret]=6
  [varied]=7
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
EXPECTED_TOTAL=261

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
# The pipeline's status is `cut`'s, which is 0 even when sha256sum could not
# read the file, so the empty string is the failure signal here and is named
# rather than left to print as a blank field.
_hs="$(sha256sum "$_hp" 2>>"$_tcerr" | cut -d' ' -f1)"; _hs="${_hs:-unreadable}"
_ks="$(sha256sum "$_kp" 2>>"$_tcerr" | cut -d' ' -f1)"; _ks="${_ks:-unreadable}"
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
  _env_names="$(grep -Eo 'SCION_SERVER_[A-Z_]+' "$WORK/$name.yaml" | sort -u | tr '\n' ' ')"
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
  --set hub.extraEnv=null >"$shadow_src" 2>/dev/null || true
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
settings_block "$WORK/settings.yaml"       | sed 's/^\(    mode: \)\(proxy\|oauth\)$/\1MASKED/' >"$WORK/auth-a"
settings_block "$WORK/settings-oauth.yaml" | sed 's/^\(    mode: \)\(proxy\|oauth\)$/\1MASKED/' >"$WORK/auth-b"
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
  _gactual="$(sha256sum "$GOLDEN_DIR/$_gname" 2>/dev/null | cut -d' ' -f1)"
  if [[ "$_gactual" != "$_gsum" ]]; then
    meta_failure "hack/ha-gates.txt records golden/$_gname at ${_gsum:0:12} but this tree has ${_gactual:-<absent>}. The walk did not run on the render this suite just verified. Regenerate with: go test ./cmd -run TestHelmChartHAGateWalk -update-chart-contract"
  fi
done < <(sed -n 's/^#   \([a-z-]*\.yaml\) *\([0-9a-f]\{64\}\)$/\1 \2/p' "$GATES_FILE")
# THE BINDING'S OWN DENOMINATOR. A sed that matched nothing would leave the loop
# body unexecuted and the binding silently absent.
_gcount="$(sed -n 's/^#   \([a-z-]*\.yaml\) *\([0-9a-f]\{64\}\)$/\1/p' "$GATES_FILE" | wc -l)"
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
  _esc="$(printf '%s' "$_k" | sed 's/\./\\./g')"
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
SESSION_MARKER="$(prose_marker "$(printf '%s\n' "${CANON_PROSE[@]}" | grep -F 'durable session')")"
[[ -n "$SESSION_MARKER" ]] || meta_failure "the walk's canonical arm records a durable-session gate that prose_marker() maps to the empty string, so every 'names all the gates' assertion below would pass on the strength of an empty string."
# Preflight keys that legitimately appear beside the gates. Each is here for a
# stated reason, because an exclusion list with no reasons becomes a place to
# put anything that turns a check green.
ALLOWED_NON_GATES=(
  server.hub.hub_id      # satisfied by this chart, named to explain where the refusal starts
  server.database.driver # ditto
  server.storage.provider # ditto
  server.database        # the assertExtraEnv refusal points the operator at this subtree
  server.mode            # hosted-mode prose
)
# THE OTHER LIMB'S EXTRA GATES, DERIVED AND NOT ASSUMED. The oauth arm refuses
# on server.auth.mode as well; that is the operator's own auth.mode being
# incompatible with HA detection, not an unlanded phase, so the chart's prose
# names it outside the numbered list and it is permitted rather than canonical.
# Deriving it means that if the hub ever adds a second oauth-only gate, it is
# permitted automatically and the parity check does not go red for the wrong
# reason - while a gate moving from the oauth limb into the proxy limb DOES turn
# the forward half red, because CANON_KEYS would gain it.
mapfile -t OAUTH_EXTRA_KEYS < <(
  comm -13 <(printf '%s\n' "${CANON_KEYS[@]}" | sort -u) \
           <(canon_block "settings-oauth.yaml" "well-formed" | sed -n 's/^KEY   //p' | sort -u)
)
[[ ${#OAUTH_EXTRA_KEYS[@]} -gt 0 ]] || meta_failure "the oauth arm of the walk adds no gate the proxy arm lacks. The chart's prose says it adds server.auth.mode; either that is now false, or the oauth arm was not read."
ALLOWED_NON_GATES+=("${OAUTH_EXTRA_KEYS[@]}")
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
grep -F 'This release cannot start the deployment these values describe' \
  "$CHART_DIR/templates/_helpers.tpl" >"$WORK/gates-fail.txt" || true
grep -E '^    (server\.|a durable)' "$WORK/notes-ack.txt" >"$WORK/gates-notes.txt" || true
[[ "$(wc -l <"$WORK/gates-doc.txt")" -eq "$HA_GATE_COUNT" ]] || meta_failure "the numbered gate table in _helpers.tpl matched $(wc -l <"$WORK/gates-doc.txt") lines; the walk found $HA_GATE_COUNT gates. The parity check below has nothing to compare."
[[ "$(wc -l <"$WORK/gates-fail.txt")" -eq 1 ]] || meta_failure "the assertHAUnlanded refusal string matched $(wc -l <"$WORK/gates-fail.txt") lines, not 1. The parity check below has nothing to compare."
[[ "$(wc -l <"$WORK/gates-notes.txt")" -eq "$HA_GATE_COUNT" ]] || meta_failure "the gate table in the rendered NOTES matched $(wc -l <"$WORK/gates-notes.txt") lines; the walk found $HA_GATE_COUNT gates. The parity check below has nothing to compare."

printf '%s\n' "${CANON_GATES[@]}" | sort -u >"$WORK/gates-canon.txt"
printf '%s\n' "${CANON_GATES[@]}" "${ALLOWED_NON_GATES[@]}" | sort -u >"$WORK/gates-permitted.txt"

for _src in doc fail notes; do
  _f="$WORK/gates-$_src.txt"
  gate_tokens "$_f" >"$WORK/gates-$_src.tok"
  _missing="$(comm -23 "$WORK/gates-canon.txt" "$WORK/gates-$_src.tok" | tr '\n' ' ')"
  _missing="${_missing% }"
  if [[ -z "$_missing" ]] && grep -qF "$SESSION_MARKER" "$_f"; then
    pass "the $_src copy names every gate the walk found"
  else
    fail "the $_src copy does not name every gate the walk found: missing [${_missing:-none}]$(grep -qF "$SESSION_MARKER" "$_f" || printf ' and the session-secret gate')"
  fi
  _extra="$(comm -13 "$WORK/gates-permitted.txt" "$WORK/gates-$_src.tok" | tr '\n' ' ')"
  _extra="${_extra% }"
  if [[ -z "$_extra" ]]; then
    pass "the $_src copy names no preflight key the walk did not find"
  else
    fail "the $_src copy names [$_extra], which is neither a gate the walk found nor a listed non-gate. If the hub added a gate, re-derive hack/ha-gates.txt and add it to all three copies; if it is not a gate, say why in ALLOWED_NON_GATES."
  fi
done

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
  [auth.mode]='--set-string|auth.mode=oauth|--set|auth.acknowledgeOAuthUnlanded=true'
  [database.connMaxIdleTime]='--set-string|database.connMaxIdleTime=9m'
  [database.connMaxLifetime]='--set-string|database.connMaxLifetime=9m'
  [database.driver]='--set-string|database.driver=postgres|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt|--set|acknowledgeHAUnlanded=true'
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

probe_render() {
  "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    --skip-schema-validation "${BASE[@]}" "$@" 2>&1
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
  #   auth.requireStableSigningKey   - default false; true is refused by
  #                                    templates/configmap-env.yaml unless
  #                                    config.existingSecret is set, and setting
  #                                    that companion lands us in the case above.
  #                                    Covered by tests/chart-integrity.sh
  #                                    section E, both directions.
  PROBE_UNMUTABLE=(config.existingSecret auth.requireStableSigningKey)
  if [[ ${#PROBE_UNMUTABLE[@]} -ne 2 ]]; then
    echo "HARNESS ERROR: PROBE_UNMUTABLE holds ${#PROBE_UNMUTABLE[@]} entries, not 2. Every entry is coverage this probe is not providing; read the reasons above before changing the number." >&2
    exit 2
  fi

  # probe_total is deliberately absent (gd-p1-rev, N-1, round 4). It counted the
  # kept leaves for the `-ge 50` floor, the floor is gone, and its four
  # surviving siblings are all read below. A counter nobody reads is a reader's
  # invitation to assume something is checked.
  probe_settings_only=0 probe_half=0 probe_quiet=0 probe_err=0
  : >"$WORK/probe-observed.txt"
  probe_quiet_names="" probe_err_names="" probe_unaccounted=""
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

    # Refused is a complete answer: the operator cannot reach the silent state.
    if ! "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
        --skip-schema-validation "${BASE[@]}" --set config.existingSecret=mine \
        "${mutation[@]}" >"$WORK/probe-ex.yaml" 2>&1 \
      && grep -qF 'config.existingSecret is set together with inline settings values' "$WORK/probe-ex.yaml"; then
      continue
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
  _got="$(init_entries_without_always "$WORK/$_fx.yaml" | tr '\n' ' ')"
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
  [db]=0              # server.database.url - Cloud SQL
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
prose="$(find "$CHART_DIR/templates" -type f -exec cat {} + | strip_quotes)"
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
ann_id="$(grep -oE 'scion\.io/hub-id: .*' "$WORK/hubid.yaml" | sed -e 's/.*: //' -e 's/"//g' | sort -u)"
set_id="$(settings_block "$WORK/hubid.yaml" | sed -n 's/^    hub_id: //p' | sed 's/"//g' | sort -u)"
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
  a="$(grep -oE 'scion\.io/hub-id: .*' "$WORK/$name.yaml" | sed -e 's/.*: //' -e 's/"//g' | sort -u || true)"
  s="$(settings_block "$WORK/$name.yaml" 2>/dev/null | sed -n 's/^    hub_id: //p' | sed 's/"//g' | sort -u || true)"
  if [[ $name == existing-secret ]]; then
    # The documented exception, asserted rather than skipped. Here the settings
    # file is the operator's and the chart renders none, so there is genuinely
    # nothing to agree with - and the annotation must still be there, because an
    # absent annotation would also satisfy an equality check against nothing.
    if [[ -n $a && -z $s ]]; then
      pass "$name: the annotation is rendered and no chart settings file exists to agree with it"
    else
      fail "$name: expected an annotation and no rendered hub_id, got annotation ${a@Q} and hub_id ${s@Q}"
    fi
  elif [[ -n $a && $a == "$s" ]]; then
    pass "$name: the annotation and server.hub.hub_id agree (${a@Q})"
  else
    fail "$name: the annotation (${a@Q}) and server.hub.hub_id (${s@Q}) disagree or are missing"
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
  if grep -Eq "$config_flag_re" <<<"$fixture"; then
    pass "the config-flag pattern matches $(sed 's/^ *- *//' <<<"$fixture")"
  else
    fail "the config-flag pattern does NOT match $(sed 's/^ *- *//' <<<"$fixture") - the checks below cannot detect what they exist to detect"
  fi
done
for fixture in '            - "--hosted"' '            - "--enable-web"' '            - "--configure-something"' '            - "--concurrency"' '            - "--config-dir-hint"'; do
  if grep -Eq "$config_flag_re" <<<"$fixture"; then
    fail "the config-flag pattern matches $(sed 's/^ *- *//' <<<"$fixture"), which is not a config-path flag - the checks below would reject legitimate arguments"
  else
    pass "the config-flag pattern does not match $(sed 's/^ *- *//' <<<"$fixture")"
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
  "hub.baseUrl" \
  --set image.repository=example.test/scion-hub-gke \
  --set hub.hubId=neg \
  --set hub.baseUrl=http://neg.example.com

expect_render_failure \
  "the SCHEMA rejects oauth mode without the acknowledgement" \
  "acknowledgeOAuthUnlanded" \
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
expect_render_failure \
  "the TEMPLATE rejects postgres without a GCS bucket" \
  "storage.bucket is required when database.driver is postgres" \
  --skip-schema-validation \
  "${BASE[@]}" \
  --set database.driver=postgres \
  --set storage.provider=gcs

expect_render_failure \
  "the TEMPLATE rejects a plaintext base URL" \
  "The session cookie's Secure attribute is derived from this prefix" \
  --skip-schema-validation \
  --set image.repository=example.test/scion-hub-gke \
  --set hub.hubId=neg \
  --set hub.baseUrl=http://neg.example.com

expect_render_failure \
  "the TEMPLATE rejects oauth mode without the acknowledgement" \
  "every human login fails" \
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
MLVALUES
if "$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    --values "$WORK/multiline-env.yaml" >/dev/null 2>&1; then
  pass "hub.extraEnv accepts an ordinary multi-line value"
else
  fail "hub.extraEnv rejected an ordinary multi-line value - the PEM check is matching on whitespace rather than on the PEM header"
fi

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
