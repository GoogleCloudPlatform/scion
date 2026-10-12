# Project membership reads honour role binding activation windows

**Date:** 2026-10-12
**Branch:** scion/mmr-fed-p1-0
**Tracked in:** ptone/scion#4603

## Problem

`GetProjectMembership`, `ListProjectMembers` and `IsProjectMember`
(`pkg/store/entadapter/role_store.go`) counted every project-scoped role
binding for a user, including bindings whose `NotBefore` is in the future or
whose `ExpiresAt` has passed. `GetProjectMembership` also picked the
highest-ranked binding before any window check, so an expired owner binding
outranked an active member or admin binding.

## Change

The three reads now keep only bindings active under the authorization
kernel's rule (`evaluateActivation`, `pkg/hub/authz_kernel.go`):

- an unset bound is unbounded;
- `NotBefore` is inclusive (active when now >= NotBefore);
- `ExpiresAt` is exclusive (active when now < ExpiresAt);
- the clock is the Go wall clock (`time.Now`), the same clock the hub hands
  the kernel. `RoleStore.now` is injectable for tests.

Filtering is done in Go after the query rather than with SQL predicates, so
the comparison uses the Go clock and the kernel's exact boundaries on every
database backend. The result sets are one user's or one project's bindings.

## Callers affected

- `handlers_agent_message_mode.go` (owner/admin role check for
  `set_message_mode`).
- `ListProjectMembers` callers: chat recipient lists
  (`chat_v2_no_recipient.go`, `handlers_chat_v2.go`) and the access
  constraint preview.
- `IsProjectMember` has no production callers.

The hub owner and member checks (`isProjectOwner`, `isActiveMember`,
`authorize_message.go`) already read bindings through
`activeDirectProjectRoles` / `isBindingActive` and do not call these store
methods.

## Left alone

- `ListRoleBindings`, `ListRoleBindingsForPrincipal(s)`, `GetRoleBinding`:
  they return bindings as stored for management listings and for the
  kernel, which evaluates the window itself.
- Group membership has no activation window.

## Hub helper aligned

`isBindingActive` in `pkg/hub/authz.go` treated `now == ExpiresAt` as active
(`now.After`), while the kernel treats it as inactive. It now delegates to
`bindingActivationOK`, so `ExpiresAt` is exclusive and a zero evaluation time
with any bound set is inactive, as in the kernel. Every caller passes
`time.Now()` or a service clock. The test identity expiry check
(`now.Before(ExpiresAt)`) already used the exclusive rule, so the clamp and the
binding check agree at the instant.

Every store-to-kernel bound conversion (`toCandidateBindings`,
`bindingActivationOK`, `getEffectivePermissions`, the access-constraint
preview) goes through `candidateActivationBounds`, which maps a set-but-zero
bound to the earliest non-zero instant. A set-but-zero `ExpiresAt` is therefore
expired in the kernel, the hub helpers and the store alike. Some handlers still
have their own inline window checks (`artifacts_host.go`,
`handlers_users_core.go`, `handlers_admin_effective_access.go`); they are left
for a later change.

## Tests

`pkg/store/entadapter/role_store_membership_window_test.go`: expired and
future bindings are not membership; unbounded and in-window bindings are;
an expired owner row beside an active member binding resolves to member;
boundary instants match the kernel; unit table for `roleBindingActiveAt`.
`pkg/hub/authz_test.go` `TestIsBindingActive_BoundariesMatchKernel` checks
`isBindingActive`, `bindingActivationOK` and the kernel candidate path against
a fixed expected result per case (boundaries, unbounded, set-but-zero bounds,
zero evaluation time). `TestEvaluate_StoredBindingWindowMatchesIsBindingActive`
runs stored bindings through `toCandidateBindings` and the kernel evaluator and
checks the decision matches `isBindingActive`.

## Follow-up: nil bindings and the store clock

A nil binding is never active: `roleBindingActiveAt` and `bindingActivationOK`
return false, and `candidateActivationBounds` returns `setZeroBound` for both
bounds (zero bounds would read as unbounded, which is active).
`activeRoleBindings` uses `time.Now` when the `RoleStore` has no clock set, and
still returns a new slice rather than filtering its input in place. Tests:
`TestRoleBindingActiveAt_NilBindingInactive`,
`TestActiveRoleBindings_NoClockUsesWallClock`,
`TestBindingActivationOK_NilBindingInactive`,
`TestCandidateActivationBounds_NilBindingNeverActive`.
