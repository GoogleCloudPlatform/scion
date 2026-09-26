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

# Regression test for the IAP SSH firewall rule scoping fix
# (ptone/scion#1807): deploy.sh must tag the hub VM and create/update the
# IAP SSH firewall rule with --target-tags so it no longer allows IAP-range
# SSH to every VM on network default.
#
# This never touches a real GCP project or VM. It runs the actual Phase
# 1/2 prefix of deploy.sh (through VM creation, before the SSH-wait loop)
# against a fake `gcloud` on PATH (gcloud-stub.sh) that logs every
# invocation and returns canned output driven by env vars. Running the real
# script source (rather than reimplementing its logic) means the test
# tracks deploy.sh's actual behavior, not a copy of it.
#
# Usage: ./test-iap-fw-scope.sh

set -euo pipefail

TESTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SNV_DIR="$(cd "${TESTS_DIR}/.." && pwd)"
DEPLOY_SH="${SNV_DIR}/deploy.sh"

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "${WORK_DIR}"' EXIT

# A fake `gcloud` must actually be named `gcloud` to shadow the real binary
# on PATH. Symlink the stub into a dedicated bin dir rather than naming the
# checked-in file itself "gcloud".
STUB_BIN_DIR="${WORK_DIR}/bin"
mkdir -p "${STUB_BIN_DIR}"
ln -s "${TESTS_DIR}/gcloud-stub.sh" "${STUB_BIN_DIR}/gcloud"

PASS_COUNT=0
FAIL_COUNT=0

fail() {
  echo "  FAIL: $1"
  FAIL_COUNT=$((FAIL_COUNT + 1))
}

pass() {
  PASS_COUNT=$((PASS_COUNT + 1))
}

# Extract the real deploy.sh source from the top through the end of the "Create
# VM" block, stopping just before the SSH-wait loop (Phase 2 ends there; Phase
# 3+ drives the VM over `gcloud compute ssh`, which is out of scope for this
# fix and out of scope for a gcloud stub). Anchored on a comment already
# present in deploy.sh so the slice tracks the file instead of a hardcoded
# line number.
extract_partial_deploy() {
  local out="$1"
  awk '/^# --- Wait for SSH readiness/{exit} {print}' "${DEPLOY_SH}" >"${out}"
  {
    echo ''
    echo 'echo "TEST_HARNESS_REACHED_END"'
    echo 'exit 0'
  } >>"${out}"
  chmod +x "${out}"
}

PARTIAL_DEPLOY="${WORK_DIR}/deploy-partial.sh"
extract_partial_deploy "${PARTIAL_DEPLOY}"

CONFIG_FILE="${WORK_DIR}/config.json"
cat >"${CONFIG_FILE}" <<'EOF'
{
  "hub_name": "my-hub",
  "project_id": "test-project",
  "region": "us-central1",
  "machine_size": "small",
  "disk_size_gb": 200,
  "chat_plugins": [],
  "container_images": { "source": "build", "registry": "", "force_rebuild": false },
  "admin_email": "admin@example.com",
  "update_policy": "auto",
  "release_channel": "nightly"
}
EOF

HUB_TAG="scion-hub-my-hub"
FW_RULE="scion-hub-my-hub-allow-iap-ssh"

# Runs the partial deploy.sh with the given scenario env vars. Prints the
# gcloud call log path via $CALL_LOG on success; the script itself exits
# non-zero to the caller only via `run_scenario`'s own checks, not here.
run_scenario() {
  local name="$1"; shift
  local vm_exists="$1"; local fw_exists="$2"; local fw_tags="$3"

  echo "--- Scenario: ${name} ---"
  CALL_LOG="${WORK_DIR}/${name}.log"
  : >"${CALL_LOG}"

  if ! PATH="${STUB_BIN_DIR}:${PATH}" \
      GCLOUD_STUB_LOG="${CALL_LOG}" \
      MOCK_VM_EXISTS="${vm_exists}" \
      MOCK_FW_EXISTS="${fw_exists}" \
      MOCK_FW_TARGET_TAGS="${fw_tags}" \
      PYTHON="python3" \
      bash "${PARTIAL_DEPLOY}" --config "${CONFIG_FILE}" --version v1.2.3 \
        >"${WORK_DIR}/${name}.out" 2>&1 </dev/null; then
    fail "${name}: deploy.sh partial run exited non-zero"
    sed 's/^/    /' "${WORK_DIR}/${name}.out"
    return
  fi
  if ! grep -q "TEST_HARNESS_REACHED_END" "${WORK_DIR}/${name}.out"; then
    fail "${name}: harness did not reach the end marker (script exited early)"
    sed 's/^/    /' "${WORK_DIR}/${name}.out"
    return
  fi
  pass
}

