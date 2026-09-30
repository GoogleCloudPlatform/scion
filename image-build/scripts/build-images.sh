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
REGISTRY_EXPLICIT="false"
TARGET="common"
TAG="latest"
PLATFORM=""
PLATFORM_EXPLICIT="false"
PUSH="false"
DRY_RUN="false"
CONTINUE_ON_ERROR="false"

# shellcheck source=lib/targets.sh
source "${SCRIPT_DIR}/lib/targets.sh"

usage() {
  cat <<EOF
Usage: $(basename "$0") [options]

Build Scion container images via a pluggable builder backend.

Options:
  --registry <path>     Target registry path (e.g., ghcr.io/myorg).
                        Required when --push is set or with --builder cloud-build.
                        Falls back to SCION_IMAGE_REGISTRY env var when omitted.
                        When both are unset, images are tagged with bare names
                        (e.g., scion-claude:latest) and stay in the local store.
  --builder <name>      Build backend (default: local-docker)
                          local-docker  - docker buildx, local
                          local-podman  - podman build, local (single-arch by default)
                          cloud-build   - Google Cloud Build (group targets submit a
                                          static cloudbuild-*.yaml; an individual harness
                                          step ID submits a config generated on the fly)
  --target <target>     Build target (default: common)
                        Group targets:
                          core-base   - just the core-base layer
                          scion-base  - just scion-base (uses existing core-base:<tag>)
                          harnesses   - all catalog harness images with Dockerfiles
                                        (uses existing scion-base:<tag>)
                          hub         - just scion-hub (uses existing scion-base:<tag>)
                          common      - scion-base + harnesses + hub (skip core-base)
                          all         - full rebuild including core-base
                          thick-prep  - just the thick base prep layer (amd64 only)
                          omni        - omni image chain for Cloud Run Instances
                                        single-node deployment (amd64 only,
                                        needs scion-base pre-built or in registry)
                          thick       - full thick rebuild: thick-prep + scion-base +
                                        harnesses + hub (amd64 only, uses Cloud
                                        Workstations base instead of core-base)
                        Individual step IDs (e.g. scion-claude, scion-hub,
                        scion-omni) are also accepted by local-docker and
                        local-podman. Under cloud-build, only individual
                        harness step IDs (e.g. scion-claude, scion-codex)
                        work this way, submitting a generated single-step
                        config instead of the group's static
                        cloudbuild-*.yaml; scion-hub, scion-omni, and other
                        non-harness step IDs are not mapped there, so use
                        their group target (hub, omni) with cloud-build
                        instead. Use the group target "all" with --dry-run
                        to list all valid step IDs.
  --tag <tag>           Mutable image tag (default: latest). The :<short-sha> tag
                        is always added when run inside a git repo.
  --platform <plat>     Target platform(s) (default: builder's native arch)
                          all         - linux/amd64,linux/arm64
                          Or pass a value directly: linux/amd64,linux/arm64
                        Ignored by --builder cloud-build (YAMLs hardcode amd64+arm64).
  --push                Push images after building.
                        Auto-enabled for multi-arch builds (buildx limitation).
                        Ignored by --builder cloud-build (YAMLs always push).
  --continue-on-error   When building multiple images, continue past failures
                        and report a summary at the end instead of stopping at
                        the first error. Exit code is non-zero if any step failed.
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
    --registry) REGISTRY="$2"; REGISTRY_EXPLICIT="true"; shift 2 ;;
    --target)   TARGET="$2"; shift 2 ;;
    --tag)      TAG="$2"; shift 2 ;;
    --platform) PLATFORM="$2"; PLATFORM_EXPLICIT="true"; shift 2 ;;
    --push)     PUSH="true"; shift ;;
    --continue-on-error) CONTINUE_ON_ERROR="true"; shift ;;
    --dry-run)  DRY_RUN="true"; shift ;;
    -h|--help)  usage 0 ;;
    *) echo "Unknown option: $1" >&2; usage 1 ;;
  esac
done

# Default REGISTRY from SCION_IMAGE_REGISTRY when --registry was not passed.
# This ensures locally-built images match the hub's configured registry prefix
# without requiring --registry on every invocation. When --registry was
# explicitly passed (even as empty), the env var is not consulted.
if [[ "${REGISTRY_EXPLICIT}" != "true" && -n "${SCION_IMAGE_REGISTRY:-}" ]]; then
  REGISTRY="${SCION_IMAGE_REGISTRY}"
  echo "Note: Using SCION_IMAGE_REGISTRY (${REGISTRY}) as default registry."
fi

REGISTRY="${REGISTRY%/}"

# Set THICK_BUILD flag when building the thick target, so step descriptors
# in targets.sh route scion-base to thick-prep instead of core-base.
# The omni target also implies thick build (thick base has no arm64 variant).
THICK_BUILD="${THICK_BUILD:-false}"
if [[ "${TARGET}" == "thick" || "${TARGET}" == "thick-prep" || "${TARGET}" == "omni" ]]; then
  THICK_BUILD="true"
