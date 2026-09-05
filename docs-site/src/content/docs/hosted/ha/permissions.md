---
title: Permissions & Policy
description: Designing access control for Scion projects and agents.
---

Scion implements a robust, principal-based access control system to manage resources across distributed projects and teams. The system is built on the **Permissions Foundation** architecture, providing deterministic authorization evaluation, declarative route guards, and comprehensive auditing.

For a detailed technical specification of the permissions model, role definitions, access constraints, and agent identity claims, see the [Permissions & Access Constraints Reference](/scion/reference/permissions-policy/).

## Core Concepts

### Unified Authorization
Scion uses a `UnifiedAuthMiddleware` to enforce declarative route guards across the Hub. Every request undergoes deterministic authorization evaluation via a Decide path before reaching the handler, ensuring no resource can be accessed without explicit permission. Engine internals, settings handlers, User Access Token (UAT) endpoints, user management, integrations, and operations have all been converted to explicit permission-based checks, deprecating the legacy `requireAdmin` fallback.

### Roles and Bindings
Access is granted through explicit role assignments:
- **RoleDefinition**: A named collection of permissions (e.g., `developer`, `viewer`, `admin`).
- **RoleBinding**: A grant of a `RoleDefinition` to a principal (user, group, or agent) within a specific scope (Hub or Project).
- **Project Membership**: Users gain access to project resources by being bound to a role within that project.

### Delegation and Revocation
- **CanDelegate Admission Gate**: Prevents lateral privilege escalation by ensuring a principal can only grant roles or permissions they themselves possess.
- **Credential Revocation**: Agent credentials and User Access Tokens can be instantly revoked, terminating access system-wide.

### Observability
- **Decision & Mutation Audit**: All authorization decisions and role mutations are captured in a structured audit log.
- **Explain API**: Administrators can use the Explain API to query why a specific permission was granted or denied for a principal on a given resource.

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
- `scheduled_event`: A time-based recurring schedule or scheduled event.

### Actions
Scion uses a standardized set of actions:
- **CRUD**: `create`, `read`, `update`, `delete`, `list`.
- **Administrative**: `manage`.
- **Resource-Specific**: `start`, `stop`, `attach`, `message`.

## Access Control & Authorization

Scion enforces strict role-binding-based authorization for all agent operations:
- **Agent Creation**: Requires active membership in the target project.
- **Agent Interaction**: Interacting with an agent (e.g., via PTY/terminal or structured messaging) is restricted to the agent's owner (the creator) or system administrators.
- **Agent Deletion**: Only the agent's owner, a system administrator, or authorized agent callers can delete an agent. For an agent caller to perform a deletion, it must have `project:agent:lifecycle` (associated with the `full` role) and must target an agent within its own project (which closes a cross-project agent deletion vulnerability).

### Membership-Based Project Access (Visibility Eradication)

The legacy, non-functional project `Visibility` field (e.g., `private`, `team`, or `public`) has been completely eradicated. Instead, access control is governed entirely by membership-based policies.
- **Project Scope Governance**: Access to a project and its associated resources is restricted to principals belonging to the project's member group (i.e. `project:<slug>:members`). This group is bound to per-project read and access roles using Project-scoped RoleBindings (such as `project:<slug>:member-read-project` and `project:<slug>:member-read-agent` mappings).
- **Fail-Closed Retrieval (404 Gate)**: Project read access is verified via a `CheckAccess` gate on retrieval. If a caller is not authorized to read the project, the API responds with a standard `404 Not Found` (rather than a `403 Forbidden`) to prevent callers from probing the existence of private projects.

### Scheduler Authorization & Owner-Based Access Control

