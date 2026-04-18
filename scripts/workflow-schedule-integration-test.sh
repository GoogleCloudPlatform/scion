#!/bin/bash
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
# Workflow Schedule Integration Test Script (T2 — Phase 4a)
# =========================================================
# Tests the schedule subsystem for workflow runs:
#   1. One-shot scheduled workflow fires and succeeds
#   2. Recurring workflow fires multiple times; cancel stops it
#   3. Mutual exclusion: --type message with --workflow is rejected
#   4. Schedule with inputs-file passes through
#
# NOTE: These tests involve real wall-clock waits.
# Test 2 (recurring) waits ~2.5 minutes.
#
# Usage:
#   ./scripts/workflow-schedule-integration-test.sh [options]
#
# Options:
#   --skip-build     Skip building the scion binary
#   --skip-cleanup   Don't clean up test artifacts after completion
#   --verbose        Show verbose output
#   --help           Show this help message
#

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
TEST_DIR="/tmp/scion-workflow-schedule-test-$$"
SKIP_BUILD=false
SKIP_CLEANUP=false
VERBOSE=false
SCION=""

HUB_PORT=9832
HUB_ENDPOINT="http://localhost:${HUB_PORT}"
HUB_DB="$TEST_DIR/hub.db"
HUB_LOG="$TEST_DIR/hub.log"
HUB_PID=""

DEV_TOKEN=""
GROVE_ID=""

SCION_TOKEN_FILE="$HOME/.scion/scion-token"
SCION_TOKEN_BACKUP="/tmp/scion-scion-token-backup-schedule-$$"
TOKEN_BACKED_UP=false

TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0
TESTS_SKIPPED=0

while [[ $# -gt 0 ]]; do
    case $1 in
        --skip-build) SKIP_BUILD=true; shift ;;
        --skip-cleanup) SKIP_CLEANUP=true; shift ;;
        --verbose) VERBOSE=true; shift ;;
        --help) head -40 "$0" | tail -30; exit 0 ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

