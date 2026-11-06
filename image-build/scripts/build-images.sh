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
TAG_EXPLICIT="false"
PLATFORM=""
PLATFORM_EXPLICIT="false"
PUSH="false"
DRY_RUN="false"
CONTINUE_ON_ERROR="false"
ALLOW_STALE_BASE="false"
STRICT_BASE="false"

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
                        is always added when run inside a git repo. When HEAD is
                        exactly on a git tag and --tag is not given, a warning
                        suggests --tag <that tag>; the default stays latest.
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
  --allow-stale-base    Skip the base check. Without it, a target that stacks
                        on a base it does not build (core-base:<tag> for
                        scion-base and common, scion-base:<tag> for harnesses,
                        hub, omni and single images) stops before building when
                        that base is definitely not found locally or in the
                        registry. A lookup that fails (permissions, network, a
                        missing tool) only warns, and the build goes ahead.
  --strict-base         Also stop when the base lookup fails, not only when the
                        base is definitely missing (for CI that wants a hard
                        gate). Ignored with --allow-stale-base.
  --dry-run             Print the steps and the exact builder commands without executing.
                        A missing base image is reported as a warning.
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
    --tag)      TAG="$2"; TAG_EXPLICIT="true"; shift 2 ;;
    --platform) PLATFORM="$2"; PLATFORM_EXPLICIT="true"; shift 2 ;;
    --push)     PUSH="true"; shift ;;
    --continue-on-error) CONTINUE_ON_ERROR="true"; shift ;;
    --allow-stale-base) ALLOW_STALE_BASE="true"; shift ;;
    --strict-base) STRICT_BASE="true"; shift ;;
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

# exact_tag_version <repo>
#
# Echoes the tag HEAD is exactly on, or nothing when HEAD is not on a tag.
# Same rule as hack/version.sh: a v* tag first, then any exact tag. A
# preview or stable tag shares its commit with a nightly tag, and among
# lightweight tags git describe picks the first in refname order
# (nightly-* before v*), so a plain --exact-match would name the nightly.
exact_tag_version() {
  git -C "$1" describe --tags --exact-match --match 'v*' 2>/dev/null \
    || git -C "$1" describe --tags --exact-match 2>/dev/null \
    || true
}

# Compute git metadata once. Both used directly (in build-args, substitutions)
# and to build the :<short-sha> tag.
SHORT_SHA=""
COMMIT_SHA=""
VERSION=""
BUILD_TIME=""
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-}"
# An explicit SOURCE_DATE_EPOCH is a promise from the caller: a bad value is
# an error. One derived from git is best effort: a bad value warns and the
# build falls back to the build clock (pre-P2 behaviour).
SOURCE_DATE_EPOCH_FROM_ENV="false"
[[ -n "${SOURCE_DATE_EPOCH}" ]] && SOURCE_DATE_EPOCH_FROM_ENV="true"
if git -C "${REPO_ROOT}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  SHORT_SHA="$(git -C "${REPO_ROOT}" rev-parse --short HEAD 2>/dev/null || true)"
  COMMIT_SHA="$(git -C "${REPO_ROOT}" rev-parse HEAD 2>/dev/null || true)"
  # Same rule as hack/version.sh: VERSION is only set when HEAD is exactly
  # on a tag, and a v* tag wins over any other tag on the same commit (see
  # exact_tag_version). Off-tag, the embedded sciontool/scion Version falls
  # back to "dev" the same way a local `make build` off-tag does. The .git
  # directory is not in the docker build context (VCS stamping is disabled
  # there), so this has to be resolved on the host and threaded through as a
  # build-arg, the same way GIT_COMMIT already is. VERSION also drives the
  # HEAD-on-tag warning below.
  VERSION="$(exact_tag_version "${REPO_ROOT}")"
  # Build timestamps come from the commit time, not the wall clock, so two
  # builds of one commit stamp the same BuildTime. An explicit
  # SOURCE_DATE_EPOCH (reproducible-builds.org convention) wins.
  # The only git call here whose output the user's git config can change:
  # log.showSignature prints the signature check on stdout before %ct, and
  # color.ui=always could add escapes. --no-show-signature (git >= 2.10)
  # and --no-color pin it; --no-pager keeps a pager out even on a tty.
  # (The rev-parse/describe calls above print plumbing output that no
  # config key reformats.)
  if [[ -z "${SOURCE_DATE_EPOCH}" ]]; then
    SOURCE_DATE_EPOCH="$(git --no-pager -C "${REPO_ROOT}" log -1 --no-show-signature --no-color --format=%ct 2>/dev/null || true)"
  fi
