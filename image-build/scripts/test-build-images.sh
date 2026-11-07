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

# Tests for the build-images.sh guards. Pure bash: no docker, no network.
#
# Unlike test-build-release-images.sh, which drives a stub build-images.sh,
# this suite runs the real build-images.sh (with its lib/ and builders/) from
# a copy of image-build/ inside a temp git repository, so VERSION, the short
# SHA and the commit time come from a fixture with a fixed commit date.
# docker, podman and gcloud are stubs that answer `image inspect` /
# `imagetools inspect` from a list of present images and record every call.
# PATH holds only the stub dir and a private toolbin of symlinks.
#
# IMAGE_BUILD_UNDER_TEST=<dir> runs the suite against another copy of
# image-build/ (used for mutation testing).
#
# Usage: bash image-build/scripts/test-build-images.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE_BUILD_SRC="${IMAGE_BUILD_UNDER_TEST:-$(cd "${SCRIPT_DIR}/.." && pwd)}"
[ -f "${IMAGE_BUILD_SRC}/scripts/build-images.sh" ] || { echo "build-images.sh not found under ${IMAGE_BUILD_SRC}" >&2; exit 2; }

TMP="$(mktemp -d "${TMPDIR:-/tmp}/test-build-images.XXXXXX")"
trap 'rm -rf -- "${TMP:?}"' EXIT

FAILS=0
PASSES=0
pass() { PASSES=$((PASSES + 1)); echo "ok   - $1"; }
fail() { FAILS=$((FAILS + 1)); echo "FAIL - $1" >&2; }
check_eq() {
  if [ "$2" = "$3" ]; then
    pass "$1"
  else
    fail "$1"
    printf '       want: %s\n       got:  %s\n' "$2" "$3" >&2
  fi
}
check_contains() {
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1"; printf '       missing: %s\n       in output:\n%s\n' "$2" "$3" >&2 ;;
  esac
}
check_not_contains() {
  case "$3" in
    *"$2"*) fail "$1"; printf '       unexpected: %s\n' "$2" >&2 ;;
    *) pass "$1" ;;
  esac
}
check_rc() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (want rc $2, got rc $3)"; printf '%s\n' "${OUT}" >&2; fi
}

# ---------------------------------------------------------------------------
# Fixture repositories: OFF (HEAD not on a tag) and ON (HEAD tagged v9.9.9)
# ---------------------------------------------------------------------------

export GIT_AUTHOR_NAME="test" GIT_AUTHOR_EMAIL="test@example.com"
export GIT_COMMITTER_NAME="test" GIT_COMMITTER_EMAIL="test@example.com"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

COMMIT_DATE="2024-01-02T03:04:05Z"
COMMIT_EPOCH="1704164645"

OFF="${TMP}/off"
mkdir -p "${OFF}/harnesses/claude" "${OFF}/harnesses/codex"
cp -R "${IMAGE_BUILD_SRC}" "${OFF}/image-build"
# shellcheck disable=SC2016 # literal file content, not an expansion
printf 'ARG BASE_IMAGE\nFROM ${BASE_IMAGE}\n' > "${OFF}/harnesses/claude/Dockerfile"
cp "${OFF}/harnesses/claude/Dockerfile" "${OFF}/harnesses/codex/Dockerfile"
git -C "${OFF}" init -q -b main
git -C "${OFF}" add -A
GIT_AUTHOR_DATE="2023-06-01T00:00:00Z" GIT_COMMITTER_DATE="${COMMIT_DATE}" git -C "${OFF}" commit -q -m "fixture"
SHA="$(git -C "${OFF}" rev-parse --short HEAD)"
ON="${TMP}/on"
cp -R "${OFF}" "${ON}"
git -C "${ON}" tag v9.9.9
# v* tag first (hack/version.sh's rule): BOTH carries a nightly tag and a v*
# tag on HEAD (lightweight, as the release workflows push them; nightly-*
# sorts first in refname order); ANNOTATED has an annotated nightly next to a
# lightweight v* (plain describe prefers annotated tags); NIGHTLY has only a
# nightly tag; PAST has v9.9.9 one commit behind HEAD (off-tag).
BOTH="${TMP}/both"
cp -R "${OFF}" "${BOTH}"
git -C "${BOTH}" tag nightly-20261106
git -C "${BOTH}" tag v9.9.9
ANNOTATED="${TMP}/annotated"
cp -R "${OFF}" "${ANNOTATED}"
git -C "${ANNOTATED}" tag -a -m nightly nightly-20261106
git -C "${ANNOTATED}" tag v9.9.9
NIGHTLY="${TMP}/nightly"
cp -R "${OFF}" "${NIGHTLY}"
git -C "${NIGHTLY}" tag nightly-20261106
PAST="${TMP}/past"
cp -R "${ON}" "${PAST}"
GIT_AUTHOR_DATE="2023-06-01T00:00:00Z" GIT_COMMITTER_DATE="${COMMIT_DATE}" git -C "${PAST}" commit -q --allow-empty -m "after the tag"

# ---------------------------------------------------------------------------
# Stubs and toolbin
# ---------------------------------------------------------------------------

STUBBIN="${TMP}/stubbin"
mkdir -p "${STUBBIN}"
cat > "${STUBBIN}/docker" <<'STUB'
#!/usr/bin/env bash
me="$(basename "$0")"
printf '%s %s\n' "${me}" "$*" >> "${STUB_CTR_LOG:?}"
ref=""
for a in "$@"; do ref="${a}"; done
# gcloud artifacts docker tags list <image> --format=value(tag): list the
# tags present for <image> (bare names, or full resource names with
# STUB_GCLOUD_FULLNAME=1). An image with no tags at all is NOT_FOUND, or an
# empty list with STUB_GCLOUD_EMPTY_PKG=1.
if [ "${1:-} ${2:-} ${3:-} ${4:-}" = "artifacts docker tags list" ]; then
  image="$5"
  # Like real gcloud: core/user_output_enabled=false prints nothing (rc 0)
  # unless --user-output-enabled is on the command line.
  uoe="false"
  for a in "$@"; do [ "${a}" = "--user-output-enabled" ] && uoe="true"; done
  if [ "${CLOUDSDK_CORE_USER_OUTPUT_ENABLED:-}" = "false" ] && [ "${uoe}" != "true" ]; then
    exit 0
  fi
  verb=""
  for a in "$@"; do case "${a}" in --verbosity=*) verb="${a#--verbosity=}" ;; esac; done
  # emit_err <msg>: an ERROR line as the user's gcloud config would render
  # it (STUB_GCLOUD_USERCFG), unless the probe's overrides are present.
  emit_err() {
    case "${STUB_GCLOUD_USERCFG:-}" in
      structured) if [ "${CLOUDSDK_CORE_SHOW_STRUCTURED_LOGS:-}" != never ]; then printf '{"verbosity": "ERROR", "message": "%s"}\n' "$1" >&2; return; fi ;;
      detailed) if [ "${CLOUDSDK_CORE_CONSOLE_LOG_FORMAT:-}" != standard ]; then echo "2026-11-05 12:00:00,000 ERROR    root            $1" >&2; return; fi ;;
      quiet) if [ "${verb}" != error ]; then return; fi ;;
    esac
    echo "ERROR: $1" >&2
  }
  if grep -q "^gcwarnnf:gcloud:${image}:" "${STUB_PRESENT:?}"; then
    echo "WARNING: mirror answered NOT_FOUND, falling back" >&2
    emit_err "(gcloud.artifacts.docker.tags.list) INVALID_ARGUMENT: Request contains an invalid argument."
    exit 1
  fi
  if grep -q "^gcdenynf:gcloud:${image}:" "${STUB_PRESENT:?}"; then
    emit_err "(gcloud.artifacts.docker.tags.list) PERMISSION_DENIED: Permission 'artifactregistry.tags.list' denied on resource (or it may not exist). NOT_FOUND"
    exit 1
  fi
  if grep -q "^gcmixed:gcloud:${image}:" "${STUB_PRESENT:?}"; then
    emit_err "(gcloud.artifacts.docker.tags.list) NOT_FOUND: Requested entity was not found. (after retry: 503 Service Unavailable)"
    exit 1
  fi
  if [ -n "${STUB_GCLOUD_CRONLY:-}" ]; then
    printf '\r\n\r\n'
    exit 0
  fi
  if [ -n "${STUB_GCLOUD_STDERR_ERROR:-}" ]; then
    echo "ERROR: (gcloud.artifacts.docker.tags.list) unexpected response" >&2
    exit 0
  fi
  if [ -n "${STUB_GCLOUD_WARN:-}" ]; then
    echo "WARNING: Python 3.8 is deprecated" >&2
  fi
  if grep -q "^error:gcloud:${image}:" "${STUB_PRESENT:?}"; then
    echo "ERROR: (gcloud.artifacts.docker.tags.list) PERMISSION_DENIED: Permission 'artifactregistry.tags.list' denied" >&2
    exit 1
  fi
  if grep -q "^gcnet:gcloud:${image}:" "${STUB_PRESENT:?}"; then
    echo "ERROR: gcloud crashed (ConnectionError): HTTPSConnectionPool(host='artifactregistry.googleapis.com'): host not found" >&2
    exit 1
  fi
  if [ -n "${STUB_GCLOUD_BLANK:-}" ]; then
    printf ' \n\t\n'
    exit 0
  fi
  tags="$(sed -n "s|^remote:${image}:||p" "${STUB_PRESENT:?}")"
  if [ -n "${STUB_GCLOUD_NOEOL:-}" ]; then
    printf '%s' "${tags}"
    exit 0
  fi
  if [ -z "${tags}" ] && [ -z "${STUB_GCLOUD_EMPTY_PKG:-}" ]; then
    emit_err "(gcloud.artifacts.docker.tags.list) NOT_FOUND: Requested entity was not found: ${image}"
    exit 1
  fi
  if [ -n "${STUB_GCLOUD_BLANKLINE:-}" ]; then
    echo ""
  fi
  printf '%s\n' "${tags}" | while IFS= read -r t; do
    [ -n "${t}" ] || continue
    cr=""
    if [ -n "${STUB_GCLOUD_CRLF:-}" ]; then cr="$(printf '\r')"; fi
    if [ -n "${STUB_GCLOUD_FULLNAME:-}" ]; then
      echo "projects/p/locations/us/repositories/r/packages/${image##*/}/tags/${t}${cr}"
    else
      echo "${t}${cr}"
    fi
  done
  exit 0
