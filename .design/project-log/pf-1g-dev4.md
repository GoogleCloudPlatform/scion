# Phase 1G Round 3 Fixes (pf-1g-dev4)

**Date:** 2026-08-25
**Branch:** `scion/auth-refactor`
**Parent commit:** `1bd2203`

## Summary

Fixed two findings from the Phase 1G Round 3 architect review on the delegation
ceiling implementation.

## Findings Addressed

### R3-1 (Blocker): Scope derivation silently denies most agent requests

**Problem:** `filterEdgesByRequestScope` derived the delegation scope from
`Resource.ParentType` and `Resource.ParentID`. Out of ~76 Resource literals in
`pkg/hub/`, only ~17 set `ParentType`. The rest mapped to `(system, "")` scope.
Since no system-scoped delegation edges exist, `filterEdgesByRequestScope`
returned empty for most requests, triggering the no-edge denial path.

Two specific faults:
- A resource that IS a project (`Resource{Type: "project", ID: project.ID}`)
  has no parent and mapped to system scope.
- Everything else that omits `ParentType` silently became system scope.

**Fix:** Derive the ceiling scope from the **principal** (agent's own project
via `AgentIdentity.ProjectID()`), not the resource. This is correct because
delegation edges are always created with the agent's project ID as scope.

Changes:
- `checkDelegationCeiling` now extracts the agent's project ID from the
  identity and passes it as the scope to `walkDelegationChain`.
- `walkDelegationChain` accepts explicit `scopeType`/`scopeID` parameters
  instead of deriving scope from the resource.
- Replaced `filterEdgesByRequestScope(edges, resource)` with
  `filterEdgesByScope(edges, scopeType, scopeID)`.
- Replaced `requestScopeFromResource` with `resourceProjectScope` which
  returns the project ID a resource belongs to (for cross-project detection).
- Cross-project requests (resource in a different project than the agent's own)
  are detected and denied: the scope is set to the resource's project, which
  won't match any of the agent's edges.

### R3-2 (Major): sync.Once latch can cache 'pre-backfill' permanently

**Problem:** `sync.Once` caches whichever answer arrives first in both
directions. If the first call to `backfillCompleted` lands before the marker
exists, `backfillDone` stays `false` for the entire process lifetime, allowing
edge-less agents indefinitely.

**Fix:** Replaced `sync.Once` + `bool` with `atomic.Bool`. The latch is
monotonic: only `false -> true` is cached. A `false` result is not cached and
re-queries the store on the next call, so the latch catches up as soon as the
backfill completes.

Changes in `authz.go`:
- Replaced `backfillOnce sync.Once` + `backfillDone bool` with
  `backfillDone atomic.Bool`.
- Changed import from `"sync"` to `"sync/atomic"`.

Changes in `authz_delegation_ceiling.go`:
- Rewrote `backfillCompleted` to use `atomic.Bool` fast-path + store query
  fallback pattern. Only `true` is latched; `false` triggers re-query.

## Tests Added

- `TestDelegationCeiling_ResourceWithNoParentType`: Resource built as
  `Resource{Type: "project", ID: projectID}` (no ParentType) must NOT be
  denied.
- `TestDelegationCeiling_CrossProjectDenied`: Agent with edge in P1, request
  targets P2, must be DENIED.
- `TestDelegationCeiling_ProjectSettingsNoParentType`: Production-shape
  project settings resource without ParentType must pass.
- `TestBackfillCompleted_MonotonicLatch`: Verifies that a false result is not
  cached and the latch catches up once the marker appears.
- `TestResourceProjectScope`: Unit tests for the new `resourceProjectScope`
  helper.

## Tests Updated

- `TestFilterEdgesByScope` (renamed from `TestFilterEdgesByRequestScope`):
  Updated to use `filterEdgesByScope` with explicit scope parameters.
- `TestBackfillCompleted_PreBackfillReturnsFalse`: Updated comment to reflect
  R3-2 monotonic latch behavior (false is NOT cached).

## Verification

- `go test ./pkg/hub/ -timeout=600s -count=1` — PASS
- `go test ./pkg/store/entadapter/ -count=1` — PASS
- `make ci` — PASS

## Files Changed

- `pkg/hub/authz.go` — replaced `sync.Once`+`bool` with `atomic.Bool`
- `pkg/hub/authz_delegation_ceiling.go` — principal-derived scope, monotonic
  backfill latch, replaced `filterEdgesByRequestScope`/`requestScopeFromResource`
- `pkg/hub/delegation_ceiling_test.go` — new R3-1/R3-2 tests, updated existing
