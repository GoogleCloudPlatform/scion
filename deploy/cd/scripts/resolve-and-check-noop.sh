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

# Step 1 (resolve-and-check-noop):
#   1. Validates CD pipeline substitutions.
#   2. Resolves target release tag from _VERSION_TAG (when non-empty) or from
#      LATEST.json on main for _CHANNEL (nightly | preview | stable).
#   3. Reads gs://$_STATE_BUCKET/$_SHARED_PREFIX/hubs/$_HUB_NAME/deployed.json
#      and default.tfstate; if already converged on TARGET_TAG (hub_installed == true
#      and hub_image ends with @${deployed.hub_image_digest}) and
#      _FORCE_REDEPLOY != "true", writes $WORKSPACE_DIR/SKIP_DEPLOY and exits 0.
#   4. Enforces downgrade protection against deployed.json unless
#      _ALLOW_DOWNGRADE == "true".

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
source "${SCRIPT_DIR}/lib.sh"

WORKSPACE_DIR="${WORKSPACE_DIR:-/workspace}"
CHANNEL="${_CHANNEL:-}"
VERSION_TAG="${_VERSION_TAG:-}"
FORCE_REDEPLOY="${_FORCE_REDEPLOY:-false}"
ALLOW_DOWNGRADE="${_ALLOW_DOWNGRADE:-false}"
AGENT_PLATFORMS="${_AGENT_PLATFORMS:-linux/amd64}"
REPO_URL="${_REPO_URL:-https://github.com/GoogleCloudPlatform/scion.git}"

mkdir -p "${WORKSPACE_DIR}"
rm -f -- "${WORKSPACE_DIR}/SKIP_DEPLOY" "${WORKSPACE_DIR}/target.env"

validate_cd_common_inputs
validate_registry_path "${_REGISTRY:-}"
validate_agent_platforms "${AGENT_PLATFORMS}"
validate_bool_flag "_FORCE_REDEPLOY" "${FORCE_REDEPLOY}"
validate_bool_flag "_ALLOW_DOWNGRADE" "${ALLOW_DOWNGRADE}"

case "${REPO_URL}" in
  ""|-*) die "_REPO_URL must be a valid repository URL (got '${REPO_URL}')" ;;
esac

TARGET_TAG=""
TARGET_VERSION_DATE=""

if [[ -n "${VERSION_TAG}" ]]; then
  is_valid_release_tag "${VERSION_TAG}" \
    || die "_VERSION_TAG '${VERSION_TAG}' is not a valid Scion release tag (expected vX.Y.Z, vX.Y.Z-preview.N, vX.Y.Z-rc.N, or nightly-YYYYMMDD)"
  TARGET_TAG="${VERSION_TAG}"
  log_info "Using explicit _VERSION_TAG=${TARGET_TAG} (overrides _CHANNEL='${CHANNEL}')."
else
  case "${CHANNEL}" in
    nightly|preview|stable) ;;
    "") die "Either _VERSION_TAG or _CHANNEL (nightly | preview | stable) must be set." ;;
    *)  die "Unsupported _CHANNEL '${CHANNEL}' (expected nightly | preview | stable | '')." ;;
  esac

  manifest_url="$(resolve_manifest_url "${REPO_URL}")"
  log_info "Fetching release manifest for channel '${CHANNEL}' from ${manifest_url}..."
  manifest_tmp="$(mktemp "${TMPDIR:-/tmp}/scion-latest.XXXXXX")"
  if ! curl -fsSL --max-time 20 "${manifest_url}" -o "${manifest_tmp}"; then
    rm -f -- "${manifest_tmp}"
    die "Failed to fetch LATEST.json from ${manifest_url}"
  fi

  parsed_manifest=""
  if ! parsed_manifest="$(python3 - "${manifest_tmp}" "${CHANNEL}" <<'PY'
import json
import sys

path, channel = sys.argv[1:3]
with open(path, encoding="utf-8") as f:
    data = json.load(f)
channels = data.get("channels")
if not isinstance(channels, dict) or channel not in channels:
    raise SystemExit(f"channel '{channel}' missing from LATEST.json")
entry = channels[channel]
if not isinstance(entry, dict):
    raise SystemExit(f"channel '{channel}' entry is malformed")