fi
# "error:<tool>:<ref>" in the present list makes that tool's lookup fail
# the way a missing credential does (not a "not found"); "credhelper:..."
# the way docker does when a configured credential helper is not on PATH.
case "$*" in
  "image inspect "*|"buildx imagetools inspect "*|"manifest inspect "*)
    if grep -qxF "error:${me}:${ref}" "${STUB_PRESENT:?}"; then
      echo "ERROR: failed to authorize: 401 Unauthorized (denied: permission denied)" >&2
      exit 1
    fi
    if grep -qxF "neterror:${me}:${ref}" "${STUB_PRESENT:?}"; then
      echo "ERROR: proxyconnect tcp: dial tcp: lookup proxy.corp: host not found" >&2
      exit 1
    fi
    if grep -qxF "credsh:${me}:${ref}" "${STUB_PRESENT:?}"; then
      echo "ERROR: failed to solve: docker-credential-gcloud: not found" >&2
      exit 1
    fi
    if grep -qxF "credhelper:${me}:${ref}" "${STUB_PRESENT:?}"; then
      echo "ERROR: error getting credentials - err: exec: \"docker-credential-gcloud\": executable file not found in \$PATH, out: \`\`" >&2
      exit 1
    fi ;;
esac
# "mixed:<tool>:<ref>": the tool's own not-found text AND an error marker.
case "$*" in
  "image inspect "*)
    if grep -qxF "mixed:${me}:${ref}" "${STUB_PRESENT:?}"; then
      if [ "${me}" = podman ]; then
        echo "Error: ${ref}: image not known (storage: connection refused)" >&2
      else
        echo "Error: No such image: ${ref} (retry: dial tcp 10.0.0.1:443: i/o timeout)" >&2
      fi
      exit 1
    fi ;;
  "manifest inspect "*|"buildx imagetools inspect "*)
    if grep -qxF "mixed:${me}:${ref}" "${STUB_PRESENT:?}"; then
      if [ "${me}" = podman ]; then
        echo "Error: reading manifest ${ref##*:} in ${ref%:*}: manifest unknown; received unexpected HTTP status: 503 Service Unavailable" >&2
      else
        printf 'ERROR: %s: not found\nERROR: 503 Service Unavailable\n' "${ref}" >&2
      fi
      exit 1
    fi ;;
esac
# podman registries.conf mirror aggregation (containers/image):
# "mirrorNNN:podman:<ref>" with NNN = timeout, 503, 429 or nf (primary also
# not found).
case "$*" in
  "manifest inspect "*)
    m="(Mirrors also failed: [mirror.example/x/${ref##*/}: reading manifest ${ref##*:} in mirror.example/x/${ref##*/}: manifest unknown])"
    if grep -qxF "mirrortimeout:${me}:${ref}" "${STUB_PRESENT:?}"; then
      echo "Error: ${m}: ${ref}: pinging container registry ${ref%%/*}: Get \"https://${ref%%/*}/v2/\": dial tcp 10.0.0.1:443: i/o timeout" >&2; exit 1
    fi
    if grep -qxF "mirror503:${me}:${ref}" "${STUB_PRESENT:?}"; then
      echo "Error: ${m}: ${ref}: reading manifest ${ref##*:} in ${ref%:*}: received unexpected HTTP status: 503 Service Unavailable" >&2; exit 1
    fi
    if grep -qxF "mirror429:${me}:${ref}" "${STUB_PRESENT:?}"; then
      echo "Error: ${m}: ${ref}: reading manifest ${ref##*:} in ${ref%:*}: toomanyrequests: You have reached your pull rate limit" >&2; exit 1
    fi
    if grep -qxF "mirrornf:${me}:${ref}" "${STUB_PRESENT:?}"; then
      echo "Error: ${m}: ${ref}: reading manifest ${ref##*:} in ${ref%:*}: manifest unknown" >&2; exit 1
    fi ;;
esac
# "regerror:<tool>:<ref>": only the registry lookup fails (401).
case "$*" in
  "buildx imagetools inspect "*|"manifest inspect "*)
    if grep -qxF "regerror:${me}:${ref}" "${STUB_PRESENT:?}"; then
      echo "ERROR: failed to authorize: 401 Unauthorized" >&2
      exit 1
    fi ;;
esac
case "$*" in
  "image inspect "*)
    if ! grep -qxF "local:${ref}" "${STUB_PRESENT:?}"; then
      if [ "${me}" = podman ]; then echo "Error: ${ref}: image not known" >&2; else echo "Error: No such image: ${ref}" >&2; fi
      exit 1
    fi ;;
  "manifest inspect "*)
    grep -qxF "remote:${ref}" "${STUB_PRESENT:?}" || { echo "Error: reading manifest ${ref##*:} in ${ref%:*}: manifest unknown" >&2; exit 1; } ;;
  "buildx imagetools inspect "*)
    grep -qxF "remote:${ref}" "${STUB_PRESENT:?}" || { echo "ERROR: ${ref}: not found" >&2; exit 1; } ;;
  "buildx build "*|"build "*)
    printf 'SOURCE_DATE_EPOCH=%s\n' "${SOURCE_DATE_EPOCH-<unset>}" >> "${STUB_ENV_LOG:?}" ;;
  "config get-value project")
    echo "stub-project" ;;
esac
exit 0
STUB
chmod +x "${STUBBIN}/docker"
cp "${STUBBIN}/docker" "${STUBBIN}/podman"
cp "${STUBBIN}/docker" "${STUBBIN}/gcloud"
# The same stubs without docker, for the cloud-build gcloud fallback.
NODOCKER="${TMP}/stubbin-nodocker"
mkdir -p "${NODOCKER}"
cp "${STUBBIN}/gcloud" "${NODOCKER}/gcloud"

TOOLBIN="${TMP}/toolbin"
mkdir -p "${TOOLBIN}"
for tool in git bash sed grep awk date mktemp dirname basename find sort cat tr head rm mkdir cut env; do
  p="$(command -v "${tool}")" || { echo "required tool missing: ${tool}" >&2; exit 2; }
  ln -s "${p}" "${TOOLBIN}/${tool}"
done

# A date that behaves like BSD (macOS) date: no -d, and -r <epoch>.
BSDDATE="${TMP}/stubbin-bsddate"
mkdir -p "${BSDDATE}"
cat > "${BSDDATE}/date" <<STUB
#!/usr/bin/env bash
for a in "\$@"; do
  case "\${a}" in -d|-d*) echo "date: illegal option -- d" >&2; exit 1 ;; esac
done
if [ "\${1:-}" = "-u" ] && [ "\${2:-}" = "-r" ]; then
  epoch="\$3"; shift 3
  exec "$(command -v date)" -u -d "@\${epoch}" "\$@"
fi
exec "$(command -v date)" "\$@"
STUB
chmod +x "${BSDDATE}/date"
cp "${STUBBIN}/docker" "${STUBBIN}/podman" "${STUBBIN}/gcloud" "${BSDDATE}/"

