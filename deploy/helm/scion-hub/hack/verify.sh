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

# The one permutation where the chart renders no settings.yaml, because the
# operator supplied the whole file. Held as an explicit list rather than as an
# "if the file is missing, skip" rule: a skip that derives itself from the
# output is a skip that silently grows to cover a regression.
NO_RENDERED_SETTINGS=(existing-secret)

failures=0
pass() { printf '  ok      %s\n' "$1"; }
fail() { printf '  FAIL    %s\n' "$1"; failures=$((failures + 1)); }
step() { printf '\n== %s\n' "$1"; }

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
    | grep '^    ' \
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
  if out=$("$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" "$@" 2>&1); then
    fail "$label: the render SUCCEEDED and was supposed to fail"
    return
  fi
  # "%!" is Go's marker for a printf verb that could not render its argument -
  # %!q(<nil>), %!d(string=x). It reaches an operator as line noise inside the
  # one sentence that is supposed to tell them what the chart read, and it is
  # invisible to a check that greps for the wording, because the wording is
  # still there. Asserted inside this helper rather than beside one message, so
  # a diagnostic added later is covered without anyone remembering to cover it.
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

for name in "${PERMUTATIONS[@]}"; do
  if render "$name" >"$WORK/$name.yaml" 2>"$WORK/$name.err"; then
    pass "template $name"
  else
    fail "template $name"
    cat "$WORK/$name.err"
    continue
  fi
  if "$KUBECONFORM" -strict -summary <"$WORK/$name.yaml" >/dev/null 2>&1; then
    pass "kubeconform $name"
  else
    fail "kubeconform $name"
    "$KUBECONFORM" -strict <"$WORK/$name.yaml" || true
  fi
done

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
step "no dead environment variable is ever emitted"
# --------------------------------------------------------------------------
# SCION_SERVER_DATABASE_* and SCION_SERVER_OIDC_* bind under no spelling at all.
# The loader ignores unmatched keys and discards the load error, so a chart that
# emitted one would install cleanly and behave as though the setting had never
# been given. There is no error to catch at runtime, which is why it is caught
# here.
for name in "${PERMUTATIONS[@]}"; do
  if grep -Eq 'SCION_SERVER_(DATABASE|OIDC)_[A-Z_]*[[:space:]]*[:=]' "$WORK/$name.yaml"; then
    fail "$name emits a SCION_SERVER_DATABASE_* or SCION_SERVER_OIDC_* variable"
    grep -En 'SCION_SERVER_(DATABASE|OIDC)_' "$WORK/$name.yaml" || true
  else
    pass "$name emits no dead SCION_SERVER_DATABASE_/OIDC_ variable"
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
if grep -q 'kind: Secret' "$WORK/existing-secret.yaml"; then
  fail "config.existingSecret did not suppress the chart's own Secret"
else
  pass "config.existingSecret suppresses the chart's Secret"
fi
if grep -q 'secretName: my-own-hub-settings' "$WORK/existing-secret.yaml"; then
  pass "the pod mounts the operator's Secret"
else
  fail "the pod does not mount the Secret named in config.existingSecret"
fi
if grep -q 'checksum/settings:' "$WORK/existing-secret.yaml"; then
  fail "checksum/settings is rendered under config.existingSecret, where it is a constant"
else
  pass "checksum/settings is omitted under config.existingSecret"
fi
# The positive twin for both: the chart does render a Secret, and does annotate
# the pod with its checksum, when it owns the file.
if grep -q 'kind: Secret' "$WORK/settings.yaml" && grep -q 'checksum/settings:' "$WORK/settings.yaml"; then
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
  [database.driver]='--set-string|database.driver=postgres|--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt'
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
  [storage.bucket]='--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt'
  [storage.provider]='--set-string|storage.provider=gcs|--set-string|storage.bucket=probe-bkt'
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
    | sed -e '1d' -e '/^$/d' | grep '^    ' \
    | sed -e 's/^    //' -e '/^[[:space:]]*#/d' \
    | python3 "$WORK/settings-leaves.py"
}
probe_other() {
  awk '/^# Source: /{keep = ($0 !~ /secret-settings\.yaml/)} keep' "$1" \
    | grep -v 'checksum/settings:'
}

