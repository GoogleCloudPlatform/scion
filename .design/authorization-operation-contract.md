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

Every operation has a stable, unique ID in lowercase dot-separated format:

```
project.membership.add
project.membership.update
project.membership.remove
agent.create
secret.read
constraint.relax
```

Validation enforces:
- Dot-separated segments of lowercase letters and digits only.
- At least two dot-separated segments (domain + verb).
- No leading, trailing, or consecutive dots.
- No empty segments.
- The ID must start with the `Domain` prefix (domain-prefix consistency).

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
| `websocket`        | `GET /api/v1/ws/agent/{id}`                    |
| `sse`              | `GET /api/v1/events/{id}`                      |
| `broker_call`      | `agent.lifecycle.start`                        |
| `scheduler_job`    | `dispatch_agent`                               |
| `cli_command`      | `scion server recover-authz`                   |
| `background_job`   | `agent-heartbeat-timeout`                      |
| `internal_dispatch`| `authorizeScheduledAgentCreate`                |

HTTP-like entry points (`http_route`, `websocket`, `sse`) require an explicit
method. Non-HTTP entry points (`broker_call`, `scheduler_job`, `cli_command`,
`background_job`, `internal_dispatch`) must not declare a method. `websocket`
and `sse` are distinct from `http_route` because the audit plan identifies
upgrade/stream semantics as separate protocol concerns.

One entry point maps to exactly one operation or a documented exemption.
Duplicate entry points across operations are a validation error.

## 3. Admitted Principals and Credentials

Each operation declares both the admitted principal kinds and the admitted
credential kinds. These are separate closed vocabularies: a principal is the
authenticated identity class; a credential is the authentication mechanism
carrying that identity.

### 3.1 Principal Kinds

| Kind              | Description                                     |
|-------------------|------------------------------------------------|
| `user`            | Authenticated user                              |
| `agent`           | Agent identity                                  |
| `broker`          | Runtime Broker identity                         |
| `service_account` | GCP service account                             |
| `system`          | System/internal caller (seeding, migration)     |

### 3.2 Credential Kinds

| Kind                   | Description                                      |
|------------------------|--------------------------------------------------|
| `session_jwt`          | Full session JWT (user login)                    |
| `scoped_uat`           | User Access Token (project-scoped)               |
| `agent_jwt`            | Agent JWT credential                             |
| `broker_token`         | Runtime Broker authentication token              |
| `service_account_key`  | GCP service account key/identity                 |
| `system_internal`      | System/internal authentication                   |
| `identity_token`       | Identity token (e.g., OIDC, federated identity)  |

### 3.3 Validation Rules

Both vocabularies are closed sets. An operation that does not list a principal
kind rejects that kind at the entry point. An operation that does not list a
credential kind rejects that credential. Principal and credential admission are
validated independently: a user admitted via `session_jwt` but not `scoped_uat`
is expressible, as is an agent admitted only by `agent_jwt`.

At least one principal and one credential must be declared unless explicitly
waived (`principals`, `credentials` waived obligations). Exemptions
(`authentication_only`, `public_endpoint`, `internal_only`) may justify
empty lists for operations that do not perform authorization.

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

Each effect selects specific obligation requirements. The spec must declare
obligations that meet or exceed the minimum required by its strongest effect
(subsumption model). Validation enforces these rules deterministically.

#### 6.2.1 Delegation Requirements

| Effect              | Minimum delegation kind      | Rationale                          |
|---------------------|------------------------------|------------------------------------|
| `grant-authority`   | `non_amplification`          | Actor must hold all granted perms  |
| `change-authority`  | `conditional_on_increase`    | Delegate only if authority grows   |
| `change-ownership`  | `non_amplification`          | Ownership is full authority        |
| All others          | `none`                       | No delegation check required       |

Revocation (`revoke-authority`) does not require delegation: removing authority
does not grant the actor any new permissions.

#### 6.2.2 Governance Requirements

| Effect                | Governance required | Rationale                              |
|-----------------------|:-------------------:|----------------------------------------|
| `revoke-authority`    | Yes                 | Actor/target relationship check        |
| `change-ownership`    | Yes                 | Ownership governance                   |
| `relax-boundary`      | Yes                 | Boundary relaxation governance         |
| `tighten-boundary`    | Yes                 | Constraint administration governance   |
| `issue-credential`    | Yes                 | Issuer governance                      |
| `mint-credential`     | Yes                 | Credential authority governance        |
| `assign-credential`   | Yes                 | Assignment governance                  |

Authority grant/change effects do not intrinsically require governance at
the domain-neutral level. The effect validation tables above define the
minimum obligations that all operations must satisfy regardless of domain.
Domain governance appendices (e.g., project membership) impose additional
requirements: the project membership appendix declares `peer_superior`
governance for `grant-authority` operations, but this is a domain-specific
rule, not a generic effect obligation. AF1 domain-level validators enforce
domain-specific governance requirements beyond the base effect minimums.

#### 6.2.3 Authority Evaluation Requirements