OUT=""
RC=0
RUN_ENV=()
RUN_PATH=""
# run_bi <repo> <build-images.sh args...>: fresh logs; sets OUT and RC.
run_bi() {
  local repo="$1"
  shift
  : > "${TMP}/ctr.log"
  : > "${TMP}/env.log"
  touch "${TMP}/present"
  set +e
  OUT="$(cd "${TMP}" && env -i PATH="${RUN_PATH:-${STUBBIN}}:${TOOLBIN}" HOME="${TMP}/home" TMPDIR="${TMP}" \
    GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 GCLOUD_PROJECT=p \
    STUB_CTR_LOG="${TMP}/ctr.log" STUB_PRESENT="${TMP}/present" STUB_ENV_LOG="${TMP}/env.log" \
    ${RUN_ENV[@]+"${RUN_ENV[@]}"} \
    bash "${repo}/image-build/scripts/build-images.sh" "$@" 2>&1)"
  RC=$?
  set -e
}
ctr() { cat "${TMP}/ctr.log"; }
present() { printf '%s\n' "$@" > "${TMP}/present"; }
# line_for <image>: the dry-run builder command line that builds <image>.
line_for() { printf '%s\n' "${OUT}" | grep '^\[dry-run\] docker buildx build' | grep -- "-t $1:" | head -n 1 || true; }

# ---------------------------------------------------------------------------
# Base guard: refusal, opt-out, found, dry-run warning
# ---------------------------------------------------------------------------

present
run_bi "${OFF}" --target common
check_rc "guard: common without core-base: exit 1" 1 "${RC}"
check_contains "guard: common without core-base: names the image" "Error: --target common stacks on core-base:latest, which was not found in the local image store." "${OUT}"
check_contains "guard: common without core-base: says how to fix" "Build core-base first (--target core-base --tag latest)" "${OUT}"
check_not_contains "guard: common without core-base: nothing built" "buildx build" "$(ctr)"
check_contains "guard: common without core-base: local store was checked" "docker image inspect core-base:latest" "$(ctr)"

run_bi "${OFF}" --target common --allow-stale-base
check_rc "opt-out: --allow-stale-base: exit 0" 0 "${RC}"
check_contains "opt-out: --allow-stale-base: reported as not checked" "Base:     core-base:latest (not checked: --allow-stale-base)" "${OUT}"
check_not_contains "opt-out: --allow-stale-base: no lookup at all" "inspect" "$(ctr)"
check_contains "opt-out: --allow-stale-base: builds" "docker buildx build" "$(ctr)"
check_not_contains "opt-out: --allow-stale-base: no error" "Error:" "${OUT}"

present local:core-base:latest
run_bi "${OFF}" --target common
check_rc "guard: core-base present locally: exit 0" 0 "${RC}"
check_contains "guard: core-base present locally: reported" "Base:     core-base:latest (found)" "${OUT}"
check_not_contains "guard: core-base present locally: no warning" "stacks on" "${OUT}"

present
run_bi "${OFF}" --target common --dry-run
check_rc "dry-run: missing base: exit 0" 0 "${RC}"
check_contains "dry-run: missing base: warning" "Warning: --target common stacks on core-base:latest, which was not found in the local image store." "${OUT}"
check_contains "dry-run: missing base: says a real build stops" "A real build would stop here." "${OUT}"
check_not_contains "dry-run: missing base: no error" "Error:" "${OUT}"
check_contains "dry-run: missing base: commands still printed" "[dry-run] docker buildx build" "${OUT}"

# Required base per target.
for case_ in "harnesses:scion-base" "hub:scion-base" "omni:scion-base" "scion-claude:scion-base" "scion-hub:scion-base" "scion-base:core-base"; do
  t="${case_%%:*}"
  b="${case_#*:}"
  present
  run_bi "${OFF}" --target "${t}"
  check_rc "guard: --target ${t} without ${b}: exit 1" 1 "${RC}"
  check_contains "guard: --target ${t} needs ${b}" "stacks on ${b}:latest" "${OUT}"
done
for t in all core-base thick-prep thick; do
  present
  run_bi "${OFF}" --target "${t}"
  check_rc "guard: --target ${t} builds its own base: exit 0" 0 "${RC}"
  check_not_contains "guard: --target ${t}: no base check" "stacks on" "${OUT}"
  check_not_contains "guard: --target ${t}: no base lookup" "inspect" "$(ctr)"
done

present
RUN_ENV=(THICK_BUILD=true)
run_bi "${OFF}" --target scion-base
RUN_ENV=()
check_contains "guard: THICK_BUILD scion-base needs thick-prep" "stacks on thick-prep:latest" "${OUT}"

# Single steps follow step_parent (R1-F3); target mode follows the yamls.
RUN_ENV=(OMNI_BUILD=true)
run_bi "${OFF}" --target scion-omni
check_contains "guard: OMNI_BUILD scion-omni needs scion-grok-build" "stacks on scion-grok-build:latest" "${OUT}"
run_bi "${OFF}" --target scion-claude
check_contains "guard: OMNI_BUILD scion-claude needs scion-base" "stacks on scion-base:latest" "${OUT}"
RUN_ENV=()
run_bi "${OFF}" --target scion-omni
check_contains "guard: scion-omni needs scion-base" "stacks on scion-base:latest" "${OUT}"
present
RUN_ENV=(THICK_BUILD=true)
run_bi "${OFF}" --builder cloud-build --target scion-base --registry reg.example/x --dry-run
RUN_ENV=()
check_contains "cloud-build: THICK_BUILD scion-base still checks core-base (yaml)" "stacks on reg.example/x/core-base:latest" "${OUT}"
RUN_ENV=(OMNI_BUILD=true)
run_bi "${OFF}" --target scion-codex
check_contains "guard: OMNI_BUILD scion-codex needs scion-claude" "stacks on scion-claude:latest" "${OUT}"
run_bi "${OFF}" --builder cloud-build --target scion-codex --registry reg.example/x --dry-run
RUN_ENV=()
check_contains "cloud-build: OMNI_BUILD single harness checks scion-base (generated config)" "stacks on reg.example/x/scion-base:latest" "${OUT}"

# Registry: the base may live only in the registry; the tag is --tag.
present remote:reg.example/x/scion-base:v1
run_bi "${OFF}" --target harnesses --registry reg.example/x --push --tag v1
check_rc "guard: base only in the registry: exit 0" 0 "${RC}"
check_contains "guard: base only in the registry: found" "Base:     reg.example/x/scion-base:v1 (found)" "${OUT}"
check_contains "guard: base only in the registry: imagetools used" "docker buildx imagetools inspect reg.example/x/scion-base:v1" "$(ctr)"
present remote:reg.example/x/scion-base:latest
run_bi "${OFF}" --target harnesses --registry reg.example/x --push --tag v1
check_rc "guard: registry has :latest but not :v1: exit 1" 1 "${RC}"
check_contains "guard: registry message" "which was not found in registry reg.example/x." "${OUT}"

# --push (and auto-promoted multi-platform) builds pull the base from the
# registry, so a local copy does not count (R1-F1).
present local:reg.example/x/core-base:latest
run_bi "${OFF}" --target common --registry reg.example/x --push
check_rc "push: local-only base: exit 1" 1 "${RC}"
check_contains "push: local-only base: registry named" "which was not found in registry reg.example/x." "${OUT}"
check_contains "push: registry probed" "docker buildx imagetools inspect reg.example/x/core-base:latest" "$(ctr)"
check_not_contains "push: local store not consulted" "image inspect reg.example" "$(ctr | grep -v imagetools || true)"
run_bi "${OFF}" --target common --registry reg.example/x --platform all
check_rc "multi-platform: local-only base: exit 1" 1 "${RC}"
check_not_contains "multi-platform: nothing built" "buildx build" "$(ctr)"
run_bi "${OFF}" --target common --registry reg.example/x
check_rc "local build with registry prefix: local copy counts" 0 "${RC}"
present
run_bi "${OFF}" --target common --registry reg.example/x
check_contains "local build with registry: both places named" "which was not found locally or in registry reg.example/x." "${OUT}"

