# Domain Governance Appendix: Project Membership

**Status:** Phase 1 (CT1) — approved and frozen
**Decision authority:** `ptone@google.com`
**Approval date:** 2026-09-01
**Parent contract:** `authorization-operation-contract.md`
**Implementation plan:** `authz-audit-implementation-plan.md` (scratchpad)
**C0 containment:** `authz-audit-findings.md`
**Decision record:** CT1 decision packet (D1–D8)

## Purpose

This appendix defines the approved governance rules for project membership
operations. It is the first domain governance appendix, not the canonical
abstraction. Other domains (constraints, credentials, group membership) will
have their own appendices using the same domain-neutral vocabulary from the
parent contract.

## 1. Project Roles

| Role             | Scope     | Principal kinds        | Notes                          |
|------------------|-----------|------------------------|--------------------------------|
| `project-owner`  | project   | Direct user only       | At least one must always exist |
| `project-admin`  | project   | Direct user only (C0); groups approved (D3) | Group eligibility approved but not yet implemented |
| `project-member` | project   | User, agent, or group  | Standard access                |

**D3 status:**
- `project-owner` remains permanently direct-user-only.
- `project-admin` group eligibility is approved. C0 currently blocks it via
  `directUserOnlyProjectRoles`; removal is a Phase 3 reference implementation task.
- Groups may never hold `project-owner`.

## 2. Governance Matrix (Approved)

This matrix specifies which actor roles may perform which membership operations
on which target roles. Approved by decision authority; implementation deferred
to RS1 typed governance.

| Operation                            | project-member | project-admin | project-owner |
|--------------------------------------|:--------------:|:-------------:|:-------------:|
| View direct and effective members    | Yes (D8)†      | Yes           | Yes           |
| Add an ordinary member               | No             | Yes           | Yes           |
| Change or remove an ordinary member  | No             | Yes           | Yes           |
| Add, promote, demote, or remove admin| No             | No            | Yes           |
| Add or promote an owner              | No             | No            | Yes           |
| Demote or remove an owner            | No             | No            | Yes‡          |
| Change a binding outside this project| No             | No            | No            |

†D8: Members may view direct and effective project access under `project.read`,
subject to group-domain privacy boundaries. Mutation capabilities are governed
separately by the typed governance matrix.
‡Subject to last-owner invariant: at least one active direct owner must remain.

### Current C0 containment

C0 restricts **all** membership mutations (POST, PATCH, DELETE) to
`project-owner` only. This is more restrictive than the approved matrix, which
allows admins to manage ordinary members. The containment is temporary
and must not be relaxed without:

1. Implementation of target-role governance checks (RS1).
2. Regression tests for each actor/target combination.

### Group membership governance (D3)

Binding a project role to a group is the project-governed delegation point.
Subsequent group membership mutations are governed solely by group roles and
bindings, with **no linked project-authority verification**.

**Temporary over-restriction:** The current `canDelegateGroupMembership`
implementation performs a cross-domain effective-authority scan that checks
project-level authority when mutating groups that hold project roles. This
does NOT match the approved decision and is explicitly recorded as a temporary
over-restriction to be removed in the appropriate Phase 3 reference work.
The approved behavior is: group membership governance uses only group roles,
not linked project authority.

## 3. Operation Contracts

### 3.1 project.membership.add

| Field               | Value                                                     |
|---------------------|-----------------------------------------------------------|
| **ID**              | `project.membership.add`                                  |
| **Domain**          | `project.membership`                                      |
| **Entry points**    | `POST /api/v1/projects/{id}/members`                      |
| **Principals**      | `user`                                                    |
| **Credentials**     | `session_jwt`, `scoped_uat`                               |
| **Resolver**        | `project-from-url`                                        |
| **Base permission** | `project.manage`                                          |
| **Effects**         | `grant-authority`                                         |
| **Delegation**      | `non_amplification`: actor must hold all target role perms|
| **Authority eval**  | `none`                                                    |
| **Governance**      | `peer_superior`: see matrix above                         |
| **Invariants**      | Binding scope matches role scope type (business)          |
| **Audit**           | `membership.add` — context: actor_id, project_id; after: target_principal, role; atomic |
| **Denial codes**    | `role_assignment_forbidden`, `target_role_protected`, `principal_ineligible` |
| **Tests**           | `pkg/hub:TestProjectMembership_*`, `pkg/hub:TestC0_*`    |

