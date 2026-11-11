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
# bash 3.2 (macOS /bin/bash), and it needs only git and docker or podman (plus gcloud and base64 for
# --channel-tag with --builder cloud-build), so
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
                        After a pushed build, also point :preview or :stable
                        at exactly the images just built: each channel tag
                        gets the registry digest of that image's :<version>.
                        :latest is never touched by this flag, and it does
                        not combine with --also-tag latest. It needs a
                        registry path <host>[:<port>]/<path> and pushed
                        images, and the release tag must
                        match the channel: preview needs vX.Y.Z-preview.N or
                        vX.Y.Z-rc.N, stable needs vX.Y.Z. Nightly tags
                        and --ref builds are refused. These checks run before
                        anything is cloned or built. Every image's digest is
                        verified, and each channel tag's current digest is
                        recorded, before the first channel tag moves. If a
                        move or read-back fails part-way, the run stops, lists
                        the tags moved so far, and writes the commands that
                        point them back to <src-dir>/channel-recovery.XXXXXX
                        (never run by the driver); no build-manifest.json is
                        written then. A podman run needs skopeo, and reads
                        every registry digest with it.
                        With --builder cloud-build (Artifact Registry only:
                        <location>-docker.pkg.dev/<project>/<repository>,
                        where <location> is us, europe, asia or a region such
                        as us-central1) the driver waits for the submitted
                        builds with gcloud. It takes each digest from the
                        builds' own results (results.images, or
                        "<ref> sha256:<hex>" lines in results.buildStepOutputs)
                        and requires :<version> to point to that same digest.
                        It then runs gcloud artifacts docker tags add
                        <image>@<digest> <image>:<channel>. A build that lists
                        neither result stops the run, and no channel tag
                        moves.
  --build-timeout <s>   Cloud Build wait: at most this many seconds per
                        build (default 14400).
  --wait-timeout <s>    Cloud Build wait: at most this many seconds in total
                        (default 21600). A timeout, or a build that ends in
                        any state other than SUCCESS, stops the run, and no
                        channel tag moves. Both count poll intervals (30s)
                        only, so the real wait is never shorter than stated
                        and can be longer; a gcloud call that hangs is not
                        bounded by them.
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
BUILD_TIMEOUT=""
WAIT_TIMEOUT=""
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
    --build-timeout) need_val "$1" $#; BUILD_TIMEOUT="$2"; shift 2 ;;
    --wait-timeout) need_val "$1" $#; WAIT_TIMEOUT="$2"; shift 2 ;;
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
    preview|stable) ;;
    *) usage_err "--channel-tag must be 'preview' or 'stable' (got '${CHANNEL_TAG}')" ;;
  esac
  # --channel-tag never touches :latest, so it does not combine with a flag
  # that moves :latest.
  if [ -n "${ALSO_TAG}" ]; then
    usage_err "--channel-tag does not combine with --also-tag latest (or --force-latest): a channel run never moves :latest"
  fi
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
# digest lookup, no channel tag, and a manifest marked "submitted".
SUBMITTED="false"
if [ "${P_BUILDER}" = "cloud-build" ] && [ "${P_DRY_RUN}" != "true" ]; then
  SUBMITTED="true"
fi
if [ "${ALSO_TAG}" = "latest" ] && [ "${P_BUILDER}" = "cloud-build" ]; then
  usage_err "--also-tag latest does not work with --builder cloud-build: the build is submitted asynchronously, so :latest can only be moved after it has finished (e.g. docker buildx imagetools create -t <registry>/<image>:latest <registry>/<image>:<version>)"
fi