# Lookup errors are not "not found": warn and go ahead (R2-F1), unless
# --strict-base.
present error:docker:core-base:latest
run_bi "${OFF}" --target common
check_rc "lookup error: exit 0 (warn, go ahead)" 0 "${RC}"
check_contains "lookup error: names tool and error" "Warning: --target common stacks on core-base:latest, which could not be checked (docker: ERROR: failed to authorize" "${OUT}"
check_contains "lookup error: says it goes ahead" "Going ahead: the builder resolves core-base:latest itself" "${OUT}"
check_contains "lookup error: points at --strict-base" "Pass --strict-base to stop on lookup failures." "${OUT}"
check_not_contains "lookup error: not reported as missing" "was not found" "${OUT}"
check_contains "lookup error: builds" "docker buildx build" "$(ctr)"
run_bi "${OFF}" --target common --strict-base
check_rc "lookup error, --strict-base: exit 1" 1 "${RC}"
check_contains "lookup error, --strict-base: error" "Error: --target common stacks on core-base:latest, which could not be checked (docker:" "${OUT}"
check_contains "lookup error, --strict-base: says how to relax" "or drop --strict-base to warn and go ahead." "${OUT}"
check_not_contains "lookup error, --strict-base: nothing built" "buildx build" "$(ctr)"
run_bi "${OFF}" --target common --dry-run
check_rc "lookup error, dry run, not strict: exit 0" 0 "${RC}"
check_contains "lookup error, dry run, not strict: goes ahead" "Going ahead: the builder resolves core-base:latest itself" "${OUT}"
check_not_contains "lookup error, dry run, not strict: no 'would stop'" "would stop here" "${OUT}"
run_bi "${OFF}" --target common --strict-base --dry-run
check_rc "lookup error, --strict-base, dry run: exit 0" 0 "${RC}"
check_contains "lookup error, --strict-base, dry run: warns a real build stops" "A real build would stop here (--strict-base)." "${OUT}"
run_bi "${OFF}" --target common --strict-base --allow-stale-base
check_rc "lookup error, --strict-base + --allow-stale-base: exit 0" 0 "${RC}"
present
run_bi "${OFF}" --target common --strict-base
check_rc "--strict-base, definite not-found: still exit 1" 1 "${RC}"
present local:core-base:latest
run_bi "${OFF}" --target common --strict-base
check_rc "--strict-base, base found: exit 0" 0 "${RC}"

# R2-F2(a): a credential-helper failure saying "executable file not found"
# is a lookup error, not a missing image.
present credhelper:docker:reg.example/x/core-base:latest
run_bi "${OFF}" --target common --registry reg.example/x --push
check_rc "credential helper missing, --push: exit 0 (C2)" 0 "${RC}"
check_contains "credential helper missing, --push: lookup error named" "could not be checked (docker: ERROR: error getting credentials" "${OUT}"
check_not_contains "credential helper missing, --push: not reported as missing" "was not found" "${OUT}"
run_bi "${OFF}" --target common --registry reg.example/x --push --strict-base
check_rc "credential helper missing, --push, --strict-base: exit 1" 1 "${RC}"
# A network error that says "host not found" is not a missing image.
present neterror:docker:reg.example/x/core-base:latest
run_bi "${OFF}" --target common --registry reg.example/x --push
check_rc "network error, --push: exit 0" 0 "${RC}"
check_contains "network error, --push: lookup error named" "could not be checked (docker: ERROR: proxyconnect tcp: dial tcp: lookup proxy.corp: host not found)" "${OUT}"
check_contains "network error, --push: builds" "docker buildx build" "$(ctr)"
# A shell-style "docker-credential-gcloud: not found" is a credential failure.
present credsh:docker:reg.example/x/core-base:latest
run_bi "${OFF}" --target common --registry reg.example/x --push
check_rc "credential helper not found (shell form), --push: exit 0" 0 "${RC}"
check_not_contains "credential helper not found (shell form): not reported as missing" "was not found" "${OUT}"
# R2-F2(b): a local miss plus a registry error is unknown, not missing (D).
present regerror:docker:reg.example/x/core-base:latest
run_bi "${OFF}" --target common --registry reg.example/x
check_rc "local miss + registry error: exit 0 (D)" 0 "${RC}"
check_contains "local miss + registry error: lookup error named" "could not be checked (docker: ERROR: failed to authorize: 401 Unauthorized)" "${OUT}"
check_contains "local miss + registry error: local store was asked" "docker image inspect reg.example/x/core-base:latest" "$(ctr)"
check_not_contains "local miss + registry error: not reported as missing" "was not found" "${OUT}"
present
run_bi "${OFF}" --target common --registry reg.example/x
check_rc "local miss + registry not-found: exit 1" 1 "${RC}"

# local-podman uses podman's store and podman manifest inspect.
present local:core-base:latest
run_bi "${OFF}" --builder local-podman --target scion-base
check_rc "guard: local-podman base present: exit 0" 0 "${RC}"
check_contains "guard: local-podman checks podman's store" "podman image inspect core-base:latest" "$(ctr)"
check_not_contains "guard: local-podman never asks docker" "docker " "$(ctr)"
present remote:reg.example/x/core-base:latest
run_bi "${OFF}" --builder local-podman --target scion-base --registry reg.example/x --dry-run
check_contains "guard: local-podman registry probe" "podman manifest inspect reg.example/x/core-base:latest" "$(ctr)"
check_contains "guard: local-podman registry base found" "Base:     reg.example/x/core-base:latest (found)" "${OUT}"
# local-podman outcomes, with podman's own messages (R3-F1).
present
run_bi "${OFF}" --builder local-podman --target scion-base
check_rc "podman: store miss (image not known), no registry: exit 1" 1 "${RC}"
check_contains "podman: store miss: message" "which was not found in the local image store." "${OUT}"
check_not_contains "podman: store miss: nothing built" "podman build" "$(ctr)"
present error:podman:core-base:latest
run_bi "${OFF}" --builder local-podman --target scion-base
check_rc "podman: store lookup error: exit 0" 0 "${RC}"
check_contains "podman: store lookup error: named" "could not be checked (podman: ERROR: failed to authorize" "${OUT}"
check_contains "podman: store lookup error: builds" "podman build" "$(ctr)"
run_bi "${OFF}" --builder local-podman --target scion-base --strict-base
check_rc "podman: store lookup error, --strict-base: exit 1" 1 "${RC}"
check_not_contains "podman: store lookup error, --strict-base: nothing built" "podman build" "$(ctr)"
present regerror:podman:reg.example/x/core-base:latest
run_bi "${OFF}" --builder local-podman --target scion-base --registry reg.example/x
check_rc "podman: local miss + registry error: exit 0" 0 "${RC}"
check_contains "podman: local miss + registry error: named" "could not be checked (podman: ERROR: failed to authorize: 401 Unauthorized)" "${OUT}"
present
run_bi "${OFF}" --builder local-podman --target scion-base --registry reg.example/x
check_rc "podman: registry not-found (manifest unknown): exit 1" 1 "${RC}"
check_contains "podman: registry not-found: message" "which was not found locally or in registry reg.example/x." "${OUT}"
present credhelper:podman:reg.example/x/core-base:latest
run_bi "${OFF}" --builder local-podman --target scion-base --registry reg.example/x --push
check_rc "podman: --push credential error: exit 0" 0 "${RC}"
check_not_contains "podman: --push credential error: not reported as missing" "was not found" "${OUT}"
check_not_contains "podman: --push: local store not consulted" "podman image inspect" "$(ctr)"

