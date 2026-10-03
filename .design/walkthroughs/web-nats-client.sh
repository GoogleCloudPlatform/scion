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

# =============================================================================
# Web NATS + SSE — Client-Side Testing Walkthrough
# =============================================================================
#
# This script provides runnable tests for the NATS-to-SSE real-time event
# pipeline. It covers health checks, SSE subscriptions, end-to-end message
# flow, error cases, and connection lifecycle.
#
# Prerequisites:
#   - Server is running (see web-nats-server.sh)
#   - curl, jq (optional), nats CLI (for publishing)
#
# Usage:
#   chmod +x web-nats-client.sh
#   ./web-nats-client.sh [command]
#
# Commands:
#   health        Run health endpoint checks (sections 3, 7)
#   session       Obtain and print a dev session cookie
#   subscribe     Open an SSE connection to a project (section 4, 5)
#   errors        Test SSE error cases (section 4.4)
#   publish       Publish test events via NATS (section 5)
#   lifecycle     Run connection lifecycle tests (section 6)
#   degraded      Test NATS-disabled graceful degradation (section 7)
#   quick         Run the full quick-reference test sequence (section 13)
#   all           Run all non-interactive tests in sequence
#
# =============================================================================

set -euo pipefail

WEB_PORT="${PORT:-8080}"
BASE_URL="http://localhost:${WEB_PORT}"
# Test project/agent IDs used throughout
TEST_PROJECT="test-project"
TEST_AGENT="agent-001"

# ---------------------------------------------------------------------------
# Helper: colored output
# ---------------------------------------------------------------------------
info()    { printf '\033[1;34m[INFO]\033[0m    %s\n' "$*"; }
ok()      { printf '\033[1;32m[OK]\033[0m      %s\n' "$*"; }
warn()    { printf '\033[1;33m[WARN]\033[0m    %s\n' "$*"; }
err()     { printf '\033[1;31m[ERROR]\033[0m   %s\n' "$*"; }
section() { printf '\n\033[1;36m=== %s ===\033[0m\n\n' "$*"; }
step()    { printf '\033[1m--- %s ---\033[0m\n' "$*"; }

# ---------------------------------------------------------------------------
# Session cookie management
# ---------------------------------------------------------------------------
#
# With DEV_AUTH=true, any request to a protected route auto-creates a session.
# We capture the Set-Cookie header from the initial request.
#

get_session_cookie() {
    local cookie
    cookie=$(curl -s -D - "${BASE_URL}/" 2>&1 \
        | grep -i 'set-cookie' \
        | head -1 \
        | sed 's/.*: //' \
        | cut -d';' -f1)

    if [ -z "$cookie" ]; then
        err "Failed to obtain session cookie. Is the server running with DEV_AUTH=true?"
        exit 1
    fi

    echo "$cookie"
}

ensure_session() {
    if [ -z "${SESSION_COOKIE:-}" ]; then
        SESSION_COOKIE=$(get_session_cookie)
    fi
}

cmd_session() {
    section "Obtain Session Cookie"
    SESSION_COOKIE=$(get_session_cookie)
    ok "Session cookie: ${SESSION_COOKIE}"
    echo ""
    echo "Export for use in other commands:"
    echo "  export SESSION_COOKIE='${SESSION_COOKIE}'"
}

# ---------------------------------------------------------------------------
# 3. Health Endpoint Verification
# ---------------------------------------------------------------------------

