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

# Step 3 (build-images):
#   Builds version-stamped Scion images from $WORKSPACE_DIR/release-src:
#     - scion-hub-gke (linux/amd64, --target hub-gke, tagged :$SHORT_SHA only,
#       extracting sha256:<64hex> from --metadata-file into hub-digest.env)
#     - core-base (with content+platform hash caching on image-build/core-base + image-build/lib)
#     - scion-base (stamped with VERSION, GIT_COMMIT, BUILD_TIME)
#     - all catalog harnesses discovered under harnesses/*/Dockerfile
#
# Supports two phases so Cloud Build can run gcloud/python3 setup in
# gcr.io/cloud-builders/gcloud and docker buildx builds in
# gcr.io/cloud-builders/docker:
#   --phase prepare  verify registry, install cred helper, check AR tags & retag cached core-base
#   --phase build    verify cred helper, run docker buildx builds, record HUB_IMAGE_DIGEST
#   --phase all      run both prepare and build sequentially (default)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CD_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${CD_DIR}/../.." && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
source "${SCRIPT_DIR}/lib.sh"

PHASE="all"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --phase)
      [[ $# -ge 2 ]] || die "--phase requires an argument (prepare | build | all)"
      PHASE="$2"
      shift 2
      ;;
    *)
      die "Unknown argument: $1 (expected --phase prepare|build|all)"
      ;;
  esac
done

case "${PHASE}" in
  prepare|build|all) ;;
  *) die "Invalid --phase '${PHASE}' (expected prepare | build | all)" ;;
esac

WORKSPACE_DIR="${WORKSPACE_DIR:-/workspace}"
RELEASE_SRC_DIR="${RELEASE_SRC_DIR:-${WORKSPACE_DIR}/release-src}"
REGISTRY="${_REGISTRY:-}"
REGISTRY="${REGISTRY%/}"
AGENT_PLATFORMS="${_AGENT_PLATFORMS:-linux/amd64}"
FORCE_REDEPLOY="${_FORCE_REDEPLOY:-false}"
CORE_BASE_CACHE="${CORE_BASE_CACHE:-true}"
AR_VERIFY_MAX_ATTEMPTS="${AR_VERIFY_MAX_ATTEMPTS:-6}"
AR_VERIFY_RETRY_DELAY="${AR_VERIFY_RETRY_DELAY:-20}"

if [[ -f "${WORKSPACE_DIR}/SKIP_DEPLOY" ]]; then
  log_info "Skipping build-images (${PHASE}; SKIP_DEPLOY present)."
  exit 0
fi

[[ -f "${WORKSPACE_DIR}/build.env" ]] \
  || die "Missing ${WORKSPACE_DIR}/build.env (run checkout-release-tag.sh first)"

TARGET_TAG=""
COMMIT_SHA=""
SHORT_SHA=""
BUILD_TIME=""
# shellcheck disable=SC1091
source "${WORKSPACE_DIR}/build.env"

validate_registry_path "${REGISTRY}" "${_SHARED_PREFIX:-}"
validate_agent_platforms "${AGENT_PLATFORMS}"
validate_bool_flag "_FORCE_REDEPLOY" "${FORCE_REDEPLOY}"
validate_bool_flag "CORE_BASE_CACHE" "${CORE_BASE_CACHE}"

# Locate helper scripts in release-src, falling back to repo root.
find_image_script() {
  local name="$1"
  if [[ -x "${RELEASE_SRC_DIR}/image-build/scripts/${name}" ]]; then
    printf '%s\n' "${RELEASE_SRC_DIR}/image-build/scripts/${name}"
  elif [[ -x "${REPO_ROOT}/image-build/scripts/${name}" ]]; then
    printf '%s\n' "${REPO_ROOT}/image-build/scripts/${name}"
  else
    die "Required helper script not found: image-build/scripts/${name}"
  fi
}

