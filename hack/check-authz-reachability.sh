#!/usr/bin/env bash
# Guard: messaging handler files must contain authorizeAgentMessage
#
# DEF-37: FILE-LEVEL check that every pkg/hub/handlers_*.go file containing
# message dispatch calls also contains authorizeAgentMessage. This is the
# per-message authorization choke point added in #1371.
#
# SCOPE AND LIMITATIONS
#
# This script checks at the FILE level, not the function level. It verifies:
#   - Known handler files contain authorizeAgentMessage (REQUIRED gates)
#   - Any handlers_*.go file with dispatch calls also contains authorizeAgentMessage
#     (fail-closed scan via glob, catches new files)
#   - Known handler files contain ValidateLegacyMessage
#
# It CANNOT detect a new dispatching function inside an already-covered file
# that bypasses authorizeAgentMessage. That requires function-level AST
# analysis, which is out of scope for a bash guard.
#
# DESIGN: FAIL-CLOSED
#
# Handler files are discovered by glob (pkg/hub/handlers_*.go), not by a
# hardcoded list. A new handler file that dispatches without authz will
# fail the scan. Exemptions must be enumerated with reason and date.
#
# EXEMPT entry points (enumerated bypass, architect-approved):
#   E1. fanOutGlobal in messagebroker.go (admin-only --all broadcast,
#       separate authz via project-level permission check;
#       O-2 tracks tightening. 2026-08-29)
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
echo "--- Required gates (ValidateLegacyMessage — shape/content validation) ---"

# V1-V3: ValidateLegacyMessage on all primary send paths (pre-attribution)
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

echo ""
echo "--- Required gates (ValidateAttributed — post-attribution validation) ---"

# V4-V6: ValidateAttributed on all primary send paths (post-attribution).
# DEF-41: the legacy validation split. ValidateLegacyMessage checks shape/content
# before attribution; ValidateAttributed checks ConversationID after attribution
# has set a real one. Both halves must be present on every path that attributes
# a conversation — removing either half while the gate watches only the other
# would let a handler skip validation and stay green.
check_symbol_in_file \
    pkg/hub/handlers_agent_messaging.go \
    ValidateAttributed \
    "ValidateAttributed in handlers_agent_messaging.go"

check_symbol_in_file \
    pkg/hub/handlers_broker_inbound.go \
    ValidateAttributed \
    "ValidateAttributed in handlers_broker_inbound.go"

check_symbol_in_file \
    pkg/hub/handlers_chat_v2.go \
    ValidateAttributed \
    "ValidateAttributed in handlers_chat_v2.go"

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
echo "--- Fail-closed scan (glob: pkg/hub/handlers_*.go) ---"

dispatch_hits=0
for hfile in pkg/hub/handlers_*.go; do
    [ -f "$hfile" ] || continue
    # Skip test files
    case "$hfile" in *_test.go) continue;; esac
    # Check for message dispatch calls. We look for method CALLS (s.func or p.func)
    # to dispatch functions, not for function DEFINITIONS. A file that defines a
    # dispatch helper but is always called from an authorized context is safe;
    # a file that CALLS a dispatch function is an ingress point.
    #
    # Patterns: dispatchWithBrokerRetry, PublishUserMessage, PublishBroadcast,
    # managedAgentMessage are the four dispatch sinks. All use a leading dot
    # (\.Symbol) so they match method CALLS (s.Symbol) not function DEFINITIONS
    # (func ... Symbol). handlers_managed_agents.go defines managedAgentMessage
    # but does not call it via method syntax, so it correctly shows 0 hits.
    if grep -E -q 'dispatchWithBrokerRetry|\.PublishUserMessage|\.PublishBroadcast|\.managedAgentMessage' "$hfile" 2>/dev/null; then
        dispatch_hits=$((dispatch_hits + 1))
        if ! grep -q "authorizeAgentMessage" "$hfile" 2>/dev/null; then
            echo "FAIL [FAIL-CLOSED] $(basename "$hfile") contains dispatch calls but no authorizeAgentMessage"
            failures=$((failures + 1))
        else
            echo "  ok  $(basename "$hfile") — dispatch paths covered"
        fi
    else
        echo "  --  $(basename "$hfile") — no dispatch calls (not a messaging handler)"
    fi
done

# Self-test: the dispatch pattern must match at least one file. If zero files
# matched, the recogniser regex is broken and the fail-closed scan is inert.
if [ "$dispatch_hits" -eq 0 ]; then
    echo "FAIL [SELF-TEST] dispatch pattern matched zero handler files — recogniser is broken"
    failures=$((failures + 1))
fi

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
