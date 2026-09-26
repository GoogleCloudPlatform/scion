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
# at the front of PATH, sources tests/lib/harness.sh once for shared
# fixture and assertion helpers, then sources every tests/test_*.sh file
# (sorted) -- each in its own subshell -- and calls each test_* function
# it finds. Test files that drive deploy.sh itself run it as a real
# subprocess against the same stub; see README.md for how to add a new
# stub case, fixture, and test file.
#
# ISOLATION: each test file is sourced into its own subshell, not into
# run.sh's own shell. A file-level global or helper (e.g. HUB,
# run_deploy_create) is therefore private to the file that defines it --
# two files defining the same name never clobber each other, and there is
# no ordering dependency between files. See README.md "How it works" for
# the mechanics and for what this does and doesn't isolate.
#
# Requires python3: the stub's own JSON fixtures (see tests/lib/gcloud)
# are built and parsed with it. jq is optional -- deploy.sh shells out to
# it, when available, only to parse a GitHub Releases response for its
# default VERSION, and every test in this suite passes --version to skip
# that path.
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

if ! command -v python3 &>/dev/null; then
  echo "run.sh: python3 is required (the stub gcloud and its fixture helpers parse/build JSON with it) but was not found on PATH" >&2
  exit 1
fi

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

# Discover every test file in this directory (sorted, for a deterministic
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

# Every test_* name claimed so far, across all files -- one per line.
# Populated by each file's own subshell below (a real file on disk, not a
# shell variable, since a subshell can't write back into its parent's
# variables) and checked before that same subshell trusts one of its own
# test names, so two files defining the same test_* name is a loud
# failure instead of one of them silently disappearing.
CLAIMED_TEST_NAMES_FILE="$(mktemp)"

TOTAL_PASS=0
TOTAL_FAIL=0
TOTAL_TEST_COUNT=0

for TEST_FILE in "${TEST_FILES[@]}"; do
  FILE_RESULT_FILE="$(mktemp)"
  (
    # Everything this test file defines at source time -- HUB, DEPLOY_SH,
    # run_deploy_create, or any other file-level global/helper -- lives
    # only in this subshell and the test-level subshells it forks below.
    # It never reaches run.sh's own shell or any other file's subshell,
    # so two files are free to reuse the same names (as #1900's
    # test_deploy_wiring.sh and this harness's own test_deploy_base.sh
    # both do, for HUB and INSTANCE_NAME) without one clobbering the
    # other. harness.sh's shared helpers (assert_*, fresh_gcloud_state,
    # gcloud_log, ...) are already in scope here, inherited from run.sh's
    # own shell at the point this subshell forked.
    # `source` on a file with a syntax error (a leftover merge-conflict
    # marker is the realistic case, with three PRs rebasing onto this
    # file) prints to stderr and abandons the rest of the file -- bash
    # does not treat that as fatal, and `set -e` is off in this script on
    # purpose (see the top-of-file comment). Left unchecked, every test_*
    # after the bad line simply never gets defined, and this subshell
    # would still write a result file with whatever ran before the error,
    # so the file's own testing looks like a normal (partial) pass instead
    # of the loud failure below. Check the file parses *before* sourcing
    # it, not just handle a bad exit status after. "$BASH" (not a bare
    # `bash`) so the syntax check uses the exact interpreter that will
    # `source` the file below, not whatever `bash` resolves to first on
    # PATH.
    if ! "$BASH" -n "$TEST_FILE" 2>&1; then
      echo "CRASH [$(basename "$TEST_FILE")]: syntax error -- this file was not sourced, so none of its tests ran"
      exit 1
    fi
    mapfile -t BEFORE_NAMES < <(declare -F | awk '{print $3}' | grep '^test_' | sort)
    # shellcheck disable=SC1090
    source "$TEST_FILE"
    mapfile -t AFTER_NAMES < <(declare -F | awk '{print $3}' | grep '^test_' | sort)
    mapfile -t FILE_TEST_NAMES < <(comm -13 <(printf '%s\n' "${BEFORE_NAMES[@]}") <(printf '%s\n' "${AFTER_NAMES[@]}"))

    FILE_PASS=0
    FILE_FAIL=0
    for CURRENT_TEST in "${FILE_TEST_NAMES[@]}"; do
      if grep -qxF "$CURRENT_TEST" "$CLAIMED_TEST_NAMES_FILE" 2>/dev/null; then
        echo "FAIL [${CURRENT_TEST}]: defined by more than one tests/test_*.sh file (already claimed by an earlier one) -- rename one of them"
        FILE_FAIL=$((FILE_FAIL + 1))
        continue
      fi
      echo "$CURRENT_TEST" >> "$CLAIMED_TEST_NAMES_FILE"

      # Each test additionally runs in its own subshell: a test_*
      # function that unexpectedly hits an unhandled `exit` (a bug, since
      # only run_expect_fail cases are supposed to do that) then only
      # ends that one subshell, not this file's subshell or run.sh
      # itself, so the remaining tests -- in this file and every other
      # one -- still get a chance to run.
      RESULT_FILE="$(mktemp)"
      (
        # Cleans up this one test's own fresh_gcloud_state entries the
        # moment this subshell exits, on any path -- normal return, a
        # test's own early `exit` (the CRASH case below), or a signal.
        # This is deliberately systematic rather than relying on each
        # test remembering its own cleanup: without a trap here, each
        # test's mktemp entries only get removed in bulk when the whole
        # run's RUN_TMPDIR is torn down at the very end, so anyone
        # inspecting TMPDIR mid-run (or a run that never reaches its own
        # exit, killed from outside) would still see the full
        # accumulation.
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
      FILE_PASS=$((FILE_PASS + PASS_COUNT))
      FILE_FAIL=$((FILE_FAIL + FAIL_COUNT))
      rm -f "$RESULT_FILE"
    done

    {
      echo "FILE_PASS=${FILE_PASS}"
      echo "FILE_FAIL=${FILE_FAIL}"
      echo "FILE_TEST_COUNT=${#FILE_TEST_NAMES[@]}"
    } > "$FILE_RESULT_FILE"
  )
  FILE_SUBSHELL_RC=$?
  if [[ -s "$FILE_RESULT_FILE" ]]; then
    # shellcheck disable=SC1090
    source "$FILE_RESULT_FILE"
  else
    echo "CRASH [$(basename "$TEST_FILE")]: test file's own subshell exited with status ${FILE_SUBSHELL_RC} before reporting any result -- if the syntax-error message above named this file, that's the cause; otherwise an unguarded top-level \`exit\` (or a signal) ran at source time, before any of this file's own tests, not inside one of them"
    FILE_PASS=0
    FILE_FAIL=1
    FILE_TEST_COUNT=0
  fi
  TOTAL_PASS=$((TOTAL_PASS + FILE_PASS))
  TOTAL_FAIL=$((TOTAL_FAIL + FILE_FAIL))
  TOTAL_TEST_COUNT=$((TOTAL_TEST_COUNT + FILE_TEST_COUNT))
  rm -f "$FILE_RESULT_FILE"
done
rm -f "$CLAIMED_TEST_NAMES_FILE"

echo ""
echo "deploy.sh gcloud-stub tests: ${TOTAL_PASS} passed, ${TOTAL_FAIL} failed (of $((TOTAL_PASS + TOTAL_FAIL)) assertions across ${TOTAL_TEST_COUNT} tests)."

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
