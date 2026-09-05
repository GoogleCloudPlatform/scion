---
title: Permissions & Access Constraints Reference
description: Technical reference for the Scion role-binding and access constraint authorization architecture.
---

## Overview

Scion employs a rigorous, positive-authority authorization architecture with monotonic restrictions to secure interactions between agents, users, and the Hub. 

The legacy YAML/JSON `Policy` and `PolicyBinding` resource paths have been fully removed. Instead, authorization is governed by two complementary constructs:
1. **Positive Authority (RoleBindings)**: Additive grants that map a principal (user, group, or agent) to a `RoleDefinition` within a specific scope.
2. **Monotonic Restrictions (AccessConstraints)**: Absolute maximum-permissions boundaries that can only reduce (never widen) the authority granted by positive RoleBindings.

This architecture ensures a deterministic, fail-closed evaluation model, and includes robust safety features such as delegation protection and an offline maintenance recovery path.

---

## Agent Identity

When an agent is provisioned by the Scion Hub, it is issued a cryptographically signed identity token (JWT). This token serves as the agent's passport for all interactions with the Hub API.

### Token Structure

The agent identity token contains standard JWT claims alongside Scion-specific metadata.

```json
{
  "iss": "https://hub.scion.dev",
  "sub": "agent:550e8400-e29b-41d4-a716-446655440000",
  "aud": "scion-hub",
  "iat": 1615985870,
  "exp": 1616072270,
  "scion_claims": {
    "project_id": "project:12345",
    "creator_user_id": "user:jane.doe@example.com",
    "template_id": "template:security-auditor:v2",
    "broker_id": "broker:aws-us-east-1",
    "mode": "hosted"
  }
}
```

### Provenance Claims

Crucially, the identity token includes **provenance claims** that attest to the agent's origin. These claims are signed by the Hub and cannot be forged by the agent or the user.

| Claim | Description | Usage in Evaluation |
| :--- | :--- | :--- |
| `creator_user_id` | The ID of the user who requested the agent's creation. | Restricts agent access to resources owned by the creator. |
| `template_id` | The ID and version of the template used. | Restricts capabilities (e.g. baseline vs. full) based on template roles. |
| `project_id` | The project workspace the agent belongs to. | Restricts agent operations to its specific project scope. |
| `broker_id` | The identity of the Runtime Broker executing the agent. | Validates that execution occurs in a trusted environment. |

---

## RoleBindings (Positive Authority)

Access to any resource or operation in Scion must be explicitly authorized by a **RoleBinding**. RoleBindings are strictly positive-only grants; there are no explicit "deny" bindings.

A `RoleBinding` contains the following fields:

* **Role Definition ID**: The UUID of the `RoleDefinition` being granted. Role definitions contain specific permission IDs (e.g., `agent.create`, `project.read`).
* **Principal Type**: The category of principal (`user`, `agent`, or `group`).
* **Principal ID**: The identifier of the specific principal.
* **Scope Type**: The scope bounds, either `system` (global permissions) or `project` (project-restricted permissions).
* **Scope ID**: Empty for `system` scope, or the specific project ID for `project` scope.
* **Activation Window (`not_before` / `expires_at`)**: Optional time bounds during which the binding is active.

### Agent Synthetic RoleBindings
At runtime, agents do not require static RoleBindings to be manually created. Instead, the authorization engine dynamically extracts the agent's JWT claims and scopes and constructs **synthetic project-scoped role bindings** so that agent actions can be evaluated through the unified authorization pipeline.

---

## AccessConstraints (Monotonic Restrictions)

An **AccessConstraint** is the underlying database and API representation of a user-facing **Access Boundary**. It acts as a monotonic restriction defining a **maximum-permissions boundary** (permission ceiling). It can only reduce (never widen) the authority granted by positive RoleBindings.

An `AccessConstraint` contains the following fields:

* **Name**: A unique name per scope.
* **Subject Kind**: Specifies who is constrained:
  - `principal`: A single user, agent, or group. Requires setting `subject_principal_type` and `subject_principal_id`.
  - `group_closure`: A group and all its nested subgroups. Requires setting `subject_group_id`.
  - `all_principals`: Constrains every principal across the targeted scope.
* **Scope Type / Scope ID**: Can be `system` (system-wide boundary) or `project` (restricting actions within a single project).
* **Maximum Permissions**: A JSON array of permission IDs that targeted principals are allowed to hold. If a permission is not listed in this array, targeted principals **cannot** exercise it, regardless of their positive RoleBindings.
* **Time Bounds (`not_before` / `expires_at`)**: Optional time window during which the constraint is active.
* **Disabled**: A boolean flag (`true`/`false`) used to deactivate the constraint. This is primarily used for **offline recovery** in lockout scenarios.

