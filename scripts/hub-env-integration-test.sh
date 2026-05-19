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
# Hub Environment Variables Integration Test Script
# ===================================================
# This script tests the full hub env storage feature by exercising the
# scion CLI commands for setting, getting, listing, and clearing
# environment variables at user and grove scopes.
#
# It starts a Hub server with dev auth, links a test grove, and runs
# the complete set of env CRUD operations.
#
# Usage:
#   ./scripts/hub-env-integration-test.sh [options]
#
# Options:
#   --skip-build     Skip building the scion binary
#   --skip-cleanup   Don't clean up test artifacts after completion
#   --verbose        Show verbose output
#   --help           Show this help message
#

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
TEST_DIR="/tmp/scion-hub-env-test-$$"
HUB_PORT=9820
SKIP_BUILD=false
SKIP_CLEANUP=false
VERBOSE=false
SCION=""
SERVER_PID=""

# Test counters
TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --skip-build)
            SKIP_BUILD=true
            shift
            ;;
        --skip-cleanup)
            SKIP_CLEANUP=true
            shift
            ;;
        --verbose)
            VERBOSE=true
            shift
            ;;
        --help)
            head -36 "$0" | tail -31
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# ============================================================================
# Logging
# ============================================================================

log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[PASS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARN]${NC} $1"
}

log_error() {
    echo -e "${RED}[FAIL]${NC} $1"
}

log_section() {
    echo ""
    echo -e "${BLUE}========================================${NC}"
    echo -e "${BLUE}$1${NC}"
    echo -e "${BLUE}========================================${NC}"
}

# ============================================================================
# Test assertion helpers
# ============================================================================

assert_success() {
    local description="$1"
    shift
    TESTS_RUN=$((TESTS_RUN + 1))
    if "$@" > "$TEST_DIR/last_stdout" 2> "$TEST_DIR/last_stderr"; then
        log_success "$description"
        TESTS_PASSED=$((TESTS_PASSED + 1))
        return 0
    else
        log_error "$description"
        log_error "  stdout: $(cat "$TEST_DIR/last_stdout")"
        log_error "  stderr: $(cat "$TEST_DIR/last_stderr")"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        return 1
    fi
}

assert_failure() {
    local description="$1"
    shift
    TESTS_RUN=$((TESTS_RUN + 1))
    if "$@" > "$TEST_DIR/last_stdout" 2> "$TEST_DIR/last_stderr"; then
        log_error "$description (expected failure but got success)"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        return 1
    else
        log_success "$description"
        TESTS_PASSED=$((TESTS_PASSED + 1))
        return 0
    fi
}

assert_output_contains() {
    local description="$1"
    local expected="$2"
    shift 2
    TESTS_RUN=$((TESTS_RUN + 1))
    local output
    if ! output=$("$@" 2>/dev/null); then
        log_error "$description (command failed)"
        log_error "  output: $output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        return 1
    fi
    if echo "$output" | grep -qF "$expected"; then
        log_success "$description"
        TESTS_PASSED=$((TESTS_PASSED + 1))
        return 0
    else
        log_error "$description"
        log_error "  expected to contain: $expected"
        log_error "  actual output: $output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        return 1
    fi
}

assert_output_not_contains() {
    local description="$1"
    local unexpected="$2"
    shift 2
    TESTS_RUN=$((TESTS_RUN + 1))
    local output
    if ! output=$("$@" 2>/dev/null); then
        log_error "$description (command failed)"
        log_error "  output: $output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        return 1
    fi
    if echo "$output" | grep -qF "$unexpected"; then
        log_error "$description"
        log_error "  should not contain: $unexpected"
        log_error "  actual output: $output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
        return 1
    else
        log_success "$description"
        TESTS_PASSED=$((TESTS_PASSED + 1))
        return 0
    fi
}

# ============================================================================
# Setup and teardown
# ============================================================================

