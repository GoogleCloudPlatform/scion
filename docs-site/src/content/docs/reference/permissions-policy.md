---
title: Policy & Permissions Reference
description: Technical reference for the Scion policy language and permission system.
---

:::caution[Draft Specification]
The policy and permissions system described here is currently in **active design and development**. Interfaces and data structures are subject to change.
:::

## Overview

Scion employs a rigorous, claim-based access control system to secure the interactions between Agents, Users, and the Hub. This system goes beyond simple role-based access control (RBAC) by allowing policies to evaluate cryptographically signed claims embedded in an agent's identity token.

This enables sophisticated security postures, such as:
*   "An agent can only read secrets belonging to the user who created it."
*   "Agents created from the `security-auditor` template have read-only access to the codebase."
*   "Only agents running on trusted brokers can access production database credentials."

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

| Claim | Description | Usage in Policy |
| :--- | :--- | :--- |
| `creator_user_id` | The ID of the user who requested the agent's creation. | Restrict agent access to resources owned by the creator. |
| `template_id` | The ID and version of the template used. | Enforce least-privilege based on the agent's role (e.g., QA vs. Dev). |
| `project_id` | The project workspace the agent belongs to. | Isolate agents to their specific project scope. |
| `broker_id` | The identity of the Runtime Broker executing the agent. | Restrict sensitive tasks to trusted hardware/locations. |

## Policy Language

Scion policies are defined in JSON or YAML and are evaluated at the Hub whenever an API request is made. A policy binds a **Principal** (User or Agent) to a set of allowed **Actions** on a **Resource**, subject to **Conditions**.

### Policy Structure

```yaml
apiVersion: scion.dev/v1alpha1
kind: Policy
metadata:
  name: "limit-auditor-agents"
spec:
  # The scope of the policy (e.g., global, project-specific)
  scope: "project:12345"
  
  # Who is being regulated?
  principal:
    type: "agent"
    match:
      # Match agents created from the 'security-auditor' template
      claims.template_id: "template:security-auditor:*"

  # What are they allowed to do?
  allow:
    - action: "secret.read"
      resource: "secret:prod-db-*"
      condition:
        # CEL (Common Expression Language) expression
        expression: "request.auth.claims.creator_user_id == resource.owner"
```

### Evaluation Logic

1.  **Authentication**: The request is validated. If the requester is an Agent, its JWT is verified and unpacked.
2.  **Policy Matching**: The Hub retrieves all active policies relevant to the request's Scope (Global + Project).
3.  **Condition Check**: For each matching policy, the `condition` expression is evaluated against the request context.
4.  **Decision**:
    *   **Deny Override**: If *any* matching policy explicitly denies the action, the request is rejected.
    *   **Allow**: If at least one policy allows the action (and no denies exist), the request proceeds.
    *   **Default Deny**: If no policies match, the request is rejected.

## Access Scenarios

### Scenario 1: User-Bound Secrets

**Goal:** Allow an agent to access a secret only if the agent was created by the secret's owner.

**Policy:**
```yaml
principal: { type: "agent" }
action: "secret.access"
condition: "request.auth.claims.creator_user_id == resource.labels.owner_id"
```
**Mechanism:** The Hub compares the `creator_user_id` claim from the agent's signed JWT against the `owner_id` label on the stored Secret resource.

### Scenario 2: Immutable Audit Logs

**Goal:** Ensure `audit-logger` agents can write logs but never read or delete them.

**Policy:**
```yaml
principal:
  match: { claims.template_id: "template:audit-logger" }
allow:
  - action: "log.write"
deny:
  - action: "log.read"
  - action: "log.delete"
```

## Future Work

*   **Runtime Attestation**: Integrating lower-level hardware attestation (TPM/Enclave) into the `broker_id` claim.
*   **Just-in-Time Grants**: Generating short-lived policies for specific user sessions (e.g., "Allow this agent to deploy for the next 5 minutes").
*   **Policy Simulation**: Tooling to test policy changes against historical traffic before enforcement.
