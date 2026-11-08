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

# build-release-images.sh: build the Scion image set from the image specs of a
# release tag (ptone/scion#6625, Layer 1).
#
# The recipe is the tag's tree; this driver lives outside it. It clones the
# tag and runs THAT tree's image-build/scripts/build-images.sh, forwarding only
# the flags every supported tag understands.
#
# This file is deliberately standalone: it sources nothing, it runs under
# bash 3.2 (macOS /bin/bash), and it needs only git and docker or podman, so
# it can be fetched and run as a single file:
#
#   curl -fsSLO https://raw.githubusercontent.com/GoogleCloudPlatform/scion/main/image-build/scripts/build-release-images.sh
#   bash build-release-images.sh v0.3.0-preview.5

set -euo pipefail

PROG="build-release-images.sh"
DEFAULT_REPO="https://github.com/GoogleCloudPlatform/scion"

die() { echo "${PROG}: error: $*" >&2; exit 1; }
usage_err() { echo "${PROG}: error: $*" >&2; echo "Run '${PROG} --help' for usage." >&2; exit 2; }
note() { echo "${PROG}: $*"; }
warn() { echo "${PROG}: warning: $*" >&2; }

usage() {
  cat <<'EOF'
Usage: build-release-images.sh [<version> | --ref <git-ref>] [options]
                               [-- <passthrough>]

Build the Scion container images (core-base, scion-base, harnesses, hub) from
the image specs of a release tag. The tag's tree is cloned and its own
image-build/scripts/build-images.sh is run with --target all --tag <version>,
so the images are tagged :<version> (plus :<short-sha>). From
v0.3.0-preview.4 on, the scion and sciontool binaries in them also report
<version>; older releases (v0.3.0-preview.1 to .3) do not stamp a version, so
their binaries report "dev" (the driver warns and records
"version_stamped": false in the manifest).

Version selection (one of):
  <version>             A release tag, e.g. v0.3.0-preview.5. It must exist as
                        a tag in --repo. Nightly tags (nightly-YYYYMMDD) are
                        deleted after 14 days; an expired one fails with
                        "tag not found".
  (none)                Use the "version" field of `scion version --format json`
                        for the scion binary on PATH. Fails if scion is not on
                        PATH or reports "dev".
  --ref <git-ref>       Build an arbitrary branch, tag or full commit SHA. The
                        images are tagged with the commit's short SHA and the
                        binaries inside report "dev" (no tag is fetched).

Options:
  --repo <url>          Git repository to clone (default:
                        https://github.com/GoogleCloudPlatform/scion). Forks
                        must pass their own URL.
  --src-dir <dir>       Where to clone the source (default:
                        ${XDG_CACHE_HOME:-~/.cache}/scion/image-src/<version>).
                        An existing clone is reused only when its HEAD is the
                        tag's commit and `git describe` resolves to the tag;
                        any other existing, non-empty directory is an error
                        (nothing is ever deleted).
  --also-tag latest     Also tag every built image :latest (docker/podman tag
                        for local images, imagetools create or podman push for
                        pushed ones). Use this until the image_tag setting
                        lands. With --registry it requires --force-latest.
                        Not with --builder cloud-build, which only submits the
                        build: tag :latest after the build has finished.
  --force-latest        Allow --also-tag latest to move :latest in a registry.
  --channel-tag preview|stable
                        Move a channel tag on the pushed images. The value is
                        validated, but the behaviour is NOT YET IMPLEMENTED
                        (Phase 5): passing it exits with an error before
                        anything is cloned or built.
  -h, --help            Show this help.

Passthrough (after --), forwarded to the tag's build-images.sh. Only the flag
set that every supported tag understands is accepted:
  --target <t>          Default: all. Group targets: core-base, scion-base,
                        harnesses, hub, omni, common, all, thick-prep, thick.
                        Single-image targets such as scion-claude need
                        v0.3.0-preview.4 or later (older releases only accept
                        group targets; the driver checks the tree and stops).
                        A target that stacks on an existing base
                        (scion-base, common, harnesses, hub, omni, or a single
                        image such as scion-claude) is allowed only when
                        core-base:<version> (scion-base and common) or
                        scion-base:<version> (everything else) exists locally
                        or in the registry. The check needs docker (with
                        buildx for registry images) or podman on PATH; a dry
                        run only warns.
  --registry <path>     Registry prefix, e.g. ghcr.io/myorg.
  --platform <p>        all, or e.g. linux/amd64,linux/arm64.
  --push                Push the images.
  --builder <b>         local-docker (default), local-podman or cloud-build.
                        local-podman makes this driver use podman for its own
                        inspect and tag commands.
  --dry-run             Print the build steps without building. No tagging,
                        no manifest.
--tag is owned by this driver and is rejected. Any other flag is rejected
because older tags' build-images.sh does not understand it.

Output: a summary of image refs with their digests (pushed images) or local
image IDs, and the source commit, also written to
<src-dir>/build-manifest.json as {version, commit, built_at, builder, pushed,
submitted, version_stamped, images: {name: {ref, digest}}}. With
--builder cloud-build the build is only submitted (asynchronously): the
manifest then has "submitted": true, "pushed": false and empty digests.
When that file already describes the same
version, commit, builder and push mode (a staged build: core-base, then
scion-base, then a harness, with the same passthrough flags), the new images
are merged into it; otherwise it is rewritten. A reused clone must have no
modified or untracked files (build-manifest.json excepted).

Reproducibility: same recipe, not same bits. The Dockerfiles, install scripts
and scion source are exactly those of the tag, but floating inputs (harness
CLIs installed at @latest, apt packages, base images by tag, kubectl
stable.txt and similar) resolve at build time. Two builds of the same version
a week apart can differ. build-manifest.json records what this build actually
produced.

Examples:
  build-release-images.sh             # the version of `scion` on PATH
  build-release-images.sh v0.3.0-preview.5 -- --target scion-claude
  build-release-images.sh v0.3.0-preview.5 -- \
    --registry ghcr.io/myorg --platform all --push
  build-release-images.sh v0.3.0-preview.5 --also-tag latest
EOF
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------

VERSION_ARG=""
REF=""
REPO="${DEFAULT_REPO}"
SRC_DIR=""
ALSO_TAG=""
FORCE_LATEST="false"
CHANNEL_TAG=""
PASSTHROUGH=()

need_val() {
  # need_val <flag> <remaining-arg-count>
  if [ "$2" -lt 2 ]; then
    usage_err "$1 needs a value"
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --ref) need_val "$1" $#; REF="$2"; shift 2 ;;
    --repo) need_val "$1" $#; REPO="$2"; shift 2 ;;
    --src-dir) need_val "$1" $#; SRC_DIR="$2"; shift 2 ;;
    --also-tag) need_val "$1" $#; ALSO_TAG="$2"; shift 2 ;;
    --force-latest) FORCE_LATEST="true"; shift ;;
    --channel-tag) need_val "$1" $#; CHANNEL_TAG="$2"; shift 2 ;;
    --) shift; PASSTHROUGH=("$@"); break ;;
    -*) usage_err "unknown option: $1 (build-images.sh flags go after --)" ;;
    *)
      if [ -n "${VERSION_ARG}" ]; then
        usage_err "unexpected argument: $1 (only one version may be given)"
      fi
      VERSION_ARG="$1"; shift ;;
  esac