ver = str(entry.get("version") or "").strip()
date = str(entry.get("date") or "").strip()
if not ver:
    raise SystemExit(f"channel '{channel}' has empty version in LATEST.json")
print(ver)
print(date)
PY
  )"; then
    rm -f -- "${manifest_tmp}"
    die "Failed to resolve .channels.${CHANNEL}.version from LATEST.json"
  fi
  rm -f -- "${manifest_tmp}"

  TARGET_TAG="$(printf '%s\n' "${parsed_manifest}" | sed -n '1p')"
  TARGET_VERSION_DATE="$(printf '%s\n' "${parsed_manifest}" | sed -n '2p')"
  is_valid_release_tag "${TARGET_TAG}" \
    || die "LATEST.json .channels.${CHANNEL}.version '${TARGET_TAG}' is not a valid Scion release tag"
  log_info "Resolved channel '${CHANNEL}' -> ${TARGET_TAG}${TARGET_VERSION_DATE:+ (published ${TARGET_VERSION_DATE})}."
fi

STATE_BASE_URI="gs://${_STATE_BUCKET}/${_SHARED_PREFIX}/hubs/${_HUB_NAME}"
DEPLOYED_URI="${STATE_BASE_URI}/deployed.json"
TFSTATE_URI="${STATE_BASE_URI}/default.tfstate"

deployed_tmp="$(mktemp "${TMPDIR:-/tmp}/scion-deployed.XXXXXX")"
gcs_rc=0
gcs_cat_if_exists "${DEPLOYED_URI}" "${deployed_tmp}" || gcs_rc=$?

if [[ ${gcs_rc} -eq 2 ]]; then
  rm -f -- "${deployed_tmp}"
  die "Failed to read ${DEPLOYED_URI} from GCS (check bucket existence and storage.objects.get permission)"
fi

if [[ ${gcs_rc} -eq 1 ]]; then
  log_info "No existing ${DEPLOYED_URI} found; proceeding with initial deployment of ${TARGET_TAG}."
else
  deployed_fields=""
  if ! deployed_fields="$(python3 - "${deployed_tmp}" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as f:
    d = json.load(f)
if not isinstance(d, dict):
    raise SystemExit("deployed.json root must be an object")
