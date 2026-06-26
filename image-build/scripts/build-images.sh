#!/bin/bash
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

set -euo pipefail

# Scion image build orchestrator.
#
# Owns the target DAG (which images to build, in what order, with which
# tags). Dispatches each step to a pluggable builder backend selected by
# --builder. Backends are small adapters that know how to run "build one
# image with these inputs" (per-image mode) or "submit one target"
# (target mode, e.g. cloud-build).

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
IMAGE_BUILD_DIR="${REPO_ROOT}/image-build"
export REPO_ROOT IMAGE_BUILD_DIR

# Hard-coded builder allow-list. Adding a new builder requires both an edit
# here and a new file under builders/.
ALLOWED_BUILDERS=(local-docker local-podman cloud-build)

BUILDER="local-docker"
REGISTRY=""
TARGET="common"
TAG="latest"
PLATFORM=""
PUSH="false"
DRY_RUN="false"
EMBED_WEB="false"

# shellcheck source=lib/targets.sh
source "${SCRIPT_DIR}/lib/targets.sh"

usage() {
  cat <<EOF
Usage: $(basename "$0") [options]

Build Scion container images via a pluggable builder backend.

Options:
  --registry <path>     Target registry path (e.g., ghcr.io/myorg).
                        Required when --push is set or with --builder cloud-build.
                        When omitted, images are tagged with bare names
                        (e.g., scion-claude:latest) and stay in the local store.
  --builder <name>      Build backend (default: local-docker)
                          local-docker  - docker buildx, local
                          local-podman  - podman build, local (single-arch by default)
                          cloud-build   - Google Cloud Build (submits a static cloudbuild-*.yaml)
  --target <target>     Build target (default: common)
                          core-base   - just the core-base layer
                          scion-base  - just scion-base (uses existing core-base:<tag>)
                          harnesses   - all harness images (uses existing scion-base:<tag>)
                          hub         - just scion-hub (uses existing scion-base:<tag>)
                          common      - scion-base + harnesses + hub (skip core-base)
                          all         - full rebuild including core-base
  --tag <tag>           Mutable image tag (default: latest). The :<short-sha> tag
                        is always added when run inside a git repo.
  --platform <plat>     Target platform(s) (default: builder's native arch)
                          all         - linux/amd64,linux/arm64
                          Or pass a value directly: linux/amd64,linux/arm64
                        Ignored by --builder cloud-build (YAMLs hardcode amd64+arm64).
  --push                Push images after building.
                        Auto-enabled for multi-arch builds (buildx limitation).
                        Ignored by --builder cloud-build (YAMLs always push).
  --embed-web           Build scion-base with web client assets embedded in
                        the scion binary. Supported for local Docker/Podman
                        image builds; not supported with --builder cloud-build.
  --dry-run             Print the steps and the exact builder commands without executing.
  -h, --help            Show this help message.

To trigger a build via GitHub Actions instead, run:
  gh workflow run build-images.yml -f registry=<registry> -f target=<target>
EOF
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --builder)  BUILDER="$2"; shift 2 ;;
    --registry) REGISTRY="$2"; shift 2 ;;
    --target)   TARGET="$2"; shift 2 ;;
    --tag)      TAG="$2"; shift 2 ;;
    --platform) PLATFORM="$2"; shift 2 ;;
    --push)     PUSH="true"; shift ;;
    --embed-web) EMBED_WEB="true"; shift ;;
    --dry-run)  DRY_RUN="true"; shift ;;
    -h|--help)  usage 0 ;;
    *) echo "Unknown option: $1" >&2; usage 1 ;;
  esac
done

REGISTRY="${REGISTRY%/}"

# Validate builder against allow-list.
builder_ok="false"
for b in "${ALLOWED_BUILDERS[@]}"; do
  if [[ "${BUILDER}" == "${b}" ]]; then
    builder_ok="true"
    break
  fi
done
if [[ "${builder_ok}" != "true" ]]; then
  echo "Error: unknown --builder '${BUILDER}'" >&2
  echo "Allowed: ${ALLOWED_BUILDERS[*]}" >&2
  exit 1
fi

if [[ "${EMBED_WEB}" == "true" && "${BUILDER}" == "cloud-build" ]]; then
  echo "Error: --embed-web is not supported with --builder cloud-build." >&2
  echo "Cloud Build uses static YAML definitions; use local-docker or local-podman for embedded web image builds." >&2
  exit 1
fi

# Validate target.
if ! resolve_targets "${TARGET}" >/dev/null; then
  echo "Error: unknown --target '${TARGET}'" >&2
  echo "Allowed: ${ALL_TARGETS[*]}" >&2
  exit 1
fi

# Resolve platform to a canonical comma-separated string ("" = builder native).
PLATFORMS=""
if [[ -n "${PLATFORM}" ]]; then
  if [[ "${PLATFORM}" == "all" ]]; then
    PLATFORMS="linux/amd64,linux/arm64"
  else
    PLATFORMS="${PLATFORM}"
  fi
fi

# Multi-arch builds require --push (buildx can't load multi-arch images
# into the local docker daemon). Auto-promote and warn, matching the prior
# build-images.sh behavior.
if [[ "${PLATFORMS}" == *","* && "${PUSH}" != "true" ]]; then
  echo "Warning: multi-platform builds require --push. Adding --push automatically."
  PUSH="true"
fi