# Check whether an Artifact Registry Docker image already carries <tag>.
# Uses `gcloud artifacts docker tags list` (needs only AR reader/writer; avoids
# `images describe` which requires containeranalysis.occurrences.list).
# Returns 0 if tag exists, 1 if definitely absent, dies on permission/API error.
ar_has_tag() {
  local pkg_name="$1" want_tag="$2"
  local image="${REGISTRY}/${pkg_name}"
  local out_f err_f rc=0
  out_f="$(mktemp "${TMPDIR:-/tmp}/ar-tags-out.XXXXXX")"
  err_f="$(mktemp "${TMPDIR:-/tmp}/ar-tags-err.XXXXXX")"

  CLOUDSDK_CORE_SHOW_STRUCTURED_LOGS=never CLOUDSDK_CORE_CONSOLE_LOG_FORMAT=standard \
    gcloud artifacts docker tags list "${image}" --format='value(tag)' --user-output-enabled \
    --verbosity=error >"${out_f}" 2>"${err_f}" </dev/null || rc=$?

  local err
  err="$(cat "${err_f}")"
  if [[ ${rc} -ne 0 ]]; then
    rm -f -- "${out_f}" "${err_f}"
    if grep -qiE 'PERMISSION_DENIED|unauthori[sz]ed|forbidden|credential|HTTPError 40[13]' <<<"${err}"; then
      printf '%s\n' "${err}" >&2
      die "Artifact Registry tag check failed for ${image}"
    fi
    if grep -qE 'NOT_FOUND' <<<"${err}"; then
      return 1
    fi
    printf '%s\n' "${err}" >&2
    die "Artifact Registry tag lookup failed for ${image}"
  fi

  local found=1 line
  while IFS= read -r line || [[ -n "${line}" ]]; do
    line="${line%$'\r'}"
    [[ -n "${line}" ]] || continue
    if [[ "${line}" == "${want_tag}" || "${line}" == */tags/"${want_tag}" ]]; then
      found=0
      break
    fi
  done <"${out_f}"

  rm -f -- "${out_f}" "${err_f}"
  return "${found}"
}

# Compute deterministic content+platform hash tag for core-base
# (hashing normalized _AGENT_PLATFORMS + image-build/core-base + image-build/lib).
compute_core_base_cache_tag() {
  python3 - "${RELEASE_SRC_DIR}" "${AGENT_PLATFORMS}" <<'PY'
import hashlib
import os
import sys

src_root, platforms_raw = sys.argv[1:3]
platforms = ",".join(sorted({p.strip() for p in platforms_raw.split(",") if p.strip()}))
h = hashlib.sha256()
h.update(f"platforms={platforms}\n".encode("utf-8"))

for rel_dir in ("image-build/core-base", "image-build/lib"):
    abs_dir = os.path.join(src_root, rel_dir)
    if not os.path.isdir(abs_dir):
        continue
    entries = []
    for root, dirs, files in os.walk(abs_dir):
        dirs.sort()
        files.sort()
        for fname in files:
            full = os.path.join(root, fname)
            rel = os.path.relpath(full, src_root).replace(os.sep, "/")
            entries.append((rel, full))
    entries.sort(key=lambda x: x[0])
    for rel, full in entries:
        h.update(f"file:{rel}\n".encode("utf-8"))
        if os.path.islink(full):
            h.update(f"symlink:{os.readlink(full)}\n".encode("utf-8"))
        else:
            with open(full, "rb") as f:
                while True:
                    chunk = f.read(65536)
                    if not chunk:
                        break
                    h.update(chunk)
        h.update(b"\0")

print("cache-" + h.hexdigest()[:24])
PY
}

# Discover catalog harnesses from release-src (targets.sh if present, else find).
discover_release_harnesses() {
  if [[ -f "${RELEASE_SRC_DIR}/image-build/scripts/lib/targets.sh" ]]; then
    (
      REPO_ROOT="${RELEASE_SRC_DIR}"
      IMAGE_BUILD_DIR="${RELEASE_SRC_DIR}/image-build"
      export REPO_ROOT IMAGE_BUILD_DIR
      # shellcheck disable=SC1091
      source "${RELEASE_SRC_DIR}/image-build/scripts/lib/targets.sh"
      LC_ALL=C discover_harness_names
    )
    return 0
  fi
  local harness_root="${RELEASE_SRC_DIR}/harnesses"
  [[ -d "${harness_root}" ]] || return 0
  find "${harness_root}" -mindepth 2 -maxdepth 2 -name Dockerfile -print \
    | while IFS= read -r df; do basename "$(dirname "${df}")"; done \
    | LC_ALL=C sort -u
}

