# Phase 3 Reference Archetype Selections

**Status:** Phase 1 (CT1) — approved and frozen
**Date:** 2026-09-01
**Parent contract:** `authorization-operation-contract.md`

## Purpose

Phase 3 requires at least one reference implementation for each of six
operation archetypes, proving that the domain-neutral contract handles
qualitatively different security effects. This document selects concrete
operations from real repository entry points and provides evidence that each
exists.

## Selected Archetypes

### 1. Authority Grant/Change/Revoke

**Selected operation:** Project membership add/update/remove

| Evidence              | Location                                          |
|-----------------------|---------------------------------------------------|
| Add handler           | `pkg/hub/handlers_project_members.go:216` — `addProjectMember()` |
| Update handler        | `pkg/hub/handlers_project_members.go:428` — `updateProjectMemberRole()` |
| Remove handler        | `pkg/hub/handlers_project_members.go:615` — `removeProjectMember()` |
| Entry points          | `POST /api/v1/projects/{id}/members`, `PATCH .../members/{bindingID}`, `DELETE .../members/{bindingID}` |
| C0 containment        | Owner-only gate via `canDelegateProjectMembership()` at `authz_candelegate.go:379` |
| Delegation check      | Double CanDelegate: `GrantTypeProjectMembership` + `GrantTypeRoleBinding` |
| Last-owner invariant  | `countDirectOwnerBindings()` at `handlers_projects_core.go:773` |
| Stable denial codes   | `role_assignment_forbidden`, `target_role_protected`, `last_owner` |
| Existing tests         | `pm1_membership_test.go`, `handlers_project_members_test.go` |

**Why this archetype:** Contains all three authority effects (grant, change,
revoke) in one API family, has existing containment and governance rules, and
exercises delegation (with distinct kinds per effect: `non_amplification` for
grant, `conditional_on_increase` for change, `none` for revoke), governance,
authority evaluation (`before_and_after` for change), and invariant checks
(security kind, fail-closed last-owner-guard) simultaneously. Demonstrates
separate principal/credential admission (`user` principal with `session_jwt`
and `scoped_uat` credentials) and structured audit with effect-specific
before/after field requirements.

### 2. Cross-Scope Read/List

**Selected operation:** Project list

| Evidence              | Location                                          |
|-----------------------|---------------------------------------------------|
| Handler               | `pkg/hub/handlers_projects_core.go:131` — `listProjects()` |
| Entry point           | `GET /api/v1/projects`                             |
| Scope resolution      | `ResolveAuthorizedScopes()` at `authz_scope.go`   |
| Mine/Shared filter    | `handlers_projects_core.go:168-176` (C0 containment markers) |
| Scope-pushed queries  | Project IDs pushed into store query               |

**Why this archetype:** Demonstrates authorized scope resolution, scope-pushed
pagination, Mine/Shared classification, and the requirement that totals and
rows agree with the authorized scope set.

### 3. Destructive/Protected-Target Action

**Selected operation:** Project delete

| Evidence              | Location                                          |
|-----------------------|---------------------------------------------------|
| Handler               | `pkg/hub/handlers_projects_core.go:2803` — `deleteProject()` |
| Entry point           | `DELETE /api/v1/projects/{id}`                     |
| Authorization         | Uses `authorize()` fail-closed guard               |
| Cascading effects     | Deletes agents, role bindings, group memberships   |
| Permission            | `project.delete`                                   |

**Why this archetype:** Exercises base permission, target ownership, cascading
deletion of security-relevant state (role bindings, agents), and audit
obligations for destructive operations.

### 4. Credential or Secret Operation

**Selected operation:** UAT (User Access Token) create

| Evidence              | Location                                          |
|-----------------------|---------------------------------------------------|
| Handler               | `pkg/hub/handlers_auth.go:676` — `handleTokens()` |
| Entry point           | `POST /api/v1/auth/tokens`                         |
| Credential minting    | Creates project-scoped bearer token                |
| Scope validation      | Token scopes validated against permission registry |
| Permission            | `user.manage` (self-service)                       |

**Why this archetype:** Exercises credential minting, scope validation, issuer
identity, and the requirement that minted credentials cannot exceed the
issuer's authority. Demonstrates `issuer_credential` governance kind, separate
credential kinds (`session_jwt` for authenticated user issuing a `scoped_uat`),
and after-state audit fields for credential issuance effects.

### 5. Boundary Relaxation

**Selected operation:** Access constraint update (relaxation)

| Evidence              | Location                                          |
|-----------------------|---------------------------------------------------|
| Handler               | `pkg/hub/handlers_access_constraints.go:273` — `updateAccessConstraint()` |
| Entry point           | `PATCH /api/v1/admin/access-constraints/{id}`      |
| Before/after calc     | Constraint evaluation at `access_constraint_eval.go` |
| Constraint model      | `access_constraint.go:189` — `AccessConstraint` type |
| Permission            | `access_constraint.update`                         |
| Protected admin       | Constraint modification requires elevated authority|

**Why this archetype:** Exercises boundary relaxation semantics: `before_and_after`
authority evaluation, `constraint_admin` governance kind, lockout prevention
invariant (security kind, fail-closed), and the requirement that relaxing a
boundary is an authority-increasing operation. Demonstrates both before and
after audit fields required by boundary effects.

### 6. External Effect

**Selected operation:** Agent dispatch (scheduled)

| Evidence              | Location                                          |
|-----------------------|---------------------------------------------------|
| Handler               | `pkg/hub/server.go:3056` — `dispatchAgentEventHandler()` |
| Entry point           | `scheduler_job:dispatch_agent`                     |
| External effect       | Creates and starts an agent container via runtime broker |
| Authorization         | `authorizeScheduledAgentCreate()` at `server.go`   |
| Delegation            | CanDelegate check for agent creation authority     |

**Why this archetype:** The meaningful effect (starting a container, executing
code, consuming resources) occurs beyond a simple local row mutation. Exercises
the `ExternalEffectPolicy` requirement: delivery mode, failure mode, idempotency
key, and retry/compensation semantics. Demonstrates that external effects have
a typed failure/retry contract, after-state audit fields for external emission,
and that authorization is checked even for scheduler-dispatched operations.

## Validation

All six archetype handlers were verified to exist at the listed file paths and
line numbers on branch `scion/authz-audit` at commit `885470b`. Each has at
least one externally reachable entry point and performs security-meaningful
authorization.
