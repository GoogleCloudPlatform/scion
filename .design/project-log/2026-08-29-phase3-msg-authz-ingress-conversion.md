# Phase 3: Message Authorization Ingress Conversion

**Date**: 2026-08-29
**Agent**: dev-phase3-ingress
**Branch**: scion/dev-phase3-ingress (based on scion/msg-authz)

## Summary

Converted the three remaining ingresses (chat v2, broadcast, broker inbound)
to use `authorizeAgentMessage` as the single choke point for messaging
authorization, completing the enforcement model from the msg-authz design doc
(Section 5, D1-D10).

## Changes

### Chat v2 (`pkg/hub/handlers_chat_v2.go`)

- **Primary agent**: Replaced `ActionAttach` check with `authorizeAgentMessage`
  in `sendAgentRouted`. Chat v2 is purely messaging, not PTY/attach.
- **Fan-out mentions**: Replaced `authzService.CheckAccess(ActionAttach)` with
  `authorizeAgentMessage` for each mentioned agent. Unauthorized mentions are
  marked as `"unauthorized"` in the response (same UX pattern as before).
- **DM participation checks**: Left intact as the brief requires (necessary
  but no longer sufficient).
- **Human-to-human messages**: No change needed (no agent recipient).

### Broadcast (`pkg/hub/handlers_agent_messaging.go`)

- **Agent callers**: Removed `ScopeAgentLifecycle` requirement per D1
  (messaging is a first-class axis, separate from lifecycle). Kept same-project
  fast-fail check.
- **User callers**: Replaced project-level `ActionAttach` with `ActionRead`
  as a fast-fail gate. The real authorization is per-recipient.
- **Per-recipient filtering**: Added `authorizeAgentMessage` filter on the
  `runningAgents` list before delivery. Owner's broadcast reaches
  lineage/branch agents; member's reaches only project-mode; none-mode agents
  never reached (except by super-admin).
- **Broker path**: When a broker proxy is available, publish per-agent messages
  through `proxy.PublishMessage` for each authorized agent (since
  `PublishBroadcast` fans out to ALL subscribed agents and cannot target a
  subset).
- **System-plane delivery failures**: `publishBroadcastDeliveryFailed` uses
  `messages.TypeSystem` and dispatches directly via `dispatcher.DispatchAgentMessage`,
  correctly bypassing authorization (hub-internal path, not an ingress).

### Broker Inbound (`pkg/hub/handlers_broker_inbound.go`)

- **User senders**: Replaced `ActionAttach` check with `authorizeAgentMessage`.
  The user identity is constructed from the resolved sender email (same as
  before) and passed to the choke point.
- **System/infrastructure senders**: Messages where the sender is not `user:`
  or `agent:` are system-plane messages (D8 — scheduled events, internal hub
  triggers). These bypass authorization unconditionally through broker HMAC trust.
- **Agent-to-agent via broker**: Documented as a known gap. The broker path
  for agent-to-agent is less common than the direct API, and constructing an
  `AgentIdentity` from the broker request is non-trivial (no agent token in
  the broker HMAC context).

### Ingress Parity Test (`pkg/hub/authorize_message_test.go`)

Added `TestAuthorizeAgentMessage_IngressParity` with 13 test cases covering:
- Owner, member, and agent senders
- Project, lineage, branch, and none mode targets
- System-plane bypass for none and lineage modes
- All cases verify the same `authorizeAgentMessage` function produces
  consistent decisions regardless of which ingress calls it.

## Key Design Decisions

1. **Per-recipient filtering for broadcast** rather than project-level gating:
   the broadcast handler now always succeeds (202) but delivers to 0 agents
   if none are authorized. Project-level `ActionRead` provides a fast-fail
   gate for outsiders.
2. **Broker path uses per-agent publish**: Since `PublishBroadcast` fans out
   to ALL subscribed agents, filtered broadcasts use `proxy.PublishMessage`
   per authorized agent instead.
3. **Agent-to-agent via broker is a known gap**: Documenting rather than
   implementing — the direct API path covers the common case, and the broker
   path would require constructing an `AgentIdentity` from broker context.

## Pre-existing Test Failures

Two tests were already failing before these changes (verified by running on
the unmodified msg-authz branch):
- `TestTemplateResource_UATConfinement` (UAT confinement for global templates)
- `TestScopedAdmin_ProjectAdminDeniedUnboundProject` (405 vs 403 on project PUT)

These are unrelated to messaging authorization.

## Verification

- `go build ./...` passes
- `go vet ./pkg/hub/...` passes
- All `authorizeAgentMessage` tests pass (15 tests)
- All broadcast tests pass (14 tests)
- All broker inbound tests pass (2 tests)
