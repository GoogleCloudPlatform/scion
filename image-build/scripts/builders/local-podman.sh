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

# local-podman builder for the scion image build orchestrator.
#
# Wraps `podman build`. Native arch only by default; multi-arch --platform
# values are rejected with an actionable error (QEMU binfmt setup is the
# user's responsibility and must be opted into deliberately).

BUILDER_MODE="per-image"

builder_check() {
  if ! command -v podman >/dev/null 2>&1; then
    echo "Error: 'podman' not found in PATH."
    echo "Install Podman (https://podman.io/) before using --builder local-podman."
    return 1
  fi
}

builder_prepare() {
  :
}

# See local-docker.sh for the flag contract.
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
      *) echo "local-podman: unknown builder_build flag: $1" >&2; return 1 ;;
    esac
  done

  if [[ "${platforms}" == *","* ]]; then
    echo "Error: local-podman does not perform multi-arch builds by default." >&2
    echo "Requested platforms: ${platforms}" >&2
    echo "Multi-arch Podman builds require manual QEMU binfmt setup." >&2
    echo "Either install and register qemu-user-static, then run a single-platform" >&2
    echo "build per arch, or use --builder local-docker for multi-arch buildx." >&2
    return 1
  fi

  local -a cmd=(podman build)
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
  for arg in ${build_args[@]+"${build_args[@]}"}; do
    cmd+=(--build-arg "${arg}")
  done

  cmd+=(-f "${dockerfile}" "${context_dir}")

  echo "==> [local-podman] building ${image_name}..."
  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    printf '[dry-run]'
    printf ' %q' "${cmd[@]}"
    printf '\n'
    if [[ "${push}" == "true" ]]; then
      for t in "${tag_list[@]}"; do
        printf '[dry-run] podman push %q\n' "${t}"
      done
    fi
    return 0
  fi

  "${cmd[@]}"

  if [[ "${push}" == "true" ]]; then
    for t in "${tag_list[@]}"; do
      echo "==> [local-podman] pushing ${t}"
      podman push "${t}"
    done
  fi
  # --load is implicit for podman: built images already live in the local
  # store, no separate step needed.

  echo "    ${image_name} done."
}

builder_finalize() {
  :
}
