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

# GCS state helper for Step 4 of the GKE CD pipeline (runs in gcr.io/cloud-builders/gcloud
# before and after hashicorp/terraform:1.9.8, which is an Alpine image without gcloud):
#   fetch-tfvars    Download gs://$_STATE_BUCKET/$_SHARED_PREFIX/hubs/$_HUB_NAME/hub.tfvars
#   write-deployed  Generate and upload deployed.json after a converged terraform apply

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
source "${SCRIPT_DIR}/lib.sh"

ACTION="${1:-}"
case "${ACTION}" in
  fetch-tfvars|write-deployed) ;;
  *) die "Usage: $(basename "$0") <fetch-tfvars | write-deployed>" ;;
esac

WORKSPACE_DIR="${WORKSPACE_DIR:-/workspace}"

if [[ -f "${WORKSPACE_DIR}/SKIP_DEPLOY" ]]; then
  log_info "Skipping gcs-hub-state (${ACTION}; SKIP_DEPLOY present)."
  exit 0
fi

validate_cd_common_inputs

STATE_BASE_URI="gs://${_STATE_BUCKET}/${_SHARED_PREFIX}/hubs/${_HUB_NAME}"

if [[ "${ACTION}" == "fetch-tfvars" ]]; then
  TFVARS_URI="${STATE_BASE_URI}/hub.tfvars"
  TFVARS_OUT="${WORKSPACE_DIR}/hub.tfvars"
  rm -f -- "${TFVARS_OUT}"
  log_info "Downloading ${TFVARS_URI}..."
  if ! CLOUDSDK_CORE_SHOW_STRUCTURED_LOGS=never CLOUDSDK_CORE_CONSOLE_LOG_FORMAT=standard \
    gcloud storage cp "${TFVARS_URI}" "${TFVARS_OUT}" --verbosity=error; then
    die "Failed to download ${TFVARS_URI}. Upload your filled-in hub.tfvars (see deploy/cd/hub.tfvars.example and deploy/cd/README.md) before running the CD pipeline."
  fi
  chmod 0600 "${TFVARS_OUT}"
  [[ -s "${TFVARS_OUT}" ]] || die "Downloaded ${TFVARS_URI} is empty."
  log_info "Downloaded hub.tfvars (${TFVARS_URI})."
  exit 0
fi

# write-deployed
[[ -f "${WORKSPACE_DIR}/build.env" ]] \
  || die "Missing ${WORKSPACE_DIR}/build.env"
[[ -f "${WORKSPACE_DIR}/hub-digest.env" ]] \
  || die "Missing ${WORKSPACE_DIR}/hub-digest.env"

TARGET_TAG=""
TARGET_VERSION_DATE=""
COMMIT_SHA=""
SHORT_SHA=""
BUILD_TIME=""
SOURCE_DOWNGRADE_FLOOR=""
HUB_IMAGE_DIGEST=""
if [[ -f "${WORKSPACE_DIR}/target.env" ]]; then
  # shellcheck disable=SC1091
  source "${WORKSPACE_DIR}/target.env"
fi
# shellcheck disable=SC1091
source "${WORKSPACE_DIR}/build.env"
# shellcheck disable=SC1091
source "${WORKSPACE_DIR}/hub-digest.env"

[[ "${HUB_IMAGE_DIGEST:-}" =~ ^sha256:[0-9a-f]{64}$ ]] \
  || die "Invalid HUB_IMAGE_DIGEST '${HUB_IMAGE_DIGEST:-}'"

DEPLOYED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
VERSION_DATE="${TARGET_VERSION_DATE:-${BUILD_TIME:-${DEPLOYED_AT}}}"
TF_OUTPUTS_FILE="${WORKSPACE_DIR}/tf-outputs.json"
DEPLOYED_JSON_FILE="${WORKSPACE_DIR}/deployed.json"
DEPLOYED_URI="${STATE_BASE_URI}/deployed.json"

hub_installed_str="$(
  python3 - \
    "${DEPLOYED_JSON_FILE}" \
    "${TARGET_TAG}" \
    "${VERSION_DATE}" \
    "${COMMIT_SHA}" \
    "${SHORT_SHA}" \
    "${HUB_IMAGE_DIGEST}" \
    "${SOURCE_DOWNGRADE_FLOOR:-}" \
    "${BUILD_ID:-}" \
    "${DEPLOYED_AT}" \
    "${TF_OUTPUTS_FILE}" <<'PY'
import json
import os
import sys

(
    out_path,
    version,
    version_date,
    commit,
    short_sha,
    digest,
    floor,
    build_id,
    deployed_at,
    tf_outputs_path,
) = sys.argv[1:11]

hub_installed = True
public_url = ""
if os.path.isfile(tf_outputs_path):
    with open(tf_outputs_path, encoding="utf-8") as f:
        tf_out = json.load(f)
    if isinstance(tf_out, dict):
        inst_entry = tf_out.get("hub_installed")
        if isinstance(inst_entry, dict) and isinstance(inst_entry.get("value"), bool):
            hub_installed = inst_entry["value"]
        url_entry = tf_out.get("public_url")
        if isinstance(url_entry, dict) and isinstance(url_entry.get("value"), str):
            public_url = url_entry["value"]

payload = {
    "version": version,
    "version_date": version_date,
    "commit": commit,
    "short_sha": short_sha,
    "hub_image_digest": digest,
    "hub_installed": hub_installed,
    "downgrade_floor": floor,
    "build_id": build_id,
    "deployed_at": deployed_at,
}
if public_url:
    payload["public_url"] = public_url

with open(out_path, "w", encoding="utf-8") as f:
    json.dump(payload, f, indent=2)
    f.write("\n")

print("true" if hub_installed else "false")
PY
)"

log_info "Uploading ${DEPLOYED_URI}..."
CLOUDSDK_CORE_SHOW_STRUCTURED_LOGS=never CLOUDSDK_CORE_CONSOLE_LOG_FORMAT=standard \
  gcloud storage cp "${DEPLOYED_JSON_FILE}" "${DEPLOYED_URI}" \
  --content-type=application/json --verbosity=error

if [[ "${hub_installed_str}" != "true" ]]; then
  log_warn "Terraform apply completed with hub_installed=false (iap_oauth_client_id is unset in hub.tfvars)."
  log_warn "Discover the Google-managed IAP OAuth client ID on ${_HUB_NAME}-hub-backend, set iap_oauth_client_id in ${STATE_BASE_URI}/hub.tfvars, and re-run this trigger to install the Hub Helm release."
else
  log_info "Deployment recorded in ${DEPLOYED_URI}: ${TARGET_TAG} (${HUB_IMAGE_DIGEST})."
fi
