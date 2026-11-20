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

# Step 2 (checkout-release-tag):
#   1. Short-circuits cleanly if $WORKSPACE_DIR/SKIP_DEPLOY is present.
#   2. Shallow-clones _REPO_URL at TARGET_TAG into $WORKSPACE_DIR/release-src.
#   3. Verifies HEAD is an exact tag match (`git describe --tags --exact-match`
#      and refs/tags/$TARGET_TAG^{commit} == HEAD).
#   4. Writes COMMIT_SHA, SHORT_SHA, BUILD_TIME, TARGET_TAG, and
#      SOURCE_DOWNGRADE_FLOOR to $WORKSPACE_DIR/build.env.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=SCRIPTDIR/lib.sh
source "${SCRIPT_DIR}/lib.sh"

WORKSPACE_DIR="${WORKSPACE_DIR:-/workspace}"
RELEASE_SRC_DIR="${RELEASE_SRC_DIR:-${WORKSPACE_DIR}/release-src}"
REPO_URL="${_REPO_URL:-https://github.com/GoogleCloudPlatform/scion.git}"

if [[ -f "${WORKSPACE_DIR}/SKIP_DEPLOY" ]]; then
  log_info "Skipping checkout-release-tag (SKIP_DEPLOY present)."
  exit 0
fi

[[ -f "${WORKSPACE_DIR}/target.env" ]] \
  || die "Missing ${WORKSPACE_DIR}/target.env (run resolve-and-check-noop.sh first)"

# shellcheck disable=SC1091
source "${WORKSPACE_DIR}/target.env"

[[ -n "${TARGET_TAG:-}" ]] || die "TARGET_TAG is empty in ${WORKSPACE_DIR}/target.env"
case "${TARGET_TAG}" in
  -*) die "TARGET_TAG must not start with '-' (got '${TARGET_TAG}')" ;;
esac

rm -rf -- "${RELEASE_SRC_DIR}" "${WORKSPACE_DIR}/build.env"

log_info "Cloning ${REPO_URL} at tag '${TARGET_TAG}' into ${RELEASE_SRC_DIR}..."
clone_err_file="$(mktemp "${TMPDIR:-/tmp}/scion-clone-err.XXXXXX")"
if ! GIT_TERMINAL_PROMPT=0 git clone --depth 1 --branch "${TARGET_TAG}" "${REPO_URL}" "${RELEASE_SRC_DIR}" 2>"${clone_err_file}"; then
  clone_err="$(cat "${clone_err_file}")"
  rm -f -- "${clone_err_file}"
  printf '%s\n' "${clone_err}" >&2
  if [[ "${TARGET_TAG}" == nightly-* ]]; then
    die "Failed to clone tag '${TARGET_TAG}' from ${REPO_URL}. Note: nightly-YYYYMMDD git tags are pruned after 14 days (docs/release-process.md); check LATEST.json or choose a current release tag."
  fi
  die "Failed to clone tag '${TARGET_TAG}' from ${REPO_URL}."
fi
rm -f -- "${clone_err_file}"

# Verify HEAD is on an exact git tag AND specifically refs/tags/${TARGET_TAG}.
described_tag=""
if ! described_tag="$(git -C "${RELEASE_SRC_DIR}" describe --tags --exact-match 2>/dev/null)"; then
  die "Ref '${TARGET_TAG}' in ${REPO_URL} is not an exact git tag (git describe --tags --exact-match failed)."
fi

COMMIT_SHA="$(git -C "${RELEASE_SRC_DIR}" rev-parse HEAD)"
TAG_COMMIT_SHA="$(git -C "${RELEASE_SRC_DIR}" rev-parse -q --verify "refs/tags/${TARGET_TAG}^{commit}" 2>/dev/null || true)"
[[ -n "${TAG_COMMIT_SHA}" && "${TAG_COMMIT_SHA}" == "${COMMIT_SHA}" ]] \
  || die "Checked-out HEAD (${COMMIT_SHA}) does not match refs/tags/${TARGET_TAG} (described='${described_tag}')."

SHORT_SHA="$(git -C "${RELEASE_SRC_DIR}" rev-parse --short HEAD)"
[[ "${COMMIT_SHA}" =~ ^[0-9a-f]{40}$ ]] || die "Unexpected COMMIT_SHA '${COMMIT_SHA}'"
[[ "${SHORT_SHA}" =~ ^[0-9a-f]{7,40}$ ]] || die "Unexpected SHORT_SHA '${SHORT_SHA}'"

# Sanity-check required release files exist before starting long image builds.
[[ -f "${RELEASE_SRC_DIR}/Dockerfile" ]] \
  || die "Tag '${TARGET_TAG}' does not contain repo-root Dockerfile"
[[ -d "${RELEASE_SRC_DIR}/deploy/terraform/configurations/hub-gke" ]] \
  || die "Tag '${TARGET_TAG}' does not contain deploy/terraform/configurations/hub-gke"

# Derive deterministic UTC BUILD_TIME from commit epoch (matching image-build/scripts/build-images.sh).
commit_epoch="$(git --no-pager -C "${RELEASE_SRC_DIR}" log -1 --no-show-signature --no-color --format=%ct 2>/dev/null || true)"
BUILD_TIME=""
if [[ "${commit_epoch}" =~ ^[0-9]+$ ]]; then
  BUILD_TIME="$(date -u -d "@${commit_epoch}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
    || date -u -r "${commit_epoch}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || true)"
fi
if [[ -z "${BUILD_TIME}" ]]; then
  BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
fi

SOURCE_DOWNGRADE_FLOOR=""
if [[ -f "${RELEASE_SRC_DIR}/pkg/version/downgrade_floor.go" ]]; then
  SOURCE_DOWNGRADE_FLOOR="$(sed -n 's/^[[:space:]]*const[[:space:]]*DowngradeFloor[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' \
    "${RELEASE_SRC_DIR}/pkg/version/downgrade_floor.go" | head -n 1)"
fi

{
  format_env_kv "TARGET_TAG" "${TARGET_TAG}"
  format_env_kv "COMMIT_SHA" "${COMMIT_SHA}"
  format_env_kv "SHORT_SHA" "${SHORT_SHA}"
  format_env_kv "BUILD_TIME" "${BUILD_TIME}"
  format_env_kv "SOURCE_DOWNGRADE_FLOOR" "${SOURCE_DOWNGRADE_FLOOR}"
} > "${WORKSPACE_DIR}/build.env"

log_info "Checked out ${TARGET_TAG} @ ${COMMIT_SHA} (short=${SHORT_SHA}, build_time=${BUILD_TIME}, floor=${SOURCE_DOWNGRADE_FLOOR:-none})."
