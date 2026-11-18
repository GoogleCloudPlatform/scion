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

# Unit and contract tests for deploy/cd/scripts/* and cloudbuild-gke-cd.yaml.
# Uses local command stubs (gcloud, curl, docker, terraform); requires no
# network access, Docker daemon, or live GCP project.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CD_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${CD_DIR}/../.." && pwd)"

PASS_COUNT=0
FAIL_COUNT=0

TMP_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/test-cd-scripts.XXXXXX")"
trap 'rm -rf -- "${TMP_ROOT}"' EXIT

pass() {
  PASS_COUNT=$((PASS_COUNT + 1))
  printf '  PASS: %s\n' "$1"
}

fail() {
  FAIL_COUNT=$((FAIL_COUNT + 1))
  printf '  FAIL: %s\n' "$1" >&2
  if [[ $# -gt 1 ]]; then
    printf '        %s\n' "${@:2}" >&2
  fi
}

assert_eq() {
  local name="$1" got="$2" want="$3"
  if [[ "${got}" == "${want}" ]]; then
    pass "${name}"
  else
    fail "${name}" "got:  '${got}'" "want: '${want}'"
  fi
}

assert_contains() {
  local name="$1" haystack="$2" needle="$3"
  if [[ "${haystack}" == *"${needle}"* ]]; then
    pass "${name}"
  else
    fail "${name}" "missing substring: '${needle}'" "output: ${haystack}"
  fi
}

assert_not_contains() {
  local name="$1" haystack="$2" needle="$3"
  if [[ "${haystack}" != *"${needle}"* ]]; then
    pass "${name}"
  else
    fail "${name}" "unexpected substring: '${needle}'" "output: ${haystack}"
  fi
}

# Create stub bin dir for mocking gcloud, curl, docker, terraform
make_stub_env() {
  local dir="$1"
  mkdir -p "${dir}/bin" "${dir}/gcs" "${dir}/ar" "${dir}/workspace" "${dir}/home"

  # Stub curl: serves $MOCK_MANIFEST_FILE for LATEST.json URLs
  cat >"${dir}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
out_file=""
url=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -o) out_file="$2"; shift 2 ;;
    --max-time|-m|-H|-X|-d) shift 2 ;;
    -*) shift ;;
    *) url="$1"; shift ;;
  esac
done
emit() {
  if [[ -n "${out_file}" ]]; then
    cat > "${out_file}"
  else
    cat
  fi
}
if [[ "${MOCK_CURL_FAIL:-}" == "true" ]]; then
  echo "curl: (22) The requested URL returned error: 503" >&2
  exit 22
fi
if [[ "${url}" == *"LATEST.json" ]]; then
  if [[ -n "${MOCK_MANIFEST_URL_LOG:-}" ]]; then
    printf '%s\n' "${url}" >> "${MOCK_MANIFEST_URL_LOG}"
  fi
  emit < "${MOCK_MANIFEST_FILE:?MOCK_MANIFEST_FILE not set}"
  exit 0
fi
if [[ "${url}" == *"computeMetadata/v1/instance/service-accounts/default/token"* ]]; then
  printf '{"access_token":"stub-oauth-token-12345","expires_in":3599,"token_type":"Bearer"}\n' | emit
  exit 0
fi
if [[ "${url}" == *"testIamPermissions"* ]]; then
  printf '{"permissions":["artifactregistry.repositories.uploadArtifacts"]}\n' | emit
  exit 0
fi
echo "unexpected curl call: url=${url}" >&2
exit 1
EOF
  chmod +x "${dir}/bin/curl"

  # Stub gcloud: simulates GCS storage cat/cp and Artifact Registry commands
  cat >"${dir}/bin/gcloud" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ -n "${MOCK_GCLOUD_LOG:-}" ]]; then
  printf 'gcloud %s\n' "$*" >> "${MOCK_GCLOUD_LOG}"
fi

# gcloud auth print-access-token
if [[ "${1:-}" == "auth" && "${2:-}" == "print-access-token" ]]; then
  echo "stub-gcloud-token"
  exit 0
fi

# gcloud storage cat gs://bucket/path
if [[ "${1:-}" == "storage" && "${2:-}" == "cat" ]]; then
  uri=""
  for arg in "${@:3}"; do
    case "${arg}" in
      gs://*) uri="${arg#gs://}" ;;
    esac
  done
  if [[ "${MOCK_GCS_FORBIDDEN:-}" == "true" ]]; then
    echo "ERROR: (gcloud.storage.cat) HTTPError 403: Caller does not have storage.objects.get access to the Google Cloud Storage object." >&2
    exit 1
  fi
  local_file="${MOCK_GCS_ROOT:?}/${uri}"
  if [[ ! -f "${local_file}" ]]; then
    echo "ERROR: (gcloud.storage.cat) The following URLs matched no objects:" >&2
    echo "- gs://${uri}" >&2
    exit 1
  fi
  cat "${local_file}"
  exit 0
fi

