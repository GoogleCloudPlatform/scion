# Authorization Operation Catalog

*Generated from Go-native OperationSpec definitions. Do not edit manually.*

**Operations:** 113

## Table of Contents

- [agent.lifecycle.create](#agentlifecyclecreate) — Create an agent in a project
- [agent.lifecycle.delete](#agentlifecycledelete) — Delete an agent
- [agent.lifecycle.control](#agentlifecyclecontrol) — Start, stop, suspend or restart an agent
- [agent.lifecycle.restore](#agentlifecyclerestore) — Restore a soft-deleted agent
- [agent.lifecycle.exec](#agentlifecycleexec) — Run a command in an agent's container
- [agent.lifecycle.env](#agentlifecycleenv) — Submit environment values to an agent
- [agent.lifecycle.resetauth](#agentlifecycleresetauth) — Reset an agent's harness authentication
- [agent.lifecycle.reincarnate](#agentlifecyclereincarnate) — Reincarnate an agent
- [agent.read](#agentread) — Read a single agent's metadata by ID
- [agent.list](#agentlist) — List agents within the caller's authorized project scope
- [agent.update](#agentupdate) — Update agent configuration or metadata
- [agent.attach](#agentattach) — Attach to an agent session via WebSocket
- [agent.portaccess](#agentportaccess) — Access forwarded ports on an agent
- [agent.stopall](#agentstopall) — Stop all running agents in a project
- [agent.setmessagemode](#agentsetmessagemode) — Change an agent's message mode
- [agent.token.refresh](#agenttokenrefresh) — Refresh the calling agent's own hub token
- [agent.outbound.message](#agentoutboundmessage) — Deliver an outbound message from the calling agent
- [agent.metrics.report](#agentmetricsreport) — Report runtime metrics for the calling agent
- [agent.secrets.access](#agentsecretsaccess) — List, read and write the secrets available to the calling agent: its project's and its creating user's
- [project.membership.add](#projectmembershipadd) — Add a member to a project with a specified role
- [project.membership.update](#projectmembershipupdate) — Change a project member's role
- [project.membership.remove](#projectmembershipremove) — Remove a member from a project
- [project.membership.list](#projectmembershiplist) — List project members and their roles
- [project.membership.transfer](#projectmembershiptransfer) — Atomically transfer project ownership from the actor to another user
- [project.lifecycle.create](#projectlifecyclecreate) — Create a new project
- [project.lifecycle.delete](#projectlifecycledelete) — Delete a project with cascading security state cleanup and atomic audit
- [project.read](#projectread) — Read a single project's metadata by ID or slug
- [project.list](#projectlist) — List projects within the caller's authorized scope
- [project.update](#projectupdate) — Update project settings and metadata
- [project.register](#projectregister) — Register a project from an external source
- [schedule.event.read](#scheduleeventread) — Read scheduled events or list events in a project
- [schedule.event.create](#scheduleeventcreate) — Create a scheduled event or recurring schedule
- [schedule.event.update](#scheduleeventupdate) — Update a recurring schedule
- [schedule.event.delete](#scheduleeventdelete) — Cancel a scheduled event or delete a recurring schedule
- [agent.message.send](#agentmessagesend) — Send a message to an agent
- [chat.access](#chataccess) — Access chat threads, spaces, topics, and messages within a project
- [role.definition.create](#roledefinitioncreate) — Create a custom role definition
- [role.definition.update](#roledefinitionupdate) — Update a custom role definition
- [role.definition.delete](#roledefinitiondelete) — Delete a custom role definition
- [role.binding.create](#rolebindingcreate) — Create a role binding (grant authority to a principal)
- [role.binding.delete](#rolebindingdelete) — Delete a role binding (revoke authority from a principal)
- [group.member.add](#groupmemberadd) — Add a member to a group
- [group.member.remove](#groupmemberremove) — Remove a member from a group
- [group.delete](#groupdelete) — Delete a group
- [access.constraint.create](#accessconstraintcreate) — Create an access constraint (tighten boundary)
- [access.constraint.update](#accessconstraintupdate) — Update an access constraint (may relax or tighten boundary)
- [access.constraint.delete](#accessconstraintdelete) — Delete an access constraint (relax boundary)
- [credential.token.create](#credentialtokencreate) — Create a user access token (UAT)
- [credential.token.revoke](#credentialtokenrevoke) — Revoke or delete a user access token
- [user.admin.suspend](#useradminsuspend) — Suspend or reactivate a user account (dispatched from PATCH /api/v1/users/{id} when status field is present)
- [user.admin.invite](#useradmininvite) — Invite a user to the platform
- [user.admin.promote](#useradminpromote) — Promote or demote a user's administrative level (dispatched from PATCH /api/v1/users/{id} when role field is present)
- [user.admin.delete](#useradmindelete) — Delete a user account
- [group.read](#groupread) — Read group details or list groups
- [group.create](#groupcreate) — Create a new group
- [group.update](#groupupdate) — Update group metadata
- [user.read](#userread) — Read user profile or list users
- [user.update](#userupdate) — Update user profile or settings (PATCH may also dispatch user.admin.suspend/promote per field)
- [role.read](#roleread) — Read role definitions and permission registry
- [role.binding.read](#rolebindingread) — Read role binding assignments
- [access.constraint.read](#accessconstraintread) — Read access constraint definitions
- [user.provision](#userprovision) — Create a user directly through the API; refused for every caller, because sign-in flows create users
- [user.session.logout](#usersessionlogout) — Sign-in flow logout step; the hub holds no server-side session state for it to change
- [hub.authreset](#hubauthreset) — Reset all agent authentication credentials (emergency action)
- [hub.config.read](#hubconfigread) — Read server configuration and schema
- [hub.config.update](#hubconfigupdate) — Update server configuration sections
- [hub.messaging.update](#hubmessagingupdate) — Read and update messaging configuration switches
- [hub.experiments.update](#hubexperimentsupdate) — Read and update hub-wide experiment overrides
- [hub.maintenance.execute](#hubmaintenanceexecute) — Execute maintenance operations including migrations and restarts
- [hub.adminmode.update](#hubadminmodeupdate) — Toggle admin/maintenance mode
- [hub.allowlist.update](#huballowlistupdate) — Manage the platform email allow list
- [hub.health.read](#hubhealthread) — Read platform health summary and GCP quota status
- [hub.diagnostics.read](#hubdiagnosticsread) — Read diagnostic logs and messaging divergence data
- [hub.scheduler.read](#hubschedulerread) — Read scheduler status and configuration
- [hub.projectdefaults.read](#hubprojectdefaultsread) — Read project default settings
- [hub.lifecyclehooks.read](#hublifecyclehooksread) — Read lifecycle hook definitions
- [hub.validate.execute](#hubvalidateexecute) — Validate resource definitions against schema
- [hub.integrations.read](#hubintegrationsread) — Read integration configurations
- [hub.teamsmanifest.read](#hubteamsmanifestread) — Read Teams integration manifest
- [hub.metrics.read](#hubmetricsread) — Read metrics dashboard data
- [hub.githubapp.read](#hubgithubappread) — Read GitHub App configuration and installations
- [hub.githubapp.update](#hubgithubappupdate) — Update GitHub App configuration, manage installations, discover and sync
- [quota.read](#quotaread) — Read limit definitions, entitlements, and usage
- [quota.create](#quotacreate) — Create limit definitions and entitlement bindings
- [quota.update](#quotaupdate) — Update limit definitions and entitlement bindings
- [quota.delete](#quotadelete) — Delete limit definitions and entitlement bindings
- [hub.policies.removed](#hubpoliciesremoved) — Removed policy API; every method and sub-path answers 410 Gone and points callers to role bindings
- [skill.read](#skillread) — Read skill definitions or list/discover skills
- [skill.create](#skillcreate) — Create a new skill definition
- [skill.update](#skillupdate) — Update an existing skill definition
- [skill.delete](#skilldelete) — Delete a skill definition
- [skill.register](#skillregister) — Register skills in a skill registry
- [template.read](#templateread) — Read template definitions or discover available templates
- [template.create](#templatecreate) — Create a new template or import resources
- [template.update](#templateupdate) — Update an existing template definition
- [template.delete](#templatedelete) — Delete a template definition
- [harnessconfig.read](#harnessconfigread) — Read harness configurations or list available configs
- [harnessconfig.create](#harnessconfigcreate) — Create a new harness configuration
- [harnessconfig.update](#harnessconfigupdate) — Update a harness configuration
- [harnessconfig.delete](#harnessconfigdelete) — Delete a harness configuration
- [broker.read](#brokerread) — Read runtime broker status or list brokers
- [broker.agent.launchreport](#brokeragentlaunchreport) — Record a broker's launch report for an agent it runs
- [broker.messagefailures.report](#brokermessagefailuresreport) — Record buffered message delivery failures reported by a broker
- [broker.controlchannel.call](#brokercontrolchannelcall) — Carry a call between the hub and a connected broker over the control channel; each call runs under the operation that initiated it
- [gcp.identity.create](#gcpidentitycreate) — Create a GCP service account binding
- [gcp.identity.delete](#gcpidentitydelete) — Delete a GCP service account binding
- [gcp.identity.assign](#gcpidentityassign) — Assign a GCP service account to an agent
- [gcp.identity.mint](#gcpidentitymint) — Mint a GCP access token for a service account
- [secret.read](#secretread) — Read project secrets or environment variables containing secrets
- [secret.write](#secretwrite) — Create or update project secrets
- [gcp.identity.read](#gcpidentityread) — Read GCP service account details or list accounts
- [gcp.identity.verify](#gcpidentityverify) — Verify a GCP service account's IAM configuration
- [env.read](#envread) — Read project environment variables

---

## agent.lifecycle.create

**Domain:** agent

**Description:** Create an agent in a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_body`; boundaries `project`, `hub`)

**Base Permission:** `agent.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

### Delegation

- **Kind:** `non_amplification`
- Actor must hold the role and scopes delegated to the new agent (CanDelegate non-amplification); an agent actor is also evaluated against the delegation ceiling of its live delegation chain for agent.create on the target project

**Denial Codes:** `forbidden`, `conflict`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestAgentCreate_ExplicitRoleAboveParentDenied`
- `pkg/hub:TestAgentCreate_RequiresLiveDelegator`

---

## agent.lifecycle.delete

**Domain:** agent

**Description:** Delete an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/agents/{id}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.delete`

**Resource Resolver:** agent-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `agent.lifecycle.delete`
- **Context Fields:** actor_id, project_id
- **Before Fields:** agent_id, agent_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## agent.lifecycle.control

**Domain:** agent

**Description:** Start, stop, suspend or restart an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/start` |
| http_route | POST | `/api/v1/agents/{id}/stop` |
| http_route | POST | `/api/v1/agents/{id}/suspend` |
| http_route | POST | `/api/v1/agents/{id}/restart` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/start` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/stop` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/suspend` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/restart` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.lifecycle`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.lifecycle.restore

**Domain:** agent

**Description:** Restore a soft-deleted agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/restore` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/restore` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.lifecycle`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `conflict`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.lifecycle.exec

**Domain:** agent

**Description:** Run a command in an agent's container

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/exec` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/exec` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.attach`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.lifecycle.env

**Domain:** agent

**Description:** Submit environment values to an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/env` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/env` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.attach`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.lifecycle.resetauth

**Domain:** agent

**Description:** Reset an agent's harness authentication

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/reset-auth` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/reset-auth` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.attach`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.lifecycle.reincarnate

**Domain:** agent

**Description:** Reincarnate an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/reincarnate` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/reincarnate` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.lifecycle`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestAgentSubRoute_CatalogDrift`

---

## agent.read

**Domain:** agent

**Description:** Read a single agent's metadata by ID

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.read`

**Resource Resolver:** agent-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## agent.list

**Domain:** agent

**Description:** List agents within the caller's authorized project scope

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Base Permission:** `agent.list`

**Resource Resolver:** list-scope-resolver

**Effects:** `list-scoped`

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| scope-pushed-query | security | Rows, totalCount, and nextCursor come from the same SQL predicate that includes the authorization scope | Yes |
| cursor-scope-binding | security | Cursor binding includes endpoint, caller filters, authorization scope, and principal/credential context | Yes |
| no-broad-query-on-none | security | ScopeSetNone produces empty list without issuing any resource query | Yes |
| slug-not-oracle | security | Project slug lookup for agent list filter must not distinguish unauthorized from nonexistent | Yes |

**Denial Codes:** `forbidden`, `credential_insufficient`, `user_suspended`

### Tests

- `pkg/hub:TestRS2_AgentListScopePushed`
- `pkg/hub:TestRS2_AgentListMineSharedClassification`
- `pkg/hub:TestRS2_AgentListSlugOracle`
- `pkg/hub:TestRS2_AgentListMultiPageInterleaved`
- `pkg/hub:TestRS2_FailureInjection_PrincipalGroupClosure`
- `pkg/hub:TestRS2_FailureInjection_StoreListCount`
- `pkg/hub:TestRS2_CursorReplayAfterGrantRemoval`
- `pkg/hub:TestRS2_CursorReplayAfterBindingExpiry`
- `pkg/hub:TestRS2_AllPlusConstraint_EndToEnd`
- `pkg/hub:TestRS2_ProductionAgentJWT`
- `pkg/hub:TestRS2_SystemAllSharedSemantics`
- `pkg/hub:TestRS2_GroupChangeCursorReplay`
- `pkg/hub:TestRS2_ConstraintChangeCursorReplay`
- `pkg/hub:TestRS2_TransitiveGroupAccess`
- `pkg/hub:TestRS2_FilterCompositionMatrix`

---

## agent.update

**Domain:** agent

**Description:** Update agent configuration or metadata

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/agents/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `agent.update`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## agent.attach

**Domain:** agent

**Description:** Attach to an agent session via WebSocket

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| websocket | GET | `/api/v1/agents/{id}/pty` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `agent_record`; boundaries `project`, `hub`)

**Base Permission:** `agent.attach`

**Resource Resolver:** agent-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## agent.portaccess

**Domain:** agent

**Description:** Access forwarded ports on an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}/ports` |
| http_route | GET | `/api/v1/agents/{id}/ports/{port}/proxy` |
| http_route | POST | `/api/v1/agents/{id}/ports/{port}/proxy` |
| http_route | PUT | `/api/v1/agents/{id}/ports/{port}/proxy` |
| http_route | DELETE | `/api/v1/agents/{id}/ports/{port}/proxy` |
| http_route | GET | `/api/v1/agents/{id}/ports/{port}/proxy/{subpath}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Base Permission:** `agent.port_access`

**Resource Resolver:** agent-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## agent.stopall

**Domain:** agent

**Description:** Stop all running agents in a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/stop-all` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `agent.stop_all`

**Resource Resolver:** project-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## agent.setmessagemode

**Domain:** agent

**Description:** Change an agent's message mode

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/set_message_mode` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/set_message_mode` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `agent.set_message_mode`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## agent.token.refresh

**Domain:** agent

**Description:** Refresh the calling agent's own hub token

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/token/refresh` |
| http_route | POST | `/api/v1/agents/{id}/refresh-token` |

**Principals:** `agent`

**Credentials:** `agent_jwt`

**Bearer:** `non_user`

**Resource Resolver:** agent-self

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** The agent authenticates with its own agent JWT for its own record; no user permission applies (scope: agent self access) — waives: `base_permission`

---

## agent.outbound.message

**Domain:** agent

**Description:** Deliver an outbound message from the calling agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/outbound-message` |
| http_route | POST | `/api/v1/projects/{projectId}/agents/{id}/outbound-message` |

**Principals:** `agent`

**Credentials:** `agent_jwt`

**Bearer:** `non_user`

**Resource Resolver:** agent-self

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** The agent authenticates with its own agent JWT for its own record; no user permission applies (scope: agent self access) — waives: `base_permission`

---

## agent.metrics.report

**Domain:** agent

**Description:** Report runtime metrics for the calling agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents/{id}/metrics` |

**Principals:** `agent`

**Credentials:** `agent_jwt`

**Bearer:** `non_user`

**Resource Resolver:** agent-self

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** The agent authenticates with its own agent JWT for its own record; no user permission applies (scope: agent self access) — waives: `base_permission`

---

## agent.secrets.access

**Domain:** agent

**Description:** List, read and write the secrets available to the calling agent: its project's and its creating user's

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}/secrets` |
| http_route | GET | `/api/v1/agents/{id}/secrets/{key}` |
| http_route | PUT | `/api/v1/agents/{id}/secrets/{key}` |

**Principals:** `agent`

**Credentials:** `agent_jwt`

**Bearer:** `non_user`

**Resource Resolver:** agent-self

**Effects:** `read-secret`, `update-resource`

### Audit

- **Event Type:** `agent.secrets.access`
- **Context Fields:** actor_id, project_id, scope
- **Before Fields:** secret_key
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** The agent authenticates with its own agent JWT, whose subject must match the agent ID in the path; no user permission applies; the project and user scope IDs come from the token (scope: agent self access) — waives: `base_permission`

---

## project.membership.add

**Domain:** project.membership

**Description:** Add a member to a project with a specified role

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/members` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-url

**Effects:** `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- Actor must hold all permissions in the target role (CanDelegate non-amplification)

### Governance

- **Kind:** peer_superior
- RS1 governance: CT1 D5 typed governance matrix — owners manage all roles, admins manage members only. Enforced by ProjectMembershipService.checkGovernance.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| direct-user-only-owner | security | project-owner role is direct-user-only | Yes |
| single-binding-per-principal | business | CT1 D4: one direct binding per principal per project | No |

### Audit

- **Event Type:** `project.membership.add`
- **Context Fields:** actor_id, project_id
- **After Fields:** target_principal_id, target_role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`, `target_role_protected`, `principal_ineligible`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.membership.update

**Domain:** project.membership

**Description:** Change a project member's role

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/projects/{id}/members/{memberId}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-url

**Effects:** `change-authority`

### Delegation

- **Kind:** `conditional_on_increase`
- CanDelegate checked when new role has more permissions than old role

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** peer_superior
- RS1 governance: CT1 D5 typed governance matrix — owners manage all roles, admins manage members only. Both old and new target roles are governed. Enforced by ProjectMembershipService.checkGovernance.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| direct-user-only-owner | security | project-owner role is direct-user-only | Yes |
| last-owner-guard | security | Cannot demote the last active direct owner | Yes |

### Audit

- **Event Type:** `project.membership.update`
- **Context Fields:** actor_id, project_id
- **Before Fields:** target_principal_id, old_role
- **After Fields:** new_role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`, `target_role_protected`, `last_owner`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.membership.remove

**Domain:** project.membership

**Description:** Remove a member from a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/projects/{id}/members/{memberId}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-url

**Effects:** `revoke-authority`

**Authority Evaluation:** `proposed_post_state`

### Governance

- **Kind:** peer_superior
- RS1 governance: CT1 D5 typed governance matrix — owners manage all roles, admins manage members only. CT1 D1 allows self-removal when another active direct owner remains. Enforced by ProjectMembershipService.checkGovernance.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| last-owner-guard | security | Cannot remove the last active direct owner | Yes |

### Audit

- **Event Type:** `project.membership.remove`
- **Context Fields:** actor_id, project_id
- **Before Fields:** target_principal_id, target_role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`, `target_role_protected`, `last_owner`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.membership.list

**Domain:** project.membership

**Description:** List project members and their roles

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}/members` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.membership.transfer

**Domain:** project.membership

**Description:** Atomically transfer project ownership from the actor to another user

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/transfer-ownership` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `project.manage`

**Resource Resolver:** project-from-url

**Effects:** `change-authority`

### Delegation

- **Kind:** `conditional_on_increase`
- Actor must be a direct project owner; target is promoted to owner, actor is downgraded to member — conditional-on-increase applies to the target's authority change

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** peer_superior
- RS1 governance: only active direct project owners may transfer ownership. Actor-must-be-direct-owner is enforced by the ProjectMembershipService.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| direct-user-only-owner | security | project-owner role is direct-user-only | Yes |
| last-owner-guard | security | Post-state: at least one active direct owner must remain | Yes |
| single-binding-per-principal | business | CT1 D4: one direct binding per principal per project; atomic replacement for both actor and target | No |

### Audit

- **Event Type:** `project.membership.transfer`
- **Context Fields:** actor_id, project_id
- **Before Fields:** old_owner_id
- **After Fields:** new_owner_id, old_owner_role, new_owner_role
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`, `principal_ineligible`, `last_owner`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.lifecycle.create

**Domain:** project

**Description:** Create a new project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `project.create`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.lifecycle.delete

**Domain:** project

**Description:** Delete a project with cascading security state cleanup and atomic audit

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/projects/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `IRREVERSIBLE_CASCADE`)

**Base Permission:** `project.delete`

**Resource Resolver:** project-from-url

**Effects:** `delete-resource`, `emit-external-effect`

### Governance

- **Kind:** ownership_ancestry
- RS3 governance: direct project owner or super-admin. Hub-admin lacks project.delete and is denied at base permission. Group-derived ownership does not confer deletion authority. Stale Project.OwnerID is not consulted. Enforced by ProjectDeletionService.checkDeletionGovernance.

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| target-exists | business | Project must exist and not be already deleted | Yes |

### Audit

- **Event Type:** `project.lifecycle.delete`
- **Context Fields:** actor_id
- **Before Fields:** project_id, project_name, project_slug, owner_id
- **After Fields:** cascade_summary
- **Atomic:** Yes

### External Effect Policy

- **Delivery:** `fire_and_forget`
- **Failure Mode:** `log_and_continue`
- **Idempotency:** project ID (single deletion per project)
- **Retry:** no retry — cascading deletes are best-effort; DB cascade is authoritative
- **Auth Before Emit:** Yes

**Denial Codes:** `forbidden`, `user_suspended`, `credential_insufficient`, `not_found`

### Tests

- `pkg/hub:TestRS3_ProjectDeleteOwnerPositiveControl`
- `pkg/hub:TestRS3_ProjectDeleteGovernanceMatrix`
- `pkg/hub:TestRS3_ProjectDeleteAtomicAudit`

---

## project.read

**Domain:** project

**Description:** Read a single project's metadata by ID or slug

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.list

**Domain:** project

**Description:** List projects within the caller's authorized scope

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Base Permission:** `project.list`

**Resource Resolver:** list-scope-resolver

**Effects:** `list-scoped`

### Invariants

| ID | Kind | Description | Fail-Closed |
|----|------|-------------|-------------|
| scope-pushed-query | security | Rows, totalCount, and nextCursor come from the same SQL predicate that includes the authorization scope | Yes |
| cursor-scope-binding | security | Cursor binding includes endpoint, caller filters, authorization scope, and principal/credential context | Yes |
| no-broad-query-on-none | security | ScopeSetNone produces empty list without issuing any resource query | Yes |

**Denial Codes:** `forbidden`, `credential_insufficient`, `user_suspended`

### Tests

- `pkg/hub:TestRS2_ProjectListScopePushed`
- `pkg/hub:TestRS2_ProjectListMineSharedClassification`
- `pkg/hub:TestRS2_ProjectListCursorBinding`
- `pkg/hub:TestRS2_ProjectListMultiPageInterleaved`
- `pkg/hub:TestRS2_ProjectListInterleavedWithCallerFilter`
- `pkg/hub:TestRS2_FailureInjection_PrincipalGroupClosure`
- `pkg/hub:TestRS2_FailureInjection_StoreListCount`
- `pkg/hub:TestRS2_CursorReplayAfterGrantRemoval`
- `pkg/hub:TestRS2_CursorReplayAfterBindingExpiry`
- `pkg/hub:TestRS2_AllPlusConstraint_EndToEnd`
- `pkg/hub:TestRS2_MalformedConstraintExclusionHTTP`
- `pkg/hub:TestRS2_SystemAllSharedSemantics`
- `pkg/hub:TestRS2_GroupChangeCursorReplay`
- `pkg/hub:TestRS2_ConstraintChangeCursorReplay`
- `pkg/hub:TestRS2_SuspensionCursorReplay`
- `pkg/hub:TestRS2_CredentialChangeCursorReplay`
- `pkg/hub:TestRS2_TransferredOwnership`
- `pkg/hub:TestRS2_TransitiveGroupAccess`
- `pkg/hub:TestRS2_FilterCompositionMatrix`

---

## project.update

**Domain:** project

**Description:** Update project settings and metadata

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/projects/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `project_path`; boundaries `project`, `hub`)

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.register

**Domain:** project

**Description:** Register a project from an external source

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/register` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `project.register`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## schedule.event.read

**Domain:** schedule

**Description:** Read scheduled events or list events in a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{projectId}/scheduled-events` |
| http_route | GET | `/api/v1/projects/{projectId}/scheduled-events/{id}` |
| http_route | GET | `/api/v1/projects/{projectId}/schedules` |
| http_route | GET | `/api/v1/projects/{projectId}/schedules/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `scheduled_event.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## schedule.event.create

**Domain:** schedule

**Description:** Create a scheduled event or recurring schedule

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{projectId}/scheduled-events` |
| http_route | POST | `/api/v1/projects/{projectId}/schedules` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `scheduled_event.create`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## schedule.event.update

**Domain:** schedule

**Description:** Update a recurring schedule

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/projects/{projectId}/schedules/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `scheduled_event.update`

**Resource Resolver:** project-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## schedule.event.delete

**Domain:** schedule

**Description:** Cancel a scheduled event or delete a recurring schedule

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/projects/{projectId}/scheduled-events/{id}` |
| http_route | DELETE | `/api/v1/projects/{projectId}/schedules/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `scheduled_event.delete`

**Resource Resolver:** project-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `schedule.event.delete`
- **Context Fields:** actor_id, project_id
- **Before Fields:** event_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## agent.message.send

**Domain:** agent.message

**Description:** Send a message to an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| broker_call | — | `broker.inbound` |

**Principals:** `user`, `agent`, `broker`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`, `broker_token`

**Base Permission:** `agent.message`

**Resource Resolver:** agent-from-thread

**Effects:** `emit-external-effect`

### Audit

- **Event Type:** `agent.message.send`
- **Context Fields:** actor_id, project_id
- **After Fields:** message_id, target_agent_id
- **Atomic:** No
- **Non-Atomic Justification:** Message dispatch is fire-and-forget; audit recorded before dispatch

### External Effect Policy

- **Delivery:** `fire_and_forget`
- **Failure Mode:** `log_and_continue`
- **Idempotency:** message ID
- **Retry:** no retry for user-sent messages
- **Auth Before Emit:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## chat.access

**Domain:** chat

**Description:** Access chat threads, spaces, topics, and messages within a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/prefs` |
| http_route | PUT | `/api/v1/chat/prefs` |
| http_route | GET | `/api/v1/chat/spaces` |
| http_route | GET | `/api/v1/chat/spaces/{id}/threads` |
| http_route | GET | `/api/v1/chat/conversations/{id}/messages` |
| http_route | GET | `/api/v1/chat/topics/{id}` |
| http_route | GET | `/api/v1/chat/dms` |
| http_route | GET | `/api/v1/chat/search` |
| http_route | POST | `/api/v1/chat/attachments` |
| http_route | GET | `/api/v1/chat/attachments/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## role.definition.create

**Domain:** role

**Description:** Create a custom role definition

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/roles` |
| http_route | POST | `/api/v1/admin/roles/import` |
| http_route | POST | `/api/v1/admin/roles/{id}/duplicate` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `role.create`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`

### Audit

- **Event Type:** `role.definition.create`
- **Context Fields:** actor_id
- **After Fields:** role_name, permissions
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

### Exemptions

- **authentication_only:** Role CRUD currently requires hub-admin via route guard; full operation contract deferred to AH1 (scope: AF1 catalog only) — waives: `audit_obligation`

---

## role.definition.update

**Domain:** role

**Description:** Update a custom role definition

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/admin/roles/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `role.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## role.definition.delete

**Domain:** role

**Description:** Delete a custom role definition

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/admin/roles/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `role.delete`

**Resource Resolver:** hub-scoped

**Effects:** `delete-resource`

### Audit

- **Event Type:** `role.definition.delete`
- **Context Fields:** actor_id
- **Before Fields:** role_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## role.binding.create

**Domain:** role.binding

**Description:** Create a role binding (grant authority to a principal)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/role-bindings` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `role_binding.create`

**Resource Resolver:** hub-scoped

**Effects:** `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- Actor must hold all permissions in the bound role (CanDelegate)

### Audit

- **Event Type:** `role.binding.create`
- **Context Fields:** actor_id
- **After Fields:** principal_id, role_name, scope
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## role.binding.delete

**Domain:** role.binding

**Description:** Delete a role binding (revoke authority from a principal)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/admin/role-bindings/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `role_binding.delete`

**Resource Resolver:** hub-scoped

**Effects:** `revoke-authority`

**Authority Evaluation:** `proposed_post_state`

### Governance

- **Kind:** peer_superior
- Revoking authority from a peer or superior principal requires governance review

### Audit

- **Event Type:** `role.binding.delete`
- **Context Fields:** actor_id
- **Before Fields:** principal_id, role_name, scope
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `role_assignment_forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## group.member.add

**Domain:** group

**Description:** Add a member to a group

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/groups/{id}/members` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `group.addMember`

**Resource Resolver:** group-from-url

**Effects:** `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- Adding a member to a role-bearing group effectively grants authority; actor must hold the group's role permissions

### Audit

- **Event Type:** `group.member.add`
- **Context Fields:** actor_id, group_id
- **After Fields:** member_principal_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## group.member.remove

**Domain:** group

**Description:** Remove a member from a group

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/groups/{id}/members/{memberType}/{memberId}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `group.removeMember`

**Resource Resolver:** group-from-url

**Effects:** `revoke-authority`

### Governance

- **Kind:** peer_superior
- Removing from a constraint-bearing group may change effective authority; governed by group role hierarchy

### Audit

- **Event Type:** `group.member.remove`
- **Context Fields:** actor_id, group_id
- **Before Fields:** member_principal_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## group.delete

**Domain:** group

**Description:** Delete a group

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/groups/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `group.delete`

**Resource Resolver:** group-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `group.delete`
- **Context Fields:** actor_id
- **Before Fields:** group_id, group_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## access.constraint.create

**Domain:** access.constraint

**Description:** Create an access constraint (tighten boundary)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/access-constraints` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `access_constraint.admin`

**Resource Resolver:** hub-scoped

**Effects:** `tighten-boundary`

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** constraint_admin
- Constraint creation requires constraint admin authority

### Audit

- **Event Type:** `access.constraint.create`
- **Context Fields:** actor_id
- **Before Fields:** effective_authority_before
- **After Fields:** constraint_id, constraint_type, target_scope
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## access.constraint.update

**Domain:** access.constraint

**Description:** Update an access constraint (may relax or tighten boundary)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/admin/access-constraints/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `access_constraint.admin`

**Resource Resolver:** hub-scoped

**Effects:** `relax-boundary`, `tighten-boundary`

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** constraint_admin
- Constraint modification requires constraint admin authority; relaxation has higher governance bar

### Audit

- **Event Type:** `access.constraint.update`
- **Context Fields:** actor_id
- **Before Fields:** constraint_id, old_scope
- **After Fields:** new_scope
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## access.constraint.delete

**Domain:** access.constraint

**Description:** Delete an access constraint (relax boundary)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/admin/access-constraints/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `access_constraint.admin`

**Resource Resolver:** hub-scoped

**Effects:** `relax-boundary`

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** constraint_admin
- Constraint deletion relaxes boundary and requires constraint admin authority

### Audit

- **Event Type:** `access.constraint.delete`
- **Context Fields:** actor_id
- **Before Fields:** constraint_id, constraint_type, target_scope
- **After Fields:** effective_authority_after
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## credential.token.create

**Domain:** credential

**Description:** Create a user access token (UAT)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/auth/tokens` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `user.read`

**Resource Resolver:** self-principal

**Effects:** `mint-credential`

### Governance

- **Kind:** issuer_credential
- User mints tokens for self; token scopes cannot exceed session authority

### Audit

- **Event Type:** `credential.token.create`
- **Context Fields:** actor_id
- **After Fields:** token_id, scopes
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `scope_violation`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestRS4_IssuerAuthority`
- `pkg/hub:TestRS4_TargetScope`
- `pkg/hub:TestRS4_Audit_Mint`

### Exemptions

- **authentication_only:** Token creation is authenticated-only (user manages own tokens); no per-resource permission required beyond session validity (scope: self-token management only) — waives: `base_permission`

---

## credential.token.revoke

**Domain:** credential

**Description:** Revoke or delete a user access token

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/auth/tokens/{id}/revoke` |
| http_route | DELETE | `/api/v1/auth/tokens/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `CREDENTIAL_MANAGEMENT`)

**Base Permission:** `user.read`

**Resource Resolver:** self-principal

**Effects:** `revoke-authority`

### Governance

- **Kind:** issuer_credential
- User may revoke own tokens; admin may revoke via hub-admin path

### Audit

- **Event Type:** `credential.token.revoke`
- **Context Fields:** actor_id
- **Before Fields:** token_id, action
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`
- `pkg/hub:TestRS4_Audit_Revoke`
- `pkg/hub:TestRS4_Audit_Delete`

### Exemptions

- **authentication_only:** Token revocation is authenticated-only (user manages own tokens) (scope: self-token management only) — waives: `base_permission`

---

## user.admin.suspend

**Domain:** user.admin

**Description:** Suspend or reactivate a user account (dispatched from PATCH /api/v1/users/{id} when status field is present)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| internal_dispatch | — | `updateUser:status-field` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `user.suspend`

**Resource Resolver:** user-from-url

**Effects:** `change-principal-status`

### Audit

- **Event Type:** `user.admin.suspend`
- **Context Fields:** actor_id
- **Before Fields:** target_user_id, old_status
- **After Fields:** new_status
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## user.admin.invite

**Domain:** user.admin

**Description:** Invite a user to the platform

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/users/invite` |
| http_route | POST | `/api/v1/admin/users/invite/bulk` |
| http_route | GET | `/api/v1/admin/invites` |
| http_route | GET | `/api/v1/admin/invites/{id}` |
| http_route | DELETE | `/api/v1/admin/invites/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `user.invite`

**Resource Resolver:** hub-scoped

**Effects:** `issue-credential`

### Governance

- **Kind:** issuer_credential
- Invitation issues a credential granting platform access

### Audit

- **Event Type:** `user.admin.invite`
- **Context Fields:** actor_id
- **After Fields:** invite_email, invite_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## user.admin.promote

**Domain:** user.admin

**Description:** Promote or demote a user's administrative level (dispatched from PATCH /api/v1/users/{id} when role field is present)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| internal_dispatch | — | `updateUser:role-field` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `user.promote`

**Resource Resolver:** user-from-url

**Effects:** `change-authority`

### Delegation

- **Kind:** `conditional_on_increase`
- Promotion delegation checked only when effective authority increases

**Authority Evaluation:** `before_and_after`

### Audit

- **Event Type:** `user.admin.promote`
- **Context Fields:** actor_id
- **Before Fields:** target_user_id, old_level
- **After Fields:** new_level
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## user.admin.delete

**Domain:** user.admin

**Description:** Delete a user account

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/users/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `session_only` (reason `GOV_PENDING`)

**Base Permission:** `user.delete`

**Resource Resolver:** user-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `user.admin.delete`
- **Context Fields:** actor_id
- **Before Fields:** target_user_id, email, role, status
- **Atomic:** Yes

**Denial Codes:** `forbidden`, `last_owner`, `conflict`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## group.read

**Domain:** group

**Description:** Read group details or list groups

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/groups` |
| http_route | GET | `/api/v1/groups/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `group.read`

**Resource Resolver:** group-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## group.create

**Domain:** group

**Description:** Create a new group

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/groups` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `group.create`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## group.update

**Domain:** group

**Description:** Update group metadata

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/groups/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `group.update`

**Resource Resolver:** group-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## user.read

**Domain:** user

**Description:** Read user profile or list users

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/users` |
| http_route | GET | `/api/v1/users/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `user.read`

**Resource Resolver:** user-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## user.update

**Domain:** user

**Description:** Update user profile or settings (PATCH may also dispatch user.admin.suspend/promote per field)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/users/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `user.update`

**Resource Resolver:** user-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## role.read

**Domain:** role

**Description:** Read role definitions and permission registry

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/roles` |
| http_route | GET | `/api/v1/admin/roles/{id}` |
| http_route | GET | `/api/v1/admin/roles/export` |
| http_route | GET | `/api/v1/admin/roles/{id}/export` |
| http_route | GET | `/api/v1/admin/permissions` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `role.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## role.binding.read

**Domain:** role.binding

**Description:** Read role binding assignments

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/role-bindings` |
| http_route | GET | `/api/v1/admin/role-bindings/user/{userId}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `role_binding.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## access.constraint.read

**Domain:** access.constraint

**Description:** Read access constraint definitions

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/access-constraints` |
| http_route | GET | `/api/v1/admin/access-constraints/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `access_constraint.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## user.provision

**Domain:** user

**Description:** Create a user directly through the API; refused for every caller, because sign-in flows create users

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/users` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `out_of_scope` (owner `user-provisioning`)

**Resource Resolver:** none

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** Direct user creation is refused for every caller; user records come from sign-in flows (scope: direct user creation) — waives: `base_permission`

---

## user.session.logout

**Domain:** user

**Description:** Sign-in flow logout step; the hub holds no server-side session state for it to change

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/auth/logout` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `non_user`

**Resource Resolver:** none

**Effects:** `update-resource`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **authentication_only:** Sign-in flow step that reads and changes no hub state; no resource permission applies (scope: session logout) — waives: `base_permission`, `denial_codes`

---

## hub.authreset

**Domain:** hub

**Description:** Reset all agent authentication credentials (emergency action)

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/agents/reset-auth-all` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.auth_reset.execute`

**Resource Resolver:** hub-scoped

**Effects:** `revoke-authority`

### Governance

- **Kind:** peer_superior
- Mass auth reset is a drastic authority revocation requiring hub admin governance

### Audit

- **Event Type:** `hub.authreset`
- **Context Fields:** actor_id
- **Before Fields:** agent_count
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.config.read

**Domain:** hub

**Description:** Read server configuration and schema

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/server-config` |
| http_route | GET | `/api/v1/admin/server-config/schema` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.config.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.config.update

**Domain:** hub

**Description:** Update server configuration sections

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/admin/server-config` |
| http_route | DELETE | `/api/v1/admin/server-config/sections/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.config.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.messaging.update

**Domain:** hub

**Description:** Read and update messaging configuration switches

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/messaging` |
| http_route | PUT | `/api/v1/admin/messaging` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.messaging.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.experiments.update

**Domain:** hub

**Description:** Read and update hub-wide experiment overrides

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/experiments` |
| http_route | PUT | `/api/v1/admin/experiments` |
| http_route | DELETE | `/api/v1/admin/experiments` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.experiments.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.maintenance.execute

**Domain:** hub

**Description:** Execute maintenance operations including migrations and restarts

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/maintenance/operations` |
| http_route | GET | `/api/v1/admin/maintenance/operations/{id}` |
| http_route | POST | `/api/v1/admin/maintenance/operations/{id}/run` |
| http_route | POST | `/api/v1/admin/maintenance/restart` |
| http_route | POST | `/api/v1/admin/maintenance/check-updates` |
| http_route | POST | `/api/v1/admin/maintenance/migrations/{id}/run` |
| http_route | GET | `/api/v1/admin/maintenance/update-available` |
| http_route | DELETE | `/api/v1/admin/maintenance/update-available` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.maintenance.execute`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.adminmode.update

**Domain:** hub

**Description:** Toggle admin/maintenance mode

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/admin/maintenance` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.admin_mode.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.allowlist.update

**Domain:** hub

**Description:** Manage the platform email allow list

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/allow-list` |
| http_route | POST | `/api/v1/admin/allow-list` |
| http_route | DELETE | `/api/v1/admin/allow-list/{email}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.allow_list.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`, `last_owner`, `conflict`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.health.read

**Domain:** hub

**Description:** Read platform health summary and GCP quota status

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/health/summary` |
| http_route | GET | `/api/v1/admin/gcp-quota` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.health.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.diagnostics.read

**Domain:** hub

**Description:** Read diagnostic logs and messaging divergence data

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/diagnostics/logs` |
| sse | GET | `/api/v1/admin/diagnostics/logs/stream` |
| http_route | GET | `/api/v1/admin/messaging/divergence` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.diagnostics.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.scheduler.read

**Domain:** hub

**Description:** Read scheduler status and configuration

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/scheduler` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.scheduler.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.projectdefaults.read

**Domain:** hub

**Description:** Read project default settings

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/project-defaults` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.project_defaults.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.lifecyclehooks.read

**Domain:** hub

**Description:** Read lifecycle hook definitions

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/lifecycle-hooks` |
| http_route | GET | `/api/v1/admin/lifecycle-hooks/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.lifecycle_hooks.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.validate.execute

**Domain:** hub

**Description:** Validate resource definitions against schema

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/validate-resources` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.validate.execute`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.integrations.read

**Domain:** hub

**Description:** Read integration configurations

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/integrations` |
| http_route | GET | `/api/v1/admin/integrations/{name}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.integrations.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.teamsmanifest.read

**Domain:** hub

**Description:** Read Teams integration manifest

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/integrations/teams/manifest` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.teams_manifest.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.metrics.read

**Domain:** hub

**Description:** Read metrics dashboard data

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/metrics/{name}` |
| http_route | GET | `/api/v1/admin/metrics-dashboard` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.metrics.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.githubapp.read

**Domain:** hub

**Description:** Read GitHub App configuration and installations

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/github-app` |
| http_route | GET | `/api/v1/github-app/installations` |
| http_route | GET | `/api/v1/github-app/installations/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.github_app.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.githubapp.update

**Domain:** hub

**Description:** Update GitHub App configuration, manage installations, discover and sync

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/github-app` |
| http_route | POST | `/api/v1/github-app/installations` |
| http_route | PUT | `/api/v1/github-app/installations/{id}` |
| http_route | DELETE | `/api/v1/github-app/installations/{id}` |
| http_route | POST | `/api/v1/github-app/installations/discover` |
| http_route | POST | `/api/v1/github-app/sync-permissions` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.github_app.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## quota.read

**Domain:** quota

**Description:** Read limit definitions, entitlements, and usage

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/limits` |
| http_route | GET | `/api/v1/admin/limits/{id}` |
| http_route | GET | `/api/v1/admin/entitlements/{id}` |
| http_route | GET | `/api/v1/admin/usage` |
| http_route | GET | `/api/v1/admin/usage/{limit}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `quota.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## quota.create

**Domain:** quota

**Description:** Create limit definitions and entitlement bindings

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/limits` |
| http_route | POST | `/api/v1/admin/limits/{id}/entitlements` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `quota.create`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## quota.update

**Domain:** quota

**Description:** Update limit definitions and entitlement bindings

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/admin/limits/{id}` |
| http_route | PUT | `/api/v1/admin/entitlements/{id}` |
| http_route | PUT | `/api/v1/runtime-brokers/{id}/settings` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `quota.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## quota.delete

**Domain:** quota

**Description:** Delete limit definitions and entitlement bindings

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/admin/limits/{id}` |
| http_route | DELETE | `/api/v1/admin/entitlements/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `quota.delete`

**Resource Resolver:** hub-scoped

**Effects:** `delete-resource`

### Audit

- **Event Type:** `quota.delete`
- **Context Fields:** actor_id
- **Before Fields:** limit_id, limit_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## hub.policies.removed

**Domain:** hub

**Description:** Removed policy API; every method and sub-path answers 410 Gone and points callers to role bindings

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/policies` |
| http_route | GET | `/api/v1/policies/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Bearer:** `non_user`

**Resource Resolver:** none

**Effects:** `read-one`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **authentication_only:** The handler answers 410 Gone for every caller and reads or changes nothing; no resource permission applies (scope: removed policy API) — waives: `base_permission`, `denial_codes`

---

## skill.read

**Domain:** skill

**Description:** Read skill definitions or list/discover skills

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/skills` |
| http_route | GET | `/api/v1/skills/{id}` |
| http_route | POST | `/api/v1/skills/discover-directory` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `skill.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## skill.create

**Domain:** skill

**Description:** Create a new skill definition

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/skills` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `skill.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## skill.update

**Domain:** skill

**Description:** Update an existing skill definition

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PATCH | `/api/v1/skills/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `skill.update`

**Resource Resolver:** skill-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## skill.delete

**Domain:** skill

**Description:** Delete a skill definition

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/skills/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `skill.delete`

**Resource Resolver:** skill-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `skill.delete`
- **Context Fields:** actor_id, project_id
- **Before Fields:** skill_id, skill_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## skill.register

**Domain:** skill

**Description:** Register skills in a skill registry

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/skill-registries` |
| http_route | GET | `/api/v1/skill-registries` |
| http_route | GET | `/api/v1/skill-registries/{id}` |
| http_route | PUT | `/api/v1/skill-registries/{id}` |
| http_route | DELETE | `/api/v1/skill-registries/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `skill.register`

**Resource Resolver:** hub-scoped

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `skill.register`
- **Context Fields:** actor_id
- **Before Fields:** registry_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## template.read

**Domain:** template

**Description:** Read template definitions or discover available templates

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/templates` |
| http_route | GET | `/api/v1/templates/{id}` |
| http_route | POST | `/api/v1/resources/discover` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `template.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## template.create

**Domain:** template

**Description:** Create a new template or import resources

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/templates` |
| http_route | POST | `/api/v1/resources/import` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `template.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## template.update

**Domain:** template

**Description:** Update an existing template definition

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/templates/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `template.update`

**Resource Resolver:** template-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## template.delete

**Domain:** template

**Description:** Delete a template definition

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/templates/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `template.delete`

**Resource Resolver:** template-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `template.delete`
- **Context Fields:** actor_id, project_id
- **Before Fields:** template_id, template_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## harnessconfig.read

**Domain:** harnessconfig

**Description:** Read harness configurations or list available configs

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/harness-configs` |
| http_route | GET | `/api/v1/harness-configs/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `harness_config.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## harnessconfig.create

**Domain:** harnessconfig

**Description:** Create a new harness configuration

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/harness-configs` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `harness_config.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## harnessconfig.update

**Domain:** harnessconfig

**Description:** Update a harness configuration

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/harness-configs/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Bearer:** `admit` (target `catalog_record`; boundaries `project`, `hub`)

**Base Permission:** `harness_config.update`

**Resource Resolver:** harnessconfig-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## harnessconfig.delete

**Domain:** harnessconfig

**Description:** Delete a harness configuration

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/harness-configs/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `harness_config.delete`

**Resource Resolver:** harnessconfig-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `harnessconfig.delete`
- **Context Fields:** actor_id, project_id
- **Before Fields:** config_id, config_name
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## broker.read

**Domain:** broker

**Description:** Read runtime broker status or list brokers

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/runtime-brokers` |
| http_route | GET | `/api/v1/runtime-brokers/{id}` |
| http_route | GET | `/api/v1/runtime-brokers/{id}/settings` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `broker.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## broker.agent.launchreport

**Domain:** broker

**Description:** Record a broker's launch report for an agent it runs

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/runtime-brokers/{id}/agents/{agentId}/launch` |

**Principals:** `broker`

**Credentials:** `broker_token`

**Bearer:** `non_user`

**Resource Resolver:** broker-self

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** The broker authenticates with its own HMAC credential for its own record; no user permission applies (scope: broker self access) — waives: `base_permission`

---

## broker.messagefailures.report

**Domain:** broker

**Description:** Record buffered message delivery failures reported by a broker

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/runtime-brokers/{id}/message-failures` |

**Principals:** `broker`

**Credentials:** `broker_token`

**Bearer:** `non_user`

**Resource Resolver:** broker-self

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** The broker authenticates with its own HMAC credential for its own record; no user permission applies (scope: broker self access) — waives: `base_permission`

---

## broker.controlchannel.call

**Domain:** broker

**Description:** Carry a call between the hub and a connected broker over the control channel; each call runs under the operation that initiated it

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| broker_call | — | `TunnelRequest:controlchannel` |
| broker_call | — | `OpenStream:controlchannel` |
| broker_call | — | `SendStreamData:controlchannel` |
| broker_call | — | `ResizeStream:controlchannel` |
| broker_call | — | `CloseStream:controlchannel` |
| broker_call | — | `handleResponse:controlchannel` |
| broker_call | — | `handleStreamData:controlchannel` |
| broker_call | — | `handleStreamClose:controlchannel` |
| broker_call | — | `handleEvent:controlchannel` |

**Principals:** `broker`, `system`

**Credentials:** `broker_token`, `system_internal`

**Bearer:** `non_user`

**Resource Resolver:** initiating-operation

**Effects:** `read-one`, `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub:TestBearerDisposition_EveryRoutePatternCovered`

### Exemptions

- **internal_only:** Transport between the hub and an authenticated broker; authorization belongs to the initiating operation (scope: control channel transport) — waives: `base_permission`

---

## gcp.identity.create

**Domain:** gcp.identity

**Description:** Create a GCP service account binding

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/gcp-service-accounts` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `gcp_service_account.create`

**Resource Resolver:** project-from-body

**Effects:** `assign-credential`

### Governance

- **Kind:** issuer_credential
- Service account creation assigns a credential to project scope

### Audit

- **Event Type:** `gcp.identity.create`
- **Context Fields:** actor_id, project_id
- **After Fields:** service_account_email
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## gcp.identity.delete

**Domain:** gcp.identity

**Description:** Delete a GCP service account binding

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/gcp-service-accounts/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `gcp_service_account.delete`

**Resource Resolver:** gcp-service-account-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `gcp.identity.delete`
- **Context Fields:** actor_id
- **Before Fields:** service_account_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## gcp.identity.assign

**Domain:** gcp.identity

**Description:** Assign a GCP service account to an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| internal_dispatch | — | `createAgentInProject:gcp-identity-assign` |
| internal_dispatch | — | `applyAgentUpdate:gcp-identity-assign` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `gcp_service_account.assign`

**Resource Resolver:** gcp-service-account-from-url

**Effects:** `assign-credential`

### Governance

- **Kind:** issuer_credential
- Assigning a service account to an agent grants the agent access to the service account's identity

### Audit

- **Event Type:** `gcp.identity.assign`
- **Context Fields:** actor_id
- **After Fields:** service_account_id, agent_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## gcp.identity.mint

**Domain:** gcp.identity

**Description:** Mint a GCP access token for a service account

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agent/gcp-token` |

**Principals:** `agent`

**Credentials:** `agent_jwt`

**Base Permission:** `gcp_service_account.mint`

**Resource Resolver:** agent-gcp-service-account

**Effects:** `mint-credential`

### Governance

- **Kind:** issuer_credential
- Agent mints GCP tokens scoped to its assigned service account

### Audit

- **Event Type:** `gcp.identity.mint`
- **Context Fields:** agent_id
- **After Fields:** service_account_email, token_scopes
- **Atomic:** No
- **Non-Atomic Justification:** Token minting calls external GCP API; audit recorded before external call

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## secret.read

**Domain:** secret

**Description:** Read project secrets or environment variables containing secrets

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/secrets` |
| http_route | GET | `/api/v1/secrets/{key}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-secret`

### Audit

- **Event Type:** `secret.read`
- **Context Fields:** actor_id, project_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## secret.write

**Domain:** secret

**Description:** Create or update project secrets

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/secrets/{key}` |
| http_route | DELETE | `/api/v1/secrets/{key}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `create-resource`, `update-resource`, `delete-resource`

### Audit

- **Event Type:** `secret.write`
- **Context Fields:** actor_id, project_id
- **Before Fields:** secret_key
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## gcp.identity.read

**Domain:** gcp.identity

**Description:** Read GCP service account details or list accounts

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/gcp-service-accounts` |
| http_route | GET | `/api/v1/gcp-service-accounts/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `gcp_service_account.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## gcp.identity.verify

**Domain:** gcp.identity

**Description:** Verify a GCP service account's IAM configuration

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/gcp-service-accounts/{id}/verify` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `gcp_service_account.verify`

**Resource Resolver:** gcp-identity-from-url

**Effects:** `assign-credential`

### Governance

- **Kind:** issuer_credential
- Verification may re-bind IAM credentials

### Audit

- **Event Type:** `gcp.identity.verify`
- **Context Fields:** actor_id, project_id
- **After Fields:** service_account_id, verification_status
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## env.read

**Domain:** env

**Description:** Read project environment variables

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/env` |
| http_route | GET | `/api/v1/env/{key}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `project.read`

**Resource Resolver:** project-from-url

**Effects:** `read-one`, `list-scoped`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

