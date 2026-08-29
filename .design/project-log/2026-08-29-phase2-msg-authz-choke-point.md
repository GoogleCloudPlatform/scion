# Phase 2: Messaging Authorization Choke Point + Direct API Conversion

**Date**: 2026-08-29
**Agent**: dev-phase2-authz
**Branch**: `scion/dev-phase2-authz` (based on `scion/msg-authz`)

## Summary

Implemented the `authorizeAgentMessage` choke point function and converted
the direct API ingress (`POST /api/v1/agents/{id}/message`) to use it.
This is Phase 2 of the messaging authorization refactor (design doc D1–D10).

## Changes

### New file: `pkg/hub/authorize_message.go`

Single enforcement function for all messaging authorization. Implements the
full Section 5 decision logic:

- **System plane bypass (D8)**: `isSystemPlane` flag, settable only by hub
  internals (self-message path, system notifications).
- **Super-admin bypass (D6)**: `IsUnscopedLocalPlatformAdmin` check before
  any mode evaluation.
- **User sender path**: mode-aware with ancestry check, project-owner
  piercing (owner only, not admin), and `agent.message` permission check
  for project-mode targets. Federated identity ancestry is not trusted
  (`AncestryIsHubAttested`).
- **Agent sender path**: fetches sender agent record, checks both-side
  modes, enforces project isolation, evaluates parent/child relationships
  for branch mode, denies all agent-to-agent edges for lineage mode (D4).
- **`isProjectOwner` helper**: stricter than `isProjectOwnerOrAdmin` —
  checks owner role only for messaging piercing.
- **`isDirectParentChild` helper**: checks ancestry arrays for direct
  parent/child relationship.

### Modified: `pkg/hub/handlers_agents_core.go`

`handleAgentAction` now routes `AgentActionMessage` through
`authorizeAgentMessage` before the action dispatch switch, instead of
falling through to the generic lifecycle/attach auth. Self-message path
preserved as system-plane bypass with `isSystemPlane: true`. Uses `goto
actionDispatch` to skip the legacy auth block.

### Modified: `pkg/hub/handlers_projects_core.go`

Added `"message"` to the project member policy actions in
`createProjectMembersGroupAndPolicy`. This enables project members to
message agents via the policy path without requiring `agent.attach` (D1/D2).

### Modified: `pkg/hub/bypass_census_test.go`

Added allowlist entry for the D6 super-admin check in
`authorize_message.go`.

### New file: `pkg/hub/authorize_message_test.go`

Comprehensive test suite covering all 8 required acceptance scenarios:

1. Baseline project-mode agent messaging without lifecycle scope
2. Mode-none agent denied (super-admin can deliver)
3. Member without attach can message but not attach
4. Lineage mode: ancestry users, non-lineage denied, owner piercing,
   no agent-to-agent edges
5. Branch mode: parent/child allowed, sibling denied, bridge test
   (project-mode in branch denied both directions)
6. Relay pinning: owner's agent denied delivery to restricted agents
7. None mode: denied for lineage owner and project owner, allowed for
   super-admin, attach still works
8. Cross-project agent-to-agent denied

Plus additional coverage: system plane bypass, nil inputs, broker denied,
mixed branch modes.

## Pre-existing Test Failures

Two tests fail on the base branch (before this change):
- `TestTemplateResource_UATConfinement/global_template_is_still_not_confined`
- `TestScopedAdmin_ProjectAdminDeniedUnboundProject`

Both confirmed pre-existing by running against the stashed base.

## Remaining Work (Phase 3+)

- Convert chat v2, broadcast, broker inbound to the choke point
- `set_message_mode` endpoint (single-agent + cascade)
- Agent-to-user messaging path (relevant for outbound)