fi
if [[ -n "${SOURCE_DATE_EPOCH}" && ! "${SOURCE_DATE_EPOCH}" =~ ^[0-9]+$ ]]; then
  if [[ "${SOURCE_DATE_EPOCH_FROM_ENV}" == "true" ]]; then
    echo "Error: SOURCE_DATE_EPOCH must be a Unix timestamp in seconds, got '${SOURCE_DATE_EPOCH}'" >&2
    exit 1
  fi
  echo "Warning: could not read the commit time (git log printed '$(printf '%s\n' "${SOURCE_DATE_EPOCH}" | head -n 1)'); using the build clock." >&2
  SOURCE_DATE_EPOCH=""
fi
if [[ -n "${SOURCE_DATE_EPOCH}" ]]; then
  # GNU/busybox date take -d @<epoch>; BSD (macOS) date takes -r <epoch>.
  BUILD_TIME="$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
    || date -u -r "${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || true)"
  # Exported so docker buildx (and BuildKit) stamp image timestamps with the
  # same instant.
  export SOURCE_DATE_EPOCH
fi
export SHORT_SHA COMMIT_SHA VERSION BUILD_TIME

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
echo "Version:  ${VERSION:-<none — HEAD is not on a tag>}"
echo "Built at: ${BUILD_TIME:-<build host clock>}${SOURCE_DATE_EPOCH:+ (SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH})}"
if [[ "${BUILDER_MODE}" == "per-image" ]]; then
  echo "Platforms: ${PLATFORMS:-<native>}"
  echo "Push:     ${PUSH}"
fi
if [[ "${BUILDER_MODE}" == "target" ]]; then
  # In target mode the builder submits one config for the whole target, and
  # that config can build more images than STEPS lists (e.g. thick-prep
  # maps to cloudbuild-thick.yaml, which builds the full thick chain). Show
  # the config and the image build steps it contains (ids "build-<image>";
  # setup steps such as verify-registry are skipped). Same declare -F guard
  # as warn_if_scion_base_not_in_run below.
  TARGET_CONFIG=""
  TARGET_BUILDS=""
  if declare -F cloud_build_config_for_target >/dev/null \
    && TARGET_CONFIG="$(cloud_build_config_for_target "${TARGET}" 2>/dev/null)" \
    && [[ -f "${TARGET_CONFIG}" ]]; then
    TARGET_BUILDS="$(sed -n "s/^[[:space:]]*id:[[:space:]]*'build-\([^']*\)'.*/\1/p" \
      "${TARGET_CONFIG}" 2>/dev/null | tr '\n' ' ' || true)"
    TARGET_BUILDS="${TARGET_BUILDS% }"
  fi
  if [[ -n "${TARGET_BUILDS}" ]]; then
    echo "Config:   ${TARGET_CONFIG#"${REPO_ROOT}"/}"
    echo "Steps:    ${TARGET_BUILDS}  (image builds in config)"
  else
    echo "Steps:    ${STEPS[*]}"
    echo "Note:     no readable ${BUILDER} config was found for target"
    echo "          '${TARGET}'; if one is mapped, it may build more images"
    echo "          than the steps listed above."
  fi
else
  echo "Steps:    ${STEPS[*]}"
fi
if [[ "${DRY_RUN}" == "true" ]]; then
  echo "(dry-run: no commands will be executed)"
fi
echo ""

# warn_if_release_tag_not_used
#
# HEAD is exactly on a release tag but --tag was not given, so the images
# get the default :latest. Warn, don't change the default: existing
# workflows that build :latest from a tag checkout keep working.
warn_if_release_tag_not_used() {
  if [[ -z "${VERSION}" || "${TAG_EXPLICIT}" == "true" || "${TAG}" != "latest" ]]; then
    return 0
  fi
  echo "Warning: HEAD is exactly on tag ${VERSION}, but --tag was not given," >&2
  echo "  so these images are tagged :latest${SHORT_SHA:+ (and :${SHORT_SHA})}, not :${VERSION}." >&2
  echo "  To tag them as this release, re-run with: --tag ${VERSION}" >&2
  echo "" >&2
}
warn_if_release_tag_not_used