fi
export THICK_BUILD

# Set OMNI_BUILD flag when building the omni target, so step descriptors
# in targets.sh chain harnesses instead of branching from scion-base.
OMNI_BUILD="${OMNI_BUILD:-false}"
if [[ "${TARGET}" == "omni" ]]; then
  OMNI_BUILD="true"
fi
export OMNI_BUILD

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

# Thick targets are amd64-only. When the user explicitly requested arm64,
# error out. When no --platform was given, default to linux/amd64 so that
# builds on arm64 hosts (e.g. Apple Silicon) don't silently fail.
if [[ "${THICK_BUILD}" == "true" ]]; then
  if [[ "${PLATFORM_EXPLICIT}" == "true" && "${PLATFORMS}" == *"arm64"* ]]; then
    echo "Error: --target ${TARGET} is amd64-only (Cloud Workstations base has no arm64 variant)." >&2
    echo "Remove --platform or use --platform linux/amd64." >&2
    exit 1
  elif [[ "${PLATFORM_EXPLICIT}" != "true" ]]; then
    PLATFORMS="linux/amd64"
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
VERSION=""
if git -C "${REPO_ROOT}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  SHORT_SHA="$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || true)"
  COMMIT_SHA="$(git -C "${REPO_ROOT}" rev-parse HEAD 2>/dev/null || true)"
  # Same convention as hack/version.sh: VERSION is only set when HEAD is
  # exactly on a tag, so the embedded sciontool/scion Version falls back to
  # "dev" the same way a local `make build` off-tag does. The .git directory
  # is not in the docker build context (VCS stamping is disabled there), so
  # this has to be resolved on the host and threaded through as a build-arg,
  # the same way GIT_COMMIT already is.
  VERSION="$(git -C "${REPO_ROOT}" describe --tags --exact-match 2>/dev/null || true)"
fi
export SHORT_SHA COMMIT_SHA VERSION

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
fi
echo "Steps:    ${STEPS[*]}"
if [[ "${DRY_RUN}" == "true" ]]; then
  echo "(dry-run: no commands will be executed)"
fi
echo ""

# warn_if_scion_base_not_in_run
#
# usage-telemetry (#2053, D5): sciontool fixes only reach agents when
# scion-base is rebuilt, and a harness-only build (e.g. --target harnesses)
# silently inherits whatever scion-base already exists — stale sciontool and
# all. Warn, don't fail: this is a build-time provenance nudge, not a version
# check (the design's non-goal is runtime version checks, not this).
warn_if_scion_base_not_in_run() {
  local built_scion_base="false"
  local needs_scion_base="false"

  # In target mode (cloud-build), STEPS is resolve_targets()'s per-image view
  # and is only used above for the banner/dry-run listing -- it is not what
  # actually runs. The orchestrator hands the *whole target* off to a static
  # cloudbuild-*.yaml, and that yaml can build scion-base itself even when
  # STEPS (computed the same way regardless of builder) doesn't include it.
  # cloudbuild-omni.yaml is exactly this case: it rebuilds the full chain
  # from thick-prep, unlike the per-image "omni" target, which chains from
  # whatever scion-base image already exists. Ask the yaml, not STEPS, in
  # that mode. Guarded by `declare -F` so this stays a no-op if a future
  # target-mode builder doesn't define the helper.
  if [[ "${BUILDER_MODE}" == "target" ]] && declare -F cloud_build_config_for_target >/dev/null; then
    local target_config
    if target_config="$(cloud_build_config_for_target "${TARGET}" 2>/dev/null)" \
      && [[ -f "${target_config}" ]] \
      && grep -q "id: 'build-scion-base'" "${target_config}"; then
      built_scion_base="true"
    fi
  fi

  local s
  for s in "${STEPS[@]}"; do
    if [[ "${s}" == "scion-base" ]]; then
      built_scion_base="true"
    elif [[ "$(step_parent "${s}")" == "scion-base" ]]; then
      needs_scion_base="true"
    fi
  done
  if [[ "${needs_scion_base}" != "true" || "${built_scion_base}" == "true" ]]; then
    return 0
  fi

  local prefix=""
  [[ -n "${REGISTRY}" ]] && prefix="${REGISTRY}/"
  local inherited="${prefix}scion-base:${TAG}"

  echo "Warning: this build does not (re)build scion-base." >&2
  echo "  These images will inherit whatever sciontool is already baked into" >&2
  echo "  ${inherited}." >&2
  echo "  If that scion-base predates a sciontool fix you need (e.g. usage" >&2
  echo "  telemetry), rebuild it first: --target scion-base, then re-run" >&2
  echo "  this build." >&2

  # Best-effort only: skip when there's no local store to trust -- cloud-build
  # and any push/multi-arch local-docker build (buildx docker-container
  # driver) both resolve BASE_IMAGE from the registry, not a local store.
  # Otherwise inspect the store the selected builder actually uses. A hit is
  # a local copy that can still be stale; any miss is silently swallowed.
  if [[ "${BUILDER_MODE}" != "target" && "${PUSH}" != "true" ]]; then
    local inspect_tool=""
    case "${BUILDER}" in
      # if-guarded, not "cmd && x": a failing && list that ends up as the
      # function's return status would trip set -e.
      local-podman) if command -v podman >/dev/null 2>&1; then inspect_tool="podman"; fi ;;
      *)            if command -v docker >/dev/null 2>&1; then inspect_tool="docker"; fi ;;
    esac
    if [[ -n "${inspect_tool}" ]]; then
      local revision
      revision="$("${inspect_tool}" image inspect "${inherited}" \
        --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' \
        2>/dev/null || true)"
      if [[ -n "${revision}" && "${revision}" != "<no value>" ]]; then
        echo "  local copy of ${inherited}: revision ${revision}" >&2
      fi
    fi
  fi
  echo "" >&2
}
warn_if_scion_base_not_in_run

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