done

if [ -n "${VERSION_ARG}" ] && [ -n "${REF}" ]; then
  usage_err "give either <version> or --ref, not both"
fi
if [ -n "${ALSO_TAG}" ] && [ "${ALSO_TAG}" != "latest" ]; then
  usage_err "--also-tag only accepts 'latest' (got '${ALSO_TAG}')"
fi
if [ "${FORCE_LATEST}" = "true" ] && [ "${ALSO_TAG}" != "latest" ]; then
  usage_err "--force-latest only applies together with --also-tag latest"
fi
if [ -n "${CHANNEL_TAG}" ]; then
  case "${CHANNEL_TAG}" in
    preview|stable) die "--channel-tag ${CHANNEL_TAG}: not yet implemented (it lands with registry publishing, Phase 5)" ;;
    *) usage_err "--channel-tag must be 'preview' or 'stable' (got '${CHANNEL_TAG}')" ;;
  esac
fi
if [ -z "${REPO}" ]; then
  usage_err "--repo must not be empty"
fi
# Values that start with '-' would reach git (and mkdir, mv, ...) as options,
# e.g. --repo '--upload-pack=<cmd>'. Reject them outright.
for v in "${REPO}" "${REF}" "${SRC_DIR}" "${VERSION_ARG}"; do
  case "${v}" in
    -*) usage_err "argument values must not start with '-' (got '${v}')" ;;
  esac
done

# Passthrough: forward only the base flag set every supported tag accepts
# (v0.3.0-preview.1 onward: --builder --registry --target --platform --push
# --dry-run). --tag is owned by the driver. --continue-on-error is rejected
# because v0.3.0-preview.1's build-images.sh does not know it.
P_TARGET="all"
P_REGISTRY=""
P_REGISTRY_SET="false"
P_PLATFORM=""
P_BUILDER="local-docker"
P_PUSH="false"
P_DRY_RUN="false"
FORWARD=()
set -- ${PASSTHROUGH[@]+"${PASSTHROUGH[@]}"}
while [ $# -gt 0 ]; do
  case "$1" in
    --target|--registry|--platform|--builder)
      need_val "$1" $#
      case "$2" in
        -*) usage_err "passthrough $1 value must not start with '-' (got '$2')" ;;
      esac
      case "$1" in
        --target) P_TARGET="$2" ;;
        --registry) P_REGISTRY="$2"; P_REGISTRY_SET="true" ;;
        --platform) P_PLATFORM="$2" ;;
        --builder) P_BUILDER="$2" ;;
      esac
      if [ "$1" != "--target" ]; then
        FORWARD+=("$1" "$2")
      fi
      shift 2 ;;
    --push) P_PUSH="true"; FORWARD+=("$1"); shift ;;
    --dry-run) P_DRY_RUN="true"; FORWARD+=("$1"); shift ;;
    --tag) usage_err "passthrough --tag is not allowed: the driver tags images with the version" ;;
    *) usage_err "passthrough flag '$1' is not forwarded: only --target --registry --platform --push --builder --dry-run are supported by every release's build-images.sh" ;;
  esac
