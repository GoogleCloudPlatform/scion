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

# Tests for build-release-images.sh. Pure bash: no docker, no network.
#
# A temp git repository with tags plays the upstream (cloned over file://).
# Its image-build/scripts/build-images.sh is a stub that records its argv and
# the version its tree would stamp (`git describe --tags --exact-match`).
# docker, podman, scion, gcloud and sleep are stubs. The driver runs with PATH set to the
# stub dir plus a private toolbin of symlinks to the real tools it needs, so
# a real docker, podman, skopeo, scion or gcloud next to git (e.g. /usr/bin on CI
# runners) is never reachable. A self-check at the end re-runs the suite
# with fake docker, podman, skopeo, scion and gcloud in the same directory as the tools.
#
# DRIVER_UNDER_TEST=<path> runs the suite against another copy of the driver
# (used to show that a broken driver fails the suite).
#
# Usage: bash image-build/scripts/test-build-release-images.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DRIVER="${DRIVER_UNDER_TEST:-${SCRIPT_DIR}/build-release-images.sh}"
[ -f "${DRIVER}" ] || { echo "driver not found: ${DRIVER}" >&2; exit 2; }
HACK_VERSION="${SCRIPT_DIR}/../../hack/version.sh"
[ -f "${HACK_VERSION}" ] || { echo "hack/version.sh not found: ${HACK_VERSION}" >&2; exit 2; }

TMP="$(mktemp -d "${TMPDIR:-/tmp}/test-build-release-images.XXXXXX")"
trap 'rm -rf -- "${TMP:?}"' EXIT

FAILS=0
PASSES=0
pass() { PASSES=$((PASSES + 1)); echo "ok   - $1"; }
fail() { FAILS=$((FAILS + 1)); echo "FAIL - $1" >&2; }

# check_eq <name> <want> <got>
check_eq() {
  if [ "$2" = "$3" ]; then
    pass "$1"
  else
    fail "$1"
    printf '       want: %s\n       got:  %s\n' "$2" "$3" >&2
  fi
}
# check_contains <name> <needle> <haystack>
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
# Fixture upstream repository
# ---------------------------------------------------------------------------

export GIT_AUTHOR_NAME="test" GIT_AUTHOR_EMAIL="test@example.com"
export GIT_COMMITTER_NAME="test" GIT_COMMITTER_EMAIL="test@example.com"
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
# Tests clone their own file:// fixture; protocol.file.allow is not needed for
# a top-level file:// clone.

UP="${TMP}/up"
mkdir -p "${UP}/image-build/scripts/lib" "${UP}/image-build/scion-base" "${UP}/hack"
# The real hack/version.sh: the driver runs the tree's copy to check that a
# stamped tree stamps exactly the tag.
cp "${HACK_VERSION}" "${UP}/hack/version.sh"
printf 'FROM core-base\nARG VERSION\n' > "${UP}/image-build/scion-base/Dockerfile"
# The step-ID fallback line that v0.3.0-preview.4+ targets.sh carries.
# shellcheck disable=SC2016 # literal file content, not an expansion
STEP_FALLBACK='      for _id in "${ALL_STEP_IDS[@]}"; do'
printf '%s\n' "resolve_targets() {" "${STEP_FALLBACK}" "}" > "${UP}/image-build/scripts/lib/targets.sh"
cat > "${UP}/image-build/scripts/build-images.sh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
printf '%s\n' "$*" >> "${STUB_ARGV_LOG:?}"
printf 'THICK=%s OMNI=%s\n' "${THICK_BUILD:-unset}" "${OMNI_BUILD:-unset}" >> "${STUB_ENV_LOG:?}"
stamp="$(git -C "${root}" describe --tags --exact-match 2>/dev/null || echo '<none>')"
printf '%s\n' "${stamp}" >> "${STUB_STAMP_LOG:?}"
target="common"
builder="local-docker"
while [ $# -gt 0 ]; do
  case "$1" in
    --target) target="$2"; shift 2 ;;
    --builder) builder="$2"; shift 2 ;;
    --tag|--registry|--platform) shift 2 ;;
    *) shift ;;
  esac
done
case "${target}" in
  all) steps="core-base scion-base scion-claude scion-hub" ;;
  common) steps="scion-base scion-claude scion-hub" ;;
  harnesses) steps="scion-claude" ;;
  hub) steps="scion-hub" ;;
  *) steps="${target}" ;;
esac
if [ -n "${STUB_BANNER:-}" ]; then
  steps="${STUB_BANNER}"
fi
if [ "${builder}" = "cloud-build" ]; then
  # Target mode, as v0.3.0-preview.5's build-images.sh prints it; its
  # builder only submits (gcloud builds submit --async) and returns.
  echo "Config:   image-build/cloudbuild.yaml"
  echo "Steps:    ${steps}  (image builds in config)"
  echo "==> [cloud-build] submitting target '${target}' via image-build/cloudbuild.yaml"
  echo "ID: stub-build-0001  STATUS: QUEUED"
  # gcloud prints the build's resource name on stderr.
  echo "Created [https://cloudbuild.googleapis.com/v1/projects/stub-proj/locations/global/builds/${STUB_CB_ID:-0b1c2d3e-0000-4000-8000-000000000001}]." >&2
  if [ -n "${STUB_CB_ID2:-}" ]; then
    echo "Created [https://cloudbuild.googleapis.com/v1/projects/stub-proj/locations/global/builds/${STUB_CB_ID2}]." >&2
  fi
  echo ""
  echo "Build submitted. View progress at:"
  echo "  https://console.cloud.google.com/cloud-build/builds?project=stub"
else
  echo "Steps:    ${steps}"
fi
if [ -n "${STUB_FAIL:-}" ]; then
  echo "stub: simulated build failure" >&2
  exit 3
fi
STUB
chmod +x "${UP}/image-build/scripts/build-images.sh"
git -C "${UP}" init -q -b main
git -C "${UP}" add -A
git -C "${UP}" commit -q -m "c1"
C1="$(git -C "${UP}" rev-parse HEAD)"
# Two lightweight tags on one commit. nightly-* sorts first, so a plain
# tag-following clone would make `git describe` print it instead of v1.0.0.
git -C "${UP}" tag v1.0.0
git -C "${UP}" tag nightly-20200101
# A weekly preview and a release candidate on the same commit (--channel-tag).
git -C "${UP}" tag v1.0.0-preview.1
git -C "${UP}" tag v1.0.0-rc.1
echo "two" > "${UP}/README"
git -C "${UP}" add README
git -C "${UP}" commit -q -m "c2"
C2="$(git -C "${UP}" rev-parse HEAD)"
git -C "${UP}" tag -a -m "release v1.1.0" v1.1.0
# A release from before version stamping (like v0.3.0-preview.1 to .3): its
# scion-base Dockerfile has no ARG VERSION.
printf 'FROM core-base\n' > "${UP}/image-build/scion-base/Dockerfile"
printf '%s\n' "resolve_targets() {" "}" > "${UP}/image-build/scripts/lib/targets.sh"
git -C "${UP}" commit -q -am "c3: no version stamping, no single-image targets"
C3="$(git -C "${UP}" rev-parse HEAD)"
git -C "${UP}" tag v0.9.0
# The other half: ARG VERSION is back, but build-images.sh does not derive
# VERSION with `git describe --tags --exact-match`.
printf 'FROM core-base\nARG VERSION\n' > "${UP}/image-build/scion-base/Dockerfile"
printf '%s\n' "resolve_targets() {" "${STEP_FALLBACK}" "}" > "${UP}/image-build/scripts/lib/targets.sh"
sed 's/describe --tags --exact-match/describe --exact-match --tags/' "${UP}/image-build/scripts/build-images.sh" > "${TMP}/bi.sh"
cp "${TMP}/bi.sh" "${UP}/image-build/scripts/build-images.sh"
git -C "${UP}" commit -q -am "c4: no VERSION derivation"
C4="$(git -C "${UP}" rev-parse HEAD)"
git -C "${UP}" tag v0.9.1
# A stamped tree on a side branch (main stays at C4) tagged with a form
# hack/version.sh does not accept as a release tag.
git -C "${UP}" checkout -q -b side
sed 's/describe --exact-match --tags/describe --tags --exact-match/' "${UP}/image-build/scripts/build-images.sh" > "${TMP}/bi.sh"
cp "${TMP}/bi.sh" "${UP}/image-build/scripts/build-images.sh"
git -C "${UP}" commit -q -am "c5: stamped again"
git -C "${UP}" tag v1.2.0-beta.1
git -C "${UP}" tag v1.2.0-preview.01
git -C "${UP}" checkout -q main
REPO_URL="file://${UP}"

# ---------------------------------------------------------------------------
# Stubs and environment
# ---------------------------------------------------------------------------

STUBBIN="${TMP}/stubbin"
mkdir -p "${STUBBIN}"
cat > "${STUBBIN}/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s %s\n' "$(basename "$0")" "$*" >> "${STUB_CTR_LOG:?}"
ref=""
for a in "$@"; do ref="${a}"; done
fmt="false"
case " $* " in *" --format "*) fmt="true" ;; esac
# Tags made by "tag" and "imagetools create" are recorded as "<dst> <src>"
# in STUB_ALIAS; a digest lookup of <dst> answers for <src>.
alias_of() { awk -v r="$1" '$1 == r { v = $2 } END { print v }' "${STUB_ALIAS:?}"; }
# dg <ref> <kind>: a ref@digest answers its digest. With STUB_HEX set, the
# digest is sha256:<hex of the ref>; otherwise sha256:<kind>-<ref>.
dg() {
  case "$1" in *@*) printf '%s' "${1#*@}"; return ;; esac
  if [ -n "${STUB_HEX:-}" ]; then
    printf 'sha256:%s' "$(printf '%s' "$1" | sha256sum | cut -c1-64)"
  else
    printf 'sha256:%s-%s' "$2" "$1"
  fi
}
case "$1 ${2:-} ${3:-}" in
  "image inspect "*)
    grep -qxF "local:${ref}" "${STUB_PRESENT:?}" || exit 1
    if [ "${fmt}" = "true" ]; then echo "sha256:id-${ref}"; fi ;;
  "buildx imagetools inspect"|"manifest inspect "*)
    # STUB_INSPECT_DENY=<ref>: the lookup fails with a non-"not found" error.
    if [ -n "${STUB_INSPECT_DENY:-}" ] && [ "${ref}" = "${STUB_INSPECT_DENY}" ]; then
      echo "ERROR: ${ref}: denied: permission denied" >&2; exit 1
    fi
    target="$(alias_of "${ref}")"
    if [ -z "${target}" ]; then
      grep -qxF "remote:${ref}" "${STUB_PRESENT:?}" || { echo "ERROR: ${ref}: not found" >&2; exit 1; }
      target="${ref}"
    fi
    if [ "${fmt}" = "true" ]; then dg "${target}" dg; echo; fi ;;
  "buildx imagetools create")
    # buildx imagetools create -t <dst> <src>; STUB_CREATE_FAIL=<dst> fails.
    if [ -n "${STUB_CREATE_FAIL:-}" ] && [ "$5" = "${STUB_CREATE_FAIL}" ]; then exit 1; fi
    printf '%s %s\n' "$5" "$6" >> "${STUB_ALIAS:?}" ;;
  "tag "*)
    printf '%s %s\n' "$3" "$2" >> "${STUB_ALIAS:?}" ;;
  "push --digestfile "*)
    target="$(alias_of "${ref}")"
    dg "${target:-${ref}}" pd > "$3" ;;
  "push "*)
    # STUB_PUSH_FAIL=<ref>: a plain push of <ref> fails.
    if [ -n "${STUB_PUSH_FAIL:-}" ] && [ "${ref}" = "${STUB_PUSH_FAIL}" ]; then exit 1; fi ;;
esac
exit 0
STUB
chmod +x "${STUBBIN}/docker"
cp "${STUBBIN}/docker" "${STUBBIN}/podman"
cat > "${STUBBIN}/scion" <<'STUB'
#!/usr/bin/env bash
[ "$*" = "version --format json" ] || exit 3
printf '%s\n' "${STUB_SCION_JSON:?}"
STUB
chmod +x "${STUBBIN}/scion"
# gcloud: Cloud Build status and results, and Artifact Registry tags.
#   STUB_CB_STATUS     status of every build (default SUCCESS)
#   STUB_CB_FAIL_ID    this build ID reports FAILURE
#   STUB_CB_DESCRIBE_FAIL  status lookups fail
#   STUB_CB_IMAGES     file: results.images as "<name><TAB><digest>" lines
#   STUB_CB_STEPOUT    file: results.buildStepOutputs as value() prints it
#   STUB_AR            file: "<ref> <digest>" lines; tags add appends
cat > "${STUBBIN}/gcloud" <<'STUB'
#!/usr/bin/env bash
printf 'gcloud %s\n' "$*" >> "${STUB_CTR_LOG:?}"
fmt=""
for a in "$@"; do
  case "${a}" in --format=*) fmt="${a#--format=}" ;; esac