FAILED_STEPS=()

if [[ "${BUILDER_MODE}" == "target" ]]; then
  builder_run_target "${TARGET}" "${REGISTRY}" "${TAG}" "${PUSH}"
else
  for step in "${STEPS[@]}"; do
    image_name="$(step_image_name "${step}")"
    dockerfile="$(step_dockerfile "${step}")"
    context_dir="$(step_context_dir "${step}")"
    tags="$(compute_tags "${image_name}")"

    BASE_TAG="$(resolve_base_tag "${step}")"
    export BASE_TAG REGISTRY TAG SHORT_SHA COMMIT_SHA VERSION

    # Collect build-args for this step.
    build_arg_flags=()
    while IFS= read -r line; do
      [[ -z "${line}" ]] && continue
      build_arg_flags+=(--build-arg "${line}")
    done < <(step_build_args "${step}")

    if [[ "${CONTINUE_ON_ERROR}" == "true" ]]; then
      set +e
    fi

    DRY_RUN="${DRY_RUN}" \
    builder_build \
      --image-name "${image_name}" \
      --context-dir "${context_dir}" \
      --dockerfile "${dockerfile}" \
      --tags "${tags}" \
      --platforms "${PLATFORMS}" \
      ${build_arg_flags[@]+"${build_arg_flags[@]}"} \
      --push "${PUSH}" \
      --load "${LOAD}"
    build_rc=$?

    if [[ "${CONTINUE_ON_ERROR}" == "true" ]]; then
      set -e
      if [[ ${build_rc} -ne 0 ]]; then
        echo ""
        echo "Error: step '${step}' failed (exit ${build_rc}). Continuing..." >&2
        FAILED_STEPS+=("${step}")
      fi
    fi
  done
fi

builder_finalize

echo ""
if [[ ${#FAILED_STEPS[@]} -gt 0 ]]; then
  echo "Build completed with errors." >&2
  echo "" >&2
  echo "Failed steps (${#FAILED_STEPS[@]}):" >&2
  for _failed in "${FAILED_STEPS[@]}"; do
    echo "  - ${_failed}" >&2
  done
  exit 1
elif [[ "${DRY_RUN}" == "true" ]]; then
  echo "Dry run complete. No images were built or pushed."
else
  echo "Done."
  if [[ "${BUILDER_MODE}" == "per-image" ]]; then
    echo ""
    echo "Built images:"
    for step in "${STEPS[@]}"; do
      image_name="$(step_image_name "${step}")"
      echo "  $(compute_tags "${image_name}" | tr ',' '\n' | head -1)"
    done
    if [[ -n "${REGISTRY}" ]]; then
      echo ""
      echo "To configure scion to use these images, run:"
      echo "  scion config set image_registry ${REGISTRY}"
    elif [[ "${REGISTRY_EXPLICIT}" != "true" && -n "${SCION_IMAGE_REGISTRY:-}" ]]; then
      # REGISTRY is empty and --registry was not explicitly passed, yet
      # SCION_IMAGE_REGISTRY is set. This shouldn't happen after the
      # default-from-env logic above, but warn just in case.
      echo ""
      echo "Warning: SCION_IMAGE_REGISTRY is set to '${SCION_IMAGE_REGISTRY}'"
      echo "but images were tagged without a registry prefix. The hub will look"
      echo "for '${SCION_IMAGE_REGISTRY}/<image>:<tag>' and won't find bare-tagged images."
      echo "Re-run with: --registry ${SCION_IMAGE_REGISTRY}"
    fi
  fi
fi
