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

# cloud-build builder for the scion image build orchestrator.
#
# Operates in target mode: instead of looping per image, the orchestrator
# calls builder_run_target once per invocation and this builder hands the
# whole target off to Google Cloud Build via `gcloud builds submit`. Caching
# and per-image step ordering are concerns of the static cloudbuild-*.yaml
# files that ship alongside this script.

# shellcheck disable=SC2034 # sourced by build-images.sh, which reads BUILDER_MODE
BUILDER_MODE="target"

# Single-harness targets (ptone/scion#2354) generate a throwaway Cloud Build
# config per step ID, in a private temp directory, rather than editing a
# static file in-tree. cloud_build_config_for_target can be called more than
# once per `build-images.sh` invocation (build-images.sh's own
# warn_if_scion_base_not_in_run peeks at the config too), each call in its
# own command-substitution subshell -- so an EXIT trap set inside a function
# would only ever see its own subshell's copy. Register a single, real
# process-wide EXIT trap here, at source time (before any target is
# resolved), that removes the one directory every such call writes into.
# Subshells from command substitution inherit CLOUD_BUILD_TMPDIR, so every
# call site shares it without reporting a randomized name back.
#
# mktemp -d (rather than a predictable "${TMPDIR:-/tmp}/..." path) avoids a
# classic insecure-tempfile bug (CWE-377): a predictable path in a shared
# temp directory lets another local user plant a symlink or a file they can
# write ahead of time, so the config this builder feeds to
# `gcloud builds submit` could be attacker-controlled. build-images.sh must
# not set its own EXIT trap before sourcing this file (or must chain it),
# or that trap would replace this one.
CLOUD_BUILD_TMPDIR="$(mktemp -d "${TMPDIR:-/tmp}/cloudbuild-single-harness.XXXXXX")"
trap 'rm -rf "${CLOUD_BUILD_TMPDIR}"' EXIT

builder_check() {
  if ! command -v gcloud >/dev/null 2>&1; then
    echo "Error: 'gcloud' not found in PATH."
    echo "Install Google Cloud SDK before using --builder cloud-build."
    return 1
  fi
}

builder_prepare() {
  :
}

# generate_single_harness_config <step_id>
#
# Writes a one-step Cloud Build config to a temp file for a single harness
# image (e.g. "scion-claude") and echoes its path. Mirrors the per-harness
# build step in cloudbuild-harnesses.yaml, minus the other harnesses --
# this is what lets `--target scion-<name>` (ptone/scion#2354) rebuild one
# harness via Cloud Build instead of the whole catalog. is_harness_step and
# IMAGE_BUILD_DIR come from lib/targets.sh / build-images.sh, both sourced
# into the same process before any builder runs.
#
# Keep this step in sync by hand with its sibling in cloudbuild-harnesses.yaml
# (name, dir, tag pair, env, and build args): check-harness-coverage.sh only
# checks the static cloudbuild-*.yaml files, not this generated one, so a
# future edit to the static step (e.g. a new --build-arg) will not be caught
# here automatically.
generate_single_harness_config() {
  local step_id="$1"
  local harness_name="${step_id#scion-}"
  # Written into the private CLOUD_BUILD_TMPDIR (see the source-time trap
  # above); a second call for the same step_id in this process just
  # overwrites identical content.
  local out="${CLOUD_BUILD_TMPDIR}/${step_id}.yaml"
  cat >"${out}" <<EOF
steps:
  - name: 'gcr.io/cloud-builders/gcloud'
    id: 'verify-registry'
    entrypoint: 'bash'
    args: ['image-build/scripts/verify-registry.sh', '\$_REGISTRY']

  - name: 'gcr.io/cloud-builders/docker'
    id: 'setup-buildx'
    args: ['buildx', 'create', '--name', 'mybuilder', '--use']
    env:
      - 'DOCKER_CLI_EXPERIMENTAL=enabled'

  - name: 'gcr.io/cloud-builders/docker'
    id: 'bootstrap-buildx'
    args: ['buildx', 'inspect', '--bootstrap']
    env:
      - 'DOCKER_CLI_EXPERIMENTAL=enabled'

  - name: 'gcr.io/cloud-builders/docker'
    id: 'build-${step_id}'
    dir: 'harnesses/${harness_name}'
    args: ['buildx', 'build', '--platform', 'linux/amd64,linux/arm64', '--build-arg', 'BASE_IMAGE=\$_REGISTRY/scion-base:\$_TAG', '-t', '\$_REGISTRY/${step_id}:\$_SHORT_SHA', '-t', '\$_REGISTRY/${step_id}:\$_TAG', '-f', 'Dockerfile', '--pull', '--push', '.']
    env:
      - 'DOCKER_CLI_EXPERIMENTAL=enabled'

substitutions:
  _REGISTRY: 'us-central1-docker.pkg.dev/\${PROJECT_ID}/public-docker'
  _TAG: 'latest'
options:
  dynamicSubstitutions: true
  machineType: 'E2_HIGHCPU_8'
timeout: 1200s
EOF
  echo "${out}"
}