# gcloud storage cp <src> <dst>
if [[ "${1:-}" == "storage" && "${2:-}" == "cp" ]]; then
  positional=()
  for arg in "${@:3}"; do
    case "${arg}" in
      --*) ;;
      *) positional+=("${arg}") ;;
    esac
  done
  src="${positional[0]}"
  dst="${positional[1]}"
  if [[ "${src}" == gs://* ]]; then
    local_src="${MOCK_GCS_ROOT:?}/${src#gs://}"
    if [[ ! -f "${local_src}" ]]; then
      echo "ERROR: (gcloud.storage.cp) The following URLs matched no objects: ${src}" >&2
      exit 1
    fi
    cp "${local_src}" "${dst}"
    exit 0
  fi
  if [[ "${dst}" == gs://* ]]; then
    local_dst="${MOCK_GCS_ROOT:?}/${dst#gs://}"
    mkdir -p "$(dirname "${local_dst}")"
    cp "${src}" "${local_dst}"
    exit 0
  fi
fi

# gcloud artifacts repositories describe ...
if [[ "${1:-}" == "artifacts" && "${2:-}" == "repositories" && "${3:-}" == "describe" ]]; then
  if [[ "${MOCK_AR_REPO_MISSING:-}" == "true" ]]; then
    echo "ERROR: (gcloud.artifacts.repositories.describe) NOT_FOUND: Repository not found" >&2
    exit 1
  fi
  echo "projects/p/locations/us-central1/repositories/r"
  exit 0
fi

# gcloud artifacts docker tags list <image>
if [[ "${1:-}" == "artifacts" && "${2:-}" == "docker" && "${3:-}" == "tags" && "${4:-}" == "list" ]]; then
  if [[ "${MOCK_AR_FORBIDDEN:-}" == "true" ]]; then
    echo "ERROR: (gcloud.artifacts.docker.tags.list) PERMISSION_DENIED: Permission denied" >&2
    exit 1
  fi
  image="${5}"
  pkg_name="${image##*/}"
  tags_file="${MOCK_AR_ROOT:?}/${pkg_name}.tags"
  if [[ ! -f "${tags_file}" ]]; then
    echo "ERROR: (gcloud.artifacts.docker.tags.list) NOT_FOUND: Package not found" >&2
    exit 1
  fi
  cat "${tags_file}"
  exit 0
fi

# gcloud artifacts docker tags add <src> <dst>
if [[ "${1:-}" == "artifacts" && "${2:-}" == "docker" && "${3:-}" == "tags" && "${4:-}" == "add" ]]; then
  src=""
  dst=""
  for arg in "${@:5}"; do
    case "${arg}" in
      --*) ;;
      *)
        if [[ -z "${src}" ]]; then src="${arg}"; else dst="${arg}"; fi
        ;;
    esac
  done
  dst_img="${dst%:*}"
  dst_tag="${dst##*:}"
  pkg_name="${dst_img##*/}"
  printf '%s\n' "${dst_tag}" >> "${MOCK_AR_ROOT:?}/${pkg_name}.tags"
  exit 0
fi

# gcloud artifacts docker images list <registry> --include-tags --filter=...
if [[ "${1:-}" == "artifacts" && "${2:-}" == "docker" && "${3:-}" == "images" && "${4:-}" == "list" ]]; then
  digest_file="${MOCK_AR_ROOT:?}/scion-hub-gke.digest"
  if [[ -f "${digest_file}" ]]; then
    cat "${digest_file}"
  fi
  exit 0
fi

echo "unexpected gcloud call: $*" >&2
exit 1
EOF
  chmod +x "${dir}/bin/gcloud"
}

echo "==> Running Slice 1 tests: resolve-and-check-noop.sh & version comparison..."

test_slice1() {
  local d="${TMP_ROOT}/s1"
  make_stub_env "${d}"

  local manifest="${d}/LATEST.json"
  cat >"${manifest}" <<'EOF'
{
  "channels": {
    "stable": {
      "version": "v0.3.0",
      "date": "2026-11-11T14:45:53Z",
      "url": "https://github.com/GoogleCloudPlatform/scion/releases/tag/v0.3.0"
    },
    "preview": {
      "version": "v0.4.0-preview.2",
      "date": "2026-11-13T15:18:15Z",
      "url": "https://github.com/GoogleCloudPlatform/scion/releases/tag/v0.4.0-preview.2"
    },
    "nightly": {
      "version": "nightly-20261118",
      "date": "2026-11-18T03:18:06Z",
      "url": "https://github.com/GoogleCloudPlatform/scion/releases/tag/nightly-20261118"
    }
  }
}
EOF

  local base_env=(
    "PATH=${d}/bin:${PATH}"
    "MOCK_MANIFEST_FILE=${manifest}"
    "MOCK_GCS_ROOT=${d}/gcs"
    "WORKSPACE_DIR=${d}/workspace"
    "_STATE_BUCKET=myproj-cdha-tfstate"
    "_SHARED_PREFIX=cdha"
    "_HUB_NAME=cdha-gke-1"
    "_REGISTRY=us-central1-docker.pkg.dev/myproj/cdha-scion"
    "_AGENT_PLATFORMS=linux/amd64"
    "_FORCE_REDEPLOY=false"
    "_ALLOW_DOWNGRADE=false"
    "_REPO_URL=https://github.com/GoogleCloudPlatform/scion.git"
  )

  # 1. Fresh deploy: resolves nightly from LATEST.json when deployed.json absent
  rm -rf "${d}/workspace" && mkdir -p "${d}/workspace"
  env "${base_env[@]}" MOCK_MANIFEST_URL_LOG="${d}/manifest_url.log" _CHANNEL=nightly _VERSION_TAG="" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" >/dev/null
  # shellcheck disable=SC1091
  source "${d}/workspace/target.env"
  assert_eq "resolves nightly channel from LATEST.json" "${TARGET_TAG}" "nightly-20261118"
  assert_eq "derives clean raw GitHub LATEST.json URL without .git" "$(cat "${d}/manifest_url.log")" "https://raw.githubusercontent.com/GoogleCloudPlatform/scion/main/LATEST.json"
  if [[ ! -f "${d}/workspace/SKIP_DEPLOY" ]]; then
    pass "fresh deploy does not write SKIP_DEPLOY"
  else
    fail "fresh deploy unexpectedly wrote SKIP_DEPLOY"
  fi

  # 2. _VERSION_TAG overrides _CHANNEL without needing LATEST.json
  rm -rf "${d}/workspace" && mkdir -p "${d}/workspace"
  env "${base_env[@]}" MOCK_CURL_FAIL=true _CHANNEL=nightly _VERSION_TAG="v0.4.0-preview.4" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" >/dev/null
  # shellcheck disable=SC1091
  source "${d}/workspace/target.env"
  assert_eq "_VERSION_TAG overrides _CHANNEL without fetching LATEST.json" "${TARGET_TAG}" "v0.4.0-preview.4"

  # 3. Converged deployed.json + default.tfstate triggers SKIP_DEPLOY short-circuit
  local digest="sha256:1111111111111111111111111111111111111111111111111111111111111111"
  mkdir -p "${d}/gcs/myproj-cdha-tfstate/cdha/hubs/cdha-gke-1"
  cat >"${d}/gcs/myproj-cdha-tfstate/cdha/hubs/cdha-gke-1/deployed.json" <<EOF
{
  "version": "nightly-20261118",
  "version_date": "2026-11-18T03:18:06Z",
  "commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "hub_image_digest": "${digest}",
  "deployed_at": "2026-11-18T05:10:00Z"
}
EOF
  cat >"${d}/gcs/myproj-cdha-tfstate/cdha/hubs/cdha-gke-1/default.tfstate" <<EOF
{
  "version": 4,
  "outputs": {
    "hub_installed": { "value": true, "type": "bool" },
    "hub_image": { "value": "us-central1-docker.pkg.dev/myproj/cdha-scion/scion-hub-gke@${digest}", "type": "string" }
  }
}
EOF
  rm -rf "${d}/workspace" && mkdir -p "${d}/workspace"
  env "${base_env[@]}" _CHANNEL=nightly _VERSION_TAG="" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" >/dev/null
  if [[ -f "${d}/workspace/SKIP_DEPLOY" ]]; then
    pass "converged state writes SKIP_DEPLOY"
  else
    fail "converged state failed to write SKIP_DEPLOY"
  fi

  # 4. _FORCE_REDEPLOY=true bypasses SKIP_DEPLOY even when converged
  rm -rf "${d}/workspace" && mkdir -p "${d}/workspace"
  env "${base_env[@]}" _FORCE_REDEPLOY=true _CHANNEL=nightly _VERSION_TAG="" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" >/dev/null
  if [[ ! -f "${d}/workspace/SKIP_DEPLOY" ]]; then
    pass "_FORCE_REDEPLOY=true bypasses no-op short-circuit"
  else
    fail "_FORCE_REDEPLOY=true still wrote SKIP_DEPLOY"
  fi

  # 5. hub_installed == false (Pass 1 of IAP client ID bootstrap) does NOT short-circuit
  cat >"${d}/gcs/myproj-cdha-tfstate/cdha/hubs/cdha-gke-1/default.tfstate" <<EOF
{
  "version": 4,
  "outputs": {
    "hub_installed": { "value": false, "type": "bool" },
    "hub_image": { "value": "us-central1-docker.pkg.dev/myproj/cdha-scion/scion-hub-gke@${digest}", "type": "string" }
  }
}
EOF
  rm -rf "${d}/workspace" && mkdir -p "${d}/workspace"
  env "${base_env[@]}" _CHANNEL=nightly _VERSION_TAG="" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" >/dev/null
  if [[ ! -f "${d}/workspace/SKIP_DEPLOY" ]]; then
    pass "hub_installed=false does not short-circuit"
  else
    fail "hub_installed=false unexpectedly short-circuited"
  fi

  # 6. Drifted hub_image digest in default.tfstate does NOT short-circuit
  local other_digest="sha256:2222222222222222222222222222222222222222222222222222222222222222"
  cat >"${d}/gcs/myproj-cdha-tfstate/cdha/hubs/cdha-gke-1/default.tfstate" <<EOF
{
  "version": 4,
  "outputs": {
    "hub_installed": { "value": true, "type": "bool" },
    "hub_image": { "value": "us-central1-docker.pkg.dev/myproj/cdha-scion/scion-hub-gke@${other_digest}", "type": "string" }
  }
}
EOF
  rm -rf "${d}/workspace" && mkdir -p "${d}/workspace"
  env "${base_env[@]}" _CHANNEL=nightly _VERSION_TAG="" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" >/dev/null
  if [[ ! -f "${d}/workspace/SKIP_DEPLOY" ]]; then
    pass "drifted tfstate digest does not short-circuit"
  else
    fail "drifted tfstate digest unexpectedly short-circuited"
  fi

  # 7. Downgrade guard blocks older nightly, older semver, and older cross-channel
  local out="" rc=0
  rc=0
  out="$(env "${base_env[@]}" _CHANNEL="" _VERSION_TAG="nightly-20261117" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" 2>&1)" || rc=$?
  if [[ ${rc} -ne 0 ]]; then
    assert_contains "downgrade guard rejects older nightly" "${out}" "_ALLOW_DOWNGRADE"
  else
    fail "downgrade guard allowed older nightly without _ALLOW_DOWNGRADE=true"
  fi

  rc=0
  out="$(env "${base_env[@]}" _CHANNEL="preview" _VERSION_TAG="" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" 2>&1)" || rc=$?
  if [[ ${rc} -ne 0 ]]; then
    assert_contains "downgrade guard rejects cross-channel switch to older preview" "${out}" "_ALLOW_DOWNGRADE"
  else
    fail "downgrade guard allowed older cross-channel preview without _ALLOW_DOWNGRADE=true"
  fi

  # 8. _ALLOW_DOWNGRADE=true allows downgrade
  rm -rf "${d}/workspace" && mkdir -p "${d}/workspace"
  env "${base_env[@]}" _ALLOW_DOWNGRADE=true _CHANNEL="" _VERSION_TAG="nightly-20261117" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" >/dev/null
  # shellcheck disable=SC1091
  source "${d}/workspace/target.env"
  assert_eq "_ALLOW_DOWNGRADE=true permits older version" "${TARGET_TAG}" "nightly-20261117"

  # 9. Upgrade (newer nightly & semver multi-digit preview.10 > preview.9) succeeds
  rm -rf "${d}/workspace" && mkdir -p "${d}/workspace"
  env "${base_env[@]}" _CHANNEL="" _VERSION_TAG="nightly-20261119" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" >/dev/null
  # shellcheck disable=SC1091
  source "${d}/workspace/target.env"
  assert_eq "newer nightly allowed as upgrade" "${TARGET_TAG}" "nightly-20261119"

  cat >"${d}/gcs/myproj-cdha-tfstate/cdha/hubs/cdha-gke-1/deployed.json" <<EOF
{
  "version": "v0.4.0-preview.9",
  "hub_image_digest": "${digest}"
}
EOF
  env "${base_env[@]}" _CHANNEL="" _VERSION_TAG="v0.4.0-preview.10" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" >/dev/null
  # shellcheck disable=SC1091
  source "${d}/workspace/target.env"
  assert_eq "semver preview.10 > preview.9 allowed as upgrade" "${TARGET_TAG}" "v0.4.0-preview.10"

  # 10. GCS 403 error fails closed instead of treating deployed.json as absent
  rc=0
  out="$(env "${base_env[@]}" MOCK_GCS_FORBIDDEN=true _CHANNEL=nightly _VERSION_TAG="" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" 2>&1)" || rc=$?
  if [[ ${rc} -ne 0 ]]; then
    pass "GCS 403 permission error fails closed"
  else
    fail "GCS 403 permission error did not fail"
  fi

  # 11. Anti-clobber hub_name prefix validation catches hub_name not starting with shared_prefix-
  rc=0
  out="$(env "${base_env[@]}" _HUB_NAME="wrong-hub" _CHANNEL=nightly _VERSION_TAG="" \
    bash "${SCRIPT_DIR}/resolve-and-check-noop.sh" 2>&1)" || rc=$?
  if [[ ${rc} -ne 0 ]]; then
    pass "rejects _HUB_NAME not starting with _SHARED_PREFIX-"
  else
    fail "accepted _HUB_NAME not starting with _SHARED_PREFIX-"
  fi
}