if ! probe_render >"$WORK/probe-base.yaml"; then
  fail "the mutation probe's baseline render failed; every classification below is void"
else
  probe_settings "$WORK/probe-base.yaml" >"$WORK/probe-base.settings" || true
  probe_other "$WORK/probe-base.yaml" >"$WORK/probe-base.other"

  if ! "$HELM" template probe "$probe_dir" "${BASE[@]}" >"$WORK/probe-paths.yaml" 2>&1; then
    fail "the values walk did not render - the leaf enumeration below is empty"
  fi
  sed -n '/paths: |/,$p' "$WORK/probe-paths.yaml" \
    | sed -e '1d' -e 's/^    //' | grep '|' >"$WORK/probe-leaves.txt" || true

  probe_total=0 probe_settings_only=0 probe_half=0 probe_quiet=0 probe_err=0
  : >"$WORK/probe-observed.txt"
  probe_quiet_names="" probe_err_names="" probe_unaccounted=""

  while IFS='|' read -r leaf kind value; do
    [[ -z "$leaf" || "$leaf" == config.existingSecret ]] && continue
    probe_total=$((probe_total + 1))
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
      && grep -q 'config.existingSecret is set together with inline settings values' "$WORK/probe-ex.yaml"; then
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

  if [[ $probe_total -ge 50 ]]; then
    pass "the values walk enumerated $probe_total leaves to mutate"
  else
    fail "the values walk enumerated only $probe_total leaves - values.yaml has ~60 and this step is checking almost nothing"
  fi
  if [[ $probe_err -eq 0 ]]; then
    pass "every leaf could be mutated and rendered"
  else
    fail "$probe_err leaves could not be rendered with their mutation and were classified not at all:$probe_err_names - add an entry to PROBE_MUTATION"
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
    grep -n 'mountPath:' "$f" || true
  fi
  if grep -qF "HOME: \"$home\"" "$f"; then
    pass "$name sets HOME to $home"
  else
    fail "$name does not set HOME to $home - the mount and the variable have come apart, and only one of them moved"
    grep -n 'HOME:' "$f" || true
  fi
  if grep -q 'subPath: settings.yaml' "$f" && grep -qF "mountPath: \"$scion_dir/settings.yaml\"" "$f"; then
    pass "$name mounts settings.yaml as a subPath inside it"
  else
    fail "$name does not mount settings.yaml as a subPath at $scion_dir/settings.yaml"
  fi
  if grep -q 'defaultMode: 0444' "$f"; then
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
  if [[ "${#home_items[@]}" -gt 0 ]] && printf '%s\n' "${home_items[@]}" | grep -q 'readOnly: *true'; then
    fail "$name mounts the hub's state directory read-only; only settings.yaml may be read-only"
  else
    pass "$name leaves the hub's state directory writable"
  fi
  settings_mount="$(yaml_list_items "$f" settings | grep -F 'subPath: settings.yaml' || true)"
  if [[ -n "$settings_mount" ]] && grep -q 'readOnly: *true' <<<"$settings_mount"; then
    pass "$name mounts settings.yaml read-only"
  else
    fail "$name does not mount settings.yaml read-only - the file is the one thing in that directory the hub may not write, and defaultMode 0444 alone does not stop a write by uid 0 or a rename by the owner"
  fi
  if grep -q 'fsGroup' "$f"; then
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
init_entries_without_always() {
  awk '
    !inblock {
      if ($0 ~ /^[[:space:]]*initContainers:[[:space:]]*$/) {
        indent = match($0, /[^ ]/) - 1
        inblock = 1; entry = 0; ok = 0; name = "(unnamed)"
      }
      next
    }
    {
      if ($0 ~ /^[[:space:]]*$/) next
      if (match($0, /[^ ]/) - 1 <= indent) {
        if (entry && !ok) print name
        inblock = 0; entry = 0
        next
      }
      if ($0 ~ /^[[:space:]]*-[[:space:]]/) {
        if (entry && !ok) print name
        entry = 1; ok = 0; name = "(unnamed)"
      }
      if (name == "(unnamed)" && $0 ~ /name:[[:space:]]/) {
        n = $0; sub(/^.*name:[[:space:]]*/, "", n); name = n
      }
      if ($0 ~ /restartPolicy:[[:space:]]*Always[[:space:]]*$/) ok = 1
    }
    END { if (inblock && entry && !ok) print name }
  ' "$1"
}