done
case "$1 ${2:-}" in
  "builds describe")
    case "${fmt}" in
      'value(status)')
        [ -z "${STUB_CB_DESCRIBE_FAIL:-}" ] || exit 1
        if [ -n "${STUB_CB_FAIL_ID:-}" ] && [ "$3" = "${STUB_CB_FAIL_ID}" ]; then
          echo FAILURE
        else
          echo "${STUB_CB_STATUS:-SUCCESS}"
        fi ;;
      # A non-empty file named <file>.<build id> answers for that build.
      'value(results.images.name,results.images.digest)')
        f="${STUB_CB_IMAGES:?}"; [ ! -s "${f}.$3" ] || f="${f}.$3"; cat "${f}" ;;
      'value(results.buildStepOutputs)')
        f="${STUB_CB_STEPOUT:?}"; [ ! -s "${f}.$3" ] || f="${f}.$3"; cat "${f}" ;;
      *) exit 2 ;;
    esac ;;
  "artifacts docker")
    case "${3:-} ${4:-}" in
      "images describe")
        # STUB_AR_DESCRIBE_FAIL=<ref>: the lookup fails (not "not found").
        # STUB_AR_READBACK_FAIL=<ref>: the same, once a tags add made <ref>.
        if [ -n "${STUB_AR_DESCRIBE_FAIL:-}" ] && [ "$5" = "${STUB_AR_DESCRIBE_FAIL}" ]; then
          echo "ERROR: (gcloud.artifacts.docker.images.describe) PERMISSION_DENIED: denied" >&2; exit 1
        fi
        if [ -n "${STUB_AR_READBACK_FAIL:-}" ] && [ "$5" = "${STUB_AR_READBACK_FAIL}" ] \
          && grep -q "^$5 " "${STUB_AR_TAGGED:?}"; then
          echo "ERROR: (gcloud.artifacts.docker.images.describe) UNAVAILABLE: try again" >&2; exit 1
        fi
        v="$(awk -v r="$5" '$1 == r { v = $2 } END { print v }' "${STUB_AR:?}")"
        if [ -z "${v}" ]; then
          echo "ERROR: (gcloud.artifacts.docker.images.describe) NOT_FOUND: Requested entity was not found." >&2; exit 1
        fi
        echo "${v}" ;;
      "tags add")
        # STUB_AR_TAGS_FAIL=<dst> fails; STUB_AR_TAGS_WRONG=<dst> records
        # another digest for <dst>.
        if [ -n "${STUB_AR_TAGS_FAIL:-}" ] && [ "$6" = "${STUB_AR_TAGS_FAIL}" ]; then exit 1; fi
        d="${5#*@}"
        if [ -n "${STUB_AR_TAGS_WRONG:-}" ] && [ "$6" = "${STUB_AR_TAGS_WRONG}" ]; then
          d="sha256:$(printf 'wrong' | sha256sum | cut -c1-64)"
        fi
        printf '%s %s\n' "$6" "${d}" >> "${STUB_AR:?}"
        printf '%s %s\n' "$6" "${d}" >> "${STUB_AR_TAGGED:?}" ;;
      *) exit 2 ;;
    esac ;;
  *) exit 2 ;;
esac
STUB
chmod +x "${STUBBIN}/gcloud"
cat > "${STUBBIN}/sleep" <<'STUB'
#!/usr/bin/env bash
printf 'sleep %s\n' "$*" >> "${STUB_CTR_LOG:?}"
STUB
chmod +x "${STUBBIN}/sleep"

# Private toolbin: one symlink per tool the driver, the stubs and git need.
# Only the stub dir and this toolbin go on the driver's PATH.
TOOLBIN="${TMP}/toolbin"
mkdir -p "${TOOLBIN}"
for tool in git bash sed awk grep cut head tr date mktemp dirname basename ls mv rmdir mkdir rm tee cat env chmod cp touch find base64 sha256sum; do
  p="$(command -v "${tool}")" || { echo "required tool missing: ${tool}" >&2; exit 2; }
  ln -s "${p}" "${TOOLBIN}/${tool}"
done
if p="$(command -v python3)"; then
  ln -s "${p}" "${TOOLBIN}/python3"
fi
for d in stubbin-noscion stubbin-noctr stubbin-skopeo stubbin-race stubbin-gitfail; do
  mkdir -p "${TMP}/${d}"
done
cat > "${TMP}/stubbin-skopeo/skopeo" <<'STUB'
#!/usr/bin/env bash
printf 'skopeo %s\n' "$*" >> "${STUB_CTR_LOG:?}"
ref=""
for a in "$@"; do ref="${a}"; done
ref="${ref#docker://}"
[ "$1" = inspect ] || exit 0
# STUB_SKOPEO_DENY=<ref>: the lookup fails with a non-"not found" error.
if [ -n "${STUB_SKOPEO_DENY:-}" ] && [ "${ref}" = "${STUB_SKOPEO_DENY}" ]; then
  echo "FATA[0000] Error reading manifest: unauthorized: authentication required" >&2; exit 1
fi
# Tags made by the podman stub resolve to their source; a channel tag
# (:stable, :preview) that was never made or listed as remote: in
# STUB_PRESENT is absent, as a registry reports it.
target="$(awk -v r="${ref}" '$1 == r { v = $2 } END { print v }' "${STUB_ALIAS:?}")"
if [ -z "${target}" ]; then
  case "${ref}" in
    *:stable|*:preview)
      grep -qxF "remote:${ref}" "${STUB_PRESENT:?}" \
        || { echo "FATA[0000] Error parsing image name \"docker://${ref}\": reading manifest stable in x: manifest unknown" >&2; exit 1; } ;;
  esac
  target="${ref}"
fi
if [ -n "${STUB_HEX:-}" ]; then
  printf 'sha256:%s\n' "$(printf '%s' "${target}" | sha256sum | cut -c1-64)"
else
  printf 'sha256:sk-%s\n' "${target}"
fi
STUB
chmod +x "${TMP}/stubbin-skopeo/skopeo"
# mv that simulates another run creating the destination first.
cat > "${TMP}/stubbin-race/mv" <<'STUB'
#!/usr/bin/env bash
dst=""
for a in "$@"; do dst="${a}"; done
if [ -n "${STUB_RACE_DIR:-}" ] && [ "${dst}" = "${STUB_RACE_DIR}" ] && [ ! -e "${dst}" ]; then
  # The other run's directory is a usable tree: if the driver went on, it
  # would run this build-images.sh.
  mkdir -p "${dst}/image-build/scripts"
  echo other > "${dst}/other-run"
  printf '#!/usr/bin/env bash\necho OTHER-RUN-BUILD >> "${STUB_ARGV_LOG:?}"\necho "Steps:    core-base"\n' > "${dst}/image-build/scripts/build-images.sh"
  chmod +x "${dst}/image-build/scripts/build-images.sh"
  # A real git checkout, as another run's clone would be: a driver that went
  # on would get as far as running this build-images.sh.
  git -C "${dst}" init -q
  git -C "${dst}" add -A
  git -C "${dst}" -c user.name=other -c user.email=other@example.com commit -q -m "other run"
fi
if [ -n "${STUB_MV_FAIL_DST:-}" ] && [ "${dst}" = "${STUB_MV_FAIL_DST}" ]; then
  echo "mv: simulated failure" >&2
  exit 1
fi
exec "${STUB_REAL_MV:?}" "$@"
STUB
chmod +x "${TMP}/stubbin-race/mv"
# rmdir that simulates another run filling the empty --src-dir first.
cat > "${TMP}/stubbin-race/rmdir" <<'STUB'
#!/usr/bin/env bash
dst=""
for a in "$@"; do dst="${a}"; done
if [ -n "${STUB_RACE_RMDIR_DIR:-}" ] && [ "${dst}" = "${STUB_RACE_RMDIR_DIR}" ]; then
  echo other > "${dst}/other-run"
fi
exec "${STUB_REAL_RMDIR:?}" "$@"
STUB
chmod +x "${TMP}/stubbin-race/rmdir"
# git whose `status` fails (e.g. an unreadable index).
cat > "${TMP}/stubbin-gitfail/git" <<'STUB'
#!/usr/bin/env bash
for a in "$@"; do
  if [ "${a}" = status ]; then echo "fatal: simulated status failure" >&2; exit 128; fi
done
exec "${STUB_REAL_GIT:?}" "$@"
STUB
chmod +x "${TMP}/stubbin-gitfail/git"
cp "${STUBBIN}/docker" "${STUBBIN}/podman" "${TMP}/stubbin-noscion/"
cp "${STUBBIN}/scion" "${TMP}/stubbin-noctr/"
# mktemp that fails for the channel recovery file only.
mkdir -p "${TMP}/stubbin-mktempfail"
cat > "${TMP}/stubbin-mktempfail/mktemp" <<'STUB'
#!/usr/bin/env bash
for a in "$@"; do
  case "${a}" in *channel-recovery.*) echo "mktemp: simulated failure" >&2; exit 1 ;; esac
