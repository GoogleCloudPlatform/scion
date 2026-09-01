# Authorization Operation Contract

**Status:** Phase 1 (CT1) — frozen contract vocabulary  
**Date:** 2026-09-01  
**Companion plan:** `authz-audit-implementation-plan.md` (scratchpad)  
**Predecessor:** C0 containment on `scion/authz-audit`  
**Implementation:** `pkg/hub/authzop`

## Purpose

This document defines the domain-neutral security operation contract vocabulary.
An authorization operation is the unit of audit: not a permission string, URL
pattern, handler file, or UI control. Each operation contract declares the
complete proof obligation for one security-meaningful action.

The vocabulary defined here is implemented as Go types in `pkg/hub/authzop`. The
canonical catalog (Phase 2, AF1) will populate these types from repository entry
points. This document governs the semantics; the Go types enforce them
deterministically.

## 1. Operation Identity

### 1.1 Operation ID

Every operation has a stable, unique ID in `domain.verb` format:

```
project.membership.add
project.membership.update
project.membership.remove
agent.create
secret.read
constraint.relax
```

Once assigned, an ID must not change or be reused. IDs are the join key between
catalog entries, test references, audit records, and coverage gates.

### 1.2 Domain

The domain identifies the product area that owns the operation
(`project.membership`, `agent`, `secret`, `constraint`, `credential`, etc.).
Domains organize operations for review, governance, and catalog generation.
A domain does not imply a Go package boundary.

## 2. Entry Points

Every externally reachable path that dispatches an operation must be enumerated:

| Kind               | Example                                        |
|--------------------|------------------------------------------------|
| `http_route`       | `POST /api/v1/projects/{id}/members`           |
| `broker_call`      | `agent.lifecycle.start`                        |
| `scheduler_job`    | `dispatch_agent`                               |
| `cli_command`      | `scion server recover-authz`                   |
| `background_job`   | `agent-heartbeat-timeout`                      |
| `internal_dispatch`| `authorizeScheduledAgentCreate`                |

HTTP routes require an explicit method. One entry point maps to exactly one
operation or a documented exemption. Duplicate entry points across operations are
a validation error.

## 3. Admitted Principals and Credentials

Each operation declares which principal and credential kinds may attempt it:

| Kind              | Description                                     |
|-------------------|------------------------------------------------|
| `user`            | Authenticated user (full session)               |
| `agent`           | Agent with JWT credentials                      |
| `scoped_uat`      | User Access Token (project-scoped)              |
| `broker`          | Runtime Broker identity                         |
| `service_account` | GCP service account                             |
| `system`          | System/internal caller (seeding, migration)     |

The admitted set is a closed vocabulary. An operation that does not list a
principal kind rejects that kind at the entry point. Exemptions
(`authentication_only`, `public_endpoint`, `internal_only`) may justify an
empty principal list for operations that do not perform authorization.

## 4. Resource and Scope Resolution

Each operation names its authoritative resource and scope resolver. The resolver
determines which project or system scope the base permission is evaluated
against. Examples:

- `project-from-url` — extracts the project ID from the URL path.
- `agent-owner-project` — resolves the agent's owning project.
- `system-scope` — the operation is system-scoped.

The resolver is a symbolic name, not a function pointer. AF1 will wire
resolvers to implementations; CT1 defines the naming convention.

## 5. Base Permission

The base permission is the canonical permission ID required for the operation
(e.g., `project.manage`, `agent.create`, `secret.read`). It must exist in the
permission registry (`pkg/hub/permissions`).

The base permission is necessary but not sufficient. Security effects, delegation,
governance, and invariants impose additional obligations that the base permission
alone does not satisfy.

## 6. Security Effects

Security effects classify the security-meaningful consequence of an operation.
Effects do not replace permissions; they select additional checks and invariants.

### 6.1 Effect Vocabulary