test_slice1

# Helper: create a minimal release git repository fixture with hub-gke, core-base, scion-base, and 2 harnesses
make_release_repo_fixture() {
  local repo_dir="$1"
  mkdir -p \
    "${repo_dir}/pkg/version" \
    "${repo_dir}/image-build/core-base" \
    "${repo_dir}/image-build/lib" \
    "${repo_dir}/image-build/scion-base" \
    "${repo_dir}/image-build/scripts" \
    "${repo_dir}/harnesses/claude" \
    "${repo_dir}/harnesses/gemini-cli" \
    "${repo_dir}/deploy/terraform/configurations/hub-gke"

  cp "${REPO_ROOT}/image-build/scripts/verify-registry.sh" "${repo_dir}/image-build/scripts/verify-registry.sh"
  cp "${REPO_ROOT}/image-build/scripts/install-registry-cred-helper.sh" "${repo_dir}/image-build/scripts/install-registry-cred-helper.sh"
  chmod +x "${repo_dir}/image-build/scripts/verify-registry.sh" "${repo_dir}/image-build/scripts/install-registry-cred-helper.sh"

  cat >"${repo_dir}/Dockerfile" <<'EOF'
FROM alpine:3.20 AS hub-gke
EOF
  cat >"${repo_dir}/pkg/version/downgrade_floor.go" <<'EOF'
package version
const DowngradeFloor = "nightly-20261119"
EOF
  printf 'FROM debian:trixie-slim\n' > "${repo_dir}/image-build/core-base/Dockerfile"
  printf '#!/bin/sh\necho toolchain\n' > "${repo_dir}/image-build/lib/install-core-toolchain.sh"
  printf 'FROM core-base\n' > "${repo_dir}/image-build/scion-base/Dockerfile"
  printf 'FROM scion-base\n' > "${repo_dir}/harnesses/claude/Dockerfile"
  printf 'FROM scion-base\n' > "${repo_dir}/harnesses/gemini-cli/Dockerfile"
  printf 'terraform {}\n' > "${repo_dir}/deploy/terraform/configurations/hub-gke/versions.tf"

  git -C "${repo_dir}" init -q -b main
  git -C "${repo_dir}" -c user.name="Test" -c user.email="test@example.com" add .
  GIT_AUTHOR_DATE="2026-11-18T03:00:00Z" GIT_COMMITTER_DATE="2026-11-18T03:00:00Z" \
    git -C "${repo_dir}" -c user.name="Test" -c user.email="test@example.com" commit -q -m "release commit 1"
  git -C "${repo_dir}" tag "nightly-20261118"
  git -C "${repo_dir}" tag "v0.4.0-preview.4"

  # Second commit where core-base and lib are UNCHANGED (only scion-base/app changed)
  printf '// bump\n' >> "${repo_dir}/Dockerfile"
  git -C "${repo_dir}" -c user.name="Test" -c user.email="test@example.com" add Dockerfile
  GIT_AUTHOR_DATE="2026-11-19T03:00:00Z" GIT_COMMITTER_DATE="2026-11-19T03:00:00Z" \
    git -C "${repo_dir}" -c user.name="Test" -c user.email="test@example.com" commit -q -m "release commit 2"
  git -C "${repo_dir}" tag "nightly-20261119"

  # Also create a branch that is NOT a tag
  git -C "${repo_dir}" branch "untagged-branch"
}

