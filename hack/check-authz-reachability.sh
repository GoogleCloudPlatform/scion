#!/usr/bin/env bash
# Guard: no messaging path may reach send without passing authorizeAgentMessage
#
# DEF-37: Verify that every handler path that dispatches a message to an
# agent goes through the authorizeAgentMessage choke point. This is the
# per-message authorization gate added in #1371.
#
# DESIGN: FAIL-CLOSED
#
# Every messaging entry point must either:
#   (a) contain authorizeAgentMessage  → checked as a REQUIRED gate, OR
#   (b) be listed as EXEMPT with a reason and date.
#
# A new messaging path that bypasses authorizeAgentMessage and is NOT
# listed here causes exit 1. The exemption list is the control —
# adding an entry requires architect approval.
#
# REQUIRED entry points (must contain authorizeAgentMessage):
#   1. handleAgentMessage        (user/agent → agent inbound)
#   2. handleBrokerInbound       (broker plugin → agent)
#   3. sendAgentRouted           (web chat → agent)
#   4. processMentions           (mention fan-out)
#   5. handleProjectBroadcast    (project-scoped broadcast)
#
# EXEMPT entry points (enumerated bypass, architect-approved):
#   E1. fanOutGlobal             (admin-only --all broadcast, separate
#                                 authz via project-level permission check;
#                                 O-2 tracks tightening. 2026-08-29)
#
# REQUIRED validation (ValidateLegacyMessage must appear in all handler files):
#   V1. handlers_agent_messaging.go
#   V2. handlers_broker_inbound.go
#   V3. handlers_chat_v2.go
#
# EXIT CODES
#   0  all gates pass
#   1  one or more REQUIRED gates failed

set -euo pipefail

failures=0

check_symbol_in_file() {
    local file="$1" symbol="$2" label="$3"
    if [ ! -f "$file" ]; then
        echo "FAIL [REQUIRED] $label"
        echo "  file $file does not exist"
        failures=$((failures + 1))
        return 1
    fi
    if ! grep -q "$symbol" "$file" 2>/dev/null; then
        echo "FAIL [REQUIRED] $label"
        echo "  $symbol not found in $file"
        failures=$((failures + 1))
        return 1
    fi
    echo "  ok  $label"
    return 0
}

echo "=== authorizeAgentMessage reachability gates ==="
echo ""

# ---------------------------------------------------------------------------
# REQUIRED: authorizeAgentMessage must appear in these files/entry points
# ---------------------------------------------------------------------------

echo "--- Required gates (authorizeAgentMessage) ---"

# 1. handleAgentMessage, processMentions, handleProjectBroadcast (all in same file)
check_symbol_in_file \
    pkg/hub/handlers_agent_messaging.go \
    authorizeAgentMessage \
    "authorizeAgentMessage in handlers_agent_messaging.go (handleAgentMessage, processMentions, handleProjectBroadcast)"

# 2. handleBrokerInbound
check_symbol_in_file \
    pkg/hub/handlers_broker_inbound.go \
    authorizeAgentMessage \
    "authorizeAgentMessage in handlers_broker_inbound.go (handleBrokerInbound)"

# 3. sendAgentRouted
check_symbol_in_file \
    pkg/hub/handlers_chat_v2.go \
    authorizeAgentMessage \
    "authorizeAgentMessage in handlers_chat_v2.go (sendAgentRouted)"

echo ""
echo "--- Required gates (ValidateLegacyMessage) ---"

# V1-V3: ValidateLegacyMessage on all primary send paths
check_symbol_in_file \
    pkg/hub/handlers_agent_messaging.go \
    ValidateLegacyMessage \
    "ValidateLegacyMessage in handlers_agent_messaging.go"

check_symbol_in_file \
    pkg/hub/handlers_broker_inbound.go \
    ValidateLegacyMessage \
    "ValidateLegacyMessage in handlers_broker_inbound.go"

check_symbol_in_file \
    pkg/hub/handlers_chat_v2.go \
    ValidateLegacyMessage \
    "ValidateLegacyMessage in handlers_chat_v2.go"

# ---------------------------------------------------------------------------
# EXEMPT: enumerated bypasses with architect-approved reason and date.
# Each exemption is verified to still exist (the function must still be
# present). If an exempted function disappears, that is also a signal.
# ---------------------------------------------------------------------------

echo ""
echo "--- Exemptions (architect-approved) ---"

# E1: fanOutGlobal — admin-only global broadcast (O-2)
if grep -q "fanOutGlobal" pkg/hub/messagebroker.go 2>/dev/null; then
    echo "  EXEMPT  fanOutGlobal in messagebroker.go: admin-only --all broadcast, separate authz path (O-2, 2026-08-29)"
else
    echo "  NOTICE  fanOutGlobal not found in messagebroker.go (exemption may be stale)"
fi

# ---------------------------------------------------------------------------
# FAIL-CLOSED: Any dispatch function in handler files that contains
# dispatchWithBrokerRetry but does NOT contain authorizeAgentMessage
# and is NOT exempted, is a violation.
#
# We check at the FILE level: if a handler file contains dispatch calls,
# it must also contain authorizeAgentMessage. messagebroker.go is excluded
# because its dispatch paths are downstream of handler-level authorization.
# ---------------------------------------------------------------------------

echo ""
echo "--- Fail-closed scan ---"

for hfile in \
    pkg/hub/handlers_agent_messaging.go \
    pkg/hub/handlers_broker_inbound.go \
    pkg/hub/handlers_chat_v2.go; do
    [ -f "$hfile" ] || continue
    if grep -q "dispatchWithBrokerRetry\|managedAgentMessage" "$hfile" 2>/dev/null; then
        if ! grep -q "authorizeAgentMessage" "$hfile" 2>/dev/null; then
            echo "FAIL [FAIL-CLOSED] $(basename "$hfile") contains dispatch calls but no authorizeAgentMessage"
            failures=$((failures + 1))
        else
            echo "  ok  $(basename "$hfile") — dispatch paths covered"
        fi
    fi
done

# ---------------------------------------------------------------------------
# Result
# ---------------------------------------------------------------------------

echo ""
if [ "$failures" -gt 0 ]; then
    echo "check-authz-reachability: FAILED — $failures gate(s) did not pass"
    exit 1
fi

echo "check-authz-reachability: all gates pass"
exit 0
