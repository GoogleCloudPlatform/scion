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

# scripts/cloudrun/tests/test_deploy_wiring.sh -- runs scripts/cloudrun/deploy.sh
# end to end against stub gcloud and docker commands and asserts on the
# gcloud calls it makes. Never contacts GCP.
#
# Covers the hub service account minting grant: by default the hub SA is
# granted roles/iam.serviceAccountAdmin on the project and
# iamcredentials.googleapis.com is enabled; SCION_HUB_SA_MINTING=false
# skips the grant entirely.
#
# Requires bash, python3 and openssl (deploy.sh uses the last two).
#
# Usage:
#   ./scripts/cloudrun/tests/test_deploy_wiring.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_SH="$(dirname "$SCRIPT_DIR")/deploy.sh"

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT

STUB_BIN="${WORK_DIR}/bin"
mkdir -p "$STUB_BIN"

# Stub gcloud: logs every call (one line, arguments space-joined) and
# answers the few read calls deploy.sh depends on. Everything else
# succeeds silently.
cat > "${STUB_BIN}/gcloud" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$GCLOUD_LOG"
# Drain stdin for calls that read a secret payload, so the writer in
# deploy.sh's pipeline does not fail with SIGPIPE.
for arg in "$@"; do
  [[ "$arg" == "--data-file=-" ]] && cat >/dev/null
done
case "$*" in
  "projects describe "*) echo "123456789" ;;
  "run services describe "*) echo "https://scion-hub-test.a.run.app" ;;
  "secrets describe "*) exit 1 ;;
esac
exit 0
STUB
printf '#!/usr/bin/env bash\nexit 0\n' > "${STUB_BIN}/docker"
chmod +x "${STUB_BIN}/gcloud" "${STUB_BIN}/docker"

PROJECT="demo-project"
SA_EMAIL="scion-hub-sa@${PROJECT}.iam.gserviceaccount.com"
SA_ADMIN_BINDING_RE="^projects add-iam-policy-binding ${PROJECT} --member=serviceAccount:${SA_EMAIL//./\\.} --role=roles/iam\\.serviceAccountAdmin --condition=None( |$)"

PASS=0
FAIL=0
DEPLOY_RC=0
DEPLOY_LOG=""

# run_deploy [VAR=value ...] -- runs deploy.sh --skip-build with the
# required inputs set and any extra environment assignments given.
run_deploy() {
  GCLOUD_LOG="${WORK_DIR}/gcloud.log"
  : > "$GCLOUD_LOG"
  DEPLOY_LOG="$(env -i \
    PATH="${STUB_BIN}:/usr/bin:/bin" \
    HOME="$WORK_DIR" \
    GCLOUD_LOG="$GCLOUD_LOG" \
    SCION_PROJECT="$PROJECT" \
    SCION_CLOUDSQL_INSTANCE="pg" \
    SCION_DATABASE_PASSWORD="pw" \
    SCION_GCS_BUCKET="bucket" \
    SCION_RUNTIME_NETWORK="net" \
    SCION_RUNTIME_SUBNETWORK="subnet" \
    SCION_FILESTORE_IP="10.0.0.2" \
    SCION_FILESTORE_EXPORT="/export" \
    SCION_SESSION_SECRET="session" \
    "$@" \
    bash "$DEPLOY_SH" --skip-build 2>&1)"
  DEPLOY_RC=$?
}

gcloud_log() { cat "$GCLOUD_LOG"; }

count_matching() { gcloud_log | grep -cE -- "$1" || true; }

assert_eq() {
  if [[ "$1" == "$2" ]]; then
    PASS=$((PASS + 1))
  else
    FAIL=$((FAIL + 1))
    echo "  FAIL: $3 (expected '$1', got '$2')"
  fi
}

assert_contains() {
  if [[ "$1" == *"$2"* ]]; then
    PASS=$((PASS + 1))
  else
    FAIL=$((FAIL + 1))
    echo "  FAIL: $3 (output does not contain '$2')"
  fi
}

test_default_grants_service_account_admin() {
  run_deploy
  assert_eq "0" "$DEPLOY_RC" "a default deploy must exit 0"
  assert_eq "1" "$(count_matching "$SA_ADMIN_BINDING_RE")" \
    "a default deploy must grant roles/iam.serviceAccountAdmin to the hub SA once, at project scope, with --condition=None"
  assert_contains "$DEPLOY_LOG" "iam.serviceAccountUser, iam.serviceAccountAdmin" \
    "the roles-bound summary must list iam.serviceAccountAdmin"
}

test_default_enables_iamcredentials_api() {
  run_deploy
  assert_eq "1" "$(count_matching "^services enable iamcredentials\\.googleapis\\.com --project=${PROJECT}$")" \
    "a default deploy must enable iamcredentials.googleapis.com once"
}

test_minting_true_grants_service_account_admin() {
  run_deploy SCION_HUB_SA_MINTING=true
  assert_eq "1" "$(count_matching "$SA_ADMIN_BINDING_RE")" \
    "SCION_HUB_SA_MINTING=true must grant roles/iam.serviceAccountAdmin to the hub SA"
}

test_minting_false_makes_no_service_account_admin_binding() {
  run_deploy SCION_HUB_SA_MINTING=false
  assert_eq "0" "$DEPLOY_RC" "SCION_HUB_SA_MINTING=false must still deploy"
  assert_eq "0" "$(count_matching 'roles/iam\.serviceAccountAdmin')" \
    "SCION_HUB_SA_MINTING=false must make no gcloud call that mentions roles/iam.serviceAccountAdmin"
  assert_eq "1" "$(count_matching "^projects add-iam-policy-binding ${PROJECT} .*--role=roles/iam\\.serviceAccountUser ")" \
    "SCION_HUB_SA_MINTING=false must still grant the other hub SA roles"
  assert_eq "1" "$(count_matching "^services enable iamcredentials\\.googleapis\\.com ")" \
    "SCION_HUB_SA_MINTING=false must still enable iamcredentials.googleapis.com"
  assert_contains "$DEPLOY_LOG" "Skipped iam.serviceAccountAdmin (SCION_HUB_SA_MINTING is false)" \
    "the deploy must say the grant was skipped"
  assert_contains "$DEPLOY_LOG" "gcloud projects remove-iam-policy-binding ${PROJECT} --member=serviceAccount:${SA_EMAIL} --role=roles/iam.serviceAccountAdmin --condition=None" \
    "the deploy must print how to remove a grant left by an earlier deploy"
}

test_minting_invalid_value_refused() {
  run_deploy SCION_HUB_SA_MINTING=yes
  assert_eq "true" "$([[ "$DEPLOY_RC" -ne 0 ]] && echo true || echo false)" \
    "an invalid SCION_HUB_SA_MINTING must stop the deploy"
  assert_contains "$DEPLOY_LOG" "Invalid SCION_HUB_SA_MINTING: 'yes' (expected: true or false)" \
    "the error must name the variable and the accepted values"
  assert_eq "0" "$(count_matching '^projects add-iam-policy-binding ')" \
    "no role may be granted after an invalid SCION_HUB_SA_MINTING"
}

mapfile -t TESTS < <(declare -F | awk '{print $3}' | grep '^test_' | sort)
for t in "${TESTS[@]}"; do
  echo "--- $t"
  "$t"
done

echo ""
echo "scripts/cloudrun wiring: ${PASS} passed, ${FAIL} failed"
[[ "$FAIL" -eq 0 ]]