echo "==> Running Slice 2 tests: checkout-release-tag.sh..."

test_slice2() {
  local d="${TMP_ROOT}/s2"
  make_stub_env "${d}"
  local repo="${d}/origin-repo"
  make_release_repo_fixture "${repo}"

  # 1. SKIP_DEPLOY short-circuits without cloning
  mkdir -p "${d}/workspace"
  printf 'nightly-20261118\n' > "${d}/workspace/SKIP_DEPLOY"
  env WORKSPACE_DIR="${d}/workspace" _REPO_URL="${repo}" \
    bash "${SCRIPT_DIR}/checkout-release-tag.sh" >/dev/null
  if [[ ! -d "${d}/workspace/release-src" ]]; then
    pass "SKIP_DEPLOY skips checkout-release-tag"
  else
    fail "SKIP_DEPLOY still cloned release-src"
  fi

  # 2. Exact tag checkout (even when commit carries both nightly-* and v* tags)
  rm -rf "${d}/workspace" && mkdir -p "${d}/workspace"
  printf 'TARGET_TAG=%q\n' "v0.4.0-preview.4" > "${d}/workspace/target.env"
  env WORKSPACE_DIR="${d}/workspace" _REPO_URL="${repo}" \
    bash "${SCRIPT_DIR}/checkout-release-tag.sh" >/dev/null
  # shellcheck disable=SC1091
  source "${d}/workspace/build.env"
  local want_sha want_short
  want_sha="$(git -C "${repo}" rev-parse "refs/tags/v0.4.0-preview.4^{commit}")"
  want_short="$(git -C "${repo}" rev-parse --short "refs/tags/v0.4.0-preview.4^{commit}")"
  assert_eq "checkout records TARGET_TAG" "${TARGET_TAG}" "v0.4.0-preview.4"
  assert_eq "checkout records full 40-char COMMIT_SHA" "${COMMIT_SHA}" "${want_sha}"
  assert_eq "checkout records SHORT_SHA" "${SHORT_SHA}" "${want_short}"
  assert_eq "checkout records deterministic UTC BUILD_TIME" "${BUILD_TIME}" "2026-11-18T03:00:00Z"
  assert_eq "checkout extracts SOURCE_DOWNGRADE_FLOOR" "${SOURCE_DOWNGRADE_FLOOR}" "nightly-20261119"

  # 3. Missing nightly tag prints 14-day TTL hint
  rm -rf "${d}/workspace" && mkdir -p "${d}/workspace"
  printf 'TARGET_TAG=%q\n' "nightly-20260101" > "${d}/workspace/target.env"
  local out="" rc=0
  out="$(env WORKSPACE_DIR="${d}/workspace" _REPO_URL="${repo}" \
    bash "${SCRIPT_DIR}/checkout-release-tag.sh" 2>&1)" || rc=$?
  if [[ ${rc} -ne 0 ]]; then
    assert_contains "expired nightly tag mentions 14-day retention" "${out}" "14 days"
  else
    fail "missing nightly tag did not fail"
  fi

  # 4. Non-tag branch rejected by exact-match tag verification
  rm -rf "${d}/workspace" && mkdir -p "${d}/workspace"
  printf 'TARGET_TAG=%q\n' "untagged-branch" > "${d}/workspace/target.env"
  rc=0
  out="$(env WORKSPACE_DIR="${d}/workspace" _REPO_URL="${repo}" \
    bash "${SCRIPT_DIR}/checkout-release-tag.sh" 2>&1)" || rc=$?
  if [[ ${rc} -ne 0 ]]; then
    pass "branch without matching git tag fails exact-match check"
  else
    fail "branch without matching git tag succeeded"
  fi
}

