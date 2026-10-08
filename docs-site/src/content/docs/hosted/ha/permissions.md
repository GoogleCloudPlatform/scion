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
- **Multi-Role Membership**: A principal (user, group, or agent) can hold one built-in project role (`project-owner`, `project-admin`, or `project-member`) plus any number of custom project-scoped roles. The project **Members** editor in the web UI shows one row per principal with its full role set, and saves the whole set atomically (`PUT /api/v1/projects/{id}/members/principals/{type}/{id}`, see the [API reference](/scion/reference/api/#projects-apiv1projects)). The last usable owner (an in-effect owner binding of an existing, active user) cannot be removed or demoted, nor can the last owner binding of any kind, and built-in role changes follow the owner/admin governance rules. Custom roles are governed as described under **Role & Binding Management** in [Managing Access and Infrastructure](#managing-access-and-infrastructure) below; the editor offers only the roles the Hub reports as grantable to you (`GET /api/v1/projects/{id}/members/assignable-roles`).
- **Project Ownership Comes Only From Role Bindings**: A project's `ownerId` field is informational and grants no access. Authority over a project comes only from project-scoped role bindings, so a user whose only claim was `ownerId` (for example a creator who was later removed from the project) is denied. Reading a project does not re-grant `project-owner` to its creator. Transferring ownership (`POST /api/v1/projects/{id}/transfer-ownership`) grants the new owner `project-owner`, downgrades the previous owner to member, and moves `ownerId`. At startup the Hub logs a warning, and grants nothing, for each legacy project whose `ownerId` user has no `project-owner` binding; an admin must add an owner binding for those projects.
- **Project Members Groups Are System-Managed**: Each project's members group is created by the Hub without an owner, and a startup backfill clears the owner on existing members groups, so a former project creator keeps no group-level authority over it. Updating, deleting, or changing the membership of a members group through the group API (`/api/v1/groups`) is restricted to hub admins, and no one — not even a hub admin — can set an owner on it. Manage project membership through the project members endpoints (`/api/v1/projects/{id}/members/...`) or the project **Members** editor instead.

### Delegation and Revocation
- **CanDelegate Admission Gate**: Prevents lateral privilege escalation by ensuring a principal can only grant roles or permissions they themselves possess.
- **Delegation Ceiling**: An agent acts with authority delegated by whoever created it, and that authority can never exceed its delegators'. For lifecycle actions and agent creation, every link in the agent's delegation chain must name a live delegator that holds the permission on the target resource: an active user, or an agent that exists and is not deleted (a stopped agent still counts). A deleted or deactivated delegator supplies no authority. The chain walk is limited to 10 links, and a cycle, a chain that is too deep, or a failed lookup denies the request. Delegation edges are project-scoped, so a resource in another project is denied.
- **Frozen Delegation Ceilings and Provenance**: When an agent is created, the Hub records where its authority came from (the creating credential) and freezes that credential's permission ceiling on the delegation edge. The child's role is capped to what the creating credential covers — for example, an agent created with a narrowly scoped [user access token](/scion/hosted/user/personal-access-tokens/) gets the highest role the token's scopes cover, and an explicit `--role` above that is rejected with `403`. The delegation walk then applies each hop's frozen ceiling, and agent token mint and refresh only issue scopes inside the chain's ceiling. Edges created before provenance was recorded still work for ordinary actions, but are denied for permissions that issue, use or deliver sensitive material (GCP service account use and assignment, secret read/use/delivery, env-var and skill delivery, and agent identity tokens); recreate such agents to restore those capabilities.
- **Delegation Edges Follow the Agent**: Creating an agent records the agent, its delegation edge, provenance and audit record together; if dispatch to the Runtime Broker fails, they are all rolled back. Deleting an agent deactivates its edges, and restoring it reactivates them. A restore fails with `409` if a delegator is no longer active or a reactivated edge conflicts with an active one (and `503` if the Hub cannot check the delegators), leaving the state unchanged. To reincarnate another principal's agent you must be able to delegate the agent's role in the target project. The agent keeps its existing delegator and frozen ceiling unless you change its role with `--role`; then you become its recorded delegator, and a requester that descends from the agent is refused with `403`, because the new edge would close a loop in the delegation chain.
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
- **Resource-Specific**: `lifecycle` (start, stop, suspend, restart, restore), `attach` (terminal, exec, env, reset-auth, the `POST /:id/keys` literal-terminal-input operation), `port_access`, `message`.

## Access Control & Authorization

Scion enforces strict role-binding-based authorization for all agent operations:
- **Agent Creation**: Requires active membership in the target project.
- **Agent Interaction**: Interacting with an agent (e.g., via PTY/terminal or structured messaging) is restricted to the agent's owner (the creator), users in the agent's ancestry chain, or system administrators. The default project-member role does not grant the `agent:message` permission — messaging authorization is aligned with the terminal attach permission gate.
- **Lifecycle vs. Attach**: Lifecycle operations (start, stop, suspend, restart, restore, reincarnate) are gated by `agent.lifecycle`, separately from `agent.attach` (terminal, exec, env, reset-auth) and `agent.port_access`. Because an agent runs with its creator's user-scoped secrets, the built-in `project-owner` and `project-admin` roles grant `agent.lifecycle`, messaging and `agent.port_access` but **not** `agent.attach`. Owners and admins can start, stop, and message other members' agents and open their forwarded ports, but cannot open a terminal on them. `project-member` does not carry `agent.port_access`; grant it to members through a custom project role. Owners and admins keep full access to their own agents and descendants through the resource-owner and ancestry grants.
- **Keys (`POST /:id/keys`)**: Sending literal terminal input through this dedicated Hub operation is authorized exactly like `agent.attach`, never through `agent.message` or a message-mode setting — a closed or `none` message mode does not block it, and message authority alone does not grant it. A human session or User Access Token needs to own the target agent or hold live `agent.attach` (`ActionAttach`) on it — no built-in project role grants `agent.attach`; an agent credential additionally needs `ScopeAgentLifecycle`, the same project as the target, **and** a live attach relationship on the target evaluated through the shared relationship pipeline — same-project membership alone is not sufficient for an agent caller. An authenticated agent crossing its own project boundary is refused (`422`) on the project-scoped route unconditionally — that comparison runs before any scope or target check — and on the top-level route provided it already holds the lifecycle scope above (an agent lacking that scope is refused with `403 keys_denied` first, regardless of project). The disclosure also differs by route: on the project-scoped route this is decided before any target lookup, so it discloses nothing about whether the target exists in the foreign project; on the top-level route the target is resolved first (as for every other action on that route), so a foreign *existing* agent and a *nonexistent* one get different outcomes — the same disclosure trade-off that route's other lifecycle actions already make today, not a new trade-off introduced by keys. A federated (cross-Hub) agent identity is never one of the two caller kinds above — it is always denied, the same as any other unrecognized principal kind. The `scion keys` CLI command calls this operation in Hub mode. Raw delivery through `POST /:id/message` has been removed: a request carrying `raw` is rejected with `422 raw_input_removed` — see [CLI Reference](/scion/reference/cli/#scion-keys) and [API Reference](/scion/reference/api/#agents-apiv1agents).
- **Agent Deletion**: Only the agent's owner, a system administrator, or authorized agent callers can delete an agent. For an agent caller to perform a deletion, it must have `project:agent:lifecycle` (associated with the `full` role) and must target an agent within its own project (which closes a cross-project agent deletion vulnerability).

### Membership-Based Project Access (Visibility Eradication)

The legacy, non-functional project `Visibility` field (e.g., `private`, `team`, or `public`) has been completely eradicated. Instead, access control is governed entirely by membership-based policies. The same applies to agents, templates, harness configs, and skills: their `visibility` field has been removed from the API (including the agent SSE payload), and access depends only on scope and grants. Skill create and update requests that still include `visibility` are rejected with `400 validation_error`. User- and project-scoped templates, harness configs, and skills are readable only by their owner, project members, and Hub admins; Hub-wide member and viewer grants cover only hub- and global-scoped records (see [Security](/scion/reference/security/#34-fail-closed-api-authorization-and-resource-isolation)).
- **Project Scope Governance**: Access to a project and its associated resources is restricted to its members. A principal is a member when it holds any active project-scoped role binding, built-in (`project-owner`, `project-admin`, `project-member`) or custom, directly or through a group. Expired, not-yet-active and revoked bindings do not count, and a group never confers `project-owner`.
- **System-Managed Members Group**: Each project has a members group (`project:<slug>:members`) that Scion manages. It cannot be the principal of a role binding on any scope, and it cannot be added as a child of another group: such requests fail with `400 principal_ineligible` (`details.reason: project_members_group`). Grant roles to the members individually or to a dedicated group instead. Deleting an existing binding or child-group edge is still allowed.
- **Fail-Closed Retrieval (404 Gate)**: Project read access is verified via a `CheckAccess` gate on retrieval. If a caller is not authorized to read the project, the API responds with a standard `404 Not Found` (rather than a `403 Forbidden`) to prevent callers from probing the existence of private projects.

### Scheduler Authorization & Owner-Based Access Control

Scheduled events and recurring schedules are strictly protected using an **Owner-Based Access Control** model, combined with dedicated permissions and dynamic RoleBindings:
- **Owner-Based Protection**: Only the creator (the owner) of a schedule/event, or a system-wide administrator, has the authority to view, update, delete, or otherwise manage a scheduled event or recurring schedule. This is enforced via creator/owner ID validation at the API handlers layer.
- **Project Member Permissions**: The built-in `project-member` role carries `scheduled_event.create`, `scheduled_event.list` and `scheduled_event.read`, so members can schedule events within their project; `project-admin` also carries `scheduled_event.update`.
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

The skills list (`GET /api/v1/skills`) also returns a list-level `_capabilities` object. It includes `create` when the caller can create a skill in at least one scope the request covers: any scope when no `scope` filter is given (every signed-in user can create in their own user scope), otherwise only the filtered scope and `scopeId`. Listing itself is not reported as a capability, because a successful response already means the caller may list.

## GCP Service Account Assignment Gates

To prevent lateral privilege escalation—where an agent with low privileges creates a child agent with high privileges, or a user assigns a highly privileged GCP service account they shouldn't have access to—Scion implements a secure, **two-layer gate** for binding a GCP service account to any agent:

1. **Layer 1: Scion Hub Authorization**: The Hub's built-in authorization engine verifies the caller has the `ActionAssign` permission on the GCP service account resource within Scion. For project-scoped service accounts, the `project-owner`, `project-admin` and `project-member` roles hold `gcp_service_account.assign`, so every owner, admin and member of a project passes this layer for any project-scoped service account in that project.
2. **Layer 2: GCP IAM Policy (`actAs`)**: This layer runs only when `gcp_iam_check_mode` is set to `enforce`; in the default `off` mode, Layer 1 alone decides project-scoped assignment. `enforce` is strongly recommended, see the caution under [GCP IAM Check Mode](/scion/reference/server-config/#gcp-iam-check-mode). Under `enforce`, the Hub evaluates Google Cloud's IAM delegation model via the **GCP Policy Troubleshooter v3 API**. It verifies that the caller's GCP principal possesses `iam.serviceAccounts.actAs` permission on the target service account.

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

### Start-Time Admissibility Check

An assignment that was valid when it was made can stop being usable later. Before the Hub starts, restarts or resumes an agent with an assigned service account (from the Web Dashboard, the API, or `scion start` / `scion resume`), it checks that the account:

- still exists and is still reachable from the agent's project;
- is still verified, under the same email the agent was assigned;
- for a hub-scoped account, is still allowed by `gcp_iam_check_mode: enforce`.

If any check fails, the start is refused with `400` and a message that says what to fix (for example, re-verify the service account or assign another one), instead of the agent starting and failing later when it asks for a token. If the Hub cannot complete the check, the start fails with `500`. Stop and suspend are not checked. Every token request repeats the same check, so passing it at start does not exempt an agent later.

If the Hub cannot save the result of a service account verification (on registration, minting or an explicit verify), the request fails with `500` instead of reporting success. On registration the account record already exists, so re-run verification on it (`scion project service-accounts verify <id>`, or `POST .../gcp-service-accounts/<id>/verify`) rather than registering it again.

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

#### Per-Profile Default Service Accounts

A project can also set a default service account per broker profile, in the project settings API field `defaultGCPIdentityServiceAccountIDByProfile` (a map of profile name to registered service account ID, stored as the project annotation `scion.io/default-gcp-identity-service-account-id-by-profile`). This lets agents on a Kubernetes profile default to a service account that has Workload Identity set up (a `kubernetes_service_account_mappings` entry on that profile), while agents on other profiles keep the project-wide default.

- **Which profile counts**: the profile the agent actually runs under. That is the profile in the create request, else the project's active profile, else the broker's own default profile (or its only profile). The Hub uses the same resolution as the hub-default passthrough check. When no profile can be resolved (for example, no request or active profile and a broker with several profiles and no default), this rung is skipped and the project default applies. The Hub pins the resolved profile on the agent, including the create inputs that reincarnate replays, so the broker dispatches under that profile. A reincarnated agent keeps the pinned profile, and so does a moved one (the move target must have that profile).
- **Profile names are not checked against brokers**: the map's keys are not compared with the profiles brokers report, since a broker may be offline or not yet registered when the setting is saved. An entry for a profile no agent runs under is never used. Profile names may contain letters, digits, `-` and `_`, must start with a letter or digit, and are at most 63 characters; the map holds at most 64 entries.
- **Precedence**: when the create request has no GCP identity, a matching entry assigns its service account (mode **Assign**). It wins over the project-wide default, including an explicit project **Block**, because it is the more specific setting. **Block** is not offered on Kubernetes anyway. With no entry for that profile, or no map at all, the ladder below is unchanged.
- **Same gates**: each entry must name a verified service account reachable from the project, and this is checked when the setting is saved. Each agent creation runs the same creator `actAs` authorization as the project default, recorded under the `project-default` audit surface. An entry that has gone stale (the account was deleted or un-verified) fails the create with an error that names the profile, for example `project default for profile "remote" GCP service account is not verified`.
- **Updating**: `PUT /api/v1/projects/{id}/settings` keeps the stored map when the field is absent, and clears it when the field is an empty object (`{}`). The web UI does not edit this field yet. Project clone remaps entries that name project-scoped accounts to the cloned accounts.
- **Debugging**: the Hub logs at debug level which source chose the service account (`project-profile-default`, `project-default`, or `hub-default`).

### Hub-Default GCP Identity

Hub administrators can set a hub-wide default GCP identity in **Admin > Server Config > Agent Defaults > General** (`agent_defaults.default_gcp_identity_mode` and `default_gcp_identity_service_account_id`). The Hub picks the identity for a new agent from the first of these that is set:

1. The GCP identity in the agent create request. Not applicable to an agent dispatched by a schedule, which carries no explicit identity.
2. The project's per-profile default service account for the profile the agent runs under (see [Per-Profile Default Service Accounts](#per-profile-default-service-accounts)).
3. The project's default GCP identity. An explicit project **Block** counts as set, so the hub default is not consulted.
4. The hub default. An explicit hub default of **Block** counts as set too.
5. Nothing configured at any rung: the Hub leaves the agent's GCP identity unset and sends no mode, so the Runtime Broker applies its runtime default: **Block** on every runtime except Kubernetes, **Passthrough** on Kubernetes.

This ladder applies the same way whether the agent is created interactively/via the API or dispatched by a schedule (ptone/scion#1927): a scheduled dispatch starts at rung 2, and a hub default with no project-level override reaches it exactly as it would an interactive create.

**Kubernetes runtime note**: **Block** is not offered on the Kubernetes runtime. If the agent's mode resolves to **Block** — the create request, an explicit project default, an explicit hub default, a project or hub default of **Assign** with no service account selected — dispatching that agent to the Kubernetes runtime fails with an actionable error naming **Assign** and **Passthrough** as the alternatives, rather than starting the agent. Rung 5 (nothing configured) and a denied hub-default **Passthrough** (below) are not **Block**: they get the Kubernetes runtime default, **Passthrough**. A Hub older than this behaviour still sends **Block** for them, so those agents fail on Kubernetes until the Hub is upgraded.

This also applies to agents created before this restriction existed. An earlier Hub or web UI version could write an explicit **Block** into an agent's own stored GCP identity even when the intent was "nothing configured" (the web UI, for example, sent an explicit **Block** on every create or save until it was updated to stop doing so). That stored value is **not migrated**: starting, restarting, or resuming such an agent dispatched to the Kubernetes runtime fails with the same actionable error as a newly-created agent would. The fix is the same in both cases — edit the affected agent's own GCP identity mode to **Assign** or **Passthrough** (a project or hub default only affects agents created after the change, not this agent's already-stored value).

**Passthrough** on Kubernetes reaches the pod's ambient identity (the node's or the pod's own Kubernetes ServiceAccount, whatever Workload Identity or the cluster otherwise provides). An explicit per-agent passthrough request goes through the broker-owner and host-SA checks. Operators who want the Hub to control which service account an agent on Kubernetes gets should configure **Assign** — at the project or hub default level, or per agent.

A hub-default **Passthrough** denied by either gate below — not the embedded Runtime Broker, or resolved to a non-local runtime profile — is treated the same as rung 5 on every runtime: the Hub leaves the agent's GCP identity unset rather than writing an explicit **Block**, and the Runtime Broker applies its runtime default: **Block** on most runtimes, **Passthrough** on Kubernetes. An *explicit* per-agent passthrough request denied by its own equivalent check still fails with its own clear error — this only changes what the hub *default* ladder does when it can't honor its own choice.

**Assign** on the Kubernetes runtime uses Workload Identity instead of the metadata
emulator every other runtime uses for **Assign**: the pod runs as a Kubernetes
ServiceAccount (KSA) already bound to the resolved GSA, and no
`SCION_METADATA_MODE=assign`/`GCE_METADATA_HOST` override or in-pod emulator is used.
The `actAs` and hub-scoped-service-account gates above are unchanged and evaluated
exactly the same way regardless of runtime — they authorize *who may be assigned* a
GSA, independent of how the runtime later establishes that identity. What differs on
Kubernetes is a separate, broker-side requirement: the resolved GSA must have a KSA
mapped to it in `kubernetes_service_account_mappings` (runtime or profile level, in the
broker's own global settings — this mapping is operator-configured only and is
never read from a project's settings), pre-provisioned by the operator — Scion does not
create, annotate, or bind KSAs itself. A dispatch whose GSA has no such mapping fails
before any pod is created, naming the setting to add. See
[Running Scion on Kubernetes](/scion/hosted/ha/kubernetes/#gcp-identity-mode-assign-workload-identity-mapping)
and [setup-gcp.md §2i](/scion/hosted/ha/setup-gcp/#2i-gke-workload-identity-for-gcp-identity-mode-assign).

The hub default does not bypass the existing gates:
- **Assign**: the service account must be verified and hub-scoped, and `gcp_iam_check_mode` must be `enforce`. These are checked when the setting is saved. Each agent creation also runs the same creator `actAs` authorization as project-default assignment, recorded under the audit surface `hub-default`. For a scheduled dispatch, the "creator" is the schedule's immediate creator (the user or agent that created the schedule), the same principal the project-default rung already authorizes against on that path.
- **Passthrough**: applies only when the agent is dispatched to the Hub's embedded (co-located) Runtime Broker. The Hub identifies its embedded Runtime Broker by the ID it records when it starts it, not by the `scion.io/broker-role` label, because a Runtime Broker's owner can set its labels. During startup the Hub API can accept requests before the embedded Runtime Broker has registered; an agent created in that window waits up to 15 seconds for registration rather than being denied immediately. If registration fails, the Hub has no embedded Runtime Broker until it restarts, and the Hub log says so on each affected create. This covers the single-node deployment. Even on the embedded Runtime Broker, passthrough also requires the agent's resolved runtime profile to be a local container runtime (`docker` or `podman`) that shares the host's metadata server. The profile is the one named in the request or the project's active profile, falling back to the Runtime Broker's default profile. A `kubernetes` profile, any other type (including Apple `container`), or a profile the Hub cannot resolve is denied this grant, as is any agent on a Runtime Broker other than the embedded one. In both cases the Hub leaves the agent's GCP identity unset rather than writing **Block**, and logs why. The Runtime Broker re-checks the resolved runtime before it honors passthrough. When this rung leaves the identity unset, the agent gets the runtime default (**Block**, or **Passthrough** on Kubernetes). Without this limit, a hub-wide default would expose the host identity of every registered Runtime Broker to every agent creator. It would also skip the checks on the Runtime Broker's owner and host service account that explicit passthrough requests go through.

### Passthrough Mode Security & PATCH Parity

In **Passthrough Mode**, an agent bypasses explicit service account binding and directly assumes the GCP identity of its GKE/GCE broker host. To prevent unauthorized access to host-level authority:
1. **Broker-Owner Restriction**: The caller must own that specific Runtime Broker or be a Hub admin. On single-node deployments the Hub's embedded Runtime Broker has no recorded owner, so a Hub admin signed in directly (not through a scoped token or a federated identity) counts as its owner. Every other Runtime Broker still requires ownership or admin rights.
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
| `super-admin` | Full platform administrator with all permissions (System Role). |
| `hub-admin` | Hub administrator with scopeable admin permissions (System Role). |
| `hub-member` | Standard user; read access to directory resources, can create their own projects and register Runtime Brokers (`broker.create`) (System Role). |
| `hub-viewer` | Read-only access to directory resources (System Role). |
| `global-catalog-author` | Non-admin global skill authoring; grants only `skill.create_global` (System Role). |
| `project-owner` | Full project permissions, including agent lifecycle, messaging and forwarded-port access. Does not include `agent.attach` on other members' agents. |
| `project-admin` | Like `project-owner`, but without `agent.delete` or `agent.set_message_mode`. |
| `project-member` | Basic project permissions. |

:::caution[Breaking change: role revision 3; port access restored in revision 5]
The `project-owner` and `project-admin` roles are at revision 5. On upgrade, existing Hubs reconcile these roles automatically. Revision 3 removed `agent.attach` and `agent.port_access` and added `agent.lifecycle`; revision 4 added `gcp_service_account.assign`; revision 5 restores `agent.port_access`, so owners and admins can open forwarded ports on any agent in their project. Owners and admins who previously attached to other members' agents can no longer do so. User access tokens minted before the split that hold `agent:attach` keep lifecycle authority so existing automation continues to work. See [Personal Access Tokens](/scion/hosted/user/personal-access-tokens/) for the current scope list.
:::

### Hub Roles

Every user has one **hub role**: `admin`, `member` or `viewer`. It is shown in **Admin > Users** and as a badge on the user's own profile page. The hub role decides what a user can do across the whole hub. The hub grants it through the system roles above:

| Hub role | Granted through | What it allows |
|----------|-----------------|----------------|
| `admin` | A system-scope `super-admin` binding | Full administrative access to the hub. |
| `member` | Membership of the `hub-members` group, which holds the `hub-member` role | Read the hub directory and catalogs (users, groups, templates, harness configs, brokers, skills, and so on), **create projects** and **register Runtime Brokers**. |
| `viewer` | A system-scope `hub-viewer` binding | The same as `member`, but **cannot create projects** or **register Runtime Brokers**. This includes cloning a project. |

- **Project roles are independent of the hub role.** A viewer can still be added to a project, and then works in it according to their project role (`project-member`, `project-admin` or `project-owner`). The hub role only controls hub-level actions, such as creating a project.
- **New users** get the hub role set by [`server.auth.default_user_role`](/scion/reference/server-config/#authentication-serverauth) (`member` unless configured otherwise). It is applied when the account is first created or activated, which includes the first sign-in of an invited or allow-listed user. Invites and allow-list entries carry no role of their own. Users listed in `admin_emails` are always admins.
- **Changing the default does not change existing users.** To change an individual user's role, use **Change role** in the actions menu on **Admin > Users**, or `PATCH /api/v1/users/{id}` with `{"role": "viewer"}` (`admin`, `member` or `viewer`). A pending invite has no role yet, so its role cannot be changed until the user has signed in.
- **Role changes take effect immediately.** The hub updates the user's group membership and role bindings when the role changes, whether an admin changes it or it changes at sign-in. No hub restart is needed.
- The UI hides controls the user's hub role does not allow. For example, a viewer does not see **Create Project**.

`server.auth.default_user_role` is not the same setting as `server.federation.trusted_issuers[].default_role`, which sets the role for users who authenticate with federated OIDC tokens.

### Tiered Agent Authorization Roles

Scion implements a dedicated, tiered authorization model for **agents**. This ensures that running agents only possess the specific permissions they need to interact with the Hub API.

Agents are assigned one of four named roles, each mapping to a fixed set of JWT scopes:

| Agent Role | Granted Scopes | Description |
| :--- | :--- | :--- |
| `none` | *None* | No access to the Hub API (runs with no authorization claims). |
| `readonly` | `project:read` | Can view and query project state, but cannot report status, register port forwards, or manage other agents. |
| `baseline` | `project:read`<br>`agent:status:update`<br>`agent:token:refresh`<br>`project:agent:notify`<br>`agent:port:forward` | Standard execution permissions. Allows the agent to report progress, refresh its token, register reverse-proxied port forwards, send notifications, and manage its own notification subscriptions. |
| `full` | *All baseline scopes* +<br>`project:agent:create`<br>`project:agent:sa_assign`<br>`project:agent:lifecycle`<br>`project:secret:read` | Complete agent control. Allows spawning child (sub) agents, assigning GCP service accounts to agents, managing their lifecycles, and reading project-scoped secrets from the secret backend. |

#### Creation-Time Role Ceilings
The effective role granted to an agent at creation depends on the caller:

$$\text{user dispatch} = \min(\text{requestedRole}, \text{projectMax})$$

$$\text{sub-agent dispatch} = \min(\text{requestedRole}, \text{parentRole}, \text{projectMax})$$

1. **Requested Role**: The role requested during agent dispatch (e.g., using the `--role` flag in the CLI). For user dispatches, an omitted role defaults to the project-level or Hub-level `default_agent_role`. For sub-agent dispatches, it inherits the parent agent's role.
   - **Default Role Update**: For better usability, the default fallback role has been changed from `baseline` to `full`.
   - **Configuration Options**: You can specify `default_agent_role` globally under `agent_defaults` in the Hub settings (via settings/admin UI) or customize it per-project using the admin UI dropdown or the project setting `scion.io/default-agent-role`.
2. **Project Max**: Set by the project's `max_agent_role` setting, which defaults to the global Hub configuration (`default_max_agent_role` under `agent_defaults`).
3. **Parent Role**: For sub-agent dispatches, the child cannot exceed the parent agent's stored role. Explicit over-requests are rejected with `403 Forbidden`.

The live delegation check separately requires the caller to hold agent-creation authority in the target project.

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
- **Users List**: View all authenticated users, search for specific accounts, track "Last Seen" timestamps, and manage their system-wide roles (e.g., granting `hub-admin` access). Administrators can also **revoke all active sessions** for a user, forcing immediate re-authentication across all devices (see [Session Revocation](#per-user-session-revocation) below).
- **Groups Management**: Full-featured admin UI/UX for creating and managing custom membership groups. Administrators can easily define hierarchical collections of users and manage their membership using a human-friendly editor with user search autocomplete. Group creation is strictly authorized, and the `project:` prefix is a reserved slug. To prevent slug collisions, colliding group identifiers require a system marker combined with the `ProjectID`. Membership lookups rely on canonical identity resolution. This enables policy-based authorization where permissions can be granted to an entire team at once, while strictly enforcing group ownership and authorization rules.
- **Access Boundaries**: Full-featured administrative suite for defining and managing monotonic permission ceilings (AccessConstraints) via the Hub Admin UI.
  - **Inventory Page**: Provides a centralized view of all active and disabled access boundaries configured on the Hub.
  - **Guided Authoring Workflow**: A step-by-step UI workflow for creating and editing boundaries, including targeting individual principals, group closures, or all principals, and specifying allowed maximum permissions.
  - **Preview Engine**: An interactive evaluation sandbox enabling administrators to simulate, dry-run, and verify the impact of an access boundary on a principal's effective permissions prior to committing. Backed by the **Provenance/Explain API**, it details exactly which positive permissions are restricted and why.
  - **Transactional Governance & Atomic Audit**: Built-in backend security guarantees that all access boundary operations are transactionally secure and recorded in the atomic mutation audit log.
  - **Audit History**: Each boundary's detail page shows an audit timeline backed by `GET /api/v1/admin/access-constraints/{id}/audit` (newest first, cursor-paged). Creating a boundary writes a structured audit event and its history entry in the same transaction as the boundary itself, so a boundary never exists without its creation record. The Hub keeps the most recent 1,000 history entries per boundary.
- **Role & Binding Management**: Full CRUD interfaces for Role Definitions and Role Bindings. Administrators can define custom roles, map permissions, and bind them to users, groups, or agents at the Hub or Project scope, while the system enforces `CanDelegate` checks to prevent privilege escalation. Project owners can also assign and remove existing custom project roles for users and groups on their own project from the project's Members editor, alongside the member's built-in role (owner, admin or member); each save is applied atomically and audited, and the same `CanDelegate` ceiling applies, so an owner can only grant permissions they hold themselves. Project admins see custom roles read-only. Custom roles can't be granted to agents from the Members editor; hub administrators can still bind them through the admin Role Bindings page. In the Members editor's add dialog, a role you can't assign is shown disabled with the reason. The **Source** column appears only when some access in the view comes from somewhere other than a direct binding, such as a group. On a project's members group, the group owner can't be changed.
- **Quota Management**: Dedicated admin view to manage the Quota System. Administrators can view, create, and update `LimitDefinition` thresholds and monitor `EntitlementBinding` status across projects.
- **Admin Security & Navigation**: The dashboard uses a **Per-Resource Permission-Gated Admin UI** to render and restrict access to the Admin Suite. Instead of a binary `role===admin` check:
  - Nav and route guards use granular, per-item permission checks.
  - The admin status API endpoint returns a per-resource permissions array that determines what elements are active and visible in the Admin UI.
  - Settings page tabs are gated by the caller's actual resource-level permissions. For example, a role with template-only permissions (like `template.*`) sees only the Templates tab, while other administrative tabs are hidden.
- **Broker Visibility**: Comprehensive broker detail pages provide a grouped view of all active agents by their respective projects, helping administrators understand resource distribution.
- **Maintenance Mode**: Administrators can toggle maintenance mode for the Hub and Web servers directly from the UI to facilitate safe infrastructure updates.

By leveraging these administrative views, Platform Ops can efficiently map their organization's structure directly into Scion's Principal and Policy hierarchy.
## Per-User Session Revocation

Administrators can force any user to re-authenticate by revoking all of their active sessions. This is useful when a user's credentials may be compromised, when an account needs to be immediately locked out, or after a security incident.

### How It Works

Each user record carries a `session_generation` counter. When an admin revokes a user's sessions, the counter is incremented. On every subsequent web request, the Hub middleware compares the counter stored in the user's session cookie against the database value. If the database value is higher, the session is invalidated immediately and the user is redirected to re-authenticate.

### Usage

- **Web Dashboard**: On the Admin Users page, open the actions menu for a user and select **Revoke Sessions**. A confirmation dialog appears; on confirmation the revocation takes effect immediately.
- **API**: `POST /api/v1/users/:id/revoke-sessions` (requires admin privileges).

Session revocation affects cookie-based web sessions only. Agent tokens and User Access Tokens (UATs) are managed through their own revocation mechanisms.

## Break-Glass Admin Recovery

If all admin users have been removed or an organization has lost administrative access to the Hub, the `scion admin promote` CLI command provides an emergency recovery path. This command connects directly to the database — bypassing the running Hub server — and promotes an existing user to the admin role.

```bash
scion admin promote --email user@example.com
```

The target user must already exist in the database. See the [CLI Reference](/scion/reference/cli/#scion-admin-promote) for the full command syntax and flags.

:::caution[Break-glass only]
This command modifies the database directly. Use it only when normal admin access through the Hub API or Web Dashboard is unavailable.
:::

### AdminEmails and UI-Promoted Admins

Users listed in the `admin_emails` server setting are always admins: they are promoted to admin when they sign in. When the list is non-empty, removing an email from it demotes that admin to the hub's [default role for new users](#hub-roles) (`server.auth.default_user_role`) at the next hub restart or their next sign-in, whichever comes first. Their permissions change at once. At restart, both `admin_emails` and the default role come from `settings.yaml` or the environment, so a change made only in the Admin UI (Postgres mode) takes effect at the user's next sign-in. If the default role was set only in the Admin UI, a user demoted at restart becomes Member.

There are two exceptions:

- **UI-promoted admins keep admin.** A user promoted to admin through the Web Dashboard (the Users list) or the users API holds admin because of that explicit action, not because of `admin_emails`. Removing their email from `admin_emails` does not demote them. To remove their admin role, change it on **Admin > Users**.
- **The startup safety check must have passed.** When the hub starts, it checks that the `admin_emails` from its startup configuration (`settings.yaml` or the environment) matches at least one existing user, or that at least one UI-promoted admin exists. If not, the hub refuses all demotions, both at startup and at sign-in, until the configuration is fixed **and the hub is restarted**. This stops a configuration mistake from removing every administrator.
