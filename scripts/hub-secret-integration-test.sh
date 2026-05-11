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
# Hub Secrets Integration Test Script
# =====================================
# This script tests the full hub secret storage feature by exercising the
# scion CLI commands for setting, getting, listing, and clearing
# secrets at user and grove scopes, including type-aware secrets
# (environment, variable, file) and the hub env --secret redirect.
#
# It starts a Hub server with dev auth, links a test grove, and runs
# the complete set of secret CRUD operations.
#
# Usage:
#   ./scripts/hub-secret-integration-test.sh [options]
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
TEST_DIR="/tmp/scion-hub-secret-test-$$"
HUB_PORT=9821
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
            head -38 "$0" | tail -33
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
# Phase 1: User-Scoped Secrets — Basic CRUD
# ============================================================================

test_phase1_user_scope_crud() {
    log_section "Phase 1: User-Scoped Secrets — Basic CRUD"

    # 1.1 List secrets when none exist
    assert_output_contains \
        "1.1  List secrets (empty)" \
        "No secrets found" \
        $SCION hub secret get

    # 1.2 Set a secret (default type = environment)
    assert_output_contains \
        "1.2  Set secret (default type)" \
        "Created" \
        $SCION hub secret set API_KEY sk-test-key-123

    # 1.3 Set another secret
    assert_output_contains \
        "1.3  Set another secret" \
        "Created" \
        $SCION hub secret set DB_PASSWORD supersecret

    # 1.4 Get specific secret metadata
    assert_output_contains \
        "1.4  Get secret metadata (key)" \
        "API_KEY" \
        $SCION hub secret get API_KEY

    # 1.5 Get shows type field
    assert_output_contains \
        "1.5  Get secret shows type" \
        "environment" \
        $SCION hub secret get API_KEY

    # 1.6 Values are never returned (write-only)
    assert_output_not_contains \
        "1.6  Secret value not in get output" \
        "sk-test-key-123" \
        $SCION hub secret get API_KEY

    # 1.7 List all secrets
    assert_output_contains \
        "1.7  List all secrets (contains API_KEY)" \
        "API_KEY" \
        $SCION hub secret get

    assert_output_contains \
        "1.7b List all secrets (contains DB_PASSWORD)" \
        "DB_PASSWORD" \
        $SCION hub secret get

    # 1.8 Update an existing secret
    assert_output_contains \
        "1.8  Update existing secret" \
        "Updated" \
        $SCION hub secret set API_KEY sk-updated-key-456

    # 1.9 Updated value is still not shown
    assert_output_not_contains \
        "1.9  Updated value not returned" \
        "sk-updated-key-456" \
        $SCION hub secret get API_KEY

    # 1.10 Get secret in JSON format
    TESTS_RUN=$((TESTS_RUN + 1))
    local json_output=""
    local json_key=""
    json_output=$($SCION hub secret get --json API_KEY 2>/dev/null) || true
    json_key=$(echo "$json_output" | jq -r '.key // empty' 2>/dev/null) || true
    if [[ "$json_key" == "API_KEY" ]]; then
        log_success "1.10 Get secret --json returns key"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "1.10 Get secret --json returns key"
        log_error "  expected key 'API_KEY' in JSON, got: $json_output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi

    # 1.11 List secrets in JSON format
    TESTS_RUN=$((TESTS_RUN + 1))
    json_output=$($SCION hub secret get --json 2>/dev/null) || true
    local secret_count=""
    secret_count=$(echo "$json_output" | jq '.secrets | length' 2>/dev/null) || true
    if [[ "$secret_count" -ge 2 ]]; then
        log_success "1.11 List secrets --json ($secret_count secrets)"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "1.11 List secrets --json (expected >= 2 secrets)"
        log_error "  output: $json_output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi

    # 1.12 Clear a secret
    assert_output_contains \
        "1.12 Clear secret" \
        "Deleted" \
        $SCION hub secret clear DB_PASSWORD

    # 1.13 Verify cleared secret is gone
    assert_failure \
        "1.13 Verify cleared secret is gone (get should fail)" \
        $SCION hub secret get DB_PASSWORD

    # 1.14 Remaining secret still exists
    assert_output_contains \
        "1.14 Remaining secret survives clear" \
        "API_KEY" \
        $SCION hub secret get

    log_info "Phase 1 complete"
}