done
exec "${STUB_REAL_MKTEMP:?}" "$@"
STUB
chmod +x "${TMP}/stubbin-mktempfail/mktemp"
# Without gcloud, and a toolbin without base64 (--channel-tag with cloud-build).
mkdir -p "${TMP}/stubbin-nogcloud" "${TMP}/toolbin-nob64"
cp "${STUBBIN}/docker" "${STUBBIN}/podman" "${STUBBIN}/scion" "${STUBBIN}/sleep" "${TMP}/stubbin-nogcloud/"
for f in "${TOOLBIN}"/*; do
  [ "$(basename "${f}")" = base64 ] || cp -P "${f}" "${TMP}/toolbin-nob64/"
done

RUN_N=0
OUT=""
RC=0
CACHE=""
# run_driver [--no-scion|--no-ctr] <driver args...>: fresh logs; sets OUT
# and RC. RUN_ENV (NAME=value words) is added to the driver's environment.
# The cache dir is shared across runs unless reset_cache is called.
RUN_ENV=()
reset_cache() { RUN_N=$((RUN_N + 1)); CACHE="${TMP}/cache-${RUN_N}"; }
# driver_env <path> <driver args...>: run the driver in its isolated environment.
driver_env() {
  local path="$1"
  shift
  cd "${RUN_CWD:-${TMP}}" && env -i PATH="${path}" HOME="${TMP}/home" XDG_CACHE_HOME="${CACHE}" TMPDIR="${TMP}" \
    GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 \
    STUB_ARGV_LOG="${TMP}/argv.log" STUB_STAMP_LOG="${TMP}/stamp.log" \
    STUB_CTR_LOG="${TMP}/ctr.log" STUB_PRESENT="${TMP}/present" STUB_ENV_LOG="${TMP}/env.log" \
    STUB_SCION_JSON="${SCION_JSON:-}" STUB_ALIAS="${TMP}/alias.log" STUB_AR="${TMP}/ar" STUB_AR_TAGGED="${TMP}/artagged" \
    STUB_CB_IMAGES="${TMP}/cbimages" STUB_CB_STEPOUT="${TMP}/cbstepout" ${RUN_ENV[@]+"${RUN_ENV[@]}"} \
    bash "${DRIVER}" "$@"
}
run_driver() {
  local path="${STUBBIN}:${TOOLBIN}"
  case "${1:-}" in
    --no-scion) shift; path="${TMP}/stubbin-noscion:${TOOLBIN}" ;;
    --no-ctr) shift; path="${TMP}/stubbin-noctr:${TOOLBIN}" ;;
    --with-skopeo) shift; path="${TMP}/stubbin-skopeo:${STUBBIN}:${TOOLBIN}" ;;
    --race) shift; path="${TMP}/stubbin-race:${STUBBIN}:${TOOLBIN}" ;;
    --git-status-fails) shift; path="${TMP}/stubbin-gitfail:${STUBBIN}:${TOOLBIN}" ;;
    --no-gcloud) shift; path="${TMP}/stubbin-nogcloud:${TOOLBIN}" ;;
    --mktemp-fails) shift; path="${TMP}/stubbin-mktempfail:${STUBBIN}:${TOOLBIN}" ;;
    --no-base64) shift; path="${STUBBIN}:${TMP}/toolbin-nob64" ;;
  esac
  : > "${TMP}/argv.log"
  : > "${TMP}/stamp.log"
  : > "${TMP}/ctr.log"
  : > "${TMP}/env.log"
  : > "${TMP}/alias.log"
  : > "${TMP}/artagged"
  touch "${TMP}/ar" "${TMP}/cbimages" "${TMP}/cbstepout"
  touch "${TMP}/present"
  set +e
  # RUN_STDERR_FILE=<file>: the driver's stderr goes there instead of OUT.
  if [ -n "${RUN_STDERR_FILE:-}" ]; then
    OUT="$(driver_env "${path}" "$@" 2>"${RUN_STDERR_FILE}")"
  else
    OUT="$(driver_env "${path}" "$@" 2>&1)"
  fi
  RC=$?
  set -e
}
argv() { cat "${TMP}/argv.log"; }
stamp() { cat "${TMP}/stamp.log"; }
ctr() { cat "${TMP}/ctr.log"; }
present() { printf '%s\n' "$@" > "${TMP}/present"; }

S1="$(printf '%s' "${C1}" | cut -c1-7)"

# ---------------------------------------------------------------------------
# Negative control: the harness must detect a wrong expectation.
# ---------------------------------------------------------------------------

before="${FAILS}"
check_eq "negative control (deliberately wrong expectation)" "--target all --tag v1.0.0" "--target all --tag WRONG" 2>/dev/null
if [ "${FAILS}" -eq $((before + 1)) ]; then
  FAILS="${before}"
  pass "negative control was detected by the harness"
else
  fail "negative control was NOT detected: the assertions are broken"
fi

# ---------------------------------------------------------------------------
# Default build of a tag; two tags on one commit; manifest shape
# ---------------------------------------------------------------------------

reset_cache
present local:core-base:v1.0.0 local:scion-base:v1.0.0 local:scion-claude:v1.0.0 local:scion-hub:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}"
check_rc "default: exit 0" 0 "${RC}"
check_eq "default: golden argv" "--target all --tag v1.0.0" "$(argv)"
check_eq "default: requested tag wins over a second tag on the same commit" "v1.0.0" "$(stamp)"
SRC="${CACHE}/scion/image-src/v1.0.0"
check_eq "default: clone in the XDG cache dir" "${C1}" "$(git -C "${SRC}" rev-parse HEAD 2>/dev/null || true)"
check_eq "default: only the requested tag is in the clone" "v1.0.0" "$(git -C "${SRC}" tag 2>/dev/null || true)"
check_contains "default: summary has the source commit" "@ ${C1}" "${OUT}"
check_contains "default: summary has local image IDs" "scion-claude:v1.0.0  sha256:id-scion-claude:v1.0.0" "${OUT}"
want_manifest="$(cat <<EOF
{
  "version": "v1.0.0",
  "commit": "${C1}",
  "built_at": "<T>",
  "builder": "local-docker",
  "pushed": false,
  "submitted": false,
  "version_stamped": true,
  "images": {
    "core-base": {"ref": "core-base:v1.0.0", "digest": "sha256:id-core-base:v1.0.0"},
    "scion-base": {"ref": "scion-base:v1.0.0", "digest": "sha256:id-scion-base:v1.0.0"},
    "scion-claude": {"ref": "scion-claude:v1.0.0", "digest": "sha256:id-scion-claude:v1.0.0"},
    "scion-hub": {"ref": "scion-hub:v1.0.0", "digest": "sha256:id-scion-hub:v1.0.0"}
  }
}
EOF
)"
got_manifest="$(sed 's/"built_at": "[0-9]\{4\}-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]Z"/"built_at": "<T>"/' "${SRC}/build-manifest.json" 2>/dev/null || true)"
check_eq "default: build-manifest.json shape" "${want_manifest}" "${got_manifest}"
if command -v python3 >/dev/null 2>&1; then
  if python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); assert set(d)=={"version","commit","built_at","builder","pushed","submitted","version_stamped","images"}; assert all(set(v)=={"ref","digest"} for v in d["images"].values())' "${SRC}/build-manifest.json" 2>/dev/null; then
    pass "default: build-manifest.json parses as JSON with the expected keys"
  else
    fail "default: build-manifest.json is not valid JSON with the expected keys"
  fi
fi

# The other tag on that commit stamps its own name.
reset_cache
run_driver nightly-20200101 --repo "${REPO_URL}"
check_rc "second tag on the commit: exit 0" 0 "${RC}"
check_eq "second tag on the commit: stamps its own name" "nightly-20200101" "$(stamp)"

# ---------------------------------------------------------------------------
# Clone reuse
# ---------------------------------------------------------------------------

reset_cache
run_driver v1.0.0 --repo "${REPO_URL}"
SRC="${CACHE}/scion/image-src/v1.0.0"
echo marker > "${SRC}/.git/reuse-marker" 2>/dev/null || fail "reuse: first run left no clone to reuse"
run_driver v1.0.0 --repo "${REPO_URL}"
check_rc "reuse: second run exit 0" 0 "${RC}"
check_contains "reuse: second run reuses the clone" "reusing existing clone" "${OUT}"
check_not_contains "reuse: second run does not clone" "cloning" "${OUT}"
check_eq "reuse: clone dir kept (no re-clone)" "marker" "$(cat "${SRC}/.git/reuse-marker" 2>/dev/null || true)"
check_eq "reuse: golden argv" "--target all --tag v1.0.0" "$(argv)"

# Reuse re-checks describe, not just HEAD: an extra tag on HEAD blocks it.
git -C "${SRC}" tag a-local-tag 2>/dev/null || fail "reuse: could not tag the clone"
run_driver v1.0.0 --repo "${REPO_URL}"
check_rc "reuse with another tag on HEAD: exit 1" 1 "${RC}"
check_contains "reuse with another tag on HEAD: describe message" "cannot be reused" "${OUT}"
check_eq "reuse with another tag on HEAD: build not started" "" "$(argv)"
check_eq "reuse with another tag on HEAD: nothing deleted" "marker" "$(cat "${SRC}/.git/reuse-marker" 2>/dev/null || true)"

# A non-empty, non-git --src-dir is never reused or deleted.
reset_cache
mkdir -p "${TMP}/userdir"
echo keep > "${TMP}/userdir/file"
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${TMP}/userdir"
check_rc "non-git --src-dir: exit 1" 1 "${RC}"
check_eq "non-git --src-dir: contents untouched" "keep" "$(cat "${TMP}/userdir/file" 2>/dev/null || true)"
check_eq "non-git --src-dir: build not started" "" "$(argv)"

# A clone of another tag in --src-dir is not reused.
run_driver v1.1.0 --repo "${REPO_URL}" --src-dir "${SRC}"
check_rc "--src-dir at another commit: exit 1" 1 "${RC}"
check_contains "--src-dir at another commit: HEAD message" "expected ${C2}" "${OUT}"

# An empty --src-dir is used.
reset_cache
mkdir -p "${TMP}/emptydir"
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${TMP}/emptydir"
check_rc "empty --src-dir: exit 0" 0 "${RC}"
check_eq "empty --src-dir: cloned into it" "${C1}" "$(git -C "${TMP}/emptydir" rev-parse HEAD 2>/dev/null || true)"

# ---------------------------------------------------------------------------
# Base pre-flight
# ---------------------------------------------------------------------------

reset_cache
present local:core-base:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" -- --target common
check_rc "common, base present: exit 0" 0 "${RC}"
check_eq "common, base present: golden argv" "--target common --tag v1.0.0" "$(argv)"
check_contains "common, base present: core-base inspected" "docker image inspect core-base:v1.0.0" "$(ctr)"

present
run_driver v1.0.0 --repo "${REPO_URL}" -- --target common
check_rc "common, base absent: exit 1" 1 "${RC}"
check_contains "common, base absent: message" "stacks on core-base:v1.0.0" "${OUT}"
check_eq "common, base absent: stub never ran" "" "$(argv)"

present local:core-base:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" -- --target scion-claude
check_rc "single harness without scion-base: exit 1" 1 "${RC}"
check_contains "single harness without scion-base: message" "stacks on scion-base:v1.0.0" "${OUT}"
check_eq "single harness without scion-base: stub never ran" "" "$(argv)"

present local:scion-base:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" -- --target scion-claude
check_rc "single harness with scion-base: exit 0" 0 "${RC}"
check_eq "single harness with scion-base: golden argv" "--target scion-claude --tag v1.0.0" "$(argv)"

present local:scion-base:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" -- --target hub --builder local-podman
check_rc "podman: exit 0" 0 "${RC}"
check_contains "podman: podman used for the inspect" "podman image inspect scion-base:v1.0.0" "$(ctr)"
check_not_contains "podman: docker not used" "docker " "$(ctr)"
check_eq "podman: golden argv" "--target hub --tag v1.0.0 --builder local-podman" "$(argv)"

# ---------------------------------------------------------------------------
# Passthrough
# ---------------------------------------------------------------------------

present remote:reg.example/x/scion-base:v1.0.0 remote:reg.example/x/scion-claude:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" -- --registry reg.example/x --target scion-claude --platform linux/amd64 --push --builder local-docker
check_rc "passthrough: exit 0" 0 "${RC}"
check_eq "passthrough: golden argv" "--target scion-claude --tag v1.0.0 --registry reg.example/x --platform linux/amd64 --push --builder local-docker" "$(argv)"
check_contains "passthrough: base found in the registry" "docker buildx imagetools inspect reg.example/x/scion-base:v1.0.0" "$(ctr)"
check_contains "passthrough: pushed images report registry digests" "reg.example/x/scion-claude:v1.0.0  sha256:dg-reg.example/x/scion-claude:v1.0.0" "${OUT}"

run_driver v1.0.0 --repo "${REPO_URL}" -- --dry-run
check_rc "passthrough --dry-run: exit 0" 0 "${RC}"
check_eq "passthrough --dry-run: golden argv" "--target all --tag v1.0.0 --dry-run" "$(argv)"

for bad in --continue-on-error "--tag x" --target=all --allow-stale-base; do
  # shellcheck disable=SC2086 # split "--tag x" into two words on purpose
  run_driver v1.0.0 --repo "${REPO_URL}" -- ${bad}
  check_rc "passthrough ${bad}: rejected (rc 2)" 2 "${RC}"
  check_eq "passthrough ${bad}: stub never ran" "" "$(argv)"
done

# ---------------------------------------------------------------------------
# --ref
# ---------------------------------------------------------------------------

reset_cache
run_driver --ref "${C1}" --repo "${REPO_URL}"
check_rc "--ref sha: exit 0" 0 "${RC}"
check_eq "--ref sha: golden argv (short-SHA tag)" "--target all --tag ${S1}" "$(argv)"
check_eq "--ref sha: no tag fetched although the commit is tagged (binaries report dev)" "<none>" "$(stamp)"
check_eq "--ref sha: describe fails in the checkout" "fail" "$(git -C "${CACHE}/scion/image-src/ref-$(printf '%s' "${C1}" | cut -c1-12)" describe --tags --exact-match >/dev/null 2>&1 && echo ok || echo fail)"
check_contains "--ref sha: manifest version is dev" '"version": "dev"' "$(cat "${CACHE}/scion/image-src/ref-$(printf '%s' "${C1}" | cut -c1-12)/build-manifest.json" 2>/dev/null || true)"

# A --ref checkout that has gained a tag is not reused (its build would stamp it).
git -C "${CACHE}/scion/image-src/ref-$(printf '%s' "${C1}" | cut -c1-12)" tag v7.7.7 2>/dev/null || fail "--ref reuse: no checkout to tag"
run_driver --ref "${C1}" --repo "${REPO_URL}"
check_rc "--ref reuse with a tag in the checkout: exit 1" 1 "${RC}"
check_contains "--ref reuse with a tag in the checkout: message" "carries tag 'v7.7.7'" "${OUT}"
check_eq "--ref reuse with a tag in the checkout: build not started" "" "$(argv)"

run_driver --ref main --repo "${REPO_URL}"
check_rc "--ref branch: exit 0" 0 "${RC}"
check_eq "--ref branch: golden argv" "--target all --tag $(printf '%s' "${C4}" | cut -c1-7)" "$(argv)"
check_eq "--ref branch: tag on the commit not fetched" "<none>" "$(stamp)"

run_driver --ref no-such-branch --repo "${REPO_URL}"
check_rc "--ref unknown: exit 1" 1 "${RC}"

run_driver v1.0.0 --ref main --repo "${REPO_URL}"
check_rc "version and --ref together: rc 2" 2 "${RC}"

# ---------------------------------------------------------------------------
# Version resolution and its errors
# ---------------------------------------------------------------------------

reset_cache
run_driver v9.9.9 --repo "${REPO_URL}"
check_rc "missing tag: exit 1" 1 "${RC}"
check_contains "missing tag: message" "tag not found" "${OUT}"
check_eq "missing tag: stub never ran" "" "$(argv)"

SCION_JSON='{
  "buildTime": "2026-01-01T00:00:00Z",
  "commit": "abc",
  "short": "v1.1.0",
  "version": "v1.1.0"
}'
run_driver --repo "${REPO_URL}"
check_rc "scion version: exit 0" 0 "${RC}"
check_eq "scion version: golden argv" "--target all --tag v1.1.0" "$(argv)"
check_eq "scion version: annotated tag stamps its name" "v1.1.0" "$(stamp)"

SCION_JSON='{"commit": "abc", "short": "dev", "version": "dev"}'
run_driver --repo "${REPO_URL}"
check_rc "dev version: exit 1" 1 "${RC}"
check_contains "dev version: actionable message" "reports version 'dev'" "${OUT}"
check_eq "dev version: stub never ran" "" "$(argv)"

SCION_JSON='{"commit": "abc"}'
run_driver --repo "${REPO_URL}"
check_rc "missing version field: exit 1" 1 "${RC}"
check_contains "missing version field: message" "<missing>" "${OUT}"
SCION_JSON=""

run_driver --no-scion --repo "${REPO_URL}"
check_rc "scion not on PATH: exit 1" 1 "${RC}"
check_contains "scion not on PATH: actionable message" "'scion' is not on PATH" "${OUT}"
check_eq "scion not on PATH: stub never ran" "" "$(argv)"

# ---------------------------------------------------------------------------
# --also-tag latest, --force-latest, --channel-tag
# ---------------------------------------------------------------------------

reset_cache
run_driver v1.0.0 --repo "${REPO_URL}" --also-tag latest -- --registry reg.example/x --push
check_rc "latest + registry without --force-latest: exit 1" 1 "${RC}"
check_contains "latest + registry without --force-latest: message" "--force-latest" "${OUT}"
check_eq "latest + registry without --force-latest: stub never ran" "" "$(argv)"
check_eq "latest + registry without --force-latest: nothing cloned" "absent" "$([ -e "${CACHE}/scion/image-src/v1.0.0" ] && echo present || echo absent)"

run_driver v1.0.0 --repo "${REPO_URL}" --also-tag latest --force-latest -- --registry reg.example/x --push
check_rc "latest + registry + --force-latest: exit 0" 0 "${RC}"
check_contains "latest + registry + --force-latest: imagetools create" "docker buildx imagetools create -t reg.example/x/scion-claude:latest reg.example/x/scion-claude:v1.0.0" "$(ctr)"

run_driver v1.0.0 --repo "${REPO_URL}" --also-tag latest
check_rc "latest local: exit 0" 0 "${RC}"
check_contains "latest local: docker tag" "docker tag core-base:v1.0.0 core-base:latest" "$(ctr)"
check_contains "latest local: summary lists :latest" "scion-hub:latest  (also tagged)" "${OUT}"

run_driver v1.0.0 --repo "${REPO_URL}"
check_not_contains "no --also-tag: :latest never touched" ":latest" "$(ctr)"

run_driver v1.0.0 --repo "${REPO_URL}" --also-tag edge
check_rc "--also-tag other than latest: rc 2" 2 "${RC}"
run_driver v1.0.0 --repo "${REPO_URL}" --force-latest
check_rc "--force-latest alone: rc 2" 2 "${RC}"

run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag nightly
check_rc "--channel-tag invalid: rc 2" 2 "${RC}"

# ---------------------------------------------------------------------------
# --channel-tag: refusals (all before the clone and the build)
# ---------------------------------------------------------------------------

# refused <name> <want-rc> <message> <driver args...>
refused() {
  local name="$1" want="$2" msg="$3"
  shift 3
  reset_cache
  run_driver "$@"
  check_rc "${name}: rc ${want}" "${want}" "${RC}"
  check_contains "${name}: message" "${msg}" "${OUT}"
  check_eq "${name}: stub never ran" "" "$(argv)"
  check_eq "${name}: nothing cloned" "absent" "$([ -e "${CACHE}/scion" ] && echo present || echo absent)"
  check_not_contains "${name}: no tag moved" " tag" "$(ctr)"
}
PUSH_X=(-- --registry reg.example/x --push)
refused "channel without a registry" 2 "needs a registry" v1.0.0 --repo "${REPO_URL}" --channel-tag stable
refused "channel with a registry but a local build" 2 "needs pushed images" v1.0.0 --repo "${REPO_URL}" --channel-tag stable -- --registry reg.example/x
refused "channel preview on a stable tag" 2 "--channel-tag preview needs a preview-channel tag" v1.0.0 --repo "${REPO_URL}" --channel-tag preview "${PUSH_X[@]}"
refused "channel stable on a preview tag" 2 "--channel-tag stable needs a stable release tag" v1.0.0-preview.1 --repo "${REPO_URL}" --channel-tag stable "${PUSH_X[@]}"
refused "channel stable on an rc tag" 2 "--channel-tag stable needs a stable release tag" v1.0.0-rc.1 --repo "${REPO_URL}" --channel-tag stable "${PUSH_X[@]}"
# Near-miss rc and preview forms are refused for both channels.
for near in v1.0.0-rc v1.0.0-rc.01 v1.0.0-rc.1x v1.0.0-rc.1-1 v1.0.0-RC.1 v1.0.0-preview.01; do
  refused "channel preview on near-miss ${near}" 2 "--channel-tag preview needs a preview-channel tag" "${near}" --repo "${REPO_URL}" --channel-tag preview "${PUSH_X[@]}"
  refused "channel stable on near-miss ${near}" 2 "--channel-tag stable needs a stable release tag" "${near}" --repo "${REPO_URL}" --channel-tag stable "${PUSH_X[@]}"
done
refused "channel stable on a nightly tag" 2 "nightly builds never get a channel tag" nightly-20200101 --repo "${REPO_URL}" --channel-tag stable "${PUSH_X[@]}"
refused "channel preview on a nightly tag" 2 "nightly builds never get a channel tag" nightly-20200101 --repo "${REPO_URL}" --channel-tag preview "${PUSH_X[@]}"
refused "channel on a --ref build" 2 "a --ref build has none" --ref main --repo "${REPO_URL}" --channel-tag stable "${PUSH_X[@]}"
refused "channel with --also-tag latest" 2 "does not combine with --also-tag latest" v1.0.0 --repo "${REPO_URL}" --channel-tag stable --also-tag latest --force-latest "${PUSH_X[@]}"
refused "channel with --also-tag latest, no --force-latest" 2 "does not combine with --also-tag latest" v1.0.0 --repo "${REPO_URL}" --channel-tag stable --also-tag latest "${PUSH_X[@]}"
# shellcheck disable=SC2016 # literal test values, nothing expands
for badreg in 'reg.example/x;y' 'reg.example/x y' 'reg.example/x$(id)' 'reg.example' 'reg.example/X'; do
  refused "channel on a registry path outside the grammar (${badreg})" 2 "is not a plain <host>[:<port>]/<path>" v1.0.0 --repo "${REPO_URL}" --channel-tag stable -- --registry "${badreg}" --push
done
# A valid Artifact Registry path with a double hyphen passes the grammar.
reset_cache
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable -- --builder cloud-build --registry us-docker.pkg.dev/my--proj/repo --dry-run
check_rc "channel on a double-hyphen AR path (cloud-build dry run): exit 0" 0 "${RC}"
check_not_contains "channel on a double-hyphen AR path (cloud-build dry run): not refused" "is not a plain" "${OUT}"
refused "timeouts without --channel-tag" 2 "only apply to --channel-tag with --builder cloud-build" v1.0.0 --repo "${REPO_URL}" --build-timeout 60 -- --builder cloud-build --registry us-central1-docker.pkg.dev/proj-x/repo
refused "timeouts on a local channel build" 2 "only apply to --channel-tag with --builder cloud-build" v1.0.0 --repo "${REPO_URL}" --channel-tag stable --wait-timeout 60 "${PUSH_X[@]}"
refused "timeout not a number" 2 "number of seconds" v1.0.0 --repo "${REPO_URL}" --channel-tag stable --build-timeout 1h -- --builder cloud-build --registry us-central1-docker.pkg.dev/proj-x/repo
for bad in reg.example/x ghcr.io/org/repo docker.pkg.dev/proj-x/repo us-central1-docker.pkg.dev.evil.example/proj-x/repo evil.example/us-central1-docker.pkg.dev/proj-x/repo us-central1-docker.pkg.dev/proj-x \
  evil-us-central1-docker.pkg.dev/proj-x/repo attacker-docker.pkg.dev/proj-x/repo us-central1-evil-docker.pkg.dev/proj-x/repo \
  user@us-central1-docker.pkg.dev/proj-x/repo us-central1-docker.pkg.dev:443/proj-x/repo US-CENTRAL1-docker.pkg.dev/proj-x/repo us-central1-docker.pkg.dev/Proj-X/repo; do
  refused "cloud-build channel on a non-Artifact Registry path (${bad})" 2 "needs an Artifact Registry path" v1.0.0 --repo "${REPO_URL}" --channel-tag stable -- --builder cloud-build --registry "${bad}"
done

# ---------------------------------------------------------------------------
# --channel-tag on a pushed local build (docker, podman)
# ---------------------------------------------------------------------------

# hexd <ref>: the digest the stubs give <ref> with STUB_HEX set.
hexd() { printf 'sha256:%s' "$(printf '%s' "$1" | sha256sum | cut -c1-64)"; }
IMGS4=(core-base scion-base scion-claude scion-hub)
present_remote() {  # present_remote <registry> <tag>
  local n refs=()
  for n in "${IMGS4[@]}"; do refs+=("remote:$1/${n}:$2"); done
  present "${refs[@]}"
}

reset_cache
present_remote reg.example/x v1.0.0
RUN_ENV=(STUB_HEX=1)
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${PUSH_X[@]}"
RUN_ENV=()
check_rc "channel stable, docker push: exit 0" 0 "${RC}"
check_eq "channel stable, docker push: golden build argv" "--target all --tag v1.0.0 --registry reg.example/x --push" "$(argv)"
want=""
for n in "${IMGS4[@]}"; do
  want="${want}docker buildx imagetools create -t reg.example/x/${n}:stable reg.example/x/${n}@$(hexd "reg.example/x/${n}:v1.0.0")"$'\n'
done
check_eq "channel stable, docker push: golden retag argv (to the :v1.0.0 digests)" "${want%$'\n'}" "$(ctr | grep 'imagetools create' || true)"
check_not_contains "channel stable, docker push: :latest never touched" ":latest" "$(ctr)"
check_contains "channel stable, docker push: summary lists the channel tag" "reg.example/x/scion-hub:stable  (channel tag)" "${OUT}"
M="$(cat "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" 2>/dev/null || true)"
D="$(hexd reg.example/x/scion-claude:v1.0.0)"
check_contains "channel stable, docker push: manifest records digest, tag digest and channel ref" "\"scion-claude\": {\"ref\": \"reg.example/x/scion-claude:v1.0.0\", \"digest\": \"${D}\", \"tag_digest\": \"${D}\", \"channel_ref\": \"reg.example/x/scion-claude:stable\"}" "${M}"
if command -v python3 >/dev/null 2>&1; then
  if python3 -I -c 'import json,sys; d=json.load(open(sys.argv[1])); assert all(set(v)=={"ref","digest","tag_digest","channel_ref"} for v in d["images"].values())' "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" 2>/dev/null; then
    pass "channel stable, docker push: manifest parses as JSON with the channel keys"
  else
    fail "channel stable, docker push: manifest is not valid JSON with the channel keys"
  fi
fi

reset_cache
present_remote reg.example/x v1.0.0-preview.1
RUN_ENV=(STUB_HEX=1)
run_driver v1.0.0-preview.1 --repo "${REPO_URL}" --channel-tag preview -- --registry reg.example/x --platform all
RUN_ENV=()
check_rc "channel preview, multi-platform: exit 0" 0 "${RC}"
check_eq "channel preview, multi-platform: golden build argv" "--target all --tag v1.0.0-preview.1 --registry reg.example/x --platform all" "$(argv)"
check_contains "channel preview, multi-platform: retag to the :v1.0.0-preview.1 digest" "docker buildx imagetools create -t reg.example/x/scion-claude:preview reg.example/x/scion-claude@$(hexd reg.example/x/scion-claude:v1.0.0-preview.1)" "$(ctr)"
check_eq "channel preview, multi-platform: four channel tags" "4" "$(ctr | grep -c 'imagetools create' || true)"
check_not_contains "channel preview, multi-platform: :latest never touched" ":latest" "$(ctr)"

# rc releases are on the preview channel: --channel-tag preview accepts them.
reset_cache
present_remote reg.example/x v1.0.0-rc.1
RUN_ENV=(STUB_HEX=1)
run_driver v1.0.0-rc.1 --repo "${REPO_URL}" --channel-tag preview "${PUSH_X[@]}"
RUN_ENV=()
check_rc "channel preview on an rc tag: exit 0" 0 "${RC}"
check_eq "channel preview on an rc tag: golden build argv" "--target all --tag v1.0.0-rc.1 --registry reg.example/x --push" "$(argv)"
check_contains "channel preview on an rc tag: retag to the :v1.0.0-rc.1 digest" "docker buildx imagetools create -t reg.example/x/scion-hub:preview reg.example/x/scion-hub@$(hexd reg.example/x/scion-hub:v1.0.0-rc.1)" "$(ctr)"
check_not_contains "channel preview on an rc tag: :latest never touched" ":latest" "$(ctr)"

reset_cache
present local:reg.example/x/scion-base:v1.0.0
RUN_ENV=(STUB_HEX=1)
run_driver --with-skopeo v1.0.0 --repo "${REPO_URL}" --channel-tag stable -- --target hub --builder local-podman --registry reg.example/x --push
RUN_ENV=()
check_rc "channel stable, podman push: exit 0" 0 "${RC}"
check_eq "channel stable, podman push: golden tag and push argv" "podman tag reg.example/x/scion-hub:v1.0.0 reg.example/x/scion-hub:stable
podman push reg.example/x/scion-hub:stable" "$(ctr | grep -E '^podman (tag|push [^-])' || true)"
check_not_contains "channel stable, podman push: :latest never touched" ":latest" "$(ctr)"

# A digest that is not sha256:<64 hex> stops the run before any tag moves.
reset_cache
present_remote reg.example/x v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${PUSH_X[@]}"
check_rc "channel, malformed registry digest: exit 1" 1 "${RC}"
check_contains "channel, malformed registry digest: message" "is not sha256:<64 hex>; no channel tag was moved" "${OUT}"
check_not_contains "channel, malformed registry digest: no retag" "imagetools create" "$(ctr)"

# An image name outside the grammar in the step list stops the run before
# any tag moves (recovery commands are built only from checked names).
reset_cache
present_remote reg.example/x v1.0.0
# The bad name resolves too, so without the check the run would retag it.
printf '%s\n' "remote:reg.example/x/scion-hub;touch:v1.0.0" >> "${TMP}/present"
RUN_ENV=(STUB_HEX=1 "STUB_BANNER=core-base scion-hub;touch")
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${PUSH_X[@]}"
RUN_ENV=()
check_rc "channel, image name outside the grammar: exit 1" 1 "${RC}"
check_contains "channel, image name outside the grammar: message" "unexpected image name 'scion-hub;touch' in the build-images.sh step list; no channel tag was moved" "${OUT}"
check_not_contains "channel, image name outside the grammar: no retag" "imagetools create" "$(ctr)"

# A valid Artifact Registry path with a double hyphen is retagged.
reset_cache
present_remote us-docker.pkg.dev/my--proj/repo v1.0.0
RUN_ENV=(STUB_HEX=1)
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable -- --registry us-docker.pkg.dev/my--proj/repo --push
RUN_ENV=()
check_rc "channel on a double-hyphen AR path (docker push): exit 0" 0 "${RC}"
check_contains "channel on a double-hyphen AR path (docker push): retagged" "docker buildx imagetools create -t us-docker.pkg.dev/my--proj/repo/scion-hub:stable" "$(ctr)"

# Dry run: nothing is tagged.
reset_cache
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable -- --registry reg.example/x --push --dry-run
check_rc "channel dry run: exit 0" 0 "${RC}"
check_not_contains "channel dry run: no retag" "imagetools create" "$(ctr)"

# ---------------------------------------------------------------------------
# --channel-tag with --builder cloud-build: wait, build results, tags add
# ---------------------------------------------------------------------------

AR=us-central1-docker.pkg.dev/proj-x/repo
CB_ID=0b1c2d3e-0000-4000-8000-000000000001
CB_ID2=0b1c2d3e-0000-4000-8000-000000000002
CB_X=(-- --builder cloud-build --registry "${AR}")
# bd <image>: the build-result digest the fixtures use for <image>.
bd() { printf 'sha256:%s' "$(printf 'build-%s' "$1" | sha256sum | cut -c1-64)"; }
# cb_fixture: results.images lists every image at :v1.0.0 (plus a
# :<short-sha> ref the driver must ignore), no step outputs, and the
# registry's :v1.0.0 tags point to the same digests.
cb_fixture() {
  local n
  : > "${TMP}/cbimages"; : > "${TMP}/cbstepout"; : > "${TMP}/ar"
  for n in "${IMGS4[@]}"; do
    printf '%s\t%s\n' "${AR}/${n}:v1.0.0" "$(bd "${n}")" >> "${TMP}/cbimages"
    printf '%s\t%s\n' "${AR}/${n}:${S1}" "$(bd "${n}")" >> "${TMP}/cbimages"
    printf '%s %s\n' "${AR}/${n}:v1.0.0" "$(bd "${n}")" >> "${TMP}/ar"
  done
}
# b64 <text>: one-line base64.
b64() { printf '%s' "$1" | base64 | tr -d '\n'; }
cb_no_tag() {  # cb_no_tag <name>: nothing was tagged
  check_not_contains "$1: no tags add" "tags add" "$(ctr)"
  check_not_contains "$1: :latest never touched" ":latest" "$(ctr)"
  check_eq "$1: no manifest" "absent" "$([ -e "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" ] && echo present || echo absent)"
}

# Success from results.images.
reset_cache
cb_fixture
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${CB_X[@]}"
check_rc "cloud-build channel (results.images): exit 0" 0 "${RC}"
check_eq "cloud-build channel (results.images): golden build argv" "--target all --tag v1.0.0 --builder cloud-build --registry ${AR}" "$(argv)"
check_contains "cloud-build channel (results.images): waits on the parsed build" "gcloud builds describe ${CB_ID} --project=stub-proj --region=global --format=value(status)" "$(ctr)"
want=""
for n in "${IMGS4[@]}"; do
  want="${want}gcloud artifacts docker tags add ${AR}/${n}@$(bd "${n}") ${AR}/${n}:stable"$'\n'
done
check_eq "cloud-build channel (results.images): golden tags add argv" "${want%$'\n'}" "$(ctr | grep 'tags add' || true)"
check_contains "cloud-build channel (results.images): :v1.0.0 checked against the build digest" "gcloud artifacts docker images describe ${AR}/scion-hub:v1.0.0 --format=value(image_summary.digest)" "$(ctr)"
check_not_contains "cloud-build channel (results.images): :latest never touched" ":latest" "$(ctr)"
check_not_contains "cloud-build channel (results.images): no docker call" "docker buildx" "$(ctr)"
M="$(cat "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" 2>/dev/null || true)"
check_contains "cloud-build channel (results.images): manifest not submitted" '"submitted": false,' "${M}"
check_contains "cloud-build channel (results.images): manifest pushed" '"pushed": true,' "${M}"
check_contains "cloud-build channel (results.images): manifest records the build digest and the channel tag" "\"scion-hub\": {\"ref\": \"${AR}/scion-hub:v1.0.0\", \"digest\": \"$(bd scion-hub)\", \"tag_digest\": \"$(bd scion-hub)\", \"channel_ref\": \"${AR}/scion-hub:stable\"}" "${M}"
check_not_contains "cloud-build channel (results.images): not reported as only submitted" "submitted to Cloud Build" "${OUT}"

# Success from results.buildStepOutputs (two builds, an empty step output).
reset_cache
cb_fixture
: > "${TMP}/cbimages"
printf '%s;;%s\n' \
  "$(b64 "${AR}/core-base:v1.0.0 $(bd core-base)
${AR}/scion-base:v1.0.0 $(bd scion-base)")" \
  "$(b64 "${AR}/scion-claude:v1.0.0 $(bd scion-claude)
${AR}/scion-hub:v1.0.0 $(bd scion-hub)
")" > "${TMP}/cbstepout.${CB_ID}"
printf ';\n' > "${TMP}/cbstepout.${CB_ID2}"
RUN_ENV=(STUB_CB_ID2="${CB_ID2}")
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${CB_X[@]}"
RUN_ENV=()
: > "${TMP}/cbstepout.${CB_ID}"; : > "${TMP}/cbstepout.${CB_ID2}"
check_rc "cloud-build channel (buildStepOutputs): exit 0" 0 "${RC}"
check_contains "cloud-build channel (buildStepOutputs): waits on the second build too" "gcloud builds describe ${CB_ID2} --project=stub-proj --region=global --format=value(status)" "$(ctr)"
check_contains "cloud-build channel (buildStepOutputs): tags the step-output digest" "gcloud artifacts docker tags add ${AR}/scion-claude@$(bd scion-claude) ${AR}/scion-claude:stable" "$(ctr)"
check_eq "cloud-build channel (buildStepOutputs): four channel tags" "4" "$(ctr | grep -c 'tags add' || true)"

# cb_stop <name> <message> [driver args after the version...]: the run stops
# with rc 1 and the message, and nothing is tagged.
cb_stop() {
  local name="$1" msg="$2"
  shift 2
  run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "$@" "${CB_X[@]}"
  check_rc "${name}: exit 1" 1 "${RC}"
  check_contains "${name}: message" "${msg}" "${OUT}"
  cb_no_tag "${name}"
}

# Today's release trees: the builds report no digest anywhere.
reset_cache; cb_fixture; : > "${TMP}/cbimages"
cb_stop "cloud-build channel, no build results" "no build-result digest for ${AR}/core-base:v1.0.0"

# One image missing from the results.
reset_cache; cb_fixture; grep -v "scion-hub:v1.0.0" "${TMP}/cbimages" > "${TMP}/cbimages.tmp"; cp "${TMP}/cbimages.tmp" "${TMP}/cbimages"
cb_stop "cloud-build channel, one image missing" "no build-result digest for ${AR}/scion-hub:v1.0.0"

# A malformed build ID in gcloud's output.
reset_cache; cb_fixture
RUN_ENV=(STUB_CB_ID="0b1c2d3e-0000-4000-8000-00000000000Z")
cb_stop "cloud-build channel, malformed build ID" "unexpected Cloud Build reference in the build output"
RUN_ENV=()
check_not_contains "cloud-build channel, malformed build ID: no describe call" "builds describe" "$(ctr)"
reset_cache; cb_fixture
RUN_ENV=(STUB_CB_ID="0b1c2d3e-0000-4000-8000-000000000001 --project=other")
cb_stop "cloud-build channel, build ID with extra words" "unexpected Cloud Build reference in the build output"
RUN_ENV=()
reset_cache; cb_fixture
RUN_ENV=(STUB_CB_ID="0b1c2d3e-0000-4000-8000-000000000001x")
cb_stop "cloud-build channel, build ID with trailing junk" "unexpected Cloud Build reference in the build output"
RUN_ENV=()

# Malformed digests in results.images: short, uppercase hex, one hex digit
# short of 64, one over.
D64="$(bd scion-claude)"
UP64="sha256:$(printf '%s' "${D64#sha256:}" | tr 'a-f' 'A-F')"
for bad in "sha256:ABC" "${UP64}" "${D64%?}" "${D64}0"; do
  reset_cache; cb_fixture
  sed "s|${D64}\$|${bad}|" "${TMP}/cbimages" > "${TMP}/cbimages.tmp"; cp "${TMP}/cbimages.tmp" "${TMP}/cbimages"
  cb_stop "cloud-build channel, malformed build-result digest (${#bad} chars)" "the build-result digest for ${AR}/scion-claude:v1.0.0 is not sha256:<64 hex>"
done
# The same in a step output line.
reset_cache; cb_fixture; : > "${TMP}/cbimages"
printf '%s\n' "$(b64 "${AR}/scion-hub:v1.0.0 ${UP64}")" > "${TMP}/cbstepout"
cb_stop "cloud-build channel, uppercase digest in a step output" "is not '<registry>/<image>:<tag> sha256:<64 hex>'"

# Step outputs: a malformed line, an image this run did not build, a
# duplicate line, and step outputs that disagree with results.images.
reset_cache; cb_fixture; : > "${TMP}/cbimages"
printf '%s\n' "$(b64 "${AR}/scion-hub:v1.0.0 sha256:xyz")" > "${TMP}/cbstepout"
cb_stop "cloud-build channel, malformed step output line" "is not '<registry>/<image>:<tag> sha256:<64 hex>'"
reset_cache; cb_fixture; : > "${TMP}/cbimages"
printf '%s\n' "$(b64 "${AR}/scion-other:v1.0.0 $(bd scion-other)")" > "${TMP}/cbstepout"
cb_stop "cloud-build channel, step output for another image" "names ${AR}/scion-other:v1.0.0, which this run did not build"
reset_cache; cb_fixture; : > "${TMP}/cbimages"
# The other three images are listed too, so without the duplicate check the
# run would reach the tag moves.
printf '%s;%s\n' "$(b64 "${AR}/core-base:v1.0.0 $(bd core-base)
${AR}/scion-base:v1.0.0 $(bd scion-base)
${AR}/scion-claude:v1.0.0 $(bd scion-claude)
${AR}/scion-hub:v1.0.0 $(bd scion-hub)")" "$(b64 "${AR}/scion-hub:v1.0.0 $(bd scion-hub)")" > "${TMP}/cbstepout"
cb_stop "cloud-build channel, duplicate step output line" "name ${AR}/scion-hub:v1.0.0 more than once"
reset_cache; cb_fixture
printf '%s\n' "$(b64 "${AR}/scion-hub:v1.0.0 $(bd other)")" > "${TMP}/cbstepout"
cb_stop "cloud-build channel, step output disagrees with results.images" "give different digests for ${AR}/scion-hub:v1.0.0"
reset_cache; cb_fixture
printf 'not*base64\n' > "${TMP}/cbstepout"
cb_stop "cloud-build channel, step output not base64" "is not base64"

# :v1.0.0 in the registry no longer points to the build-result digest.
reset_cache; cb_fixture
sed "s|^${AR}/scion-hub:v1.0.0 .*|${AR}/scion-hub:v1.0.0 $(bd moved)|" "${TMP}/ar" > "${TMP}/ar.tmp"; cp "${TMP}/ar.tmp" "${TMP}/ar"
cb_stop "cloud-build channel, :v1.0.0 differs from the build digest" "${AR}/scion-hub:v1.0.0 points to '$(bd moved)', not to the build-result digest $(bd scion-hub)"

# One of two builds fails.
reset_cache; cb_fixture
RUN_ENV=(STUB_CB_ID2="${CB_ID2}" STUB_CB_FAIL_ID="${CB_ID2}")
cb_stop "cloud-build channel, one build fails" "Cloud Build ${CB_ID2} ended with status 'FAILURE'"
RUN_ENV=()

# Timeouts: per build, and in total. The poll interval is 30s.
reset_cache; cb_fixture
RUN_ENV=(STUB_CB_STATUS=WORKING)
cb_stop "cloud-build channel, --build-timeout" "has not finished after 60s (--build-timeout)" --build-timeout 60
check_eq "cloud-build channel, --build-timeout: polled until the deadline" "2" "$(ctr | grep -c '^sleep 30$' || true)"
cb_stop "cloud-build channel, --wait-timeout" "passed 30s (--wait-timeout)" --wait-timeout 30
RUN_ENV=()

# The status lookup keeps failing.
reset_cache; cb_fixture
RUN_ENV=(STUB_CB_DESCRIBE_FAIL=1)
cb_stop "cloud-build channel, describe keeps failing" "failed 3 times in a row"
RUN_ENV=()

# No channel tag on a submit-only cloud-build run, and none in a dry run.
reset_cache; cb_fixture
run_driver v1.0.0 --repo "${REPO_URL}" "${CB_X[@]}"
check_rc "cloud-build without --channel-tag: exit 0" 0 "${RC}"
check_not_contains "cloud-build without --channel-tag: no gcloud call by the driver" "gcloud" "$(ctr)"
reset_cache; cb_fixture
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${CB_X[@]}" --dry-run
check_rc "cloud-build channel dry run: exit 0" 0 "${RC}"
check_not_contains "cloud-build channel dry run: no gcloud call by the driver" "gcloud" "$(ctr)"

# More cloud-build stops: a results.images conflict across two builds, other
# end states, step-output digests of the wrong length, CR and leading-space
# step lines, and gcloud or base64 missing.
reset_cache; cb_fixture
printf '%s\t%s\n' "${AR}/scion-hub:v1.0.0" "$(bd scion-hub)" > "${TMP}/cbimages.${CB_ID}"
printf '%s\t%s\n' "${AR}/scion-hub:v1.0.0" "$(bd other)" > "${TMP}/cbimages.${CB_ID2}"
RUN_ENV=(STUB_CB_ID2="${CB_ID2}")
cb_stop "cloud-build channel, results.images conflict across builds" "results.images report two different digests for ${AR}/scion-hub:v1.0.0"
RUN_ENV=()
: > "${TMP}/cbimages.${CB_ID}"; : > "${TMP}/cbimages.${CB_ID2}"
for st in CANCELLED TIMEOUT EXPIRED INTERNAL_ERROR; do
  reset_cache; cb_fixture
  RUN_ENV=(STUB_CB_STATUS="${st}")
  cb_stop "cloud-build channel, build ends ${st}" "Cloud Build ${CB_ID} ended with status '${st}'"
  RUN_ENV=()
done
D64="$(bd scion-hub)"
CR=$'\r'
for line in "${AR}/scion-hub:v1.0.0 ${D64%?}" "${AR}/scion-hub:v1.0.0 ${D64}0" "${AR}/scion-hub:v1.0.0 ${D64}${CR}" " ${AR}/scion-hub:v1.0.0 ${D64}"; do
  reset_cache; cb_fixture; : > "${TMP}/cbimages"
  printf '%s\n' "$(b64 "${line}")" > "${TMP}/cbstepout"
  cb_stop "cloud-build channel, step output line of ${#line} chars" "is not '<registry>/<image>:<tag> sha256:<64 hex>'"
done
reset_cache; cb_fixture
run_driver --no-gcloud v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${CB_X[@]}"
check_rc "cloud-build channel without gcloud: exit 1" 1 "${RC}"
check_contains "cloud-build channel without gcloud: message" "needs gcloud on PATH" "${OUT}"
check_eq "cloud-build channel without gcloud: stub never ran" "" "$(argv)"
reset_cache; cb_fixture
run_driver --no-base64 v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${CB_X[@]}"
check_rc "cloud-build channel without base64: exit 1" 1 "${RC}"
check_contains "cloud-build channel without base64: message" "needs base64 on PATH" "${OUT}"
check_eq "cloud-build channel without base64: stub never ran" "" "$(argv)"

# ---------------------------------------------------------------------------
# --channel-tag: the current channel digests are recorded first; a part-way
# failure stops, lists the moved tags and writes recovery commands
# ---------------------------------------------------------------------------

rfiles() { find "${CACHE}/scion/image-src/v1.0.0" -maxdepth 1 -name 'channel-recovery.*' 2>/dev/null; }
rfile_content() { local f; f="$(rfiles | head -n 1)"; [ -n "${f}" ] && cat "${f}"; }
RHEAD="# build-release-images.sh: channel tag recovery for :stable at v1.0.0
# The run stopped part-way. Each command points one channel tag back to
# the digest it had before the run. Nothing here has been run."
PREVD="$(bd previous-core-base)"
# cb_prev: core-base:stable exists (PREVD); the other channel tags do not.
cb_prev() { cb_fixture; printf '%s %s\n' "${AR}/core-base:stable" "${PREVD}" >> "${TMP}/ar"; }

# Success writes no recovery file.
reset_cache; cb_prev
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${CB_X[@]}"
check_rc "cloud-build channel over an existing :stable: exit 0" 0 "${RC}"
check_contains "cloud-build channel over an existing :stable: its digest was read first" "gcloud artifacts docker images describe ${AR}/core-base:stable --format=value(image_summary.digest)" "$(ctr)"
check_eq "cloud-build channel over an existing :stable: no recovery file" "" "$(rfiles)"

# (c) tags add fails at the third image.
reset_cache; cb_prev
RUN_ENV=(STUB_AR_TAGS_FAIL="${AR}/scion-claude:stable")
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${CB_X[@]}"
RUN_ENV=()
check_rc "cloud-build channel, tags add fails part-way: exit 1" 1 "${RC}"
check_contains "cloud-build channel, tags add fails part-way: message and moved list" "gcloud artifacts docker tags add ${AR}/scion-claude:stable failed (channel tags moved so far: ${AR}/core-base:stable ${AR}/scion-base:stable); recovery commands: " "${OUT}"
check_eq "cloud-build channel, tags add fails part-way: recovery file (a previous digest and none)" "${RHEAD}
gcloud artifacts docker tags add ${AR}/core-base@${PREVD} ${AR}/core-base:stable
gcloud artifacts docker tags delete ${AR}/scion-base:stable
gcloud artifacts docker tags delete ${AR}/scion-claude:stable" "$(rfile_content)"
check_eq "cloud-build channel, tags add fails part-way: the fourth image is not tagged" "0" "$(ctr | grep -c "tags add .*scion-hub" || true)"
check_not_contains "cloud-build channel, tags add fails part-way: recovery commands are not run" "tags delete" "$(ctr)"
check_eq "cloud-build channel, tags add fails part-way: no manifest" "absent" "$([ -e "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" ] && echo present || echo absent)"
# The same command again reuses the clone (the recovery file is expected).
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${CB_X[@]}"
check_rc "cloud-build channel, re-run after a part-way failure: exit 0" 0 "${RC}"
check_contains "cloud-build channel, re-run after a part-way failure: reuses the clone" "reusing existing clone" "${OUT}"

# (a) the read-back of the third image fails.
reset_cache; cb_prev
RUN_ENV=(STUB_AR_READBACK_FAIL="${AR}/scion-claude:stable")
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${CB_X[@]}"
RUN_ENV=()
check_rc "cloud-build channel, read-back absent: exit 1" 1 "${RC}"
check_contains "cloud-build channel, read-back absent: message and moved list" "${AR}/scion-claude:stable points to '<unavailable>', expected $(bd scion-claude) (channel tags moved so far: ${AR}/core-base:stable ${AR}/scion-base:stable ${AR}/scion-claude:stable); recovery commands: " "${OUT}"
check_eq "cloud-build channel, read-back absent: recovery file" "${RHEAD}
gcloud artifacts docker tags add ${AR}/core-base@${PREVD} ${AR}/core-base:stable
gcloud artifacts docker tags delete ${AR}/scion-base:stable
gcloud artifacts docker tags delete ${AR}/scion-claude:stable" "$(rfile_content)"

# (b) the read-back of the second image mismatches.
reset_cache; cb_prev
RUN_ENV=(STUB_AR_TAGS_WRONG="${AR}/scion-base:stable")
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${CB_X[@]}"
RUN_ENV=()
check_rc "cloud-build channel, read-back mismatch: exit 1" 1 "${RC}"
check_contains "cloud-build channel, read-back mismatch: message and moved list" "${AR}/scion-base:stable points to 'sha256:$(printf 'wrong' | sha256sum | cut -c1-64)', expected $(bd scion-base) (channel tags moved so far: ${AR}/core-base:stable ${AR}/scion-base:stable); recovery commands: " "${OUT}"
check_eq "cloud-build channel, read-back mismatch: recovery file" "${RHEAD}
gcloud artifacts docker tags add ${AR}/core-base@${PREVD} ${AR}/core-base:stable
gcloud artifacts docker tags delete ${AR}/scion-base:stable" "$(rfile_content)"

# (d) the :v1.0.0 lookup fails: a stop with nothing moved.
reset_cache; cb_prev
RUN_ENV=(STUB_AR_DESCRIBE_FAIL="${AR}/scion-hub:v1.0.0")
cb_stop "cloud-build channel, :v1.0.0 lookup fails" "${AR}/scion-hub:v1.0.0 points to '<unavailable>', not to the build-result digest $(bd scion-hub); no channel tag was moved"
RUN_ENV=()
check_eq "cloud-build channel, :v1.0.0 lookup fails: no recovery file" "" "$(rfiles)"

# The pre-read of a channel tag fails (not "not found"), or is malformed.
reset_cache; cb_prev
RUN_ENV=(STUB_AR_DESCRIBE_FAIL="${AR}/scion-hub:stable")
cb_stop "cloud-build channel, channel pre-read fails" "could not read the current digest of ${AR}/scion-hub:stable"
RUN_ENV=()
check_eq "cloud-build channel, channel pre-read fails: no recovery file" "" "$(rfiles)"
reset_cache; cb_prev
printf '%s %s\n' "${AR}/scion-hub:stable" "sha256:ABC" >> "${TMP}/ar"
cb_stop "cloud-build channel, channel pre-read malformed" "could not read the current digest of ${AR}/scion-hub:stable"

# Docker: imagetools create fails at the third image; core-base:stable exists.
reset_cache
present_remote reg.example/x v1.0.0
printf '%s\n' "remote:reg.example/x/core-base:stable" >> "${TMP}/present"
RUN_ENV=(STUB_HEX=1 STUB_CREATE_FAIL=reg.example/x/scion-claude:stable)
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${PUSH_X[@]}"
RUN_ENV=()
check_rc "channel docker, create fails part-way: exit 1" 1 "${RC}"
check_contains "channel docker, create fails part-way: message and moved list" "imagetools create reg.example/x/scion-claude:stable failed (channel tags moved so far: reg.example/x/core-base:stable reg.example/x/scion-base:stable); recovery commands: " "${OUT}"
check_eq "channel docker, create fails part-way: recovery file" "${RHEAD}
docker buildx imagetools create -t reg.example/x/core-base:stable reg.example/x/core-base@$(hexd reg.example/x/core-base:stable)
# reg.example/x/scion-base:stable did not exist before this run; remove that tag with the registry's own tools
# reg.example/x/scion-claude:stable did not exist before this run; remove that tag with the registry's own tools" "$(rfile_content)"
check_eq "channel docker, create fails part-way: no manifest" "absent" "$([ -e "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" ] && echo present || echo absent)"
# Docker: the pre-read is denied: a stop with nothing moved.
reset_cache
present_remote reg.example/x v1.0.0
RUN_ENV=(STUB_HEX=1 STUB_INSPECT_DENY=reg.example/x/scion-hub:stable)
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${PUSH_X[@]}"
RUN_ENV=()
check_rc "channel docker, pre-read denied: exit 1" 1 "${RC}"
check_contains "channel docker, pre-read denied: message" "could not read the current digest of reg.example/x/scion-hub:stable" "${OUT}"
check_not_contains "channel docker, pre-read denied: no retag" "imagetools create" "$(ctr)"

# Podman: the push of the channel tag fails (one image).
reset_cache
present local:reg.example/x/scion-base:v1.0.0
RUN_ENV=(STUB_HEX=1 STUB_PUSH_FAIL=reg.example/x/scion-hub:stable)
run_driver --with-skopeo v1.0.0 --repo "${REPO_URL}" --channel-tag stable -- --target hub --builder local-podman --registry reg.example/x --push
RUN_ENV=()
check_rc "channel podman, push fails: exit 1" 1 "${RC}"
check_contains "channel podman, push fails: message and moved list" "podman push reg.example/x/scion-hub:stable failed (channel tags moved so far: ); recovery commands: " "${OUT}"
check_eq "channel podman, push fails: recovery file" "${RHEAD}
# reg.example/x/scion-hub:stable did not exist before this run; remove that tag with the registry's own tools" "$(rfile_content)"
# Podman without skopeo is refused before the build.
reset_cache
present local:reg.example/x/scion-base:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" --channel-tag stable -- --target hub --builder local-podman --registry reg.example/x --push
check_rc "channel podman without skopeo: exit 1" 1 "${RC}"
check_contains "channel podman without skopeo: message" "needs skopeo on PATH" "${OUT}"
check_eq "channel podman without skopeo: stub never ran" "" "$(argv)"
# Podman: a pre-read failure that is not "not found" stops with nothing moved.
reset_cache
present local:reg.example/x/scion-base:v1.0.0
RUN_ENV=(STUB_HEX=1 STUB_SKOPEO_DENY=reg.example/x/scion-hub:stable)
run_driver --with-skopeo v1.0.0 --repo "${REPO_URL}" --channel-tag stable -- --target hub --builder local-podman --registry reg.example/x --push
RUN_ENV=()
check_rc "channel podman, pre-read denied: exit 1" 1 "${RC}"
check_contains "channel podman, pre-read denied: message" "could not read the current digest of reg.example/x/scion-hub:stable" "${OUT}"
check_not_contains "channel podman, pre-read denied: no channel tag" "podman tag reg.example/x/scion-hub:v1.0.0 reg.example/x/scion-hub:stable" "$(ctr)"
# Podman: :v1.0.0 is read with skopeo only, never by a push.
reset_cache
present local:reg.example/x/scion-base:v1.0.0
RUN_ENV=(STUB_HEX=1 STUB_SKOPEO_DENY=reg.example/x/scion-hub:v1.0.0)
run_driver --with-skopeo v1.0.0 --repo "${REPO_URL}" --channel-tag stable -- --target hub --builder local-podman --registry reg.example/x --push
RUN_ENV=()
check_rc "channel podman, :v1.0.0 unreadable by skopeo: exit 1" 1 "${RC}"
check_contains "channel podman, :v1.0.0 unreadable by skopeo: message" "no registry digest for reg.example/x/scion-hub:v1.0.0; no channel tag was moved" "${OUT}"
check_not_contains "channel podman, :v1.0.0 unreadable by skopeo: no push fallback" "push --digestfile" "$(ctr)"

# The recovery file cannot be created: the commands go to stderr instead.
reset_cache; cb_prev
RUN_ENV=(STUB_AR_TAGS_FAIL="${AR}/scion-claude:stable" STUB_REAL_MKTEMP="${TOOLBIN}/mktemp")
: > "${TMP}/stderr.log"
RUN_STDERR_FILE="${TMP}/stderr.log"
run_driver --mktemp-fails v1.0.0 --repo "${REPO_URL}" --channel-tag stable "${CB_X[@]}"
RUN_STDERR_FILE=""
RUN_ENV=()
ERR="$(cat "${TMP}/stderr.log")"
check_rc "channel, recovery file cannot be created: exit 1" 1 "${RC}"
check_contains "channel, recovery file cannot be created: the commands are on stderr" "no recovery file could be created in ${CACHE}/scion/image-src/v1.0.0:
gcloud artifacts docker tags add ${AR}/core-base@${PREVD} ${AR}/core-base:stable
gcloud artifacts docker tags delete ${AR}/scion-base:stable
gcloud artifacts docker tags delete ${AR}/scion-claude:stable" "${ERR}"
check_not_contains "channel, recovery file cannot be created: the commands are not on stdout" "gcloud artifacts docker tags delete" "${OUT}"
check_contains "channel, recovery file cannot be created: message and moved list on stderr" "gcloud artifacts docker tags add ${AR}/scion-claude:stable failed (channel tags moved so far: ${AR}/core-base:stable ${AR}/scion-base:stable); could not write a recovery file, the commands are above" "${ERR}"
check_eq "channel, recovery file cannot be created: no recovery file" "" "$(rfiles)"

run_driver --help
check_rc "--help: exit 0" 0 "${RC}"
check_contains "--help: reproducibility stance" "same recipe, not same bits" "${OUT}"

# ---------------------------------------------------------------------------
# Round 2: version stamping floor (F2)
# ---------------------------------------------------------------------------

reset_cache
run_driver v0.9.0 --repo "${REPO_URL}"
check_rc "unstamped release: exit 0" 0 "${RC}"
check_eq "unstamped release: golden argv" "--target all --tag v0.9.0" "$(argv)"
check_contains "unstamped release: warning" "predates version stamping" "${OUT}"
check_contains "unstamped release: summary warning" "WARNING: v0.9.0 predates version stamping" "${OUT}"
check_contains "unstamped release: manifest records it" '"version_stamped": false,' "$(cat "${CACHE}/scion/image-src/v0.9.0/build-manifest.json" 2>/dev/null || true)"
check_contains "unstamped release: manifest commit" "\"commit\": \"${C3}\"" "$(cat "${CACHE}/scion/image-src/v0.9.0/build-manifest.json" 2>/dev/null || true)"
run_driver v0.9.1 --repo "${REPO_URL}"
check_rc "release without VERSION derivation: exit 0" 0 "${RC}"
check_contains "release without VERSION derivation: warning" "predates version stamping" "${OUT}"
check_contains "release without VERSION derivation: manifest records it" '"version_stamped": false,' "$(cat "${CACHE}/scion/image-src/v0.9.1/build-manifest.json" 2>/dev/null || true)"
run_driver v1.0.0 --repo "${REPO_URL}"
check_not_contains "stamped release: no stamping warning" "predates version stamping" "${OUT}"

# A stamped tree whose hack/version.sh does not stamp the tag (a tag form
# outside the release rule) fails before the build instead of building
# images whose binaries report "dev".
for t in v1.2.0-beta.1 v1.2.0-preview.01; do
  reset_cache
  run_driver "${t}" --repo "${REPO_URL}"
  check_rc "non-release tag form ${t}: exit 1" 1 "${RC}"
  check_contains "non-release tag form ${t}: message" "would not stamp '${t}' into its binaries" "${OUT}"
  check_eq "non-release tag form ${t}: build not started" "" "$(argv)"
done

# ---------------------------------------------------------------------------
# Round 2: arguments that start with '-' (F3, F5)
# ---------------------------------------------------------------------------

reset_cache
run_driver v1.0.0 --repo "--upload-pack=touch ${TMP}/pwned"
check_rc "--repo starting with '-': rejected (rc 2)" 2 "${RC}"
check_eq "--repo starting with '-': no command ran" "absent" "$([ -e "${TMP}/pwned" ] && echo present || echo absent)"
run_driver --ref "--upload-pack=touch ${TMP}/pwned" --repo "${REPO_URL}"
check_rc "--ref starting with '-': rejected (rc 2)" 2 "${RC}"
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir -flat
check_rc "--src-dir starting with '-': rejected (rc 2)" 2 "${RC}"
run_driver -v1.0.0 --repo "${REPO_URL}"
check_rc "version starting with '-': rejected (rc 2)" 2 "${RC}"
check_eq "'-' arguments: stub never ran" "" "$(argv)"
check_eq "'-' arguments: no command ran" "absent" "$([ -e "${TMP}/pwned" ] && echo present || echo absent)"
SCION_JSON='{"version": "-x"}'
run_driver --repo "${REPO_URL}"
check_rc "scion version starting with '-': rejected (rc 2)" 2 "${RC}"
SCION_JSON=""

# Bad version strings (path safety, TAG_RE).
for bad in "../x" "a/b" "v1 0" ".hidden"; do
  run_driver "${bad}" --repo "${REPO_URL}"
  check_rc "bad version '${bad}': rejected (rc 2)" 2 "${RC}"
done
check_eq "bad versions: nothing cloned" "absent" "$([ -e "${CACHE}/scion" ] && echo present || echo absent)"

# ---------------------------------------------------------------------------
# Round 2: SCION_IMAGE_REGISTRY fallback (F4)
# ---------------------------------------------------------------------------

reset_cache
RUN_ENV=(SCION_IMAGE_REGISTRY=reg.example/env/)
present remote:reg.example/env/scion-base:v1.0.0 remote:reg.example/env/scion-claude:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" -- --target scion-claude --push
check_rc "env registry: exit 0" 0 "${RC}"
check_contains "env registry: pre-flight checks the env registry" "docker buildx imagetools inspect reg.example/env/scion-base:v1.0.0" "$(ctr)"
check_contains "env registry: summary refs use it" "reg.example/env/scion-claude:v1.0.0  sha256:dg-reg.example/env/scion-claude:v1.0.0" "${OUT}"
run_driver v1.0.0 --repo "${REPO_URL}" --also-tag latest -- --push
check_rc "env registry + --also-tag latest without --force-latest: exit 1" 1 "${RC}"
check_contains "env registry + --also-tag latest: message names it" "registry (reg.example/env)" "${OUT}"
check_eq "env registry + --also-tag latest: stub never ran" "" "$(argv)"
run_driver v1.0.0 --repo "${REPO_URL}" --also-tag latest -- --registry ""
check_rc "env registry overridden by --registry '': exit 0" 0 "${RC}"
check_contains "env registry overridden: bare local :latest" "docker tag core-base:v1.0.0 core-base:latest" "$(ctr)"
RUN_ENV=()

# ---------------------------------------------------------------------------
# Round 2: cloud-build target-mode banner, podman :latest push and digest (F4, F7)
# ---------------------------------------------------------------------------

reset_cache
present
# The registry already holds an OLDER build of this tag: its digests must not
# be reported for a build that was only submitted.
present remote:reg.example/x/core-base:v1.0.0 remote:reg.example/x/scion-hub:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" -- --builder cloud-build --registry reg.example/x
CBM="$(cat "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" 2>/dev/null || true)"
check_rc "cloud-build: exit 0" 0 "${RC}"
check_contains "cloud-build: says the build was only submitted" "submitted to Cloud Build; images and digests are not available until the build finishes" "${OUT}"
check_not_contains "cloud-build: no registry digest lookup" "imagetools" "$(ctr)"
check_not_contains "cloud-build: the older build's digest is not reported" "sha256:" "${OUT}${CBM}"
check_contains "cloud-build: manifest marked submitted" '"submitted": true,' "${CBM}"
check_contains "cloud-build: manifest not marked pushed" '"pushed": false,' "${CBM}"
check_contains "cloud-build: manifest lists scion-hub with an empty digest" '"scion-hub": {"ref": "reg.example/x/scion-hub:v1.0.0", "digest": ""}' "${CBM}"
check_eq "cloud-build: the banner gives exactly the four images" "4" "$(printf '%s\n' "${CBM}" | grep -c '"ref": ' || true)"
check_not_contains "cloud-build banner: the parenthesised note is not an image" "(image" "${CBM}"
run_driver v1.0.0 --repo "${REPO_URL}" --also-tag latest --force-latest -- --builder cloud-build --registry reg.example/x
check_rc "cloud-build + --also-tag latest: rejected (rc 2)" 2 "${RC}"
check_contains "cloud-build + --also-tag latest: message" "submitted asynchronously" "${OUT}"
check_eq "cloud-build + --also-tag latest: stub never ran" "" "$(argv)"
check_not_contains "cloud-build + --also-tag latest: no imagetools create" "imagetools create" "$(ctr)"
reset_cache
run_driver v1.0.0 --repo "${REPO_URL}" --also-tag latest --force-latest -- --builder cloud-build --registry reg.example/x
check_eq "cloud-build + --also-tag latest: rejected before the clone" "absent" "$([ -e "${CACHE}/scion" ] && echo present || echo absent)"
# A submitted manifest is never merged with a finished one, either way.
SD3="${TMP}/staged3"
present local:core-base:v1.0.0 local:reg.example/x/core-base:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD3}" -- --target core-base --builder cloud-build --registry reg.example/x
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD3}" -- --target core-base --registry reg.example/x
check_not_contains "after a submitted manifest: a local build starts fresh" "earlier image(s) kept" "${OUT}"
check_contains "after a submitted manifest: header is the local build's" '"submitted": false,' "$(cat "${SD3}/build-manifest.json" 2>/dev/null || true)"
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD3}" -- --target core-base --builder cloud-build --registry reg.example/x
check_not_contains "after a local manifest: a submitted build starts fresh" "earlier image(s) kept" "${OUT}"
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD3}" -- --target core-base --builder cloud-build --registry reg.example/x
check_not_contains "two submitted builds: no merge either" "earlier image(s) kept" "${OUT}"

present local:reg.example/x/scion-base:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" --also-tag latest --force-latest -- --target hub --builder local-podman --registry reg.example/x --push
check_rc "podman push: exit 0" 0 "${RC}"
check_contains "podman push: :latest tagged" "podman tag reg.example/x/scion-hub:v1.0.0 reg.example/x/scion-hub:latest" "$(ctr)"
check_contains "podman push: :latest pushed" "podman push reg.example/x/scion-hub:latest" "$(ctr)"
check_contains "podman push: registry digest via --digestfile" "reg.example/x/scion-hub:v1.0.0  sha256:pd-reg.example/x/scion-hub:v1.0.0" "${OUT}"

# ---------------------------------------------------------------------------
# Round 2: dry-run pre-flight and pre-flight without docker (F4, F6)
# ---------------------------------------------------------------------------

reset_cache
present
run_driver v1.0.0 --repo "${REPO_URL}" -- --target common --dry-run
check_rc "dry run, base absent: exit 0" 0 "${RC}"
check_contains "dry run, base absent: warns" "core-base:v1.0.0 not found (dry run, continuing)" "${OUT}"
check_eq "dry run, base absent: build-images.sh dry run ran" "--target common --tag v1.0.0 --dry-run" "$(argv)"

run_driver --no-ctr v1.0.0 --repo "${REPO_URL}" -- --target common --dry-run
check_rc "dry run without docker: exit 0" 0 "${RC}"
check_contains "dry run without docker: warns" "docker is not on PATH" "${OUT}"

run_driver --no-ctr v1.0.0 --repo "${REPO_URL}" -- --target common --builder cloud-build --registry reg.example/x
check_rc "cloud-build stacked target without docker: exit 1" 1 "${RC}"
check_contains "cloud-build stacked target without docker: actionable message" "use the default --target all" "${OUT}"
check_eq "cloud-build stacked target without docker: stub never ran" "" "$(argv)"

# ---------------------------------------------------------------------------
# Round 2: dirty clone is not reused (F4)
# ---------------------------------------------------------------------------

reset_cache
run_driver v1.0.0 --repo "${REPO_URL}"
SRC="${CACHE}/scion/image-src/v1.0.0"
echo "changed" >> "${SRC}/image-build/scripts/build-images.sh" 2>/dev/null || fail "dirty: first run left no clone"
run_driver v1.0.0 --repo "${REPO_URL}"
check_rc "modified tracked file: exit 1" 1 "${RC}"
check_contains "modified tracked file: message" "modified or untracked files" "${OUT}"
check_eq "modified tracked file: build not started" "" "$(argv)"

# ---------------------------------------------------------------------------
# Round 2: staged builds merge into one manifest (F8)
# ---------------------------------------------------------------------------

reset_cache
SD="${TMP}/staged"
present local:core-base:v1.0.0 local:scion-base:v1.0.0 local:scion-claude:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD}" -- --target core-base
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD}" -- --target scion-base
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD}" -- --target scion-claude
check_rc "staged: third run exit 0" 0 "${RC}"
check_contains "staged: third run reports the merge" "2 earlier image(s) kept" "${OUT}"
want_staged="$(cat <<EOF
  "images": {
    "core-base": {"ref": "core-base:v1.0.0", "digest": "sha256:id-core-base:v1.0.0"},
    "scion-base": {"ref": "scion-base:v1.0.0", "digest": "sha256:id-scion-base:v1.0.0"},
    "scion-claude": {"ref": "scion-claude:v1.0.0", "digest": "sha256:id-scion-claude:v1.0.0"}
  }
}
EOF
)"
check_eq "staged: one manifest lists all three images" "${want_staged}" "$(sed -n '/"images"/,$p' "${SD}/build-manifest.json" 2>/dev/null || true)"
# Rebuilding one image replaces only its entry.
present local:core-base:v1.0.0 local:scion-base:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD}" -- --target scion-base
check_contains "staged rebuild: other entries kept" '"scion-claude": {"ref": "scion-claude:v1.0.0", "digest": "sha256:id-scion-claude:v1.0.0"}' "$(cat "${SD}/build-manifest.json" 2>/dev/null || true)"
check_eq "staged rebuild: no duplicate entry" "1" "$(grep -c '"scion-base": ' "${SD}/build-manifest.json" || true)"
# A manifest of another commit is not merged.
if sed 's/"commit": "[0-9a-f]*"/"commit": "0000000000000000000000000000000000000000"/' "${SD}/build-manifest.json" > "${TMP}/m.json" 2>/dev/null; then
  cp "${TMP}/m.json" "${SD}/build-manifest.json"
else
  fail "staged: no manifest to rewrite"
fi
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD}" -- --target core-base
check_not_contains "different commit: starts fresh (no merge)" "earlier image(s) kept" "${OUT}"
check_eq "different commit: only the new image" "1" "$(grep -c '"ref": ' "${SD}/build-manifest.json" || true)"
check_contains "different commit: real commit recorded" "\"commit\": \"${C1}\"" "$(cat "${SD}/build-manifest.json" 2>/dev/null || true)"

# ---------------------------------------------------------------------------
# Round 2: json_str escaping (F10). The function is taken from the driver.
# ---------------------------------------------------------------------------

json_fn="$(sed -n '/^json_str() {/,/^}/p' "${DRIVER}")"
if [ -n "${json_fn}" ] && eval "${json_fn}"; then
  check_eq "json_str: quote and backslash" '"a\"b\\c&d"' "$(json_str 'a"b\c&d')"
  check_eq "json_str: tab, CR, newline" '"x\ty\rz\nw"' "$(json_str "$(printf 'x\ty\rz\nw')")"
  check_eq "json_str: plain value unchanged" '"scion-claude:v1.0.0"' "$(json_str 'scion-claude:v1.0.0')"
else
  fail "json_str: could not load the function from the driver"
fi

# ---------------------------------------------------------------------------
# Round 3: dry runs never tag or write a manifest (R2-F1.1)
# ---------------------------------------------------------------------------

reset_cache
present local:core-base:v1.0.0 local:scion-base:v1.0.0 local:scion-claude:v1.0.0 local:scion-hub:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" --also-tag latest -- --dry-run
check_rc "dry run + --also-tag latest: exit 0" 0 "${RC}"
check_not_contains "dry run + --also-tag latest: no tag call" " tag " "$(ctr)"
check_eq "dry run + --also-tag latest: no manifest" "absent" "$([ -e "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" ] && echo present || echo absent)"
run_driver v1.0.0 --repo "${REPO_URL}" --also-tag latest --force-latest -- --registry reg.example/x --dry-run
check_rc "dry run + --also-tag latest + registry: exit 0" 0 "${RC}"
check_not_contains "dry run + --also-tag latest + registry: no imagetools create" "imagetools create" "$(ctr)"
check_eq "dry run + --also-tag latest + registry: no manifest" "absent" "$([ -e "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" ] && echo present || echo absent)"

# ---------------------------------------------------------------------------
# Round 3: --platform all without --push is a pushed build (R2-F1.2)
# ---------------------------------------------------------------------------

reset_cache
present remote:reg.example/x/scion-claude:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" --also-tag latest --force-latest -- --registry reg.example/x --platform all
check_rc "multi-platform: exit 0" 0 "${RC}"
check_eq "multi-platform: golden argv (no --push added by the driver)" "--target all --tag v1.0.0 --registry reg.example/x --platform all" "$(argv)"
check_contains "multi-platform: registry digests" "reg.example/x/scion-claude:v1.0.0  sha256:dg-reg.example/x/scion-claude:v1.0.0" "${OUT}"
check_contains "multi-platform: :latest via imagetools create" "docker buildx imagetools create -t reg.example/x/core-base:latest reg.example/x/core-base:v1.0.0" "$(ctr)"
check_not_contains "multi-platform: no local docker tag" "docker tag " "$(ctr)"
check_contains "multi-platform: manifest says pushed" '"pushed": true,' "$(cat "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" 2>/dev/null || true)"

# ---------------------------------------------------------------------------
# Round 3: podman pushed digest via skopeo, and podman registry-only base (R2-F1.3)
# ---------------------------------------------------------------------------

reset_cache
present remote:reg.example/x/scion-base:v1.0.0
run_driver --with-skopeo v1.0.0 --repo "${REPO_URL}" -- --target hub --builder local-podman --registry reg.example/x --push
check_rc "podman + skopeo: exit 0" 0 "${RC}"
check_contains "podman registry-only base: found via podman manifest inspect" "podman manifest inspect reg.example/x/scion-base:v1.0.0" "$(ctr)"
check_contains "podman + skopeo: skopeo digest used" "reg.example/x/scion-hub:v1.0.0  sha256:sk-reg.example/x/scion-hub:v1.0.0" "${OUT}"
check_contains "podman + skopeo: skopeo asked the registry" "skopeo inspect --format {{.Digest}} docker://reg.example/x/scion-hub:v1.0.0" "$(ctr)"
check_not_contains "podman + skopeo: no --digestfile re-push" "push --digestfile" "$(ctr)"
run_driver v1.0.0 --repo "${REPO_URL}" -- --target hub --builder local-podman --registry reg.example/x --push
check_contains "podman without skopeo: --digestfile fallback" "reg.example/x/scion-hub:v1.0.0  sha256:pd-reg.example/x/scion-hub:v1.0.0" "${OUT}"

# ---------------------------------------------------------------------------
# Round 3: builder and push mode are part of the merge key (R2-F2)
# ---------------------------------------------------------------------------

reset_cache
SD2="${TMP}/staged2"
present local:reg.example/x/core-base:v1.0.0 remote:reg.example/x/core-base:v1.0.0 remote:reg.example/x/scion-base:v1.0.0
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD2}" -- --target core-base --registry reg.example/x
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD2}" -- --target scion-base --registry reg.example/x --push
check_rc "push mode change: exit 0" 0 "${RC}"
check_not_contains "push mode change: starts fresh" "earlier image(s) kept" "${OUT}"
check_eq "push mode change: only the pushed image" "1" "$(grep -c '"ref": ' "${SD2}/build-manifest.json" || true)"
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD2}" -- --target scion-base --registry reg.example/x --push --builder local-podman
check_not_contains "builder change: starts fresh" "earlier image(s) kept" "${OUT}"
check_contains "builder change: header records the builder" '"builder": "local-podman",' "$(cat "${SD2}/build-manifest.json" 2>/dev/null || true)"
run_driver v1.0.0 --repo "${REPO_URL}" --src-dir "${SD2}" -- --target core-base --registry reg.example/x --push --builder local-podman
check_contains "same builder and push mode: merges" "1 earlier image(s) kept" "${OUT}"

# ---------------------------------------------------------------------------
# Round 3: single-image targets need the step-ID fallback (R2-F3)
# ---------------------------------------------------------------------------

reset_cache
present local:scion-base:v0.9.0
run_driver v0.9.0 --repo "${REPO_URL}" -- --target scion-claude
check_rc "single image on an old release: exit 1" 1 "${RC}"
check_contains "single image on an old release: names the floor" "single-image target, which needs v0.3.0-preview.4 or later" "${OUT}"
check_eq "single image on an old release: stub never ran" "" "$(argv)"
run_driver v0.9.0 --repo "${REPO_URL}" -- --target harnesses
check_rc "group target on an old release: exit 0" 0 "${RC}"
check_eq "group target on an old release: golden argv" "--target harnesses --tag v0.9.0" "$(argv)"

# ---------------------------------------------------------------------------
# Round 3: canaries against this repository's own tree (R2-F3, R2-F4)
# ---------------------------------------------------------------------------

REPO_ROOT_FOR_CANARY="$(cd "${SCRIPT_DIR}/../.." && pwd)"
canary_fns="$(sed -n '/^tree_is_stamped() {/,/^}/p; /^tree_has_step_targets() {/,/^}/p' "${DRIVER}")"
if [ -n "${canary_fns}" ] && eval "${canary_fns}"; then
  if tree_is_stamped "${REPO_ROOT_FOR_CANARY}"; then
    pass "canary: this repo's image-build is detected as version-stamped"
  else
    fail "canary: the driver's stamping detection no longer matches this repo's build-images.sh / scion-base Dockerfile; update tree_is_stamped"
  fi
  if tree_has_step_targets "${REPO_ROOT_FOR_CANARY}"; then
    pass "canary: this repo's targets.sh is detected as accepting single-image targets"
  else
    fail "canary: the driver's single-image target detection no longer matches this repo's lib/targets.sh; update tree_has_step_targets"
  fi
else
  fail "canary: could not load tree_is_stamped/tree_has_step_targets from the driver"
fi

# ---------------------------------------------------------------------------
# Round 3: passthrough values that start with '-' (R2-F5)
# ---------------------------------------------------------------------------

reset_cache
for flag in --registry --target --platform --builder; do
  run_driver v1.0.0 --repo "${REPO_URL}" -- "${flag}" -x
  check_rc "passthrough ${flag} '-x': rejected (rc 2)" 2 "${RC}"
done
check_eq "passthrough '-' values: stub never ran" "" "$(argv)"
check_eq "passthrough '-' values: nothing cloned" "absent" "$([ -e "${CACHE}/scion" ] && echo present || echo absent)"

# ---------------------------------------------------------------------------
# Round 3: the Steps banner is not glob-expanded (R2-F6)
# ---------------------------------------------------------------------------

reset_cache
mkdir -p "${TMP}/globcwd"
touch "${TMP}/globcwd/GLOBBED"
RUN_ENV=(STUB_BANNER="core-base *")
RUN_CWD="${TMP}/globcwd"
run_driver v1.0.0 --repo "${REPO_URL}"
RUN_ENV=()
RUN_CWD=""
check_rc "glob in banner: exit 0" 0 "${RC}"
check_contains "glob in banner: '*' kept literally" '"*": {"ref": "*:v1.0.0"' "$(cat "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" 2>/dev/null || true)"
check_not_contains "glob in banner: no pathname expansion" "GLOBBED" "$(cat "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" 2>/dev/null || true)"

# ---------------------------------------------------------------------------
# Round 3: untracked files block reuse; build-manifest.json does not (FYI-a)
# ---------------------------------------------------------------------------

reset_cache
run_driver v1.0.0 --repo "${REPO_URL}"
SRC="${CACHE}/scion/image-src/v1.0.0"
run_driver v1.0.0 --repo "${REPO_URL}"
check_rc "reuse with build-manifest.json present: exit 0" 0 "${RC}"
check_contains "reuse with build-manifest.json present: reused" "reusing existing clone" "${OUT}"
echo stray > "${SRC}/stray.go" 2>/dev/null || fail "untracked: no clone"
run_driver v1.0.0 --repo "${REPO_URL}"
check_rc "untracked file in the clone: exit 1" 1 "${RC}"
check_contains "untracked file in the clone: message" "modified or untracked files" "${OUT}"
check_contains "untracked file in the clone: names the file" "files: stray.go" "${OUT}"
check_not_contains "untracked file in the clone: no count for a single file" "(and " "${OUT}"
for k in 1 2 3 4 5 6 7 8 9 10; do echo x > "${SRC}/stray-${k}.txt"; done
run_driver v1.0.0 --repo "${REPO_URL}"
check_contains "11 untracked files: truncated to 10 plus a count" "(and 1 more)" "${OUT}"
check_not_contains "11 untracked files: no negative count" "(and -" "${OUT}"
listed="$(printf '%s\n' "${OUT}" | sed -n 's/.*modified or untracked files: \(.*\) (and 1 more).*/\1/p' | head -n 1)"
# shellcheck disable=SC2086 # split the listed names into words on purpose
check_eq "11 untracked files: exactly 10 names listed" "10" "$(printf '%s\n' ${listed} | grep -c . || true)"
check_eq "untracked file in the clone: build not started" "" "$(argv)"
check_eq "untracked file in the clone: file kept" "stray" "$(cat "${SRC}/stray.go" 2>/dev/null || true)"