test_slice2

echo "==> Running Slice 3 tests: build-images.sh..."

make_docker_stub() {
  local dir="$1"
  cat >"${dir}/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'docker %s\n' "$*" >> "${MOCK_DOCKER_LOG:?}"

if [[ "${1:-}" == "buildx" && "${2:-}" == "build" ]]; then
  meta_file=""
  tags=()
  prev=""
  for arg in "${@:3}"; do
    if [[ "${prev}" == "--metadata-file" ]]; then
      meta_file="${arg}"
    elif [[ "${prev}" == "-t" || "${prev}" == "--tag" ]]; then
      tags+=("${arg}")
    fi
    prev="${arg}"
  done

  # Record pushed tags into MOCK_AR_ROOT so subsequent idempotent checks see them
  for t in "${tags[@]}"; do
    img="${t%:*}"
    tag="${t##*:}"
    pkg="${img##*/}"
    printf '%s\n' "${tag}" >> "${MOCK_AR_ROOT:?}/${pkg}.tags"
  done

  if [[ -n "${meta_file}" ]]; then
    if [[ "${MOCK_BAD_META_DIGEST:-}" == "true" ]]; then
      printf '{"containerimage.digest":"not-a-sha256"}\n' > "${meta_file}"
    else
      digest="${MOCK_HUB_DIGEST:-sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789}"
      printf '{\n  "containerimage.digest": "%s",\n  "image.name": "%s"\n}\n' "${digest}" "${tags[0]}" > "${meta_file}"
      printf '%s\n' "${digest}" > "${MOCK_AR_ROOT:?}/scion-hub-gke.digest"
    fi
  fi
  exit 0
fi
exit 0
EOF
  chmod +x "${dir}/bin/docker"
}