# ============================================================================
# Phase 2: Secret Types — Environment, Variable, File
# ============================================================================

test_phase2_secret_types() {
    log_section "Phase 2: Secret Types — Environment, Variable, File"

    # 2.1 Set secret with explicit environment type
    assert_output_contains \
        "2.1  Set secret --type environment" \
        "environment" \
        $SCION hub secret set --type environment ENV_SECRET env_value

    # 2.2 Set secret with variable type
    assert_output_contains \
        "2.2  Set secret --type variable" \
        "variable" \
        $SCION hub secret set --type variable VAR_SECRET '{"db":"prod"}'

    # 2.3 Set secret with file type and target
    assert_output_contains \
        "2.3  Set secret --type file --target" \
        "file" \
        $SCION hub secret set --type file --target /etc/ssl/cert.pem TLS_CERT "cert-content-here"

    # 2.4 Get variable secret shows correct type
    assert_output_contains \
        "2.4  Get variable secret shows type" \
        "variable" \
        $SCION hub secret get VAR_SECRET

    # 2.5 Get file secret shows correct type
    assert_output_contains \
        "2.5  Get file secret shows type" \
        "file" \
        $SCION hub secret get TLS_CERT

    # 2.6 Get file secret shows target path
    assert_output_contains \
        "2.6  File secret shows target path" \
        "/etc/ssl/cert.pem" \
        $SCION hub secret get TLS_CERT

    # 2.7 JSON output includes type field
    TESTS_RUN=$((TESTS_RUN + 1))
    local json_output=""
    json_output=$($SCION hub secret get --json VAR_SECRET 2>/dev/null) || true
    local json_type=""
    json_type=$(echo "$json_output" | jq -r '.type // .secretType // empty' 2>/dev/null) || true
    if [[ "$json_type" == "variable" ]]; then
        log_success "2.7  JSON output includes type=variable"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "2.7  JSON output includes type=variable"
        log_error "  expected type='variable', got: $json_output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi

    # 2.8 List with type column shows types
    assert_output_contains \
        "2.8  List output includes TYPE column header" \
        "TYPE" \
        $SCION hub secret get

    # 2.9 Set file secret using @file syntax
    echo "file-secret-content-from-disk" > "$TEST_DIR/test-secret-file.txt"
    assert_output_contains \
        "2.9  Set secret with @file syntax" \
        "file" \
        $SCION hub secret set FILE_FROM_DISK "@$TEST_DIR/test-secret-file.txt"

    # 2.10 @file syntax defaults type to file
    assert_output_contains \
        "2.10 @file defaults type to file" \
        "file" \
        $SCION hub secret get FILE_FROM_DISK

    # Clean up type test secrets
    for key in ENV_SECRET VAR_SECRET TLS_CERT FILE_FROM_DISK; do
        $SCION hub secret clear "$key" 2>/dev/null || true
    done

    log_info "Phase 2 complete"
}

# ============================================================================
# Phase 3: Grove-Scoped Secrets
# ============================================================================

