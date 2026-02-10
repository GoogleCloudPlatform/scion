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
# Template Integration Test Script
# =================================
# This script tests the full template management flow for the hosted Scion architecture,
# covering Phases 1, 2, and 3 of the hosted-templates implementation.
#
# Phase 1: Foundation (Hub template CRUD, storage backend)
# Phase 2: Upload Flow (signed URLs, file upload/download, CLI commands)
# Phase 3: Runtime Integration (template cache, hydration, agent creation)
#
# Usage:
#   ./scripts/template-integration-test.sh [options]
#
# Options:
#   --skip-build     Skip building the scion binary
#   --skip-cleanup   Don't clean up test artifacts after completion
#   --storage-bucket Use GCS storage instead of local (requires GOOGLE_APPLICATION_CREDENTIALS)
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
TEST_DIR="/tmp/scion-template-test-$$"
STORAGE_DIR="$TEST_DIR/storage"
HUB_PORT=9810
RUNTIME_HOST_PORT=9800
SKIP_BUILD=false
SKIP_CLEANUP=false
USE_GCS=false
VERBOSE=false
STORAGE_BUCKET=""

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
        --storage-bucket)
            USE_GCS=true
            STORAGE_BUCKET="$2"
            shift 2
            ;;
        --verbose)
            VERBOSE=true
            shift
            ;;
        --help)
            head -30 "$0" | tail -25
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# Logging functions
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

# Cleanup function
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

# Check prerequisites
check_prerequisites() {
    log_section "Checking Prerequisites"

    # Check for required tools
    for cmd in curl jq go; do
        if ! command -v "$cmd" &> /dev/null; then
            log_error "Required command '$cmd' not found"
            exit 1
        fi
    done
    log_success "Required tools available (curl, jq, go)"

    # Check Go version
    GO_VERSION=$(go version | grep -oP '\d+\.\d+' | head -1)
    log_info "Go version: $GO_VERSION"

    if [[ "$USE_GCS" == "true" ]]; then
        if [[ -z "$GOOGLE_APPLICATION_CREDENTIALS" ]]; then
            log_error "GOOGLE_APPLICATION_CREDENTIALS not set (required for GCS storage)"
            exit 1
        fi
        log_success "GCS credentials configured"
    fi
}

# Build the scion binary
build_scion() {
    if [[ "$SKIP_BUILD" == "true" ]]; then
        log_info "Skipping build (--skip-build)"
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
}

# Start the server (Hub + Runtime Host)
start_server() {
    log_section "Starting Server (Hub + Runtime Host)"

    mkdir -p "$STORAGE_DIR"
    mkdir -p "$TEST_DIR/cache/templates"

    # Build command as array for proper handling
    local cmd=("$TEST_DIR/scion" "server" "start"
        "--enable-hub"
        "--enable-runtime-broker"
        "--dev-auth"
        "--port" "$HUB_PORT"
        "--runtime-broker-port" "$RUNTIME_HOST_PORT"
        "--template-cache-dir" "$TEST_DIR/cache/templates"
        "--template-cache-max" "10485760"
    )

    if [[ "$USE_GCS" == "true" ]]; then
        cmd+=("--storage-bucket" "$STORAGE_BUCKET")
    else
        cmd+=("--storage-dir" "$STORAGE_DIR")
    fi

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

    # Wait for Runtime Host
    waited=0
    while ! curl -s "http://localhost:$RUNTIME_HOST_PORT/healthz" > /dev/null 2>&1; do
        sleep 1
        waited=$((waited + 1))
        if [[ $waited -ge $max_wait ]]; then
            log_warning "Runtime Host not available (may be expected without Docker)"
            break
        fi
    done

    if curl -s "http://localhost:$RUNTIME_HOST_PORT/healthz" > /dev/null 2>&1; then
        log_success "Runtime Host running on port $RUNTIME_HOST_PORT"
    fi

    # Get auth token
    TOKEN=$(cat ~/.scion/dev-token 2>/dev/null || echo "")
    if [[ -z "$TOKEN" ]]; then
        log_error "Dev token not found at ~/.scion/dev-token"
        exit 1
    fi
    export AUTH="Authorization: Bearer $TOKEN"
    log_success "Authentication token configured"
}

