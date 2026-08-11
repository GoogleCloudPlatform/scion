---
title: Permissions & Policy
description: Designing access control for Scion projects and agents.
---

Scion is implementing a robust, principal-based access control system to manage resources across distributed projects and teams. While currently in the design and early implementation phase, this document outlines the core concepts and policy model.

For a detailed technical specification of the policy language and agent identity claims, see the [Policy & Permissions Reference](/scion/reference/permissions-policy/).

## Core Concepts

### Principals
A **Principal** is an identity that can be granted permissions.
- **Users**: Identified by their email address.
- **Groups**: Collections of users or other groups, allowing for hierarchical team structures.

### Resources
Permissions are granted on specific resource types:
- `hub`: The global Scion Hub instance.
- `project`: A project-level workspace.
- `agent`: An individual agent instance.
- `template`: An agent configuration blueprint.

### Actions
Scion uses a standardized set of actions:
- **CRUD**: `create`, `read`, `update`, `delete`, `list`.
- **Administrative**: `manage`.
- **Resource-Specific**: `start`, `stop`, `attach`, `message`.

## Policy-Based Authorization

Scion enforces strict policy-based authorization for all agent operations:
- **Agent Creation**: Requires active membership in the target project.
- **Agent Interaction**: Interacting with an agent (e.g., via PTY/terminal or structured messaging) is restricted to the agent's owner (the creator) or system administrators.
- **Agent Deletion**: Only the agent's owner, a system administrator, or authorized agent callers can delete an agent. For an agent caller to perform a deletion, it must have `ScopeAgentLifecycle` (associated with the `full` role) and must target an agent within its own project (which closes a cross-project agent deletion vulnerability).

Scion uses a **Hierarchical Override Model** for policies. Policies can be attached at three levels:

1.  **Hub Level**: Global policies applying to all resources.
2.  **Project Level**: Policies applying to all resources within a specific project.
3.  **Resource Level**: Policies applying to a single specific agent or template.

### Resolution Logic
When an action is attempted, Scion resolves effective permissions by traversing the hierarchy from the most specific to the most general:
- A policy at the **Resource level** overrides a policy at the **Project level**.
- A policy at the **Project level** overrides a global **Hub level** policy.

This model allows for granular delegation, where project owners can manage their own team's access without global administrator intervention.

## Capability-Based Access Control

The Hub API and Web UI utilize a capability gating system. Resource responses from the API include `_capabilities` annotations. These annotations explicitly state the actions the authenticated user is permitted to perform on that specific resource. This ensures granular UI controls (e.g., disabling the "Delete" button if the user lacks permission) and provides a secondary layer of API-level enforcement.

## Policy Structure

A policy defines the rules for access:

```json
{
  "name": "Project Developer Policy",
  "scopeType": "project",
  "scopeId": "project-uuid",
  "resourceType": "agent",
  "actions": ["create", "read", "start", "stop"],
  "effect": "allow"
}
```

- **Effect**: Can be `allow` or `deny`.
- **Conditions**: (Future) Optional rules based on resource labels or time-of-day.

## Roles

To simplify management, Scion separates roles into **User Roles** (for human operators) and **Agent Roles** (for running agents).

### User Roles

These built-in roles bundle common permissions for human users:

| Role | Description |
|------|-------------|
| `hub:admin` | Full control over the entire Hub. |
| `hub:member` | Standard user; can create their own projects. |
| `project:admin` | Full control over a specific project and its agents. |
| `project:developer` | Can create and manage agents within a project. |
| `project:viewer` | Read-only access to project status and logs. |

### Tiered Agent Authorization Roles

Scion implements a dedicated, tiered authorization model for **agents**. This ensures that running agents only possess the specific permissions they need to interact with the Hub API.

Agents are assigned one of four named roles, each mapping to a fixed set of JWT scopes:

| Agent Role | Granted Scopes | Description |
| :--- | :--- | :--- |
| `none` | *None* | No access to the Hub API (runs with no authorization claims). |
| `readonly` | `project:read` | Can view and query project state, but cannot report status, register port forwards, or manage other agents. |
| `baseline` | `project:read`<br>`agent:status:update`<br>`agent:token:refresh`<br>`project:agent:notify`<br>`agent:port:forward` | Standard execution permissions. Allows the agent to report progress, refresh its token, register reverse-proxied port forwards, and send notifications. |
| `full` | *All baseline scopes* +<br>`project:agent:create`<br>`project:agent:lifecycle`<br>`project:secret:read` | Complete agent control. Allows spawning child (sub) agents, managing their lifecycles, and reading project-scoped secrets from the secret backend. |

#### Two-Gate Authority Lattice
The effective role granted to an agent at creation is resolved by a **two-gate authority lattice**:

$$\text{effectiveRole} = \min(\text{requestedRole}, \text{userCeiling}, \text{projectMax})$$