done

if [ -z "${P_TARGET}" ]; then
  usage_err "--target must not be empty"
fi

# The tag's build-images.sh falls back to SCION_IMAGE_REGISTRY when --registry
# is not passed; mirror that so pre-flight and the summary use the same refs.
EFF_REGISTRY="${P_REGISTRY}"
if [ "${P_REGISTRY_SET}" != "true" ] && [ -n "${SCION_IMAGE_REGISTRY:-}" ]; then
  EFF_REGISTRY="${SCION_IMAGE_REGISTRY}"
fi
EFF_REGISTRY="${EFF_REGISTRY%/}"
IMG_PREFIX=""
if [ -n "${EFF_REGISTRY}" ]; then
  IMG_PREFIX="${EFF_REGISTRY}/"
fi

# Are the images pushed (registry digests) or kept local (image IDs)?
PUSHED="false"
case "${P_PUSH}:${P_BUILDER}:${P_PLATFORM}" in
  true:*) PUSHED="true" ;;
  *:cloud-build:*) PUSHED="true" ;;
  *:*:all|*:*:*,*) PUSHED="true" ;;
esac

# Every supported tag's cloud-build builder runs `gcloud builds submit --async`
# and returns at once: when build-images.sh exits, the images do not exist yet.
# The driver therefore treats cloud-build as submit-only: no :latest move, no
# digest lookup, and a manifest marked "submitted". (Phase 5's --channel-tag
# on cloud-build needs to wait for the build first; this is its seam.)
SUBMITTED="false"
if [ "${P_BUILDER}" = "cloud-build" ] && [ "${P_DRY_RUN}" != "true" ]; then
  SUBMITTED="true"
fi
if [ "${ALSO_TAG}" = "latest" ] && [ "${P_BUILDER}" = "cloud-build" ]; then
  usage_err "--also-tag latest does not work with --builder cloud-build: the build is submitted asynchronously, so :latest can only be moved after it has finished (e.g. docker buildx imagetools create -t <registry>/<image>:latest <registry>/<image>:<version>)"
fi

if [ "${ALSO_TAG}" = "latest" ] && [ -n "${EFF_REGISTRY}" ] && [ "${FORCE_LATEST}" != "true" ]; then
  die "--also-tag latest with a registry (${EFF_REGISTRY}) would move that registry's :latest; add --force-latest if that is intended"
fi

CTR="docker"
if [ "${P_BUILDER}" = "local-podman" ]; then
  CTR="podman"
fi

# ---------------------------------------------------------------------------
# Version resolution
# ---------------------------------------------------------------------------

# Docker tag grammar; also keeps the version safe as a path component.
TAG_RE='^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$'
SHA_RE='^[0-9a-f]{40}$'

MODE="tag"
VERSION=""
if [ -n "${REF}" ]; then
  MODE="ref"
elif [ -n "${VERSION_ARG}" ]; then
  VERSION="${VERSION_ARG}"
else
  if ! command -v scion >/dev/null 2>&1; then
    die "no version given and 'scion' is not on PATH; pass a version (e.g. ${PROG} v0.3.0-preview.5) or --ref <git-ref>"
  fi
  scion_json="$(scion version --format json 2>/dev/null)" \
    || die "'scion version --format json' failed; pass a version or --ref <git-ref>"
  # Portable JSON field read, no jq: the first "version": "<value>" pair.
  VERSION="$(printf '%s\n' "${scion_json}" \
    | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
  if [ -z "${VERSION}" ] || [ "${VERSION}" = "dev" ]; then
    die "the scion binary on PATH reports version '${VERSION:-<missing>}', which is not a release; pass a version (e.g. ${PROG} v0.3.0-preview.5) or --ref <git-ref>"
  fi
  note "using the version of the scion binary on PATH: ${VERSION}"
fi

EXPECT_SHA=""
if [ "${MODE}" = "tag" ]; then
  if ! [[ "${VERSION}" =~ ${TAG_RE} ]]; then
    usage_err "'${VERSION}' is not a valid tag name"
  fi
  ls_out="$(git ls-remote --tags -- "${REPO}" "refs/tags/${VERSION}" "refs/tags/${VERSION}^{}")" \
    || die "git ls-remote ${REPO} failed (network or repository URL?)"
  # Prefer the peeled commit of an annotated tag.
  EXPECT_SHA="$(printf '%s\n' "${ls_out}" | awk -v r="refs/tags/${VERSION}^{}" '$2 == r { print $1; exit }')"
  if [ -z "${EXPECT_SHA}" ]; then
    EXPECT_SHA="$(printf '%s\n' "${ls_out}" | awk -v r="refs/tags/${VERSION}" '$2 == r { print $1; exit }')"
  fi
  if [ -z "${EXPECT_SHA}" ]; then
    die "tag not found: ${VERSION} does not exist in ${REPO} (nightly tags are deleted after 14 days; forks need --repo)"
  fi
