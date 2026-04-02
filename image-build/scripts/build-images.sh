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

# Build Scion container images locally using docker buildx.
#
# Usage:
#   build-images.sh --registry <registry> [--target <target>] [--push] [--platform <platforms>] [--tag <tag>]
#
# Examples:
#   build-images.sh --registry ghcr.io/myorg
#   build-images.sh --registry ghcr.io/myorg --target all --push --platform all
#   build-images.sh --registry us-docker.pkg.dev/myproject/scion --target harnesses --tag v1.0

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
IMAGE_BUILD_DIR="${REPO_ROOT}/image-build"

REGISTRY=""
TARGET="common"
PUSH=""
PLATFORM=""
TAG="latest"

HARNESSES=(claude gemini opencode codex)

usage() {
  cat <<EOF
Usage: $(basename "$0") --registry <registry> [options]

Build Scion container images locally using docker buildx.

Required:
  --registry <path>    Target registry path (e.g., ghcr.io/myorg)

Options:
  --target <target>    Build target (default: common)
                         common    - scion-base + all harnesses (skip core-base)
                         all       - full rebuild including core-base
                         core-base - build only core-base
                         harnesses - build only harness images
  --push               Push images after building (default: build only)
  --platform <plat>    Target platform(s) (default: current architecture)
                         all       - linux/amd64,linux/arm64
                         Or specify directly: linux/amd64,linux/arm64
  --tag <tag>          Image tag (default: latest)
  -h, --help           Show this help message
EOF
  exit "${1:-0}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --registry)   REGISTRY="$2"; shift 2 ;;
    --target)     TARGET="$2"; shift 2 ;;
    --push)       PUSH="--push"; shift ;;
    --platform)   PLATFORM="$2"; shift 2 ;;
    --tag)        TAG="$2"; shift 2 ;;
    -h|--help)    usage 0 ;;
    *)            echo "Unknown option: $1"; usage 1 ;;
  esac
done

if [[ -z "${REGISTRY}" ]]; then
  echo "Error: --registry is required"
  usage 1
fi

# Strip trailing slash from registry
REGISTRY="${REGISTRY%/}"

# Resolve platform flags
PLATFORM_ARGS=()
if [[ -n "${PLATFORM}" ]]; then
  if [[ "${PLATFORM}" == "all" ]]; then
    PLATFORM_ARGS=(--platform "linux/amd64,linux/arm64")
  else
    PLATFORM_ARGS=(--platform "${PLATFORM}")
  fi
fi

# When doing multi-platform builds without --push, we need --load for single
# platform or must push (buildx limitation). Warn the user.
if [[ ${#PLATFORM_ARGS[@]} -gt 0 && -z "${PUSH}" ]]; then
  PLAT_VAL="${PLATFORM_ARGS[1]}"
  if [[ "${PLAT_VAL}" == *","* ]]; then
    echo "Warning: Multi-platform builds require --push. Adding --push automatically."
    PUSH="--push"
  fi
fi

LOAD_ARG=""
if [[ -z "${PUSH}" ]]; then
  LOAD_ARG="--load"
fi

# Ensure buildx builder exists
ensure_builder() {
  if ! docker buildx inspect scion-builder &>/dev/null; then
    echo "Creating buildx builder 'scion-builder'..."
    docker buildx create --name scion-builder --use
  else
    docker buildx use scion-builder
  fi
  docker buildx inspect --bootstrap >/dev/null
}

build_core_base() {
  echo "==> Building core-base..."
  docker buildx build \
    ${PLATFORM_ARGS[@]+"${PLATFORM_ARGS[@]}"} \
    -t "${REGISTRY}/core-base:${TAG}" \
    -f "${IMAGE_BUILD_DIR}/core-base/Dockerfile" \
    ${PUSH} ${LOAD_ARG} \
    "${IMAGE_BUILD_DIR}/core-base"
  echo "    core-base done."
}

build_scion_base() {
  local base_tag="${1:-latest}"
  echo "==> Building scion-base..."
  docker buildx build \
    ${PLATFORM_ARGS[@]+"${PLATFORM_ARGS[@]}"} \
    --build-arg "BASE_IMAGE=${REGISTRY}/core-base:${base_tag}" \
    --build-arg "GIT_COMMIT=$(git -C "${REPO_ROOT}" rev-parse HEAD 2>/dev/null || echo unknown)" \
    -t "${REGISTRY}/scion-base:${TAG}" \
    -f "${IMAGE_BUILD_DIR}/scion-base/Dockerfile" \
    ${PUSH} ${LOAD_ARG} \
    "${REPO_ROOT}"
  echo "    scion-base done."
}

build_harness() {
  local name="$1"
  local base_tag="${2:-latest}"
  echo "==> Building scion-${name}..."
  docker buildx build \
    ${PLATFORM_ARGS[@]+"${PLATFORM_ARGS[@]}"} \
    --build-arg "BASE_IMAGE=${REGISTRY}/scion-base:${base_tag}" \
    -t "${REGISTRY}/scion-${name}:${TAG}" \
    -f "${IMAGE_BUILD_DIR}/${name}/Dockerfile" \
    ${PUSH} ${LOAD_ARG} \
    "${IMAGE_BUILD_DIR}/${name}"
  echo "    scion-${name} done."
}

build_all_harnesses() {
  local base_tag="${1:-latest}"
  for h in "${HARNESSES[@]}"; do
    build_harness "${h}" "${base_tag}"
  done
}

# Main
ensure_builder

case "${TARGET}" in
  common)
    build_scion_base "latest"
    build_all_harnesses "${TAG}"
    ;;
  all)
    build_core_base
    build_scion_base "${TAG}"
    build_all_harnesses "${TAG}"
    ;;
  core-base)
    build_core_base
    ;;
  harnesses)
    build_all_harnesses "latest"
    ;;
  *)
    echo "Unknown target: ${TARGET}"
    usage 1
    ;;
esac

echo ""
echo "Images built successfully!"
echo ""
echo "To configure scion to use these images, run:"
echo "  scion config set image_registry ${REGISTRY}"
echo ""
echo "Or add to your ~/.scion/settings.yaml:"
echo "  image_registry: \"${REGISTRY}\""