# --channel-tag moves a tag in a registry, to the digests this run pushed.
# With --builder cloud-build the driver waits for the submitted builds
# (CB_WAIT) and takes each digest from the build's own results; that path
# tags with gcloud, so it is limited to Artifact Registry.
# The host is exactly <location>-docker.pkg.dev, where <location> follows the
# Artifact Registry grammar: a multi-region (us, europe, asia) or a region
# such as us-central1 or northamerica-northeast2 (one hyphen, then 1-2 digits).
AR_RE='^(us|europe|asia|[a-z]+-[a-z]+[0-9]{1,2})-docker\.pkg\.dev/[a-z][a-z0-9-]*[a-z0-9]/[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$'
CB_WAIT="false"
CB_POLL=30
# Refs a channel run moves, reads and writes into recovery commands are built
# only from a registry and image names in this reference grammar: path
# components are lowercase alphanumeric runs joined by the OCI separators
# ".", "_", "__" or one or more "-".
REGISTRY_RE='^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]+)?(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)+$'
IMAGE_NAME_RE='^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*$'
if [ -n "${CHANNEL_TAG}" ]; then
  if [ -z "${EFF_REGISTRY}" ]; then
    usage_err "--channel-tag ${CHANNEL_TAG} needs a registry (-- --registry <path> --push): a channel tag is only moved in a registry"
  fi
  if [ "${P_BUILDER}" = "cloud-build" ]; then
    if ! [[ "${EFF_REGISTRY}" =~ ${AR_RE} ]]; then
      usage_err "--channel-tag with --builder cloud-build needs an Artifact Registry path <location>-docker.pkg.dev/<project>/<repository> (got '${EFF_REGISTRY}')"
    fi
    if [ "${P_DRY_RUN}" != "true" ]; then
      CB_WAIT="true"
      SUBMITTED="false"
      command -v gcloud >/dev/null 2>&1 \
        || die "--channel-tag with --builder cloud-build needs gcloud on PATH (to wait for the build and to move the tag)"
      command -v base64 >/dev/null 2>&1 \
        || die "--channel-tag with --builder cloud-build needs base64 on PATH (to read build step outputs)"
    fi
  fi
  if ! [[ "${EFF_REGISTRY}" =~ ${REGISTRY_RE} ]]; then
    usage_err "--channel-tag ${CHANNEL_TAG}: the registry path '${EFF_REGISTRY}' is not a plain <host>[:<port>]/<path>"
  fi
  if [ "${PUSHED}" != "true" ]; then
    usage_err "--channel-tag ${CHANNEL_TAG} needs pushed images (-- --push, or --platform all): this build stays local"
  fi
fi
if [ -n "${BUILD_TIMEOUT}${WAIT_TIMEOUT}" ] \
  && { [ -z "${CHANNEL_TAG}" ] || [ "${P_BUILDER}" != "cloud-build" ]; }; then
  usage_err "--build-timeout and --wait-timeout only apply to --channel-tag with --builder cloud-build"
fi
for v in "${BUILD_TIMEOUT}" "${WAIT_TIMEOUT}"; do
  if [ -n "${v}" ] && ! [[ "${v}" =~ ^[1-9][0-9]{0,5}$ ]]; then
    usage_err "--build-timeout and --wait-timeout take a number of seconds from 1 to 999999 (got '${v}')"
  fi
done
# Defaults: the longest per-build timeout in the cloudbuild configs is 4h
# (cloudbuild.yaml, cloudbuild-thick.yaml); a run waits at most 6h in total.
BUILD_TIMEOUT="${BUILD_TIMEOUT:-14400}"
WAIT_TIMEOUT="${WAIT_TIMEOUT:-21600}"

if [ "${ALSO_TAG}" = "latest" ] && [ -n "${EFF_REGISTRY}" ] && [ "${FORCE_LATEST}" != "true" ]; then
  die "--also-tag latest with a registry (${EFF_REGISTRY}) would move that registry's :latest; add --force-latest if that is intended"
fi

CTR="docker"
if [ "${P_BUILDER}" = "local-podman" ]; then
  CTR="podman"
fi
# A podman channel run reads each channel tag's current digest with skopeo:
# podman itself can only read a registry digest by pushing.
if [ -n "${CHANNEL_TAG}" ] && [ "${CTR}" = "podman" ] && [ "${P_DRY_RUN}" != "true" ] \
  && ! command -v skopeo >/dev/null 2>&1; then
  die "--channel-tag with --builder local-podman needs skopeo on PATH (to read each channel tag's current digest)"
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