else
  if [ -z "${REF}" ]; then
    usage_err "--ref must not be empty"
  fi
  if [[ "${REF}" =~ ${SHA_RE} ]]; then
    EXPECT_SHA="${REF}"
  else
    ls_out="$(git ls-remote -- "${REPO}" "refs/heads/${REF}" "refs/tags/${REF}" "refs/tags/${REF}^{}")" \
      || die "git ls-remote ${REPO} failed (network or repository URL?)"
    for r in "refs/heads/${REF}" "refs/tags/${REF}^{}" "refs/tags/${REF}"; do
      EXPECT_SHA="$(printf '%s\n' "${ls_out}" | awk -v r="${r}" '$2 == r { print $1; exit }')"
      if [ -n "${EXPECT_SHA}" ]; then
        break
      fi
    done
    if [ -z "${EXPECT_SHA}" ]; then
      die "--ref ${REF}: no branch or tag of that name in ${REPO} (pass a branch, a tag or a full 40-hex commit SHA)"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# Source: clone (or reuse) the tree
# ---------------------------------------------------------------------------

if [ -z "${SRC_DIR}" ]; then
  cache_root="${XDG_CACHE_HOME:-}"
  if [ -z "${cache_root}" ]; then
    cache_root="${HOME:?HOME is not set; pass --src-dir}/.cache"
  fi
  if [ "${MODE}" = "tag" ]; then
    SRC_DIR="${cache_root}/scion/image-src/${VERSION}"
  else
    SRC_DIR="${cache_root}/scion/image-src/ref-$(printf '%s' "${EXPECT_SHA}" | cut -c1-12)"
  fi
fi
SRC_DIR="${SRC_DIR%/}"
: "${SRC_DIR:?--src-dir must not be empty or /}"

# check_tree <dir>: the tree is the expected commit and describes correctly.
# In tag mode `git describe --tags --exact-match` must print exactly the
# version (the tag's build-images.sh stamps VERSION that way, and several tags
# can share one commit). In ref mode it must fail, so the binaries report dev.
check_tree() {
  local d="${1:?}" head desc
  head="$(git -C "${d}" rev-parse HEAD 2>/dev/null)" || { echo "not a git checkout"; return 1; }
  if [ "${head}" != "${EXPECT_SHA}" ]; then
    echo "HEAD is ${head}, expected ${EXPECT_SHA}"; return 1
  fi
  if [ "${MODE}" = "tag" ]; then
    desc="$(git -C "${d}" describe --tags --exact-match 2>/dev/null || true)"
    if [ "${desc}" != "${VERSION}" ]; then
      echo "git describe --tags --exact-match gives '${desc}', expected '${VERSION}' (another tag on the same commit?)"; return 1
    fi
  else
    if desc="$(git -C "${d}" describe --tags --exact-match 2>/dev/null)"; then
      echo "the checkout carries tag '${desc}', so the binaries would not report dev"; return 1
    fi
  fi
  # Modified or untracked files would enter the build context unchecked
  # (scion-base compiles the tree). The driver's own build-manifest.json is
  # the one expected untracked file; ignored files are not listed.
  local st_raw st n
  if ! st_raw="$(git -C "${d}" status --porcelain --untracked-files=normal 2>/dev/null)"; then
    echo "git status failed in the checkout, so it cannot be checked for modified or untracked files"; return 1
  fi
  st="$(printf '%s\n' "${st_raw}" | grep -vxF '?? build-manifest.json' || true)"
  if [ -n "${st}" ]; then
    n="$(printf '%s\n' "${st}" | grep -c . || true)"
    echo "the checkout has modified or untracked files: $(printf '%s\n' "${st}" | head -n 10 | cut -c4- | tr '\n' ' ')$([ "${n}" -gt 10 ] && echo "(and $((n - 10)) more)")"
    return 1
  fi
  return 0
}

dir_is_empty() {
  local d="${1:?}"
  [ -z "$(ls -A "${d}" 2>/dev/null)" ]
}

CLONE_TMP=""
MANIFEST_TMP=""
cleanup() {
  # Only ever removes the driver's own mktemp directory and temp manifest.
  if [ -n "${CLONE_TMP}" ] && [ -d "${CLONE_TMP}" ]; then
    rm -rf -- "${CLONE_TMP:?}"
  fi
  if [ -n "${MANIFEST_TMP}" ] && [ -f "${MANIFEST_TMP}" ]; then
    rm -f -- "${MANIFEST_TMP:?}"
  fi
}
trap cleanup EXIT

if [ -e "${SRC_DIR}" ] && ! dir_is_empty "${SRC_DIR}"; then
  if ! why="$(check_tree "${SRC_DIR}")"; then
    die "--src-dir ${SRC_DIR} exists and cannot be reused: ${why}. Remove it yourself or pass a different --src-dir."
  fi
  note "reusing existing clone ${SRC_DIR} at ${EXPECT_SHA}"
