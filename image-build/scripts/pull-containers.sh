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

# Pull all scion container images from a registry.
#
# Usage:
#   pull-containers.sh --registry <registry> [--tag <tag>] [runtime]
#
# Examples:
#   pull-containers.sh --registry ghcr.io/myorg               # pull from registry
#   pull-containers.sh --registry ghcr.io/myorg --tag v1.0    # specific tag

REGISTRY=""
TAG="latest"
RUNTIME_ARG=""
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --registry) REGISTRY="$2"; shift 2 ;;
    --tag)      TAG="$2"; shift 2 ;;
    -h|--help)
      echo "Usage: $(basename "$0") --registry <registry> [--tag <tag>] [runtime]"
      echo ""
      echo "Required:"
      echo "  --registry <path>  Registry path (e.g., ghcr.io/myorg)"
      echo ""
      echo "Options:"
      echo "  --tag <tag>        Image tag (default: latest)"
      echo "  runtime            Container runtime: docker, podman, container (auto-detected)"
      exit 0
      ;;
    -*) echo "Unknown option: $1"; exit 1 ;;
    *)  RUNTIME_ARG="$1"; shift ;;
  esac
done

if [[ -z "${REGISTRY}" ]]; then
  echo "Error: --registry is required"
  echo "Usage: $(basename "$0") --registry <registry> [--tag <tag>] [runtime]"
  exit 1
fi
REGISTRY="${REGISTRY%/}"

IMAGES=(
  "${REGISTRY}/scion-base:${TAG}"
  "${REGISTRY}/scion-hub:${TAG}"
)
while IFS= read -r dockerfile; do
  harness="$(basename "$(dirname "${dockerfile}")")"
  IMAGES+=("${REGISTRY}/scion-${harness}:${TAG}")
done < <(find "${REPO_ROOT}/harnesses" -mindepth 2 -maxdepth 2 -name Dockerfile -print | sort)

detect_runtime() {
  if command -v container &>/dev/null && [[ "$(uname)" == "Darwin" ]]; then
    echo "container"
  elif command -v docker &>/dev/null; then
    echo "docker"
  elif command -v podman &>/dev/null; then
    echo "podman"
  else
    echo ""
  fi
}

if [[ -n "$RUNTIME_ARG" ]]; then
  case "$RUNTIME_ARG" in
    container|docker|podman) RUNTIME="$RUNTIME_ARG" ;;
    *)
      echo "Error: unsupported runtime '$RUNTIME_ARG'. Use one of: container, docker, podman"
      exit 1
      ;;
  esac
else
  RUNTIME="$(detect_runtime)"
  if [[ -z "$RUNTIME" ]]; then
    echo "Error: no container runtime found. Install docker, podman, or container (macOS)."
    exit 1
  fi
fi

echo "Using runtime: $RUNTIME"
echo "Registry: $REGISTRY"
echo ""

for image in "${IMAGES[@]}"; do
  echo "Pulling: $image"
  "$RUNTIME" image pull "$image"
  echo ""
done

echo "Pruning unused images..."
if [[ "$RUNTIME" == "container" ]]; then
  "$RUNTIME" image prune
else
  "$RUNTIME" image prune -f
fi
echo "Done."