# The fixtures. These are what keep the scan honest while the chart renders no
# init containers at all: they prove the detector still distinguishes the two
# shapes, so a Phase 2 developer adding the proxy sidecar sees this pass, and one
# adding a run-once container sees it fail.
cat >"$WORK/fx-plain.yaml" <<'FXPLAIN'
    spec:
      initContainers:
        - name: settings-init
          image: busybox
FXPLAIN
cat >"$WORK/fx-sidecar.yaml" <<'FXSIDE'
    spec:
      initContainers:
        - name: cloudsql-proxy
          image: proxy
          restartPolicy: Always
      containers:
        - name: hub
FXSIDE
cat >"$WORK/fx-both.yaml" <<'FXBOTH'
    spec:
      initContainers:
        - name: cloudsql-proxy
          image: proxy
          restartPolicy: Always
        - name: settings-init
          image: busybox
      containers:
        - name: hub
FXBOTH
if [[ "$(init_entries_without_always "$WORK/fx-plain.yaml")" == "settings-init" ]]; then
  pass "the init-container rule flags a run-once init container"
else
  fail "the init-container rule did NOT flag a run-once init container - the scan below cannot detect what it exists to detect"
fi
if [[ -z "$(init_entries_without_always "$WORK/fx-sidecar.yaml")" ]]; then
  pass "the init-container rule accepts a native sidecar"
else
  fail "the init-container rule flags a native sidecar - it would go red on Phase 2's Cloud SQL proxy, which is correct code"
fi
if [[ "$(init_entries_without_always "$WORK/fx-both.yaml")" == "settings-init" ]]; then
  pass "the init-container rule flags a run-once container alongside a sidecar"
else
  fail "the init-container rule missed a run-once container sharing the list with a native sidecar"
fi

for name in "${PERMUTATIONS[@]}"; do
  offenders="$(init_entries_without_always "$WORK/$name.yaml")"
  if [[ -n "$offenders" ]]; then
    fail "$name has a run-once init container ($(tr '\n' ' ' <<<"$offenders")) - settings are delivered by mount, not by copy. A native sidecar carries restartPolicy: Always and is allowed."
  else
    pass "$name has no run-once init container"
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
  if grep -q 'secretKeyRef' <<<"$out"; then
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
if grep -q 'app.kubernetes.io/name: scion-hub' <<<"$pod_tpl_labels" \
  && grep -q 'app.kubernetes.io/name: scion-hub' <<<"$selector_block"; then
  pass "the pod-template and selector label blocks were both found"
else
  fail "one of the two label blocks came back empty - the two checks above prove nothing"
fi

# The twin for the public_url refusal. server.hub is not a closed namespace - the
# refusal is on one key - so an unmodelled key under it must still render.
if out=$("$HELM" template "$RELEASE" "$CHART_DIR" --namespace "$NAMESPACE" \
    "${BASE[@]}" \
    --set config.extra.server.hub.hub_description=example 2>&1); then
  if grep -q 'hub_description: example' <<<"$out"; then
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
if [[ $failures -eq 0 ]]; then
  printf 'All static checks passed. Nothing here has been run against a cluster; see VALIDATION.md.\n'
  exit 0
fi
printf '%d check(s) FAILED.\n' "$failures"
exit 1