# cloud_build_config_for_target <target>
# Echoes the absolute path to the cloudbuild-*.yaml that implements the
# given target, or (for an individual harness step ID such as
# "scion-claude") the path to a config generated on the fly by
# generate_single_harness_config. Returns nonzero if no mapping exists.
cloud_build_config_for_target() {
  local target="$1"

  if declare -F is_harness_step >/dev/null && is_harness_step "${target}"; then
    generate_single_harness_config "${target}"
    return 0
  fi

  local file
  case "${target}" in
    common)     file="cloudbuild-common.yaml" ;;
    all)        file="cloudbuild.yaml" ;;
    core-base)  file="cloudbuild-core-base.yaml" ;;
    scion-base) file="cloudbuild-scion-base.yaml" ;;
    harnesses)  file="cloudbuild-harnesses.yaml" ;;
    hub)        file="cloudbuild-hub.yaml" ;;
    omni)      file="cloudbuild-omni.yaml" ;;
    thick-prep) file="cloudbuild-thick.yaml" ;;
    thick)      file="cloudbuild-thick.yaml" ;;
    *)
      echo "cloud-build: no cloudbuild-*.yaml mapping for target '${target}'" >&2
      return 1
      ;;
  esac
  local path="${IMAGE_BUILD_DIR}/${file}"
  if [[ ! -f "${path}" ]]; then
    echo "cloud-build: expected config file does not exist: ${path}" >&2
    echo "(target table and cloudbuild-*.yaml files are out of sync)" >&2
    return 1
  fi
  echo "${path}"
}

# builder_run_target <target> <registry> <tag> <push>
#
# Submits the target to Cloud Build. <push> is ignored — the YAMLs always
# push. <registry> and <tag> become _REGISTRY and _TAG substitutions.
builder_run_target() {
  local target="$1"
  local registry="$2"
  local tag="$3"
  # local push="$4"  # ignored: cloud-build YAMLs always push

  local config
  config="$(cloud_build_config_for_target "${target}")" || return 1

  # Auto-detect project from the registry path (<host>/<project>/<repo>)
  # when neither $GCLOUD_PROJECT nor gcloud config provides one.
  local project="${GCLOUD_PROJECT:-}"
  if [[ -z "${project}" ]]; then
    project="$(gcloud config get-value project 2>/dev/null)" || true
  fi
  if [[ -z "${project}" && -n "${registry}" ]]; then
    project="$(echo "${registry}" | cut -d/ -f2)"
  fi
  if [[ -z "${project}" ]]; then
    echo "Error: could not determine GCP project." >&2
    echo "Set \$GCLOUD_PROJECT or run 'gcloud config set project <project>'." >&2
    return 1
  fi

  # Warn if the registry lives in a different project than the one Cloud
  # Build will run in — the build SA in that project likely lacks push
  # access to the other project's Artifact Registry.
  if [[ -n "${registry}" ]]; then
    local reg_project
    reg_project="$(echo "${registry}" | cut -d/ -f2)"
    if [[ "${reg_project}" != "${project}" ]]; then
      echo "Warning: Cloud Build project '${project}' differs from registry project '${reg_project}'." >&2
      echo "The build SA in '${project}' may not have push access to '${registry}'." >&2
      echo "Consider: export GCLOUD_PROJECT=${reg_project}" >&2
      echo ""
    fi
  fi

  # Only pass substitutions that the template actually references.
  # Cloud Build rejects any key in --substitutions that is not matched
  # (referenced) in the template steps.
  local short_sha="${SHORT_SHA:-unknown}"
  local commit_sha="${COMMIT_SHA:-unknown}"
  # Unlike short_sha/commit_sha, an empty VERSION is a normal, expected value
  # (off-tag builds), not a placeholder — same convention as hack/version.sh.
  local version_val="${VERSION:-}"

  local subs="_TAG=${tag}"
  if grep -q '_SHORT_SHA' "${config}"; then
    subs="${subs},_SHORT_SHA=${short_sha}"
  fi
  if grep -q '_COMMIT_SHA' "${config}"; then
    subs="${subs},_COMMIT_SHA=${commit_sha}"
  fi
  # Omit _VERSION entirely when empty so the yaml's own `_VERSION: ''`
  # default applies. The regex excludes `GO_VERSION` and the `_VERSION: ''`
  # declaration line (both lack a leading `$`); it's POSIX ERE, so it also
  # works with BSD grep.
  if [[ -n "${version_val}" ]] && grep -qE '\$\{?_VERSION([^A-Za-z0-9_]|$)' "${config}"; then
    subs="${subs},_VERSION=${version_val}"
  fi
  if [[ -n "${registry}" ]]; then
    subs="${subs},_REGISTRY=${registry}"
  fi

  echo "==> [cloud-build] submitting target '${target}' via ${config}"
  local -a cmd=(
    gcloud builds submit --async
    --project="${project}"
    --substitutions="${subs}"
    --config="${config}"
  )

  # When a target-specific gcloudignore file exists, use it instead of the
  # default .gcloudignore. This allows targets like omni (which need web
  # source files excluded by the default) to override upload filtering.
  local ignore_file="${IMAGE_BUILD_DIR}/gcloudignore-${target}"
  if [[ -f "${ignore_file}" ]]; then
    cmd+=(--ignore-file="${ignore_file}")
  fi

  cmd+=("${REPO_ROOT}")

  if [[ "${DRY_RUN:-false}" == "true" ]]; then
    printf '[dry-run]'
    printf ' %q' "${cmd[@]}"
    printf '\n'
    return 0
  fi

  "${cmd[@]}"

  echo ""
  echo "Build submitted. View progress at:"
  echo "  https://console.cloud.google.com/cloud-build/builds?project=${project}"
}

# Per-image entry point is unused for target-mode builders, but define a
# stub that errors loudly if the orchestrator ever calls it by mistake.
builder_build() {
  echo "cloud-build: builder_build called on a target-mode builder (orchestrator bug)" >&2
  return 1
}

builder_finalize() {
  :
}
