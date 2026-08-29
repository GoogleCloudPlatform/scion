# Phase 2 Backend: Messageability Metadata + Structured Rejection Errors

**Date**: 2026-08-29
**Author**: dev-phase2-backend
**Branch**: scion/ma-ui-em

## Summary

Extended the Hub API with viewer-relative messageability metadata on agent
list/detail responses and structured rejection errors for message authorization
denials. These backend capabilities support the UI messaging indicators
designed in Section 3.2 / 4.7 of the UI design doc.

## Changes

### New File: `pkg/hub/messageability.go`
- `AgentMessageability` struct: `canMessage`, `canReachViewer`, `reason`
- `AgentMessageabilityDetail` struct: extends base with `reachableAgentCount` and `reachableUserCount`
- `ComputeMessageability()` — evaluates forward (viewer→agent) and reverse (agent→viewer) reachability
- `ComputeMessageabilityDetail()` — adds reachable agent/user counts for detail endpoint
- `mapReasonToCode()` — maps authorization denial strings to structured codes:
  `mode_none`, `mode_none_sender`, `mode_lineage_no_ancestry`, `mode_branch_no_edge`,
  `mode_lineage_agent_to_agent`, `missing_permission`
- `agentIdentityFromAgent()` — constructs AgentIdentity from store.Agent for sender-side checks
- `getSenderMode()` — resolves sender's message mode for error details

### Modified: `pkg/hub/response_types.go`
- Added `Messageability interface{}` field to `AgentWithCapabilities`
- Updated `MarshalJSON`/`UnmarshalJSON` to include the new field

### Modified: `pkg/hub/handlers_agents_core.go`
- `listAgents`: injects `AgentMessageability` per agent when identity is present
- `getAgent`: injects `AgentMessageabilityDetail` with reachable counts
- Message action denial: uses `ErrCodeMessageDenied` with structured details

### Modified: `pkg/hub/handlers_chat_v2.go`
- Chat v2 message denial: uses `ErrCodeMessageDenied` with reason/mode details

### Modified: `pkg/hub/handlers_broker_inbound.go`
- Broker inbound denial: uses `ErrCodeMessageDenied` with reason/mode details

### Modified: `pkg/hub/errors.go`
- Added `ErrCodeMessageDenied = "message_denied"` constant

## Verification
- `go build ./...` passes
- `TestAuthorizeAgentMessage_IngressParity` — all 13 sub-tests pass
- `TestAuthorize*` suite passes
- `TestMessage*` suite passes
- Full hub test suite times out at 300s due to container resource limits (pre-existing)

## Design Decisions
- Used `interface{}` for the `Messageability` field to support both `AgentMessageability`
  (list) and `AgentMessageabilityDetail` (detail) without separate response types
- `computeCanReachViewer` uses a simplified reverse check (based on target agent's mode
  and ancestry) rather than a full reverse authorization call
- `countReachableUsers` is a simplified count based on ancestry length; a full project
  membership count would require additional store queries
- Broker inbound errors preserve the existing `sender`/`agent_slug` fields alongside
  the new structured fields for backward compatibility