cleanup() {
    log_info "Cleaning up..."

    # Kill server if running
    if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
        kill "$SERVER_PID" 2>/dev/null || true
        wait "$SERVER_PID" 2>/dev/null || true
    fi

    if [[ "$SKIP_CLEANUP" == "false" ]]; then
        rm -rf "$TEST_DIR"
        log_info "Test directory cleaned up: $TEST_DIR"
    else
        log_info "Test artifacts preserved in: $TEST_DIR"
    fi
}

trap cleanup EXIT

check_prerequisites() {
    log_section "Checking Prerequisites"

    for cmd in go jq; do
        if ! command -v "$cmd" &> /dev/null; then
            log_error "Required command '$cmd' not found"
            exit 1
        fi
    done
    log_success "Required tools available (go, jq)"
}

build_scion() {
    if [[ "$SKIP_BUILD" == "true" ]]; then
        log_info "Skipping build (--skip-build)"
        SCION="$TEST_DIR/scion"
        return
    fi

    log_section "Building Scion Binary"

    cd "$PROJECT_ROOT"
    log_info "Building scion from $PROJECT_ROOT..."

    if go build -buildvcs=false -o "$TEST_DIR/scion" ./cmd/scion 2>&1; then
        log_success "Build successful: $TEST_DIR/scion"
    else
        log_error "Build failed"
        exit 1
    fi
    SCION="$TEST_DIR/scion"
}

start_hub_server() {
    log_section "Starting Hub Server"

    mkdir -p "$TEST_DIR"

    local cmd=("$SCION" "server" "start" "--foreground" "--production"
        "--enable-hub"
        "--dev-auth"
        "--port" "$HUB_PORT"
        "--db" "$TEST_DIR/hub-test.db"
    )

    if [[ "$VERBOSE" == "true" ]]; then
        cmd+=("--debug")
    fi

    log_info "Starting server: ${cmd[*]}"
    "${cmd[@]}" > "$TEST_DIR/server.log" 2>&1 &
    SERVER_PID=$!

    # Wait for server to be ready
    local max_wait=30
    local waited=0
    while ! curl -s "http://localhost:$HUB_PORT/healthz" > /dev/null 2>&1; do
        sleep 1
        waited=$((waited + 1))
        if [[ $waited -ge $max_wait ]]; then
            log_error "Server failed to start within ${max_wait}s"
            cat "$TEST_DIR/server.log"
            exit 1
        fi
    done

    log_success "Hub server running on port $HUB_PORT (PID: $SERVER_PID)"

    # Retrieve dev token
    local token
    token=$(cat ~/.scion/dev-token 2>/dev/null || echo "")
    if [[ -z "$token" ]]; then
        log_error "Dev token not found at ~/.scion/dev-token"
        exit 1
    fi

    export SCION_DEV_TOKEN="$token"
    export SCION_HUB_ENDPOINT="http://localhost:$HUB_PORT"

    log_success "Authentication configured (dev token)"
}

setup_test_grove() {
    log_section "Setting Up Test Grove"

    local grove_dir="$TEST_DIR/test-grove"
    mkdir -p "$grove_dir"

    # Initialize a grove
    cd "$grove_dir"
    git init -q .
    git commit --allow-empty -m "init" -q

    $SCION init -y 2>&1 || true
    log_success "Grove initialized at $grove_dir"

    # Link grove to the Hub
    if $SCION hub link -y 2>&1; then
        log_success "Grove linked to Hub"
    else
        log_error "Failed to link grove to Hub"
        exit 1
    fi

    # Extract the grove ID for later use
    GROVE_ID=$($SCION config get grove_id 2>/dev/null || "")
    if [[ -z "$GROVE_ID" ]]; then
        # Fall back to reading settings.yaml directly
        GROVE_ID=$(grep 'grove_id:' "$grove_dir/.scion/settings.yaml" 2>/dev/null | awk '{print $2}' || echo "")
    fi
    log_info "Grove ID: ${GROVE_ID:-<not found>}"
}