# cloud-build: only the registry counts; omni's config builds scion-base.
present local:reg.example/x/core-base:latest
run_bi "${OFF}" --builder cloud-build --target common --registry reg.example/x --dry-run
check_contains "cloud-build: a local base does not count" "which was not found in registry reg.example/x." "${OUT}"
check_not_contains "cloud-build: local store not consulted" "image inspect" "$(ctr)"
present remote:reg.example/x/core-base:latest
run_bi "${OFF}" --builder cloud-build --target common --registry reg.example/x --dry-run
check_contains "cloud-build: registry base found" "Base:     reg.example/x/core-base:latest (found)" "${OUT}"
present
run_bi "${OFF}" --builder cloud-build --target omni --registry reg.example/x --dry-run
check_not_contains "cloud-build: omni config builds scion-base, no check" "stacks on" "${OUT}"
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "cloud-build: harnesses without scion-base: exit 1" 1 "${RC}"
check_not_contains "cloud-build: refused before submitting" "builds submit" "$(ctr)"
# cloud-build asks gcloud first (R1-F2).
present remote:reg.example/x/scion-base:latest
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "cloud-build: gcloud finds the base: exit 0" 0 "${RC}"
check_eq "cloud-build: gcloud asked first, docker not needed" "gcloud artifacts docker tags list reg.example/x/scion-base --format=value(tag) --user-output-enabled --verbosity=error" "$(ctr | grep -E 'tags list|imagetools' || true)"
present remote:reg.example/x/scion-base:latest error:gcloud:reg.example/x/scion-base:latest
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "cloud-build: gcloud errors, docker finds: exit 0" 0 "${RC}"
present error:docker:reg.example/x/scion-base:latest
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "cloud-build: gcloud not-found, docker unauthenticated: exit 1" 1 "${RC}"
check_contains "cloud-build: gcloud's not-found is definite" "which was not found in registry reg.example/x." "${OUT}"
present error:gcloud:reg.example/x/scion-base:latest error:docker:reg.example/x/scion-base:latest
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "cloud-build: both lookups error: exit 0 (warn, go ahead)" 0 "${RC}"
check_contains "cloud-build: both lookups error: reasons named" "could not be checked (gcloud: ERROR: (gcloud.artifacts.docker.tags.list) PERMISSION_DENIED: Permission 'artifactregistry.tags.list' denied; docker: ERROR:" "${OUT}"
check_not_contains "cloud-build: both lookups error: not reported as missing" "was not found" "${OUT}"
check_contains "cloud-build: both lookups error: submitted" "builds submit" "$(ctr)"
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --strict-base
check_rc "cloud-build: both lookups error, --strict-base: exit 1" 1 "${RC}"
check_not_contains "cloud-build: both lookups error, --strict-base: nothing submitted" "builds submit" "$(ctr)"
# gcloud denied plus docker's credential helper missing (C).
present error:gcloud:reg.example/x/scion-base:latest credhelper:docker:reg.example/x/scion-base:latest
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "cloud-build: gcloud denied + docker credential helper missing: exit 0 (C)" 0 "${RC}"
check_not_contains "cloud-build: (C) not reported as missing" "was not found" "${OUT}"
check_contains "cloud-build: (C) submitted" "builds submit" "$(ctr)"
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --allow-stale-base
check_rc "cloud-build: lookup error with --allow-stale-base: exit 0" 0 "${RC}"
check_contains "cloud-build: lookup error with --allow-stale-base: submitted" "builds submit" "$(ctr)"
present error:gcloud:reg.example/x/scion-base:latest
RUN_PATH="${NODOCKER}"
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
RUN_PATH=""
check_rc "cloud-build without docker: gcloud error: exit 0 (A)" 0 "${RC}"
check_contains "cloud-build without docker: gcloud error named" "could not be checked (gcloud: ERROR: (gcloud.artifacts.docker.tags.list) PERMISSION_DENIED" "${OUT}"
check_contains "cloud-build without docker: gcloud error: submitted" "builds submit" "$(ctr)"
# R2-F3: the ERROR line is shown, not a leading WARNING line.
RUN_PATH="${NODOCKER}"
RUN_ENV=(STUB_GCLOUD_WARN=1)
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
RUN_ENV=()
RUN_PATH=""
check_contains "gcloud WARNING first: the ERROR line is shown" "could not be checked (gcloud: ERROR: (gcloud.artifacts.docker.tags.list) PERMISSION_DENIED" "${OUT}"
check_not_contains "gcloud WARNING first: the WARNING line is not shown" "gcloud: WARNING" "${OUT}"
present remote:reg.example/x/scion-base:latest
RUN_PATH="${NODOCKER}"
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --dry-run
RUN_PATH=""
check_contains "cloud-build without docker: gcloud fallback" "gcloud artifacts docker tags list reg.example/x/scion-base --format=value(tag) --user-output-enabled --verbosity=error" "$(ctr)"
check_contains "cloud-build without docker: base found" "Base:     reg.example/x/scion-base:latest (found)" "${OUT}"

# gcloud tags list: exact tag match only (EM-P2-1).
RUN_PATH="${NODOCKER}"
present remote:reg.example/x/scion-base:latest-old remote:reg.example/x/scion-base:v10 remote:reg.example/x/scion-base:xlatest
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "gcloud: only latest-old/xlatest listed: :latest not found" 1 "${RC}"
check_contains "gcloud: other tags listed is a definite not-found" "which was not found in registry reg.example/x." "${OUT}"
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --tag v1
check_rc "gcloud: :v10 does not satisfy :v1" 1 "${RC}"
present remote:reg.example/x/scion-base:v10 remote:reg.example/x/scion-base:v1
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --tag v1
check_rc "gcloud: exact :v1 listed: found" 0 "${RC}"
RUN_ENV=(STUB_GCLOUD_FULLNAME=1)
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --tag v1
check_rc "gcloud: full tag resource names: found" 0 "${RC}"
present remote:reg.example/x/scion-base:v10
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --tag v1
check_rc "gcloud: full resource names, :v10 does not satisfy :v1" 1 "${RC}"
RUN_ENV=(STUB_GCLOUD_EMPTY_PKG=1)
present
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "gcloud: empty tag list (rc 0): not found" 1 "${RC}"
check_contains "gcloud: empty tag list is a definite not-found" "which was not found in registry reg.example/x." "${OUT}"
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --tag ''
check_rc "gcloud: empty tag list with --tag '': not found (R2-F4)" 1 "${RC}"
# R3-F4: output with only blank lines is a lookup error; no output at all
# (above) is a definite not-found.
RUN_ENV=(STUB_GCLOUD_BLANK=1)
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "gcloud: only blank lines (rc 0): exit 0 (lookup error)" 0 "${RC}"
check_contains "gcloud: only blank lines: named" "could not be checked (gcloud: tags list printed only blank lines for reg.example/x/scion-base)" "${OUT}"
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --strict-base
check_rc "gcloud: only blank lines, --strict-base: exit 1" 1 "${RC}"
RUN_ENV=(STUB_GCLOUD_BLANKLINE=1)
present remote:reg.example/x/scion-base:v1
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --tag ''
check_rc "gcloud: a blank line among real tags does not match --tag ''" 1 "${RC}"
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --tag v1
check_rc "gcloud: a blank line among real tags: :v1 still found" 0 "${RC}"
RUN_ENV=(STUB_GCLOUD_NOEOL=1)
present remote:reg.example/x/scion-base:latest
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "gcloud: last line without newline: found" 0 "${RC}"
RUN_ENV=()
# R3-F3(a): a gcloud error saying "not found" without NOT_FOUND is an error.
present gcnet:gcloud:reg.example/x/scion-base:x
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "gcloud: crash saying 'host not found': exit 0" 0 "${RC}"
check_contains "gcloud: crash saying 'host not found': lookup error" "could not be checked (gcloud: ERROR: gcloud crashed (ConnectionError)" "${OUT}"
# R4-F1: user output disabled must not read as "no tags".
present remote:reg.example/x/scion-base:latest
RUN_ENV=(CLOUDSDK_CORE_USER_OUTPUT_ENABLED=false)
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "gcloud: CLOUDSDK_CORE_USER_OUTPUT_ENABLED=false: base still found" 0 "${RC}"
check_contains "gcloud: --user-output-enabled passed" "--user-output-enabled --verbosity=error" "$(ctr | grep 'tags list' || true)"
# R4-F4: rc 0, empty stdout, ERROR on stderr is a lookup error.
RUN_ENV=(STUB_GCLOUD_STDERR_ERROR=1)
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "gcloud: rc 0 + empty stdout + stderr ERROR: exit 0 (lookup error)" 0 "${RC}"
check_contains "gcloud: rc 0 + stderr ERROR: named" "could not be checked (gcloud: ERROR: (gcloud.artifacts.docker.tags.list) unexpected response)" "${OUT}"
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --strict-base
check_rc "gcloud: rc 0 + stderr ERROR, --strict-base: exit 1" 1 "${RC}"
# R4-F2: CRLF tag lines.
present remote:reg.example/x/scion-base:v1 remote:reg.example/x/scion-base:latest
RUN_ENV=(STUB_GCLOUD_CRLF=1)
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "gcloud: CRLF lines: found" 0 "${RC}"
RUN_ENV=(STUB_GCLOUD_CRLF=1 STUB_GCLOUD_FULLNAME=1)
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --tag v1
check_rc "gcloud: CRLF full resource names: found" 0 "${RC}"
RUN_ENV=(STUB_GCLOUD_CRLF=1)
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --tag v2
check_rc "gcloud: CRLF lines, absent tag: not found" 1 "${RC}"
# R4-F3 (M05): a glob-looking --tag matches literally only.
present remote:reg.example/x/scion-base:v1
RUN_ENV=()
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x --tag 'v*'
check_rc "gcloud: --tag 'v*' does not glob-match :v1" 1 "${RC}"
# R4-F3 (M18): no probe temp files are left behind.
check_eq "gcloud: no probe temp files leak" "" "$(find "${TMP}" -maxdepth 1 -name 'build-images-probe.*' 2>/dev/null)"
RUN_ENV=()
present
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "gcloud: NOT_FOUND package: not found" 1 "${RC}"
check_contains "gcloud: NOT_FOUND package is a definite not-found" "which was not found in registry reg.example/x." "${OUT}"
present remote:reg.example:5000/x/scion-base:latest
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example:5000/x
check_rc "gcloud: registry with a port: found" 0 "${RC}"
check_contains "gcloud: registry with a port: image path keeps the port" "gcloud artifacts docker tags list reg.example:5000/x/scion-base --format=value(tag) --user-output-enabled --verbosity=error" "$(ctr)"
RUN_PATH=""