# ---------------------------------------------------------------------------
# Round 3: another run creating --src-dir during the clone (FYI-b)
# ---------------------------------------------------------------------------

reset_cache
RACE="${TMP}/race-src"
RUN_ENV=(STUB_RACE_DIR="${RACE}" STUB_REAL_MV="${TOOLBIN}/mv")
run_driver --race v1.0.0 --repo "${REPO_URL}" --src-dir "${RACE}"
RUN_ENV=()
check_rc "src-dir race: exit 1" 1 "${RC}"
check_contains "src-dir race: message" "was created by another run" "${OUT}"
check_eq "src-dir race: the other run's tree is never built" "" "$(argv)"
check_eq "src-dir race: the other run's file is kept" "other" "$(cat "${RACE}/other-run" 2>/dev/null || true)"
check_eq "src-dir race: our nested copy is removed" "" "$(find "${RACE}" -mindepth 1 -maxdepth 1 -name '.scion-image-src.*' 2>/dev/null)"

# The usual race: the other run fills --src-dir before this one replaces it.
reset_cache
RACE2="${TMP}/race-src2"
mkdir -p "${RACE2}"
RUN_ENV=(STUB_RACE_RMDIR_DIR="${RACE2}" STUB_REAL_RMDIR="${TOOLBIN}/rmdir" STUB_REAL_MV="${TOOLBIN}/mv")
run_driver --race v1.0.0 --repo "${REPO_URL}" --src-dir "${RACE2}"
RUN_ENV=()
check_rc "src-dir filled during the clone: exit 1" 1 "${RC}"
check_contains "src-dir filled during the clone: race message" "was created by another run" "${OUT}"
check_eq "src-dir filled during the clone: build not started" "" "$(argv)"
check_eq "src-dir filled during the clone: other run's file kept" "other" "$(cat "${RACE2}/other-run" 2>/dev/null || true)"