assert_called() {
  local name="$1" log="$2" pattern="$3"
  if grep -qF -- "${pattern}" "${log}"; then
    pass
  else
    fail "${name}: expected a call matching '${pattern}'"
    echo "    --- call log ---"
    sed 's/^/    /' "${log}"
  fi
}

assert_not_called() {
  local name="$1" log="$2" pattern="$3"
  if grep -qF -- "${pattern}" "${log}"; then
    fail "${name}: unexpected call matching '${pattern}'"
    echo "    --- call log ---"
    sed 's/^/    /' "${log}"
  else
    pass
  fi
}

# Asserts the first call matching $pattern_a appears before the first call
# matching $pattern_b in the log (line-number order).
assert_before() {
  local name="$1" log="$2" pattern_a="$3" pattern_b="$4"
  local line_a line_b
  line_a="$(grep -nF -- "${pattern_a}" "${log}" | head -1 | cut -d: -f1)"
  line_b="$(grep -nF -- "${pattern_b}" "${log}" | head -1 | cut -d: -f1)"
  if [[ -z "${line_a}" || -z "${line_b}" ]]; then
    fail "${name}: could not locate both '${pattern_a}' and '${pattern_b}' to compare ordering"
    return
  fi
  if [[ "${line_a}" -lt "${line_b}" ]]; then
    pass
  else
    fail "${name}: expected '${pattern_a}' (line ${line_a}) before '${pattern_b}' (line ${line_b})"
  fi
}

echo "Using deploy.sh: ${DEPLOY_SH}"
echo

# ---------------------------------------------------------------------------
# Scenario 1: fresh deploy — no VM, no firewall rule yet.
# Expect: tag applied at instance creation; rule created already scoped with
# --target-tags. No add-tags/update calls (nothing pre-existing to fix up).
# ---------------------------------------------------------------------------
run_scenario "fresh-deploy" false false ""
LOG="${WORK_DIR}/fresh-deploy.log"
assert_not_called "fresh-deploy" "${LOG}" "compute instances add-tags"
assert_called "fresh-deploy" "${LOG}" "compute firewall-rules create ${FW_RULE}"
assert_called "fresh-deploy" "${LOG}" "--target-tags=${HUB_TAG}"
assert_not_called "fresh-deploy" "${LOG}" "compute firewall-rules update"
assert_called "fresh-deploy" "${LOG}" "compute instances create scion-hub-my-hub"
assert_called "fresh-deploy" "${LOG}" "--tags=${HUB_TAG}"
echo

# ---------------------------------------------------------------------------
# Scenario 2: re-run against a pre-fix deploy — VM and firewall rule already
# exist, but the rule has no target tags (the bug) and the VM has no tag.
# Expect: VM gets tagged, THEN the rule is narrowed in place. Never the
# other order — a re-run must not leave a window where the rule targets a
# tag the VM lacks.
# ---------------------------------------------------------------------------
run_scenario "rerun-untagged" true true ""
LOG="${WORK_DIR}/rerun-untagged.log"
assert_called "rerun-untagged" "${LOG}" "compute instances add-tags scion-hub-my-hub"
assert_called "rerun-untagged" "${LOG}" "compute firewall-rules update ${FW_RULE}"
assert_called "rerun-untagged" "${LOG}" "--target-tags=${HUB_TAG}"
assert_not_called "rerun-untagged" "${LOG}" "compute firewall-rules create"
assert_not_called "rerun-untagged" "${LOG}" "compute instances create"
assert_before "rerun-untagged" "${LOG}" "compute instances add-tags" "compute firewall-rules update"
echo

# ---------------------------------------------------------------------------
# Scenario 3: idempotent re-run — VM and firewall rule already exist, and
# the rule is already scoped to the hub tag from a prior (fixed) run.
# Expect: add-tags is still called (idempotent, harmless), but the rule is
# left alone — no update, no create.
# ---------------------------------------------------------------------------
run_scenario "idempotent-rerun" true true "${HUB_TAG}"
LOG="${WORK_DIR}/idempotent-rerun.log"
assert_called "idempotent-rerun" "${LOG}" "compute instances add-tags scion-hub-my-hub"
assert_not_called "idempotent-rerun" "${LOG}" "compute firewall-rules update"
assert_not_called "idempotent-rerun" "${LOG}" "compute firewall-rules create"
assert_not_called "idempotent-rerun" "${LOG}" "compute instances create"
echo

echo "=========================================="
echo "Passed: ${PASS_COUNT}  Failed: ${FAIL_COUNT}"
if [[ "${FAIL_COUNT}" -gt 0 ]]; then
  exit 1
fi
