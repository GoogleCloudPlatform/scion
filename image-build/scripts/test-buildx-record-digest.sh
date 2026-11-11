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

# Tests for image-build/scripts/buildx-record-digest.sh, and a static check
# that the Cloud Build configs the release driver submits run every pushing
# step through it. Pure bash: no docker, no gcloud, no network. A stub docker
# writes a fixture buildx metadata file.
#
# Usage: bash image-build/scripts/test-buildx-record-digest.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE_BUILD_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
REC="${SCRIPT_DIR}/buildx-record-digest.sh"
[ -f "${REC}" ] || { echo "buildx-record-digest.sh not found at ${REC}" >&2; exit 2; }

TMP="$(mktemp -d "${TMPDIR:-/tmp}/test-buildx-record-digest.XXXXXX")"
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
    *) fail "$1"; printf '       want substring: %s\n       got: %s\n' "$2" "$3" >&2 ;;
  esac
}

# Stub docker: logs its argv, writes ${STUB_META} (a fixture file, or nothing
# when it is "none") to the --metadata-file path, and exits ${STUB_RC:-0}.
mkdir -p "${TMP}/bin"
cat > "${TMP}/bin/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${STUB_LOG:?}"
meta=""
prev=""
for a in "$@"; do
  if [ "${prev}" = "--metadata-file" ]; then meta="${a}"; fi
  prev="${a}"
done
if [ -n "${meta}" ] && [ "${STUB_META:-none}" != "none" ]; then
  cat "${STUB_META}" > "${meta}"
fi
exit "${STUB_RC:-0}"
STUB
chmod +x "${TMP}/bin/docker"

REG="us-central1-docker.pkg.dev/proj-1/public-docker"
HEX="0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
DIG="sha256:${HEX}"

# meta_pretty <file> <image> <digest>: buildx --metadata-file output, pretty.
meta_pretty() {
  cat > "$1" <<EOF
{
  "buildx.build.ref": "mybuilder/mybuilder0/abc",
  "containerimage.descriptor": {
    "mediaType": "application/vnd.oci.image.index.v1+json",
    "digest": "$3",
    "size": 1609
  },
  "containerimage.digest": "$3",
  "image.name": "${REG}/$2:abc1234,${REG}/$2:v1.0.0"
}
EOF
}

# run_rec <image> [extra buildx args...]: runs the recorder for <image> with a
# fresh BUILDER_OUTPUT; sets RC, OUT and OUTDIR.
N=0
run_rec() {
  local img=$1
  shift
  N=$((N + 1))
  OUTDIR="${TMP}/out.${N}"
  mkdir -p "${OUTDIR}"
  : > "${TMP}/log"
  set +e
  OUT="$(PATH="${TMP}/bin:${PATH}" STUB_LOG="${TMP}/log" BUILDER_OUTPUT="${OUTDIR}" \
    bash "${REC}" "${REG}/${img}:v1.0.0" buildx build --platform linux/amd64,linux/arm64 \
    -t "${REG}/${img}:abc1234" -t "${REG}/${img}:v1.0.0" -f Dockerfile "$@" 2>&1)"
  RC=$?
  set -e
}
output() { if [ -e "${OUTDIR}/output" ]; then cat "${OUTDIR}/output"; else echo "<absent>"; fi; }

# --- good: pretty JSON --------------------------------------------------------
meta_pretty "${TMP}/m.good" scion-claude "${DIG}"
STUB_META="${TMP}/m.good"; export STUB_META
run_rec scion-claude --pull --push .
check_eq "good: exit 0" 0 "${RC}"
check_eq "good: exact output bytes" "$(printf '%s %s\n' "${REG}/scion-claude:v1.0.0" "${DIG}" | od -An -c)" "$(od -An -c < "${OUTDIR}/output")"
check_contains "good: docker gets --metadata-file before the build args" "buildx build --metadata-file " "$(cat "${TMP}/log")"
check_contains "good: the caller's args follow unchanged" "-t ${REG}/scion-claude:v1.0.0 -f Dockerfile --pull --push ." "$(cat "${TMP}/log")"