# ---------------------------------------------------------------------------
# Round 4: git status failure, temp manifest cleanup, group targets (R3-F2, F4, F6)
# ---------------------------------------------------------------------------

reset_cache
run_driver v1.0.0 --repo "${REPO_URL}"
RUN_ENV=(STUB_REAL_GIT="${TOOLBIN}/git")
run_driver --git-status-fails v1.0.0 --repo "${REPO_URL}"
RUN_ENV=()
check_rc "git status fails on reuse: exit 1" 1 "${RC}"
check_contains "git status fails on reuse: message" "git status failed in the checkout" "${OUT}"
check_eq "git status fails on reuse: build not started" "" "$(argv)"

SRC="${CACHE}/scion/image-src/v1.0.0"
RUN_ENV=(STUB_MV_FAIL_DST="${SRC}/build-manifest.json" STUB_REAL_MV="${TOOLBIN}/mv")
run_driver --race v1.0.0 --repo "${REPO_URL}"
RUN_ENV=()
check_rc "manifest write fails: non-zero exit" 1 "$([ "${RC}" -ne 0 ] && echo 1 || echo 0)"
check_eq "manifest write fails: temp manifest removed" "" "$(find "${SRC}" -maxdepth 1 -name 'build-manifest.json.*' 2>/dev/null)"
run_driver v1.0.0 --repo "${REPO_URL}"
check_contains "manifest write fails: the clone is still reusable" "reusing existing clone" "${OUT}"