test_phase3_grove_scope() {
    log_section "Phase 3: Grove-Scoped Secrets"

    # 3.1 List grove secrets (should be empty)
    assert_output_contains \
        "3.1  List grove secrets (empty)" \
        "No secrets found" \
        $SCION hub secret get --grove="$GROVE_ID"

    # 3.2 Set a grove-scoped secret
    assert_output_contains \
        "3.2  Set grove-scoped secret" \
        "Created" \
        $SCION hub secret set --grove="$GROVE_ID" GROVE_SECRET grove_secret_val

    # 3.3 Set a grove-scoped file secret
    assert_output_contains \
        "3.3  Set grove-scoped file secret" \
        "Created" \
        $SCION hub secret set --grove="$GROVE_ID" --type file --target /app/config.json GROVE_CONFIG '{"env":"prod"}'

    # 3.4 Get specific grove secret
    assert_output_contains \
        "3.4  Get grove secret metadata" \
        "GROVE_SECRET" \
        $SCION hub secret get --grove="$GROVE_ID" GROVE_SECRET

    # 3.5 List grove secrets
    assert_output_contains \
        "3.5  List grove secrets (contains GROVE_SECRET)" \
        "GROVE_SECRET" \
        $SCION hub secret get --grove="$GROVE_ID"

    assert_output_contains \
        "3.5b List grove secrets (contains GROVE_CONFIG)" \
        "GROVE_CONFIG" \
        $SCION hub secret get --grove="$GROVE_ID"

    # 3.6 Grove and user scopes are independent
    assert_output_not_contains \
        "3.6  User scope does not contain grove secrets" \
        "GROVE_SECRET" \
        $SCION hub secret get

    assert_output_not_contains \
        "3.6b Grove scope does not contain user secrets" \
        "API_KEY" \
        $SCION hub secret get --grove="$GROVE_ID"

    # 3.7 Update grove secret
    assert_output_contains \
        "3.7  Update grove secret" \
        "Updated" \
        $SCION hub secret set --grove="$GROVE_ID" GROVE_SECRET updated_grove_val

    # 3.8 Clear grove secret
    assert_output_contains \
        "3.8  Clear grove secret" \
        "Deleted" \
        $SCION hub secret clear --grove="$GROVE_ID" GROVE_CONFIG

    # 3.9 Verify cleared grove secret is gone
    assert_failure \
        "3.9  Verify cleared grove secret is gone" \
        $SCION hub secret get --grove="$GROVE_ID" GROVE_CONFIG

    # 3.10 JSON output shows grove scope
    TESTS_RUN=$((TESTS_RUN + 1))
    local json_output=""
    json_output=$($SCION hub secret get --grove="$GROVE_ID" --json GROVE_SECRET 2>/dev/null) || true
    local json_scope=""
    json_scope=$(echo "$json_output" | jq -r '.scope // empty' 2>/dev/null) || true
    if [[ "$json_scope" == "project" ]]; then
        log_success "3.10 JSON output shows scope=project"
        TESTS_PASSED=$((TESTS_PASSED + 1))
    else
        log_error "3.10 JSON output shows scope=project"
        log_error "  expected scope='project', got: $json_output"
        TESTS_FAILED=$((TESTS_FAILED + 1))
    fi

    # Clean up grove secrets
    $SCION hub secret clear --grove="$GROVE_ID" GROVE_SECRET 2>/dev/null || true

    log_info "Phase 3 complete"
}

# ============================================================================
# Phase 4: hub env --secret Redirect
# ============================================================================

test_phase4_env_secret_redirect() {
    log_section "Phase 4: hub env --secret Redirect"

    # 4.1 Set env var with --secret creates a secret
    assert_output_contains \
        "4.1  hub env set --secret creates secret" \
        "(secret)" \
        $SCION hub env set --secret "ENV_AS_SECRET=secret_via_env"

    # 4.2 Value is masked in output
    assert_output_contains \
        "4.2  --secret masks value in output" \
        "********" \
        $SCION hub env set --secret "ENV_AS_SECRET2=another_secret"

    # 4.3 Secret value is not shown in env set output
    assert_output_not_contains \
        "4.3  Secret value not in env set output" \
        "secret_via_env" \
        $SCION hub env set --secret "ENV_AS_SECRET3=hidden_val"

    # 4.4 Secret created via env --secret appears in secret list
    assert_output_contains \
        "4.4  Secret via env --secret appears in hub secret get" \
        "ENV_AS_SECRET" \
        $SCION hub secret get

    # 4.5 Secret type is environment (since it came from env --secret)
    assert_output_contains \
        "4.5  Secret via env --secret has type=environment" \
        "environment" \
        $SCION hub secret get ENV_AS_SECRET

    # Clean up
    for key in ENV_AS_SECRET ENV_AS_SECRET2 ENV_AS_SECRET3; do
        $SCION hub secret clear "$key" 2>/dev/null || true
    done

    log_info "Phase 4 complete"
}

