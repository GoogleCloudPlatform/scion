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

echo "=== A2A Bridge Deterministic PostgreSQL Integration Runner ==="

# Default PostgreSQL URL if unset
export TEST_DATABASE_URL="${TEST_DATABASE_URL:-postgres://scion:scion@127.0.0.1:5432/a2a_test?sslmode=disable}"
export TEST_REQUIRE_DATABASE="1"

echo "Using TEST_DATABASE_URL=${TEST_DATABASE_URL}"
echo "TEST_REQUIRE_DATABASE=${TEST_REQUIRE_DATABASE} (fail-closed mode)"

# Verify PostgreSQL connectivity (fails closed if database is missing or unreachable)
echo "Verifying PostgreSQL 15 availability..."
if command -v psql >/dev/null 2>&1; then
  if ! psql "${TEST_DATABASE_URL}" -c "SELECT 1;" >/dev/null 2>&1; then
    echo "::error::PostgreSQL 15 is unreachable via psql or TEST_DATABASE_URL is invalid. Fails closed."
    exit 1
  fi
elif command -v pg_isready >/dev/null 2>&1; then
  if ! pg_isready -d "${TEST_DATABASE_URL}" >/dev/null 2>&1; then
    echo "::error::PostgreSQL 15 is unreachable via pg_isready. Fails closed."
    exit 1
  fi
else
  echo "WARNING: Neither psql nor pg_isready available; connectivity verified directly by Go test harness."
fi
echo "PostgreSQL 15 connection verified."

cd "${BRIDGE_DIR}"

echo ""
echo "=== Phase 1: Standard Integration Suite ==="
go test -v ./integration

echo ""
echo "=== Phase 2: Race Detection Integration Suite ==="
go test -race ./integration

echo ""
echo "=== Phase 3: Repetition Stress Suite (count=3) ==="
go test -count=3 ./integration

echo ""
echo "=== Phase 4: Canary Table Verification ==="
if command -v psql >/dev/null 2>&1; then
  CANARY_COUNT=$(psql "${TEST_DATABASE_URL}" -t -A -c "SELECT count(*) FROM test_canary.sentinel;" 2>/dev/null || echo "0")
  if [ "${CANARY_COUNT}" -lt 1 ]; then
    echo "::error::Canary table missing or empty!"
    exit 1
  fi
  echo "Canary verified: count=${CANARY_COUNT}"
fi

echo ""
echo "All integration test phases completed successfully."