# --- good: compact JSON ----------------------------------------------------
printf '{"containerimage.digest":"%s","image.name":"%s"}' "${DIG}" "${REG}/scion-hub:v1.0.0" > "${TMP}/m.compact"
STUB_META="${TMP}/m.compact"
run_rec scion-hub --push .
check_eq "compact JSON: exit 0" 0 "${RC}"
check_eq "compact JSON: output" "${REG}/scion-hub:v1.0.0 ${DIG}" "$(output)"

# --- failures after the build: no output is written --------------------------
# bad_meta <name> <fixture file> <message>
bad_meta() {
  STUB_META="$2"
  run_rec scion-claude --push .
  check_eq "$1: exit 1" 1 "${RC}"
  check_contains "$1: message" "$3" "${OUT}"
  check_eq "$1: no step output" "<absent>" "$(output)"
}
bad_meta "metadata missing" none "no metadata file"
: > "${TMP}/m.empty"
bad_meta "metadata empty" "${TMP}/m.empty" "no metadata file"
printf '{"image.name": "%s"}\n' "${REG}/scion-claude:v1.0.0" > "${TMP}/m.nodigest"
bad_meta "no digest key" "${TMP}/m.nodigest" "no containerimage.digest"
printf 'not json at all\n' > "${TMP}/m.garbage"
bad_meta "malformed metadata" "${TMP}/m.garbage" "no containerimage.digest"
meta_pretty "${TMP}/m.short" scion-claude "sha256:${HEX%?}"
bad_meta "digest of 63 hex" "${TMP}/m.short" "is not sha256:<64 hex>"
meta_pretty "${TMP}/m.long" scion-claude "sha256:${HEX}0"
bad_meta "digest of 65 hex" "${TMP}/m.long" "is not sha256:<64 hex>"
meta_pretty "${TMP}/m.upper" scion-claude "sha256:$(printf '%s' "${HEX}" | tr 'a-f' 'A-F')"
bad_meta "uppercase digest" "${TMP}/m.upper" "is not sha256:<64 hex>"
{ printf '{"containerimage.digest": "%s",\n' "${DIG}"; printf '"containerimage.digest": "%s",\n' "${DIG}"; printf '"image.name": "%s"}\n' "${REG}/scion-claude:v1.0.0"; } > "${TMP}/m.two"
bad_meta "two digest keys" "${TMP}/m.two" "more than one containerimage.digest"
printf '{"containerimage.digest": "%s"}\n' "${DIG}" > "${TMP}/m.noname"
bad_meta "no image.name" "${TMP}/m.noname" "no image.name"
meta_pretty "${TMP}/m.other" scion-codex "${DIG}"
bad_meta "image.name does not name the ref" "${TMP}/m.other" "does not name ${REG}/scion-claude:v1.0.0"
printf '{"containerimage.digest": "%s", "image.name": "%s"}\n' "${DIG}" "${REG}/scion-claude:v1.0.0x" > "${TMP}/m.prefix"
bad_meta "image.name with the ref as a prefix only" "${TMP}/m.prefix" "does not name"

STUB_META="${TMP}/m.good"
STUB_RC=1 run_rec scion-claude --push .
check_eq "build fails: exit 1" 1 "${RC}"
check_contains "build fails: message" "docker buildx build failed" "${OUT}"
check_eq "build fails: no step output" "<absent>" "$(output)"

# --- the metadata dir is removed on exit, after a success and after a failure --
# Each run gets a private, empty TMPDIR; the stub's log proves the metadata
# file was under it, and the TMPDIR must be empty again afterwards.
mkdir "${TMP}/tmpdir.ok" "${TMP}/tmpdir.fail"
STUB_META="${TMP}/m.good"
TMPDIR="${TMP}/tmpdir.ok" run_rec scion-claude --push .
check_eq "cleanup on success: exit 0" 0 "${RC}"
check_contains "cleanup on success: metadata under the private TMPDIR" "--metadata-file ${TMP}/tmpdir.ok/" "$(cat "${TMP}/log")"
check_eq "cleanup on success: TMPDIR empty afterwards" "" "$(ls -A "${TMP}/tmpdir.ok")"
STUB_META="${TMP}/m.nodigest"
TMPDIR="${TMP}/tmpdir.fail" run_rec scion-claude --push .
check_eq "cleanup on failure: exit 1" 1 "${RC}"
check_contains "cleanup on failure: message" "no containerimage.digest" "${OUT}"
check_contains "cleanup on failure: metadata under the private TMPDIR" "--metadata-file ${TMP}/tmpdir.fail/" "$(cat "${TMP}/log")"
check_eq "cleanup on failure: TMPDIR empty afterwards" "" "$(ls -A "${TMP}/tmpdir.fail")"
STUB_META="${TMP}/m.good"

