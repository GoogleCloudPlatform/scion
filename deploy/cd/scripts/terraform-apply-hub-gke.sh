#!/bin/sh
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

# Step 4 (terraform-apply-hub-gke):
#   Written in strict POSIX /bin/sh so it executes natively inside the Alpine-based
#   hashicorp/terraform:1.9.8 container image (which acquires a fresh 60-minute
#   OAuth token from the GCE metadata server for the google, kubernetes, and helm
#   providers after long image builds).
#
#   1. Short-circuits cleanly if $WORKSPACE_DIR/SKIP_DEPLOY is present.
#   2. Ensures $WORKSPACE_DIR/hub.tfvars is present (pre-fetched in Cloud Build by
#      gcs-hub-state.sh fetch-tfvars, or fetched directly when gcloud is on PATH).
#   3. Runs terraform init (-lockfile=readonly), plan (-out=hub.tfplan), apply,
#      and post-apply plan -detailed-exitcode convergence verification.
#   4. Captures terraform output -json to $WORKSPACE_DIR/tf-outputs.json (and writes
#      deployed.json via gcs-hub-state.sh unless SKIP_WRITE_DEPLOYED=true).

set -eu

SCRIPT_DIR="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)"
WORKSPACE_DIR="${WORKSPACE_DIR:-/workspace}"
RELEASE_SRC_DIR="${RELEASE_SRC_DIR:-${WORKSPACE_DIR}/release-src}"

log_info() {
  printf '[scion-cd] %s\n' "$*"
}

die() {
  printf '[scion-cd] ERROR: %s\n' "$*" >&2
  exit 1
}

if [ -f "${WORKSPACE_DIR}/SKIP_DEPLOY" ]; then
  log_info "Skipping terraform-apply-hub-gke (SKIP_DEPLOY present)."
  exit 0
fi

[ -n "${_STATE_BUCKET:-}" ] || die "_STATE_BUCKET is required"
[ -n "${_SHARED_PREFIX:-}" ] || die "_SHARED_PREFIX is required"
[ -n "${_HUB_NAME:-}" ] || die "_HUB_NAME is required"

case "${_HUB_NAME}" in
  "${_SHARED_PREFIX}-"*) ;;
  *) die "_HUB_NAME ('${_HUB_NAME}') must start with '${_SHARED_PREFIX}-'" ;;
esac

[ -f "${WORKSPACE_DIR}/build.env" ] || die "Missing ${WORKSPACE_DIR}/build.env"
[ -f "${WORKSPACE_DIR}/hub-digest.env" ] || die "Missing ${WORKSPACE_DIR}/hub-digest.env"

# shellcheck disable=SC1091
. "${WORKSPACE_DIR}/build.env"
# shellcheck disable=SC1091
. "${WORKSPACE_DIR}/hub-digest.env"

printf '%s\n' "${HUB_IMAGE_DIGEST:-}" | grep -qE '^sha256:[0-9a-f]{64}$' \
  || die "HUB_IMAGE_DIGEST '${HUB_IMAGE_DIGEST:-}' is not sha256:<64 lowercase hex>"

TFVARS_FILE="${WORKSPACE_DIR}/hub.tfvars"
if [ ! -f "${TFVARS_FILE}" ]; then
  if command -v bash >/dev/null 2>&1 && command -v gcloud >/dev/null 2>&1; then
    bash "${SCRIPT_DIR}/gcs-hub-state.sh" fetch-tfvars
  else
    die "Missing ${TFVARS_FILE}; run gcs-hub-state.sh fetch-tfvars before terraform-apply-hub-gke.sh"
  fi
fi

TF_DIR="${RELEASE_SRC_DIR}/deploy/terraform/configurations/hub-gke"
[ -d "${TF_DIR}" ] || die "Terraform configuration directory not found: ${TF_DIR}"

STATE_PREFIX="${_SHARED_PREFIX}/hubs/${_HUB_NAME}"
PLAN_FILE="${WORKSPACE_DIR}/hub.tfplan"
OUTPUTS_FILE="${WORKSPACE_DIR}/tf-outputs.json"

rm -f "${PLAN_FILE}" "${OUTPUTS_FILE}"
export TF_IN_AUTOMATION="true"

log_info "Initializing Terraform for ${_HUB_NAME} (gs://${_STATE_BUCKET}/${STATE_PREFIX})..."
terraform -chdir="${TF_DIR}" init \
  -input=false \
  -lockfile=readonly \
  -backend-config="bucket=${_STATE_BUCKET}" \
  -backend-config="prefix=${STATE_PREFIX}"

log_info "Planning hub-gke deployment (${TARGET_TAG:-unknown}, digest=${HUB_IMAGE_DIGEST})..."
terraform -chdir="${TF_DIR}" plan \
  -input=false \
  -var-file="${TFVARS_FILE}" \
  -var="hub_image_digest=${HUB_IMAGE_DIGEST}" \
  -out="${PLAN_FILE}"

log_info "Applying saved Terraform plan..."
terraform -chdir="${TF_DIR}" apply \
  -input=false \
  "${PLAN_FILE}"
rm -f "${PLAN_FILE}"

log_info "Running post-apply Terraform convergence check (-detailed-exitcode)..."
conv_rc=0
terraform -chdir="${TF_DIR}" plan \
  -input=false \
  -detailed-exitcode \
  -var-file="${TFVARS_FILE}" \
  -var="hub_image_digest=${HUB_IMAGE_DIGEST}" || conv_rc=$?

if [ "${conv_rc}" -eq 2 ]; then
  die "Post-apply Terraform convergence check failed (exit code 2: non-empty diff remains after apply)."
elif [ "${conv_rc}" -ne 0 ]; then
  die "Post-apply Terraform convergence plan failed with exit code ${conv_rc}."
fi

terraform -chdir="${TF_DIR}" output -json > "${OUTPUTS_FILE}"
log_info "Terraform apply and post-apply convergence check succeeded for ${_HUB_NAME}."

if [ "${SKIP_WRITE_DEPLOYED:-false}" != "true" ]; then
  if command -v bash >/dev/null 2>&1 && command -v gcloud >/dev/null 2>&1; then
    bash "${SCRIPT_DIR}/gcs-hub-state.sh" write-deployed
  else
    die "Cannot upload deployed.json without bash/gcloud; set SKIP_WRITE_DEPLOYED=true and run gcs-hub-state.sh write-deployed in a gcloud step"
  fi
fi
