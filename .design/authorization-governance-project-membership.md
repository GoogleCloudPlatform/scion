# Domain Governance Appendix: Project Membership

**Status:** Phase 1 (CT1) — pending user approval  
**Date:** 2026-09-01  
**Parent contract:** `authorization-operation-contract.md`  
**Implementation plan:** `authz-audit-implementation-plan.md` (scratchpad)  
**C0 containment:** `authz-audit-findings.md`

## Purpose

This appendix defines the governance rules for project membership operations.
It is the first domain governance appendix, not the canonical abstraction.
Other domains (constraints, credentials, group membership) will have their own
appendices using the same domain-neutral vocabulary from the parent contract.

All governance rules below are proposed defaults from the implementation plan's
recommended membership matrix. They are **pending user approval**. The contract
code supports either approved outcome without encoding an unapproved hierarchy
assumption.

## 1. Project Roles

| Role             | Scope     | Principal kinds        | Notes                          |
|------------------|-----------|------------------------|--------------------------------|
| `project-owner`  | project   | Direct user only       | At least one must always exist |
| `project-admin`  | project   | Direct user only (C0)  | Group eligibility pending      |
| `project-member` | project   | User, agent, or group  | Standard access                |

**C0 containment status:**
- Both `project-owner` and `project-admin` are currently direct-user-only.
- The frozen design permits groups to receive up to `project-admin`.
- Decision #3 in the CT1 decision packet addresses group eligibility.

## 2. Governance Matrix (Proposed)

This matrix specifies which actor roles may perform which membership operations
on which target roles. It is the recommended contract from the implementation
plan.

| Operation                            | project-member | project-admin | project-owner |
|--------------------------------------|:--------------:|:-------------:|:-------------:|
| View direct and effective members    | Optional†      | Yes           | Yes           |
| Add an ordinary member               | No             | Yes           | Yes           |
| Change or remove an ordinary member  | No             | Yes           | Yes           |
| Add, promote, demote, or remove admin| No             | No            | Yes           |
| Add or promote an owner              | No             | No            | Yes           |
| Demote or remove an owner            | No             | No            | Yes‡          |
| Change a binding outside this project| No             | No            | No            |

†Member view access is a product decision (CT1 decision packet item not
currently listed but may be added).  
‡Subject to last-owner invariant: at least one active direct owner must remain.

### Current C0 containment

C0 restricts **all** membership mutations (POST, PATCH, DELETE) to
`project-owner` only. This is more restrictive than the proposed matrix, which
would allow admins to manage ordinary members. The containment is temporary
and must not be relaxed without:

1. Approval of the governance matrix above.
2. Implementation of target-role governance checks.
3. Regression tests for each actor/target combination.

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
| **Denial codes**    | `role_assignment_forbidden`, `target_role_protected`, `LAST_OWNER` |
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
| **Denial codes**    | `role_assignment_forbidden`, `target_role_protected`, `LAST_OWNER` |
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

## 4. Proposed Semantics

### 4.1 Mine and Shared Classification

- **Mine**: projects where the user has an active, direct `project-owner`
  RoleBinding. "Active" means `NotBefore <= now` and (`ExpiresAt` is nil or
  `ExpiresAt > now`). "Direct" means the binding's principal is the user
  themselves, not a group.

- **Shared**: projects where the user has effective project access (direct or
  group-derived) but lacks an active direct owner binding. Group-derived access
  is included.

A scheduled or expired owner binding does not count as "Mine" until/unless it
becomes active.

### 4.2 Self-Removal

Whether a project owner may remove their own binding is a product decision
(CT1 decision packet item #1). The last-owner invariant prevents removing the
last owner regardless.

### 4.3 Ownership Transfer

Explicit ownership transfer semantics must be decided rather than inherited
from generic delete+add behavior (CT1 decision packet item #1).

### 4.4 Scheduled and Future Owners

A scheduled or future owner binding (`NotBefore > now`) does not grant
ownership authority at decision time. The implementation plan strongly
recommends active-at-decision-time semantics. Edge cases are documented in
CT1 decision packet item #2.

### 4.5 Multiple Simultaneous Bindings

Whether a principal may hold multiple project-scoped role bindings
simultaneously (e.g., both owner and admin) is addressed in CT1 decision
packet item #4. Under the current union model, duplicate bindings are additive
and harmless, but they complicate governance and audit.

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
agent). This prevents group restructuring from orphaning a project.

`project-admin` is currently also direct-user-only (C0 containment). The
frozen design permits group principals for admin; see CT1 decision packet
item #3.

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
approval and regression tests.