reset_cache
present local:core-base:v0.9.0 local:scion-base:v0.9.0
for g in core-base thick-prep scion-base harnesses hub omni common all thick; do
  run_driver v0.9.0 --repo "${REPO_URL}" -- --target "${g}"
  check_rc "group target ${g} on an old release: accepted" 0 "${RC}"
done

# ---------------------------------------------------------------------------
# Round 5: inherited THICK_BUILD / OMNI_BUILD never reach the tag's script (R4-F3)
# ---------------------------------------------------------------------------

reset_cache
present local:core-base:v1.0.0
RUN_ENV=(THICK_BUILD=true OMNI_BUILD=true)
run_driver v1.0.0 --repo "${REPO_URL}" -- --target scion-base
RUN_ENV=()
check_rc "THICK_BUILD/OMNI_BUILD exported: exit 0" 0 "${RC}"
check_eq "THICK_BUILD/OMNI_BUILD exported: cleared for build-images.sh" "THICK=unset OMNI=unset" "$(cat "${TMP}/env.log")"
check_contains "THICK_BUILD/OMNI_BUILD exported: pre-flight still checks core-base" "pre-flight: core-base:v1.0.0 found for --target scion-base" "${OUT}"

# ---------------------------------------------------------------------------
# Round 6: a failing build-images.sh stops the run (R5-F1)
# ---------------------------------------------------------------------------