cmd_health() {
    section "3. Health Endpoint Verification"

    # Liveness probe — always returns 200 if the server is running
    step "Liveness probe (/healthz)"
    info "Expected: 200 with status=healthy, timestamp, uptime"
    echo ""
    local healthz_code
    healthz_code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/healthz")
    curl -s "${BASE_URL}/healthz" | jq . 2>/dev/null || curl -s "${BASE_URL}/healthz"
    echo ""
    if [ "$healthz_code" = "200" ]; then
        ok "/healthz returned HTTP ${healthz_code}"
    else
        err "/healthz returned HTTP ${healthz_code} (expected 200)"
    fi

    echo ""

    # Readiness probe — includes NATS status
    #   Connected:    200 { "status": "healthy",   "nats": "connected" }
    #   Disconnected: 503 { "status": "unhealthy", "nats": "reconnecting" }
    #   NATS disabled:200 { "status": "healthy" }  (no nats field)
    step "Readiness probe (/readyz)"
    info "Expected: 200 with nats=connected (when NATS is up)"
    echo ""
    local readyz_code
    readyz_code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/readyz")
    curl -s "${BASE_URL}/readyz" | jq . 2>/dev/null || curl -s "${BASE_URL}/readyz"
    echo ""
    if [ "$readyz_code" = "200" ]; then
        ok "/readyz returned HTTP ${readyz_code}"
    elif [ "$readyz_code" = "503" ]; then
        warn "/readyz returned HTTP ${readyz_code} — NATS may be disconnected"
    else
        err "/readyz returned HTTP ${readyz_code}"
    fi
}

# ---------------------------------------------------------------------------
# 4. SSE Endpoint Tests
# ---------------------------------------------------------------------------
#
# The SSE endpoint is GET /events?sub=<subject>. It requires authentication
# (a valid session cookie or dev-auth).
#
# Subscription Scope Verification Matrix:
#
#   Page Route            Expected SSE URL                                     Scope
#   /projects             /events?sub=project.*.summary                        dashboard
#   /projects/:projectId  /events?sub=project.{projectId}.>                    project
#   /agents/:agentId      /events?sub=project.{projectId}.>&sub=agent.{agentId}.>  agent-detail
#
# The agent-detail scope includes both project-level and agent-level
# subscriptions. The project-level subscription keeps sidebar/breadcrumb state
# fresh; the agent subscription adds heavy events (harness output) for the
# detail view.
#
# Allowed Subject Prefixes:
#
#   project.  Project-scoped events (agent status, broker health)
#   agent.    Agent-scoped heavy events (harness output)
#   broker.   Broker-scoped events
#
# Subjects outside these prefixes are rejected with 400. Bare wildcards
# (">", "*") are rejected. All subjects must have at least two tokens
# (e.g., "project.myproject" minimum).
#

cmd_subscribe() {
    ensure_session
    local subject="${1:-project.${TEST_PROJECT}.>}"

    section "4/5. SSE Subscription"
    info "Subscribing to: ${subject}"
    info "Press Ctrl+C to close the connection"
    echo ""

    # The connection stays open and emits:
    #   id: 1
    #   event: connected
    #   data: {"connectionId":"sse-1","subjects":["project.test-project.>"]}
    #
    # Then :heartbeat <timestamp> comments every 30 seconds to keep alive.
    info "Expected: initial 'connected' event, then heartbeats every 30s"
    info "Publish events via: ./web-nats-client.sh publish"
    echo ""

    curl -N -H "Cookie: ${SESSION_COOKIE}" \
        "${BASE_URL}/events?sub=${subject}"
}

cmd_subscribe_multi() {
    ensure_session

    section "4.3. Multiple Subject Subscriptions"

    # Both formats are supported — multiple sub params or comma-separated.
    info "Opening SSE connection with two subjects:"
    info "  project.${TEST_PROJECT}.>"
    info "  agent.${TEST_AGENT}.>"
    info "Press Ctrl+C to close the connection"
    echo ""
    info "The 'connected' event will list all subjects."
    echo ""

    curl -N -H "Cookie: ${SESSION_COOKIE}" \
        "${BASE_URL}/events?sub=project.${TEST_PROJECT}.>&sub=agent.${TEST_AGENT}.>"
}

# ---------------------------------------------------------------------------
# 4.4 Subject Validation — Error Cases
# ---------------------------------------------------------------------------

