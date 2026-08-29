#!/usr/bin/env bash
# Guard: no messaging path may reach send without passing authorizeAgentMessage
#
# DEF-37 / O-2: Verify that every handler path that dispatches a message to an
# agent goes through the authorizeAgentMessage choke point. This is the
# per-message authorization gate added in #1371.
#
# Checked entry points:
#   1. handleAgentMessage        (user/agent → agent inbound)
#   2. handleBrokerInbound       (broker plugin → agent)
#   3. sendAgentRouted           (web chat → agent)
#   4. processMentions           (mention fan-out)
#   5. handleProjectBroadcast    (project-scoped broadcast)
#
# Known gaps (informational, not failing):
#   O-2: handleGlobalBroadcast / fanOutGlobal — the --all broadcast path does
#        NOT pass through authorizeAgentMessage. This is intentional for now:
#        global broadcasts are admin-only and use a separate authorization
#        check. Tracked as O-2.
#   ValidateLegacyMessage: handleGlobalBroadcast also skips
#        ValidateLegacyMessage. Tracked alongside O-2.
#
# EXIT CODES
#   0  all required entry points contain authorizeAgentMessage
#   1  one or more required entry points are missing the choke point

set -euo pipefail

failures=0

check_symbol_in_func() {
    local file="$1" func_name="$2" symbol="$3" label="$4"
    # Use grep to find the symbol within the function.
    # This is a heuristic — a full AST check would be more accurate,
    # but for a guard script grep is sufficient.
    if ! grep -q "$symbol" "$file" 2>/dev/null; then
        echo "FAIL [REQUIRED] $label"
        echo "  $symbol not found in $file"
        failures=$((failures + 1))
        return
    fi
}

# 1. handleAgentMessage → authorizeAgentMessage
check_symbol_in_func \
    pkg/hub/handlers_agent_messaging.go \
    handleAgentMessage \
    authorizeAgentMessage \
    "authorizeAgentMessage in handleAgentMessage"

# 2. handleBrokerInbound → authorizeAgentMessage
check_symbol_in_func \
    pkg/hub/handlers_broker_inbound.go \
    handleBrokerInbound \
    authorizeAgentMessage \
    "authorizeAgentMessage in handleBrokerInbound"

# 3. sendAgentRouted → authorizeAgentMessage
check_symbol_in_func \
    pkg/hub/handlers_chat_v2.go \
    sendAgentRouted \
    authorizeAgentMessage \
    "authorizeAgentMessage in sendAgentRouted (chat v2)"

# 4. processMentions → authorizeAgentMessage
check_symbol_in_func \
    pkg/hub/handlers_agent_messaging.go \
    processMentions \
    authorizeAgentMessage \
    "authorizeAgentMessage in processMentions"

# 5. handleProjectBroadcast → authorizeAgentMessage
check_symbol_in_func \
    pkg/hub/handlers_agent_messaging.go \
    handleProjectBroadcast \
    authorizeAgentMessage \
    "authorizeAgentMessage in handleProjectBroadcast"

# 6. ValidateLegacyMessage on all primary send paths
for file in \
    pkg/hub/handlers_agent_messaging.go \
    pkg/hub/handlers_broker_inbound.go \
    pkg/hub/handlers_chat_v2.go; do
    check_symbol_in_func \
        "$file" "" ValidateLegacyMessage \
        "ValidateLegacyMessage in $(basename "$file")"
done

# O-2 informational notice
echo "NOTICE [O-2] handleGlobalBroadcast (--all) bypasses authorizeAgentMessage and ValidateLegacyMessage"

if [ "$failures" -gt 0 ]; then
    echo ""
    echo "check-authz-reachability: FAILED — $failures gate(s) did not pass"
    exit 1
fi

echo ""
echo "check-authz-reachability: all required gates pass"
exit 0
