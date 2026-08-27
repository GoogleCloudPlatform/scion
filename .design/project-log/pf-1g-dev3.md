# Phase 1G Round 2 Fixes — Developer Log (pf-1g-dev3)

**Date:** 2026-08-25
**Branch:** `scion/auth-refactor`
**Base commit:** `6fd0c33` (fix(authz): harden delegation ceiling)

## Findings Addressed

### R2-1 (Blocker): Partial unique index on delegation_edges

**Problem:** The unique index on `(delegate_type, delegate_id, scope_type, scope_id, active)` including the `active` column caps both active AND inactive rows at one per (delegate, scope). A second revocation of the same agent in the same scope fails with a unique violation.

**Fix:**
- Changed index to a **partial unique index**: `(delegate_type, delegate_id, scope_type, scope_id) WHERE active = true` using `entsql.IndexWhere`.
- Only active rows are constrained — multiple inactive (revoked) rows are allowed.
- Added `deduplicateDelegationEdges()` migration pass before `AutoMigrate` to remove duplicate active rows from databases that ran the initial backfill at `3597507` and were interrupted.
- Extracted `tableExists()` helper from `accessPoliciesTableExists()` pattern.
- Regenerated Ent code via `go generate ./pkg/ent/`.

### R2-2 (Major): Inverted isMintingOperation default

**Problem:** `isMintingOperation` returned `false` by default — every future action added was automatically fail-open. Destructive actions like `ActionDelete`, `ActionStop`, `ActionAttach` were treated as fail-open.

**Fix:**
- Added `isReadOnlyOperation()` with an **allowlist** of safe operations (`ActionRead`, `ActionList`, `ActionVerify`). Default is `false` (fail-closed).
- Replaced all fail-open/fail-closed decision points to use `!isReadOnlyOperation()` instead of `isMintingOperation()`.
- Kept `isMintingOperation()` for the orphaned delegation minting check where the semantic distinction matters.
- Updated `handleOrphanedDelegation` to deny non-read-only mutations (not just minting) for orphaned delegations.

### R2-3 (Major): Unmapped permission grants access in handleOrphanedDelegation

**Problem:** `permissionToAgentScope` returning `""` was treated as "allow at baseline" in orphaned delegations. An unmapped permission should deny.

**Fix:**
- When `requiredScope == ""`, check the permissions registry to determine if the permission is a known read/list/verify action.
- If it IS a known read-class permission (exists in registry with read/list/verify action), allow at frozen ceiling level (implicit baseline access).
- If it is NOT in the registry (genuinely unmapped), **deny** and log the unmapped permission ID.

### R2-4 (Minor): backfillCompleted fails open on store error

**Problem:** `return err == nil` meant a store fault returned `false` (pre-backfill), which allowed agents without edges.

**Fix:**
- `ErrNotFound` → return `false` (genuinely pre-backfill)
- `nil` (marker found) → return `true` (post-backfill, require edges)
- Any other error → return `true` (fail closed — require edges)
- Added `sync.Once` caching on the `AuthzService` struct. The marker is write-once and never removed, so once found it latches permanently.
- Removed the misleading "cached for the lifetime" comment that was not previously true.

### R2-5 (Late addition): Cross-scope authority leak

**Problem:** `walkDelegationChain` did not filter edges by the request's scope. An edge in project P1 could satisfy the ceiling check for a request in P2.

**Fix:**
- Added `filterEdgesByRequestScope()` to filter the edge set by the request's `Resource.ParentType`/`ParentID` before counting and evaluating.
- Added `requestScopeFromResource()` to derive `(scopeType, scopeID)` from the request resource.
- The filter is applied in `walkDelegationChain` at every depth level, so the scope check is consistent throughout the chain walk.

## Tests Added

| Test | Verifies |
|------|----------|
| `TestDelegationEdge_CreateRevokeCreateRevoke` | R2-1: create→revoke→create→revoke must not fail |
| `TestIsReadOnlyOperation` | R2-2: read/list/verify are read-only, all others are not |
| `TestIsReadOnlyOperation_UnknownActionFailsClosed` | R2-2: future/unknown action defaults to fail-closed |
| `TestDelegationCeiling_DeleteFailsClosedOnOrphanedDelegation` | R2-2: ActionDelete/ActionStop fail closed on orphaned delegation |
| `TestDelegationCeiling_UnmappedPermissionOrphanedDeny` | R2-3: unmapped permission in orphaned delegation denies |
| `TestBackfillCompleted_PreBackfillReturnsFalse` | R2-4: returns false when marker absent |
| `TestBackfillCompleted_PostBackfillReturnsTrue` | R2-4: returns true when marker exists |
| `TestDelegationCeiling_CrossScopeAuthorityLeak` | R2-5: agent with edge in P1 denied for P2 request |
| `TestFilterEdgesByRequestScope` | R2-5: scope filtering function unit test |

## Files Modified

- `pkg/ent/schema/delegationedge.go` — partial unique index
- `pkg/ent/` (generated) — regenerated Ent code
- `pkg/hub/authz.go` — added `sync.Once`/`backfillDone` fields to `AuthzService`
- `pkg/hub/authz_delegation_ceiling.go` — all ceiling logic fixes
- `pkg/hub/delegation_ceiling_test.go` — new tests
- `pkg/store/entadapter/composite.go` — dedup call before AutoMigrate
- `pkg/store/entadapter/composite_migrations.go` — dedup function + `tableExists` helper

## Verification

- `go generate ./pkg/ent/` — passed
- `go test ./pkg/hub -timeout=600s -count=1` — passed
- `go test ./pkg/store/entadapter/ -count=1` — passed
- `make ci` — passed
