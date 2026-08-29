# Phase 1: Messaging Authorization Schema + Registry + Role Seeds

**Date**: 2026-08-29
**Branch**: `scion/ma-1`
**Author**: dev-phase1-schema

## Summary

Implemented Phase 1 of the messaging authorization system (design decisions
D1-D10). This phase adds the foundational schema and registry changes with NO
behavioral/enforcement changes.

## Changes

### 1. Agent Schema: `message_mode` Field

- Added `message_mode` enum to `pkg/ent/schema/agent.go` with values:
  `none`, `lineage`, `branch`, `project` (default: `project`).
- Ran `go generate ./pkg/ent/...` to regenerate all Ent code.
- The Ent auto-migration sets a column-level DEFAULT, so existing agents are
  backfilled to `project` automatically.

### 2. Store Models

- Added `MessageMode string` field to `store.Agent` struct.
- Added `MessageMode*` constants to `pkg/store/models.go`.
- Added `MessageMode` to `store.TemplateConfig` for template support.
- Added `MessageMode` to `hubclient.Agent` for API-layer types.

### 3. Ent Adapter

- `entAgentToStore`: maps `a.MessageMode` (ent enum) to `string`.
- `CreateAgent`: conditionally sets `message_mode` when non-empty.
- `UpdateAgent`: conditionally sets `message_mode` when non-empty
  (avoids enum validator failure on agents created without the field).

### 4. Permission Registry

- Added `ActionMessage` and `ActionSetMessageMode` constants.
- Registered `agent.message`: project-scoped (`CapabilityScope`), UAT scope
  `agent:message`, NO `AgentScopes` (agents do not hold this; D5).
- Registered `agent.set_message_mode`: resource-scoped (`CapabilityResource`),
  NO `UATScope`, NO `AgentScopes` (human-only, interactive-session-only; D7).

### 5. Role Seeds

- Added `"message"` to `memberActions` in `projectMemberPermissionIDs()` so
  project members get `agent.message`.
- Project owner role automatically includes `agent.set_message_mode` via
  `projectScopedPermissionIDs()` (no code change needed).
- Added explicit exclusion of `agent.set_message_mode` from
  `projectPermissionIDsExcluding()` so project admins do NOT hold it (D7).
- Verified: no agent role scope includes `agent.message` or
  `agent.set_message_mode` (no `AgentScopes` on either permission).

### 6. Template/Parent Mode Inheritance

- In `createAgentInProject`: captures parent agent's `MessageMode` when a
  sub-agent is created.
- Resolution order: template config > parent mode > default (`project`).
- Follows D10 spawn defaults: child mode defaults to parent's mode; template
  may override to any mode.

### 7. Test Updates

- Updated `TestComputeScopeCapabilities_AdminGetsAllScopeActions` to expect
  `"message"` in agent scope actions.
- Pre-existing failure `TestTemplateResource_UATConfinement` is NOT caused by
  this change (confirmed by running on clean `main`).

## Verification

- `go build ./...` passes
- `go test ./pkg/store/...` passes
- `go test ./pkg/ent/...` passes
- `go test ./pkg/hub/...` passes (except pre-existing `TestTemplateResource_UATConfinement`)

## Critical Constraint Compliance

1. `agent.message` is NOT in any agent role scope.
2. `agent.set_message_mode` has NO UAT scope and NO agent scope (human-only).
3. `agent.set_message_mode` is in project OWNER role only, NOT admin.
4. Default `message_mode` for all agents is `project`.
5. No behavioral change; all existing authorization paths unchanged.
6. Super-admin bypass in `checkAccessForUser` not touched.