### Security Gates & Backend Subsystems

The AccessConstraint system relies on several backend subsystems to ensure secure, correct, and high-performance operations:

* **Canonical Evaluator**: Built into the core AK1 Kernel, the evaluator computes the intersection of active role permissions and monotonic ceilings at wire-speed on every request.
* **Typed Constraint Model**: A strictly typed and schema-validated structural model. It validates all inputs before persistence, ensuring permission lists only refer to valid, registered permission IDs.
* **Preview Engine & Provenance/Explain API**: An interactive dry-run service (`POST /api/v1/admin/access-constraint-previews`) that evaluates the effective permissions of a targeted principal. It provides high-fidelity **provenance/explain data** showcasing exactly which positive permissions are restricted by active boundaries, which rules caused the ceiling to apply, and what the final resolved permissions set is.
* **Transactional Governance & Atomic Audit**: To prevent partial policy writes or split-brain configurations, all updates are fully transaction-bound. The system records atomic, tamper-evident audit logs of all access boundary creations, updates, or deactivations in the global Hub secure audit ledger.
* **HTTP API and Error Contract**: Standardized REST endpoints govern AccessConstraints. The API features a strict error contract, validating payloads and returning detailed error diagnostics—such as `422 Unprocessable Entity` with a validation error matrix for invalid scopes or permissions, or `403 Forbidden` for delegation and access-denial scenarios.

---

## Authorization Evaluation Logic (AK1 Kernel)

Whenever an API request is made, the Hub's `Decide` endpoint processes the authorization request using the **AK1 Kernel** to resolve the effective permissions:

```
[Principal Identity] ─► [Active RoleBindings] ─► [Positive Permission Set]
                                                          │
                                                (Calculate Intersection) ◄─── [AccessConstraints (Ceilings)]
                                                          │
                                                          ▼
                                              [Effective Permissions]
```

1. **Authentication**: The caller is authenticated, establishing their principal type, principal ID, group memberships, and credentials (such as token type/scope).
2. **Retrieve Positive Grants**: The engine retrieves all active, time-valid RoleBindings that apply to the principal directly, or to any groups they belong to.
3. **Union Allowed Permissions**: The union of all permission IDs granted by these bindings forms the positive "allowed permissions" set.
4. **Load Applicable AccessConstraints**: The engine queries all active, non-disabled AccessConstraints that match the principal (by principal ID, group closure, or `all_principals`) within the targeted scope.
5. **Intersect Permissions**: The effective permission set is calculated as the intersection of the positive set and the maximum permitted ceilings.
6. **Verdict**: If the requested permission is present in the final intersection set, access is **Allowed**. If it is missing from the positive set, or excluded by an active AccessConstraint, access is **Denied** (fail-closed).

---

## Offline Authorization Recovery

If an administrator misconfigures an AccessConstraint (e.g., applying an overly restrictive `all_principals` constraint at `system` scope), all administrators may become locked out of the Hub API.

To resolve this without performing risky database edits, Scion provides an **Offline Authorization Recovery** command.

### The `recover-authz` Utility

The `scion server recover-authz` command allows a platform operator to bypass the active HTTP server and de-escalate access constraints directly via the storage adapter.

#### Security Safeguards
* **Direct Database Connection**: The command bypasses the active HTTP authorization checks by communicating directly with the database.
* **Exclusive Maintenance Lock**: It acquires an exclusive lock and will fail to execute if an active Scion Hub server or another recovery process is running.
* **Nuclear Warning**: Disabling all constraints requires passing a specific confirmation phrase.
* **Audit Trail**: Every recovery action records a permanent mutation audit log entry.
* **Grants Prevention**: The command **never** creates users, roles, or RoleBindings. It only removes restrictive boundaries so that pre-existing positive grants can function.

#### Usage Examples

**Deactivate a specific constraint by ID:**
```bash
scion server recover-authz --disable-constraint "36f9036a-2ea2-4a0b-936d-978118b518bc"
```

**Deactivate all access constraints (the nuclear recovery option):**
```bash
scion server recover-authz --disable-all-constraints \
  --confirm "I understand this disables all access constraints"
```

**Specify database URL and operator manually:**
```bash
scion server recover-authz --disable-constraint "36f9036a-2ea2-4a0b-936d-978118b518bc" \
  --db "postgres://localhost:5432/scion" \
  --operator "admin-recovery@example.com"
```