# --channel-tag: the release tag must match the channel. preview follows the
# preview channel (preview and rc releases); stable takes only vX.Y.Z. Nightly
# builds get no channel tag; --ref builds have no release tag.
STABLE_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
PREVIEW_RE='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)-(preview|rc)\.(0|[1-9][0-9]*)$'
if [ -n "${CHANNEL_TAG}" ]; then
  if [ "${MODE}" = "ref" ]; then
    usage_err "--channel-tag ${CHANNEL_TAG} needs a release tag; a --ref build has none"
  fi
  case "${VERSION}" in
    nightly-*) usage_err "--channel-tag ${CHANNEL_TAG}: nightly builds never get a channel tag (got '${VERSION}')" ;;
  esac
  if [ "${CHANNEL_TAG}" = "stable" ] && ! [[ "${VERSION}" =~ ${STABLE_RE} ]]; then
    usage_err "--channel-tag stable needs a stable release tag vX.Y.Z (got '${VERSION}')"
  fi
  if [ "${CHANNEL_TAG}" = "preview" ] && ! [[ "${VERSION}" =~ ${PREVIEW_RE} ]]; then
    usage_err "--channel-tag preview needs a preview-channel tag vX.Y.Z-preview.N or vX.Y.Z-rc.N (got '${VERSION}')"
  fi
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
  # (scion-base compiles the tree). The driver's own build-manifest.json and
  # channel-recovery.XXXXXX files are the expected untracked files; ignored
  # files are not listed.
  local st_raw st n
  if ! st_raw="$(git -C "${d}" status --porcelain --untracked-files=normal 2>/dev/null)"; then
    echo "git status failed in the checkout, so it cannot be checked for modified or untracked files"; return 1
  fi
  st="$(printf '%s\n' "${st_raw}" | grep -vxE '\?\? (build-manifest\.json|channel-recovery\.[A-Za-z0-9]{6})' || true)"
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
# When waiting for Cloud Build, stderr is logged too: gcloud prints the
# "Created [.../builds/<id>]." line of each submitted build there.
if [ "${CB_WAIT}" = "true" ]; then
  env -u THICK_BUILD -u OMNI_BUILD \
    bash "${BUILD_SCRIPT}" --target "${P_TARGET}" --tag "${IMAGE_TAG}" ${FORWARD[@]+"${FORWARD[@]}"} 2>&1 | tee "${BUILD_LOG}" \
    || die "build-images.sh failed"
else
  env -u THICK_BUILD -u OMNI_BUILD \
    bash "${BUILD_SCRIPT}" --target "${P_TARGET}" --tag "${IMAGE_TAG}" ${FORWARD[@]+"${FORWARD[@]}"} | tee "${BUILD_LOG}" \
    || die "build-images.sh failed"
fi

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
# --channel-tag with --builder cloud-build: wait, then read the build results
# ---------------------------------------------------------------------------