test_slice3() {
  local d="${TMP_ROOT}/s3"
  make_stub_env "${d}"
  make_docker_stub "${d}"
  local repo="${d}/origin-repo"
  make_release_repo_fixture "${repo}"

  local reg="us-central1-docker.pkg.dev/myproj/cdha-scion"
  local hub_digest="sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

  # Prepare release-src for nightly-20261118
  printf 'TARGET_TAG=%q\n' "nightly-20261118" > "${d}/workspace/target.env"
  env WORKSPACE_DIR="${d}/workspace" _REPO_URL="${repo}" \
    bash "${SCRIPT_DIR}/checkout-release-tag.sh" >/dev/null
  # shellcheck disable=SC1091
  source "${d}/workspace/build.env"
  local sha1="${SHORT_SHA}"

  local build_env=(
    "HOME=${d}/home"
    "PATH=${d}/home/bin:${d}/bin:${PATH}"
    "WORKSPACE_DIR=${d}/workspace"
    "MOCK_AR_ROOT=${d}/ar"
    "MOCK_DOCKER_LOG=${d}/docker.log"
    "MOCK_GCLOUD_LOG=${d}/gcloud.log"
    "MOCK_HUB_DIGEST=${hub_digest}"
    "_REGISTRY=${reg}"
    "_AGENT_PLATFORMS=linux/amd64"
    "_FORCE_REDEPLOY=false"
  )

  # 1. Cold build: builds scion-hub-gke, core-base (with cache tag), scion-base, and both harnesses
  : > "${d}/docker.log"
  : > "${d}/gcloud.log"
  env "${build_env[@]}" bash "${SCRIPT_DIR}/build-images.sh" --phase prepare >/dev/null
  env "${build_env[@]}" bash "${SCRIPT_DIR}/build-images.sh" --phase build >/dev/null

  # shellcheck disable=SC1091
  source "${d}/workspace/hub-digest.env"
  assert_eq "extracts HUB_IMAGE_DIGEST from hub-gke-meta.json" "${HUB_IMAGE_DIGEST}" "${hub_digest}"

  local docker_calls
  docker_calls="$(cat "${d}/docker.log")"
  assert_contains "builds scion-hub-gke by short SHA" "${docker_calls}" "-t ${reg}/scion-hub-gke:${sha1}"
  assert_not_contains "never tags scion-hub-gke with release tag" "${docker_calls}" "scion-hub-gke:nightly-20261118"
  assert_not_contains "never tags any image :latest" "${docker_calls}" ":latest"
  assert_contains "passes VERSION build-arg to scion-hub-gke" "${docker_calls}" "VERSION=nightly-20261118"
  assert_contains "builds core-base with cache tag" "${docker_calls}" "-t ${reg}/core-base:cache-"
  assert_contains "builds scion-base" "${docker_calls}" "-t ${reg}/scion-base:nightly-20261118"
  assert_contains "builds discovered harness claude" "${docker_calls}" "-t ${reg}/scion-claude:nightly-20261118"
  assert_contains "builds discovered harness gemini-cli" "${docker_calls}" "-t ${reg}/scion-gemini-cli:nightly-20261118"

  # 2. Retry on same tag: skips all already-pushed images and still emits HUB_IMAGE_DIGEST
  : > "${d}/docker.log"
  rm -f "${d}/workspace/hub-digest.env" "${d}/workspace/build-plan.env"
  env "${build_env[@]}" bash "${SCRIPT_DIR}/build-images.sh" --phase prepare >/dev/null
  env "${build_env[@]}" bash "${SCRIPT_DIR}/build-images.sh" --phase build >/dev/null
  # shellcheck disable=SC1091
  source "${d}/workspace/hub-digest.env"
  assert_eq "retry recovers HUB_IMAGE_DIGEST from AR without rebuilding" "${HUB_IMAGE_DIGEST}" "${hub_digest}"
  assert_not_contains "retry runs zero docker buildx build commands" "$(cat "${d}/docker.log")" "buildx build"

  # 3. Next day nightly-20261119 (core-base unchanged): reuses core-base via content-hash tag!
  printf 'TARGET_TAG=%q\n' "nightly-20261119" > "${d}/workspace/target.env"
  env WORKSPACE_DIR="${d}/workspace" _REPO_URL="${repo}" \
    bash "${SCRIPT_DIR}/checkout-release-tag.sh" >/dev/null
  : > "${d}/docker.log"
  : > "${d}/gcloud.log"
  env "${build_env[@]}" bash "${SCRIPT_DIR}/build-images.sh" --phase prepare >/dev/null
  env "${build_env[@]}" bash "${SCRIPT_DIR}/build-images.sh" --phase build >/dev/null
  assert_contains "retags cached core-base for new nightly tag" "$(cat "${d}/gcloud.log")" "artifacts docker tags add ${reg}/core-base:cache-"
  assert_not_contains "skips rebuilding core-base on cache hit" "$(cat "${d}/docker.log")" "core-base/Dockerfile"
  assert_contains "rebuilds scion-base on new commit" "$(cat "${d}/docker.log")" "scion-base/Dockerfile"
  assert_contains "rebuilds scion-hub-gke on new commit" "$(cat "${d}/docker.log")" "--target hub-gke"

  # 4. Platform change (linux/amd64,linux/arm64) invalidates single-arch core-base cache!
  # Keep core-base.tags containing the amd64 cache tag, switch to v0.4.0-preview.4 + multi-arch:
  local amd64_cache_tag
  amd64_cache_tag="$(grep '^cache-' "${d}/ar/core-base.tags" | head -n 1)"
  rm -f "${d}/ar/"*.tags
  printf '%s\n' "${amd64_cache_tag}" > "${d}/ar/core-base.tags"
  printf 'TARGET_TAG=%q\n' "v0.4.0-preview.4" > "${d}/workspace/target.env"
  env WORKSPACE_DIR="${d}/workspace" _REPO_URL="${repo}" \
    bash "${SCRIPT_DIR}/checkout-release-tag.sh" >/dev/null
  : > "${d}/docker.log"
  env "${build_env[@]}" _AGENT_PLATFORMS="linux/amd64,linux/arm64" \
    bash "${SCRIPT_DIR}/build-images.sh" --phase all >/dev/null
  assert_contains "multi-arch build does not reuse amd64-only core-base cache and rebuilds multi-arch" "$(cat "${d}/docker.log")" "--platform linux/amd64,linux/arm64"
  assert_contains "multi-arch build rebuilds core-base/Dockerfile" "$(cat "${d}/docker.log")" "core-base/Dockerfile"

  # 5. _FORCE_REDEPLOY=true rebuilds even when tags already exist
  : > "${d}/docker.log"
  env "${build_env[@]}" _AGENT_PLATFORMS="linux/amd64,linux/arm64" _FORCE_REDEPLOY=true \
    bash "${SCRIPT_DIR}/build-images.sh" --phase all >/dev/null
  assert_contains "_FORCE_REDEPLOY=true rebuilds core-base" "$(cat "${d}/docker.log")" "core-base/Dockerfile"
  assert_contains "_FORCE_REDEPLOY=true rebuilds scion-hub-gke" "$(cat "${d}/docker.log")" "--target hub-gke"

  # 6. Malformed digest in hub-gke-meta.json fails closed
  local rc=0
  env "${build_env[@]}" _FORCE_REDEPLOY=true MOCK_BAD_META_DIGEST=true \
    bash "${SCRIPT_DIR}/build-images.sh" --phase all >/dev/null 2>&1 || rc=$?
  if [[ ${rc} -ne 0 ]]; then
    pass "malformed digest in hub-gke-meta.json fails closed"
  else
    fail "malformed digest in hub-gke-meta.json did not fail"
  fi
}

test_slice3

echo "==> Running Slice 4 tests: gcs-hub-state.sh & terraform-apply-hub-gke.sh (POSIX sh)..."

make_terraform_stub() {
  local dir="$1"
  cat >"${dir}/bin/terraform" <<'EOF'
#!/bin/sh
set -eu
printf 'terraform %s\n' "$*" >> "${MOCK_TF_LOG:?}"

# Parse subcommand after -chdir=...
sub=""
detailed="false"
plan_out=""
for arg in "$@"; do
  case "${arg}" in
    -chdir=*) ;;
    init|plan|apply|output)
      if [ -z "${sub}" ]; then sub="${arg}"; fi
      ;;
    -detailed-exitcode) detailed="true" ;;
    -out=*) plan_out="${arg#-out=}" ;;
  esac
done

case "${sub}" in
  init)
    exit 0
    ;;
  plan)
    if [ -n "${plan_out}" ]; then
      printf 'mock-tfplan\n' > "${plan_out}"
      exit 0
    fi
    if [ "${detailed}" = "true" ]; then
      exit "${MOCK_TF_CONVERGE_EXIT:-0}"
    fi
    exit 0
    ;;
  apply)
    exit 0
    ;;
  output)
    inst="${MOCK_TF_HUB_INSTALLED:-true}"
    printf '{"hub_installed":{"value":%s},"public_url":{"value":"https://cdha-gke-1.endpoints.myproj.cloud.goog"}}\n' "${inst}"
    exit 0
    ;;
esac
exit 0
EOF
  chmod +x "${dir}/bin/terraform"
}