| Effect              | Minimum evaluation kind    | Rationale                           |
|---------------------|----------------------------|-------------------------------------|
| `change-authority`  | `before_and_after`         | Detects authority increase/decrease |
| `change-ownership`  | `before_and_after`         | Ownership mutation evaluation       |
| `relax-boundary`    | `before_and_after`         | Boundary relaxation evaluation      |
| `tighten-boundary`  | `before_and_after`         | Boundary change evaluation          |
| All others          | `none`                     | No delta evaluation required        |

#### 6.2.4 Audit Requirements

Effects that require audit records, with before/after field requirements:

| Effect                    | Audit required | Before fields | After fields |
|---------------------------|:--------------:|:-------------:|:------------:|
| `grant-authority`         | Yes            | —             | Required     |
| `change-authority`        | Yes            | Required      | Required     |
| `revoke-authority`        | Yes            | Required      | —            |
| `change-ownership`        | Yes            | Required      | Required     |
| `delete-resource`         | Yes            | Required      | —            |
| `relax-boundary`          | Yes            | Required      | Required     |
| `tighten-boundary`        | Yes            | Required      | Required     |
| `change-principal-status` | Yes            | Required      | Required     |
| `issue-credential`        | Yes            | —             | Required     |
| `mint-credential`         | Yes            | —             | Required     |
| `assign-credential`       | Yes            | —             | Required     |
| `read-secret`             | Yes            | —             | —            |
| `emit-external-effect`    | Yes            | —             | Required     |

Read effects (`read-one`, `list-scoped`), create, and update effects do not
require delegation, governance, authority evaluation, or audit obligations.

#### 6.2.5 External Effect Policy

The `emit-external-effect` effect requires an `ExternalEffectPolicy` declaring
delivery mode, failure mode, idempotency, and retry/compensation semantics.
See section 10.2.

## 7. Delegation

Delegation is specified by a typed `DelegationKind` with an ordered strength
model. The spec declares its delegation kind; validation enforces that the
declared kind meets or exceeds the minimum required by the spec's strongest
effect (subsumption).

### 7.1 Delegation Kinds

| Kind                     | Strength | Semantics                                 |
|--------------------------|:--------:|-------------------------------------------|
| `none`                   | 0        | No delegation check required              |
| `non_amplification`      | 1        | Actor must hold all granted permissions   |
| `conditional_on_increase`| 2        | Delegate only when before/after shows increase |

A `DelegationDescription` is required when the kind is not `none`, explaining
the delegation semantics for reviewers.

### 7.2 Strength Subsumption

`conditional_on_increase` (strength 2) subsumes `non_amplification` (strength 1):
an operation with both `grant-authority` (requires `non_amplification`) and
`change-authority` (requires `conditional_on_increase`) may declare
`conditional_on_increase` and satisfy both requirements.

Delegation is a secondary check: passing it never bypasses target-governance
rules.

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
| `issuer_credential`     | Issuer/authority governance for credential effects   |
| `domain_specific`       | Domain callback for operations that don't fit above |

Governance does not assume a total role hierarchy. Different domains use
different governance models. A domain callback (`DomainCallback`) names the
domain-specific governance function that evaluates rules. The callback is
required when `Kind` is `domain_specific`.

### 8.2 Governance vs Delegation

| Concern       | Delegation                         | Governance                            |
|---------------|-------------------------------------|---------------------------------------|
| Question      | Does the actor hold the permissions?| May the actor manage this target?     |
| Scope         | Permission-subset check             | Actor/target relationship check       |
| Bypass        | Never bypasses governance           | Never bypasses delegation             |
| Example       | Admin holds all admin permissions   | But admin may not manage another admin|

Both checks must pass for authority-affecting operations.

### 8.3 Authority Evaluation

Authority evaluation specifies whether the operation requires before/after
effective-authority calculation. This is separate from delegation and governance:
delegation checks whether the actor holds the required permissions; governance
checks whether the actor may manage the target; authority evaluation detects
whether the proposed change increases, decreases, or shifts authority boundaries.

| Kind                | Strength | Semantics                                    |
|---------------------|:--------:|----------------------------------------------|
| `none`              | 0        | No authority-delta evaluation needed         |
| `proposed_post_state`| 1       | Evaluate proposed post-state invariants      |
| `before_and_after`  | 2        | Compare before/after effective authority     |

Strength is ordered for subsumption: `before_and_after` satisfies
`proposed_post_state`. Effects that change authority, ownership, or boundaries
require `before_and_after`; effects that only need post-state invariant
evaluation (e.g., last-owner guard) may use `proposed_post_state`.

## 9. Post-State Invariants

Invariants are evaluated against the proposed post-state within the same
transaction. Each invariant specifies:

- A stable ID for tracing.
- A human-readable description.
- A typed severity classification (`Kind`).
- Whether it is fail-closed (`FailClosed`).

### 9.1 Invariant Kinds

| Kind       | Fail-closed enforcement                                |
|------------|--------------------------------------------------------|
| `security` | Must be fail-closed. Validation rejects `FailClosed: false`. |
| `business` | May be fail-closed or fail-open depending on requirements. |

Security invariants protect authorization guarantees (e.g., last-owner guard).
Business invariants protect data integrity rules that do not directly affect
authorization (e.g., binding scope match).

