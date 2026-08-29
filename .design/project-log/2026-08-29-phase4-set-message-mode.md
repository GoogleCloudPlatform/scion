# Phase 4: set_message_mode Endpoint

**Date**: 2026-08-29
**Agent**: dev-phase4-mode
**Branch**: scion/msg-authz

## Summary

Implemented the `set_message_mode` endpoint (Phase 4 of messaging authorization), adding the ability to change an agent's message mode with cascade support and audit logging.

## Changes

### New Files
- `pkg/hub/handlers_agent_message_mode.go` — Handler and cascade implementation for set_message_mode action
- `pkg/hub/handlers_agent_message_mode_test.go` — 16 comprehensive tests covering all acceptance criteria

### Modified Files
- `pkg/api/agent_actions.go` — Added `AgentActionSetMessageMode` constant
- `pkg/hub/authz.go` — Added `ActionSetMessageMode` hub Action constant
- `pkg/store/models.go` — Added `IsValidMessageMode()` validation function
- `pkg/hub/handlers_agents_core.go` — Wired set_message_mode action routing (before generic lifecycle authz), added template MessageMode validation in agent creation
- `pkg/hub/handlers_projects_core.go` — Wired set_message_mode in project-scoped agent action handler
- `pkg/hub/template_handlers.go` — Added MessageMode validation in template create and update handlers
- `pkg/hub/bypass_census_test.go` — Added allowlist entry for the D7 project-admin exclusion check

## Design Decisions

### D7 Enforcement: Project Admin Exclusion
The generic authz engine has a blanket project owner/admin bypass that grants all actions. Since D7 restricts set_message_mode to project owners (not admins), the handler explicitly checks for and denies project admins before calling `authorize()`. Super-admins are excluded from this gate via `IsUnscopedLocalPlatformAdmin()`.

### Handler Architecture
Created a new file (`handlers_agent_message_mode.go`) rather than adding to the already-large `handlers_agents_core.go`. The handler follows the existing pattern: validate → fetch → authz → update → audit → respond.

### Cascade Design
Cascade is best-effort per agent: failures on individual descendants are logged but don't block the overall operation. Each cascaded update gets its own audit record with mutation type `agent_set_message_mode_cascade`.

## Test Coverage

All 12 required test cases plus 4 additional tests:
1. Project admin denied (D7)
2. Project owner allowed
3. Lineage owner (ancestry) allowed
4. Agent callers denied (D7)
5. UATs denied (D7, no scope)
6. Super-admin allowed
7. All 12 mode transitions legal
8. Cascade operation (root→child→grandchild)
9. Quarantine: mode=none → delivery denied, system-plane still works
10. Live effect: mode change takes effect on next delivery
11. Invalid mode rejected (400)
12. Template MessageMode validation
13. No-op detection (same mode, no cascade)
14. IsValidMessageMode unit test
15. Audit event emission
16. API routing verification

## Verification

- `go build ./...` — passes
- All new tests pass: `go test ./pkg/hub/... -run "TestSetMessageMode|TestIsValidMessageMode" -count=1`
- Pre-existing test failures (`TestTemplateResource_UATConfinement`, `TestScopedAdmin_ProjectAdminDeniedUnboundProject`) are unrelated to these changes