# Nothing to check with (dry run on a host without the tools).
present
RUN_PATH="${TMP}/empty-bin"
mkdir -p "${RUN_PATH}"
run_bi "${OFF}" --target hub --dry-run
RUN_PATH=""
check_rc "no tools, dry run: exit 0" 0 "${RC}"
check_contains "no tools, dry run: says it could not check" "could not be checked (no docker, podman or gcloud on PATH)" "${OUT}"

# ---------------------------------------------------------------------------
# Version tag warning
# ---------------------------------------------------------------------------

present local:core-base:latest
run_bi "${ON}" --dry-run --target all
check_contains "tag warning: HEAD on v9.9.9, --tag not given" "Warning: HEAD is exactly on tag v9.9.9, but --tag was not given," "${OUT}"
check_contains "tag warning: suggests --tag" "re-run with: --tag v9.9.9" "${OUT}"
check_contains "tag warning: default tag unchanged" "Tag:      latest (+ :${SHA})" "${OUT}"
run_bi "${ON}" --dry-run --target all --tag latest
check_not_contains "tag warning: explicit --tag latest silences it" "HEAD is exactly on tag" "${OUT}"
run_bi "${ON}" --dry-run --target all --tag v9.9.9
check_not_contains "tag warning: --tag v9.9.9 silences it" "HEAD is exactly on tag" "${OUT}"
run_bi "${OFF}" --dry-run --target all
check_not_contains "tag warning: HEAD not on a tag: none" "HEAD is exactly on tag" "${OUT}"

# ---------------------------------------------------------------------------
# VERSION: a v* tag wins over a nightly tag on the same commit
# ---------------------------------------------------------------------------

run_bi "${BOTH}" --dry-run --target all --tag v9.9.9
check_contains "v* first: nightly + v* on HEAD: banner names v*" "Version:  v9.9.9" "${OUT}"
check_contains "v* first: nightly + v* on HEAD: VERSION build-arg is v*" "--build-arg VERSION=v9.9.9" "$(line_for scion-base)"
check_not_contains "v* first: nightly + v* on HEAD: nightly not used" "nightly-20261106" "${OUT}"
run_bi "${BOTH}" --dry-run --target all
check_contains "v* first: nightly + v* on HEAD: warning names v*" "Warning: HEAD is exactly on tag v9.9.9, but --tag was not given," "${OUT}"
check_contains "v* first: nightly + v* on HEAD: warning suggests --tag v*" "re-run with: --tag v9.9.9" "${OUT}"
check_not_contains "v* first: nightly + v* on HEAD: warning does not name the nightly" "nightly-20261106" "${OUT}"
run_bi "${BOTH}" --dry-run --target all --builder cloud-build --registry reg.example/x --tag v9.9.9
check_contains "v* first: nightly + v* on HEAD: cloud-build gets v*" "_VERSION=v9.9.9" "${OUT}"
run_bi "${ANNOTATED}" --dry-run --target all --tag v9.9.9
check_contains "v* first: annotated nightly + lightweight v*: banner names v*" "Version:  v9.9.9" "${OUT}"
run_bi "${NIGHTLY}" --dry-run --target all --tag nightly-20261106
check_contains "v* first: only a nightly tag: banner names it" "Version:  nightly-20261106" "${OUT}"
check_contains "v* first: only a nightly tag: VERSION build-arg is the nightly" "--build-arg VERSION=nightly-20261106" "$(line_for scion-base)"
run_bi "${NIGHTLY}" --dry-run --target all
check_contains "v* first: only a nightly tag: warning names it" "Warning: HEAD is exactly on tag nightly-20261106, but --tag was not given," "${OUT}"
run_bi "${PAST}" --dry-run --target all
check_contains "v* first: v* one commit behind HEAD: off-tag banner" "Version:  <none" "${OUT}"
check_not_contains "v* first: v* one commit behind HEAD: no VERSION build-arg" "VERSION=" "$(line_for scion-base)"
check_not_contains "v* first: v* one commit behind HEAD: no warning" "HEAD is exactly on tag" "${OUT}"

# ---------------------------------------------------------------------------
# BUILD_TIME / SOURCE_DATE_EPOCH from the commit time
# ---------------------------------------------------------------------------

run_bi "${OFF}" --dry-run --target all
first="${OUT}"
check_contains "build time: banner shows the commit time" "Built at: ${COMMIT_DATE} (SOURCE_DATE_EPOCH=${COMMIT_EPOCH})" "${OUT}"
check_contains "build time: scion-base gets BUILD_TIME" "--build-arg BUILD_TIME=${COMMIT_DATE}" "$(line_for scion-base)"
check_not_contains "build time: core-base gets no BUILD_TIME" "BUILD_TIME" "$(line_for core-base)"
check_not_contains "build time: harness gets no BUILD_TIME" "BUILD_TIME" "$(line_for scion-claude)"
run_bi "${OFF}" --dry-run --target all
check_eq "build time: two runs print identical commands" "${first}" "${OUT}"
run_bi "${OFF}" --dry-run --target scion-omni --allow-stale-base
check_contains "build time: scion-omni gets BUILD_TIME" "--build-arg BUILD_TIME=${COMMIT_DATE}" "$(line_for scion-omni)"

run_bi "${OFF}" --target all
check_eq "build time: SOURCE_DATE_EPOCH exported to the builder" "SOURCE_DATE_EPOCH=${COMMIT_EPOCH}" "$(head -n 1 "${TMP}/env.log")"

RUN_ENV=(SOURCE_DATE_EPOCH=1000000000)
run_bi "${OFF}" --dry-run --target all
RUN_ENV=()
check_contains "build time: explicit SOURCE_DATE_EPOCH wins" "--build-arg BUILD_TIME=2001-09-09T01:46:40Z" "$(line_for scion-base)"
RUN_PATH="${BSDDATE}"
run_bi "${OFF}" --dry-run --target all
RUN_PATH=""
check_contains "build time: BSD date (-r) fallback gives the commit time" "--build-arg BUILD_TIME=${COMMIT_DATE}" "$(line_for scion-base)"
RUN_ENV=(SOURCE_DATE_EPOCH=yesterday)
run_bi "${OFF}" --dry-run --target all
RUN_ENV=()
check_rc "build time: non-numeric SOURCE_DATE_EPOCH: exit 1" 1 "${RC}"
check_contains "build time: non-numeric SOURCE_DATE_EPOCH: message" "SOURCE_DATE_EPOCH must be a Unix timestamp" "${OUT}"

run_bi "${ON}" --dry-run --target all --builder cloud-build --registry reg.example/x
check_contains "build time: cloud-build passes _BUILD_TIME" "_BUILD_TIME=${COMMIT_DATE}" "${OUT}"
run_bi "${OFF}" --dry-run --target scion-claude --builder cloud-build --registry reg.example/x --allow-stale-base
check_not_contains "build time: single-harness cloud config gets no _BUILD_TIME" "_BUILD_TIME" "${OUT}"

# Every Cloud Build config that builds scion-base or scion-omni passes
# BUILD_TIME and defaults _BUILD_TIME to empty.
for f in cloudbuild.yaml cloudbuild-common.yaml cloudbuild-scion-base.yaml cloudbuild-thick.yaml cloudbuild-omni.yaml; do
  y="${IMAGE_BUILD_SRC}/${f}"
  want="$(grep -cE "image-build/(scion-base|omni)/Dockerfile" "${y}" || true)"
  # shellcheck disable=SC2016 # a literal yaml line, not an expansion
  got="$(grep -c 'BUILD_TIME=\$_BUILD_TIME' "${y}" || true)"
  check_eq "cloudbuild: ${f} passes BUILD_TIME to each of its ${want} scion-base/omni builds" "${want}" "${got}"
  check_contains "cloudbuild: ${f} defaults _BUILD_TIME" "_BUILD_TIME: ''" "$(cat "${y}")"
done

# ---------------------------------------------------------------------------
# OCI version label
# ---------------------------------------------------------------------------

run_bi "${ON}" --dry-run --target all --tag v9.9.9
check_contains "version: banner" "Version:  v9.9.9" "${OUT}"
check_contains "version label: scion-base build carries VERSION" "--build-arg VERSION=v9.9.9" "$(line_for scion-base)"
check_contains "version label: scion-base build carries GIT_COMMIT" "--build-arg GIT_COMMIT=" "$(line_for scion-base)"
run_bi "${OFF}" --dry-run --target all
check_not_contains "version label: off-tag build passes no VERSION" "VERSION=" "$(line_for scion-base)"
check_contains "version: off-tag banner" "Version:  <none" "${OUT}"

