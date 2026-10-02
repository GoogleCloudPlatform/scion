# members-multirole hub cleanup (ptone/scion#2600, ptone/scion#2646 items 1-3)

**Branch**: `scion/mmr-hub-cleanup`, cut from GoogleCloudPlatform/scion main
(contains P1 GoogleCloudPlatform/scion#2273 and P2
GoogleCloudPlatform/scion#2320). Low-priority follow-ups from multi-role
project membership (ptone/scion#2529). No behaviour change in any item.

## (a) Legacy POST/PATCH refusals through the shared constructors (ptone/scion#2600)

- `checkGovernance` (used by `AddMember`, `UpdateMemberRole` and
  `RemoveMember`) returns `noProjectRoleDecision()`.
- `AddMember` returns `canDelegateRefusal`. `UpdateMemberRole` returns the
  new sibling `canDelegateRefusalFor(rd, "the new role", reason)`, which
  keeps its PATCH wording; `canDelegateRefusal` is now
  `canDelegateRefusalFor(rd, "the requested role", reason)`.
- The in-transaction "actor has no project role (re-evaluated under lock)"
  refusal is a new sibling, `noProjectRoleUnderLockDecision()`. It is used by
  `reevaluateActorTx`, and by `AddMember`/`UpdateMemberRole`, which now carry
  it out of `WithTx` as a `governanceDenialError` instead of the
  `governance:STATUS:REASON` string. `RemoveMember` and `TransferOwnership`
  still use the string form; they were out of scope.
- Byte compatibility: the shared CanDelegate constructor adds `Details` to
  the service decision. None of the three legacy HTTP callers (members
  POST, members PATCH, generic role-binding POST) render the decision's
  Details, so their response bodies do not change.
  `project_members_legacy_refusal_test.go` pins the exact bodies (status,
  code, message, details). It was written and passed on the unmodified
  code first.

## (b) One built-in role list (ptone/scion#2646 item 3)

- Removed `validProjectRoles`. Its four call sites now use
  `store.IsBuiltInProjectMembershipRole`.

## (c) One per-role decision (ptone/scion#2646 item 2)

- `memberRoleDecision` (`project_membership_role_decision.go`) is the
  per-role sequence: credential, structural `role_binding.*`, actor
  authority, governance, CanDelegate. A bitmask selects which checks run.
- `AssignableRoles` runs every check for one role at a time.
  `SetMemberRoles` runs one check at a time across the plan. That keeps
  which refusal a multi-role PUT reports first. Phase T reuses the function
  for the in-transaction structural and governance re-checks. CanDelegate
  is only ever selected before the transaction.
- `memberActorAuthorityPreTx` computes the actor side once and is shared by
  both callers. `customRoleAuthorities` is store-parameterised (`svc.store`
  before the transaction, `tx` inside it) and wraps
  `customRoleAuthorityFromStore`, which is still the single function that
  decides custom-role authority.
- `TestAssignableRoles_ConsistentWithPut` is unmodified and green.

## (d) Hub-override reachability on members PUT/DELETE (ptone/scion#2646 item 1)

The system-only hub override (`memberActorAuthorityPreTx` before the
transaction, `reevaluateActorTx` inside it) applies when the actor has no
built-in project role, direct or group-derived, and holds system-scope
`role_binding.*`. Both endpoints sit behind a `project.manage` gate, so the
actors who reach it are:

- **A super-admin who is not a member of the project.** Super-admin
  carries every permission at system scope, so it passes the gate and has
  `role_binding.*`. This is the common case, and it is real: platform
  admins managing a project they do not belong to.
  `TestSetMemberRoles_HubOverride_ReachableBySuperAdminOverHTTP` pins it
  (PUT grant 201, DELETE succeeds).
- **A holder of a custom role carrying `project.manage`** (project-scoped
  on this project, or system-scoped) who also holds system `role_binding.*`,
  for example hub-admin. Hub-admin alone lacks `project.manage` and fails
  the gate.

The issue described only the second case. The override bypasses just the
built-in matrix and the direct-owner rule. The structural guard,
custom-role authority, CanDelegate, the last-owner guard and the
in-transaction revalidation still apply.

**Recommendation: keep it as is.** Removing it would lock non-member
super-admins out of membership management on these endpoints. The
custom-role case is narrow and already bounded by CanDelegate. The comment
sits at the pre-transaction branch, with a pointer to it from
`reevaluateActorTx`.

## Notes

- Only the gate-scoped tests from the brief were run (broker throttle). Fork
  CI is the full gate.
- Adjacent, not done: `RemoveMember`/`TransferOwnership` still encode
  in-transaction denials as `governance:STATUS:REASON` strings, parsed by
  `isGovernanceError`. They could move to `governanceDenialError` and drop
  the string parser.

## Addendum: cleanup review r1 fixes

Review r1 on ptone/scion#2688 found two Low findings and one Nit. All three
are fixed here, plus one test-hygiene FYI. Tests and comments only; no
behaviour change.

- **C1-1:** added `TestAddMember_LegacyPOST_NoProjectRoleUnderLockRefusal`.
  A non-member super-admin passes the pre-transaction checks, the
  `mmrAuthoritySwapStore` seam revokes its system super-admin binding before
  the lock, and AddMember must return the exact under-lock decision with no
  binding created. Mutating AddMember's under-lock decision to
  `{x, y, 418}` makes the test fail, so the path is now pinned.
- **C1-2:** reworded the `memberRoleDecision` header and the AssignableRoles
  comment. What is shared is the per-check logic and the refusal
  constructors. The fixed order binds only AssignableRoles, which is also
  the only caller of the Credential and ActorAuthority checks. The PUT's
  stage order lives in SetMemberRoles and is pinned by
  `TestAssignableRoles_ConsistentWithPut`. The optional rewrite into
  per-check helpers was declined to keep churn down.
- **C1-3:** marked Phase T's fallback `return err` as defensive. The error
  text is unchanged.
- **FYI-3:** added `require.NotNil(t, d)` before reading `d.Details` in
  `TestSetMemberRoles_MemberRoleDecision_CheckSelectionAndOrder`.