1. **Requested Role**: The role requested during agent dispatch (e.g., using the `--role` flag in the CLI). If not specified, the role defaults to the project-level or Hub-level `default_agent_role`.
   - **Default Role Update**: For better usability, the default fallback role has been changed from `baseline` to `full`.
   - **Configuration Options**: You can specify `default_agent_role` globally under `agent_defaults` in the Hub settings (via settings/admin UI) or customize it per-project using the admin UI dropdown or the project setting `scion.io/default-agent-role`.
2. **User Ceiling**: Capped by the user's own system permissions. (Note: The user-ceiling gate is currently configured as a pass-through where all Hub users receive a ceiling of `full`, making the project's maximum role the primary operational limiter).
3. **Project Max**: Set by the project's `max_agent_role` setting, which defaults to the global Hub configuration (`default_max_agent_role` under `agent_defaults`).

#### Fallback and Fail-Closed Security
To guard against unauthorized escalations, the role fallback chain and parent lookup enforce fail-closed behavior:
- **Parent Agent Lookup Failure**: If parent agent lookup fails (e.g., due to transient database issues or invalid parent ID) when spawning a sub-agent, the sub-agent role ceiling defaults to `baseline` instead of failing open.
- **Corrupted Stored Roles**: If a parent agent's stored role is corrupted or invalid, it is treated as `baseline` for sub-agent creations to ensure robust security.

#### Sub-Agent No-Escalation Enforcement
To prevent security bypasses via sub-agent creation, Scion enforces strict no-escalation rules:
- When a parent agent spawns a child (sub-agent), the parent agent acts as the requester.
- A parent agent **cannot** grant a child agent a higher role than its own.
- Any attempt by an agent to spawn a child with elevated permissions will result in a loud, immediate `403 Forbidden` API rejection.

#### Token Refresh & Scope Re-derivation
To ensure security policies stay up-to-date and to support legacy agents created prior to the tiered role rollout, the Hub re-derives permissions from the agent's stored role during token refresh (`RefreshAgentToken`), rather than copying old JWT scopes verbatim.
- **Legacy Agent Compatibility**: Legacy agents that do not have a stored role default to the `full` role. This prevents production regressions where standing agents lose modern required scopes (such as `ScopeProjectRead`, `ScopeAgentLifecycle`, or secret access) after a token refresh.

#### Deprecation of Raw Template Scopes
With the introduction of tiered agent roles, the raw template field `hubAccess.scopes` has been **deprecated**. Agent permissions must be configured via the named roles.

## Implementation Status

The permissions system features:
- **Identity Resolution**: Core identity and domain-based authorization.
- **Capability Gating**: UI and API enforcement via `_capabilities`.
- **Policy Enforcement**: Strict authorization for agent creation, interaction, and deletion based on project membership and ownership.
- **Agent Identity & Ancestry**: Strict scoping of agent names, ancestry chains, and transitive access control.
- **Group & Policy Management**: Full support for group and policy schemas in the database, manageable via the Web Dashboard.

## Agent Ancestry & Transitive Access

Scion enforces a robust security model for agent-to-agent interactions (progeny) through **Ancestry Chains** and **Transitive Access Control**.

When an agent creates a child agent (for example, to delegate a sub-task), the system records an ancestry chain (`root` → `parent` → `child`). This chain is used to enforce strict identity scoping and transitive access permissions.

- **Transitive Access**: Any principal (human user or agent) that exists in an agent's creation chain automatically gains access to manage that agent. If a user owns the root agent, they inherently have access to all of its descendants.
- **Strict Scoping**: Agent identities are strictly scoped by their project using a specific naming convention (e.g., `project--agent`). This prevents name collisions across different workspaces and ensures that progeny agents cannot impersonate or interfere with agents in other projects.
- **Granular Secret Access**: Progeny agents inherit granular secret access controls from their parents, ensuring they only have the credentials necessary to perform their specific tasks.

## Managing Users and Groups

The Scion Web Dashboard includes a centralized **Admin Management Suite** (accessible to users with administrative privileges) that provides dedicated views for access control management:

- **Server Configuration Editor**: A full-featured settings editor at `/admin/server-config`. This allows administrators to view and modify the global `settings.yaml` through the Web UI with support for tabbed navigation, sensitive field masking, and hot-reloading of key settings like log levels, telemetry defaults, and admin emails.
- **Users List**: View all authenticated users, search for specific accounts, track "Last Seen" timestamps, and manage their system-wide roles (e.g., granting `hub:admin` access).
- **Groups Management**: Create organizational groups and manage their membership with a human-friendly member editor and user search autocomplete. This enables policy-based authorization where permissions can be granted to an entire team at once, while strictly enforcing group ownership and authorization rules.
- **Broker Visibility**: Comprehensive broker detail pages provide a grouped view of all active agents by their respective projects, helping administrators understand resource distribution.
- **Maintenance Mode**: Administrators can toggle maintenance mode for the Hub and Web servers directly from the UI to facilitate safe infrastructure updates.

By leveraging these administrative views, Platform Ops can efficiently map their organization's structure directly into Scion's Principal and Policy hierarchy.