# The label itself: final stage of scion-base, next to the revision label,
# fed by a stage-scoped ARG VERSION.
final_stage="$(awk '/^FROM /{buf=""} {buf=buf $0 "\n"} END{printf "%s", buf}' "${IMAGE_BUILD_SRC}/scion-base/Dockerfile")"
# shellcheck disable=SC2016 # literal Dockerfile lines, not expansions
check_contains "version label: revision label in the final stage" 'LABEL org.opencontainers.image.revision="${GIT_COMMIT}"' "${final_stage}"
# shellcheck disable=SC2016 # literal Dockerfile lines, not expansions
check_contains "version label: version label in the final stage" 'LABEL org.opencontainers.image.version="${VERSION}"' "${final_stage}"
check_eq "version label: ARG VERSION re-declared in the final stage" "1" "$(printf '%s' "${final_stage}" | grep -c '^ARG VERSION$' || true)"

# ---------------------------------------------------------------------------
# The P1 driver's detections still hold for this tree
# ---------------------------------------------------------------------------

DRIVER="${IMAGE_BUILD_SRC}/scripts/build-release-images.sh"
if [ -f "${DRIVER}" ]; then
  fns="$(sed -n '/^tree_is_stamped() {/,/^}/p; /^tree_has_step_targets() {/,/^}/p' "${DRIVER}")"
  if [ -n "${fns}" ] && eval "${fns}"; then
    if tree_is_stamped "${OFF}"; then pass "driver: tree_is_stamped"; else fail "driver: tree_is_stamped"; fi
    if tree_has_step_targets "${OFF}"; then pass "driver: tree_has_step_targets"; else fail "driver: tree_has_step_targets"; fi
  else
    fail "driver: could not load its detections"
  fi
fi

# ---------------------------------------------------------------------------
# Error-first precedence (R5-F1 + coordinator-2 addendum): a not-found
# marker next to ANY error marker is a lookup error, for every backend.
# ---------------------------------------------------------------------------

# Direct classifier calls, loaded from the build-images.sh under test.
cls_fns="$(sed -n "/^PROBE_ERROR_MARKERS=/p; /^probe_detail() {/,/^}/p; /^mask_ref() {/,/^}/p; /^classify_probe_failure() {/,/^}/p" "${IMAGE_BUILD_SRC}/scripts/build-images.sh")"
if [ -n "${cls_fns}" ] && eval "${cls_fns}"; then
  cls() { local rc=0; classify_probe_failure "$1" "$2" "${3:-}" || rc=$?; echo "${rc}"; }
  for row in \
    "docker-store|Error: No such image: core-base:latest|Error: No such image: core-base:latest (retry: dial tcp 10.0.0.1:443: i/o timeout)" \
    "podman-store|Error: core-base:latest: image not known|Error: core-base:latest: image not known (storage: connection refused)" \
    "docker-registry|ERROR: r/x/core-base:latest: not found|ERROR: r/x/core-base:latest: not found; 429 Too Many Requests" \
    "podman-registry|Error: reading manifest latest in r/x/core-base: manifest unknown|Error: (Mirrors also failed: [m/x/core-base:latest: reading manifest latest in m/x/core-base: manifest unknown]): r/x/core-base:latest: received unexpected HTTP status: 502 Bad Gateway" \
    "gcloud|ERROR: (gcloud.artifacts.docker.tags.list) NOT_FOUND: Requested entity was not found.|ERROR: (gcloud.artifacts.docker.tags.list) NOT_FOUND: Requested entity was not found. gcloud crashed (ConnectionError)"; do
    k="${row%%|*}"; rest="${row#*|}"; nf="${rest%%|*}"; mixed="${rest#*|}"
    check_eq "classifier ${k}: pure not-found is definite (1)" "1" "$(cls "${k}" "${nf}")"
    check_eq "classifier ${k}: not-found + error marker is a lookup error (2)" "2" "$(cls "${k}" "${mixed}")"
  done
  check_eq "classifier: markers match in any case (I/O TIMEOUT)" "2" "$(cls docker-registry "ERROR: manifest unknown; I/O TIMEOUT")"
  check_eq "classifier: a buildx tool error with 'not found' mid-line is a lookup error" "2" "$(cls docker-registry "ERROR: failed to get current builder: builder not found in this context")"
  # R6-F3: one string per PROBE_ERROR_MARKERS alternative, each holding
  # only that marker, so dropping any single alternative fails a check.
  # shellcheck disable=SC2016 # literal tool messages, nothing expands
  for marker in \
    'exec: "helper": executable file not found in $PATH' 'sh: helper: command not found' 'error getting credentials' \
    '401 Unauthorized' 'requested access to the resource is denied' '403 Forbidden' 'insufficient permission' \
    '(Mirrors also failed: [m.example/x: x])' 'i/o timeout' 'operation timed out' 'context deadline exceeded' \
    'toomanyrequests: slow down' 'too many requests' 'you have reached your pull rate limit' 'HTTP 429 ' 'status: 429' \
    'status: 504 Gateway Time-out' '500 Internal Server Error' 'Service Unavailable' 'Bad Gateway' \
    'connection refused' 'connection reset by peer' 'network is unreachable' 'lookup reg: no such host' \
    'dial tcp 1.2.3.4:443' 'remote error: tls handshake failure' 'x509: unknown authority' 'bad certificate' \
    'unexpected EOF' 'gcloud crashed (ValueError)' 'Traceback (most recent call last)' \
    'Cannot connect to the Docker daemon at unix:///var/run/docker.sock' 'Cannot connect to Podman. Please verify' \
    'Is the docker daemon running?'; do
    check_eq "classifier: isolated marker '${marker}' beats 'manifest unknown'" "2" "$(cls docker-registry "ERROR: manifest unknown; ${marker}")"
  done
  # R6-F2: the looked-up ref is masked before the marker check.
  check_eq "classifier: tag 'permission-fix' in a pure not-found is definite" "1" "$(cls docker-registry "ERROR: r/x/core-base:permission-fix: not found" r/x/core-base:permission-fix)"
  check_eq "classifier: tag 'timeout-fix' in podman's not-found is definite" "1" "$(cls podman-registry "Error: reading manifest timeout-fix in r/x/core-base: manifest unknown" r/x/core-base:timeout-fix)"
  check_eq "classifier: path 'credentials-prod' in a not-found is definite" "1" "$(cls docker-registry "ERROR: r/credentials-prod/core-base:latest: not found" r/credentials-prod/core-base:latest)"
  check_eq "classifier: tag 'certificate-rotation' in a store miss is definite" "1" "$(cls docker-store "Error: No such image: core-base:certificate-rotation" core-base:certificate-rotation)"
  check_eq "classifier: gcloud image path 'credentials-prod' is masked" "1" "$(cls gcloud "ERROR: (gcloud.artifacts.docker.tags.list) NOT_FOUND: Requested entity was not found: r/credentials-prod/core-base" r/credentials-prod/core-base)"
  check_eq "classifier: masked tag, real marker still wins" "2" "$(cls docker-registry "ERROR: r/x/core-base:timeout-fix: not found; i/o timeout" r/x/core-base:timeout-fix)"
  check_eq "classifier: tag named 'timeout', real i/o timeout still wins" "2" "$(cls docker-registry "ERROR: r/x/core-base:timeout: dial failed: i/o timeout" r/x/core-base:timeout)"
  check_eq "classifier: tag named 'timeout' next to a not-found, real marker still wins" "2" "$(cls docker-registry "ERROR: r/x/core-base:timeout: manifest unknown (i/o timeout)" r/x/core-base:timeout)"
  check_eq "classifier: registry port is not taken for a tag" "1" "$(cls docker-registry "ERROR: reg:5000/x/core-base:latest: not found" reg:5000/x/core-base:latest)"
  for marker in "denied" "permission" "i/o timeout" "status: 429" "503 Service Unavailable" "500 Internal Server Error" "connection refused" "no such host" "x509: certificate" "Mirrors also failed" "gcloud crashed" "Cannot connect to the Docker daemon"; do
    check_eq "classifier: '${marker}' beats 'manifest unknown'" "2" "$(cls docker-registry "ERROR: manifest unknown; ${marker}")"
  done
else
  fail "classifier: could not load classify_probe_failure from build-images.sh"
fi

# The same through the guard, one backend each: warn and go ahead.
present mixed:docker:core-base:latest
run_bi "${OFF}" --target common
check_rc "mixed docker store message: exit 0" 0 "${RC}"
check_contains "mixed docker store message: lookup error" "which could not be checked (docker:" "${OUT}"
present mixed:docker:reg.example/x/core-base:latest
run_bi "${OFF}" --target common --registry reg.example/x --push
check_rc "mixed docker registry message: exit 0" 0 "${RC}"
check_contains "mixed docker registry message: lookup error" "which could not be checked (docker:" "${OUT}"
present mixed:podman:core-base:latest
run_bi "${OFF}" --builder local-podman --target scion-base
check_rc "mixed podman store message: exit 0" 0 "${RC}"
check_contains "mixed podman store message: lookup error" "which could not be checked (podman:" "${OUT}"
present mixed:podman:reg.example/x/core-base:latest
run_bi "${OFF}" --builder local-podman --target scion-base --registry reg.example/x --push
check_rc "mixed podman registry message: exit 0" 0 "${RC}"
check_contains "mixed podman registry message: lookup error" "which could not be checked (podman:" "${OUT}"
present gcmixed:gcloud:reg.example/x/scion-base:x
RUN_PATH="${NODOCKER}"
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "mixed gcloud message: exit 0" 0 "${RC}"
check_contains "mixed gcloud message: lookup error" "which could not be checked (gcloud:" "${OUT}"

