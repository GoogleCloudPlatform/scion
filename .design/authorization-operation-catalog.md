# Authorization Operation Catalog

*Generated from Go-native OperationSpec definitions. Do not edit manually.*

**Operations:** 27

## Table of Contents

- [project.membership.add](#projectmembershipadd) — Add a member to a project with a specified role
- [project.membership.update](#projectmembershipupdate) — Change a project member's role
- [project.membership.remove](#projectmembershipremove) — Remove a member from a project
- [project.membership.list](#projectmembershiplist) — List project members and their roles
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
- [project.lifecycle.delete](#projectlifecycledelete) — Delete a project
- [agent.message.send](#agentmessagesend) — Send a message to an agent
- [user.admin.suspend](#useradminsuspend) — Suspend a user account

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

**Base Permission:** `project.manage`  
**Resource Resolver:** project-from-url

**Effects:** `grant-authority`

### Delegation

- **Kind:** `non_amplification`
- Actor must hold all permissions in the target role (CanDelegate non-amplification)

### Governance

- **Kind:** peer_superior
- C0 containment: only project-owner may add members. CT1 D5 approved governance matrix applies in RS1.

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

**Base Permission:** `project.manage`  
**Resource Resolver:** project-from-url

**Effects:** `change-authority`

### Delegation

- **Kind:** `conditional_on_increase`
- CanDelegate checked when new role has more permissions than old role

**Authority Evaluation:** `before_and_after`

### Governance

- **Kind:** peer_superior
- C0 containment: only project-owner may change roles. CT1 D5 governance matrix applies in RS1.

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

**Base Permission:** `project.manage`  
**Resource Resolver:** project-from-url

**Effects:** `revoke-authority`

**Authority Evaluation:** `proposed_post_state`

### Governance

- **Kind:** peer_superior
- C0 containment: only project-owner may remove members. CT1 D1 allows self-removal when another active direct owner remains.

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

**Credentials:** `session_jwt`

**Base Permission:** `project.read`  
**Resource Resolver:** project-from-url

**Effects:** `list-scoped`

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
**Description:** Delete a project

### Entry Points

| Kind | Method | Pattern |
|------|--------|---------|
| http_route | DELETE | `/api/v1/projects/{id}` |

**Principals:** `user`

**Credentials:** `session_jwt`

**Base Permission:** `project.delete`  
**Resource Resolver:** project-from-url

**Effects:** `delete-resource`

### Audit

- **Event Type:** `project.lifecycle.delete`
- **Context Fields:** actor_id
- **Before Fields:** project_id, project_name
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

