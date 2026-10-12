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

## Follow-up noted

- `isBindingActive` in `pkg/hub/authz.go` treats `now == ExpiresAt` as
  active (`now.After`), while the kernel treats it as inactive. Aligning it
  is a separate change.

## Tests

`pkg/store/entadapter/role_store_membership_window_test.go`: expired and
future bindings are not membership; unbounded and in-window bindings are;
an expired owner row beside an active member binding resolves to member;
boundary instants match the kernel; unit table for `roleBindingActiveAt`.
