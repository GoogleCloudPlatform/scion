# Project visitor role and membership rules (phase 1, item 2)

**Date:** 2026-10-12
**Branch:** scion/mmr-fed-p1-2 (stacked on scion/mmr-fed-p1-1)
**Tracked in:** ptone/scion#4603

## What changed

- **Schema.** `role_definitions.confers_membership` (bool, default true).
  Additive; every existing and custom role keeps conferring membership.
  `store.RoleDefinition.ConfersMembership` is a `*bool` (nil means true on
  create, so no existing constructor changes meaning); read it through
  `ConfersProjectMembership()`, which is false for a nil role definition.
- **Seed.** New built-in project role `project-visitor` (revision 1,
  `confers_membership = false`) holding exactly `agent.message`,
  `agent.list` and `agent.read`; no `project.read` and nothing that creates or
  changes a resource. Limiting agent list/read responses for visitors to
  name, slug and status is a later item. New permission
  `project.manage_visitors` on `project-owner` (revision 8) and
  `project-admin` (revision 7), not on `project-member`. It is registered
  with no user access credential scope, and listed in `pendingNonRouteRows`
  until the visitor grant routes check it (they must remove the entry).
  Reconciliation leaves alone an existing non-system role (or one that
  confers membership) under a no-membership built-in name, with a warning.
- **Store seam.** `CreateVisitorRoleBinding` binds only the system
  `project-visitor` role, at project scope, to a user principal whose
  account is `kind = federated` (pending or active), with optional
  NotBefore/ExpiresAt; anything else is `ErrVisitorBindingRefused`. It shares
  `CreateRoleBinding`'s validation through one internal function and runs on
  the store's client, so it joins a `WithTx` transaction. The general
  `CreateRoleBinding` keeps refusing federated accounts and now also refuses
  any role that does not confer membership (`ErrNonMembershipRoleRefused`).
  `CreateVisitorRoleBinding` is in the authorization audit's mutation
  symbols, so its first caller must be classified.
- **Membership rules skip non-membership roles.** Store reads
  `GetProjectMembership`, `ListProjectMembers`, `IsProjectMember` (query
  predicate). Hub: `ProjectMembershipEvidence` (and so the bearer gate,
  project admission, membership-loss reconciler and every other caller),
  `hasRelevantProjectAdmission`, `CheckEffectiveMembership` (and so the
  `members` inbound messaging policy and runtime material checks),
  `chatMemberProjectIDs`, `activeDirectProjectRoles` (`isActiveMember`,
  `isProjectOwner`, `isProjectOwnerOrAdmin`), the Members API list, the
  assignable roles list and the members editor PUT (invalid_role_set).
  The hub helper is `membershipBindings` (pkg/hub/membership_roles.go).

## Consumers left unchanged, and why

- Owner/admin counts and last-owner guards, `isProjectOwner` direct path,
  project deletion governance, `projectEffectiveRole`, multi-role migration:
  they match only owner/admin/member role IDs or names.
- Permission decisions (`decide`, list scopes, effective grants, constraint
  admin checks, the agent-create button): a visitor binding grants exactly
  its role's permissions, which is intended.
- System-scope and cleanup paths (super-admin and hub-member reconcile,
  cascades, admin binding listings): not project membership.

## Notes

- Role definitions for the membership skip are read with one
  `GetRoleDefinitionsByIDs` call, only when a candidate project binding
  exists. Membership evidence and project admission read the flag from
  `RolePermissions.ConfersMembership`; under the request-local authz input
  memo, membership evidence takes the definitions from the memo, so a
  memoized batch still loads them once. Without the memo, each project
  access check loads them once more, like the closure and bindings.
  `artifactHost.MemberScopes` filters the same way. A binding whose role
  definition is missing is not membership (fail closed); the Members API
  list keeps showing it as before.
- Experiment: visitor bindings can only be created through the seam,
  which has no caller yet. While `federation.project_visitors` is off, the
  role definition list and GET leave out the project-visitor role, so the
  admin pickers do not offer it.