reset_cache
present remote:reg.example/x/core-base:v1.0.0 remote:reg.example/x/scion-base:v1.0.0 remote:reg.example/x/scion-claude:v1.0.0 remote:reg.example/x/scion-hub:v1.0.0
RUN_ENV=(STUB_FAIL=1)
run_driver v1.0.0 --repo "${REPO_URL}" --also-tag latest --force-latest -- --registry reg.example/x --push
RUN_ENV=()
check_rc "failing build: exit 1" 1 "${RC}"
check_contains "failing build: message" "build-images.sh failed" "${OUT}"
check_not_contains "failing build: :latest not moved" "imagetools create" "$(ctr)"
check_not_contains "failing build: no digests looked up" "imagetools inspect --format" "$(ctr)"
check_eq "failing build: no manifest" "absent" "$([ -e "${CACHE}/scion/image-src/v1.0.0/build-manifest.json" ] && echo present || echo absent)"
check_eq "failing build: no temp manifest left" "" "$(find "${CACHE}/scion/image-src/v1.0.0" -maxdepth 1 -name 'build-manifest.json.*' 2>/dev/null)"

# ---------------------------------------------------------------------------
# Standalone and bash 3.2 safety (static)
# ---------------------------------------------------------------------------

code="$(grep -v '^[[:space:]]*#' "${DRIVER}")"
# shellcheck disable=SC2016 # literal patterns, nothing is meant to expand
for pat in 'declare -A' 'local -A' 'typeset -A' 'mapfile' 'readarray' ',,}' '^^}' '&>>' '|&' 'declare -n' 'local -n' '[[ -v' 'coproc' '@Q}' '${EPOCHSECONDS' 'wait -n'; do
  check_not_contains "bash 3.2: driver has no '${pat}'" "${pat}" "${code}"