cmd_errors() {
    ensure_session

    section "4.4. Subject Validation — Error Cases"

    # Missing subjects → 400
    step "Missing subjects (expect 400)"
    info "Expected: 400 — At least one subject is required"
    echo ""
    curl -s -w '\nHTTP Status: %{http_code}\n' \
        -H "Cookie: ${SESSION_COOKIE}" \
        "${BASE_URL}/events" | jq . 2>/dev/null || true
    echo ""

    # Bare wildcards → 400
    step "Bare wildcard '>' (expect 400)"
    info "Expected: 400 — Bare wildcards are not allowed"
    echo ""
    curl -s -w '\nHTTP Status: %{http_code}\n' \
        -H "Cookie: ${SESSION_COOKIE}" \
        "${BASE_URL}/events?sub=>" | jq . 2>/dev/null || true
    echo ""

    # Invalid prefix → 400
    step "Invalid prefix 'system.internal.>' (expect 400)"
    info "Expected: 400 — Subject must start with one of: project., agent., broker."
    echo ""
    curl -s -w '\nHTTP Status: %{http_code}\n' \
        -H "Cookie: ${SESSION_COOKIE}" \
        "${BASE_URL}/events?sub=system.internal.>" | jq . 2>/dev/null || true
    echo ""

    # Single-token subject → 400
    step "Single-token subject 'project.' (expect 400)"
    info "Expected: 400 — Subject must have at least two tokens"
    echo ""
    curl -s -w '\nHTTP Status: %{http_code}\n' \
        -H "Cookie: ${SESSION_COOKIE}" \
        "${BASE_URL}/events?sub=project." | jq . 2>/dev/null || true
    echo ""

    # NATS unavailable → 503
    # This test requires NATS to be stopped externally. If NATS is currently
    # down, the request should return 503. Otherwise this step is informational.
    step "NATS unavailable (expect 503 when NATS is down)"
    info "To test this case, stop your NATS server externally, then re-run:"
    info "  ./web-nats-client.sh errors"
    info ""
    info "Checking current response:"
    echo ""
    curl -s -w '\nHTTP Status: %{http_code}\n' \
        -H "Cookie: ${SESSION_COOKIE}" \
        "${BASE_URL}/events?sub=project.test.>" | jq . 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# 5. End-to-End NATS -> SSE Message Flow
# ---------------------------------------------------------------------------
#
# This is the core test: publish NATS messages and verify they arrive in
# the SSE stream.
#
# Terminal layout for manual testing:
#
#   T1  Web server (npm run dev)
#   T2  SSE listener (curl -N ...)          <- ./web-nats-client.sh subscribe
#   T3  NATS publisher (nats pub ...)       <- ./web-nats-client.sh publish
#

cmd_publish() {
    section "5. End-to-End NATS -> SSE Message Flow"
    info "Publishing test events to project.${TEST_PROJECT}.*"
    info "Make sure an SSE listener is open in another terminal:"
    info "  ./web-nats-client.sh subscribe"
    echo ""

    if ! command -v nats &>/dev/null; then
        err "nats CLI is required for publishing. Install from: https://github.com/nats-io/natscli"
        exit 1
    fi

    # 5.2 Agent status update
    step "5.2 Agent Status Update"
    info "Publishing: project.${TEST_PROJECT}.agent.status"
    # Expected in SSE stream:
    #   id: 2
    #   event: update
    #   data: {"subject":"project.test-project.agent.status","data":{"agentId":"agent-001","status":"running","sessionStatus":"idle"}}
    nats pub "project.${TEST_PROJECT}.agent.status" \
        '{"agentId":"agent-001","status":"running","sessionStatus":"idle"}'
    ok "Published agent status"
    echo ""

    # 5.3 Agent created event
    step "5.3 Agent Created Event"
    info "Publishing: project.${TEST_PROJECT}.agent.created"
    # Expected in SSE stream:
    #   id: 3
    #   event: update
    #   data: {"subject":"project.test-project.agent.created","data":{"agentId":"agent-002","name":"test-agent","template":"claude","status":"provisioning"}}
    nats pub "project.${TEST_PROJECT}.agent.created" \
        '{"agentId":"agent-002","name":"test-agent","template":"claude","status":"provisioning"}'
    ok "Published agent created"
    echo ""

    # 5.4 Agent deleted event
    step "5.4 Agent Deleted Event"
    info "Publishing: project.${TEST_PROJECT}.agent.deleted"
    nats pub "project.${TEST_PROJECT}.agent.deleted" \
        '{"agentId":"agent-001"}'
    ok "Published agent deleted"
    echo ""

    # 5.5 Project summary
    step "5.5 Project Summary"
    info "Publishing: project.${TEST_PROJECT}.summary"
    nats pub "project.${TEST_PROJECT}.summary" \
        "{\"projectId\":\"${TEST_PROJECT}\",\"name\":\"Test Project\",\"agentCount\":5,\"runningCount\":3}"
    ok "Published project summary"
    echo ""

    # 5.6 Agent-scoped heavy event
    step "5.6 Agent-Scoped Heavy Event"
    info "Publishing: agent.${TEST_AGENT}.event"
    info "NOTE: This should NOT appear on a project-only subscription."
    info "      It only appears on an agent-scoped subscription:"
    info "        ./web-nats-client.sh subscribe-multi"
    nats pub "agent.${TEST_AGENT}.event" \
        '{"type":"tool_use","data":"heavy harness output payload..."}'
    ok "Published agent-scoped event"
    echo ""

    # 5.7 Non-JSON payload
    # The SSE manager handles non-JSON payloads gracefully, wrapping them
    # as a string in the data field.
    step "5.7 Non-JSON Payload"
    info "Publishing: project.${TEST_PROJECT}.agent.log (plain text)"
    # Expected in SSE stream:
    #   id: N
    #   event: update
    #   data: {"subject":"project.test-project.agent.log","data":"plain text message"}
    nats pub "project.${TEST_PROJECT}.agent.log" "plain text message"
    ok "Published non-JSON payload"
    echo ""

    ok "All test events published"
}

# ---------------------------------------------------------------------------
# 6. Connection Lifecycle Tests
# ---------------------------------------------------------------------------

cmd_lifecycle() {
    ensure_session

    section "6. Connection Lifecycle Tests"

    # 6.1 Heartbeat verification
    step "6.1 Heartbeat Verification"
    info "Opening SSE connection for 35 seconds to observe heartbeats..."
    info "Heartbeats are SSE comments (:heartbeat <timestamp>) every 30 seconds."
    info "They are ignored by EventSource but keep TCP connections alive through proxies."
    echo ""
    timeout 35 curl -N -H "Cookie: ${SESSION_COOKIE}" \
        "${BASE_URL}/events?sub=project.${TEST_PROJECT}.>" 2>/dev/null || true
    echo ""
    ok "Heartbeat test complete"
    echo ""

    # 6.2 Client disconnect cleanup
    step "6.2 Client Disconnect Cleanup"
    info "Opening SSE connection for 3 seconds then disconnecting..."
    info "Check web server logs for cleanup messages (subscription removal)."
    echo ""
    timeout 3 curl -N -H "Cookie: ${SESSION_COOKIE}" \
        "${BASE_URL}/events?sub=project.${TEST_PROJECT}.>" 2>/dev/null || true
    echo ""
    ok "Client disconnect test complete — check server logs for cleanup"
    echo ""

    # 6.3 NATS reconnection
    # This test requires manually stopping and restarting NATS externally.
    step "6.3 NATS Reconnection (manual)"
    info "To test NATS reconnection:"
    info "  1. Stop your NATS server"
    info "  2. Watch web server logs for:"
    info "       [NATS] Disconnected"
    info "       [NATS] Reconnecting..."
    info "  3. Restart your NATS server"
    info "  4. Watch web server logs for:"
    info "       [NATS] Reconnected"
    info "  5. Publish a message — it should arrive on the SSE stream"
    echo ""
    info "Current /readyz status:"
    local readyz_code
    readyz_code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE_URL}/readyz" 2>/dev/null || echo "000")
    curl -s "${BASE_URL}/readyz" | jq . 2>/dev/null || curl -s "${BASE_URL}/readyz"
    if [ "$readyz_code" = "200" ]; then
        ok "/readyz is healthy (HTTP ${readyz_code})"
    else
        warn "/readyz returned HTTP ${readyz_code}"
    fi
    echo ""

    # 6.4 Last-Event-ID resume
    step "6.4 Last-Event-ID Resume"
    info "Opening SSE connection with Last-Event-ID: 5"
    info "The server starts numbering from the provided value (6, 7, ...)."
    info "NOTE: Missed events between disconnect and reconnect are NOT replayed."
    info "      The event ID is used for ordering continuity only."
    echo ""
    timeout 5 curl -N -H "Cookie: ${SESSION_COOKIE}" \
        -H "Last-Event-ID: 5" \
        "${BASE_URL}/events?sub=project.${TEST_PROJECT}.>" 2>/dev/null || true
    echo ""
    ok "Last-Event-ID resume test complete"
}

