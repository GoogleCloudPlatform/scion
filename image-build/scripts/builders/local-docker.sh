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

# local-docker builder for the scion image build orchestrator.
#
# Implements the per-image builder contract on top of `docker buildx`.

BUILDER_MODE="per-image"
BUILDX_INSTANCE="scion-builder"

builder_check() {
  if ! command -v docker >/dev/null 2>&1; then
    echo "Error: 'docker' not found in PATH."
    echo "Install Docker Desktop or the docker CLI before using --builder local-docker."
    return 1
  fi
  if ! docker buildx version >/dev/null 2>&1; then
    echo "Error: 'docker buildx' is not available."
    echo "Install the buildx plugin (Docker Desktop ships it; otherwise see https://docs.docker.com/buildx/)."
    return 1
  fi
}

builder_prepare() {
  if [[ "${PUSH:-false}" != "true" ]]; then
    if [[ "${DRY_RUN:-false}" == "true" ]]; then
      echo "[dry-run] export BUILDX_BUILDER=default"
      return 0
    fi
    echo "Using default docker builder for local build..."
    export BUILDX_BUILDER="default"
    return 0
  fi

  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    echo "[dry-run] docker buildx create --name ${BUILDX_INSTANCE}   # if missing"
    echo "[dry-run] export BUILDX_BUILDER=${BUILDX_INSTANCE}"
    echo "[dry-run] docker buildx inspect --bootstrap"
    return 0
  fi
  if ! docker buildx inspect "${BUILDX_INSTANCE}" >/dev/null 2>&1; then
    echo "Creating buildx builder '${BUILDX_INSTANCE}'..."
    docker buildx create --name "${BUILDX_INSTANCE}"
  fi
  export BUILDX_BUILDER="${BUILDX_INSTANCE}"
  docker buildx inspect --bootstrap >/dev/null
}

# builder_build flag arguments:
#   --image-name <name>
#   --context-dir <abs path>
#   --dockerfile <abs path>
#   --tags <comma-separated full refs>
#   --platforms <comma-separated platforms or empty>
#   --build-arg KEY=VALUE   (repeatable)
#   --push <true|false>
#   --load <true|false>
builder_build() {
  local image_name="" context_dir="" dockerfile="" tags="" platforms="" push="false" load="false"
  local -a build_args=()

  while [[ $# -gt 0 ]]; do
    case "$1" in
      --image-name)  image_name="$2"; shift 2 ;;
      --context-dir) context_dir="$2"; shift 2 ;;
      --dockerfile)  dockerfile="$2"; shift 2 ;;
      --tags)        tags="$2"; shift 2 ;;
      --platforms)   platforms="$2"; shift 2 ;;
      --build-arg)   build_args+=("$2"); shift 2 ;;
      --push)        push="$2"; shift 2 ;;
      --load)        load="$2"; shift 2 ;;
      *) echo "local-docker: unknown builder_build flag: $1" >&2; return 1 ;;
    esac
  done

  local -a cmd=(docker buildx build)
  if [[ -n "${platforms}" ]]; then
    cmd+=(--platform "${platforms}")
  fi

  local IFS=','
  read -ra tag_list <<<"${tags}"
  unset IFS
  local t
  for t in "${tag_list[@]}"; do
    cmd+=(-t "${t}")
  done

  local arg
  for arg in "${build_args[@]}"; do
    cmd+=(--build-arg "${arg}")
  done

  cmd+=(-f "${dockerfile}")

  if [[ "${push}" == "true" ]]; then
    cmd+=(--push)
  elif [[ "${load}" == "true" ]]; then
    cmd+=(--load)
  fi

  cmd+=("${context_dir}")

  echo "==> [local-docker] building ${image_name}..."
  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    printf '[dry-run]'
    printf ' %q' "${cmd[@]}"
    printf '\n'
    return 0
  fi
  "${cmd[@]}"
  echo "    ${image_name} done."
}

builder_finalize() {
  :
}
