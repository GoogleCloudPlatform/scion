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

# scripts/single-node-vm/tests/run.sh — runs every test_*.sh file in this
# directory against a stub `gcloud`.
#
# This never contacts GCP: it puts tests/lib (containing a stub `gcloud`)
# at the front of PATH, sources tests/lib/harness.sh for shared fixture
# and assertion helpers, then sources every tests/test_*.sh file (sorted)
# and calls each test_* function it finds. Test files that drive deploy.sh
# itself run it as a real subprocess against the same stub; see
# README.md for how to add a new stub case and fixture.
#
# Requires python3 and jq: the stub's own JSON fixtures are built with
# python3 (see tests/lib/gcloud), and deploy.sh itself shells out to jq,
# when available, to parse a GitHub Releases response for its default
# VERSION -- tests that don't pass --version exercise that path.
#
# Requires bash >= 4 (uses `mapfile` and associative arrays). This is a
# dev-only test runner, not a deployment artifact: deploy.sh itself
# targets bash 3.2+ (macOS's shipped /bin/bash), but this runner does not
# need to, and is not the vehicle for verifying that support.
#
# Usage:
#   ./run.sh

# Deliberately no -e: assertions and helpers here routinely run commands
# (e.g. `grep -c` on a log with zero matches) whose non-zero exit is
# expected and meaningful, not a script error. -e would abort the whole
# suite on the first one instead of reporting a normal pass/fail count.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TIER_DIR="$(dirname "$SCRIPT_DIR")"

# The stub `gcloud` must resolve before any real one on the operator's
# machine.
export PATH="${SCRIPT_DIR}/lib:${PATH}"
export TIER_DIR

# Every mktemp/mktemp -d call anywhere in this run -- fresh_gcloud_state's
# own two per-test entries, each test's own RESULT_FILE below, and any ad
# hoc mktemp inside an individual test -- lands under TMPDIR. Pointing
# TMPDIR at a fresh, run-scoped directory instead of the caller's ambient
# one (or /tmp) means the whole run's footprint is bounded to this one
# directory and is removed in one shot on exit, including on a crash (the
# trap fires on any exit path), rather than accumulating in the caller's
# real temporary directory across every run the caller ever makes.
RUN_TMPDIR="$(mktemp -d)"
export TMPDIR="$RUN_TMPDIR"
trap 'rm -rf "$RUN_TMPDIR"' EXIT

# shellcheck source=scripts/single-node-vm/tests/lib/harness.sh
source "${SCRIPT_DIR}/lib/harness.sh"

# Source every test file in this directory (sorted, for a deterministic
# run order), not a hardcoded list -- multiple stub-case PRs each add
# their own tests/test_*.sh file on top of this harness, and a hardcoded
# source list here would be an add/add conflict point between them.
shopt -s nullglob
TEST_FILES=("${SCRIPT_DIR}"/test_*.sh)
shopt -u nullglob
if [[ "${#TEST_FILES[@]}" -eq 0 ]]; then
  echo "run.sh: no tests/test_*.sh files found in ${SCRIPT_DIR}" >&2
  exit 1
fi
for TEST_FILE in "${TEST_FILES[@]}"; do
  # shellcheck disable=SC1090
  source "$TEST_FILE"
done

mapfile -t TEST_NAMES < <(declare -F | awk '{print $3}' | grep '^test_' | sort)

TOTAL_PASS=0
TOTAL_FAIL=0

# Each test runs in its own subshell: a test_* function that unexpectedly
# hits an unhandled `exit` (a bug, since only run_expect_fail cases are
# supposed to do that) then only ends that one subshell, not the whole
# run.sh process, so the remaining tests still get a chance to run and
# the suite still reports a final pass/fail count instead of dying
# silently partway through.
for CURRENT_TEST in "${TEST_NAMES[@]}"; do
  RESULT_FILE="$(mktemp)"
  (
    # Cleans up this one test's own fresh_gcloud_state entries the moment
    # this subshell exits, on any path -- normal return, a test's own
    # early `exit` (the CRASH case above), or a signal. This is
    # deliberately systematic rather than relying on each test
    # remembering its own cleanup: without a trap here, each test's
    # mktemp entries only get removed in bulk when the whole run's
    # RUN_TMPDIR is torn down at the very end, so anyone inspecting
    # TMPDIR mid-run (or a run that never reaches its own exit, killed
    # from outside) would still see the full accumulation.
    trap 'rm -rf "${GCLOUD_STUB_STATE_DIR:-}"; rm -f "${GCLOUD_STUB_LOG:-}"' EXIT
    PASS_COUNT=0
    FAIL_COUNT=0
    "$CURRENT_TEST"
    {
      echo "PASS_COUNT=${PASS_COUNT}"
      echo "FAIL_COUNT=${FAIL_COUNT}"
    } > "$RESULT_FILE"
  )
  SUBSHELL_RC=$?
  if [[ -s "$RESULT_FILE" ]]; then
    # shellcheck disable=SC1090
    source "$RESULT_FILE"
  else
    echo "CRASH [${CURRENT_TEST}]: test subshell exited with status ${SUBSHELL_RC} before reporting any result"
    PASS_COUNT=0
    FAIL_COUNT=1
  fi
  TOTAL_PASS=$((TOTAL_PASS + PASS_COUNT))
  TOTAL_FAIL=$((TOTAL_FAIL + FAIL_COUNT))
  rm -f "$RESULT_FILE"
done

echo ""
echo "deploy.sh gcloud-stub tests: ${TOTAL_PASS} passed, ${TOTAL_FAIL} failed (of $((TOTAL_PASS + TOTAL_FAIL)) assertions across ${#TEST_NAMES[@]} tests)."

# Self-check: every test's own EXIT trap above should have already
# removed its fresh_gcloud_state entries, and every RESULT_FILE is
# removed right after it's read, so nothing should be left in TMPDIR at
# all at this point -- not just "cleaned up eventually" by RUN_TMPDIR's
# own EXIT trap once this process ends. A leftover entry here means some
# test (or a helper it calls) made a temp file/dir this harness doesn't
# know to clean up per-test, which is exactly the kind of gap that would
# otherwise let a leak go undetected.
LEFTOVER_TMP_COUNT="$(find "$TMPDIR" -mindepth 1 2>/dev/null | wc -l | tr -d ' ')"
if [[ "$LEFTOVER_TMP_COUNT" -ne 0 ]]; then
  echo "WARNING: ${LEFTOVER_TMP_COUNT} entries remain in \$TMPDIR (${TMPDIR}) after every test's own cleanup ran -- something is leaking outside the per-test EXIT trap above:"
  find "$TMPDIR" -mindepth 1 2>/dev/null | sed 's/^/  /'
  TOTAL_FAIL=$((TOTAL_FAIL + 1))
fi

if [[ "$TOTAL_FAIL" -gt 0 ]]; then
  exit 1
fi