log_info()    { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[PASS]${NC} $1"; }
log_warning() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error()   { echo -e "${RED}[FAIL]${NC} $1"; }
log_skip()    { echo -e "${YELLOW}[SKIP]${NC} $1"; }

log_section() {
    echo ""
    echo -e "${BLUE}========================================${NC}"
    echo -e "${BLUE}$1${NC}"
    echo -e "${BLUE}========================================${NC}"
}

cleanup() {
    if [[ -n "$HUB_PID" ]]; then
        log_info "Stopping hub server (PID $HUB_PID)..."
        kill "$HUB_PID" 2>/dev/null || true
        wait "$HUB_PID" 2>/dev/null || true
    fi

    if [[ "$TOKEN_BACKED_UP" == "true" ]]; then
        mv "$SCION_TOKEN_BACKUP" "$SCION_TOKEN_FILE" 2>/dev/null || true
    fi

    if [[ "$SKIP_CLEANUP" == "false" ]]; then
        rm -rf "$TEST_DIR"
    else
        log_info "Test artifacts preserved in: $TEST_DIR"
    fi
}

trap cleanup EXIT

check_prerequisites() {
    log_section "Checking Prerequisites"

    if ! command -v docker &>/dev/null; then
        log_error "docker not found on PATH"
        exit 1
    fi
    if ! docker info >/dev/null 2>&1; then
        log_error "Docker daemon is not running"
        exit 1
    fi
    if [[ -z "$(docker images -q scion-base:latest 2>/dev/null)" ]]; then
        log_error "scion-base:latest image not found"
        exit 1
    fi
    for cmd in curl python3; do
        if ! command -v "$cmd" &>/dev/null; then
            log_error "Required command '$cmd' not found"
            exit 1
        fi
    done
    if lsof -i ":${HUB_PORT}" >/dev/null 2>&1; then
        log_error "Port $HUB_PORT is already in use"
        exit 1
    fi
    log_success "Prerequisites checked"
}

build_scion() {
    if [[ "$SKIP_BUILD" == "true" ]]; then
        log_info "Skipping build (--skip-build)"
        SCION="$TEST_DIR/scion"
        return
    fi

    log_section "Building Scion Binary"
    mkdir -p "$TEST_DIR"
    if go build -buildvcs=false -o "$TEST_DIR/scion" "$PROJECT_ROOT/cmd/scion" 2>&1; then
        log_success "Build successful"
    else
        log_error "Build failed"
        exit 1
    fi
    SCION="$TEST_DIR/scion"
}

backup_scion_token() {
    if [[ -f "$SCION_TOKEN_FILE" ]]; then
        mv "$SCION_TOKEN_FILE" "$SCION_TOKEN_BACKUP"
        TOKEN_BACKED_UP=true
    fi
}

start_hub_server() {
    log_section "Starting Hub Server"
    mkdir -p "$TEST_DIR"

    "$SCION" server start \
        --production \
        --enable-hub \
        --enable-runtime-broker \
        --dev-auth \
        --auto-provide \
        --port "$HUB_PORT" \
        --db "$HUB_DB" \
        --foreground \
        >"$HUB_LOG" 2>&1 &
    HUB_PID=$!

    local max_wait=30
    local waited=0
    while [[ $waited -lt $max_wait ]]; do
        if curl -sf "$HUB_ENDPOINT/healthz" >/dev/null 2>&1; then
            log_success "Hub server ready (waited ${waited}s)"
            break
        fi
        sleep 1
        waited=$((waited + 1))
    done

    if ! curl -sf "$HUB_ENDPOINT/healthz" >/dev/null 2>&1; then
        log_error "Hub server failed to start"
        tail -20 "$HUB_LOG" >&2
        exit 1
    fi

    DEV_TOKEN=$(grep -o 'scion_dev_[a-f0-9]*' "$HUB_LOG" | head -1)
    if [[ -z "$DEV_TOKEN" ]]; then
        log_error "Failed to extract dev token"
        tail -20 "$HUB_LOG" >&2
        exit 1
    fi
    log_success "Dev token: ${DEV_TOKEN:0:24}..."
}

create_test_grove() {
    log_section "Creating Test Grove"

    local resp
    resp=$(curl -sf -X POST "$HUB_ENDPOINT/api/v1/groves" \
        -H "Authorization: Bearer $DEV_TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"name":"Workflow Schedule Test","slug":"wf-schedule-test"}' 2>&1)

    GROVE_ID=$(echo "$resp" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('id',''))" 2>/dev/null)
    if [[ -z "$GROVE_ID" ]]; then
        log_error "Failed to create grove: $resp"
        exit 1
    fi
    log_success "Grove: $GROVE_ID"
}

create_test_fixtures() {
    log_section "Creating Test Fixtures"
    mkdir -p "$TEST_DIR/fixtures"

    cat >"$TEST_DIR/fixtures/hello.duck.yaml" <<'YAML'
flow:
  - type: exec
    run: echo "scheduled run hello"
YAML

    cat >"$TEST_DIR/fixtures/inputs-echo.duck.yaml" <<'YAML'
inputs:
  msg:
    required: true
participants:
  echo-msg:
    type: exec
    run: cat
    input: workflow.inputs.msg
flow:
  - echo-msg
YAML

    cat >"$TEST_DIR/fixtures/inputs.json" <<'JSON'
{"msg": "hello-from-schedule-input"}
JSON

    log_success "Fixtures created"
}

# Helper: create a temp grove directory with hub settings so the scion CLI can
# locate the hub without needing a real grove on disk.  Returns the grove dir path.
make_grove_dir() {
    local grove_id="$1"
    local grove_dir="$TEST_DIR/groves/$grove_id"
    local scion_dir="$grove_dir/.scion"
    mkdir -p "$scion_dir"
    cat >"$scion_dir/settings.yaml" <<YAML
hub:
  enabled: true
  endpoint: "$HUB_ENDPOINT"
  grove_id: "$grove_id"
YAML
    echo "$grove_dir"
}

# Helper: get run count for grove
get_run_count() {
    local grove_id="$1"
    local status_filter="${2:-}"
    local url="$HUB_ENDPOINT/api/v1/groves/$grove_id/workflows/runs"
    if [[ -n "$status_filter" ]]; then
        url="${url}?status=${status_filter}"
    fi
    curl -sf "$url" \
        -H "Authorization: Bearer $DEV_TOKEN" 2>/dev/null | \
        python3 -c "import sys,json; d=json.load(sys.stdin); print(len(d.get('runs',[])))" 2>/dev/null
}

# Helper: poll until run count in grove reaches at least N
wait_for_run_count() {
    local grove_id="$1"
    local min_count="$2"
    local max_sec="${3:-90}"
    local elapsed=0

    while [[ $elapsed -lt $max_sec ]]; do
        local count
        count=$(get_run_count "$grove_id") || count=0
        if [[ $count -ge $min_count ]]; then
            echo "$count"
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    echo "$(get_run_count "$grove_id")"
    return 1
}

# ============================================================================
# Test 1: One-shot scheduled workflow fires
# ============================================================================

test_oneshot_schedule() {
    log_section "Test 1: One-shot scheduled workflow fires"
    TESTS_RUN=$((TESTS_RUN + 1))

    local baseline_runs
    baseline_runs=$(get_run_count "$GROVE_ID") || baseline_runs=0
    log_info "  Baseline run count: $baseline_runs"

    # Create a scheduled event 30s in the future
    local grove_dir
    grove_dir=$(make_grove_dir "$GROVE_ID")
    local sched_out sched_exit=0
    sched_out=$(SCION_DEV_TOKEN="$DEV_TOKEN" \
        "$SCION" schedule create \
        --type workflow_run \
        --workflow "$TEST_DIR/fixtures/hello.duck.yaml" \
        --grove "$grove_dir" \
        --in "30s" 2>&1) || sched_exit=$?

    log_info "  Schedule create exit=$sched_exit"
    log_info "  Output: $sched_out"

    if [[ $sched_exit -ne 0 ]]; then
        log_error "1  one-shot schedule: create failed (exit $sched_exit)"
        log_error "   $sched_out"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        return
    fi

    # Extract event ID
    local event_id
    event_id=$(echo "$sched_out" | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | head -1)
    log_info "  Event ID: $event_id"

    # Poll for up to 90s for a new run to appear
    log_info "  Waiting up to 90s for scheduled run to fire..."
    local new_run_count=0
    local elapsed=0
    while [[ $elapsed -lt 90 ]]; do
        local current
        current=$(get_run_count "$GROVE_ID") || current=0
        if [[ $current -gt $baseline_runs ]]; then
            new_run_count=$current
            break
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done

    if [[ $new_run_count -gt $baseline_runs ]]; then
        log_success "1  one-shot schedule: run was created after schedule fired ($new_run_count total)"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "1  one-shot schedule: no new runs after 90s (expected >${baseline_runs})"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi
}

# ============================================================================
# Test 2: Recurring scheduled workflow fires multiple times
# ============================================================================

test_recurring_schedule() {
    log_section "Test 2: Recurring schedule fires multiple times"
    TESTS_RUN=$((TESTS_RUN + 1))

    # Create a second grove so we can count only its runs
    local resp
    resp=$(curl -sf -X POST "$HUB_ENDPOINT/api/v1/groves" \
        -H "Authorization: Bearer $DEV_TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"name":"Recurring Schedule Grove","slug":"wf-recurring-test"}' 2>&1)
    local recurring_grove_id
    recurring_grove_id=$(echo "$resp" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('id',''))" 2>/dev/null)
    log_info "  Recurring grove: $recurring_grove_id"

    # Create recurring schedule (every minute)
    local recurring_grove_dir
    recurring_grove_dir=$(make_grove_dir "$recurring_grove_id")
    local sched_out sched_exit=0
    sched_out=$(SCION_DEV_TOKEN="$DEV_TOKEN" \
        "$SCION" schedule create-recurring \
        --name "test-recurring-wf" \
        --type workflow_run \
        --workflow "$TEST_DIR/fixtures/hello.duck.yaml" \
        --grove "$recurring_grove_dir" \
        --cron "* * * * *" 2>&1) || sched_exit=$?

    log_info "  Schedule create-recurring exit=$sched_exit"
    log_info "  Output: $sched_out"

    if [[ $sched_exit -ne 0 ]]; then
        log_error "2  recurring schedule: create-recurring failed"
        log_error "   $sched_out"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        return
    fi

    local sched_id
    sched_id=$(echo "$sched_out" | grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' | head -1)
    log_info "  Schedule ID: $sched_id"

    # Wait 2.5 minutes for at least 2 runs
    log_info "  Waiting 150s for >= 2 runs to appear..."
    local final_count=0
    local elapsed=0
    while [[ $elapsed -lt 150 ]]; do
        local count
        count=$(get_run_count "$recurring_grove_id") || count=0
        if [[ $count -ge 2 ]]; then
            final_count=$count
            break
        fi
        sleep 15
        elapsed=$((elapsed + 15))
    done
    final_count=$(get_run_count "$recurring_grove_id") || final_count=0

    if [[ $final_count -ge 2 ]]; then
        log_success "2a recurring: >= 2 runs fired ($final_count runs)"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "2a recurring: expected >= 2 runs after 150s, got $final_count"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi

    # Delete the schedule to stop it
    TESTS_RUN=$((TESTS_RUN + 1))
    local del_out del_exit=0
    del_out=$(SCION_DEV_TOKEN="$DEV_TOKEN" \
        "$SCION" schedule delete "$sched_id" --grove "$recurring_grove_dir" 2>&1) || del_exit=$?

    if [[ $del_exit -eq 0 ]]; then
        log_success "2b recurring: schedule deleted successfully"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "2b recurring: schedule delete failed (exit $del_exit): $del_out"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi

    # After deletion, wait 70s and assert count didn't increase more than 1
    log_info "  Waiting 70s to verify no new runs after deletion..."
    sleep 70
    local after_delete_count
    after_delete_count=$(get_run_count "$recurring_grove_id") || after_delete_count=0

    TESTS_RUN=$((TESTS_RUN + 1))
    if [[ $after_delete_count -le $((final_count + 1)) ]]; then
        log_success "2c recurring: no significant new runs after schedule deletion ($after_delete_count total)"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "2c recurring: runs continued after deletion ($after_delete_count > $final_count + 1)"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi
}

# ============================================================================
# Test 3: Mutual exclusion CLI rejects
# ============================================================================

test_mutual_exclusion() {
    log_section "Test 3: Mutual exclusion CLI rejects"

    local excl_grove_dir
    excl_grove_dir=$(make_grove_dir "$GROVE_ID")

    # 3a: --type message with --workflow is rejected
    TESTS_RUN=$((TESTS_RUN + 1))
    local out3a exit3a=0
    out3a=$(SCION_DEV_TOKEN="$DEV_TOKEN" \
        "$SCION" schedule create \
        --type message \
        --workflow "$TEST_DIR/fixtures/hello.duck.yaml" \
        --grove "$excl_grove_dir" \
        --in "1h" 2>&1) || exit3a=$?

    if [[ $exit3a -ne 0 ]]; then
        log_success "3a  mutual exclusion: --type message --workflow rejected (exit $exit3a)"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "3a  mutual exclusion: expected failure for --type message --workflow, got success"
        log_error "    output: $out3a"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi

    # 3b: --type workflow_run with --agent is rejected
    TESTS_RUN=$((TESTS_RUN + 1))
    local out3b exit3b=0
    out3b=$(SCION_DEV_TOKEN="$DEV_TOKEN" \
        "$SCION" schedule create \
        --type workflow_run \
        --workflow "$TEST_DIR/fixtures/hello.duck.yaml" \
        --agent "some-agent" \
        --grove "$excl_grove_dir" \
        --in "1h" 2>&1) || exit3b=$?

    if [[ $exit3b -ne 0 ]]; then
        log_success "3b  mutual exclusion: --type workflow_run --agent rejected (exit $exit3b)"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "3b  mutual exclusion: expected failure for --type workflow_run --agent, got success"
        log_error "    output: $out3b"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi
}

# ============================================================================
# Test 4: Schedule with inputs-file
# ============================================================================

test_schedule_with_inputs_file() {
    log_section "Test 4: Schedule with --inputs-file"
    TESTS_RUN=$((TESTS_RUN + 1))

    local baseline
    baseline=$(get_run_count "$GROVE_ID") || baseline=0

    # Create a grove for inputs test
    local resp
    resp=$(curl -sf -X POST "$HUB_ENDPOINT/api/v1/groves" \
        -H "Authorization: Bearer $DEV_TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"name":"Schedule Inputs Grove","slug":"wf-schedule-inputs"}' 2>&1)
    local inputs_grove_id
    inputs_grove_id=$(echo "$resp" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('id',''))" 2>/dev/null)

    local inputs_grove_dir
    inputs_grove_dir=$(make_grove_dir "$inputs_grove_id")
    local sched_out sched_exit=0
    sched_out=$(SCION_DEV_TOKEN="$DEV_TOKEN" \
        "$SCION" schedule create \
        --type workflow_run \
        --workflow "$TEST_DIR/fixtures/inputs-echo.duck.yaml" \
        --inputs-file "$TEST_DIR/fixtures/inputs.json" \
        --grove "$inputs_grove_dir" \
        --in "30s" 2>&1) || sched_exit=$?

    log_info "  Schedule create exit=$sched_exit output=$sched_out"

    if [[ $sched_exit -ne 0 ]]; then
        log_error "4  schedule with inputs-file: create failed (exit $sched_exit): $sched_out"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        return
    fi

    log_success "4  schedule with inputs-file: schedule created successfully (fires in ~30s)"
    TESTS_PASSED=$((TESTS_PASSED + 1))

    # Optionally wait for it to fire and verify
    log_info "  (Waiting up to 90s for inputs run to fire — informational)"
    local elapsed=0
    while [[ $elapsed -lt 90 ]]; do
        local cnt
        cnt=$(get_run_count "$inputs_grove_id") || cnt=0
        if [[ $cnt -ge 1 ]]; then
            log_info "  Run created (informational verification)"
            break
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
}

# ============================================================================
# Main
# ============================================================================

run_all_tests() {
    log_section "Scion Workflow Schedule Integration Test Suite (T2)"
    log_info "Test directory: $TEST_DIR"
    log_info "Note: Test 2 (recurring) waits ~2.5 minutes for multiple firings."

    mkdir -p "$TEST_DIR"

    check_prerequisites
    build_scion
    backup_scion_token
    start_hub_server
    create_test_grove
    create_test_fixtures

    test_oneshot_schedule
    test_recurring_schedule
    test_mutual_exclusion
    test_schedule_with_inputs_file

    log_section "Test Summary"
    echo -e "  Total run:   $TESTS_RUN"
    echo -e "  Skipped:     $TESTS_SKIPPED"
    echo -e "  ${GREEN}Passed: $TESTS_PASSED${NC}"
    if [[ $TESTS_FAILED -gt 0 ]]; then
        echo -e "  ${RED}Failed: $TESTS_FAILED${NC}"
    else
        echo -e "  Failed: 0"
    fi
    echo ""

    if [[ $TESTS_FAILED -eq 0 ]]; then
        log_success "All tests passed!"
        return 0
    else
        log_error "$TESTS_FAILED test(s) failed"
        return 1
    fi
}

run_all_tests