# required_base_for_target <target>
#
# Echoes the base image a target stacks on without building it, or nothing
# for targets that build their own base. Per-image mode follows the step DAG
# (step_parent): scion-base and common need core-base (thick-prep under
# THICK_BUILD); harnesses, hub and omni need scion-base; a single step needs
# its own parent (e.g. scion-grok-build for scion-omni under OMNI_BUILD).
# Target mode follows the static Cloud Build configs, which ignore
# THICK_BUILD/OMNI_BUILD: cloudbuild-scion-base.yaml and -common.yaml build
# on core-base, the harness and hub configs on scion-base. This is the
# pre-flight mapping of build-release-images.sh, made exact per builder.
required_base_for_target() {
  case "$1" in
    all|core-base|thick-prep|thick) echo "" ;;
    scion-base|common)
      if [[ "${BUILDER_MODE}" == "target" ]]; then
        echo "core-base"
      else
        step_parent scion-base
      fi
      ;;
    harnesses|hub|omni) echo "scion-base" ;;
    *)
      if [[ "${BUILDER_MODE}" == "target" ]]; then
        echo "scion-base"
      else
        step_parent "$1"
      fi
      ;;
  esac
}

# probe_detail <tool> <output>
#
# One line describing a failed lookup: the first line that looks like an
# error (gcloud prints WARNING lines first), else the first line.
probe_detail() {
  local line
  line="$(printf '%s\n' "$2" | grep -m 1 -iE '(^|[^a-z])(error|fatal)' || true)"
  if [[ -z "${line}" ]]; then
    line="$(printf '%s\n' "$2" | head -n 1)"
  fi
  printf '%s: %s' "$1" "${line}"
}

# PROBE_ERROR_MARKERS: text that means a lookup FAILED (as opposed to the
# store or registry answering "not found"). Precedence rule, for every
# backend (gcloud, docker store/registry, podman store/registry): if the
# output contains ANY of these, it is a lookup error (warn and go ahead),
# EVEN IF a not-found marker also appears. Only output with a not-found
# marker and no error marker is a definite "not found". Covered: credentials
# and permissions; timeouts and network/DNS/TLS failures; HTTP 429 and 5xx;
# a crashed tool or unreachable daemon; and multi-source messages
# (podman/containers-image registries.conf mirrors: "(Mirrors also failed:
# [...: manifest unknown]): <primary>: <error>"), which are lookup errors
# even when every source says not found, because which source is
# authoritative cannot be told safely. Matched case-insensitively.
PROBE_ERROR_MARKERS='executable file not found|command not found|credential|unauthori[sz]ed|denied|forbidden|permission|Mirrors also failed|timeout|timed out|deadline exceeded|toomanyrequests|too many requests|rate limit|429 |status:? 429|status:? 5[0-9][0-9]|Internal Server Error|Service Unavailable|Bad Gateway|connection (refused|reset)|network is unreachable|no such host|dial tcp|tls handshake|x509|certificate|unexpected EOF|crashed|Traceback|Cannot connect to (the )?(Docker|Podman)|Is the docker daemon running'

# mask_ref <output> <ref>
#
# Echoes <output> with the looked-up reference masked, so words inside the
# registry path or tag (a ":timeout-fix" or ":permission-fix" tag, a
# "credentials-prod" project) cannot pass for error markers. Replaces, as
# literal text: the image path, then ":<tag>" and podman's "manifest <tag> "
# (together these cover the full ref). awk's index() does literal matching portably (bash 3.2
# pattern substitution with quoted patterns is not relied on).
mask_ref() {
  local out="$1" ref="$2" image tag=""
  if [[ -z "${ref}" ]]; then
    printf '%s\n' "${out}"
    return 0
  fi
  image="${ref}"
  if [[ "${ref##*/}" == *:* ]]; then
    image="${ref%:*}"
    tag="${ref##*:}"
  fi
  printf '%s\n' "${out}" | awk -v b="${image}" -v t="${tag}" '
    function rep(str, from, to,   res, i) {
      if (from == "") return str
      res = ""
      while ((i = index(str, from)) > 0) {
        res = res substr(str, 1, i - 1) to
        str = substr(str, i + length(from))
      }
      return res str
    }
    {
      l = rep($0, b, "<image>")
      if (t != "") {
        l = rep(l, ":" t, ":<tag>")
        l = rep(l, "manifest " t " ", "manifest <tag> ")
      }
      print l
    }'
}