| Effect                    | Meaning                                               |
|---------------------------|-------------------------------------------------------|
| `read-one`                | Read a single resource                                |
| `list-scoped`             | List resources within an authorized scope             |
| `create-resource`         | Create a new resource                                 |
| `update-resource`         | Modify an existing resource                           |
| `delete-resource`         | Delete or soft-delete a resource                      |
| `grant-authority`         | Create a new authority grant (role binding, membership)|
| `change-authority`        | Modify an existing authority grant                    |
| `revoke-authority`        | Remove an existing authority grant                    |
| `tighten-boundary`        | Add or tighten an access constraint                   |
| `relax-boundary`          | Remove or relax an access constraint                  |
| `change-principal-status` | Suspend, reactivate, or delete a principal            |
| `issue-credential`        | Issue a credential (JWT, UAT)                         |
| `read-secret`             | Read secret material                                  |
| `mint-credential`         | Create a new credential                               |
| `assign-credential`       | Assign a credential to a principal or resource        |
| `emit-external-effect`    | Produce an effect beyond local database mutation       |
| `change-ownership`        | Mutate an explicitly protected ownership relationship |

The vocabulary is a closed set. Adding a new effect requires updating this
document and the `authzop` package.

### 6.2 Effect Obligations

| Effect class         | Required                                            |
|----------------------|-----------------------------------------------------|
| Authority effects    | Delegation policy (non-amplification check)         |
| Boundary effects     | Governance policy (before/after authority calc)      |
| Audit-requiring      | Audit obligation with event type and required fields |
| All mutations        | At least one post-state invariant is recommended     |

Read effects (`read-one`, `list-scoped`) do not require delegation, governance,
or audit obligations.

## 7. Delegation

Delegation policies specify how authority-increasing effects are checked.

```go
type DelegationPolicy struct {
    RequireNonAmplification bool
    Description             string
}
```

When `RequireNonAmplification` is true, the actor must hold every permission
being granted (the existing `CanDelegate` check). Delegation is a secondary
check: passing it never bypasses target-governance rules.

## 8. Target Governance

Governance policies specify target-governance rules for operations that change
peer/superior relationships, protected principals, constraint administration,
or ownership.

### 8.1 Governance Kinds

| Kind                    | Use case                                            |
|-------------------------|-----------------------------------------------------|
| `peer_superior`         | Actor/target role comparison (e.g., admin vs owner) |
| `ownership_ancestry`    | Ownership or ancestry relationship checks           |
| `protected_principal`   | Protected principal classes needing elevated access  |
| `constraint_admin`      | Constraint administration and issuer relationships   |
| `domain_specific`       | Domain callback for operations that don't fit above |

Governance does not assume a total role hierarchy. Different domains use
different governance models. A domain callback (`DomainCallback`) names the
domain-specific governance function that evaluates rules.

### 8.2 Governance vs Delegation

| Concern       | Delegation                         | Governance                            |
|---------------|-------------------------------------|---------------------------------------|
| Question      | Does the actor hold the permissions?| May the actor manage this target?     |
| Scope         | Permission-subset check             | Actor/target relationship check       |
| Bypass        | Never bypasses governance           | Never bypasses delegation             |
| Example       | Admin holds all admin permissions   | But admin may not manage another admin|

Both checks must pass for authority-affecting operations.

## 9. Post-State Invariants

Invariants are evaluated against the proposed post-state within the same
transaction. Each invariant specifies:

- A stable ID for tracing.
- A human-readable description.
- Whether it is fail-closed (must be `true` for security invariants).

Example invariants:

| ID                          | Description                                       |
|-----------------------------|---------------------------------------------------|
| `last-owner-guard`          | At least one active direct user project-owner must remain |
| `constraint-admin-lockout`  | At least one active direct user retains constraint admin  |
| `binding-scope-match`       | Role scope type matches binding scope type        |

Invariants that cannot be evaluated (e.g., store error) must fail closed when
`FailClosed` is true. This prevents a store failure from silently permitting a
security-violating mutation.

## 10. Audit Obligations

Operations with audit-requiring effects must declare:

- An event type (e.g., `membership.add`, `constraint.relax`).
- Required before/after fields that must be present in the audit record.

The audit record is emitted within the same transaction as the mutation.
Operations that produce external effects must document the failure/retry
contract when the external effect and the audit record cannot be atomic.

## 11. Stable Public Denial Codes

Operations declare the stable denial codes they may return. These are
product-level reasons, not internal evaluator details.

### 11.1 Well-Known Codes

