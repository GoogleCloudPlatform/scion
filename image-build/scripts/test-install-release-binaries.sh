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
#
# Tests for image-build/lib/install-release-binaries.sh.
# Pure local tests using file:// URLs with curl — no network, no docker.
#
# Usage: bash image-build/scripts/test-install-release-binaries.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE_BUILD_SRC="${IMAGE_BUILD_UNDER_TEST:-$(cd "${SCRIPT_DIR}/.." && pwd)}"
INSTALLER="${IMAGE_BUILD_SRC}/lib/install-release-binaries.sh"
[ -f "${INSTALLER}" ] || { echo "install-release-binaries.sh not found at ${INSTALLER}" >&2; exit 2; }

TMP="$(mktemp -d "${TMPDIR:-/tmp}/test-install-release-binaries.XXXXXX")"
trap 'rm -rf -- "${TMP:?}"' EXIT

FAILS=0
PASSES=0
pass() { PASSES=$((PASSES + 1)); echo "ok   - $1"; }
fail() { FAILS=$((FAILS + 1)); echo "FAIL - $1" >&2; }
check_rc() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (want rc $2, got rc $3)"; printf '%s\n' "${OUT}" >&2; fi
}
check_contains() {
  case "$3" in
    *"$2"*) pass "$1" ;;
    *) fail "$1"; printf '       missing: %s\n       in output:\n%s\n' "$2" "$3" >&2 ;;
  esac
}
check_eq() {
  if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (want '$2', got '$3')"; fi
}

if command -v sha256sum >/dev/null 2>&1; then
  sha256_file() { sha256sum "$1" | awk '{print $1}'; }
else
  sha256_file() { shasum -a 256 "$1" | awk '{print $1}'; }
fi

# ---------------------------------------------------------------------------
# Syntax check
# ---------------------------------------------------------------------------
if bash -n "${INSTALLER}"; then
  pass "bash -n syntax clean: install-release-binaries.sh"
else
  fail "bash -n syntax clean: install-release-binaries.sh"
fi

# ---------------------------------------------------------------------------
# Build local release fixtures (served via file://)
# ---------------------------------------------------------------------------
RELEASES="${TMP}/releases"
BASE_URL="file://${RELEASES}"

make_tarball() {
  local dir="$1" bin="$2" content="$3" arch="$4"
  local stage="${TMP}/stage-${bin}-${arch}-$$"
  mkdir -p "${stage}" "${dir}"
  printf '#!/bin/sh\necho "%s"\n' "${content}" > "${stage}/${bin}"
  chmod +x "${stage}/${bin}"
  tar -czf "${dir}/${bin}-linux-${arch}.tar.gz" -C "${stage}" "${bin}"
  rm -rf "${stage}"
}

# 1. Complete release with scion + sciontool for amd64 and arm64
REL_FULL="${RELEASES}/v0.5.0-preview.1"
make_tarball "${REL_FULL}" scion "scion-v0.5.0-amd64" amd64
make_tarball "${REL_FULL}" sciontool "sciontool-v0.5.0-amd64" amd64
make_tarball "${REL_FULL}" scion "scion-v0.5.0-arm64" arm64
make_tarball "${REL_FULL}" sciontool "sciontool-v0.5.0-arm64" arm64
(
  cd "${REL_FULL}"
  for f in *.tar.gz; do
    printf '%s  %s\n' "$(sha256_file "$f")" "$f"
  done | sort -k2 > SHA256SUMS
)

# 2. Old release with scion only (no sciontool)
REL_OLD="${RELEASES}/v0.4.0-preview.3"
make_tarball "${REL_OLD}" scion "scion-v0.4.0-amd64" amd64
(
  cd "${REL_OLD}"
  printf '%s  %s\n' "$(sha256_file scion-linux-amd64.tar.gz)" "scion-linux-amd64.tar.gz" > SHA256SUMS
)