# classify_probe_failure <kind> <output> [<ref>]
#
# Returns 1 when a failed lookup is a definite "not found" from the store or
# registry it asked, 2 for anything else; sets PROBE_DETAIL. Only each tool's
# own not-found answer counts, and a credential or tool failure never does,
# even when its text says "not found" (docker's
# 'exec: "docker-credential-gcloud": executable file not found in $PATH').
PROBE_DETAIL=""
classify_probe_failure() {
  local kind="$1" tool="${1%%-*}" out
  PROBE_DETAIL="$(probe_detail "${tool}" "$2")"
  out="$(mask_ref "$2" "${3:-}")"
  # Error-first precedence (see PROBE_ERROR_MARKERS). Here-strings, not
  # printf | grep: under pipefail an early grep exit could SIGPIPE printf
  # and skip this check.
  if grep -qiE "${PROBE_ERROR_MARKERS}" <<<"${out}"; then
    return 2
  fi
  local pattern
  case "${kind}" in
    docker-store)    pattern='No such image' ;;
    podman-store)    pattern='image not known|[Nn]o such image' ;;
    docker-registry) pattern='manifest unknown|: not found$' ;;
    podman-registry) pattern='manifest unknown|no such manifest|: not found$' ;;
    gcloud)          pattern='^ERROR: .*NOT_FOUND' ;;
    *)               return 2 ;;
  esac
  if grep -qE "${pattern}" <<<"${out}"; then
    return 1
  fi
  return 2
}

# probe_base <kind> <ref> <cmd...>
#
# Runs one lookup command for <ref>. Returns 0 if it found the image,
# otherwise classify_probe_failure's answer.
probe_base() {
  local kind="$1" ref="$2" out
  shift 2
  if out="$("$@" 2>&1 >/dev/null </dev/null)"; then
    return 0
  fi
  classify_probe_failure "${kind}" "${out}" "${ref}"
}

# probe_gcloud_tag <image> <tag>
#
# Artifact Registry lookup for cloud-build. `gcloud artifacts docker tags
# list` needs only Artifact Registry read; `images describe` also lists
# Container Analysis occurrences (containeranalysis.occurrences.list), which
# the identity running scheduled Cloud Build rebuilds does not have. The tag
# must match a listed tag exactly, as a bare name or as the last element of
# the tag's resource name (".../tags/<tag>"): no substring or filter match,
# so :v1 is not satisfied by :v10. Same return codes as probe_base.
probe_gcloud_tag() {
  local image="$1" tag="$2" outf errf rc=0
  outf="$(mktemp "${TMPDIR:-/tmp}/build-images-probe.XXXXXX")"
  errf="$(mktemp "${TMPDIR:-/tmp}/build-images-probe.XXXXXX")"
  # --user-output-enabled: with core/user_output_enabled=false (config or
  # CLOUDSDK_CORE_USER_OUTPUT_ENABLED) gcloud prints nothing and exits 0,
  # which would read as "no tags". The flag overrides both.
  # The env prefix and --verbosity=error pin gcloud's stderr to plain
  # "ERROR: ..." lines whatever the user's config says (structured JSON
  # logs, a timestamped console format, or verbosity none would otherwise
  # hide NOT_FOUND). CLOUDSDK_* env beats the config file; flags beat both.
  CLOUDSDK_CORE_SHOW_STRUCTURED_LOGS=never CLOUDSDK_CORE_CONSOLE_LOG_FORMAT=standard \
    gcloud artifacts docker tags list "${image}" --format='value(tag)' --user-output-enabled \
    --verbosity=error >"${outf}" 2>"${errf}" </dev/null || rc=$?
  local err
  err="$(cat "${errf}")"
  if [[ ${rc} -ne 0 ]]; then
    rm -f -- "${outf}" "${errf}"
    classify_probe_failure gcloud "${err}" "${image}"
    return $?
  fi
  # rc 0. Exact match = found. No output at all = definite not-found (an
  # image with no tags lists nothing), unless gcloud printed an ERROR line
  # on stderr. Output with no non-blank line (whitespace or CRs only) is a
  # shape this script does not understand (e.g. a renamed field rendering
  # empty values): a lookup error, not a not-found. Non-blank lines and no
  # exact match = definite not-found.
  local found=1 line
  if [[ ! -s "${outf}" ]]; then
    if grep -q '^ERROR' <<<"${err}"; then
      found=2
      PROBE_DETAIL="$(probe_detail gcloud "${err}")"
    fi
  elif ! grep -q '[^[:space:]]' "${outf}"; then
    found=2
    PROBE_DETAIL="gcloud: tags list printed only blank lines for ${image}"
  else
    # "|| [[ -n ... ]]" also reads a last line without a trailing newline.
    while IFS= read -r line || [[ -n "${line}" ]]; do
      line="${line%$'\r'}"   # CRLF output (a Windows gcloud)
      [[ -n "${line}" ]] || continue
      if [[ "${line}" == "${tag}" || "${line}" == */tags/"${tag}" ]]; then
        found=0
        break
      fi
    done <"${outf}"
  fi
  rm -f -- "${outf}" "${errf}"
  return "${found}"
}

