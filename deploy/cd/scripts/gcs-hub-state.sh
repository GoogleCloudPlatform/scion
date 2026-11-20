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
REGISTRY="${_REGISTRY:-}"
REGISTRY="${REGISTRY%/}"
if [[ -n "${REGISTRY}" ]]; then
  validate_registry_path "${REGISTRY}" "${_SHARED_PREFIX}"
fi

STATE_BASE_URI="gs://${_STATE_BUCKET}/${_SHARED_PREFIX}/hubs/${_HUB_NAME}"
TFVARS_URI="${STATE_BASE_URI}/hub.tfvars"
TFVARS_LOCAL="${WORKSPACE_DIR}/hub.tfvars"

if [[ "${ACTION}" == "fetch-tfvars" ]]; then
  rm -f -- "${TFVARS_LOCAL}"
  log_info "Downloading ${TFVARS_URI}..."
  if ! CLOUDSDK_CORE_SHOW_STRUCTURED_LOGS=never CLOUDSDK_CORE_CONSOLE_LOG_FORMAT=standard \
    gcloud storage cp "${TFVARS_URI}" "${TFVARS_LOCAL}" --verbosity=error; then
    die "Failed to download ${TFVARS_URI}. Upload your filled-in hub.tfvars (see deploy/cd/hub.tfvars.example and deploy/cd/README.md) before running the CD pipeline."
  fi
  chmod 0600 "${TFVARS_LOCAL}"
  [[ -s "${TFVARS_LOCAL}" ]] || die "Downloaded ${TFVARS_URI} is empty."

  if [[ -n "${REGISTRY}" ]]; then
    reg_err=""
    if ! reg_err="$(python3 - "${TFVARS_LOCAL}" "${REGISTRY}" <<'PY' 2>&1
import re
import sys

tfvars_path, reg = sys.argv[1:3]
reg = reg.rstrip("/")
kv = {}
str_assign = re.compile(r'^\s*([A-Za-z0-9_]+)\s*=\s*"([^"#\\]*)"\s*(?:(?:#|//).*)?$')
with open(tfvars_path, encoding="utf-8") as f:
    for raw_line in f:
        s = raw_line.strip()
        if not s or s.startswith("#") or s.startswith("//"):
            continue
        m = str_assign.match(raw_line)
        if m:
            kv[m.group(1)] = m.group(2)

proj = kv.get("project_id", "")
region = kv.get("region", "")
prefix = kv.get("shared_prefix", "")
if proj and region and prefix:
    expected = f"{region}-docker.pkg.dev/{proj}/{prefix}-scion"
    if reg != expected:
        raise SystemExit(
            f"_REGISTRY '{reg}' does not match '{expected}' derived from hub.tfvars "
            f"(region='{region}', project_id='{proj}', shared_prefix='{prefix}'; "
            f"configurations/hub-gke pulls scion-hub-gke from shared_lookup's repo_url)"
        )

img_reg = kv.get("image_registry", "").rstrip("/")
if img_reg and img_reg != reg:
    raise SystemExit(
        f"_REGISTRY '{reg}' does not match explicit image_registry='{img_reg}' in hub.tfvars"
    )
PY
    )"; then
      die "${reg_err}"
    fi
  fi

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

TFVARS_SHA256=""
if [[ -f "${TFVARS_LOCAL}" ]]; then
  TFVARS_SHA256="$(sha256_file "${TFVARS_LOCAL}")"
else
  tfvars_fallback_tmp="$(mktemp "${TMPDIR:-/tmp}/scion-tfvars-hash.XXXXXX")"
  if gcs_cat_if_exists "${TFVARS_URI}" "${tfvars_fallback_tmp}"; then
    TFVARS_SHA256="$(sha256_file "${tfvars_fallback_tmp}")"
  fi
  rm -f -- "${tfvars_fallback_tmp}"
fi

DEPLOYED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
VERSION_DATE="${TARGET_VERSION_DATE:-${BUILD_TIME:-${DEPLOYED_AT}}}"
TF_OUTPUTS_FILE="${WORKSPACE_DIR}/tf-outputs.json"
DEPLOYED_JSON_FILE="${WORKSPACE_DIR}/deployed.json"
DEPLOYED_URI="${STATE_BASE_URI}/deployed.json"

EFFECTIVE_DOWNGRADE_FLOOR="${SOURCE_DOWNGRADE_FLOOR:-}"
if [[ -n "${EFFECTIVE_DOWNGRADE_FLOOR}" ]]; then
  floor_vs_target="$(compare_release_refs "${TARGET_TAG}" "${EFFECTIVE_DOWNGRADE_FLOOR}" "${VERSION_DATE}" "")"
  case "${floor_vs_target}" in
    upgrade|switch-newer)
      log_warn "Source pkg/version.DowngradeFloor ('${EFFECTIVE_DOWNGRADE_FLOOR}') is newer than deployed tag '${TARGET_TAG}' (${floor_vs_target}); clamping deployed.json downgrade_floor to '${TARGET_TAG}'."
      EFFECTIVE_DOWNGRADE_FLOOR="${TARGET_TAG}"
      ;;
  esac
fi

hub_installed_str="$(
  python3 - \
    "${DEPLOYED_JSON_FILE}" \
    "${TARGET_TAG}" \
    "${VERSION_DATE}" \
    "${COMMIT_SHA}" \
    "${SHORT_SHA}" \
    "${HUB_IMAGE_DIGEST}" \
    "${TFVARS_SHA256}" \
    "${EFFECTIVE_DOWNGRADE_FLOOR}" \
    "${BUILD_ID:-}" \
    "${DEPLOYED_AT}" \
    "${TF_OUTPUTS_FILE}" \
    "${REGISTRY}" <<'PY'
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
    tfvars_sha256,
    floor,
    build_id,
    deployed_at,
    tf_outputs_path,
    registry,
) = sys.argv[1:13]

hub_installed = True
public_url = ""
hub_image_out = ""
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
        img_entry = tf_out.get("hub_image")
        if isinstance(img_entry, dict) and isinstance(img_entry.get("value"), str):
            hub_image_out = img_entry["value"]

if registry and hub_image_out:
    want_prefix = registry.rstrip("/") + "/scion-hub-gke@"
    if not hub_image_out.startswith(want_prefix):
        raise SystemExit(
            f"Terraform output hub_image='{hub_image_out}' does not match _REGISTRY='{registry}'"
        )

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
if tfvars_sha256:
    payload["tfvars_sha256"] = tfvars_sha256
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