# ============================================================================
# Phase 5: Edge Cases and Validation
# ============================================================================

test_phase5_edge_cases() {
    log_section "Phase 5: Edge Cases and Validation"

    # 5.1 Empty key rejected
    assert_failure \
        "5.1  Reject empty key" \
        $SCION hub secret set "" "value"

    # 5.2 Key with spaces rejected
    assert_failure \
        "5.2  Reject key with spaces" \
        $SCION hub secret set "BAD KEY" "value"

    # 5.3 Key with equals rejected
    assert_failure \
        "5.3  Reject key with equals" \
        $SCION hub secret set "BAD=KEY" "value"

    # 5.4 Clear non-existent secret fails
    assert_failure \
        "5.4  Clear non-existent secret fails" \
        $SCION hub secret clear NON_EXISTENT_SECRET_ZZZZZ

    # 5.5 Cannot use --grove and --broker at the same time
    assert_failure \
        "5.5  Reject --grove and --broker together" \
        $SCION hub secret get --grove="$GROVE_ID" --broker=fake-broker-id

    # 5.6 Set and update multiple times
    $SCION hub secret set MULTI_SECRET "first" > /dev/null 2>&1
    $SCION hub secret set MULTI_SECRET "second" > /dev/null 2>&1
    $SCION hub secret set MULTI_SECRET "third" > /dev/null 2>&1
    # Should still exist (latest update wins)
    assert_output_contains \
        "5.6  Multiple updates succeed (secret still exists)" \
        "MULTI_SECRET" \
        $SCION hub secret get MULTI_SECRET

    # 5.7 @file with non-existent file fails
    assert_failure \
        "5.7  @file with non-existent file fails" \
        $SCION hub secret set BAD_FILE "@/tmp/nonexistent-file-zzzzz.txt"

    # 5.8 Set with explicit target (env type)
    assert_output_contains \
        "5.8  Set with --target for env type" \
        "Created" \
        $SCION hub secret set --type environment --target MY_CUSTOM_VAR ALIASED_SECRET "value123"

    # Clean up
    for key in MULTI_SECRET ALIASED_SECRET; do
        $SCION hub secret clear "$key" 2>/dev/null || true
    done

    log_info "Phase 5 complete"
}

# ============================================================================
# Phase 6: Cleanup Verification
# ============================================================================

test_phase6_cleanup() {
    log_section "Phase 6: Cleanup Verification"

    # Clear any remaining user-scoped test secrets
    $SCION hub secret clear API_KEY 2>/dev/null || true

    # 6.1 Verify user scope is empty
    assert_output_contains \
        "6.1  User scope empty after cleanup" \
        "No secrets found" \
        $SCION hub secret get

    # 6.2 Verify grove scope is empty
    assert_output_contains \
        "6.2  Grove scope empty after cleanup" \
        "No secrets found" \
        $SCION hub secret get --grove="$GROVE_ID"

    log_info "Phase 6 complete"
}

# ============================================================================
# Main Test Runner
# ============================================================================

run_all_tests() {
    log_section "Scion Hub Secret Integration Test Suite"
    log_info "Test directory: $TEST_DIR"
    log_info "Project root: $PROJECT_ROOT"

    mkdir -p "$TEST_DIR"

    check_prerequisites
    build_scion
    start_hub_server
    setup_test_grove

    test_phase1_user_scope_crud
    test_phase2_secret_types
    test_phase3_grove_scope
    test_phase4_env_secret_redirect
    test_phase5_edge_cases
    test_phase6_cleanup

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