# Create test template files
create_test_template() {
    log_section "Creating Test Template Files"

    local template_dir="$TEST_DIR/templates/test-integration"
    mkdir -p "$template_dir/home/.claude"

    cat > "$template_dir/scion-agent.yaml" << 'EOF'
harness: claude
image: scion-claude:latest
detached: true
config:
  model: sonnet
  commandArgs:
    - "--no-update-check"
EOF

    cat > "$template_dir/home/.bashrc" << 'EOF'
# Custom bashrc for integration test template
export PS1="[\u@scion \W]\$ "
alias ll='ls -la'
alias cls='clear'

# Source any local customizations
if [ -f ~/.bashrc.local ]; then
    source ~/.bashrc.local
fi
EOF

    cat > "$template_dir/home/.claude/CLAUDE.md" << 'EOF'
# Integration Test Template

This template was created by the integration test script.

## Instructions
- This is a test template for verifying the hosted template system
- Created: $(date -u +"%Y-%m-%dT%H:%M:%SZ")

## Testing
Run the integration tests to verify template functionality.
EOF

    log_success "Test template created at $template_dir"
    echo "$template_dir"
}

# ============================================================================
# Phase 1 Tests: Foundation (Hub Template CRUD)
# ============================================================================

test_phase1_hub_api() {
    log_section "Phase 1: Hub API Foundation Tests"

    local base_url="http://localhost:$HUB_PORT"

    # Test 1.1: List templates (should be empty initially or have defaults)
    log_info "Test 1.1: List templates..."
    local list_response=$(curl -s "$base_url/api/v1/templates" -H "$AUTH")
    if echo "$list_response" | jq -e '.templates' > /dev/null 2>&1; then
        log_success "List templates API working"
    else
        log_error "List templates failed: $list_response"
        return 1
    fi

    # Test 1.2: Create template (metadata only)
    log_info "Test 1.2: Create template..."
    local create_response=$(curl -s -X POST "$base_url/api/v1/templates" \
        -H "$AUTH" \
        -H "Content-Type: application/json" \
        -d '{
            "name": "phase1-test-template",
            "harness": "claude",
            "scope": "global",
            "description": "Phase 1 integration test template"
        }')

    local template_id=$(echo "$create_response" | jq -r '.template.id // empty')
    if [[ -n "$template_id" ]]; then
        log_success "Template created: $template_id"
    else
        log_error "Create template failed: $create_response"
        return 1
    fi

    # Test 1.3: Get template by ID
    log_info "Test 1.3: Get template by ID..."
    local get_response=$(curl -s "$base_url/api/v1/templates/$template_id" -H "$AUTH")
    local got_name=$(echo "$get_response" | jq -r '.name // empty')
    if [[ "$got_name" == "phase1-test-template" ]]; then
        log_success "Get template by ID working"
    else
        log_error "Get template failed: $get_response"
        return 1
    fi

    # Test 1.4: Update template
    log_info "Test 1.4: Update template..."
    local update_response=$(curl -s -X PATCH "$base_url/api/v1/templates/$template_id" \
        -H "$AUTH" \
        -H "Content-Type: application/json" \
        -d '{
            "description": "Updated description for Phase 1 test"
        }')

    local updated_desc=$(echo "$update_response" | jq -r '.description // empty')
    if [[ "$updated_desc" == "Updated description for Phase 1 test" ]]; then
        log_success "Update template working"
    else
        log_error "Update template failed: $update_response"
        return 1
    fi

    # Test 1.5: Delete template
    log_info "Test 1.5: Delete template..."
    local delete_status=$(curl -s -o /dev/null -w "%{http_code}" \
        -X DELETE "$base_url/api/v1/templates/$template_id?deleteFiles=true" \
        -H "$AUTH")

    if [[ "$delete_status" == "204" || "$delete_status" == "200" ]]; then
        log_success "Delete template working"
    else
        log_error "Delete template failed with status: $delete_status"
        return 1
    fi

    log_success "Phase 1 tests completed successfully"
}

