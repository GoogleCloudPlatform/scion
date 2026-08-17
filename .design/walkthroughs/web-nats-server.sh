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
# Web NATS + SSE — Server Setup Walkthrough
# =============================================================================
#
# This script sets up the web server component for the NATS-to-SSE real-time
# event pipeline (Milestones 7 and 8).
#
# It assumes a NATS server is already running and reachable at SCION_NATS_URL
# (default: nats://localhost:4222).
#
# Architecture:
#
#   Browser (EventSource) <-- SSE stream <-- Koa /events endpoint
#                                                |
#                                           SSEManager
#                                                |
#                                           NatsClient
#                                                |
#                                           NATS Server  (external, already running)
#                                                |
#                                      Hub / Runtime Broker
#                                      (publishes events)
#
# The pipeline is unidirectional: NATS messages published by the Hub or
# Runtime Broker flow through the web server's SSE Manager and arrive at the
# browser as Server-Sent Events. Subscriptions are declared via query
# parameters at connection time and are immutable for the connection lifetime.
#
# Prerequisites:
#   - Node.js 20+
#   - A running NATS server (see SCION_NATS_URL)
#   - curl, jq (optional) for verification
#   - A running Hub API (or DEV_AUTH=true for local development without one)
#
# Usage:
#   chmod +x web-nats-server.sh
#   ./web-nats-server.sh [start|status]
#
# =============================================================================

set -euo pipefail

NATS_URL="${SCION_NATS_URL:-${NATS_URL:-nats://localhost:4222}}"
WEB_PORT="${PORT:-8080}"
WEB_DIR="${WEB_DIR:-web}"

# ---------------------------------------------------------------------------
# Helper: colored output
# ---------------------------------------------------------------------------
info()  { printf '\033[1;34m[INFO]\033[0m  %s\n' "$*"; }
ok()    { printf '\033[1;32m[OK]\033[0m    %s\n' "$*"; }
warn()  { printf '\033[1;33m[WARN]\033[0m  %s\n' "$*"; }
err()   { printf '\033[1;31m[ERROR]\033[0m %s\n' "$*"; }

# ---------------------------------------------------------------------------
# Environment Variables Reference
# ---------------------------------------------------------------------------
#
#   SCION_NATS_URL    (none)           NATS server URL(s), comma-separated for clusters
#   NATS_URL          (none)           Fallback if SCION_NATS_URL is not set
#   NATS_TOKEN        (none)           Optional auth token for NATS connection
#   NATS_ENABLED      true if URL set  Explicitly enable/disable NATS
#   NATS_MAX_RECONNECT -1 (infinite)   Max reconnect attempts before giving up
#   DEV_AUTH          (none)           Set to "true" to bypass OAuth for local testing
#   PORT              8080             Web server port
#

# ---------------------------------------------------------------------------
# NATS connectivity check
# ---------------------------------------------------------------------------
#
# Verifies the external NATS server is reachable before starting the web
# server. Uses the nats CLI if available, otherwise falls back to a simple
# TCP check.
#

check_nats() {
    info "Checking NATS connectivity at ${NATS_URL}..."

    if command -v nats &>/dev/null; then
        if nats server check connection --server "${NATS_URL}" 2>/dev/null; then
            ok "NATS server verified via nats CLI"
        else
            warn "nats CLI check failed — NATS may not be running at ${NATS_URL}"
            warn "The web server will start anyway and attempt to connect/reconnect."
        fi
    else
        # Extract host:port from the URL for a basic TCP probe
        local host_port
        host_port="${NATS_URL#nats://}"
        if curl -s --connect-timeout 2 "telnet://${host_port}" >/dev/null 2>&1; then
            ok "NATS appears reachable at ${host_port}"
        else
            warn "Could not reach NATS at ${host_port}"
            warn "The web server will start anyway and attempt to connect/reconnect."
        fi
    fi
}

# ---------------------------------------------------------------------------
# Web Server Management
# ---------------------------------------------------------------------------
#
# Start the web server with NATS enabled. You should see in output:
#
#   +----------------------------------------------------------+
#   |  NATS: enabled (nats://localhost:4222)                    |
#   +----------------------------------------------------------+
#
# And shortly after:
#
#   [NATS] Connected to nats://localhost:4222
#   [NATS] Ready for SSE subscriptions
#