# ============================================================================
# Phase 1: User-Scoped Env Variables (via CLI)
# ============================================================================

test_phase1_user_scope() {
    log_section "Phase 1: User-Scoped Environment Variables"

    # 1.1 List variables when none exist
    assert_output_contains \
        "1.1  List env vars (empty)" \
        "No environment variables found" \
        $SCION hub env get

    # 1.2 Set a variable using KEY=VALUE format
    assert_output_contains \
        "1.2  Set env var (KEY=VALUE format)" \
        "Created" \
        $SCION hub env set "TEST_VAR_A=hello_world"

    # 1.3 Set a variable using KEY VALUE format
    assert_output_contains \
        "1.3  Set env var (KEY VALUE format)" \
        "Created" \
        $SCION hub env set TEST_VAR_B some_value

    # 1.4 Set a third variable
    assert_output_contains \
        "1.4  Set another env var" \
        "Created" \
        $SCION hub env set "TEST_VAR_C=third_value"

    # 1.5 Get a specific variable
    assert_output_contains \
        "1.5  Get specific env var" \
        "TEST_VAR_A=hello_world" \
        $SCION hub env get TEST_VAR_A

    # 1.6 List all variables
    assert_output_contains \
        "1.6  List all env vars (contains TEST_VAR_A)" \
        "TEST_VAR_A" \
        $SCION hub env get

    assert_output_contains \
        "1.6b List all env vars (contains TEST_VAR_B)" \
        "TEST_VAR_B" \
        $SCION hub env get

    # 1.7 Update an existing variable
    assert_output_contains \
        "1.7  Update existing env var" \
        "Updated" \
        $SCION hub env set "TEST_VAR_A=updated_value"

    # 1.8 Verify the update took effect
    assert_output_contains \
        "1.8  Verify update" \
        "TEST_VAR_A=updated_value" \
        $SCION hub env get TEST_VAR_A

    # 1.9 Get variable in JSON format
    TESTS_RUN=$((TESTS_RUN + 1))
    local json_output=""
    local json_key=""
    json_output=$($SCION hub env get --json TEST_VAR_A 2>/dev/null) || true
    json_key=$(echo "$json_output" | jq -r '.key // empty' 2>/dev/null) || true
    if [[ "$json_key" == "TEST_VAR_A" ]]; then
        log_success "1.9  Get env var with --json flag"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "1.9  Get env var with --json flag"
        log_error "  expected key 'TEST_VAR_A' in JSON, got: $json_output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi

    # 1.10 List variables in JSON format
    TESTS_RUN=$((TESTS_RUN + 1))
    json_output=$($SCION hub env get --json 2>/dev/null) || true
    local var_count=""
    var_count=$(echo "$json_output" | jq '.envVars | length' 2>/dev/null) || true
    if [[ "$var_count" -ge 3 ]]; then
        log_success "1.10 List env vars with --json flag ($var_count vars)"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "1.10 List env vars with --json flag (expected >= 3 vars)"
        log_error "  output: $json_output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi

    # 1.11 Clear a variable
    assert_output_contains \
        "1.11 Clear env var" \
        "Deleted" \
        $SCION hub env clear TEST_VAR_C

    # 1.12 Verify the cleared variable is gone
    assert_failure \
        "1.12 Verify cleared var is gone (get should fail)" \
        $SCION hub env get TEST_VAR_C

    # 1.13 Verify remaining variables still exist
    assert_output_contains \
        "1.13 Verify remaining vars survive clear" \
        "TEST_VAR_A" \
        $SCION hub env get

    log_info "Phase 1 complete"
}

# ============================================================================
# Phase 2: Grove-Scoped Env Variables (via CLI)
# ============================================================================