else
  parent="$(dirname "${SRC_DIR:?}")"
  mkdir -p "${parent:?}"
  CLONE_TMP="$(mktemp -d "${parent:?}/.scion-image-src.XXXXXX")"
  # The clone gets the mktemp dir's unique name, so that a nested move (see
  # below) can be recognised by that name.
  work_name="$(basename "${CLONE_TMP:?}")"
  work="${CLONE_TMP:?}/${work_name:?}"
  if [ "${MODE}" = "tag" ]; then
    note "cloning ${REPO} at ${VERSION} into ${SRC_DIR}"
    # --no-tags, then exactly this one tag: a plain clone follows every tag on
    # the commit (e.g. nightly-YYYYMMDD next to a preview tag), and
    # `git describe` would then stamp the wrong name.
    git -c advice.detachedHead=false clone -q --depth 1 --no-tags --branch "${VERSION}" -- "${REPO}" "${work}" \
      || die "git clone of ${VERSION} from ${REPO} failed"
    git -C "${work}" fetch -q --depth 1 --no-tags -- origin "+refs/tags/${VERSION}:refs/tags/${VERSION}" \
      || die "fetching refs/tags/${VERSION} failed"
  else
    note "fetching ${REPO} at ${REF} (${EXPECT_SHA}) into ${SRC_DIR}"
    git init -q "${work}" || die "git init failed"
    # By commit SHA, with --no-tags: no tag may land in a --ref checkout,
    # or the tree's build-images.sh would stamp that tag's name.
    git -C "${work}" fetch -q --depth 1 --no-tags -- "${REPO}" "${EXPECT_SHA}" \
      || die "fetching ${REF} (${EXPECT_SHA}) from ${REPO} failed"
    git -C "${work}" checkout -q --detach FETCH_HEAD 2>/dev/null || die "checkout of ${EXPECT_SHA} failed"
  fi
  if ! why="$(check_tree "${work}")"; then
    die "the fresh clone does not match: ${why}"
  fi
  if [ -d "${SRC_DIR}" ]; then
    if ! rmdir "${SRC_DIR:?}" 2>/dev/null; then
      if ! dir_is_empty "${SRC_DIR}"; then
        die "${SRC_DIR} was created by another run while this one was cloning; re-run to reuse it"
      fi
      die "could not replace the empty directory ${SRC_DIR}"
    fi
  fi
  mv "${work}" "${SRC_DIR:?}" || die "could not move the clone to ${SRC_DIR}"
  # If another run created SRC_DIR between the checks above and this mv, mv
  # moved the clone INTO it. Portable detection (no GNU mv -T): our uniquely
  # named clone then sits inside SRC_DIR. Remove our copy and stop.
  if [ -e "${SRC_DIR}/${work_name}" ]; then
    rm -rf -- "${SRC_DIR:?}/${work_name:?}"
    die "${SRC_DIR} was created by another run while this one was cloning; re-run to reuse it"
  fi
fi

BUILD_SCRIPT="${SRC_DIR:?}/image-build/scripts/build-images.sh"
[ -f "${BUILD_SCRIPT}" ] || die "${BUILD_SCRIPT} not found: this revision has no image-build/scripts/build-images.sh"

# Version stamping: a tree's build-images.sh derives VERSION from
# `git describe --tags --exact-match` and its scion-base Dockerfile passes it
# to the Go ldflags (ARG VERSION) only from v0.3.0-preview.4 on. Older trees
# still build and tag :<version>, but their binaries report "dev".
# tree_is_stamped and tree_has_step_targets are also run against main's own
# tree by the test (canary), so a refactor there breaks the test rather than
# mislabelling future releases.
tree_is_stamped() {
  local root="${1:?}"
  grep -q 'describe --tags --exact-match' "${root}/image-build/scripts/build-images.sh" 2>/dev/null \
    && grep -q '^ARG VERSION' "${root}/image-build/scion-base/Dockerfile" 2>/dev/null
}
# Single-image targets (e.g. scion-claude): resolve_targets falls back to the
# step IDs only from v0.3.0-preview.4 on.
tree_has_step_targets() {
  local root="${1:?}"
  # shellcheck disable=SC2016 # a literal line of targets.sh, not an expansion
  grep -qF 'for _id in "${ALL_STEP_IDS[@]}"' "${root}/image-build/scripts/lib/targets.sh" 2>/dev/null
}
# tree_stamps_version <root> <version>: the tree's own hack/version.sh, run
# in the tree, stamps exactly <version>. hack/version.sh and build-images.sh
# derive VERSION by the same rule (from v0.3.0-preview.4 on, and with the
# same release tag forms in later trees), so a tag that rule does not accept
# would give images whose binaries report "dev". The driver fails instead.
tree_stamps_version() {
  local root="${1:?}" want="${2:?}" ldflags
  [ -f "${root}/hack/version.sh" ] || return 1
  ldflags="$(cd "${root}" && bash ./hack/version.sh scionpkg)" || return 1
  case " ${ldflags} " in
    *" -X scionpkg.Version=${want} "*) return 0 ;;
  esac
  return 1
}

case "${P_TARGET}" in
  core-base|thick-prep|scion-base|harnesses|hub|omni|common|all|thick) ;;
  *)
    if ! tree_has_step_targets "${SRC_DIR}"; then
      die "--target ${P_TARGET} is a single-image target, which needs v0.3.0-preview.4 or later; this tree only accepts the group targets core-base, scion-base, harnesses, hub, omni, common, all, thick-prep and thick"
    fi
    ;;