# Nothing here moves a tag. It ends with CB_DIGESTS (parallel to IMAGES): the
# digest each image's :<version> got in this run's builds, taken from the
# builds' own results (results.images, or "<ref> <digest>" lines in
# results.buildStepOutputs), never from a tag that could have moved since.
DIGEST_RE='^sha256:[0-9a-f]{64}$'
CB_CREATED_RE='^Created \[https://cloudbuild\.googleapis\.com/v1/projects/([a-z][a-z0-9-]{4,28}[a-z0-9])/locations/([a-z][a-z0-9-]*[a-z0-9])/builds/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\]\.$'
CB_DIGESTS=()
if [ "${CB_WAIT}" = "true" ]; then
  stop_cb() { die "--channel-tag ${CHANNEL_TAG}: $*; no channel tag was moved"; }
  if [ ${#IMAGES[@]} -eq 0 ]; then
    stop_cb "no image list from build-images.sh"
  fi
  CB_IDS=(); CB_PROJECTS=(); CB_LOCS=()
  while IFS= read -r line; do
    case "${line}" in
      'Created [https://cloudbuild.googleapis.com/'*)
        if [[ "${line}" =~ ${CB_CREATED_RE} ]]; then
          CB_PROJECTS+=("${BASH_REMATCH[1]}")
          CB_LOCS+=("${BASH_REMATCH[2]}")
          CB_IDS+=("${BASH_REMATCH[3]}")
        else
          stop_cb "unexpected Cloud Build reference in the build output: $(printf '%s' "${line}" | cut -c1-200)"
        fi
        ;;
    esac
  done < "${BUILD_LOG}"
  if [ ${#CB_IDS[@]} -eq 0 ]; then
    stop_cb "no 'Created [https://cloudbuild.googleapis.com/.../builds/<id>].' line in the build output"
  fi

  # Bounded wait: each build gets --build-timeout seconds, all of them
  # together --wait-timeout. The clock advances by the poll interval only, so
  # the real time waited is never shorter than stated and can be longer (each
  # describe call adds to it; a describe call that hangs is not bounded).
  total=0
  k=0
  for id in "${CB_IDS[@]}"; do
    waited=0
    errs=0
    while :; do
      if cb_status="$(gcloud builds describe "${id}" --project="${CB_PROJECTS[$k]}" --region="${CB_LOCS[$k]}" --format='value(status)')"; then
        errs=0
        case "${cb_status}" in
          SUCCESS) note "Cloud Build ${id}: SUCCESS"; break ;;
          QUEUED|PENDING|WORKING|STATUS_UNKNOWN) ;;
          *) stop_cb "Cloud Build ${id} ended with status '${cb_status:-<empty>}'" ;;
        esac
      else
        errs=$((errs + 1))
        if [ "${errs}" -ge 3 ]; then
          stop_cb "gcloud builds describe ${id} failed 3 times in a row"
        fi
      fi
      if [ "${waited}" -ge "${BUILD_TIMEOUT}" ]; then
        stop_cb "Cloud Build ${id} has not finished after ${BUILD_TIMEOUT}s (--build-timeout)"
      fi
      if [ "${total}" -ge "${WAIT_TIMEOUT}" ]; then
        stop_cb "the Cloud Build wait passed ${WAIT_TIMEOUT}s (--wait-timeout)"
      fi
      sleep "${CB_POLL}"
      waited=$((waited + CB_POLL))
      total=$((total + CB_POLL))
    done
    k=$((k + 1))
  done

  # The refs this run must have a digest for, in IMAGES order.
  WANTS=()
  for name in "${IMAGES[@]}"; do
    WANTS+=("${IMG_PREFIX}${name}:${IMAGE_TAG}")
  done
  # want_index <ref>: prints the index of ref in WANTS, or returns 1.
  want_index() {
    local w n=0
    for w in "${WANTS[@]}"; do
      if [ "${w}" = "$1" ]; then echo "${n}"; return 0; fi
      n=$((n + 1))
    done
    return 1
  }
  # Two sources, both the builds' own results:
  #  - results.images: "<ref><TAB><digest>" pairs. It also lists refs this
  #    run does not need (e.g. :<short-sha>); those are ignored.
  #  - results.buildStepOutputs: base64 per step; each non-empty decoded line
  #    must be "<ref> sha256:<64 hex>" for a ref in WANTS, at most once over
  #    all steps and builds. Any other line stops the run.
  IMG_DIGESTS=(); STEP_DIGESTS=()
  for w in "${WANTS[@]}"; do IMG_DIGESTS+=(""); STEP_DIGESTS+=(""); done
  tab=$'\t'
  k=0
  for id in "${CB_IDS[@]}"; do
    cb_where=(--project="${CB_PROJECTS[$k]}" --region="${CB_LOCS[$k]}")
    out="$(gcloud builds describe "${id}" "${cb_where[@]}" --flatten='results.images[]' --format='value(results.images.name,results.images.digest)')" \
      || stop_cb "could not read results.images of Cloud Build ${id}"
    while IFS="${tab}" read -r rname rdig; do
      j="$(want_index "${rname}")" || continue
      if [ -n "${IMG_DIGESTS[$j]}" ] && [ "${IMG_DIGESTS[$j]}" != "${rdig}" ]; then
        stop_cb "results.images report two different digests for ${rname}"
      fi
      IMG_DIGESTS[j]="${rdig}"
    done <<< "${out}"
    out="$(gcloud builds describe "${id}" "${cb_where[@]}" --format='value(results.buildStepOutputs)')" \
      || stop_cb "could not read results.buildStepOutputs of Cloud Build ${id}"
    # value() joins list items with ';', which base64 never contains.
    b64s=()
    IFS=';' read -r -a b64s <<< "${out}" || true
    for b in ${b64s[@]+"${b64s[@]}"}; do
      [ -n "${b}" ] || continue
      if ! [[ "${b}" =~ ^[A-Za-z0-9+/]+={0,2}$ ]]; then
        stop_cb "a build step output of Cloud Build ${id} is not base64"
      fi
      dec="$(printf '%s' "${b}" | base64 --decode)" || stop_cb "a build step output of Cloud Build ${id} does not decode"
      while IFS= read -r line; do
        [ -n "${line}" ] || continue
        if ! [[ "${line}" =~ ^([^[:space:]]+)\ (sha256:[0-9a-f]{64})$ ]]; then
          stop_cb "a build step output line of Cloud Build ${id} is not '<registry>/<image>:<tag> sha256:<64 hex>'"
        fi
        rname="${BASH_REMATCH[1]}"; rdig="${BASH_REMATCH[2]}"
        j="$(want_index "${rname}")" \
          || stop_cb "a build step output of Cloud Build ${id} names ${rname}, which this run did not build"
        if [ -n "${STEP_DIGESTS[$j]}" ]; then
          stop_cb "the build step outputs name ${rname} more than once"
        fi
        STEP_DIGESTS[j]="${rdig}"
      done <<< "${dec}"
    done
    k=$((k + 1))
  done

  # Exactly one digest per image, well formed; the two sources must agree.
  j=0
  for w in "${WANTS[@]}"; do
    found="${STEP_DIGESTS[$j]}"
    if [ -n "${IMG_DIGESTS[$j]}" ]; then
      if [ -n "${found}" ] && [ "${found}" != "${IMG_DIGESTS[$j]}" ]; then
        stop_cb "results.images and results.buildStepOutputs give different digests for ${w}"
      fi
      found="${IMG_DIGESTS[$j]}"
    fi
    if [ -z "${found}" ]; then
      stop_cb "no build-result digest for ${w} (neither results.images nor results.buildStepOutputs of build(s) ${CB_IDS[*]} names it)"
    fi
    if ! [[ "${found}" =~ ${DIGEST_RE} ]]; then
      stop_cb "the build-result digest for ${w} is not sha256:<64 hex>"
    fi
    CB_DIGESTS+=("${found}")
    j=$((j + 1))
  done
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
      # podman reports the manifest digest the tag now points to). A channel
      # run requires skopeo and reads with it only, never by pushing.
      if command -v skopeo >/dev/null 2>&1; then
        out="$(skopeo inspect --format '{{.Digest}}' "docker://${ref}" 2>/dev/null || true)"
      fi
      if [ -z "${out}" ] && [ -z "${CHANNEL_TAG}" ]; then
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
i=0
for name in ${IMAGES[@]+"${IMAGES[@]}"}; do
  if [ "${SUBMITTED}" = "true" ]; then
    # The registry may hold an older build of this tag; never report it.
    DIGESTS+=("")
  elif [ "${CB_WAIT}" = "true" ]; then
    DIGESTS+=("${CB_DIGESTS[$i]}")
  else
    DIGESTS+=("$(image_digest "${IMG_PREFIX}${name}:${IMAGE_TAG}")")
  fi
  i=$((i + 1))