# --- refusals before the build: docker is not run, no output -----------------
# refused <name> <message> [recorder args...]: runs the recorder with exactly
# the given args.
refused() {
  local name=$1 msg=$2
  shift 2
  N=$((N + 1))
  OUTDIR="${TMP}/out.${N}"
  mkdir -p "${OUTDIR}"
  : > "${TMP}/log"
  set +e
  OUT="$(PATH="${TMP}/bin:${PATH}" STUB_LOG="${TMP}/log" BUILDER_OUTPUT="${BO-${OUTDIR}}" bash "${REC}" "$@" 2>&1)"
  RC=$?
  set -e
  check_eq "${name}: exit 1" 1 "${RC}"
  check_contains "${name}: message" "${msg}" "${OUT}"
  check_eq "${name}: docker not run" "" "$(cat "${TMP}/log")"
  check_eq "${name}: no step output" "<absent>" "$(output)"
}
R="${REG}/scion-claude:v1.0.0"
refused "no --push" "no --push" "${R}" buildx build -t "${R}" .
refused "ref is not a -t value" "is not a -t value" "${R}" buildx build -t "${REG}/scion-claude:abc1234" --push .
refused "ref only as a build-arg" "is not a -t value" "${R}" buildx build --build-arg "${R}" -t "${REG}/x:v1.0.0" --push .
refused "caller passes --metadata-file" "set by this script" "${R}" buildx build --metadata-file /tmp/m -t "${R}" --push .
refused "caller passes --metadata-file=" "set by this script" "${R}" buildx build --metadata-file=/tmp/m -t "${R}" --push .
refused "not a buildx build" "must start with 'buildx build'" "${R}" build -t "${R}" --push .
refused "ref without a tag" "not an image reference with a tag" "${REG}/scion-claude" buildx build -t "${REG}/scion-claude" --push .
refused "too few arguments" "usage" "${R}" buildx
BO="" refused "BUILDER_OUTPUT unset" "BUILDER_OUTPUT is not set" "${R}" buildx build -t "${R}" --push .
BO="${TMP}/no-such-dir" refused "BUILDER_OUTPUT not a directory" "BUILDER_OUTPUT is not set" "${R}" buildx build -t "${R}" --push .

# --- several images: one line per step, each well under the 4 KB limit --------
# The longest image name in the catalog, and the longest tag OCI allows.
LONGTAG="v$(printf '%0127d' 0)"
LONGREG="northamerica-northeast2-docker.pkg.dev/$(printf 'p%.0s' $(seq 30))/$(printf 'r%.0s' $(seq 63))"
printf '{"containerimage.digest": "%s", "image.name": "%s"}\n' "${DIG}" "${LONGREG}/scion-antigravity:${LONGTAG}" > "${TMP}/m.worst"
N=$((N + 1)); OUTDIR="${TMP}/out.${N}"; mkdir -p "${OUTDIR}"; : > "${TMP}/log"
set +e
OUT="$(PATH="${TMP}/bin:${PATH}" STUB_LOG="${TMP}/log" STUB_META="${TMP}/m.worst" BUILDER_OUTPUT="${OUTDIR}" \
  bash "${REC}" "${LONGREG}/scion-antigravity:${LONGTAG}" buildx build -t "${LONGREG}/scion-antigravity:${LONGTAG}" --push . 2>&1)"
RC=$?
set -e
check_eq "worst-case ref: exit 0" 0 "${RC}"
worst="$(wc -c < "${OUTDIR}/output")"
if [ "${worst}" -le 400 ]; then pass "worst-case ref: step output is ${worst} bytes (<= 400, limit 4096)"; else fail "worst-case ref: step output is ${worst} bytes"; fi