### 9.2 Example Invariants

| ID                          | Kind     | Description                                  |
|-----------------------------|----------|----------------------------------------------|
| `last-owner-guard`          | security | At least one active direct user project-owner must remain |
| `constraint-admin-lockout`  | security | At least one active direct user retains constraint admin  |
| `binding-scope-match`       | business | Role scope type matches binding scope type   |

Invariants that cannot be evaluated (e.g., store error) must fail closed when
`FailClosed` is true. This prevents a store failure from silently permitting a
security-violating mutation. The `Kind` field is required; omitting it is a
validation error.

## 10. Audit Obligations

Operations with audit-requiring effects must declare structured audit
obligations with typed field categories and atomicity semantics.

### 10.1 Audit Record Structure

Each audit obligation specifies:

- **Event type:** stable audit event identifier (e.g., `membership.add`).
- **Context fields:** always-required fields (e.g., `actor_id`, `project_id`).
- **Before fields:** pre-mutation state required by destructive/change effects.
- **After fields:** post-mutation state required by create/change effects.
- **Atomic:** whether the audit record is written in the same transaction as
  the mutation.
- **Non-atomic justification:** required when `Atomic` is false, explaining
  why atomic audit is not feasible and what mitigations are in place.

Validation enforces:

- At least one context field when an audit obligation is present.
- Before fields required by effects that destroy or change state (revocations,
  deletions, authority/boundary changes, status transitions).
- After fields required by effects that create or change state (grants,
  credential operations, authority/boundary changes, status transitions,
  external effects).
- No empty or duplicate field values within any category.
- Non-atomic justification required when `Atomic` is false.

See section 6.2.4 for the per-effect before/after requirements.

### 10.2 External Effect Policy

Operations with `emit-external-effect` must declare an `ExternalEffectPolicy`
specifying the failure/retry contract for effects beyond local database
mutation.

| Field              | Description                                           |
|--------------------|-------------------------------------------------------|
| `DeliveryMode`     | `fire_and_forget`, `at_least_once`, or `exactly_once` |
| `FailureMode`      | `log_and_continue`, `fail_operation`, or `compensate` |
| `IdempotencyKey`   | How idempotency is ensured (e.g., "dispatch ID")      |
| `RetryPolicy`      | Retry semantics (e.g., "exponential backoff, 3 max")  |
| `Compensation`     | Rollback/compensation on failure (optional)           |
| `AuthBeforeEmit`   | Whether authorization is checked before emitting      |

All fields except `Compensation` and `AuthBeforeEmit` are required. Validation
rejects missing delivery mode, failure mode, idempotency key, or retry policy.
When `FailureMode` is `compensate`, a non-empty `Compensation` description is
required.

## 11. Stable Public Denial Codes

Operations declare the stable denial codes they may return. These are
product-level reasons, not internal evaluator details.

### 11.1 Well-Known Codes

| Code                          | Meaning                                            |
|-------------------------------|-----------------------------------------------------|
| `forbidden`                   | Generic insufficient permissions                    |
| `role_assignment_forbidden`   | Actor lacks authority to manage membership          |
| `target_role_protected`       | Target role requires elevated governance authority  |
| `last_owner`                  | Would remove the last direct-user project owner     |
| `insufficient_permissions`    | Actor lacks specific base permission                |
| `scope_violation`             | Operation crosses scope boundary                    |
| `principal_ineligible`        | Principal kind not admitted for this operation       |
| `credential_insufficient`     | Credential lacks required scope/caveat              |
| `user_suspended`              | Principal is suspended                              |
| `not_found`                   | Target resource not found                           |

At least one denial code is required per operation unless explicitly waived
(`denial_codes` waived obligation). This ensures every operation declares its
public failure contract.

### 11.2 Casing Convention

All denial codes use `lower_snake_case`. The `LAST_OWNER` wire code was
normalized to `last_owner` as an approved breaking change (D7, Option B).
New denial codes must follow the `lower_snake_case` convention.

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

Every exemption requires:

- A non-empty **reason** explaining the departure.
- A non-empty **scope** limiting the exemption's applicability.
- A non-empty **waives** list declaring the specific obligations bypassed.

### 13.1 Waivable Obligations

| Waived obligation    | What it bypasses                                    |
|----------------------|-----------------------------------------------------|
| `entry_points`       | Requirement for at least one entry point            |
| `principals`         | Requirement for at least one principal kind         |
| `credentials`        | Requirement for at least one credential kind        |
| `base_permission`    | Requirement for a base permission                   |
| `resource_resolver`  | Requirement for a resource resolver                 |
| `test_refs`          | Requirement for at least one test reference         |
| `denial_codes`       | Requirement for at least one denial code            |
| `audit_obligation`   | Requirement for audit record                        |

Validation enforces:
- Each waived obligation must be from the closed vocabulary above.
- No duplicate waives within a single exemption.
- At least one waived obligation per exemption.
- Only the named requirements are bypassed; un-waived requirements are still
  enforced.

Exemptions are searchable and must not be removed without the corresponding
contract decision.

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