done
# What the manifest says: a submitted build has pushed nothing yet.
MANIFEST_PUSHED="${PUSHED}"
if [ "${SUBMITTED}" = "true" ]; then
  MANIFEST_PUSHED="false"
fi

# ---------------------------------------------------------------------------
# --channel-tag preview|stable
# ---------------------------------------------------------------------------

# Every image's digest is verified (and, for cloud-build, its :<version> tag
# must still point at the build-result digest), and every image's current
# :<channel> digest is read and recorded, before the first channel tag moves.
# Each channel tag is set to a digest, never copied from a tag, and read
# back. A registry cannot move several tags atomically: if a move or a
# read-back fails part-way, the run stops, lists the tags moved so far, and
# writes the commands that point them back (to the recorded digests) to
# <src-dir>/channel-recovery.XXXXXX. The script never runs them. :latest is
# not touched here.

# ar_digest <ref>: the digest an Artifact Registry tag points to.
# It prints nothing when the lookup fails, so callers report '<unavailable>'
# (and the tags moved so far) instead of dying silently under set -e.
ar_digest() {
  local out=""
  out="$(gcloud artifacts docker images describe "${1:?}" --format='value(image_summary.digest)' 2>/dev/null)" || out=""
  printf '%s' "${out%%$'\n'*}"
}
# read_prev <ref>: the digest <ref> points to before this run moves it.
# Sets PREV to the digest (rc 0) or to "none" when the tag definitely does not
# exist (rc 0). The only signal taken as "absent" is a failed lookup whose
# stderr says "not found", "NOT_FOUND" or "manifest unknown" (any case). Any
# other failure returns 1, and so does output that is not sha256:<64 hex>.
read_prev() {
  local ref="${1:?}" out="" errf rc=0
  errf="$(mktemp "${TMPDIR:-/tmp}/build-release-images-prev.XXXXXX")"
  if [ "${CB_WAIT}" = "true" ]; then
    out="$(gcloud artifacts docker images describe "${ref}" --format='value(image_summary.digest)' 2>"${errf}")" || rc=$?
  elif [ "${CTR}" = "podman" ]; then
    out="$(skopeo inspect --format '{{.Digest}}' "docker://${ref}" 2>"${errf}")" || rc=$?
  else
    out="$(docker buildx imagetools inspect --format '{{.Manifest.Digest}}' "${ref}" 2>"${errf}")" || rc=$?
  fi
  PREV=""
  if [ "${rc}" -ne 0 ]; then
    if grep -qiE 'not[ _]found|manifest[ _]unknown' "${errf}"; then
      PREV="none"
    fi
    rm -f -- "${errf:?}"
    [ -n "${PREV}" ]
    return
  fi
  rm -f -- "${errf:?}"
  PREV="${out%%$'\n'*}"
  [[ "${PREV}" =~ ${DIGEST_RE} ]]
}
# recovery_line <i>: the command that points image i's channel tag back.
recovery_line() {
  local name="${IMAGES[$1]}" prev="${PREV_DIGESTS[$1]}"
  local img="${IMG_PREFIX}${name}" dst="${IMG_PREFIX}${name}:${CHANNEL_TAG}"
  if [ "${CB_WAIT}" = "true" ]; then
    if [ "${prev}" = "none" ]; then
      echo "gcloud artifacts docker tags delete ${dst}"
    else
      echo "gcloud artifacts docker tags add ${img}@${prev} ${dst}"
    fi
  elif [ "${prev}" = "none" ]; then
    echo "# ${dst} did not exist before this run; remove that tag with the registry's own tools"
  elif [ "${CTR}" = "podman" ]; then
    echo "skopeo copy docker://${img}@${prev} docker://${dst}"
  else
    echo "docker buildx imagetools create -t ${dst} ${img}@${prev}"
  fi
}
# ch_fail <i> <message>: stop after a part-way failure at image i. Images 0..i
# may have moved; restoring one that did not move is harmless.
ch_fail() {
  local k="$1" msg="$2" j=0 rf
  if ! rf="$(mktemp "${SRC_DIR}/channel-recovery.XXXXXX")"; then
    # Keep the recorded digests: print the commands, then stop.
    echo "${PROG}: recovery commands (not run); no recovery file could be created in ${SRC_DIR}:" >&2
    while [ "${j}" -le "${k}" ]; do
      recovery_line "${j}" >&2
      j=$((j + 1))
    done
    die "--channel-tag ${CHANNEL_TAG}: ${msg} (channel tags moved so far: ${CHANNEL_REFS[*]+${CHANNEL_REFS[*]}}); could not write a recovery file, the commands are above"
  fi
  {
    echo "# build-release-images.sh: channel tag recovery for :${CHANNEL_TAG} at ${IMAGE_TAG}"
    echo "# The run stopped part-way. Each command points one channel tag back to"
    echo "# the digest it had before the run. Nothing here has been run."
    while [ "${j}" -le "${k}" ]; do
      recovery_line "${j}"
      j=$((j + 1))
    done
  } >> "${rf}"
  echo "${PROG}: recovery commands (not run), also in ${rf}:" >&2
  cat "${rf}" >&2
  die "--channel-tag ${CHANNEL_TAG}: ${msg} (channel tags moved so far: ${CHANNEL_REFS[*]+${CHANNEL_REFS[*]}}); recovery commands: ${rf}"
}
TAG_DIGESTS=()
PREV_DIGESTS=()
CHANNEL_REFS=()
if [ -n "${CHANNEL_TAG}" ]; then
  stop_ch() { die "--channel-tag ${CHANNEL_TAG}: $*; no channel tag was moved"; }
  if [ ${#IMAGES[@]} -eq 0 ]; then
    stop_ch "no image list from build-images.sh"
  fi
  i=0
  for name in "${IMAGES[@]}"; do
    if ! [[ "${name}" =~ ${IMAGE_NAME_RE} ]]; then
      stop_ch "unexpected image name '${name}' in the build-images.sh step list"
    fi
    src_ref="${IMG_PREFIX}${name}:${IMAGE_TAG}"
    digest="${DIGESTS[$i]:-}"
    if [ -z "${digest}" ]; then
      stop_ch "no registry digest for ${src_ref}"
    fi
    if ! [[ "${digest}" =~ ${DIGEST_RE} ]]; then
      stop_ch "the digest of ${src_ref} is not sha256:<64 hex>"
    fi
    if [ "${CB_WAIT}" = "true" ]; then
      tag_digest="$(ar_digest "${src_ref}")"
      if [ "${tag_digest}" != "${digest}" ]; then
        stop_ch "${src_ref} points to '${tag_digest:-<unavailable>}', not to the build-result digest ${digest}"
      fi
    else
      # This digest was just read from :<version> itself.
      tag_digest="${digest}"
    fi
    TAG_DIGESTS+=("${tag_digest}")
    if ! read_prev "${IMG_PREFIX}${name}:${CHANNEL_TAG}"; then
      stop_ch "could not read the current digest of ${IMG_PREFIX}${name}:${CHANNEL_TAG} (a failed lookup that is not 'not found', or not sha256:<64 hex>)"
    fi
    PREV_DIGESTS+=("${PREV}")
    i=$((i + 1))
  done
  i=0
  for name in "${IMAGES[@]}"; do
    digest="${DIGESTS[$i]}"
    src_ref="${IMG_PREFIX}${name}:${IMAGE_TAG}"
    dst_ref="${IMG_PREFIX}${name}:${CHANNEL_TAG}"
    if [ "${CB_WAIT}" = "true" ]; then
      gcloud artifacts docker tags add "${IMG_PREFIX}${name}@${digest}" "${dst_ref}" \
        || ch_fail "${i}" "gcloud artifacts docker tags add ${dst_ref} failed"
      got="$(ar_digest "${dst_ref}")"
    elif [ "${CTR}" = "podman" ]; then
      podman tag "${src_ref}" "${dst_ref}" || ch_fail "${i}" "podman tag ${dst_ref} failed"
      podman push "${dst_ref}" || ch_fail "${i}" "podman push ${dst_ref} failed"
      got="$(image_digest "${dst_ref}")"
    else
      docker buildx imagetools create -t "${dst_ref}" "${IMG_PREFIX}${name}@${digest}" \
        || ch_fail "${i}" "imagetools create ${dst_ref} failed"
      got="$(image_digest "${dst_ref}")"
    fi
    if [ "${got}" != "${digest}" ]; then
      CHANNEL_REFS+=("${dst_ref}")
      ch_fail "${i}" "${dst_ref} points to '${got:-<unavailable>}', expected ${digest}"
    fi
    CHANNEL_REFS+=("${dst_ref}")
    i=$((i + 1))
  done
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
for r in ${CHANNEL_REFS[@]+"${CHANNEL_REFS[@]}"}; do
  echo "    ${r}  (channel tag)"
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
  # A channel run also records the digest :<version> pointed to when it was
  # checked, and the channel tag it moved.
  extra=""
  if [ ${#CHANNEL_REFS[@]} -gt 0 ]; then
    extra=", \"tag_digest\": $(json_str "${TAG_DIGESTS[$i]}"), \"channel_ref\": $(json_str "${CHANNEL_REFS[$i]}")"
  fi
  NEW_LINES+=("    $(json_str "${name}"): {\"ref\": $(json_str "${IMG_PREFIX}${name}:${IMAGE_TAG}"), \"digest\": $(json_str "${DIGESTS[$i]:-}")${extra}}")
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
