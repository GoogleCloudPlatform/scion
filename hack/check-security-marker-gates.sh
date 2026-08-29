#!/usr/bin/env bash
# Guard: security symbols from B5, #1322, #1338, and #1347 must remain in the
# handler files where they enforce authorization and identity invariants.
#
# This is the regression guard for the security fixtures that messaging-v2
# (scion/messaging-v2) would revert. The branch pre-dates B5 (#1343), #1322,
# #1347, and #1338 (DEF-31). A naive merge or cherry-pick from v2 silently
# deletes sender-identity derivation, DM ownership checks, agent authorization,
# and default-agent validation.
#
# WHAT THIS CHECKS
#
# Each gate row asserts that a named symbol appears a specific number of times
# inside a named enclosing function. Uses go/ast for exact identifier matching
# and function scoping — comments and substrings cannot produce false results.
#
# Gate categories:
#
#   REQUIRED — calls and definitions. The check runs or exists. A missing
#   REQUIRED site means the check no longer runs or no longer exists; that is
#   the revert. Gate HARD FAILS.
#
#   AUDIT — calls whose absence removes the only record of a security decision.
#   On a path that fails silently by design (no 403, the denied action is simply
#   dropped), the log carries the entire evidentiary weight. Gate HARD FAILS.
#   Distinguished from REQUIRED so the failure report can say "the denial is now
#   unrecorded" rather than "the check is gone."
#
#   INFORMATIONAL — doc-comment mentions. Prose, not behavior. Gate REPORTS
#   (printed as a notice) but does NOT fail. A gate that fails on a doc-comment
#   reword gets overridden without reading by the third occurrence.
#
# Gate rows:
#
#   authenticatedSender in handlers_agent_messaging.go:
#     REQUIRED: handleAgentMessage x1, handleGroupMessage x2,
#               handleProjectBroadcast x1, func definition x1
#     INFORMATIONAL: doc comment x1
#
#   validateDefaultAgent in handlers_chat_v2.go:
#     REQUIRED: handleCreateThread x1, handleTopicPatch x1, func definition x1
#     INFORMATIONAL: doc comments x3
#
#   authorizeAgentMessage in handlers_agent_messaging.go:
#     REQUIRED: handleProjectBroadcast x1 (per-recipient pre-filter)
#
#   authorizeAgentMessage in handlers_chat_v2.go:
#     REQUIRED: sendAgentRouted x2 (primary path + mention fan-out)
#
#   SenderID in messagebroker.go:
#     REQUIRED: fanOutToProject x3 (B5/R1 — broadcast self-skip by canonical ID)
#     REQUIRED: fanOutGlobal x3 (B5/R1 — global broadcast self-skip by canonical ID)
#
#   Broadcasted in handlers_agent_messaging.go:
#     REQUIRED: handleProjectBroadcast x1 (B5 — server-side broadcast forcing)
#
#   parseDMKeyIDs in handlers_agent_messaging.go:
#     REQUIRED: handleAgentOutboundMessage x1, handleAgentMessage x1 (#1322 — DM key ownership)
#
#   parseDMKeyIDs in handlers_chat_v2.go:
#     REQUIRED: func definition x1 (#1322 — must exist)
#
#   isDMParticipant in handlers_chat_v2.go:
#     REQUIRED: func definition x1 (#1322 — kind-label tightening, must exist)
#
#   handlers_broker_inbound.go (parallel entry point to handlers_agent_messaging):
#     REQUIRED: authorizeAgentMessage x1 in handleBrokerInbound (#1371 — message authorization)
#     REQUIRED: SenderID x4 in handleBrokerInbound (B5 — canonical sender identity)
#     REQUIRED: NewAuthenticatedUser x1 in handleBrokerInbound (B5 — server-derived identity)
#     REQUIRED: parseDMKeyIDs x1 in handleBrokerInbound (B5 — DM key ownership)
#
#   COMPOSITE: handleProjectBroadcast in handlers_agent_messaging.go must
#   contain BOTH authenticatedSender AND authorizeAgentMessage.
#
# EXIT CODES
#   0  all gates pass
#   1  one or more REQUIRED or AUDIT gates failed
#   2  could not analyse — a guarded file is missing, unreadable, or has syntax
#      errors. This is NOT a statement about the codebase; it means the
#      instrument could not run. Distinguished from exit 1 so CI can report
#      "nothing was analysed" rather than "the symbol is gone." GNU make
#      collapses both into exit 2, which is why CI invokes this script directly
#      rather than via `make`.
#
# IMPLEMENTATION
#
# The actual checking is done by a Go program (hack/checksecuritymarkergates)
# that uses go/ast for exact identifier matching and function scoping. This
# shell script compiles that program into a temp binary and exec's it.
#
# Why not `go run`? Because `go run` collapses all non-zero exit codes into 1,
# destroying the exit-1/exit-2 distinction that the CI step depends on.

set -euo pipefail

cd "$(dirname "$0")/.." || exit 2

tmpbin=$(mktemp)
trap 'rm -f "$tmpbin"' EXIT

if ! go build -o "$tmpbin" ./hack/checksecuritymarkergates 2>&1; then
  echo "ABORT: could not compile check-security-marker-gates" >&2
  echo "  Nothing was analysed. This is a build failure, not a guard failure." >&2
  exit 2
fi

exec "$tmpbin"