Scheduled events and recurring schedules are strictly protected using an **Owner-Based Access Control** model, combined with dedicated permissions and dynamic RoleBindings:
- **Owner-Based Protection**: Only the creator (the owner) of a schedule/event, or a system-wide administrator, has the authority to view, update, delete, or otherwise manage a scheduled event or recurring schedule. This is enforced via creator/owner ID validation at the API handlers layer.
- **Project Member Bindings**: During project creation or template synchronization, Scion backfills/seeds project-scoped scheduled event RoleBindings bound to the project's members group. This grants members the capability to schedule events within their project space.
- **Scheduler Permissions**: A set of 7 dedicated permissions are enforced across scheduler endpoints:
  - `scheduled_event.read`: Permission to read a scheduled event or recurring schedule.
  - `scheduled_event.list`: Permission to list scheduled events and recurring schedules.
  - `scheduled_event.create`: Permission to create a scheduled event or recurring schedule.
  - `scheduled_event.update`: Permission to update a recurring schedule (including pausing/resuming).
  - `scheduled_event.delete`: Permission to cancel a scheduled event or delete a schedule.
  - `hub.scheduler.read`: Permission to read hub-wide scheduler configurations.
  - `hub.scheduler.update`: Permission to update hub-wide scheduler configurations.

## Positive Authority & Monotonic Restrictions

Scion operates on a single positive-authority model using **RoleBindings** to grant permissions, supplemented by **AccessConstraints** to enforce maximum boundaries.

### Positive-Authority (RoleBindings)
All permissions in Scion are additive and must be explicitly granted via a RoleBinding.
- **RoleDefinition**: A named set of allowed permissions (e.g., `project:viewer`, `project:developer`, `hub-admin`).
- **RoleBinding**: Connects a principal (User, Agent, or Group) to a RoleDefinition.
- **Scope**: RoleBindings exist at either `system` scope (system-wide permissions across the entire Hub) or `project` scope (permissions restricted to a single project space).

### Monotonic Restrictions (AccessConstraints)
An **AccessConstraint** is a maximum-permissions boundary that can only *reduce* (never widen) a principal's granted authority. It acts as an absolute ceiling.

:::note[User-Facing Access Boundaries]
In the Scion Web Dashboard, monotonic restrictions (AccessConstraints) are exposed and managed end-to-end as **Access Boundaries** under the Admin Suite. They provide a guided authoring workflow and an interactive preview engine to visualize security policies before committing them.
:::

- **Ceiling Enforcement**: If a RoleBinding grants a principal 10 permissions, but an AccessConstraint limits that principal to a maximum of 3 specific permissions, the principal will only have those 3 permissions.
- **Targeting**: AccessConstraints can target specific principals, entire group closures (a group and all its subgroups), or all principals (`all_principals`).
- **Offline Recovery**: Under `disabled: true`, an AccessConstraint is deactivated. This is used in offline recovery to restore administrator access in the event of a lockout.