esac

VERSION_STAMPED="false"
if [ "${MODE}" = "tag" ]; then
  if tree_is_stamped "${SRC_DIR}"; then
    VERSION_STAMPED="true"
    if ! tree_stamps_version "${SRC_DIR}" "${VERSION}"; then
      die "the tree at ${VERSION} would not stamp '${VERSION}' into its binaries: its hack/version.sh does not accept it as a release tag (or is missing), so the images would report 'dev'"
    fi
  else
    warn "${VERSION} predates version stamping (needs v0.3.0-preview.4 or later): the images are tagged :${VERSION}, but the scion and sciontool binaries inside report 'dev'"
  fi
fi

SHORT_SHA="$(git -C "${SRC_DIR}" rev-parse --short HEAD)"
IMAGE_TAG="${VERSION}"
if [ "${MODE}" = "ref" ]; then
  IMAGE_TAG="${SHORT_SHA}"
  VERSION="dev"
  note "--ref build: images are tagged :${IMAGE_TAG}; the binaries inside report 'dev'"
fi

# ---------------------------------------------------------------------------
# Base pre-flight
# ---------------------------------------------------------------------------

# image_present <ref>: local store first, then the registry when one is set.
image_present() {
  local ref="${1:?}"
  if "${CTR}" image inspect "${ref}" >/dev/null 2>&1; then
    return 0
  fi
  if [ -n "${EFF_REGISTRY}" ]; then
    if [ "${CTR}" = "podman" ]; then
      podman manifest inspect "${ref}" >/dev/null 2>&1 && return 0
    else
      docker buildx imagetools inspect "${ref}" >/dev/null 2>&1 && return 0
    fi
  fi
  return 1
}

REQUIRED_BASE=""
case "${P_TARGET}" in
  all|core-base|thick-prep|thick) REQUIRED_BASE="" ;;
  scion-base|common) REQUIRED_BASE="core-base" ;;
  *) REQUIRED_BASE="scion-base" ;;  # harnesses, hub, omni, single images
esac
if [ -n "${REQUIRED_BASE}" ]; then
  base_ref="${IMG_PREFIX}${REQUIRED_BASE}:${IMAGE_TAG}"
  if ! command -v "${CTR}" >/dev/null 2>&1; then
    if [ "${P_DRY_RUN}" = "true" ]; then
      warn "pre-flight: ${CTR} is not on PATH, so ${base_ref} was not checked (dry run, continuing)"
    else
      # Also for --builder cloud-build: the base may only exist in the
      # registry, and checking it needs a registry client. This script's
      # stated dependency is docker or podman, so it fails here with an
      # actionable message rather than building on an unchecked base.
      die "--target ${P_TARGET} stacks on ${base_ref}, and checking for it needs ${CTR} on PATH (docker with buildx for registry images, or podman). Install it, or use the default --target all, which needs no base"
    fi
  elif image_present "${base_ref}"; then
    note "pre-flight: ${base_ref} found for --target ${P_TARGET}"
  elif [ "${P_DRY_RUN}" = "true" ]; then
    warn "pre-flight: ${base_ref} not found (dry run, continuing)"
  else
    die "--target ${P_TARGET} stacks on ${base_ref}, which exists neither locally nor in the registry; build it first (e.g. -- --target ${REQUIRED_BASE}) or use the default --target all"
  fi
fi

# ---------------------------------------------------------------------------
# Build
# ---------------------------------------------------------------------------

BUILD_LOG="$(mktemp "${TMPDIR:-/tmp}/build-release-images.XXXXXX")"
cleanup_log() { rm -f -- "${BUILD_LOG:?}"; cleanup; }
trap cleanup_log EXIT

note "running ${BUILD_SCRIPT} --target ${P_TARGET} --tag ${IMAGE_TAG} ${FORWARD[*]+${FORWARD[*]}}"
# The tag's scripts read only two undocumented environment knobs that change
# targets or parents: THICK_BUILD (scion-base on thick-prep instead of
# core-base) and OMNI_BUILD (chained harnesses), both via ${VAR:-false}. The
# driver owns targeting, so it clears them. Audited at v0.3.0-preview.1 and .5
# (build-images.sh, lib/targets.sh, builders/): every other ${VAR:-} read is
# reassigned by build-images.sh before use (REGISTRY, PUSH, DRY_RUN,
# PLATFORMS, SHORT_SHA, COMMIT_SHA, VERSION), or is a documented knob that does
# not change targets or bases (SCION_IMAGE_REGISTRY, mirrored by the driver;
# GCLOUD_PROJECT for cloud-build; NPM_REGISTRY/PIP_INDEX_URL and
# NPM_CONFIG_FILE/PIP_CONFIG_FILE package mirrors and credentials; TMPDIR).
env -u THICK_BUILD -u OMNI_BUILD \
  bash "${BUILD_SCRIPT}" --target "${P_TARGET}" --tag "${IMAGE_TAG}" ${FORWARD[@]+"${FORWARD[@]}"} | tee "${BUILD_LOG}" \
  || die "build-images.sh failed"