# 3. Corrupted checksum release
REL_BADSUM="${RELEASES}/v0.5.0-rc.1"
make_tarball "${REL_BADSUM}" scion "scion-bad" amd64
printf '0000000000000000000000000000000000000000000000000000000000000000  scion-linux-amd64.tar.gz\n' > "${REL_BADSUM}/SHA256SUMS"

# 4. Duplicate SHA256SUMS entry
REL_DUP="${RELEASES}/v0.5.0-rc.2"
make_tarball "${REL_DUP}" scion "scion-dup" amd64
dup_sum="$(sha256_file "${REL_DUP}/scion-linux-amd64.tar.gz")"
printf '%s  scion-linux-amd64.tar.gz\n%s  scion-linux-amd64.tar.gz\n' "${dup_sum}" "${dup_sum}" > "${REL_DUP}/SHA256SUMS"

# 5. Nightly release
REL_NIGHTLY="${RELEASES}/nightly-20261123"
make_tarball "${REL_NIGHTLY}" scion "scion-nightly" amd64
make_tarball "${REL_NIGHTLY}" sciontool "sciontool-nightly" amd64
(
  cd "${REL_NIGHTLY}"
  for f in *.tar.gz; do
    printf '%s  %s\n' "$(sha256_file "$f")" "$f"
  done > SHA256SUMS
)

# 6. Symlink inside tarball (must be rejected)
REL_SYM="${RELEASES}/v0.5.0-rc.3"
mkdir -p "${REL_SYM}" "${TMP}/stage-sym"
ln -s /etc/passwd "${TMP}/stage-sym/scion"
tar -czf "${REL_SYM}/scion-linux-amd64.tar.gz" -C "${TMP}/stage-sym" scion
printf '%s  scion-linux-amd64.tar.gz\n' "$(sha256_file "${REL_SYM}/scion-linux-amd64.tar.gz")" > "${REL_SYM}/SHA256SUMS"

OUT=""
RC=0
run_inst() {
  set +e
  OUT="$(bash "${INSTALLER}" "$@" 2>&1)"
  RC=$?
  set -e
}

# ---------------------------------------------------------------------------
# Happy paths
# ---------------------------------------------------------------------------
DEST1="${TMP}/dest1"
run_inst --base-url "${BASE_URL}" --tag v0.5.0-preview.1 --arch amd64 --dest "${DEST1}" scion sciontool
check_rc "full release amd64: exit 0" 0 "${RC}"
check_eq "full release amd64: scion works" "scion-v0.5.0-amd64" "$("${DEST1}/scion")"
check_eq "full release amd64: sciontool works" "sciontool-v0.5.0-amd64" "$("${DEST1}/sciontool")"

DEST2="${TMP}/dest2"
run_inst --base-url "${BASE_URL}" --tag v0.5.0-preview.1 --arch arm64 --dest "${DEST2}" scion
check_rc "single binary arm64: exit 0" 0 "${RC}"
check_eq "single binary arm64: scion works" "scion-v0.5.0-arm64" "$("${DEST2}/scion")"
if [ ! -e "${DEST2}/sciontool" ]; then
  pass "single binary arm64: sciontool not installed"
else
  fail "single binary arm64: sciontool should not be installed"
fi

DEST3="${TMP}/dest3"
run_inst --base-url "${BASE_URL}" --tag nightly-20261123 --arch amd64 --dest "${DEST3}" scion sciontool
check_rc "nightly tag: exit 0" 0 "${RC}"
check_eq "nightly tag: scion works" "scion-nightly" "$("${DEST3}/scion")"
check_eq "nightly tag: sciontool works" "sciontool-nightly" "$("${DEST3}/sciontool")"