# ---------------------------------------------------------------------------
# 7. NATS-Disabled Graceful Degradation
# ---------------------------------------------------------------------------
#
# When the web server starts without NATS (no SCION_NATS_URL / NATS_URL),
# it should display:
#
#   +----------------------------------------------------------+
#   |  NATS: disabled                                          |
#   +----------------------------------------------------------+
#
# Expected behavior:
#
#   /healthz                200, no "nats" field
#   /readyz                 200, no "nats" field
#   /events?sub=project.>   503 Service Unavailable
#   Page loads (/projects)  Pages render normally, SSE silently not connected
#

cmd_degraded() {
    section "7. NATS-Disabled Graceful Degradation"
    info "To test degraded mode, start the server without NATS:"
    info "  DEV_AUTH=true npm run dev"
    info ""
    info "Then run these checks against the running server."
    echo ""

    step "Liveness probe (expect 200, no 'nats' field)"
    curl -s -w '\nHTTP Status: %{http_code}\n' "${BASE_URL}/healthz" | jq . 2>/dev/null || true
    echo ""

    step "Readiness probe (expect 200, no 'nats' field)"
    curl -s -w '\nHTTP Status: %{http_code}\n' "${BASE_URL}/readyz" | jq . 2>/dev/null || true
    echo ""

    ensure_session
    step "SSE endpoint (expect 503 Service Unavailable)"
    curl -s -w '\nHTTP Status: %{http_code}\n' \
        -H "Cookie: ${SESSION_COOKIE}" \
        "${BASE_URL}/events?sub=project.test.>" | jq . 2>/dev/null || true
    echo ""

    info "Verify page loads (/projects) render normally with SSE silently not connected."
}