done
if printf '%s\n' "${code}" | grep -Eq '(^[[:space:]]*|[;&|][[:space:]]*)(source|\.)[[:space:]]+[^[:space:]]'; then
  fail "standalone: driver sources another file"
else
  pass "standalone: driver sources no other file"
fi

# ---------------------------------------------------------------------------
# Isolation self-check (F1): re-run the whole suite with fake docker, podman,
# skopeo and scion in the SAME directory as the real tools, as on CI runners.
# Any of them running fails the inner suite (exit 99).
# ---------------------------------------------------------------------------

if [ -z "${TBRI_INNER:-}" ]; then
  FAKEBIN="${TMP}/fake-usr-bin"
  mkdir -p "${FAKEBIN}"
  for tool in git bash sed awk grep cut head tr date mktemp dirname basename ls mv rmdir mkdir rm tee cat env chmod cp touch find ln base64 sha256sum; do
    ln -s "$(command -v "${tool}")" "${FAKEBIN}/${tool}"
  done
  if p="$(command -v python3)"; then ln -s "${p}" "${FAKEBIN}/python3"; fi
  for b in docker podman skopeo scion gcloud; do
    printf '#!/bin/sh\necho "a real %s ran" >&2\nexit 99\n' "${b}" > "${FAKEBIN}/${b}"
    chmod +x "${FAKEBIN}/${b}"
  done
  set +e
  inner_out="$(TBRI_INNER=1 DRIVER_UNDER_TEST="${DRIVER}" PATH="${FAKEBIN}" bash "${SCRIPT_DIR}/$(basename "${BASH_SOURCE[0]}")" 2>&1)"
  inner_rc=$?
  set -e
  if [ "${inner_rc}" -eq 0 ]; then
    pass "isolation: the suite passes with fake docker/podman/skopeo/scion beside the real tools ($(printf '%s\n' "${inner_out}" | tail -n 1))"
  else
    fail "isolation: the suite fails with fake docker/podman/skopeo/scion beside the real tools (rc ${inner_rc})"
    printf '%s\n' "${inner_out}" | grep -E '^FAIL|real .* ran' | head -n 10 >&2
  fi
fi

echo ""
echo "${PASSES} passed, ${FAILS} failed"
[ "${FAILS}" -eq 0 ]