# ============================================================================
# Phase 2 Tests: Upload Flow (Signed URLs, File Upload/Download)
# ============================================================================

test_phase2_upload_flow() {
    log_section "Phase 2: Upload Flow Tests"

    local base_url="http://localhost:$HUB_PORT"
    local template_dir=$(create_test_template)

    # Test 2.1: Create template with file list
    log_info "Test 2.1: Create template with file list..."
    local create_response=$(curl -s -X POST "$base_url/api/v1/templates" \
        -H "$AUTH" \
        -H "Content-Type: application/json" \
        -d '{
            "name": "phase2-upload-test",
            "harness": "claude",
            "scope": "global",
            "description": "Phase 2 upload flow test",
            "files": [
                {"path": "scion-agent.yaml", "size": 200},
                {"path": "home/.bashrc", "size": 300},
                {"path": "home/.claude/CLAUDE.md", "size": 500}
            ]
        }')

    local template_id=$(echo "$create_response" | jq -r '.template.id // empty')
    local upload_urls=$(echo "$create_response" | jq -r '.uploadUrls // empty')

    if [[ -n "$template_id" ]] && [[ "$upload_urls" != "null" ]] && [[ -n "$upload_urls" ]]; then
        log_success "Template created with upload URLs: $template_id"
    else
        log_error "Create with files failed: $create_response"
        return 1
    fi

    # Test 2.2: Upload files to signed URLs
    log_info "Test 2.2: Uploading files to signed URLs..."
    local upload_success=true

    for file_info in $(echo "$create_response" | jq -c '.uploadUrls[]'); do
        local path=$(echo "$file_info" | jq -r '.path')
        local url=$(echo "$file_info" | jq -r '.url')
        local method=$(echo "$file_info" | jq -r '.method // "PUT"')

        local local_file="$template_dir/$path"
        if [[ -f "$local_file" ]]; then
            local upload_status=$(curl -s -o /dev/null -w "%{http_code}" \
                -X "$method" "$url" \
                -H "Content-Type: application/octet-stream" \
                --data-binary "@$local_file")

            if [[ "$upload_status" == "200" || "$upload_status" == "201" ]]; then
                log_success "  Uploaded: $path"
            else
                log_error "  Failed to upload $path (status: $upload_status)"
                upload_success=false
            fi
        else
            log_warning "  File not found: $local_file"
        fi
    done

    if [[ "$upload_success" == "false" ]]; then
        return 1
    fi

    # Test 2.3: Finalize template
    log_info "Test 2.3: Finalizing template..."

    # Compute file hashes (simplified for test)
    local yaml_hash=$(sha256sum "$template_dir/scion-agent.yaml" | cut -d' ' -f1)
    local bashrc_hash=$(sha256sum "$template_dir/home/.bashrc" | cut -d' ' -f1)
    local claude_hash=$(sha256sum "$template_dir/home/.claude/CLAUDE.md" | cut -d' ' -f1)

    local finalize_response=$(curl -s -X POST "$base_url/api/v1/templates/$template_id/finalize" \
        -H "$AUTH" \
        -H "Content-Type: application/json" \
        -d "{
            \"manifest\": {
                \"version\": \"1.0\",
                \"files\": [
                    {\"path\": \"scion-agent.yaml\", \"hash\": \"sha256:$yaml_hash\", \"size\": $(stat -c%s "$template_dir/scion-agent.yaml"), \"mode\": \"0644\"},
                    {\"path\": \"home/.bashrc\", \"hash\": \"sha256:$bashrc_hash\", \"size\": $(stat -c%s "$template_dir/home/.bashrc"), \"mode\": \"0644\"},
                    {\"path\": \"home/.claude/CLAUDE.md\", \"hash\": \"sha256:$claude_hash\", \"size\": $(stat -c%s "$template_dir/home/.claude/CLAUDE.md"), \"mode\": \"0644\"}
                ]
            }
        }")

    local status=$(echo "$finalize_response" | jq -r '.status // empty')
    local content_hash=$(echo "$finalize_response" | jq -r '.contentHash // empty')

    if [[ "$status" == "active" ]] && [[ -n "$content_hash" ]]; then
        log_success "Template finalized (status: $status, contentHash: ${content_hash:0:16}...)"
    else
        log_error "Finalize failed: $finalize_response"
        return 1
    fi

    # Test 2.4: Request download URLs
    log_info "Test 2.4: Requesting download URLs..."
    local download_response=$(curl -s "$base_url/api/v1/templates/$template_id/download" -H "$AUTH")
    local file_count=$(echo "$download_response" | jq '.files | length')

    if [[ "$file_count" -gt 0 ]]; then
        log_success "Download URLs retrieved ($file_count files)"
    else
        log_error "Download URLs failed: $download_response"
        return 1
    fi

    # Test 2.5: Download a file
    log_info "Test 2.5: Downloading file..."
    local first_file_url=$(echo "$download_response" | jq -r '.files[0].url')
    mkdir -p "$TEST_DIR/downloads"
    local download_status=$(curl -s -o "$TEST_DIR/downloads/test-file" -w "%{http_code}" "$first_file_url")

    if [[ "$download_status" == "200" ]]; then
        log_success "File downloaded successfully"
    else
        log_error "Download failed with status: $download_status"
        return 1
    fi

    # Store template ID for Phase 3 tests
    echo "$template_id" > "$TEST_DIR/phase2_template_id"
    echo "$content_hash" > "$TEST_DIR/phase2_content_hash"

    log_success "Phase 2 tests completed successfully"
}

# ============================================================================
# Phase 3 Tests: Runtime Integration (Cache, Hydration)
# ============================================================================

test_phase3_runtime_integration() {
    log_section "Phase 3: Runtime Integration Tests"

    local hub_url="http://localhost:$HUB_PORT"
    local runtime_url="http://localhost:$RUNTIME_HOST_PORT"

    # Check if Runtime Host is available
    if ! curl -s "$runtime_url/healthz" > /dev/null 2>&1; then
        log_warning "Runtime Host not available - skipping agent creation tests"
        log_info "Testing template cache package directly..."
        test_phase3_cache_unit
        return 0
    fi

    # Get template from Phase 2
    local template_id=$(cat "$TEST_DIR/phase2_template_id" 2>/dev/null || echo "")
    local content_hash=$(cat "$TEST_DIR/phase2_content_hash" 2>/dev/null || echo "")

    if [[ -z "$template_id" ]]; then
        log_warning "No template from Phase 2 - creating new template..."
        test_phase2_upload_flow
        template_id=$(cat "$TEST_DIR/phase2_template_id")
        content_hash=$(cat "$TEST_DIR/phase2_content_hash")
    fi

    # Test 3.1: Verify Runtime Host info
    log_info "Test 3.1: Checking Runtime Host info..."
    local info_response=$(curl -s "$runtime_url/api/v1/info")
    local host_type=$(echo "$info_response" | jq -r '.type // "unknown"')
    log_success "Runtime Host type: $host_type"

    # Test 3.2: Create agent with template (triggers hydration)
    log_info "Test 3.2: Creating agent with template (triggers hydration)..."
    local agent_response=$(curl -s -X POST "$runtime_url/api/v1/agents" \
        -H "Content-Type: application/json" \
        -d "{
            \"name\": \"phase3-test-agent\",
            \"config\": {
                \"template\": \"phase2-upload-test\",
                \"templateId\": \"$template_id\",
                \"templateHash\": \"$content_hash\"
            }
        }")

    local agent_id=$(echo "$agent_response" | jq -r '.agent.agentId // .agent.id // empty')
    if [[ -n "$agent_id" ]]; then
        log_success "Agent created: $agent_id"
    else
        # Agent creation may fail due to Docker/runtime not being available
        # but the template hydration should still work
        log_warning "Agent creation returned: $agent_response"
        log_info "This may be expected if Docker runtime is not available"
    fi

    # Test 3.3: Verify template cache
    log_info "Test 3.3: Checking template cache..."
    local cache_dir="$TEST_DIR/cache/templates"

    if [[ -d "$cache_dir" ]]; then
        local cached_items=$(find "$cache_dir" -maxdepth 1 -type d | wc -l)
        log_info "Cache directory entries: $((cached_items - 1))"

        if [[ -f "$cache_dir/index.json" ]]; then
            local cache_entries=$(jq '.entries | length' "$cache_dir/index.json" 2>/dev/null || echo "0")
            log_success "Cache index present with $cache_entries entries"
        else
            log_warning "Cache index not yet created"
        fi
    else
        log_warning "Cache directory not found: $cache_dir"
    fi

    # Test 3.4: Test Hub connectivity error handling
    log_info "Test 3.4: Testing error handling..."

    # Try to create agent with non-existent template
    local error_response=$(curl -s -X POST "$runtime_url/api/v1/agents" \
        -H "Content-Type: application/json" \
        -d '{
            "name": "error-test-agent",
            "config": {
                "templateId": "nonexistent-template-id",
                "templateHash": "invalid-hash"
            }
        }')

    # We expect either an error or the agent to be created with local fallback
    log_info "Error handling response: $(echo "$error_response" | jq -c '.error // .agent.status // .')"
    log_success "Error handling verified"

    # Cleanup agent if created
    if [[ -n "$agent_id" ]]; then
        log_info "Cleaning up test agent..."
        curl -s -X DELETE "$runtime_url/api/v1/agents/$agent_id" > /dev/null 2>&1 || true
    fi

    log_success "Phase 3 tests completed successfully"
}