test_phase2_grove_scope() {
    log_section "Phase 2: Grove-Scoped Environment Variables"

    # 2.1 List grove vars (should be empty)
    assert_output_contains \
        "2.1  List grove env vars (empty)" \
        "No environment variables found" \
        $SCION hub env get --grove="$GROVE_ID"

    # 2.2 Set a grove-scoped variable (infer grove from current dir)
    assert_output_contains \
        "2.2  Set grove-scoped env var" \
        "Created" \
        $SCION hub env set --grove="$GROVE_ID" "GROVE_VAR_A=grove_value_1"

    # 2.3 Set another grove-scoped variable
    assert_output_contains \
        "2.3  Set another grove-scoped env var" \
        "Created" \
        $SCION hub env set --grove="$GROVE_ID" GROVE_VAR_B grove_value_2

    # 2.4 Get a specific grove variable
    assert_output_contains \
        "2.4  Get specific grove env var" \
        "GROVE_VAR_A=grove_value_1" \
        $SCION hub env get --grove="$GROVE_ID" GROVE_VAR_A

    # 2.5 List all grove variables
    assert_output_contains \
        "2.5  List grove env vars" \
        "GROVE_VAR_A" \
        $SCION hub env get --grove="$GROVE_ID"

    assert_output_contains \
        "2.5b List grove env vars (contains B)" \
        "GROVE_VAR_B" \
        $SCION hub env get --grove="$GROVE_ID"

    # 2.6 Update a grove variable
    assert_output_contains \
        "2.6  Update grove-scoped env var" \
        "Updated" \
        $SCION hub env set --grove="$GROVE_ID" "GROVE_VAR_A=updated_grove_value"

    # 2.7 Verify the grove update
    assert_output_contains \
        "2.7  Verify grove update" \
        "GROVE_VAR_A=updated_grove_value" \
        $SCION hub env get --grove="$GROVE_ID" GROVE_VAR_A

    # 2.8 Grove and user scopes are independent
    assert_output_not_contains \
        "2.8  User scope does not contain grove vars" \
        "GROVE_VAR_A" \
        $SCION hub env get

    assert_output_not_contains \
        "2.8b Grove scope does not contain user vars" \
        "TEST_VAR_A" \
        $SCION hub env get --grove="$GROVE_ID"

    # 2.9 Get grove variable in JSON format
    TESTS_RUN=$((TESTS_RUN + 1))
    local json_output=""
    json_output=$($SCION hub env get --grove="$GROVE_ID" --json GROVE_VAR_A 2>/dev/null) || true
    local json_scope=""
    json_scope=$(echo "$json_output" | jq -r '.scope // empty' 2>/dev/null) || true
    if [[ "$json_scope" == "project" ]]; then
        log_success "2.9  Get grove env var --json shows scope=project"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "2.9  Get grove env var --json shows scope=project"
        log_error "  expected scope 'project', got: $json_output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi

    # 2.10 Clear a grove variable
    assert_output_contains \
        "2.10 Clear grove env var" \
        "Deleted" \
        $SCION hub env clear --grove="$GROVE_ID" GROVE_VAR_B

    # 2.11 Verify the cleared grove variable is gone
    assert_failure \
        "2.11 Verify cleared grove var is gone" \
        $SCION hub env get --grove="$GROVE_ID" GROVE_VAR_B

    # 2.12 Verify remaining grove variable still exists
    assert_output_contains \
        "2.12 Remaining grove var survives clear" \
        "GROVE_VAR_A" \
        $SCION hub env get --grove="$GROVE_ID"

    log_info "Phase 2 complete"
}

# ============================================================================
# Phase 3: Edge Cases and Validation
# ============================================================================