print(str(d.get("version") or "").strip())
print(str(d.get("hub_image_digest") or "").strip())
print(str(d.get("version_date") or d.get("deployed_at") or "").strip())
print(str(d.get("downgrade_floor") or "").strip())
PY
  )"; then
    rm -f -- "${deployed_tmp}"
    die "Corrupt or unreadable ${DEPLOYED_URI}"
  fi
  rm -f -- "${deployed_tmp}"

  DEPLOYED_VERSION="$(printf '%s\n' "${deployed_fields}" | sed -n '1p')"
  DEPLOYED_DIGEST="$(printf '%s\n' "${deployed_fields}" | sed -n '2p')"
  DEPLOYED_DATE="$(printf '%s\n' "${deployed_fields}" | sed -n '3p')"
  DEPLOYED_FLOOR="$(printf '%s\n' "${deployed_fields}" | sed -n '4p')"

  log_info "Found deployed state: version='${DEPLOYED_VERSION}', digest='${DEPLOYED_DIGEST:-<none>}'."

  # Short-circuit check when target matches deployed version and _FORCE_REDEPLOY != true
  if [[ "${DEPLOYED_VERSION}" == "${TARGET_TAG}" ]]; then
    if [[ "${FORCE_REDEPLOY}" == "true" ]]; then
      log_info "_FORCE_REDEPLOY=true; bypassing no-op short-circuit for ${TARGET_TAG}."
    elif [[ "${DEPLOYED_DIGEST}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
      tfstate_tmp="$(mktemp "${TMPDIR:-/tmp}/scion-tfstate.XXXXXX")"
      tf_rc=0
      gcs_cat_if_exists "${TFSTATE_URI}" "${tfstate_tmp}" || tf_rc=$?
      if [[ ${tf_rc} -eq 2 ]]; then
        rm -f -- "${tfstate_tmp}"
        die "Failed to read ${TFSTATE_URI} from GCS"
      elif [[ ${tf_rc} -eq 1 ]]; then
        log_info "${TFSTATE_URI} does not exist yet; proceeding with deployment."
      else
        converged_status=""
        converged_status="$(python3 - "${tfstate_tmp}" "${DEPLOYED_DIGEST}" <<'PY'
import json
import sys

tf_path, want_digest = sys.argv[1:3]
try:
    with open(tf_path, encoding="utf-8") as f:
        st = json.load(f)
except Exception as exc:
    print(f"invalid_tfstate:{exc}")
    sys.exit(0)

outputs = st.get("outputs") if isinstance(st, dict) else None
if not isinstance(outputs, dict):
    print("missing_outputs")
    sys.exit(0)

hub_inst = (outputs.get("hub_installed") or {}).get("value")
hub_img = (outputs.get("hub_image") or {}).get("value")

if hub_inst is not True:
    print(f"hub_installed={hub_inst!r}")
elif not isinstance(hub_img, str) or not hub_img.endswith("@" + want_digest):
    print(f"hub_image_mismatch={hub_img!r}")
else:
    print("CONVERGED")
PY
        )"
        rm -f -- "${tfstate_tmp}"

        if [[ "${converged_status}" == "CONVERGED" ]]; then
          printf '%s\n' "${TARGET_TAG}" > "${WORKSPACE_DIR}/SKIP_DEPLOY"
          {
            format_env_kv "TARGET_TAG" "${TARGET_TAG}"
            format_env_kv "TARGET_VERSION_DATE" "${TARGET_VERSION_DATE}"
          } > "${WORKSPACE_DIR}/target.env"
          log_info "No-op: hub '${_HUB_NAME}' is already converged at ${TARGET_TAG} (${DEPLOYED_DIGEST}). Wrote ${WORKSPACE_DIR}/SKIP_DEPLOY."
          exit 0
        fi
        log_info "deployed.json matches ${TARGET_TAG}, but live tfstate is not converged (${converged_status}); proceeding with deployment."
      fi
    else
      log_info "deployed.json lacks a valid sha256 hub_image_digest; proceeding with deployment."
    fi
  elif [[ -n "${DEPLOYED_VERSION}" ]]; then
    # Downgrade guard against deployed.json version
    rel_class="$(compare_release_refs "${DEPLOYED_VERSION}" "${TARGET_TAG}" "${DEPLOYED_DATE}" "${TARGET_VERSION_DATE}")"
    case "${rel_class}" in
      same|upgrade|switch-newer)
        log_info "Version transition ${DEPLOYED_VERSION} -> ${TARGET_TAG} classified as '${rel_class}'."
        ;;
      downgrade|switch-older|unknown)
        if [[ "${ALLOW_DOWNGRADE}" != "true" ]]; then
          die "Target version '${TARGET_TAG}' is older than (or unorderable against) deployed version '${DEPLOYED_VERSION}' (classification: ${rel_class}). Rolling back across one-way database migrations or newer Terraform modules can break the hub. Pass _ALLOW_DOWNGRADE=true only after verifying schema compatibility (deployed downgrade_floor='${DEPLOYED_FLOOR:-unknown}')."
        fi
        log_warn "Permitting version transition ${DEPLOYED_VERSION} -> ${TARGET_TAG} (${rel_class}) because _ALLOW_DOWNGRADE=true."
        if [[ -n "${DEPLOYED_FLOOR}" ]]; then
          floor_class="$(compare_release_refs "${DEPLOYED_FLOOR}" "${TARGET_TAG}" "" "${TARGET_VERSION_DATE}")"
          case "${floor_class}" in
            downgrade|switch-older|unknown)
              log_warn "Target '${TARGET_TAG}' may be below deployed schema downgrade_floor '${DEPLOYED_FLOOR}' (${floor_class}); Hub pods may fail readiness if destructive DB migrations already ran."
              ;;
          esac
        fi
        ;;
    esac
  fi
fi

{
  format_env_kv "TARGET_TAG" "${TARGET_TAG}"
  format_env_kv "TARGET_VERSION_DATE" "${TARGET_VERSION_DATE}"
} > "${WORKSPACE_DIR}/target.env"

log_info "Target release ready for checkout: ${TARGET_TAG}"