# R3-F3/R20: PERMISSION_DENIED next to NOT_FOUND is a lookup error.
present gcdenynf:gcloud:reg.example/x/scion-base:x
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "gcloud PERMISSION_DENIED + NOT_FOUND: exit 0" 0 "${RC}"
check_contains "gcloud PERMISSION_DENIED + NOT_FOUND: lookup error" "which could not be checked (gcloud: ERROR:" "${OUT}"
# R12: NOT_FOUND outside the ERROR line does not count.
present gcwarnnf:gcloud:reg.example/x/scion-base:x
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "gcloud NOT_FOUND only in a WARNING line: exit 0 (lookup error)" 0 "${RC}"
check_contains "gcloud NOT_FOUND only in a WARNING line: the ERROR line is named" "could not be checked (gcloud: ERROR: (gcloud.artifacts.docker.tags.list) INVALID_ARGUMENT" "${OUT}"
# R14: CR-only output is the blank-only lookup error.
present remote:reg.example/x/scion-base:latest
RUN_ENV=(STUB_GCLOUD_CRONLY=1)
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
check_rc "gcloud CR-only output: exit 0 (lookup error)" 0 "${RC}"
check_contains "gcloud CR-only output: blank-only error" "tags list printed only blank lines" "${OUT}"
# R5-F2: the user's gcloud log config cannot hide a real NOT_FOUND.
present
for cfg in structured detailed quiet; do
  RUN_ENV=(STUB_GCLOUD_USERCFG="${cfg}")
  run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/x
  check_rc "gcloud user config '${cfg}': NOT_FOUND still definite (exit 1)" 1 "${RC}"
done
RUN_ENV=()
RUN_PATH=""

# R5-F1: podman registries.conf mirror aggregation.
for shape in timeout 503 429; do
  present "mirror${shape}:podman:reg.example/x/core-base:latest"
  run_bi "${OFF}" --builder local-podman --target scion-base --registry reg.example/x --push
  check_rc "podman mirror miss + primary ${shape}: exit 0 (lookup error)" 0 "${RC}"
  check_contains "podman mirror miss + primary ${shape}: named" "which could not be checked (podman: Error: (Mirrors also failed:" "${OUT}"
  check_contains "podman mirror miss + primary ${shape}: builds" "podman build" "$(ctr)"
done
present mirrortimeout:podman:reg.example/x/core-base:latest
run_bi "${OFF}" --builder local-podman --target scion-base --registry reg.example/x --push --strict-base
check_rc "podman mirror miss + primary timeout, --strict-base: exit 1" 1 "${RC}"
# Rule: a mirror message whose primary also says not found is still a
# lookup error (multi-source output is never a definite answer).
present mirrornf:podman:reg.example/x/core-base:latest
run_bi "${OFF}" --builder local-podman --target scion-base --registry reg.example/x --push
check_rc "podman mirror miss + primary not found: exit 0 (lookup error)" 0 "${RC}"
check_contains "podman mirror miss + primary not found: named" "which could not be checked (podman: Error: (Mirrors also failed:" "${OUT}"

# R6-F2 through the guard: a marker word in the tag is not an error marker.
present
run_bi "${OFF}" --target common --registry reg.example/x --push --tag permission-fix
check_rc "tag permission-fix, registry not-found: exit 1 (definite)" 1 "${RC}"
check_contains "tag permission-fix: reported as missing" "which was not found in registry reg.example/x." "${OUT}"
run_bi "${OFF}" --builder local-podman --target scion-base --registry reg.example/x --push --tag timeout-fix
check_rc "podman tag timeout-fix, registry not-found: exit 1 (definite)" 1 "${RC}"
run_bi "${OFF}" --target common --registry reg.example/credentials-prod --push
check_rc "registry path credentials-prod, not-found: exit 1 (definite)" 1 "${RC}"
RUN_PATH="${NODOCKER}"
run_bi "${OFF}" --builder cloud-build --target harnesses --registry reg.example/credentials-prod
RUN_PATH=""
check_rc "gcloud, registry path credentials-prod, NOT_FOUND: exit 1 (definite)" 1 "${RC}"

# ---------------------------------------------------------------------------
# R6-F1 + sweep: git log is immune to log.showSignature and color.ui=always;
# a git-derived non-integer falls back to the build clock.
# ---------------------------------------------------------------------------

GITSHIM="${TMP}/stubbin-gitshim"
mkdir -p "${GITSHIM}"
cat > "${GITSHIM}/git" <<STUB
#!/usr/bin/env bash
# git as a user with log.showSignature=true and color.ui=always would see
# it, unless the call opts out per command.
real="$(command -v git)"
is_log=""; nosig=""; nocolor=""
for a in "\$@"; do
  case "\${a}" in
    log) is_log=1 ;;
    --no-show-signature) nosig=1 ;;
    --no-color) nocolor=1 ;;
  esac
done
if [ -n "\${is_log}" ]; then
  if [ -n "\${STUB_GIT_GARBAGE:-}" ]; then
    echo "warning: refname 'HEAD' is ambiguous."
    exit 0
  fi
  if [ -z "\${nosig}" ]; then
    echo "gpg: Signature made Tue 02 Jan 2024 03:04:05 AM UTC"
    echo "No signature"
  fi
  if [ -z "\${nocolor}" ]; then
    printf '\033[33m'
    "\${real}" "\$@"
    rc=\$?
    printf '\033[m'
    exit "\${rc}"
  fi
fi
exec "\${real}" "\$@"
STUB
chmod +x "${GITSHIM}/git"
cp "${STUBBIN}/docker" "${STUBBIN}/podman" "${STUBBIN}/gcloud" "${GITSHIM}/"

RUN_PATH="${GITSHIM}"
run_bi "${OFF}" --dry-run --target all
check_rc "git showSignature/color: exit 0" 0 "${RC}"
check_contains "git showSignature/color: commit time still read" "Built at: ${COMMIT_DATE} (SOURCE_DATE_EPOCH=${COMMIT_EPOCH})" "${OUT}"
check_contains "git showSignature/color: BUILD_TIME is the commit time" "--build-arg BUILD_TIME=${COMMIT_DATE}" "$(line_for scion-base)"
check_not_contains "git showSignature/color: no fallback warning" "could not read the commit time" "${OUT}"
RUN_ENV=(STUB_GIT_GARBAGE=1)
run_bi "${OFF}" --dry-run --target all
check_rc "git log garbage: exit 0" 0 "${RC}"
check_contains "git log garbage: warns and falls back" "Warning: could not read the commit time (git log printed 'warning: refname 'HEAD' is ambiguous.'); using the build clock." "${OUT}"
check_contains "git log garbage: banner shows the build clock" "Built at: <build host clock>" "${OUT}"
check_not_contains "git log garbage: no BUILD_TIME build-arg" "BUILD_TIME" "$(line_for scion-base)"
run_bi "${OFF}" --target all
check_rc "git log garbage, real run: exit 0" 0 "${RC}"
check_eq "git log garbage: SOURCE_DATE_EPOCH not exported" "SOURCE_DATE_EPOCH=<unset>" "$(head -n 1 "${TMP}/env.log")"
RUN_ENV=(STUB_GIT_GARBAGE=1 SOURCE_DATE_EPOCH=abc)
run_bi "${OFF}" --dry-run --target all
check_rc "explicit SOURCE_DATE_EPOCH=abc: still exit 1" 1 "${RC}"
check_contains "explicit SOURCE_DATE_EPOCH=abc: error" "Error: SOURCE_DATE_EPOCH must be a Unix timestamp in seconds, got 'abc'" "${OUT}"
RUN_ENV=(SOURCE_DATE_EPOCH=1000000000)
run_bi "${OFF}" --dry-run --target all
check_contains "explicit SOURCE_DATE_EPOCH with the git shim: wins" "--build-arg BUILD_TIME=2001-09-09T01:46:40Z" "$(line_for scion-base)"
RUN_ENV=()
RUN_PATH=""

echo ""
echo "${PASSES} passed, ${FAILS} failed"
[ "${FAILS}" -eq 0 ]