### 3.2 project.membership.update

| Field               | Value                                                     |
|---------------------|-----------------------------------------------------------|
| **ID**              | `project.membership.update`                               |
| **Domain**          | `project.membership`                                      |
| **Entry points**    | `PATCH /api/v1/projects/{id}/members/{bindingID}`         |
| **Principals**      | `user`                                                    |
| **Credentials**     | `session_jwt`, `scoped_uat`                               |
| **Resolver**        | `project-from-url`                                        |
| **Base permission** | `project.manage`                                          |
| **Effects**         | `change-authority`                                        |
| **Delegation**      | `conditional_on_increase`: delegate only when authority grows |
| **Authority eval**  | `before_and_after`: compare old/new effective authority    |
| **Governance**      | `peer_superior`: see matrix; both old and new role checked|
| **Invariants**      | `last-owner-guard` (security, fail-closed)                |
| **Audit**           | `membership.update` — context: actor_id, project_id; before: old_role; after: new_role, target_principal; atomic |
| **Denial codes**    | `role_assignment_forbidden`, `target_role_protected`, `last_owner` |
| **Tests**           | `pkg/hub:TestProjectMembership_*`, `pkg/hub:TestC0_*`    |

### 3.3 project.membership.remove

| Field               | Value                                                     |
|---------------------|-----------------------------------------------------------|
| **ID**              | `project.membership.remove`                               |
| **Domain**          | `project.membership`                                      |
| **Entry points**    | `DELETE /api/v1/projects/{id}/members/{bindingID}`        |
| **Principals**      | `user`                                                    |
| **Credentials**     | `session_jwt`, `scoped_uat`                               |
| **Resolver**        | `project-from-url`                                        |
| **Base permission** | `project.manage`                                          |
| **Effects**         | `revoke-authority`                                        |
| **Delegation**      | `none`: revocation does not grant permissions             |
| **Authority eval**  | `none`                                                    |
| **Governance**      | `peer_superior`: see matrix; revoked role checked         |
| **Invariants**      | `last-owner-guard` (security, fail-closed)                |
| **Audit**           | `membership.remove` — context: actor_id, project_id; before: removed_role, target_principal; atomic |
| **Denial codes**    | `role_assignment_forbidden`, `target_role_protected`, `last_owner` |
| **Tests**           | `pkg/hub:TestProjectMembership_*`, `pkg/hub:TestC0_*`    |

**Note on revocation delegation:** The previous contract claimed non-amplification
was required for revocation (actor must hold all revoked role perms). This is
incorrect: removing authority does not grant the actor any new permissions.
Revocation requires governance (actor/target relationship) but not delegation.

### 3.4 project.membership.list

| Field               | Value                                                     |
|---------------------|-----------------------------------------------------------|
| **ID**              | `project.membership.list`                                 |
| **Domain**          | `project.membership`                                      |
| **Entry points**    | `GET /api/v1/projects/{id}/members`                       |
| **Principals**      | `user`, `agent`                                           |
| **Credentials**     | `session_jwt`, `scoped_uat`, `agent_jwt`                  |
| **Resolver**        | `project-from-url`                                        |
| **Base permission** | `project.read`                                            |
| **Effects**         | `list-scoped`                                             |
| **Delegation**      | `none`                                                    |
| **Authority eval**  | `none`                                                    |
| **Governance**      | None                                                      |
| **Invariants**      | None                                                      |
| **Audit**           | None (read-only)                                          |
| **Denial codes**    | `forbidden`                                               |
| **Tests**           | `pkg/hub:TestProjectMembership_*`                         |

## 4. Approved Semantics

### 4.1 Mine and Shared Classification (D6)

