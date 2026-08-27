# Project Log: P2-B2 Quota Enforcement + Concurrency Test

**Date:** 2026-08-27
**Agent:** dev-pf-p2b-enforce
**Branch:** scion/pf-p2b-enforce
**Base:** main (b09e7f4)

## Summary

Implemented the quota enforcement layer for the limits/quotas subsystem (Phase 2B, PR-B2). This builds on the schema and store layer shipped in PR-B1, adding the enforcement logic that atomically checks and reserves quota using advisory locks.

## Deliverables

### 1. QuotaService (`pkg/hub/quota.go`)
- `CheckAndReserve`: atomic check-and-reserve using `TryAdvisoryLockObject` with `LockQuotaEnforcement` class ID
- `ResolveEffectiveLimit`: merge rule implementation — collects user, group, and system-default bindings, applies "most generous (max) wins"
- `Release`: best-effort release of reservations by resource ID, logging errors without blocking deletion

### 2. Advisory Lock Infrastructure
- Added `LockQuotaEnforcement` (0x5C101002) to `pkg/store/concurrency.go` as a per-scope advisory lock class ID
- Uses `StableProjectHash(scopeID)` for the object ID, following the same pattern as `LockWorkspaceProvision`
- Returns `ErrQuotaLockContention` when the lock is held, enabling callers to retry

### 3. Enforcement Hooks
**Creation-time hooks (check before store call):**
- `createAgentInProject` — enforces `max_agents_per_project` (scope: project)
- `createProject` — enforces `max_projects_per_user` (scope: system)
- `handleProjectRegister` — enforces `max_projects_per_user` (scope: system, broker-registration path)
- `addGroupMember` — enforces `max_members_per_group` (scope: group)

**Release hooks (after successful deletion):**
- `performAgentDelete` — releases agent reservation (both soft and hard delete paths)
- `deleteProject` — releases project reservation
- `removeGroupMember` — releases group member reservation

**Excluded from enforcement:**
- `createGroup` auto-add of creator as owner (not a quota-relevant event per brief)

### 4. Error Code
- Added `ErrCodeQuotaExceeded = "quota_exceeded"` to `pkg/hub/errors.go`
- Quota exceeded returns HTTP 429 (Too Many Requests)
- Infrastructure errors fail closed with HTTP 500

### 5. Server Wiring
- Added `quotaService *QuotaService` field to Server struct
- Initialized in `New()` after store is available

### 6. Tests (`pkg/hub/quota_test.go`)
**Critical acceptance test:**
- `TestQuotaConcurrency_100Creates_Limit10`: 100 goroutines, limit=10 → exactly 10 successes, 90 failures. Deterministic across 3 runs.

**Merge rule tests:**
- User in groups with limits 5 and 50 → effective=50 (most generous wins)
- User binding (100) + group binding (50) → effective=100
- System default (10) when no user/group binding
- No binding + DefaultValue=0 → unlimited
- No binding + DefaultValue=10 → capped at 10

**Functional tests:**
- No limit defined → no enforcement
- Unlimited default → no enforcement
- Exceeds quota → ErrQuotaExceeded
- Release decreases count (create 5, release 2, create more succeeds)
- Release is idempotent (non-existent reservation is no-op)
- Scopes are independent (project-1 full, project-2 still available)

### Test Infrastructure
- Created `lockingStoreWrapper` that provides real mutex-based advisory locking for SQLite testing (SQLite advisory locks are no-ops)

## Design Decisions

1. **Advisory lock contention**: Returns `ErrQuotaLockContention` instead of blocking. Callers retry, matching the existing try-lock pattern. The concurrency test confirms this works correctly.

2. **handleProjectRegister**: Enforced quota here too — this is the CLI broker-registration path and represents user-initiated project creation. It has user identity available.

3. **Scope matching**: System-scoped bindings apply to all scopes (they're global limits). Project-scoped bindings match only their specific project.

4. **Release uses `ReleaseReservationsByResource`**: Simpler than looking up the limit definition first — releases all reservations for the given resource ID regardless of limit type.

## Verification

- `make ci` passes
- Concurrency test passes deterministically (3 runs, all 10/90 split)
- All 12 quota tests pass