test_phase3_edge_cases() {
    log_section "Phase 3: Edge Cases and Validation"

    # 3.1 Set variable with special characters in value
    assert_output_contains \
        "3.1  Set var with special chars in value" \
        "Created" \
        $SCION hub env set "SPECIAL_VAR=hello world with spaces"

    # 3.2 Retrieve variable with special value
    assert_output_contains \
        "3.2  Get var with special chars" \
        "hello world with spaces" \
        $SCION hub env get SPECIAL_VAR

    # 3.3 Set variable with URL value
    assert_output_contains \
        "3.3  Set var with URL value" \
        "Created" \
        $SCION hub env set "URL_VAR=https://example.com:8080/api/v1?key=val&other=123"

    # 3.4 Retrieve URL value
    assert_output_contains \
        "3.4  Get URL value" \
        "https://example.com:8080/api/v1?key=val&other=123" \
        $SCION hub env get URL_VAR

    # 3.5 Set variable with equals sign in value
    assert_output_contains \
        "3.5  Set var with '=' in value" \
        "Created" \
        $SCION hub env set "EQUALS_VAR=key=value=extra"

    # 3.6 Overwrite variable multiple times
    $SCION hub env set "MULTI_VAR=first" > /dev/null 2>&1
    $SCION hub env set "MULTI_VAR=second" > /dev/null 2>&1
    $SCION hub env set "MULTI_VAR=third" > /dev/null 2>&1
    assert_output_contains \
        "3.6  Overwrite var multiple times (final value)" \
        "MULTI_VAR=third" \
        $SCION hub env get MULTI_VAR

    # 3.7 Invalid key format (contains =)
    assert_failure \
        "3.7  Reject key containing '=' in KEY VALUE form" \
        $SCION hub env set "BAD=KEY" "value"

    # 3.8 Empty key
    assert_failure \
        "3.8  Reject empty key (=value)" \
        $SCION hub env set "=value"

    # 3.9 Clear non-existent variable
    assert_failure \
        "3.9  Clear non-existent var fails" \
        $SCION hub env clear NON_EXISTENT_VAR_ZZZZZ

    # 3.10 Cannot use --grove and --broker at the same time
    assert_failure \
        "3.10 Reject --grove and --broker together" \
        $SCION hub env get --grove="$GROVE_ID" --broker=fake-broker-id

    log_info "Phase 3 complete"
}

# ============================================================================
# Phase 4: Injection Mode and Secret Options
# ============================================================================

