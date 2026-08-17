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
  if grep -qF -- "$expected" <<<"$out"; then
    pass "$label"
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
settings_block "$WORK/settings.yaml"      | sed 's/^\( *mode: \)\(proxy\|oauth\)$/\1MASKED/' >"$WORK/auth-a"
settings_block "$WORK/settings-oauth.yaml" | sed 's/^\( *mode: \)\(proxy\|oauth\)$/\1MASKED/' >"$WORK/auth-b"
if diff -u "$WORK/auth-a" "$WORK/auth-b" >"$WORK/auth.diff"; then
  pass "the two auth modes render identical settings.yaml apart from auth.mode"
else
  fail "switching auth.mode changed more than the auth subtree"
  cat "$WORK/auth.diff"
fi
# The mask has to have masked something, or the comparison above is comparing
# two files that were never different.
if grep -qxF '    mode: MASKED' "$WORK/auth-a" && grep -qxF '  mode: hosted' "$WORK/auth-a"; then
  pass "the auth-mode mask matched auth.mode and left server.mode alone"
else
  fail "the auth-mode mask did not match - the comparison above proves nothing"
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
step "the settings file is delivered read-only, over a writable state directory"
# --------------------------------------------------------------------------
for name in settings existing-secret; do
  f="$WORK/$name.yaml"
  if grep -q 'mountPath: "/home/scion/.scion"' "$f"; then
    pass "$name mounts a volume at the hub's state directory"
  else
    fail "$name does not mount \$HOME/.scion"
  fi
  if grep -q 'subPath: settings.yaml' "$f" && grep -q 'mountPath: "/home/scion/.scion/settings.yaml"' "$f"; then
    pass "$name mounts settings.yaml as a subPath inside it"
  else
    fail "$name does not mount settings.yaml as a subPath"
  fi
  if grep -q 'defaultMode: 0444' "$f"; then
    pass "$name projects the settings file 0444"
  else
    fail "$name does not project the settings file 0444 - 0600 would be unreadable to a non-root uid, and a decimal 444 is group-writable"
  fi
  # The directory must NOT be read-only: storage/, templates/ and scion-token
  # are created in it at runtime. A read-only mount here breaks the hub for
  # reasons that have nothing to do with configuration.
  if grep -A2 'name: scion-home' "$f" | grep -q 'readOnly: true'; then
    fail "$name mounts the hub's state directory read-only; only settings.yaml may be read-only"
  else
    pass "$name leaves the hub's state directory writable"
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
step "nothing redirects the hub away from the mounted settings file"
# --------------------------------------------------------------------------
# Everything else in this file asserts what settings.yaml CONTAINS. Nothing so
# far asserts that the hub will read it. --config / -c redirects the entire
# configuration load away from $HOME/.scion/settings.yaml, and it does it
# silently: the mount still exists, the mode is still 0444, schema_version is
# still rendered, every check above still passes, and the hub is reading
# somebody else's file.
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
config_flag_re='^\s*-\s*"?(--config|-c)(=|"?$)'

for fixture in '            - "--config"' '            - --config' '            - "--config=/etc/x.yaml"' '            - "-c"' '            - -c'; do
  if grep -Eq "$config_flag_re" <<<"$fixture"; then
    pass "the config-flag pattern matches $(sed 's/^ *- *//' <<<"$fixture")"
  else
    fail "the config-flag pattern does NOT match $(sed 's/^ *- *//' <<<"$fixture") - the checks below cannot detect what they exist to detect"
  fi
done
for fixture in '            - "--hosted"' '            - "--enable-web"' '            - "--configure-something"' '            - "--concurrency"'; do
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
    fail "$name renders a config-path flag in the hub's arguments. It redirects the whole configuration load away from the mounted settings.yaml, and every other check in this file still passes when it does."
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
  "postgres without a GCS bucket" \
  "bucket" \
  "${BASE[@]}" \
  --set database.driver=postgres \
  --set storage.provider=gcs

expect_render_failure \
  "a plaintext base URL" \
  "hub.baseUrl" \
  --set image.repository=example.test/scion-hub-gke \
  --set hub.hubId=neg \
  --set hub.baseUrl=http://neg.example.com

expect_render_failure \
  "oauth mode without the acknowledgement" \
  "acknowledgeOAuthUnlanded" \
  "${BASE[@]}" \
  --set auth.mode=oauth

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
