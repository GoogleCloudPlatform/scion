# Phase 1F Fix Developer 2 — Round 1 Review Findings

**Date**: 2026-08-25
**Agent**: pf-1f-dev2
**Branch**: scion/auth-refactor

## Changes

### Fix 1: Extend CanDelegate to group-type member additions (R1 - REQUIRED)

**File**: `pkg/hub/handlers_groups.go`

The CanDelegate check in `addGroupMember` previously only fired for
`GroupMemberTypeUser` additions. When a user added a GROUP as a member of a
project members group, the CanDelegate check was bypassed, allowing privilege
escalation through group nesting (e.g., adding a group with owner-role members
without the actor holding owner-level authority).

**Fix**: Extended the condition to include `GroupMemberTypeGroup` additions:
```go
if s.authzService != nil && (req.MemberType == store.GroupMemberTypeUser || req.MemberType == store.GroupMemberTypeGroup) && isUserCaller {
```

The GrantDescriptor already uses the same `GrantTypeGroupMembership` pattern
with the `GroupRole` being assigned, so the CanDelegate logic correctly checks
whether the actor can delegate that role level regardless of whether the
member being added is a user or a group.

### Fix 2: Add tests for group-type nested member CanDelegate (R1 test coverage)

**File**: `pkg/hub/authz_candelegate_test.go`

Added three tests in Part D.2b:
- `TestCanDelegate_GroupMembership_AdminCannotAddGroupWithOwnerRole`: project-admin cannot add a group with owner role (escalation blocked)
- `TestCanDelegate_GroupMembership_OwnerCanAddGroupAsMember`: project-owner can add a group as member
- `TestCanDelegate_GroupMembership_SuperAdminCanAddGroupWithAnyRole`: super-admin can add any group with any role

### Fix 3: Remove dead code in IsSystemAdmin (O1)

**File**: `pkg/hub/authz.go`

Removed the unused `getEffectivePermissions` call and `_ = perms` discard in
`IsSystemAdmin`. The function now directly checks for a super-admin role
binding without the dead intermediate step.

### Fix 4: Add scheduled dispatch fire-time recheck test (O3)

**File**: `pkg/hub/authz_candelegate_test.go`

Added `TestCanDelegate_ScheduledDispatch_FireTimeRecheck` which demonstrates
the core value proposition of the fire-time CanDelegate recheck:
1. Creates a user with project-admin role
2. Verifies `authorizeScheduledAgentCreate` passes
3. Deletes the user's role binding (simulating permission revocation)
4. Verifies `authorizeScheduledAgentCreate` now fails

## Verification

- All 27 CanDelegate tests pass
- Full `pkg/hub/` test suite passes
- `make ci` passes