start_web() {
    check_nats

    info "Starting web server with NATS enabled..."

    if ! [ -d "${WEB_DIR}" ]; then
        err "Web directory '${WEB_DIR}' not found."
        err "Set WEB_DIR to the path of the web/ directory, or run from the project root."
        exit 1
    fi

    info "Web server will start in foreground. Press Ctrl+C to stop."
    echo ""
    echo "  SCION_NATS_URL=${NATS_URL}"
    echo "  DEV_AUTH=true"
    echo "  PORT=${WEB_PORT}"
    echo ""

    cd "${WEB_DIR}"
    SCION_NATS_URL="${NATS_URL}" DEV_AUTH=true PORT="${WEB_PORT}" npm run dev
}

# ---------------------------------------------------------------------------
# Status: check health of all components
# ---------------------------------------------------------------------------

check_status() {
    echo ""
    info "=== Component Status ==="
    echo ""

    # NATS connectivity
    check_nats
    echo ""

    # Web server health endpoints
    # Liveness probe — always 200 if server is running
    if curl -sf "http://localhost:${WEB_PORT}/healthz" >/dev/null 2>&1; then
        ok "Web /healthz: healthy"
        info "  $(curl -s "http://localhost:${WEB_PORT}/healthz")"
    else
        warn "Web /healthz: not reachable (server may not be running)"
    fi

    # Readiness probe — includes NATS status
    #   When connected:    200 { "status": "healthy",   "nats": "connected" }
    #   When disconnected: 503 { "status": "unhealthy", "nats": "reconnecting" }
    #   When NATS disabled:200 { "status": "healthy" }  (no nats field)
    local http_code
    http_code=$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:${WEB_PORT}/readyz" 2>/dev/null || echo "000")
    if [ "$http_code" = "200" ]; then
        ok "Web /readyz: healthy (HTTP 200)"
        info "  $(curl -s "http://localhost:${WEB_PORT}/readyz")"
    elif [ "$http_code" = "503" ]; then
        warn "Web /readyz: unhealthy (HTTP 503) — NATS may be disconnected"
        info "  $(curl -s "http://localhost:${WEB_PORT}/readyz")"
    else
        warn "Web /readyz: not reachable (HTTP ${http_code})"
    fi

    echo ""
}

# ---------------------------------------------------------------------------
# Graceful Shutdown Sequence
# ---------------------------------------------------------------------------
#
# When the web server receives SIGTERM or SIGINT, the shutdown order is:
#
#   1. SSE Manager closes all active SSE connections (unsubscribes NATS, ends streams)
#   2. NATS client drains (finishes in-flight messages) then closes
#   3. HTTP server closes
#   4. Force shutdown after 10 seconds if steps don't complete
#
# Expected server logs:
#
#   SIGTERM received. Shutting down gracefully...
#   [SSE] Closing all connections...
#   [NATS] Draining connection...
#   [NATS] Connection drained and closed
#   Server closed successfully
#
# To test graceful shutdown:
#
#   kill -SIGTERM $(pgrep -f "tsx.*index.ts")
#

# ---------------------------------------------------------------------------
# Server-Side Log Messages Reference
# ---------------------------------------------------------------------------
#
#   [NATS] Connected to ...              Successfully connected to NATS server
#   [NATS] Disconnected: ...             Lost connection, will attempt reconnect
#   [NATS] Reconnecting...               Actively attempting to reconnect
#   [NATS] Reconnected to ...            Successfully reconnected
#   [NATS] Draining connection...         Graceful shutdown in progress
#   [NATS] Connection drained and closed  Shutdown complete
#   [NATS] Ready for SSE subscriptions    Initial connection established
#   [SSE] Failed to subscribe to ...      Subscription error during connection setup
#   [SSE] Closing all connections...      Server shutdown closing SSE streams
#

# ---------------------------------------------------------------------------
# Main entry point
# ---------------------------------------------------------------------------

usage() {
    echo "Usage: $0 [start|status]"
    echo ""
    echo "Commands:"
    echo "  start   Check NATS connectivity and start the web server (foreground)"
    echo "  status  Check health of NATS and the web server"
    echo ""
    echo "Environment:"
    echo "  SCION_NATS_URL  NATS server URL (default: nats://localhost:4222)"
    echo "  NATS_URL        Fallback NATS URL if SCION_NATS_URL is not set"
    echo "  WEB_DIR         Path to web/ directory (default: web)"
    echo "  PORT            Web server port (default: 8080)"
}

case "${1:-start}" in
    start)
        start_web
        ;;
    status)
        check_status
        ;;
    -h|--help|help)
        usage
        ;;
    *)
        err "Unknown command: $1"
        usage
        exit 1
        ;;
esac