verify_registry_with_retry() {
  local verify_script="$1"
  local max_attempts="${AR_VERIFY_MAX_ATTEMPTS}"
  local retry_delay="${AR_VERIFY_RETRY_DELAY}"

  [[ "${max_attempts}" =~ ^[1-9][0-9]*$ ]] \
    || die "AR_VERIFY_MAX_ATTEMPTS must be a positive integer (got '${max_attempts}')"
  [[ "${retry_delay}" =~ ^[0-9]+$ ]] \
    || die "AR_VERIFY_RETRY_DELAY must be a non-negative integer (got '${retry_delay}')"

  local attempt=1 out="" rc=0
  while true; do
    rc=0
    out="$(bash "${verify_script}" "${REGISTRY}" 2>&1)" || rc=$?
    if [[ ${rc} -eq 0 ]]; then
      printf '%s\n' "${out}"
      return 0
    fi

    if grep -qiE 'Permission denied|PERMISSION_DENIED|Missing push permission|HTTPError 40[13]|forbidden|unauthori[sz]ed' <<<"${out}"; then
      if [[ ${attempt} -lt ${max_attempts} ]]; then
        log_warn "Artifact Registry permission check failed (attempt ${attempt}/${max_attempts}); newly bound GCP IAM policies can take ~90s to propagate — retrying in ${retry_delay}s..."
        if [[ ${retry_delay} -gt 0 ]]; then
          sleep "${retry_delay}"
        fi
        attempt=$((attempt + 1))
        continue
      fi
      printf '%s\n' "${out}" >&2
      die "Artifact Registry permission check for ${REGISTRY} failed after ${max_attempts} attempts."
    fi

    # Non-permission errors (e.g. NOT_FOUND) fail fast without retrying.
    printf '%s\n' "${out}" >&2
    exit "${rc}"
  done
}