# base_image_present <ref>
#
# Returns 0 if any lookup finds <ref>; 1 if it is definitely missing; 2 if it
# could not be checked, with the reason in BASE_CHECK_DETAIL.
#
# Definitely missing means: with a registry set, a registry lookup answered
# "not found" (a local-store miss alone proves nothing about the registry the
# build may pull from); without one, the local store answered "not found".
#
# The local store counts only for local, non-push per-image builds: Cloud
# Build cannot see it, and a --push (or multi-platform) build runs on the
# docker-container buildx instance, which pulls BASE_IMAGE from the registry
# (same rule as warn_if_scion_base_not_in_run). For cloud-build, gcloud
# (guaranteed by builder_check) is asked first, then docker if it is on PATH.
BASE_CHECK_DETAIL=""
base_image_present() {
  local ref="$1"
  local -a probes=()
  if [[ "${BUILDER_MODE}" != "target" && "${PUSH}" != "true" ]]; then
    if [[ "${BUILDER}" == "local-podman" ]]; then
      probes+=("podman-store")
    else
      probes+=("docker-store")
    fi
  fi
  if [[ -n "${REGISTRY}" ]]; then
    case "${BUILDER}" in
      local-podman) probes+=("podman-registry") ;;
      cloud-build)  probes+=("gcloud" "docker-registry") ;;
      *)            probes+=("docker-registry") ;;
    esac
  fi

  local store_missing="false" registry_missing="false" kind rc
  BASE_CHECK_DETAIL=""
  for kind in ${probes[@]+"${probes[@]}"}; do
    if ! command -v "${kind%%-*}" >/dev/null 2>&1; then
      continue
    fi
    rc=0
    case "${kind}" in
      docker-store)    probe_base "${kind}" "${ref}" docker image inspect "${ref}" || rc=$? ;;
      podman-store)    probe_base "${kind}" "${ref}" podman image inspect "${ref}" || rc=$? ;;
      docker-registry) probe_base "${kind}" "${ref}" docker buildx imagetools inspect "${ref}" || rc=$? ;;
      podman-registry) probe_base "${kind}" "${ref}" podman manifest inspect "${ref}" || rc=$? ;;
      # <ref> is <image>:<tag>; a tag never contains ':' (a registry port can).
      gcloud)          probe_gcloud_tag "${ref%:*}" "${ref##*:}" || rc=$? ;;
    esac
    case "${rc}" in
      0) return 0 ;;
      1)
        if [[ "${kind}" == *-store ]]; then
          store_missing="true"
        else
          registry_missing="true"
        fi
        ;;
      *) BASE_CHECK_DETAIL="${BASE_CHECK_DETAIL:+${BASE_CHECK_DETAIL}; }${PROBE_DETAIL}" ;;
    esac
  done
  if [[ "${registry_missing}" == "true" ]]; then
    return 1
  fi
  if [[ "${store_missing}" == "true" && -z "${REGISTRY}" ]]; then
    return 1
  fi
  if [[ -z "${BASE_CHECK_DETAIL}" ]]; then
    BASE_CHECK_DETAIL="no docker, podman or gcloud on PATH"
  fi
  return 2
}