# ---------------------------------------------------------------------------
# 8. Browser-Side Testing (Reference)
# ---------------------------------------------------------------------------
#
# These tests require a browser and cannot be automated in this script.
# They are documented here for reference.
#
# 8.1 SSE Connection in DevTools
#   1. Navigate to http://localhost:8080/projects
#   2. Open DevTools -> Network -> filter by "EventSource" or "events"
#   3. Verify an EventSource connection is opened to /events?sub=project.*.summary
#   4. The connection should show a "connected" event in the EventStream tab
#
# 8.2 Scope-Based Subscription Changes
#   1. Navigate to /projects       -> SSE URL: /events?sub=project.*.summary
#   2. Click into a project (abc)  -> Old SSE closes, new opens: /events?sub=project.abc.>
#   3. Click into agent (xyz)      -> SSE reconnects: /events?sub=project.abc.>&sub=agent.xyz.>
#   4. Navigate back to /projects  -> SSE reconnects: /events?sub=project.*.summary
#
# 8.3 Real-Time UI Updates
#   1. Navigate to a project detail page (e.g., /projects/test-project)
#   2. Publish: nats pub project.test-project.agent.status '{"agentId":"<id>","status":"stopped"}'
#   3. Verify the agent's status badge updates without a page refresh
#
# 8.4 Agent Created / Deleted
#   1. On a project detail page, publish a "created" event:
#      nats pub project.test-project.agent.created '{"agentId":"new-123","name":"New","status":"provisioning"}'
#   2. Verify the new agent appears in the agent list
#   3. Publish a "deleted" event:
#      nats pub project.test-project.agent.deleted '{"agentId":"new-123"}'
#   4. Verify the agent is removed from the list
#
# 8.5 Reconnection Behavior
#   1. Open a project page in the browser
#   2. Stop your NATS server
#   3. Console shows: [SSE] Reconnecting in Xms (attempt N)
#   4. Verify exponential backoff: 1s, 2s, 4s, 8s, 16s, 30s (capped)
#   5. Restart your NATS server
#   6. Verify [SSE] Connected appears and events resume
#
# 8.6 Page Unload Cleanup
#   1. Open DevTools Network tab
#   2. Navigate to a project page (SSE connection opens)
#   3. Close the tab or navigate away
#   4. Verify the SSE connection is closed (check server logs)
#

