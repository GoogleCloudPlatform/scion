# Phase 1I: Decision and Mutation Audit, Explain API

**Date**: 2026-08-25
**Branch**: `scion/auth-refactor`
**Base commit**: `de629ce`

## Summary

Implemented authorization decision audit, mutation audit, and the Explain API
endpoint for the hub authorization system. This adds observability into how
authorization decisions are made, tracks mutations to security-relevant
resources, and provides a debugging endpoint for authorization troubleshooting.

## Deliverables

### 1. Decision Audit
- **Ent schemas**: `DecisionAudit` (17 fields, 7 indexes) stores authorization
  decision records with principal, credential, resource, result, and matched
  policy info.
- **Store layer**: `DecisionAuditStore` interface + Ent adapter with Create,
  List, and DeleteBefore methods.
- **Emission**: `StoreDecisionAuditEmitter` fires async goroutines (with 1s
  timeout + recover) from `AuthzService.Decide()`. Deny decisions are always
  audited; allow decisions are sampled at a configurable rate.
- **No secrets**: Records never contain bearer tokens or raw secrets — only
  credential IDs and types.

### 2. Mutation Audit
- **Ent schema**: `MutationAudit` (13 fields, 5 indexes) records mutations to
  policies, groups, agent delegations, credentials, and project memberships.
- **Store layer**: `MutationAuditStore` interface + Ent adapter.
- **Handler emission**: `emitMutationAudit` on `*Server` extracts actor identity
  from context and fires async. Wired into:
  - Policy CRUD (create, update, delete, binding add/remove)
  - Group membership (add, remove, with CanDelegate result)
  - Agent lifecycle (delegation, credential revoke, suspend)
  - Auth (UAT revocation)
  - Projects (member add)
  - Scheduled dispatch CanDelegate re-check (both agent and user creators)

### 3. Explain API
- **Endpoint**: `POST /api/v1/authz/explain` (RouteAuthenticated)
- Users can explain their own authorization; super-admins can explain for any
  principal.
- Returns `allowed`, `reason`, `matchedPolicy`, `matchedGrant`, `policyId`,
  and a `trace` array of `DecisionStep` objects showing each evaluation step.
- `DecisionStep` and `ExplainTrace` added to `Decision` struct; `Explain bool`
  flag added to `AuthzRequest`.

### 4. Retention Controls
- `CleanupAuditRecords(ctx, retentionDays)` on `*Server` deletes decision and
  mutation audit records older than the specified number of days.
- `AuditRetentionDays` field added to `ServerConfig`.

## Key Implementation Details

- **Async goroutines use `context.WithTimeout(context.Background(), 1s)`** to
  prevent goroutine/memory leaks when the server shuts down. A `recover()` guard
  catches panics from closed database connections.
- **Test store cleanup**: Added `s.Close()` to `testServer` cleanup to release
  in-memory SQLite databases and prevent OOM across the full test suite (the 2
  new Ent schemas increased per-database memory footprint past the threshold).
- **Route classification**: Added `/api/v1/authz/explain` to both
  `route_metadata.go` and `route_classification_test.go`.

## Files Changed

### New files
- `pkg/ent/schema/decisionaudit.go` — Ent schema
- `pkg/ent/schema/mutationaudit.go` — Ent schema
- `pkg/ent/decisionaudit*.go` — Generated Ent code
- `pkg/ent/mutationaudit*.go` — Generated Ent code
- `pkg/store/entadapter/decision_audit_store.go` — Store adapter
- `pkg/store/entadapter/mutation_audit_store.go` — Store adapter
- `pkg/hub/audit_authz.go` — Core audit + explain implementation
- `pkg/hub/audit_authz_test.go` — 12 tests

### Modified files
- `pkg/store/models.go` — Added audit record/filter types
- `pkg/store/store.go` — Added audit store interfaces
- `pkg/store/entadapter/composite.go` — Wired audit sub-stores
- `pkg/hub/authz.go` — Added Explain, DecisionStep, audit emission
- `pkg/hub/server.go` — Wired emitter, registered route, config field
- `pkg/hub/route_metadata.go` — Route entry
- `pkg/hub/route_classification_test.go` — Classification entry
- `pkg/hub/handlers_policies.go` — 5 mutation audit calls
- `pkg/hub/handlers_groups.go` — 2 mutation audit calls
- `pkg/hub/handlers_agents_core.go` — 2 mutation audit calls
- `pkg/hub/handlers_agent_lifecycle.go` — 1 mutation audit call
- `pkg/hub/handlers_auth.go` — 1 mutation audit call
- `pkg/hub/handlers_projects_core.go` — 1 mutation audit call
- `pkg/hub/handlers_test.go` — Store cleanup in testServer

## Test Results

- All 12 new audit tests pass
- `go test ./pkg/hub -timeout=600s -count=1` passes (0 failures)
- `make ci` passes (formatting, vet, lint, all package tests, build)