| Code                          | Meaning                                            |
|-------------------------------|-----------------------------------------------------|
| `forbidden`                   | Generic insufficient permissions                    |
| `role_assignment_forbidden`   | Actor lacks authority to manage membership          |
| `target_role_protected`       | Target role requires elevated governance authority  |
| `LAST_OWNER`                  | Would remove the last direct-user project owner     |
| `insufficient_permissions`    | Actor lacks specific base permission                |
| `scope_violation`             | Operation crosses scope boundary                    |
| `principal_ineligible`        | Principal kind not admitted for this operation       |
| `credential_insufficient`     | Credential lacks required scope/caveat              |
| `user_suspended`              | Principal is suspended                              |
| `not_found`                   | Target resource not found                           |

### 11.2 Casing Convention

Existing wire codes use mixed casing (`LAST_OWNER` vs `role_assignment_forbidden`).
Phase 1 preserves existing wire codes for compatibility. The decision packet
(CT1 decisions) addresses casing normalization.

Internal evaluator details, missing-permission names, and explain provenance
must be retained in structured logs and audit data but never exposed in public
denial responses.

## 12. Test References

Every operation must reference at least one executable test that proves the
contract. References use the format `{package}:{function}`:

```go
TestRef{
    Package:  "pkg/hub",
    Function: "TestProjectMembership_OwnerCanAddMember",
}
```

AF1 will validate test references against the test binary. CT1 defines the
reference format and requires at least one reference per spec.

## 13. Exemptions

Exemptions document explicit departures from normal contract requirements:

| Kind                  | Use case                                           |
|-----------------------|-----------------------------------------------------|
| `offline_recovery`    | Offline operator recovery commands                  |
| `deterministic_seed`  | Deterministic seeding of built-in roles/bindings    |
| `migration`           | Schema or data migration                            |
| `test_fixture`        | Test fixture construction                           |
| `authentication_only` | Routes that require authentication but not authz    |
| `public_endpoint`     | Unauthenticated public endpoints (health, metrics)  |
| `internal_only`       | Internal dispatchers with no external entry point   |

Every exemption requires a non-empty reason and scope. Exemptions are
searchable and must not be removed without the corresponding contract
decision.

## 14. Fail-Closed Semantics

When any of the following cannot be resolved, the operation must fail closed
(deny):

- Role definition lookup fails.
- Group closure resolution fails.
- Resource parent or project resolution fails.
- Ownership relationship lookup fails.
- Access constraint lookup fails.
- Credential or status record lookup fails.
- Post-state invariant evaluation fails.

Fail-closed behavior is the default. An operation that intentionally fails open
(e.g., health checks) must have a documented exemption.

## 15. Catalog and Generation

The canonical catalog is Go-native (`pkg/hub/authzop.OperationSpec`). Identifiers
and functions are checked by the compiler. A generated Markdown view may serve
reviewers and product owners.

CT1 defines the schema. AF1 populates the catalog from repository entry points
and adds CI gates. The catalog is not a free-form YAML inventory; it is a
typed Go data structure with deterministic validation.

### 15.1 Renderer Contract

If CT1 can generate a reviewer-facing Markdown view from the Go schema without
inventing a second source of truth, it should. Otherwise, CT1 defines this
renderer contract and AF1 implements the generator:

```go
// RenderMarkdown produces a reviewer-facing Markdown view of the
// operation catalog. The output is deterministic for a given input.
func RenderMarkdown(specs []OperationSpec) string
```

The renderer reads only from `OperationSpec` values. It does not query the
database, file system, or any other source. The generated output is not a
source of truth; the Go specs are.

## 16. Phase 3 Reference Archetypes

Phase 1 selects at least one concrete reference operation for each of the six
Phase 3 archetypes. See the domain governance appendix and the archetype
selection document for the specific operations chosen.

| Archetype                       | Reference operation (selected)               |
|--------------------------------|-----------------------------------------------|
| Authority grant/change/revoke  | Project membership add/update/remove          |
| Cross-scope read/list          | Project list (handleProjects GET)             |
| Destructive/protected-target   | Project delete (deleteProject)                |
| Credential/secret              | UAT create (handleTokens POST)                |
| Boundary relaxation            | Access constraint update                      |
| External effect                | Agent dispatch (dispatchAgentEventHandler)    |

## Appendices

Domain governance appendices define approved governance rules for specific
product domains. Project membership is the first appendix. Each appendix
references the domain-neutral vocabulary defined here without extending it
with domain-specific types.

See: `.design/authorization-governance-project-membership.md`
