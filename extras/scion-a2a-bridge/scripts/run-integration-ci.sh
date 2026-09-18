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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BRIDGE_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
RUNNER_TEMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/scion-a2a-integration.XXXXXX")"

cleanup() {
  rm -rf -- "${RUNNER_TEMP_DIR}"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

echo "=== A2A Bridge Deterministic PostgreSQL Integration Runner ==="

# Require TEST_DATABASE_URL; fails closed immediately if unset or empty
if [[ -z "${TEST_DATABASE_URL:-}" ]]; then
  echo "::error::TEST_DATABASE_URL environment variable is unset or empty. CI runner fails closed." >&2
  exit 1
fi
export TEST_REQUIRE_DATABASE="1"

echo "Using TEST_DATABASE_URL=[REDACTED]"
echo "TEST_REQUIRE_DATABASE=${TEST_REQUIRE_DATABASE} (fail-closed mode)"

# Verify psql binary is available; fails closed if missing
if ! command -v psql >/dev/null 2>&1; then
  echo "::error::psql is required for the CI entrypoint but was not found in PATH. Fails closed." >&2
  exit 1
fi

# Verify PostgreSQL connectivity via psql; fails closed if unreachable
if ! psql "${TEST_DATABASE_URL}" -c "SELECT 1;" >/dev/null 2>&1; then
  echo "::error::PostgreSQL is unreachable via psql or TEST_DATABASE_URL is invalid. Fails closed." >&2
  exit 1
fi
echo "PostgreSQL 15 connection verified via psql."

# Ensure test_canary schema and sentinel table exist without deleting or overwriting unrelated rows
psql "${TEST_DATABASE_URL}" -v ON_ERROR_STOP=1 >/dev/null 2>&1 <<'EOF'
CREATE SCHEMA IF NOT EXISTS test_canary;
CREATE TABLE IF NOT EXISTS test_canary.sentinel (
  id TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
INSERT INTO test_canary.sentinel (id, value)
VALUES ('canary-1', 'must-survive')
ON CONFLICT (id) DO NOTHING;
EOF

# Pre-test canary identity and value capture
CANARY_ID="canary-1"
EXPECTED_CANARY_VALUE="must-survive"

BEFORE_CANARY=$(psql "${TEST_DATABASE_URL}" -t -A -c "SELECT value FROM test_canary.sentinel WHERE id = '${CANARY_ID}';")
if [ "${BEFORE_CANARY}" != "${EXPECTED_CANARY_VALUE}" ]; then
  echo "::error::Canary sentinel '${CANARY_ID}' is invalid before tests (got '${BEFORE_CANARY}', want '${EXPECTED_CANARY_VALUE}'). Fails closed." >&2
  exit 1
fi
echo "Pre-test canary sentinel '${CANARY_ID}' verified: '${BEFORE_CANARY}'"

cd "${BRIDGE_DIR}"

run_test_phase() {
  local phase_name="$1"
  shift

  echo ""
  echo "=== Running ${phase_name}: go test $* ==="

  local json_log
  json_log="$(mktemp "${RUNNER_TEMP_DIR}/test-json.XXXXXX")"

  set +e
  go test -json "$@" > "${json_log}" 2>&1
  local test_status=$?
  set -e

  # 1. Non-zero test failure check
  if [ "${test_status}" -ne 0 ]; then
    echo "::error::${phase_name} failed with exit code ${test_status}." >&2
    grep '"Action":"output"' "${json_log}" | sed -n 's/.*"Output":"\([^"]*\)".*/\1/p' | sed 's/\\n/\n/g' >&2 || cat "${json_log}" >&2
    rm -f "${json_log}"
    exit "${test_status}"
  fi

  # 2. Check for Action=skip in test events
  local skipped_tests
  skipped_tests=$(grep -E '"Action":"skip"' "${json_log}" || true)

  if [ -n "${skipped_tests}" ]; then
    echo "::error::${phase_name} detected forbidden test skip(s):" >&2
    echo "${skipped_tests}" | while IFS= read -r line; do
      local test_name
      test_name=$(echo "${line}" | sed -n 's/.*"Test":"\([^"]*\)".*/\1/p')
      echo "  - SKIPPED: ${test_name}" >&2
    done
    rm -f "${json_log}"
    echo "::error::Runner fails closed on skipped tests in CI mode." >&2
    exit 1
  fi

  # Format summary
  grep '"Action":"output"' "${json_log}" | sed -n 's/.*"Output":"\([^"]*\)".*/\1/p' | sed 's/\\n/\n/g' | grep -E '^=== RUN|^--- PASS|^PASS|^ok' || true
  rm -f "${json_log}"
  echo "${phase_name} completed successfully with 0 skips and 0 failures."
}

# Phase 1: Standard Integration Suite
run_test_phase "Phase 1: Standard Integration Suite" -v ./integration

# Optional mutation injection hook for testable runner verification
if [ "${TEST_TRIGGER_CANARY_MUTATION:-0}" = "1" ]; then
  echo "TEST_TRIGGER_CANARY_MUTATION=1 active: mutating canary row for negative runner test"
  psql "${TEST_DATABASE_URL}" -c "UPDATE test_canary.sentinel SET value = 'mutated' WHERE id = '${CANARY_ID}';" >/dev/null 2>&1
fi

# Phase 2: Race Detection Integration Suite
run_test_phase "Phase 2: Race Detection Integration Suite" -race ./integration

# Phase 3: Repetition Stress Suite (count=3)
run_test_phase "Phase 3: Repetition Stress Suite (count=3)" -count=3 ./integration

# Phase 4: Canary Table Identity & Value Post-Verification
echo ""
echo "=== Phase 4: Canary Sentinel Post-Verification ==="
AFTER_CANARY=$(psql "${TEST_DATABASE_URL}" -t -A -c "SELECT value FROM test_canary.sentinel WHERE id = '${CANARY_ID}';")
if [ "${AFTER_CANARY}" != "${EXPECTED_CANARY_VALUE}" ]; then
  echo "::error::Canary sentinel '${CANARY_ID}' was mutated or deleted after test phases (got '${AFTER_CANARY}', want '${EXPECTED_CANARY_VALUE}'). Fails closed." >&2
  exit 1
fi
echo "Post-test canary sentinel '${CANARY_ID}' matches pre-test baseline: '${AFTER_CANARY}'"

echo ""
echo "All integration test phases completed successfully."