# --registry is required for any path that publishes images. Without it,
# we tag with bare names (scion-claude:latest) and the images stay local.
if [[ -z "${REGISTRY}" ]]; then
  if [[ "${BUILDER}" == "cloud-build" ]]; then
    echo "Error: --registry is required with --builder cloud-build" >&2
    exit 1
  fi
  if [[ "${PUSH}" == "true" ]]; then
    echo "Error: --registry is required with --push" >&2
    exit 1
  fi
fi

# --load is the inverse of --push for per-image builders that build into a
# local engine.
LOAD="false"
if [[ "${PUSH}" != "true" ]]; then
  LOAD="true"
fi

# Compute git metadata once. Both used directly (in build-args, substitutions)
# and to build the :<short-sha> tag.
SHORT_SHA=""
COMMIT_SHA=""
if git -C "${REPO_ROOT}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  SHORT_SHA="$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || true)"
  COMMIT_SHA="$(git -C "${REPO_ROOT}" rev-parse HEAD 2>/dev/null || true)"
fi
export SHORT_SHA COMMIT_SHA

# Source the selected builder. The allow-list above guarantees the file
# name is one of a fixed set.
# shellcheck source=builders/local-docker.sh
source "${SCRIPT_DIR}/builders/${BUILDER}.sh"

if [[ "${DRY_RUN}" != "true" ]] && ! builder_check; then
  exit 1
fi

# Resolve step list once for both per-image execution and dry-run printing.
# Read into an array via a while-loop for compatibility with Bash 3.2 (macOS
# /bin/bash), which lacks `mapfile`/`readarray`.
STEPS=()
while IFS= read -r line; do
  STEPS+=("${line}")
done < <(resolve_targets "${TARGET}")

echo "Builder:  ${BUILDER}  (mode: ${BUILDER_MODE})"
echo "Target:   ${TARGET}"
echo "Registry: ${REGISTRY:-<none — bare local tags>}"
echo "Tag:      ${TAG}${SHORT_SHA:+ (+ :${SHORT_SHA})}"
if [[ "${BUILDER_MODE}" == "per-image" ]]; then
  echo "Platforms: ${PLATFORMS:-<native>}"
  echo "Push:     ${PUSH}"
  echo "Embed web: ${EMBED_WEB}"
fi
echo "Steps:    ${STEPS[*]}"
if [[ "${DRY_RUN}" == "true" ]]; then
  echo "(dry-run: no commands will be executed)"
fi
echo ""

builder_prepare

# resolve_base_tag <step_id>
#
# Returns the tag suffix the orchestrator should use for this step's parent
# image. If the parent was built earlier in the same run AND we have a
# short-sha, use the sha (immune to concurrent :latest overwrites). If
# the parent isn't in this run, fall back to the mutable tag.
resolve_base_tag() {
  local step="$1"
  local parent
  parent="$(step_parent "${step}")"
  if [[ -z "${parent}" ]]; then
    echo ""
    return 0
  fi

  local s
  for s in "${STEPS[@]}"; do
    if [[ "${s}" == "${step}" ]]; then
      break
    fi
    if [[ "${s}" == "${parent}" ]]; then
      if [[ -n "${SHORT_SHA}" ]]; then
        echo "${SHORT_SHA}"
      else
        echo "${TAG}"
      fi
      return 0
    fi
  done

  echo "${TAG}"
}

# Build the comma-separated tag list for an image: always :<tag>, plus
# :<short-sha> when available. Omits the registry prefix when REGISTRY is
# empty (local-only build), so tags are bare like "scion-claude:latest".
compute_tags() {
  local image_name="$1"
  local prefix=""
  if [[ -n "${REGISTRY}" ]]; then
    prefix="${REGISTRY}/"
  fi
  local tags="${prefix}${image_name}:${TAG}"
  if [[ -n "${SHORT_SHA}" ]]; then
    tags="${tags},${prefix}${image_name}:${SHORT_SHA}"
  fi
  echo "${tags}"
}

if [[ "${BUILDER_MODE}" == "target" ]]; then
  builder_run_target "${TARGET}" "${REGISTRY}" "${TAG}" "${PUSH}"
else
  for step in "${STEPS[@]}"; do
    image_name="$(step_image_name "${step}")"
    dockerfile="$(step_dockerfile "${step}")"
    context_dir="$(step_context_dir "${step}")"
    tags="$(compute_tags "${image_name}")"

    BASE_TAG="$(resolve_base_tag "${step}")"
    export BASE_TAG REGISTRY TAG SHORT_SHA COMMIT_SHA EMBED_WEB

    # Collect build-args for this step.
    build_arg_flags=()
    while IFS= read -r line; do
      [[ -z "${line}" ]] && continue
      build_arg_flags+=(--build-arg "${line}")
    done < <(step_build_args "${step}")

    DRY_RUN="${DRY_RUN}" \
    builder_build \
      --image-name "${image_name}" \
      --context-dir "${context_dir}" \
      --dockerfile "${dockerfile}" \
      --tags "${tags}" \
      --platforms "${PLATFORMS}" \
      "${build_arg_flags[@]}" \
      --push "${PUSH}" \
      --load "${LOAD}"
  done
fi

builder_finalize

echo ""
if [[ "${DRY_RUN}" == "true" ]]; then
  echo "Dry run complete. No images were built or pushed."
else
  echo "Done."
  if [[ "${BUILDER_MODE}" == "per-image" && -n "${REGISTRY}" ]]; then
    echo ""
    echo "To configure scion to use these images, run:"
    echo "  scion config set image_registry ${REGISTRY}"
  fi
fi