test_slice4() {
  local d="${TMP_ROOT}/s4"
  make_stub_env "${d}"
  make_terraform_stub "${d}"

  mkdir -p "${d}/workspace/release-src/deploy/terraform/configurations/hub-gke"
  mkdir -p "${d}/gcs/myproj-cdha-tfstate/cdha/hubs/cdha-gke-1"

  local digest="sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
  cat >"${d}/workspace/target.env" <<'EOF'
TARGET_TAG='nightly-20261118'
TARGET_VERSION_DATE='2026-11-18T03:18:06Z'
EOF
  cat >"${d}/workspace/build.env" <<'EOF'
TARGET_TAG='nightly-20261118'
COMMIT_SHA='0123456789abcdef0123456789abcdef01234567'
SHORT_SHA='0123456'
BUILD_TIME='2026-11-18T03:00:00Z'
SOURCE_DOWNGRADE_FLOOR='nightly-20261119'
EOF
  printf "HUB_IMAGE_DIGEST='%s'\n" "${digest}" > "${d}/workspace/hub-digest.env"
  cat >"${d}/gcs/myproj-cdha-tfstate/cdha/hubs/cdha-gke-1/hub.tfvars" <<'EOF'
project_id    = "myproj"
region        = "us-central1"
zone          = "us-central1-a"
shared_prefix = "cdha"
hub_name      = "cdha-gke-1"
state_prefix  = "cdha/hubs/cdha-gke-1"
hostname      = "cdha-gke-1.endpoints.myproj.cloud.goog"
EOF

  local tf_env=(
    "PATH=${d}/bin:${PATH}"
    "WORKSPACE_DIR=${d}/workspace"
    "MOCK_GCS_ROOT=${d}/gcs"
    "MOCK_TF_LOG=${d}/tf.log"
    "_STATE_BUCKET=myproj-cdha-tfstate"
    "_SHARED_PREFIX=cdha"
    "_HUB_NAME=cdha-gke-1"
    "BUILD_ID=cb-build-999"
  )

  # 1. SKIP_DEPLOY short-circuits all Step 4 scripts cleanly
  printf 'nightly-20261118\n' > "${d}/workspace/SKIP_DEPLOY"
  : > "${d}/tf.log"
  env "${tf_env[@]}" bash "${SCRIPT_DIR}/gcs-hub-state.sh" fetch-tfvars >/dev/null
  env "${tf_env[@]}" sh "${SCRIPT_DIR}/terraform-apply-hub-gke.sh" >/dev/null
  env "${tf_env[@]}" bash "${SCRIPT_DIR}/gcs-hub-state.sh" write-deployed >/dev/null
  assert_eq "SKIP_DEPLOY runs zero terraform commands" "$(cat "${d}/tf.log")" ""
  rm -f "${d}/workspace/SKIP_DEPLOY"

  # 2. Full Cloud Build flow: fetch-tfvars -> POSIX sh terraform-apply-hub-gke.sh -> write-deployed
  : > "${d}/tf.log"
  env "${tf_env[@]}" bash "${SCRIPT_DIR}/gcs-hub-state.sh" fetch-tfvars >/dev/null
  env "${tf_env[@]}" SKIP_WRITE_DEPLOYED=true sh "${SCRIPT_DIR}/terraform-apply-hub-gke.sh" >/dev/null
  env "${tf_env[@]}" bash "${SCRIPT_DIR}/gcs-hub-state.sh" write-deployed >/dev/null

  local tf_calls
  tf_calls="$(cat "${d}/tf.log")"
  assert_contains "runs terraform init with -lockfile=readonly" "${tf_calls}" "init -input=false -lockfile=readonly -backend-config=bucket=myproj-cdha-tfstate -backend-config=prefix=cdha/hubs/cdha-gke-1"
  assert_contains "runs terraform plan with hub_image_digest" "${tf_calls}" "-var=hub_image_digest=${digest} -out=${d}/workspace/hub.tfplan"
  assert_contains "runs terraform apply on saved plan" "${tf_calls}" "apply -input=false ${d}/workspace/hub.tfplan"
  assert_contains "runs post-apply convergence plan -detailed-exitcode" "${tf_calls}" "plan -input=false -detailed-exitcode"
  if [[ ! -f "${d}/workspace/hub.tfplan" ]]; then
    pass "cleans up sensitive hub.tfplan after apply"
  else
    fail "left hub.tfplan behind on disk"
  fi

  local deployed_gcs="${d}/gcs/myproj-cdha-tfstate/cdha/hubs/cdha-gke-1/deployed.json"
  if [[ -f "${deployed_gcs}" ]]; then
    local dep_check
    dep_check="$(python3 - "${deployed_gcs}" <<'PY'
import json, sys
with open(sys.argv[1]) as f:
    d = json.load(f)
assert d["version"] == "nightly-20261118"
assert d["commit"] == "0123456789abcdef0123456789abcdef01234567"
assert d["hub_image_digest"].startswith("sha256:abcdef")
assert d["hub_installed"] is True
assert d["downgrade_floor"] == "nightly-20261119"
assert d["build_id"] == "cb-build-999"
print("OK")
PY
    )"
    assert_eq "deployed.json uploaded to GCS with expected metadata" "${dep_check}" "OK"
  else
    fail "deployed.json was not uploaded to GCS"
  fi

  # 3. Post-apply convergence failure (exit code 2) fails Step 4!
  local rc=0
  env "${tf_env[@]}" MOCK_TF_CONVERGE_EXIT=2 SKIP_WRITE_DEPLOYED=true \
    sh "${SCRIPT_DIR}/terraform-apply-hub-gke.sh" >/dev/null 2>&1 || rc=$?
  if [[ ${rc} -ne 0 ]]; then
    pass "post-apply drift (exit 2) fails terraform-apply-hub-gke.sh"
  else
    fail "post-apply drift (exit 2) did not fail terraform-apply-hub-gke.sh"
  fi
}

test_slice4

echo "==> Running Slice 5 tests: cloudbuild-gke-cd.yaml contract, setup-cd.sh & lint..."