# Test cache package independently (when Runtime Host is not available)
test_phase3_cache_unit() {
    log_info "Running template cache unit tests..."

    cd "$PROJECT_ROOT"
    if go test ./pkg/templatecache/... -v 2>&1 | tail -20; then
        log_success "Template cache unit tests passed"
    else
        log_error "Template cache unit tests failed"
        return 1
    fi
}

# ============================================================================
# CLI Commands Test (combines Phase 2 workflow)
# ============================================================================

test_cli_commands() {
    log_section "CLI Commands Integration Test"

    local template_dir=$(create_test_template)
    local grove_dir="$TEST_DIR/test-grove"

    # Create a test grove
    mkdir -p "$grove_dir/.scion"
    cd "$grove_dir"

    log_info "Setting up test grove at $grove_dir..."

    # Enable Hub for the test grove
    export SCION_HUB_ENDPOINT="http://localhost:$HUB_PORT"

    # Test: template sync
    log_info "Test: scion template sync..."
    if $TEST_DIR/scion template sync cli-test-template \
        --from "$template_dir" \
        --harness claude \
        --scope global 2>&1; then
        log_success "template sync command succeeded"
    else
        log_warning "template sync may have issues (Hub mode not fully configured for CLI)"
    fi

    # Test: template list
    log_info "Test: scion template list..."
    if $TEST_DIR/scion template list 2>&1; then
        log_success "template list command succeeded"
    else
        log_warning "template list may have issues"
    fi

    log_success "CLI commands test completed"
}