for img in core-base scion-base scion-claude scion-hub; do
  meta_pretty "${TMP}/m.${img}" "${img}" "sha256:$(printf '%s' "${img}" | sha256sum | cut -d' ' -f1)"
  STUB_META="${TMP}/m.${img}"
  run_rec "${img}" --push .
  check_eq "several images: ${img}: exit 0" 0 "${RC}"
  check_eq "several images: ${img}: one line" 1 "$(wc -l < "${OUTDIR}/output")"
  size="$(wc -c < "${OUTDIR}/output")"
  if [ "${size}" -lt 4096 ]; then pass "several images: ${img}: ${size} bytes < 4096"; else fail "several images: ${img}: ${size} bytes"; fi
done

# --- static check: the configs the driver submits ----------------------------
# In each config, a step that pushes a :$_TAG ref runs through the recorder
# with ref $_REGISTRY/<id minus build-/push->:$_TAG; no other step does. One
# line per step id: "<id> <push> <tag> <rec-ref>".
step_table() {
  awk '
    function flush() {
      if (id != "") printf "%s %s %s %s\n", id, push, tag, (rec == "" ? "-" : rec)
      id = ""; push = "nopush"; tag = "notag"; rec = ""; want = 0
    }
    BEGIN { push = "nopush"; tag = "notag" }
    /^  - name:/ || /^[a-z]/ { flush() }
    /^    id:/ { id = $2; gsub(/\047/, "", id) }
    /\047--push\047/ { push = "push" }
    /:\$_TAG\047/ { tag = "tag" }
    want == 1 { rec = $2; gsub(/\047/, "", rec); want = 0 }
    /buildx-record-digest\.sh\047, \047/ {
      s = $0; sub(/.*buildx-record-digest\.sh\047, \047/, "", s); sub(/\047.*/, "", s); rec = s
    }
    /^      - \047\/workspace\/image-build\/scripts\/buildx-record-digest\.sh\047$/ { want = 1 }
    END { flush() }
  ' "$1"
}
# shellcheck disable=SC2016 # literal $_REGISTRY / $_TAG, not expansions
for f in cloudbuild.yaml cloudbuild-common.yaml cloudbuild-core-base.yaml cloudbuild-scion-base.yaml cloudbuild-harnesses.yaml cloudbuild-hub.yaml cloudbuild-thick.yaml; do
  bad=""
  wrapped=0
  while read -r id push tag rec; do
    img="${id#build-}"; img="${img#push-}"
    if [ "${push}" = push ] && [ "${tag}" = tag ]; then
      if [ "${rec}" = "\$_REGISTRY/${img}:\$_TAG" ]; then wrapped=$((wrapped + 1)); else bad="${bad} ${id}(rec=${rec})"; fi
    elif [ "${rec}" != "-" ]; then
      bad="${bad} ${id}(not a pushing step, rec=${rec})"
    fi
  done < <(step_table "${IMAGE_BUILD_DIR}/${f}")
  check_eq "static: ${f}: every pushing step records its :\$_TAG digest" "" "${bad}"
  if [ "${wrapped}" -ge 1 ]; then pass "static: ${f}: ${wrapped} recording step(s)"; else fail "static: ${f}: no recording step"; fi
done
for f in cloudbuild-omni.yaml cloudbuild-hub-gke.yaml; do
  check_eq "static: ${f}: no recording step (pushes nothing the driver reads from step output)" "0" "$(grep -c 'buildx-record-digest' "${IMAGE_BUILD_DIR}/${f}" || true)"
done
check_eq "static: cloudbuild.yaml: build-core-base (no push) does not record" "build-core-base nopush tag -" "$(step_table "${IMAGE_BUILD_DIR}/cloudbuild.yaml" | grep '^build-core-base ')"
# shellcheck disable=SC2016 # literal yaml text
check_contains "static: generated single-harness step records its :\$_TAG digest" \
  "args: ['/workspace/image-build/scripts/buildx-record-digest.sh', '\\\$_REGISTRY/\${step_id}:\\\$_TAG', 'buildx', 'build'," \
  "$(cat "${SCRIPT_DIR}/builders/cloud-build.sh")"

echo ""
echo "${PASSES} passed, ${FAILS} failed"
[ "${FAILS}" -eq 0 ]