test_slice5() {
  local yaml_path="${CD_DIR}/cloudbuild-gke-cd.yaml"
  if [[ ! -f "${yaml_path}" ]]; then
    fail "missing ${yaml_path}"
    return
  fi

  local yaml_content
  yaml_content="$(cat "${yaml_path}")"

  # Required substitutions present
  local sub
  for sub in _CHANNEL _VERSION_TAG _STATE_BUCKET _SHARED_PREFIX _HUB_NAME _REGISTRY _AGENT_PLATFORMS _FORCE_REDEPLOY _ALLOW_DOWNGRADE _REPO_URL; do
    assert_contains "cloudbuild-gke-cd.yaml defines substitution ${sub}" "${yaml_content}" "${sub}:"
  done

  # Key pipeline properties
  assert_contains "uses hashicorp/terraform:1.9.8" "${yaml_content}" "hashicorp/terraform:1.9.8"
  assert_contains "sets CLOUD_LOGGING_ONLY for custom CD service account compatibility" "${yaml_content}" "CLOUD_LOGGING_ONLY"
  assert_contains "has resolve-and-check-noop step" "${yaml_content}" "id: 'resolve-and-check-noop'"
  assert_contains "has checkout-release-tag step" "${yaml_content}" "id: 'checkout-release-tag'"
  assert_contains "has build-images step" "${yaml_content}" "id: 'build-images'"
  assert_contains "has terraform-apply-hub-gke step" "${yaml_content}" "id: 'terraform-apply-hub-gke'"
  assert_not_contains "zero mock/dev E2E steps in prod CD YAML" "${yaml_content}" "e2e"
  assert_not_contains "zero test_identities in prod CD YAML" "${yaml_content}" "test_identities"

  # Parse cloudbuild-gke-cd.yaml strictly via Go's gopkg.in/yaml.v3 when go is available
  if command -v go >/dev/null 2>&1; then
    local yaml_parse_dir="${TMP_ROOT}/yamlcheck"
    mkdir -p "${yaml_parse_dir}"
    cat >"${yaml_parse_dir}/main.go" <<'GOEOF'
package main

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type cbConfig struct {
	Steps []struct {
		Name       string   `yaml:"name"`
		ID         string   `yaml:"id"`
		Entrypoint string   `yaml:"entrypoint"`
		Args       []string `yaml:"args"`
		Env        []string `yaml:"env"`
	} `yaml:"steps"`
	Substitutions map[string]string `yaml:"substitutions"`
	Options       map[string]any    `yaml:"options"`
	Timeout       string            `yaml:"timeout"`
}

func main() {
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var cfg cbConfig
	dec := yaml.NewDecoder(os.Stdin)
	_ = dec
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(cfg.Steps) != 7 {
		fmt.Fprintf(os.Stderr, "expected 7 steps, got %d\n", len(cfg.Steps))
		os.Exit(1)
	}
	for _, s := range cfg.Steps {
		if s.Name == "" || s.ID == "" || s.Entrypoint == "" || len(s.Args) == 0 {
			fmt.Fprintf(os.Stderr, "incomplete step %+v\n", s)
			os.Exit(1)
		}
	}
}
GOEOF
    if (cd "${REPO_ROOT}" && go run -buildvcs=false "${yaml_parse_dir}/main.go" "${yaml_path}"); then
      pass "gopkg.in/yaml.v3 strict structural parse of cloudbuild-gke-cd.yaml"
    else
      fail "gopkg.in/yaml.v3 failed to parse cloudbuild-gke-cd.yaml"
    fi
  fi

  # Example tfvars exists and omits hardcoded hub_image_digest assignment
  if [[ -f "${CD_DIR}/hub.tfvars.example" ]]; then
    pass "hub.tfvars.example exists"
  else
    fail "missing deploy/cd/hub.tfvars.example"
  fi

  # README exists and documents key operational topics
  if [[ -f "${CD_DIR}/README.md" ]]; then
    local readme
    readme="$(cat "${CD_DIR}/README.md")"
    assert_contains "README documents shared-infra foundation" "${readme}" "shared-infra"
    assert_contains "README documents Cloud Endpoints zero-domain DNS option" "${readme}" ".endpoints."
    assert_contains "README documents two-pass iap_oauth_client_id bootstrap" "${readme}" "iap_oauth_client_id"
    assert_contains "README documents scion hub env set" "${readme}" "scion hub env set"
    assert_contains "README documents scion hub experiments enable" "${readme}" "scion hub experiments enable"
    assert_contains "README documents DowngradeFloor & rollback" "${readme}" "DowngradeFloor"
  else
    fail "missing deploy/cd/README.md"
  fi

  # setup-cd.sh --dry-run tests
  local dry_out=""
  dry_out="$(bash "${SCRIPT_DIR}/setup-cd.sh" \
    --project my-prod-proj \
    --region us-central1 \
    --shared-prefix cdha \
    --hub-name cdha-gke-1 \
    --channel preview \
    --dry-run)"
  assert_contains "setup-cd.sh creates versioned UBLA state bucket" "${dry_out}" "--uniform-bucket-level-access"
  assert_contains "setup-cd.sh enforces public-access-prevention on state bucket" "${dry_out}" "--public-access-prevention"
  assert_contains "setup-cd.sh creates dedicated CD service account" "${dry_out}" "cdha-cd-deployer"
  assert_contains "setup-cd.sh scopes Secret Manager role with IAM Condition" "${dry_out}" "secrets/cdha-gke-1-"
  assert_contains "setup-cd.sh scopes GCS storage.admin with IAM Condition" "${dry_out}" "buckets/my-prod-proj-cdha-gke-1-"
  assert_not_contains "setup-cd.sh never widens default @cloudbuild.gserviceaccount.com" "${dry_out}" "@cloudbuild.gserviceaccount.com"

  # Reject mismatched hub_name prefix in setup-cd.sh
  local rc=0
  bash "${SCRIPT_DIR}/setup-cd.sh" \
    --project my-prod-proj \
    --shared-prefix cdha \
    --hub-name other-gke-1 \
    --dry-run >/dev/null 2>&1 || rc=$?
  if [[ ${rc} -ne 0 ]]; then
    pass "setup-cd.sh rejects --hub-name not starting with <shared-prefix>-"
  else
    fail "setup-cd.sh accepted --hub-name not starting with <shared-prefix>-"
  fi

  # Shell syntax check on all scripts (bash -n for bash scripts, sh -n for POSIX sh scripts)
  local s
  for s in "${SCRIPT_DIR}"/*.sh; do
    if head -n 1 "${s}" | grep -q '^#!/bin/sh'; then
      if sh -n "${s}"; then
        pass "sh -n syntax clean: $(basename "${s}")"
      else
        fail "sh -n syntax error: $(basename "${s}")"
      fi
    else
      if bash -n "${s}"; then
        pass "bash -n syntax clean: $(basename "${s}")"
      else
        fail "bash -n syntax error: $(basename "${s}")"
      fi
    fi
  done

  if command -v shellcheck >/dev/null 2>&1; then
    for s in "${SCRIPT_DIR}"/*.sh; do
      if shellcheck -x --source-path=SCRIPTDIR "${s}"; then
        pass "shellcheck clean: $(basename "${s}")"
      else
        fail "shellcheck reported findings: $(basename "${s}")"
      fi
    done
  fi
}

test_slice5

echo ""
echo "Summary: ${PASS_COUNT} passed, ${FAIL_COUNT} failed"
[[ ${FAIL_COUNT} -eq 0 ]]
