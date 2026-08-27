# Phase 1E: Security Audit — RoleDefinition, RoleBinding, Authorization Pipeline

**Date**: 2026-08-25
**Agent**: pf-1e-audit1
**Branch**: scion/auth-refactor
**Role**: Independent security auditor

## Summary

Security audit of Phase 1E (commits `9cc0722..97350b8`, 6 commits, ~10K insertions,
37 files). Focused on the RoleDefinition/RoleBinding data model, seed and backfill
logic, dual-read authorization pipeline, scoped credential isolation, and
transactional project membership.

**Verdict: APPROVE** — 0 Critical, 0 High, 1 Medium, 2 Low, 4 Info.

Full report at: `/scion-volumes/scratchpad/projects/auth-refactor/reports/pf-1e-audit1.md`

## Critical Constraints Verified

All five critical constraints from the Phase 1E audit brief are satisfied:

1. **Scoped credentials do NOT carry super-admin bypass** — UAT constraints
   enforced before admin bypass; `IsUnscopedLocalPlatformAdmin` rejects
   `ScopedUserIdentity`.
2. **Raw policy authoring remains super-admin-only** — All six policy handler
   endpoints call `requireAdmin`, no new bypass paths. No API endpoints expose
   role definition/binding CRUD.
3. **PolicyBoundary is NOT introduced** — Zero grep matches for `PolicyBoundary`.
4. **Role bindings do NOT allow privilege escalation** — Backfill mapping is
   1:1 (admin→super-admin, member→hub-member, viewer→hub-viewer), project
   mappings are 1:1, unknown roles skipped, non-user members skipped.
5. **Dual-read pattern does NOT fail open** — Both role-binding and legacy paths
   return `false` on all error paths. Empty inputs rejected.

## Findings

### [MEDIUM] Non-transactional project owner role binding creation

`pkg/hub/handlers_projects_core.go:407-417` — Role binding creation after project
persist is not in a database transaction. Failure is logged at CRITICAL level but
does not fail the request. Mitigated during migration by legacy group fallback;
code has a TODO for Phase 1F to make this a request-level error.

### [LOW] Super-admin permission set is seed-time only

`pkg/hub/seed.go:432,474-480` — `super-admin` role definition seeded with current
`allPermissionIDs()`. Future permissions added to registry won't auto-update.
Non-issue today because admin bypass doesn't consult permissions; will matter in
Phase 1F when switching to permission-based checks.

### [LOW] System-scoped bindings included in project scope queries

`pkg/hub/authz.go:967-974` — `getEffectivePermissions` includes system bindings
for project-scope queries. This is correct (system scope is broader), but should
be documented in code comments to prevent confusion.

## Test Results

- All Phase 1E tests pass (24 test cases in `role_binding_test.go`)
- 10 pre-existing test failures in unrelated areas (skills, templates, workspace
  handlers) — none in files modified by Phase 1E

## Positive Observations

- Fail-closed design throughout the authorization pipeline
- Idempotent seed and backfill (handles `ErrAlreadyExists`)
- Clean separation: `GroupMembership.Role` is governance-only
- Comprehensive test coverage including regression, idempotency, scope isolation
- All store queries use parameterized Ent ORM predicates (no SQL injection risk)
- No secrets in logs, debug output, or error messages