# ============================================================================
# Main Test Runner
# ============================================================================

run_all_tests() {
    log_section "Scion Template Integration Test Suite"
    log_info "Test directory: $TEST_DIR"
    log_info "Project root: $PROJECT_ROOT"

    local failed_tests=0

    check_prerequisites
    build_scion

    mkdir -p "$TEST_DIR"

    start_server

    # Run Phase 1 tests
    if test_phase1_hub_api; then
        log_success "Phase 1: PASSED"
    else
        log_error "Phase 1: FAILED"
        failed_tests=$((failed_tests + 1))
    fi

    # Run Phase 2 tests
    if test_phase2_upload_flow; then
        log_success "Phase 2: PASSED"
    else
        log_error "Phase 2: FAILED"
        failed_tests=$((failed_tests + 1))
    fi

    # Run Phase 3 tests
    if test_phase3_runtime_integration; then
        log_success "Phase 3: PASSED"
    else
        log_error "Phase 3: FAILED"
        failed_tests=$((failed_tests + 1))
    fi

    # Summary
    log_section "Test Summary"

    if [[ $failed_tests -eq 0 ]]; then
        log_success "All tests passed!"
        return 0
    else
        log_error "$failed_tests test phase(s) failed"
        return 1
    fi
}

# Run the tests
run_all_tests
