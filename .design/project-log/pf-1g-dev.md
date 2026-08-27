# Phase 1G: Live Delegation Ceiling (F1.7) — Developer Log

**Date:** 2026-08-25
**Author:** pf-1g-dev
**Branch:** `scion/auth-refactor`

## Summary

Implemented the live delegation ceiling mechanism for Phase 1G (F1.7). This ensures
agent authority is bounded by every live delegation edge up to the origin user — if
an ancestor loses authority, descendants lose it at the next authorization decision.

Additionally addressed a federated agent ancestry security vulnerability and
implemented the delegation edge migration/backfill.

## Work Items Completed

### WI1: DelegationEdge Schema and Store
- **New file:** `pkg/ent/schema/delegationedge.go` — Ent schema with fields for
  delegator/delegate type+ID, scope, role, active, grandfathered, timestamps.
  Four indexes for primary, reverse, scoped, and revocation lookups.
- **Modified:** `pkg/store/models.go` — Added `DelegationEdge` struct and
  `DelegationPrincipalUser`/`DelegationPrincipalAgent` constants.
- **Modified:** `pkg/store/store.go` — Added `DelegationEdgeStore` interface with
  CRUD operations.
- **New file:** `pkg/store/entadapter/delegation_edge_store.go` — Full Ent store
  adapter implementation.
- **Modified:** `pkg/store/entadapter/composite.go` — Wired `DelegationEdgeStore`
  into `CompositeStore`.

### WI2: Record Delegation Edges at Agent Creation
- **Modified:** `pkg/hub/handlers_agents_core.go` — Added `recordDelegationEdge`
  and `recordDelegationEdgeWithType` methods. Called after agent creation in
  `createAgentInProject`.
- **Modified:** `pkg/hub/server.go` — Added delegation edge recording in
  `dispatchAgentEventHandler` (scheduled dispatch path).

### WI3: Live Delegation Ceiling in Decide
- **New file:** `pkg/hub/authz_delegation_ceiling.go` — Core implementation:
  - Request-scoped caching via `context.Value`
  - `isMintingOperation` — identifies create/manage/register/addMember
  - `checkDelegationCeiling` — main entry point
  - `walkDelegationChain` — recursive chain walker (max depth 10)
  - `checkUserHoldsPermission` — verifies user delegator
  - `checkAgentHoldsPermission` — verifies agent delegator
  - `resolvePermissionID` / `permissionToAgentScope` — permission mapping
- **Modified:** `pkg/hub/authz.go` — Integrated delegation ceiling check in
  `Decide()` after `checkAccessForAgent` returns allow. Fails closed for minting
  operations on error.

### WI4: Remove Hardcoded Ceiling
- **Modified:** `pkg/hub/agentrole.go` — `ResolveEffectiveRole` now returns
  `minRole(requested, projectMax)` instead of `minRole(requested, userCeiling, projectMax)`.
- **Modified:** `pkg/hub/handlers_agents_core.go` — Removed `userCeiling := AgentRoleFull`.

### WI5: Federated Agent Ancestry Validation (Security)
Three fixes for the federated ancestry vulnerability:
1. **Fix 1** (`federation_auth.go`): Reject tokens where `ancestry[0] != root_user`.
2. **Fix 2** (`federation_auth.go`): Validate every ancestry element against
   `allowed_root_users`, not just `root_user`.
3. **Fix 3** (`identity.go` + `authz.go`): Created `AncestryIsHubAttested` predicate.
   In `checkDelegation`, ancestry fallback is gated on hub-attested identity.
   In `checkDelegationCeiling`, federated agents without edges resolve to floor
   (no authority), never unlimited.

### WI6: Legacy-Backfill Agent Marker
- **Modified:** `pkg/store/models.go` — Added `AgentRoleGrandfathered bool` to
  `AgentAppliedConfig` (provenance only, no decision path reads it).
- **Modified:** `pkg/store/entadapter/composite.go` — `BackfillEmptyAgentRoles`
  sets `cfg.AgentRoleGrandfathered = true` alongside `cfg.AgentRole = "full"`.

### WI7: Tests
- **New file:** `pkg/hub/delegation_ceiling_test.go` — 14 tests covering:
  - User→Agent chain with permission loss
  - Agent→Agent chain with cascading denial
  - Pre-migration agents grandfathered
  - New agents with edges enforced
  - Fail-closed for minting operations
  - Request-scoped caching
  - `isMintingOperation` classification
  - `AncestryIsHubAttested` predicate
  - Federated agent with no edge denied (floor)
  - Federated ancestry not used for delegation
  - `ResolveEffectiveRole` without user ceiling
  - Grandfathered marker store round-trip
  - JSON round-trip
  - DelegationEdge CRUD operations

### WI8: BackfillDelegationEdges
- **Modified:** `pkg/store/entadapter/composite.go` — Added `BackfillDelegationEdges`
  function following `BackfillEmptyAgentRoles` pattern:
  - Hub-settings idempotency marker (`delegation_edge_backfill_v1`)
  - Paginated agent query (500 per page)
  - Determines delegator from provenance: CreatedBy→user, Ancestry→parent agent,
    fallback→system/migration
  - Sets `grandfathered=true`, preserves current authority
  - Runs after `BackfillEmptyAgentRoles`
- **New file:** `pkg/store/entadapter/delegation_edge_backfill_test.go` — 3 tests:
  - User-created, agent-created, and ambiguous provenance agents get edges
  - Idempotent (second run no-op)
  - Authority preserved (no downgrade)

## Existing Test Updates
- **`pkg/hub/authz_request_test.go`**: Updated `TestAuthzDecideFederatedIdentitiesHaveExplicitOutcomes`
  — federated agents without delegation edges now correctly denied (Phase 1G security posture).
- **`pkg/hub/federation_e2e_test.go`**: Fixed `TestFederationE2E_FullSuccessPath`
  — ancestry[0] now agrees with root_user (Fix 1 compliance).
- **`pkg/hub/scheduler_test.go`**: Added `CreateDelegationEdge` no-op to
  `mockScheduledEventStore`.

## Design Decisions
- **Absent edge = floor for federated agents:** No store-recorded edge means no
  delegated authority. This prevents the D6-shaped defect where absent = unlimited.
- **Absent edge = grandfathered for local agents:** Pre-migration local agents without
  edges are allowed until the backfill runs and creates edges.
- **System/migration principal:** Agents with ambiguous provenance get edges to
  `system/migration` rather than being left unbounded.
- **Request-scoped caching only:** Delegation edge lookups are cached per-request via
  `context.Value`. No cross-request caching per advisory.

## Verification
- `go test ./pkg/hub -timeout=600s -count=1` — PASS
- `go test ./pkg/store/entadapter/ -count=1` — PASS
- `make ci` — PASS