# check_required_base
#
# Base guard: a target that stacks on core-base:<tag> or scion-base:<tag>
# without building it stops here when that image is definitely missing,
# instead of building on whatever base the builder happens to resolve
# (ptone/scion#2250). A lookup that fails (permission, API, network, tool)
# never stops a build that would have worked before: it warns and goes ahead,
# unless --strict-base asks for a hard gate. --allow-stale-base skips the
# check; --dry-run only warns.
check_required_base() {
  local base
  base="$(required_base_for_target "${TARGET}")"
  if [[ -z "${base}" ]]; then
    return 0
  fi

  # In target mode the static config may build the base itself
  # (cloudbuild-omni.yaml rebuilds the chain from thick-prep). Same check as
  # warn_if_scion_base_not_in_run below.
  if [[ "${BUILDER_MODE}" == "target" ]] && declare -F cloud_build_config_for_target >/dev/null; then
    local target_config
    if target_config="$(cloud_build_config_for_target "${TARGET}" 2>/dev/null)" \
      && [[ -f "${target_config}" ]] \
      && grep -q "id: 'build-${base}'" "${target_config}"; then
      return 0
    fi
  fi

  local prefix=""
  [[ -n "${REGISTRY}" ]] && prefix="${REGISTRY}/"
  local ref="${prefix}${base}:${TAG}"

  if [[ "${ALLOW_STALE_BASE}" == "true" ]]; then
    echo "Base:     ${ref} (not checked: --allow-stale-base)"
    echo ""
    return 0
  fi

  local rc=0
  base_image_present "${ref}" || rc=$?
  if [[ ${rc} -eq 0 ]]; then
    echo "Base:     ${ref} (found)"
    echo ""
    return 0
  fi

  if [[ ${rc} -eq 2 ]]; then
    local problem="could not be checked (${BASE_CHECK_DETAIL})"
    if [[ "${STRICT_BASE}" != "true" ]]; then
      echo "Warning: --target ${TARGET} stacks on ${ref}, which ${problem}." >&2
      echo "  Going ahead: the builder resolves ${ref} itself, as it did before" >&2
      echo "  this check existed. Pass --strict-base to stop on lookup failures." >&2
      echo "" >&2
      return 0
    fi
    if [[ "${DRY_RUN}" == "true" ]]; then
      echo "Warning: --target ${TARGET} stacks on ${ref}, which ${problem}." >&2
      echo "  A real build would stop here (--strict-base). Fix the lookup" >&2
      echo "  (credentials, network, tool) or drop --strict-base." >&2
      echo "" >&2
      return 0
    fi
    echo "Error: --target ${TARGET} stacks on ${ref}, which ${problem}." >&2
    echo "  --strict-base is set. Fix the lookup (credentials, network, tool)," >&2
    echo "  or drop --strict-base to warn and go ahead." >&2
    exit 1
  fi

  local where="in the local image store"
  if [[ "${BUILDER_MODE}" == "target" || "${PUSH}" == "true" ]]; then
    where="in registry ${REGISTRY}"
  elif [[ -n "${REGISTRY}" ]]; then
    where="locally or in registry ${REGISTRY}"
  fi
  if [[ "${DRY_RUN}" == "true" ]]; then
    echo "Warning: --target ${TARGET} stacks on ${ref}, which was not found ${where}." >&2
    echo "  A real build would stop here. Build ${base} first (--target ${base} --tag ${TAG})," >&2
    echo "  use --target all, or pass --allow-stale-base." >&2
    echo "" >&2
    return 0
  fi
  echo "Error: --target ${TARGET} stacks on ${ref}, which was not found ${where}." >&2
  echo "  Build ${base} first (--target ${base} --tag ${TAG}), use --target all," >&2
  echo "  or pass --allow-stale-base to build on whatever the builder resolves." >&2
  exit 1
}
check_required_base

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
    export BASE_TAG REGISTRY TAG SHORT_SHA COMMIT_SHA VERSION BUILD_TIME

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