run_prepare_phase() {
  local verify_script cred_script
  verify_script="$(find_image_script "verify-registry.sh")"
  cred_script="$(find_image_script "install-registry-cred-helper.sh")"

  log_info "Verifying Artifact Registry access (${REGISTRY})..."
  verify_registry_with_retry "${verify_script}"

  log_info "Installing fresh-token Docker credential helper for ${REGISTRY}..."
  bash "${cred_script}" "${REGISTRY}"

  local core_cache_tag
  core_cache_tag="$(compute_core_base_cache_tag)"

  local harness_list=()
  local h
  while IFS= read -r h; do
    [[ -n "${h}" ]] && harness_list+=("${h}")
  done < <(discover_release_harnesses)
  [[ ${#harness_list[@]} -gt 0 ]] \
    || die "No catalog harnesses (harnesses/*/Dockerfile) discovered in ${RELEASE_SRC_DIR}"

  local skip_images=""
  local prebuilt_hub_digest=""

  if [[ "${FORCE_REDEPLOY}" == "true" ]]; then
    log_info "_FORCE_REDEPLOY=true; all images will be rebuilt."
  else
    log_info "Checking Artifact Registry for already-built images (tag=${TARGET_TAG}, sha=${SHORT_SHA})..."

    # 1. scion-hub-gke:$SHORT_SHA
    if ar_has_tag "scion-hub-gke" "${SHORT_SHA}"; then
      local resolved_digest=""
      resolved_digest="$(
        CLOUDSDK_CORE_SHOW_STRUCTURED_LOGS=never CLOUDSDK_CORE_CONSOLE_LOG_FORMAT=standard \
          gcloud artifacts docker images list "${REGISTRY}" --include-tags \
          --filter="package=${REGISTRY}/scion-hub-gke AND tags:${SHORT_SHA}" \
          --format="value(version)" --user-output-enabled --verbosity=error 2>/dev/null \
          | tr -d '\r' | sed '/^[[:space:]]*$/d' || true
      )"
      if [[ "$(printf '%s\n' "${resolved_digest}" | grep -c . || true)" -eq 1 ]] \
        && [[ "${resolved_digest}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
        prebuilt_hub_digest="${resolved_digest}"
        skip_images="${skip_images} scion-hub-gke "
        log_info "Reusing existing ${REGISTRY}/scion-hub-gke:${SHORT_SHA} (${prebuilt_hub_digest})."
      else
        log_warn "Tag scion-hub-gke:${SHORT_SHA} exists, but could not uniquely resolve sha256 digest; will rebuild."
      fi
    fi

    # 2. core-base (:$TARGET_TAG + :$SHORT_SHA, or content-hash cache hit)
    if ar_has_tag "core-base" "${TARGET_TAG}" && ar_has_tag "core-base" "${SHORT_SHA}"; then
      skip_images="${skip_images} core-base "
      log_info "Reusing existing ${REGISTRY}/core-base:${TARGET_TAG} (+ :${SHORT_SHA})."
    elif [[ "${CORE_BASE_CACHE}" == "true" ]] && ar_has_tag "core-base" "${core_cache_tag}"; then
      log_info "Content-hash cache hit for core-base (${core_cache_tag}); tagging :${TARGET_TAG} and :${SHORT_SHA}..."
      gcloud artifacts docker tags add \
        "${REGISTRY}/core-base:${core_cache_tag}" \
        "${REGISTRY}/core-base:${TARGET_TAG}" --quiet
      gcloud artifacts docker tags add \
        "${REGISTRY}/core-base:${core_cache_tag}" \
        "${REGISTRY}/core-base:${SHORT_SHA}" --quiet
      skip_images="${skip_images} core-base "
    fi

    # 3. scion-base
    if ar_has_tag "scion-base" "${TARGET_TAG}" && ar_has_tag "scion-base" "${SHORT_SHA}"; then
      skip_images="${skip_images} scion-base "
      log_info "Reusing existing ${REGISTRY}/scion-base:${TARGET_TAG} (+ :${SHORT_SHA})."
    fi

    # 4. Catalog harnesses
    for h in "${harness_list[@]}"; do
      local img="scion-${h}"
      if ar_has_tag "${img}" "${TARGET_TAG}" && ar_has_tag "${img}" "${SHORT_SHA}"; then
        skip_images="${skip_images} ${img} "
        log_info "Reusing existing ${REGISTRY}/${img}:${TARGET_TAG} (+ :${SHORT_SHA})."
      fi
    done
  fi

  local harnesses_joined="${harness_list[*]}"
  {
    format_env_kv "CORE_BASE_CACHE_TAG" "${core_cache_tag}"
    format_env_kv "HARNESS_NAMES" "${harnesses_joined}"
    format_env_kv "SKIP_IMAGES" "${skip_images}"
    format_env_kv "PREBUILT_HUB_DIGEST" "${prebuilt_hub_digest}"
  } > "${WORKSPACE_DIR}/build-plan.env"

  log_info "Prepared image build plan (core_base_cache_tag=${core_cache_tag}, harnesses=${#harness_list[@]})."
}

# Parse sha256:<64hex> containerimage.digest from buildx --metadata-file JSON.
# Uses only grep/bash so it runs inside gcr.io/cloud-builders/docker without python3.
extract_metadata_digest() {
  local meta_file="$1"
  [[ -s "${meta_file}" ]] || die "Buildx metadata file missing or empty: ${meta_file}"

  local matches
  matches="$(grep -oE '"containerimage\.digest"[[:space:]]*:[[:space:]]*"[^"]*"' "${meta_file}" || true)"
  [[ -n "${matches}" ]] || die "No containerimage.digest found in ${meta_file}"
  local count
  count="$(printf '%s\n' "${matches}" | grep -c .)"
  [[ "${count}" -eq 1 ]] || die "Expected 1 containerimage.digest in ${meta_file}, got ${count}"

  local digest="${matches%\"}"
  digest="${digest##*\"}"
  [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || die "Invalid containerimage.digest '${digest}' in ${meta_file} (expected sha256:<64 lowercase hex>)"

  printf '%s\n' "${digest}"
}

image_is_skipped() {
  local name="$1"
  [[ "${SKIP_IMAGES:-}" == *" ${name} "* ]]
}

run_build_phase() {
  [[ -f "${WORKSPACE_DIR}/build-plan.env" ]] \
    || die "Missing ${WORKSPACE_DIR}/build-plan.env (run build-images.sh --phase prepare first)"

  CORE_BASE_CACHE_TAG=""
  HARNESS_NAMES=""
  SKIP_IMAGES=""
  PREBUILT_HUB_DIGEST=""
  # shellcheck disable=SC1091
  source "${WORKSPACE_DIR}/build-plan.env"

  local cred_script
  cred_script="$(find_image_script "install-registry-cred-helper.sh")"
  export PATH="${HOME}/bin:${PATH}"
  export DOCKER_CLI_EXPERIMENTAL="enabled"

  log_info "Verifying registry credential helper inside Docker builder..."
  bash "${cred_script}" --verify "${REGISTRY}"

  local -a harness_list=()
  IFS=' ' read -r -a harness_list <<< "${HARNESS_NAMES}"
  [[ ${#harness_list[@]} -gt 0 ]] || die "HARNESS_NAMES is empty in build-plan.env"

  # Check if any image actually needs building before bootstrapping buildx.
  local need_build="false"
  for img in scion-hub-gke core-base scion-base; do
    if ! image_is_skipped "${img}"; then
      need_build="true"
      break
    fi
  done
  if [[ "${need_build}" == "false" ]]; then
    for h in "${harness_list[@]}"; do
      if ! image_is_skipped "scion-${h}"; then
        need_build="true"
        break
      fi
    done
  fi

  if [[ "${need_build}" == "true" ]]; then
    log_info "Setting up and bootstrapping Docker Buildx builder..."
    if ! docker buildx use cdbuilder >/dev/null 2>&1; then
      docker buildx create --name cdbuilder --use
    fi
    docker buildx inspect --bootstrap
  else
    log_info "All images for ${TARGET_TAG} (${SHORT_SHA}) are already built in ${REGISTRY}."
  fi

  local hub_image_digest=""
  if image_is_skipped "scion-hub-gke"; then
    hub_image_digest="${PREBUILT_HUB_DIGEST:-}"
    [[ "${hub_image_digest}" =~ ^sha256:[0-9a-f]{64}$ ]] \
      || die "scion-hub-gke marked skipped, but PREBUILT_HUB_DIGEST '${hub_image_digest}' is invalid"
  else
    local meta_file="${WORKSPACE_DIR}/hub-gke-meta.json"
    rm -f -- "${meta_file}"
    log_info "Building ${REGISTRY}/scion-hub-gke:${SHORT_SHA} (linux/amd64)..."
    docker buildx build \
      --platform linux/amd64 \
      --target hub-gke \
      --build-arg "VERSION=${TARGET_TAG}" \
      --build-arg "GIT_COMMIT=${COMMIT_SHA}" \
      --build-arg "BUILD_TIME=${BUILD_TIME}" \
      --metadata-file "${meta_file}" \
      -t "${REGISTRY}/scion-hub-gke:${SHORT_SHA}" \
      -f "${RELEASE_SRC_DIR}/Dockerfile" \
      --pull \
      --push \
      "${RELEASE_SRC_DIR}"
    hub_image_digest="$(extract_metadata_digest "${meta_file}")"
    log_info "Built ${REGISTRY}/scion-hub-gke:${SHORT_SHA} -> ${hub_image_digest}"
  fi

  format_env_kv "HUB_IMAGE_DIGEST" "${hub_image_digest}" > "${WORKSPACE_DIR}/hub-digest.env"

  if ! image_is_skipped "core-base"; then
    local core_tags=(
      -t "${REGISTRY}/core-base:${SHORT_SHA}"
      -t "${REGISTRY}/core-base:${TARGET_TAG}"
    )
    if [[ "${CORE_BASE_CACHE}" == "true" && -n "${CORE_BASE_CACHE_TAG:-}" ]]; then
      core_tags+=(-t "${REGISTRY}/core-base:${CORE_BASE_CACHE_TAG}")
    fi
    log_info "Building ${REGISTRY}/core-base:${TARGET_TAG} (${AGENT_PLATFORMS})..."
    docker buildx build \
      --platform "${AGENT_PLATFORMS}" \
      "${core_tags[@]}" \
      -f "${RELEASE_SRC_DIR}/image-build/core-base/Dockerfile" \
      --pull \
      --push \
      "${RELEASE_SRC_DIR}/image-build"
  fi

  if ! image_is_skipped "scion-base"; then
    log_info "Building ${REGISTRY}/scion-base:${TARGET_TAG} (${AGENT_PLATFORMS})..."
    docker buildx build \
      --platform "${AGENT_PLATFORMS}" \
      --build-arg "BASE_IMAGE=${REGISTRY}/core-base:${SHORT_SHA}" \
      --build-arg "GIT_COMMIT=${COMMIT_SHA}" \
      --build-arg "VERSION=${TARGET_TAG}" \
      --build-arg "BUILD_TIME=${BUILD_TIME}" \
      -t "${REGISTRY}/scion-base:${SHORT_SHA}" \
      -t "${REGISTRY}/scion-base:${TARGET_TAG}" \
      -f "${RELEASE_SRC_DIR}/image-build/scion-base/Dockerfile" \
      --pull \
      --push \
      "${RELEASE_SRC_DIR}"
  fi

  for h in "${harness_list[@]}"; do
    local img="scion-${h}"
    if image_is_skipped "${img}"; then
      continue
    fi
    log_info "Building ${REGISTRY}/${img}:${TARGET_TAG} (${AGENT_PLATFORMS})..."
    docker buildx build \
      --platform "${AGENT_PLATFORMS}" \
      --build-arg "BASE_IMAGE=${REGISTRY}/scion-base:${SHORT_SHA}" \
      -t "${REGISTRY}/${img}:${SHORT_SHA}" \
      -t "${REGISTRY}/${img}:${TARGET_TAG}" \
      -f "${RELEASE_SRC_DIR}/harnesses/${h}/Dockerfile" \
      --pull \
      --push \
      "${RELEASE_SRC_DIR}/harnesses/${h}"
  done

  log_info "Image build step complete (HUB_IMAGE_DIGEST=${hub_image_digest})."
}

case "${PHASE}" in
  prepare)
    run_prepare_phase
    ;;
  build)
    run_build_phase
    ;;
  all)
    run_prepare_phase
    run_build_phase
    ;;
esac
