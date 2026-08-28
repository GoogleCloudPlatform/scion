# Broker Authorization Fix

**Agent**: dev-pf-broker-authz-fix
**Date**: 2026-08-28
**Branch**: scion/pf-broker-authz-fix
**Status**: Complete

## Summary

Fixed two authorization gaps in message delivery paths identified by the broker authz audit (`broker-authz-audit.md`).

## Changes

### GAP 1 (HIGH): handleProjectBroadcast — No project-level authz for user callers

**File**: `pkg/hub/handlers_agent_messaging.go`

Any authenticated user could broadcast messages to all running agents in ANY project via `POST /api/v1/projects/{projectId}/broadcast`. The handler checked agent callers properly (ScopeAgentLifecycle + project isolation) but user callers only had an identity-presence check.

**Fix**: Added `ActionAttach` authorization check on the project resource for user callers, placed after the agent-caller block and before the request body parsing. Also handles `store.ErrNotFound` → 404 for nonexistent projects.

### GAP 2 (MEDIUM): sendAgentRouted — No ActionAttach on target agent

**File**: `pkg/hub/handlers_chat_v2.go`

The chat v2 path checked `ActionRead` on the project but did NOT check `ActionAttach` on the target agent before dispatching. The direct API path (`POST /api/v1/agents/{id}/message`) correctly gated on `ActionAttach`, creating an authorization bypass through the chat ingress.

**Fix (2a)**: Added `s.authorize(w, r, agentResource(primaryAgent), ActionAttach)` before `s.store.CreateMessage` so unauthorized messages are never persisted. Returns 403 on denial.

**Fix (2b)**: Added `s.authzService.CheckAccess` per mentioned agent in the fan-out loop. Uses `CheckAccess` directly (not `authorize`) to avoid writing a 403 to `w` mid-response — denied mentions are skipped silently with a warning log. This allows the primary agent message to proceed even when some mentions are denied.

## Tests

**File**: `pkg/hub/broker_authz_test.go`

- `TestBroadcast_UnauthenticatedDenied` — no identity → 403
- `TestBroadcast_UserWithoutProjectMembership` — member role, no project binding → 403
- `TestBroadcast_UserWithProjectAttach` — project owner → 202
- `TestBroadcast_AgentSameProject` — agent with lifecycle scope in same project → 202
- `TestBroadcast_AgentDifferentProject` — agent in different project → 403
- `TestBroadcast_NonexistentProject` — user targeting missing project → 404
- `TestSendAgentRouted_WithoutAttach` — non-member user → 403, no message persisted
- `TestSendAgentRouted_WithAttach` — project owner → 201
- `TestSendAgentRouted_MentionSkippedWithoutAttach` — owner with mentions → 201, both messages persisted

## Verification

`make ci` passes with all existing and new tests.
