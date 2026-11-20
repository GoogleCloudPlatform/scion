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

# Unit tests for image-build/scripts/verify-registry.sh error classification
# (PERMISSION_DENIED vs NOT_FOUND vs generic describe error vs missing push permission).
# Uses local gcloud/curl stubs; requires no network access or live GCP project.
#
# Usage: bash image-build/scripts/test-verify-registry.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERIFY="${SCRIPT_DIR}/verify-registry.sh"
[[ -f "${VERIFY}" ]] || { echo "verify-registry.sh not found at ${VERIFY}" >&2; exit 2; }

TMP="$(mktemp -d "${TMPDIR:-/tmp}/test-verify-registry.XXXXXX")"
trap 'rm -rf -- "${TMP:?}"' EXIT

PASSES=0
FAILS=0

pass() { PASSES=$((PASSES + 1)); echo "ok   - $1"; }
fail() {
  FAILS=$((FAILS + 1))
  echo "FAIL - $1" >&2
  if [[ $# -gt 1 ]]; then
    printf '       %s\n' "${@:2}" >&2
  fi
}

assert_eq() {
  local name="$1" got="$2" want="$3"
  if [[ "${got}" == "${want}" ]]; then
    pass "${name}"
  else
    fail "${name}" "want: ${want}" "got:  ${got}"
  fi
}

assert_contains() {
  local name="$1" haystack="$2" needle="$3"
  if [[ "${haystack}" == *"${needle}"* ]]; then
    pass "${name}"
  else
    fail "${name}" "missing substring: '${needle}'" "output: ${haystack}"
  fi
}

assert_not_contains() {
  local name="$1" haystack="$2" needle="$3"
  if [[ "${haystack}" != *"${needle}"* ]]; then
    pass "${name}"
  else
    fail "${name}" "unexpected substring: '${needle}'" "output: ${haystack}"
  fi
}

mkdir -p "${TMP}/bin"

cat >"${TMP}/bin/gcloud" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${1:-}" == "auth" && "${2:-}" == "print-access-token" ]]; then
  echo "stub-token"
  exit 0
fi
if [[ "${1:-}" == "artifacts" && "${2:-}" == "repositories" && "${3:-}" == "describe" ]]; then
  case "${MOCK_DESCRIBE_MODE:-ok}" in
    ok)
      echo "projects/my-proj/locations/us-central1/repositories/cdha-scion"
      exit 0
      ;;
    permission_denied)
      echo "ERROR: (gcloud.artifacts.repositories.describe) PERMISSION_DENIED: Permission 'artifactregistry.repositories.get' denied on resource '//artifactregistry.googleapis.com/projects/my-proj/locations/us-central1/repositories/cdha-scion'." >&2
      exit 1
      ;;
    http_403)
      echo "ERROR: (gcloud.artifacts.repositories.describe) HTTPError 403: Caller does not have permission." >&2
      exit 1
      ;;
    not_found)
      echo "ERROR: (gcloud.artifacts.repositories.describe) NOT_FOUND: Repository 'cdha-scion' was not found." >&2
      exit 1
      ;;
    network_error)
      echo "ERROR: (gcloud.artifacts.repositories.describe) Could not reach Artifact Registry API: connection timed out" >&2
      exit 1
      ;;
  esac
fi
echo "unexpected gcloud call: $*" >&2
exit 99
EOF
chmod +x "${TMP}/bin/gcloud"

cat >"${TMP}/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
if [[ "${MOCK_UPLOAD_PERM:-yes}" == "yes" ]]; then
  echo '{"permissions":["artifactregistry.repositories.uploadArtifacts"]}'
else
  echo '{}'
fi
EOF
chmod +x "${TMP}/bin/curl"

REG="us-central1-docker.pkg.dev/my-proj/cdha-scion"

run_verify() {
  RC=0
  OUT="$(env "PATH=${TMP}/bin:${PATH}" "$@" bash "${VERIFY}" "${REG}" 2>&1)" || RC=$?
}

# 1. Success when repository exists and uploadArtifacts is granted
run_verify MOCK_DESCRIBE_MODE=ok MOCK_UPLOAD_PERM=yes
assert_eq "valid repo + push permission exits 0" "${RC}" "0"
assert_contains "valid repo prints verified message" "${OUT}" "Registry access verified: ${REG}"

# 2. PERMISSION_DENIED on describe is reported as permissions error, NOT repository not found
run_verify MOCK_DESCRIBE_MODE=permission_denied
assert_eq "PERMISSION_DENIED on describe exits 1" "${RC}" "1"
assert_contains "PERMISSION_DENIED reports permission error" "${OUT}" "Permission denied accessing Artifact Registry repository"
assert_not_contains "PERMISSION_DENIED does not claim repository not found" "${OUT}" "repository not found"

# 3. HTTPError 403 on describe is reported as permissions error, NOT repository not found
run_verify MOCK_DESCRIBE_MODE=http_403
assert_eq "HTTPError 403 on describe exits 1" "${RC}" "1"
assert_contains "HTTPError 403 reports permission error" "${OUT}" "Permission denied accessing Artifact Registry repository"
assert_not_contains "HTTPError 403 does not claim repository not found" "${OUT}" "repository not found"

# 4. Real NOT_FOUND on describe is reported as repository not found
run_verify MOCK_DESCRIBE_MODE=not_found
assert_eq "NOT_FOUND on describe exits 1" "${RC}" "1"
assert_contains "NOT_FOUND reports repository not found" "${OUT}" "Artifact Registry repository not found"
assert_not_contains "NOT_FOUND does not report permission denied" "${OUT}" "Permission denied"

# 5. Generic/network error on describe reports describe failure (not not-found)
run_verify MOCK_DESCRIBE_MODE=network_error
assert_eq "network error on describe exits 1" "${RC}" "1"
assert_contains "network error reports failed to describe" "${OUT}" "Failed to describe Artifact Registry repository"
assert_not_contains "network error does not claim repository not found" "${OUT}" "repository not found"

# 6. Missing uploadArtifacts permission in testIamPermissions reports missing push permission
run_verify MOCK_DESCRIBE_MODE=ok MOCK_UPLOAD_PERM=no
assert_eq "missing uploadArtifacts exits 1" "${RC}" "1"
assert_contains "missing uploadArtifacts reports missing push permission" "${OUT}" "Missing push permission"

# 7. Syntax check
if bash -n "${VERIFY}"; then
  pass "bash -n syntax clean: verify-registry.sh"
else
  fail "bash -n syntax error: verify-registry.sh"
fi

echo ""
echo "${PASSES} passed, ${FAILS} failed"
[[ "${FAILS}" -eq 0 ]]