test_phase4_injection_and_secret() {
    log_section "Phase 4: Injection Mode and Secret Options"

    # 4.1 Set var with --always
    assert_output_contains \
        "4.1  Set var with --always" \
        "always" \
        $SCION hub env set --always "INJECT_VAR_A=always_val"

    # 4.2 Get var shows (always) annotation
    assert_output_contains \
        "4.2  Get var shows (always) annotation" \
        "always" \
        $SCION hub env get INJECT_VAR_A

    # 4.3 Set var with explicit --as-needed
    assert_output_contains \
        "4.3  Set var with --as-needed" \
        "(as-needed)" \
        $SCION hub env set --as-needed "INJECT_VAR_B=asneeded_val"

    # 4.4 Set var with neither (default is as-needed)
    assert_output_contains \
        "4.4  Set var with default injection mode" \
        "(as-needed)" \
        $SCION hub env set "INJECT_VAR_C=default_val"

    # 4.5 Set var with --secret
    assert_output_contains \
        "4.5  Set var with --secret" \
        "secret" \
        $SCION hub env set --secret "SECRET_VAR_A=s3cret_val"

    # 4.6 Get secret var shows masked value
    assert_output_contains \
        "4.6  Get secret var shows masked value" \
        "******" \
        $SCION hub env get SECRET_VAR_A

    # 4.7 Secret var value is not shown
    assert_output_not_contains \
        "4.7  Secret var value is not shown in get" \
        "s3cret_val" \
        $SCION hub env get SECRET_VAR_A

    # 4.8 Update var from as-needed to always
    assert_output_contains \
        "4.8  Update var to --always" \
        "always" \
        $SCION hub env set --always "INJECT_VAR_B=updated_always"

    # 4.9 Verify update changed injection mode
    assert_output_contains \
        "4.9  Verify updated injection mode" \
        "always" \
        $SCION hub env get INJECT_VAR_B

    # 4.10 Set var with --secret --always combined
    assert_output_contains \
        "4.10 Set var with --secret --always" \
        "always" \
        $SCION hub env set --secret --always "COMBO_VAR=combo_val"

    assert_output_contains \
        "4.10b Set var with --secret --always shows secret" \
        "secret" \
        $SCION hub env get COMBO_VAR

    # 4.11 --always and --as-needed together is rejected
    assert_failure \
        "4.11 Reject --always and --as-needed together" \
        $SCION hub env set --always --as-needed "BAD_VAR=bad"

    # 4.12 JSON output includes injectionMode and secret fields
    TESTS_RUN=$((TESTS_RUN + 1))
    local json_output=""
    json_output=$($SCION hub env get --json SECRET_VAR_A 2>/dev/null) || true
    local json_injection_mode=""
    local json_secret=""
    json_injection_mode=$(echo "$json_output" | jq -r '.injectionMode // empty' 2>/dev/null) || true
    json_secret=$(echo "$json_output" | jq -r '.secret // empty' 2>/dev/null) || true
    if [[ "$json_secret" == "true" ]]; then
        log_success "4.12 JSON output includes secret field"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "4.12 JSON output includes secret field"
        log_error "  expected secret=true, got: $json_output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi

    TESTS_RUN=$((TESTS_RUN + 1))
    json_output=$($SCION hub env get --json INJECT_VAR_A 2>/dev/null) || true
    json_injection_mode=$(echo "$json_output" | jq -r '.injectionMode // empty' 2>/dev/null) || true
    if [[ "$json_injection_mode" == "always" ]]; then
        log_success "4.12b JSON output includes injectionMode field"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "4.12b JSON output includes injectionMode field"
        log_error "  expected injectionMode=always, got: $json_output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi

    # Clean up test vars
    for key in INJECT_VAR_A INJECT_VAR_B INJECT_VAR_C SECRET_VAR_A COMBO_VAR; do
        $SCION hub env clear "$key" 2>/dev/null || true
    done

    log_info "Phase 4 complete"
}

# ============================================================================
# Phase 5: Cleanup Verification
# ============================================================================

test_phase5_cleanup() {
    log_section "Phase 5: Cleanup Verification"

    # Clear all user-scoped test variables
    for key in TEST_VAR_A TEST_VAR_B SPECIAL_VAR URL_VAR EQUALS_VAR MULTI_VAR; do
        $SCION hub env clear "$key" 2>/dev/null || true
    done

    # Clear remaining grove-scoped variables
    $SCION hub env clear --grove="$GROVE_ID" GROVE_VAR_A 2>/dev/null || true

    # 5.1 Verify user scope is empty
    assert_output_contains \
        "5.1  User scope empty after cleanup" \
        "No environment variables found" \
        $SCION hub env get

    # 5.2 Verify grove scope is empty
    assert_output_contains \
        "5.2  Grove scope empty after cleanup" \
        "No environment variables found" \
        $SCION hub env get --grove="$GROVE_ID"

    log_info "Phase 5 complete"
}

# ============================================================================
# Main Test Runner
# ============================================================================

run_all_tests() {
    log_section "Scion Hub Env Integration Test Suite"
    log_info "Test directory: $TEST_DIR"
    log_info "Project root: $PROJECT_ROOT"

    mkdir -p "$TEST_DIR"

    check_prerequisites
    build_scion
    start_hub_server
    setup_test_grove

    test_phase1_user_scope
    test_phase2_grove_scope
    test_phase3_edge_cases
    test_phase4_injection_and_secret
    test_phase5_cleanup

    # Summary
    log_section "Test Summary"
    echo -e "  Total:  $TESTS_RUN"
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

# Run the tests
run_all_tests