# ---------------------------------------------------------------------------
# 13. Quick Reference: Full Test Sequence
# ---------------------------------------------------------------------------

cmd_quick() {
    section "13. Quick Reference: Full Test Sequence"

    ensure_session

    # Health checks
    step "Health checks"
    info "/healthz:"
    curl -s "${BASE_URL}/healthz" | jq . 2>/dev/null || curl -s "${BASE_URL}/healthz"
    echo ""
    info "/readyz:"
    curl -s "${BASE_URL}/readyz" | jq . 2>/dev/null || curl -s "${BASE_URL}/readyz"
    echo ""

    # Open SSE listener in background
    step "Opening SSE listener in background"
    curl -N -H "Cookie: ${SESSION_COOKIE}" \
        "${BASE_URL}/events?sub=project.test.>" &
    local sse_pid=$!
    sleep 2
    echo ""

    # Publish test events
    if command -v nats &>/dev/null; then
        step "Publishing test events"

        nats pub project.test.agent.status '{"agentId":"a1","status":"running"}'
        nats pub project.test.agent.created '{"agentId":"a2","name":"new","status":"starting"}'
        nats pub project.test.agent.deleted '{"agentId":"a2"}'
        nats pub project.test.summary '{"projectId":"test","agentCount":1}'
        ok "Test events published"
        echo ""
    else
        warn "nats CLI not found — skipping publish tests"
    fi

    # Error cases
    step "Error cases"
    info "Missing subject (expect 400):"
    curl -s "${BASE_URL}/events" | jq . 2>/dev/null || true
    echo ""
    info "Bare wildcard (expect 400):"
    curl -s -H "Cookie: ${SESSION_COOKIE}" \
        "${BASE_URL}/events?sub=>" | jq . 2>/dev/null || true
    echo ""

    # Cleanup
    step "Cleanup"
    kill "$sse_pid" 2>/dev/null || true
    ok "SSE listener stopped"
    echo ""

    ok "Quick test sequence complete"
}

# ---------------------------------------------------------------------------
# Run all non-interactive tests
# ---------------------------------------------------------------------------

cmd_all() {
    section "Running All Non-Interactive Tests"

    cmd_health
    echo ""
    cmd_errors
    echo ""

    if command -v nats &>/dev/null; then
        cmd_publish
    else
        warn "Skipping publish tests (nats CLI not found)"
    fi

    echo ""
    cmd_lifecycle
    echo ""
    ok "All tests complete"
}

# ---------------------------------------------------------------------------
# Main entry point
# ---------------------------------------------------------------------------

usage() {
    echo "Usage: $0 [command] [args...]"
    echo ""
    echo "Commands:"
    echo "  health          Run health endpoint checks"
    echo "  session         Obtain and print a dev session cookie"
    echo "  subscribe [sub] Open SSE connection (default: project.${TEST_PROJECT}.>)"
    echo "  subscribe-multi Open SSE with project + agent subscriptions"
    echo "  errors          Test SSE error cases"
    echo "  publish         Publish test events via NATS"
    echo "  lifecycle       Run connection lifecycle tests"
    echo "  degraded        Test NATS-disabled graceful degradation"
    echo "  quick           Run the full quick-reference test sequence"
    echo "  all             Run all non-interactive tests in sequence"
    echo ""
    echo "Environment:"
    echo "  PORT              Web server port (default: 8080)"
    echo "  SESSION_COOKIE    Pre-set session cookie (auto-obtained if not set)"
}

case "${1:-}" in
    health)
        cmd_health
        ;;
    session)
        cmd_session
        ;;
    subscribe)
        cmd_subscribe "${2:-}"
        ;;
    subscribe-multi)
        cmd_subscribe_multi
        ;;
    errors)
        cmd_errors
        ;;
    publish)
        cmd_publish
        ;;
    lifecycle)
        cmd_lifecycle
        ;;
    degraded)
        cmd_degraded
        ;;
    quick)
        cmd_quick
        ;;
    all)
        cmd_all
        ;;
    -h|--help|help)
        usage
        ;;
    "")
        usage
        ;;
    *)
        err "Unknown command: $1"
        usage
        exit 1
        ;;
esac