# ---------------------------------------------------------------------------
# Error paths
# ---------------------------------------------------------------------------
DEST_OLD="${TMP}/dest-old"
run_inst --base-url "${BASE_URL}" --tag v0.4.0-preview.3 --arch amd64 --dest "${DEST_OLD}" scion sciontool
check_rc "old release missing sciontool: exit 1" 1 "${RC}"
check_contains "old release missing sciontool: mentions predates sciontool assets" "predates sciontool assets" "${OUT}"
check_contains "old release missing sciontool: suggests --binaries source" "--binaries source" "${OUT}"
if [ ! -e "${DEST_OLD}/scion" ]; then
  pass "old release missing sciontool: fails before installing scion"
else
  fail "old release missing sciontool: should not partially install scion"
fi

DEST_OLD_SCION="${TMP}/dest-old-scion"
run_inst --base-url "${BASE_URL}" --tag v0.4.0-preview.3 --arch amd64 --dest "${DEST_OLD_SCION}" scion
check_rc "old release scion-only (omni): exit 0" 0 "${RC}"
check_eq "old release scion-only: scion works" "scion-v0.4.0-amd64" "$("${DEST_OLD_SCION}/scion")"

DEST_OLD_ARM="${TMP}/dest-old-arm"
run_inst --base-url "${BASE_URL}" --tag v0.4.0-preview.3 --arch arm64 --dest "${DEST_OLD_ARM}" scion
check_rc "missing scion arch in SHA256SUMS: exit 1" 1 "${RC}"
check_contains "missing scion arch: names asset" "scion-linux-arm64.tar.gz" "${OUT}"

DEST_BADSUM="${TMP}/dest-badsum"
run_inst --base-url "${BASE_URL}" --tag v0.5.0-rc.1 --arch amd64 --dest "${DEST_BADSUM}" scion
check_rc "bad SHA256: exit 1" 1 "${RC}"
check_contains "bad SHA256: error message" "SHA256 verification failed" "${OUT}"
if [ ! -e "${DEST_BADSUM}/scion" ]; then
  pass "bad SHA256: nothing installed"
else
  fail "bad SHA256: scion should not be installed"
fi

DEST_DUP="${TMP}/dest-dup"
run_inst --base-url "${BASE_URL}" --tag v0.5.0-rc.2 --arch amd64 --dest "${DEST_DUP}" scion
check_rc "duplicate SHA256SUMS entry: exit 1" 1 "${RC}"
check_contains "duplicate SHA256SUMS entry: error message" "multiple SHA256SUMS entries" "${OUT}"

DEST_SYM="${TMP}/dest-sym"
run_inst --base-url "${BASE_URL}" --tag v0.5.0-rc.3 --arch amd64 --dest "${DEST_SYM}" scion
check_rc "symlink in tarball: exit 1" 1 "${RC}"
check_contains "symlink in tarball: error message" "did not contain regular file" "${OUT}"

run_inst --base-url "${BASE_URL}" --tag v9.9.9 --arch amd64 --dest "${TMP}/dest-missing" scion
check_rc "missing release SHA256SUMS: exit 1" 1 "${RC}"
check_contains "missing release SHA256SUMS: error message" "failed to download SHA256SUMS" "${OUT}"

# Validation errors
run_inst --tag latest --arch amd64 scion
check_rc "invalid tag 'latest': exit 1" 1 "${RC}"
check_contains "invalid tag 'latest': error message" "invalid release tag" "${OUT}"

run_inst --tag v0.5.0 --arch s390x scion
check_rc "invalid arch 's390x': exit 1" 1 "${RC}"
check_contains "invalid arch 's390x': error message" "unsupported --arch" "${OUT}"

run_inst --tag v0.5.0 --arch amd64 bogus
check_rc "invalid binary 'bogus': exit 1" 1 "${RC}"
check_contains "invalid binary 'bogus': error message" "unsupported binary" "${OUT}"

run_inst --arch amd64 scion
check_rc "missing --tag: exit 1" 1 "${RC}"
check_contains "missing --tag: error message" "--tag is required" "${OUT}"

echo ""
echo "${PASSES} passed, ${FAILS} failed"
[ "${FAILS}" -eq 0 ]