### Resolution & Evaluation Logic
On any authorization request (evaluated via the Hub's `Decide` endpoint):
1. **Load Bindings**: The engine loads all active RoleBindings for the principal (including group memberships and synthetic agent scopes).
2. **Resolve Allowed Set**: The union of all permissions from these RoleBindings is compiled into an "allowed permissions" set.
3. **Apply AccessConstraints**: The engine queries and loads all non-disabled AccessConstraints that apply to the principal (matching on direct principal ID, group memberships, or `all_principals`).
4. **Calculate Intersection**: The effective permission set is the intersection of the resolved allowed set and the AccessConstraints' `maximum_permissions` ceilings. If no positive RoleBinding grants the permission, or if an AccessConstraint excludes it, access is denied (**fail-closed**).

## Capability-Based Access Control

The Hub API and Web UI utilize a capability gating system. Resource responses from the API include `_capabilities` annotations. These annotations explicitly state the actions the authenticated user is permitted to perform on that specific resource. This ensures granular UI controls (e.g., disabling the "Delete" button if the user lacks permission) and provides a secondary layer of API-level enforcement.

## GCP Service Account Assignment Gates

To prevent lateral privilege escalation—where an agent with low privileges creates a child agent with high privileges, or a user assigns a highly privileged GCP service account they shouldn't have access to—Scion implements a secure, **two-layer gate** for binding a GCP service account to any agent:

1. **Layer 1: Scion Hub Authorization**: The Hub's built-in authorization engine verifies the caller has the `ActionAssign` permission on the GCP service account resource within Scion.
2. **Layer 2: GCP IAM Policy (`actAs`)**: If `gcp_iam_check_mode` is set to `enforce` (see [Server Configuration Reference](/scion/reference/server-config/)), the Hub evaluates Google Cloud's IAM delegation model via the **GCP Policy Troubleshooter v3 API**. It verifies that the caller's GCP principal possesses `iam.serviceAccounts.actAs` permission on the target service account.

### The `actAs` Validation Gate

The `actAs` (impersonation) check is critical because binding a service account to an agent grants that agent real cloud authority. 

| Layer | Checked Authority | Action | Checked Principal |
| :--- | :--- | :--- | :--- |
| **Hub Authorization** | Inside Scion | `ActionAssign` | Scion User/Agent |
| **GCP IAM** | Inside Google Cloud | `iam.serviceAccounts.actAs` | Caller's GCP Principal |

*Note: The Hub's own `roles/iam.serviceAccountTokenCreator` permission is used to perform impersonated credential probes. It is NOT the permission checked on the caller. The permission evaluated on the caller is `iam.serviceAccounts.actAs` (typically granted via `roles/iam.serviceAccountUser`).*

### Fail-Closed Resolution

If Policy Troubleshooter returns an indeterminate or unknown status (e.g., `ACCESS_STATE_UNKNOWN_CONDITIONAL` due to IAM conditions, or `ACCESS_STATE_UNKNOWN_INFO_DENIED` due to insufficient Hub reviewer permissions), Scion **fails closed** and denies the assignment immediately. There is no fallback to `getIamPolicy`, which can easily fail open or miss complex project-level, group-level, or org-level bindings.

### Asymmetric Cache TTLs

To maintain high API performance without violating security constraints, assignment decisions are cached using asymmetric TTLs:
- **Allow TTL**: **60 seconds**
- **Deny TTL**: **10 seconds**
- **Indeterminate / Error States**: **Never cached**. Transient failures are retried immediately on the next request to prevent short outages from becoming fixed-length service blocks.

The cache is automatically invalidated for a target service account when that service account is deleted, or when a Hub-initiated IAM mutation occurs.

### IAM Prerequisites for Enforcement

For Policy Troubleshooter to evaluate a caller's IAM permission across the organization, the Scion Hub's own GCP service account must be granted the **IAM Security Reviewer** role (`roles/iam.securityReviewer`) at either the Google Cloud project or organization level.

### Hub-Scoped Service Accounts

Hub-scoped service accounts are defined globally at the Hub level rather than being restricted to a single project. This allows Platform Ops to make shared service accounts available for selection across multiple project-level workspaces.

To prevent unauthorized assignment of global resources, Scion applies specialized security logic:
- **Enforcement Mode Dependency**: Assignment of a hub-scoped service account is **unconditionally denied** if `gcp_iam_check_mode` is set to `off`. Because "off" mode disables the GCP IAM validation layer, letting users assign global service accounts without an `actAs` check would create a massive security risk. Hub-scoped service accounts require `gcp_iam_check_mode: enforce` to be assigned.
- **Dynamic Membership Checks**: Only current, active members of the Hub or project who possess `ActionAssign` on the resource and pass the Policy Troubleshooter `actAs` check can bind the service account.
- **Former-Member Denial**: If a user is removed from a project or leaves the organization, they immediately lose the ability to assign those service accounts—even if they were the user who originally created or registered the service account record in Scion. Ownership-based bypasses do not apply to hub-scoped service accounts.

### Project-Default Service Accounts

To streamline the agent creation workflow, project administrators can configure a project-default GCP service account that is automatically applied to newly created agents. However, to prevent privilege-escalation bypasses, this assignment is strictly gated:
- **Enforced at Creation and Selection**: The Policy Troubleshooter `actAs` evaluation is automatically triggered whenever an agent is created using the project's default service account, or when a user selects the default service account option.
- **Unauthorized Bypass Prevention**: If a user does not possess `iam.serviceAccounts.actAs` permission on the project's default service account, they are barred from creating agents under that project with the default identity, even if they have full project access.

### Passthrough Mode Security & PATCH Parity

In **Passthrough Mode**, an agent bypasses explicit service account binding and directly assumes the GCP identity of its GKE/GCE broker host. To prevent unauthorized access to host-level authority:
1. **Broker-Owner Restriction**: The caller must have permission to use that specific broker in passthrough mode.
2. **Host SA check**: The caller's GCP principal is checked via Policy Troubleshooter to confirm they hold `iam.serviceAccounts.actAs` permission on the broker's underlying host service account.

To enforce this boundary reliably, Scion implements strict **PATCH Parity** across its API:
- Previously, the `actAs` check only ran on agent creation (`POST /api/v1/agents`).
- Now, the exact same validation function gates the update path (`PATCH /api/v1/agents/{id}`). This prevents users from sneaking past the delegation gates by creating a low-privilege or no-auth agent and then PATCHing it to use passthrough mode.

### Service Account Minting Permissions

**Minting** is the process where Scion Hub automatically provisions a brand-new GCP service account in the Hub's own project and registers it to the database on behalf of the user. Because minting creates new GCP authority and project IAM bindings, it operates under a highly secure flow:

- **Enforced Regardless of Mode**: Unlike assignment checks (which can be toggled via `gcp_iam_check_mode`), **minting checks are always active**. SAs cannot be minted unless the requester passes GCP IAM checks, even if `gcp_iam_check_mode` is set to `off`. Skipping mint checks would create an instant privilege-creation bypass.
- **Required GCP Permissions**: To mint a service account, the requester's GCP principal must have:
  - `iam.serviceAccounts.create` on the Hub's GCP project (to create the service account).
  - `aiplatform.endpoints.predict` on the target project (to authorize the minted SA to access the GCP Vertex AI Platform).
- **Fail-Closed Minting Flow**: A minted service account is stored as `Verified` in Scion only if all required downstream GCP IAM mutations succeed—specifically, granting the Hub SA `roles/iam.serviceAccountTokenCreator` on the minted SA, and granting the requester `roles/iam.serviceAccountUser` on the minted SA. If any mutation fails, the status is recorded as failed and the service account remains unverified.

### Web UI Integration & Identity Cards

To make the service account lifecycle transparent and auditable for users and administrators, the Web Dashboard includes the following enhancements:
- **Tiered Role Badges**: The agents list and agent detail pages display visible role badges (`none`, `readonly`, `baseline`, or `full`) highlighting the active execution role of each running container.
- **GCP Identity Card**: The agent detail view features an interactive **GCP Identity Card**. In all authentication modes, it displays the bound service account email, verification status (e.g. `verified` or `failed`), and the corresponding GCP project ID.
- **Service Account Status Manager**: Within project settings, owners can view registered service accounts, check their live Policy Troubleshooter verification status, and manually trigger verification probes.
- **Zero-Reload Service Account Dropdown Sync**: The UI dispatches custom events (`sa-list-changed`) across components upon SA registration, verification, minting, or deletion, instantly updating default service account selection dropdowns without a full-page reload, and automatically clears the default SA selection if the selected SA is deleted.

---

## Quotas and Limits

The Scion Hub enforces resource consumption through a strict **Quota System** (Permissions Phase 2). This system operates at both the project and agent creation layers:
- **Enforcement Mechanics**: Quotas are evaluated via advisory-lock-based enforcement with fail-closed semantics to ensure hard limits are respected and reservation leaks are prevented.
- **Data Model**: The quota system uses `LimitDefinition`, `EntitlementBinding`, and `UsageReservation` schemas backed by a dedicated `QuotaStore`.
- **System Limits**: Several seeded system limit definitions provide out-of-the-box safe bounds on resource usage.
- **Quota API**: A suite of 13 quota API endpoints is available for inspecting and managing quotas. These endpoints feature strict route guard read/write permission splits and built-in protection against arbitrary system limit modification.

## Roles

To simplify management, Scion separates roles into **User Roles** (for human operators) and **Agent Roles** (for running agents).

### User Roles

These built-in roles bundle common permissions for human users:

| Role | Description |
|------|-------------|
| `hub-admin` | Full control over the entire Hub (System Role). |
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
| `baseline` | `project:read`<br>`agent:status:update`<br>`agent:token:refresh`<br>`project:agent:notify`<br>`agent:port:forward` | Standard execution permissions. Allows the agent to report progress, refresh its token, register reverse-proxied port forwards, send notifications, and manage its own notification subscriptions. |
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
- **Legacy Agent Compatibility**: Legacy agents that do not have a stored role default to the `full` role. This prevents production regressions where standing agents lose modern required scopes (such as `project:read`, `project:agent:lifecycle`, or secret access) after a token refresh.

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

## Managing Access and Infrastructure

The Scion Web Dashboard includes a centralized **Admin Management Suite** (accessible to users with appropriate administrative capabilities) that provides dedicated views for access control and infrastructure management:

- **Server Configuration Editor**: A full-featured settings editor at `/admin/server-config`. This allows administrators to view and modify the global `settings.yaml` through the Web UI with support for tabbed navigation, sensitive field masking, and hot-reloading of key settings like log levels, telemetry defaults, and admin emails.
- **Users List**: View all authenticated users, search for specific accounts, track "Last Seen" timestamps, and manage their system-wide roles (e.g., granting `hub-admin` access).
- **Groups Management**: Full-featured admin UI/UX for creating and managing custom membership groups. Administrators can easily define hierarchical collections of users and manage their membership using a human-friendly editor with user search autocomplete. Group creation is strictly authorized, and the `project:` prefix is a reserved slug. To prevent slug collisions, colliding group identifiers require a system marker combined with the `ProjectID`. Membership lookups rely on canonical identity resolution. This enables policy-based authorization where permissions can be granted to an entire team at once, while strictly enforcing group ownership and authorization rules.
- **Access Boundaries**: Full-featured administrative suite for defining and managing monotonic permission ceilings (AccessConstraints) via the Hub Admin UI.
  - **Inventory Page**: Provides a centralized view of all active and disabled access boundaries configured on the Hub.
  - **Guided Authoring Workflow**: A step-by-step UI workflow for creating and editing boundaries, including targeting individual principals, group closures, or all principals, and specifying allowed maximum permissions.
  - **Preview Engine**: An interactive evaluation sandbox enabling administrators to simulate, dry-run, and verify the impact of an access boundary on a principal's effective permissions prior to committing. Backed by the **Provenance/Explain API**, it details exactly which positive permissions are restricted and why.
  - **Transactional Governance & Atomic Audit**: Built-in backend security guarantees that all access boundary operations are transactionally secure and recorded in the atomic mutation audit log.
- **Role & Binding Management**: Full CRUD interfaces for Role Definitions and Role Bindings. Administrators can define custom roles, map permissions, and bind them to users, groups, or agents at the Hub or Project scope, while the system enforces `CanDelegate` checks to prevent privilege escalation.
- **Quota Management**: Dedicated admin view to manage the Quota System. Administrators can view, create, and update `LimitDefinition` thresholds and monitor `EntitlementBinding` status across projects.
- **Admin Security & Navigation**: The dashboard uses a **Per-Resource Permission-Gated Admin UI** to render and restrict access to the Admin Suite. Instead of a binary `role===admin` check:
  - Nav and route guards use granular, per-item permission checks.
  - The admin status API endpoint returns a per-resource permissions array that determines what elements are active and visible in the Admin UI.
  - Settings page tabs are gated by the caller's actual resource-level permissions. For example, a role with template-only permissions (like `template.*`) sees only the Templates tab, while other administrative tabs are hidden.
- **Broker Visibility**: Comprehensive broker detail pages provide a grouped view of all active agents by their respective projects, helping administrators understand resource distribution.
- **Maintenance Mode**: Administrators can toggle maintenance mode for the Hub and Web servers directly from the UI to facilitate safe infrastructure updates.

By leveraging these administrative views, Platform Ops can efficiently map their organization's structure directly into Scion's Principal and Policy hierarchy.