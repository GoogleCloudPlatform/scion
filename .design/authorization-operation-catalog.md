# Authorization Operation Catalog

*Generated from Go-native OperationSpec definitions. Do not edit manually.*

**Operations:** 92

## Table of Contents

- [project.membership.add](#projectmembershipadd) — Add a member to a project with a specified role
- [project.membership.update](#projectmembershipupdate) — Change a project member's role
- [project.membership.remove](#projectmembershipremove) — Remove a member from a project
- [project.membership.list](#projectmembershiplist) — List project members and their roles
- [project.membership.transfer](#projectmembershiptransfer) — Atomically transfer project ownership from the actor to another user
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
- [credential.token.revoke](#credentialtokenrevoke) — Revoke a user access token
- [gcp.identity.create](#gcpidentitycreate) — Create a GCP service account binding
- [gcp.identity.delete](#gcpidentitydelete) — Delete a GCP service account binding
- [gcp.identity.assign](#gcpidentityassign) — Assign a GCP service account to an agent
- [gcp.identity.mint](#gcpidentitymint) — Mint a GCP access token for a service account
- [agent.lifecycle.create](#agentlifecyclecreate) — Create an agent in a project
- [agent.lifecycle.delete](#agentlifecycledelete) — Delete an agent
- [project.lifecycle.create](#projectlifecyclecreate) — Create a new project
- [project.lifecycle.delete](#projectlifecycledelete) — Delete a project with cascading security state cleanup and atomic audit
- [agent.message.send](#agentmessagesend) — Send a message to an agent
- [user.admin.suspend](#useradminsuspend) — Suspend a user account
- [secret.read](#secretread) — Read project secrets or environment variables containing secrets
- [secret.write](#secretwrite) — Create or update project secrets
- [user.admin.invite](#useradmininvite) — Invite a user to the platform
- [user.admin.promote](#useradminpromote) — Promote or demote a user's administrative level
- [hub.authreset](#hubauthreset) — Reset all agent authentication credentials (emergency action)
- [hub.config.read](#hubconfigread) — Read server configuration and schema
- [hub.config.update](#hubconfigupdate) — Update server configuration sections
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
- [agent.read](#agentread) — Read a single agent's metadata by ID
- [agent.list](#agentlist) — List agents within the caller's authorized project scope
- [agent.update](#agentupdate) — Update agent configuration or metadata
- [agent.attach](#agentattach) — Attach to an agent session via WebSocket
- [agent.portaccess](#agentportaccess) — Access forwarded ports on an agent
- [agent.stopall](#agentstopall) — Stop all running agents in a project
- [agent.setmessagemode](#agentsetmessagemode) — Change an agent's message mode
- [project.read](#projectread) — Read a single project's metadata by ID or slug
- [project.list](#projectlist) — List projects within the caller's authorized scope
- [project.update](#projectupdate) — Update project settings and metadata
- [project.register](#projectregister) — Register a project or grove from an external source
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
- [group.read](#groupread) — Read group details or list groups
- [group.create](#groupcreate) — Create a new group
- [group.update](#groupupdate) — Update group metadata
- [user.read](#userread) — Read user profile or list users
- [user.update](#userupdate) — Update user profile or settings
- [broker.read](#brokerread) — Read runtime broker status or list brokers
- [gcp.identity.read](#gcpidentityread) — Read GCP service account details or list accounts
- [gcp.identity.verify](#gcpidentityverify) — Verify a GCP service account's IAM configuration
- [role.read](#roleread) — Read role definitions and permission registry
- [role.binding.read](#rolebindingread) — Read role binding assignments
- [access.constraint.read](#accessconstraintread) — Read access constraint definitions
- [quota.read](#quotaread) — Read limit definitions, entitlements, and usage
- [quota.create](#quotacreate) — Create limit definitions and entitlement bindings
- [quota.update](#quotaupdate) — Update limit definitions and entitlement bindings
- [quota.delete](#quotadelete) — Delete limit definitions and entitlement bindings
- [schedule.event.read](#scheduleeventread) — Read scheduled events or list events in a project
- [schedule.event.create](#scheduleeventcreate) — Create a scheduled event or recurring schedule
- [schedule.event.update](#scheduleeventupdate) — Update a recurring schedule
- [schedule.event.delete](#scheduleeventdelete) — Cancel a scheduled event or delete a recurring schedule
- [chat.access](#chataccess) — Access chat threads, spaces, topics, and messages within a project
- [env.read](#envread) — Read project environment variables

---

## project.membership.add

**Domain:** project.membership

**Description:** Add a member to a project with a specified role

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/{id}/members` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

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

**Credentials:** `session_jwt`, `scoped_uat`

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

**Credentials:** `session_jwt`, `scoped_uat`

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

**Credentials:** `session_jwt`, `scoped_uat`

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

## role.definition.create

**Domain:** role

**Description:** Create a custom role definition

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/admin/roles` |

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
| http_route | DELETE | `/api/v1/groups/{id}/members/{memberId}` |

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

### Exemptions

- **authentication_only:** Token creation is authenticated-only (user manages own tokens); no per-resource permission required beyond session validity (scope: self-token management only) — waives: `base_permission`

---

## credential.token.revoke

**Domain:** credential

**Description:** Revoke a user access token

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/auth/tokens/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `user.read`

**Resource Resolver:** self-principal

**Effects:** `revoke-authority`

### Governance

- **Kind:** issuer_credential
- User may revoke own tokens; admin may revoke via hub-admin path

### Audit

- **Event Type:** `credential.token.revoke`
- **Context Fields:** actor_id
- **Before Fields:** token_id
- **Atomic:** Yes

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

### Exemptions

- **authentication_only:** Token revocation is authenticated-only (user manages own tokens) (scope: self-token management only) — waives: `base_permission`

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
| http_route | POST | `/api/v1/gcp-service-accounts/{id}/assign` |

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

## agent.lifecycle.create

**Domain:** agent

**Description:** Create an agent in a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/agents` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

**Base Permission:** `agent.create`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

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

## agent.message.send

**Domain:** agent.message

**Description:** Send a message to an agent

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/chat/threads/{id}/messages` |
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

## user.admin.suspend

**Domain:** user.admin

**Description:** Suspend a user account

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/users/{id}/suspend` |

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
| http_route | POST | `/api/v1/secrets` |
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

**Description:** Promote or demote a user's administrative level

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/users/{id}/promote` |

**Principals:** `user`

**Credentials:** `session_jwt`

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
| http_route | PUT | `/api/v1/admin/server-config/sections/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.config.update`

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
| http_route | POST | `/api/v1/admin/maintenance/operations` |
| http_route | GET | `/api/v1/admin/maintenance/operations` |
| http_route | GET | `/api/v1/admin/maintenance/operations/{id}` |
| http_route | POST | `/api/v1/admin/maintenance/restart` |
| http_route | POST | `/api/v1/admin/maintenance/check-updates` |
| http_route | POST | `/api/v1/admin/maintenance/migrations/{id}` |

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
| http_route | PUT | `/api/v1/admin/allow-list` |
| http_route | PUT | `/api/v1/admin/allow-list/{email}` |
| http_route | DELETE | `/api/v1/admin/allow-list/{email}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `hub.allow_list.update`

**Resource Resolver:** hub-scoped

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

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
| http_route | GET | `/api/v1/admin/diagnostics/logs/stream` |
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
| http_route | POST | `/api/v1/admin/validate-resources` |

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

## agent.read

**Domain:** agent

**Description:** Read a single agent's metadata by ID

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/agents/{id}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

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
| http_route | PUT | `/api/v1/agents/{id}` |

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
| websocket | GET | `/api/v1/agents/{id}/attach` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

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

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

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
| http_route | PUT | `/api/v1/agents/{id}/message-mode` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `agent.set_message_mode`

**Resource Resolver:** agent-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.read

**Domain:** project

**Description:** Read a single project's metadata by ID or slug

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/projects/{id}` |
| http_route | GET | `/api/v1/groves/{id}` |

**Principals:** `user`, `agent`

**Credentials:** `session_jwt`, `scoped_uat`, `agent_jwt`

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
| http_route | GET | `/api/v1/groves` |

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
| http_route | PUT | `/api/v1/projects/{id}` |
| http_route | PUT | `/api/v1/groves/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `project.update`

**Resource Resolver:** project-from-url

**Effects:** `update-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## project.register

**Domain:** project

**Description:** Register a project or grove from an external source

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | POST | `/api/v1/projects/register` |
| http_route | POST | `/api/v1/groves/register` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `project.register`

**Resource Resolver:** project-from-body

**Effects:** `create-resource`

**Denial Codes:** `forbidden`

### Tests

- `pkg/hub/authzop:TestCatalogValidation`

---

## skill.read

**Domain:** skill

**Description:** Read skill definitions or list/discover skills

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/skills` |
| http_route | GET | `/api/v1/skills/{id}` |
| http_route | GET | `/api/v1/skills/discover-directory` |

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
| http_route | PUT | `/api/v1/skills/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

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
| http_route | GET | `/api/v1/resources/discover` |

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
| http_route | PUT | `/api/v1/groups/{id}` |

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

**Description:** Update user profile or settings

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | PUT | `/api/v1/users/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `user.update`

**Resource Resolver:** user-from-url

**Effects:** `update-resource`

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

**Principals:** `user`

**Credentials:** `session_jwt`, `scoped_uat`

**Base Permission:** `broker.read`

**Resource Resolver:** hub-scoped

**Effects:** `read-one`, `list-scoped`

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

## role.read

**Domain:** role

**Description:** Read role definitions and permission registry

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/admin/roles` |
| http_route | GET | `/api/v1/admin/roles/{id}` |
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
| http_route | GET | `/api/v1/admin/role-bindings/{id}` |

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
| http_route | POST | `/api/v1/admin/entitlements/{id}` |

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
| http_route | PUT | `/api/v1/projects/{projectId}/schedules/{id}` |

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

## chat.access

**Domain:** chat

**Description:** Access chat threads, spaces, topics, and messages within a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | GET | `/api/v1/chat/prefs` |
| http_route | PUT | `/api/v1/chat/prefs` |
| http_route | GET | `/api/v1/chat/threads` |
| http_route | GET | `/api/v1/chat/threads/{id}` |
| http_route | GET | `/api/v1/chat/spaces` |
| http_route | GET | `/api/v1/chat/spaces/{id}` |
| http_route | GET | `/api/v1/chat/conversations/{id}` |
| http_route | GET | `/api/v1/chat/topics/{id}` |
| http_route | GET | `/api/v1/chat/dms` |
| http_route | GET | `/api/v1/chat/search` |
| http_route | GET | `/api/v1/chat/attachments` |
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