- **Mine**: projects where the user has an active, direct `project-owner`
  RoleBinding. "Active" means `NotBefore <= now` and (`ExpiresAt` is nil or
  `ExpiresAt > now`). "Direct" means the binding's principal is the user
  themselves, not a group.

- **Shared**: projects where the user has effective project access (direct or
  group-derived) but lacks an active direct owner binding. Group-derived access
  is included.

**Target:** Derive Mine and Shared only from active RoleBindings. Shared =
effective accessible projects minus Mine. The legacy `Project.OwnerID` field
is retained as metadata only — it carries no authorization semantics.

A scheduled or expired owner binding does not count as "Mine" until/unless it
becomes active.

### 4.2 Self-Removal (D1)

An owner may remove their own binding, subject to the last-owner invariant.
The last-owner guard prevents removing the last active direct owner regardless
of whether it is self-removal or third-party removal. Ownership transfer is
accomplished via add-new-owner → self-remove sequence.

RS1 adds `POST /api/v1/projects/{id}/transfer-ownership` for atomic
single-step transfer.

### 4.3 Ownership Transfer (D1)

Current transfer is add-new-owner → self-remove (two-step). RS1 adds an
atomic transfer endpoint. Self-removal and atomic transfer are complementary:
self-removal is a building block; atomic transfer is the preferred ergonomic
path.

### 4.4 Scheduled and Future Owners (D2)

Active-at-decision-time only. A scheduled or future owner binding
(`NotBefore > now`) does not grant ownership authority at decision time.
An expired binding (`ExpiresAt <= now`) does not grant ownership authority.
Both `isProjectOwner` and `countDirectOwnerBindings` enforce activation
lifecycle consistently.

### 4.5 Multiple Simultaneous Bindings (D4)

One direct project-scoped role binding per principal per project. The current
store unique index includes `role_definition_id`, permitting different roles
per principal/project. RS1 adds a constraint for single direct binding per
principal/project and implements atomic replacement. Multi-role binding
migration is handled deterministically.

Group-derived authority remains additive (union model) — this constraint
applies only to direct bindings.

## 5. Invariants

### 5.1 Last-Owner Guard

At least one active direct user `project-owner` binding must remain after any
membership mutation. This invariant is evaluated against the proposed post-state
within the same transaction.

- The guard checks all active direct owner bindings, not just the target.
- Activation lifecycle (`NotBefore`/`ExpiresAt`) is evaluated at decision time.
- The guard fails closed on store errors.

### 5.2 Direct-User-Only Invariant

`project-owner` bindings must have a direct user principal (not a group or
agent). This prevents group restructuring from orphaning a project. This is a
permanent invariant.

`project-admin` is currently also direct-user-only (C0 containment). Group
principals for admin are approved (D3) but not yet implemented. Removal of
`project-admin` from `directUserOnlyProjectRoles` is a Phase 3 reference
implementation task.

## 6. C0 Containment Markers

The following C0-CONTAINMENT markers in the codebase reference project
membership governance. Each marker identifies the finding, the restriction,
and the contract decision required to relax it:

| Location                              | Finding    | Restriction                      |
|---------------------------------------|------------|----------------------------------|
| `authz_candelegate.go:374`            | F-QA-02    | Owner-only membership mutations  |
| `authz.go:1118`                       | F-QA-02    | `isProjectOwner` restricts to owner |
| `handlers_project_members.go:341,525,675` | F-QA-02 | POST/PATCH/DELETE owner gate     |
| `handlers_project_members.go:188`     | G3         | Advisory capabilities            |
| `handlers_projects_core.go:168-176`   | F-QA-01    | Mine/Shared classification       |
| `handlers_agents_core.go:268-273`     | G1/G2      | Agent list Mine/Shared           |
| `handlers_runtime_brokers.go:609-720` | F-PLAN-01  | Broker Mine/Shared resolution    |
| `errors.go:87`                        | —          | Stable denial codes              |

These markers must not be removed without the corresponding governance matrix
implementation (RS1) and regression tests.