if [ "${P_DRY_RUN}" = "true" ]; then
  note "dry run: no images tagged, no build-manifest.json written"
  exit 0
fi

# The image list comes from the "Steps:" banner every supported
# build-images.sh prints ("Steps:    a b c", optionally followed by a
# parenthesised note in cloud-build mode).
IMAGES=()
steps_line="$(sed -n 's/^Steps:[[:space:]]*//p' "${BUILD_LOG}" | head -n 1)"
STEP_WORDS=()
read -r -a STEP_WORDS <<< "${steps_line}" || true
for w in ${STEP_WORDS[@]+"${STEP_WORDS[@]}"}; do
  case "${w}" in
    \(*) break ;;
    *) IMAGES+=("${w}") ;;
  esac
done
if [ ${#IMAGES[@]} -eq 0 ]; then
  warn "could not read the step list from build-images.sh output; the summary and manifest list no images"
fi

# ---------------------------------------------------------------------------
# --also-tag latest
# ---------------------------------------------------------------------------

LATEST_REFS=()
if [ "${ALSO_TAG}" = "latest" ]; then
  for name in ${IMAGES[@]+"${IMAGES[@]}"}; do
    src_ref="${IMG_PREFIX}${name}:${IMAGE_TAG}"
    dst_ref="${IMG_PREFIX}${name}:latest"
    if [ "${PUSHED}" = "true" ]; then
      if [ "${CTR}" = "podman" ]; then
        podman tag "${src_ref}" "${dst_ref}" || die "podman tag ${dst_ref} failed"
        podman push "${dst_ref}" || die "podman push ${dst_ref} failed"
      else
        docker buildx imagetools create -t "${dst_ref}" "${src_ref}" \
          || die "imagetools create ${dst_ref} failed"
      fi
    else
      "${CTR}" tag "${src_ref}" "${dst_ref}" || die "${CTR} tag ${dst_ref} failed"
    fi
    LATEST_REFS+=("${dst_ref}")
  done
fi

# ---------------------------------------------------------------------------
# Summary and build-manifest.json
# ---------------------------------------------------------------------------

image_digest() {
  local ref="${1:?}" out=""
  if [ "${PUSHED}" = "true" ]; then
    if [ "${CTR}" = "podman" ]; then
      # The local podman digest can differ from the registry's (push may
      # recompress). Ask the registry: skopeo when it is installed, otherwise
      # re-push the same ref with --digestfile (no layers are uploaded again;
      # podman reports the manifest digest the tag now points to).
      if command -v skopeo >/dev/null 2>&1; then
        out="$(skopeo inspect --format '{{.Digest}}' "docker://${ref}" 2>/dev/null || true)"
      fi
      if [ -z "${out}" ]; then
        local df
        df="$(mktemp "${TMPDIR:-/tmp}/build-release-images-digest.XXXXXX")"
        if podman push --digestfile "${df}" "${ref}" >/dev/null 2>&1; then
          out="$(cat "${df}")"
        fi
        rm -f -- "${df:?}"
      fi
    else
      out="$(docker buildx imagetools inspect --format '{{.Manifest.Digest}}' "${ref}" 2>/dev/null || true)"
    fi
  else
    out="$("${CTR}" image inspect --format '{{.Id}}' "${ref}" 2>/dev/null || true)"
  fi
  printf '%s' "${out}" | head -n 1
}

# json_str <value>: a JSON string literal. Pattern and replacement come from
# variables and are quoted, so the result does not depend on how a bash
# version treats backslashes and quotes inside ${var//pat/rep}.
json_str() {
  # shellcheck disable=SC1003 # bs is a single literal backslash
  local s="$1" bs='\' q='"' nl=$'\n' cr=$'\r' tab=$'\t'
  s=${s//"${bs}"/"${bs}${bs}"}
  s=${s//"${q}"/"${bs}${q}"}
  s=${s//"${nl}"/"${bs}n"}
  s=${s//"${cr}"/"${bs}r"}
  s=${s//"${tab}"/"${bs}t"}
  printf '"%s"' "${s}"
}

COMMIT="$(git -C "${SRC_DIR}" rev-parse HEAD)"
BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
MANIFEST="${SRC_DIR}/build-manifest.json"

DIGESTS=()
for name in ${IMAGES[@]+"${IMAGES[@]}"}; do
  if [ "${SUBMITTED}" = "true" ]; then
    # The registry may hold an older build of this tag; never report it.
    DIGESTS+=("")
  else
    DIGESTS+=("$(image_digest "${IMG_PREFIX}${name}:${IMAGE_TAG}")")
  fi
done
# What the manifest says: a submitted build has pushed nothing yet.
MANIFEST_PUSHED="${PUSHED}"
if [ "${SUBMITTED}" = "true" ]; then
  MANIFEST_PUSHED="false"
fi

echo ""
echo "Release image build summary"
echo "  version: ${VERSION}"
echo "  source:  ${REPO} @ ${COMMIT}"
echo "  builder: ${P_BUILDER}"
if [ "${MODE}" = "tag" ] && [ "${VERSION_STAMPED}" != "true" ]; then
  echo "  WARNING: ${IMAGE_TAG} predates version stamping; the binaries report 'dev'"
fi
if [ "${SUBMITTED}" = "true" ]; then
  echo "  submitted to Cloud Build; images and digests are not available until the build finishes"
  echo "  (check it with: gcloud builds list / gcloud builds describe <id>)"
  echo "  images (submitted):"
elif [ "${PUSHED}" = "true" ]; then
  echo "  images (registry digests):"
else
  echo "  images (local image IDs):"
fi
i=0
for name in ${IMAGES[@]+"${IMAGES[@]}"}; do
  if [ "${SUBMITTED}" = "true" ]; then
    echo "    ${IMG_PREFIX}${name}:${IMAGE_TAG}  <submitted>"
  else
    echo "    ${IMG_PREFIX}${name}:${IMAGE_TAG}  ${DIGESTS[$i]:-<unavailable>}"
  fi
  i=$((i + 1))
done
for r in ${LATEST_REFS[@]+"${LATEST_REFS[@]}"}; do
  echo "    ${r}  (also tagged)"
done

# Staged builds (core-base, then scion-base, then a harness, all in one
# --src-dir) merge into the manifest of the same version, commit, builder and
# push mode, so the manifest ends up listing every staged image and every
# "digest" in it is of one kind. A submitted (cloud-build) manifest takes part
# in no merge, in either direction: it lists only its own run's images. The
# driver writes one image per line, which is what makes this line-based merge
# work without jq.
index_of() {
  # index_of <needle> <items...>: prints the index of needle, or returns 1.
  local needle="$1" i=0 x
  shift
  for x in "$@"; do
    if [ "${x}" = "${needle}" ]; then
      echo "${i}"
      return 0
    fi
    i=$((i + 1))
  done
  return 1
}

NEW_KEYS=()
NEW_LINES=()
i=0
for name in ${IMAGES[@]+"${IMAGES[@]}"}; do
  NEW_KEYS+=("$(json_str "${name}")")
  NEW_LINES+=("    $(json_str "${name}"): {\"ref\": $(json_str "${IMG_PREFIX}${name}:${IMAGE_TAG}"), \"digest\": $(json_str "${DIGESTS[$i]:-}")}")
  i=$((i + 1))
done

OUT_LINES=()
USED=()
if [ -f "${MANIFEST}" ] \
  && grep -qxF "  \"version\": $(json_str "${VERSION}")," "${MANIFEST}" \
  && grep -qxF "  \"commit\": $(json_str "${COMMIT}")," "${MANIFEST}" \
  && grep -qxF "  \"builder\": $(json_str "${P_BUILDER}")," "${MANIFEST}" \
  && grep -qxF "  \"pushed\": ${MANIFEST_PUSHED}," "${MANIFEST}" \
  && [ "${SUBMITTED}" = "false" ] \
  && grep -qxF '  "submitted": false,' "${MANIFEST}"; then
  kept=0
  while IFS= read -r line; do
    case "${line}" in
      '    "'*'": {"ref": '*)
        line="${line%,}"
        key="${line#    }"
        key="${key%%: \{*}"
        if j="$(index_of "${key}" ${NEW_KEYS[@]+"${NEW_KEYS[@]}"})"; then
          OUT_LINES+=("${NEW_LINES[$j]}")
          USED+=("${j}")
        else
          OUT_LINES+=("${line}")
          kept=$((kept + 1))
        fi
        ;;
    esac
  done < "${MANIFEST}"
  note "merged into the existing manifest for ${VERSION} @ ${COMMIT}: ${kept} earlier image(s) kept"
fi
j=0
for line in ${NEW_LINES[@]+"${NEW_LINES[@]}"}; do
  if ! index_of "${j}" ${USED[@]+"${USED[@]}"} >/dev/null; then
    OUT_LINES+=("${line}")
  fi
  j=$((j + 1))
done

MANIFEST_TMP="$(mktemp "${MANIFEST}.XXXXXX")"
{
  printf '{\n'
  printf '  "version": %s,\n' "$(json_str "${VERSION}")"
  printf '  "commit": %s,\n' "$(json_str "${COMMIT}")"
  printf '  "built_at": %s,\n' "$(json_str "${BUILT_AT}")"
  printf '  "builder": %s,\n' "$(json_str "${P_BUILDER}")"
  printf '  "pushed": %s,\n' "${MANIFEST_PUSHED}"
  printf '  "submitted": %s,\n' "${SUBMITTED}"
  printf '  "version_stamped": %s,\n' "${VERSION_STAMPED}"
  printf '  "images": {'
  sep=""
  for line in ${OUT_LINES[@]+"${OUT_LINES[@]}"}; do
    printf '%s\n%s' "${sep}" "${line}"
    sep=","
  done
  if [ -n "${sep}" ]; then
    printf '\n  '
  fi
  printf '}\n'
  printf '}\n'
} >"${MANIFEST_TMP}"
mv -f "${MANIFEST_TMP}" "${MANIFEST}"
echo "  manifest: ${MANIFEST}"
