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

# Target DAG and step descriptors for the Scion image build orchestrator.
#
# This file is sourced by build-images.sh. It is the single source of truth
# for which images exist, which target names expand to which ordered step
# lists, and which dockerfile / context dir / build-args each step uses.
#
# Builders never read this file. The orchestrator translates step descriptors
# into the uniform builder_build call.

discover_harness_names() {
  local harness_root="${REPO_ROOT}/harnesses"
  if [[ ! -d "${harness_root}" ]]; then
    return 0
  fi
  find "${harness_root}" -mindepth 2 -maxdepth 2 -name Dockerfile -print \
    | while IFS= read -r dockerfile; do
        basename "$(dirname "${dockerfile}")"
      done \
    | sort
}

is_harness_step() {
  local step="$1"
  local name="${step#scion-}"
  [[ "${step}" == scion-* && -f "${REPO_ROOT}/harnesses/${name}/Dockerfile" ]]
}

emit_harness_steps() {
  local name
  discover_harness_names | while IFS= read -r name; do
    [[ -n "${name}" ]] && echo "scion-${name}"
  done
}

# All known step IDs. The step ID is also the published image name
# (without registry prefix). Harness steps are discovered from the root
# harnesses catalog.
ALL_STEP_IDS=(
  core-base
  scion-base
)
while IFS= read -r harness_step; do
  [[ -n "${harness_step}" ]] && ALL_STEP_IDS+=("${harness_step}")
done < <(emit_harness_steps)
ALL_STEP_IDS+=(
  scion-hub
)

# All known target names. Used by the orchestrator's --help and --target
# validation.
ALL_TARGETS=(
  core-base
  scion-base
  harnesses
  hub
  common
  all
)

# resolve_targets <target>
#
# Echoes one step ID per line, in build order, for the given target. Returns
# nonzero (and prints nothing on stdout) for an unknown target.
resolve_targets() {
  case "$1" in
    core-base)
      echo core-base
      ;;
    scion-base)
      echo scion-base
      ;;
    harnesses)
      emit_harness_steps
      ;;
    hub)
      echo scion-hub
      ;;
    common)
      printf '%s\n' scion-base
      emit_harness_steps
      printf '%s\n' scion-hub
      ;;
    all)
      printf '%s\n' core-base scion-base
      emit_harness_steps
      printf '%s\n' scion-hub
      ;;
    *)
      return 1
      ;;
  esac
}

# step_image_name <step_id>
step_image_name() {
  echo "$1"
}

# step_dockerfile <step_id>
#
# Echoes the absolute path to the dockerfile for the step. Requires
# IMAGE_BUILD_DIR to be set in the environment.
step_dockerfile() {
  case "$1" in
    core-base)     echo "${IMAGE_BUILD_DIR}/core-base/Dockerfile" ;;
    scion-base)    echo "${IMAGE_BUILD_DIR}/scion-base/Dockerfile" ;;
    scion-hub)     echo "${IMAGE_BUILD_DIR}/hub/Dockerfile" ;;
    *)
      if is_harness_step "$1"; then
        echo "${REPO_ROOT}/harnesses/${1#scion-}/Dockerfile"
      else
        return 1
      fi
      ;;
  esac
}

# step_context_dir <step_id>
#
# Echoes the absolute path to the build context for the step. scion-base
# uses the repo root because it copies go source; everything else uses its
# own image-build subdirectory.
step_context_dir() {
  case "$1" in
    core-base)     echo "${IMAGE_BUILD_DIR}/core-base" ;;
    scion-base)    echo "${REPO_ROOT}" ;;
    scion-hub)     echo "${IMAGE_BUILD_DIR}/hub" ;;
    *)
      if is_harness_step "$1"; then
        echo "${REPO_ROOT}/harnesses/${1#scion-}"
      else
        return 1
      fi
      ;;
  esac
}

# step_build_args <step_id>
#
# Emits one KEY=VALUE line per build-arg on stdout. Reads orchestrator
# state from environment: REGISTRY, TAG, SHORT_SHA, COMMIT_SHA, BASE_TAG.
# BASE_TAG is the tag (sha or mutable) the orchestrator chose for this
# step's parent image. When REGISTRY is empty (local-only build), BASE_IMAGE
# is emitted with a bare image name (e.g. core-base:latest) so it matches
# the tag the previous step actually wrote into the local image store.
step_build_args() {
  local prefix=""
  if [[ -n "${REGISTRY:-}" ]]; then
    prefix="${REGISTRY}/"
  fi
  case "$1" in
    core-base)
      # No build-args.
      ;;
    scion-base)
      echo "BASE_IMAGE=${prefix}core-base:${BASE_TAG}"
      if [[ -n "${COMMIT_SHA:-}" ]]; then
        echo "GIT_COMMIT=${COMMIT_SHA}"
      fi
      ;;
    scion-hub)
      echo "BASE_IMAGE=${prefix}scion-base:${BASE_TAG}"
      ;;
    *)
      if is_harness_step "$1"; then
        echo "BASE_IMAGE=${prefix}scion-base:${BASE_TAG}"
      else
        return 1
      fi
      ;;
  esac
}

# step_parent <step_id>
#
# Echoes the step ID of the parent image, or empty for root images. Used by
# the orchestrator to thread :short-sha through chained builds and pick the
# right :tag fallback for standalone targets.
step_parent() {
  case "$1" in
    core-base)     echo "" ;;
    scion-base)    echo "core-base" ;;
    scion-hub)     echo "scion-base" ;;
    *)
      if is_harness_step "$1"; then
        echo "scion-base"
      else
        return 1
      fi
      ;;
  esac
}
