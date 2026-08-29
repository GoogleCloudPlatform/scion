# Phase 3 Security Audit Fixes

**Date:** 2026-08-29
**Branch:** scion/msg-authz
**Commit:** fix: address Phase 3 security audit findings (HIGH-1, HIGH-2, MEDIUM-1)

## Summary

Addressed 2 HIGH and 1 MEDIUM security findings from the Phase 3 msg-authz audit.

## Changes

### HIGH-1: handleGroupMessage per-recipient authorization

**File:** `pkg/hub/handlers_agent_messaging.go` — `handleGroupMessage`

The group[] recipient fan-out path delivered to each agent WITHOUT calling
`authorizeAgentMessage`. Only the anchor agent (URL path) was checked by the
routing layer. A regular member could deliver to none/lineage/branch agents by
including them in the group list alongside a project-mode anchor.

**Fix:** Added `authorizeAgentMessage` check inside the fan-out loop after
resolving each agent recipient. Unauthorized recipients get status "unauthorized"
and are skipped. Sender identity is extracted once outside the loop for efficiency.

### HIGH-2: processMentions per-mention authorization

**File:** `pkg/hub/handlers_agent_messaging.go` — `processMentions`

The direct API's `processMentions` fanned out mention messages to each mentioned
agent without calling `authorizeAgentMessage`. Chat v2's mention path was correctly
updated in Phase 3, but this one was missed.

**Fix:** Added `authorizeAgentMessage` check inside the fan-out loop after
resolving each mention agent. Unauthorized mentions get status "unauthorized" and
are skipped. Sender identity is extracted once outside the loop for efficiency.

### MEDIUM-1: Information leakage in broadcast response

**File:** `pkg/hub/handlers_agent_messaging.go` — `handleProjectBroadcast`

The broadcast response included `"skipped_breakdown": {"unauthorized": N}` which
leaked the count of restricted agents to the caller.

**Fix:** Removed the `skippedBreakdown["unauthorized"] = filtered` assignment.
The `skipped` total is still returned, but the breakdown no longer reveals how
many agents are mode-restricted.

## Test Updates

Updated `publish_guard_test.go` to inject admin identity in tests that exercise
group message and mention fan-out paths. These tests validate publish-on-persist
behavior, not authorization — the admin identity ensures `authorizeAgentMessage`
passes via the super-admin bypass (D6).

## Verification

- `go build ./...` passes
- All tests affected by these changes pass
- 2 pre-existing test failures (`TestTemplateResource_UATConfinement`,
  `TestScopedAdmin_ProjectAdminDeniedUnboundProject`) are unrelated to this change
