# Policy Inventory — BI1 Behavior Inventory

**Status:** Complete  
**Date:** 2026-08-30  
**Companion:** `.design/` design.md §Migration Phase 0

This document classifies every `Policy` and `PolicyBinding` creation site in the
product codebase. Each site receives a disposition under the target authorization
model defined in the design doc.

## Disposition Categories

| Code | Meaning |
|------|---------|
| **RoleBinding** | Becomes an ordinary positive RoleBinding grant |
| **Relationship grant** | Becomes a named built-in relationship grant (e.g., progeny/lineage) |
| **AccessConstraint** | Becomes a maximum-permissions boundary |
| **Intrinsic restriction** | Becomes a credential/delegation/status restriction (not a stored constraint) |
| **Intentional removal** | Behavior is intentionally not preserved in the new model |

## Seeded Policies (pkg/hub/seed.go)

### 1. Hub-member per-type read policies

| Field | Value |
|-------|-------|
| **Created by** | `seedDefaultPoliciesAndGroups` → `seedPolicy` |
| **File:Line** | `pkg/hub/seed.go:63-74` (loop), `seedPolicy` at line 519 |
| **Policy name pattern** | `hub-member-read-{user,group,template,harness_config,broker,runtime_broker,gcp_service_account,policy,skill,quota,role,role_binding,hub}` (13 policies) |
| **Scope** | Hub |
| **ResourceType** | One per policy (see list above) |
| **Actions** | `read`, `list` |
| **Effect** | `allow` |
| **Bound to** | `hub-members` group |
| **Disposition** | **RoleBinding** — These become the `hub-member` RoleDefinition's curated permission set. The new `hub-member` role must include read+list for exactly these resource types. Project and agent reads are deliberately excluded (handled by per-project policies). |
| **Post-cutover equivalent** | System-scoped RoleBinding of `hub-member` role to the `hub-members` group. The role definition contains `user.read`, `user.list`, `group.read`, `group.list`, `template.read`, `template.list`, `harness_config.read`, `harness_config.list`, `broker.read`, `broker.list`, `runtime_broker.read`, `runtime_broker.list`, `gcp_service_account.read`, `gcp_service_account.list`, `policy.read`, `policy.list`, `skill.read`, `skill.list`, `quota.read`, `quota.list`, `role.read`, `role.list`, `role_binding.read`, `role_binding.list`, `hub.read`, `hub.list` (or equivalent). |

### 2. Hub-member-create-projects

| Field | Value |
|-------|-------|
| **Created by** | `seedDefaultPoliciesAndGroups` → `seedPolicy` |
| **File:Line** | `pkg/hub/seed.go:77-86` |
| **Policy name pattern** | `hub-member-create-projects` |
| **Scope** | Hub |
| **ResourceType** | `project` |
| **Actions** | `create` |
| **Effect** | `allow` |
| **Bound to** | `hub-members` group |
| **Disposition** | **RoleBinding** — Becomes part of the curated `hub-member` role's permission set. |
| **Post-cutover equivalent** | `project.create` permission in the `hub-member` RoleDefinition. |

### 3. Hub-member-read-all (legacy, deleted by narrowing)

| Field | Value |
|-------|-------|
| **Created by** | Historical seed (pre-narrowing) |
| **File:Line** | `pkg/hub/seed.go:471-494` (`narrowHubMemberReadAll` deletes it) |
| **Policy name pattern** | `hub-member-read-all` |
| **Scope** | Hub |
| **ResourceType** | `*` (wildcard) |
| **Actions** | `read`, `list` |
| **Effect** | `allow` |
| **Bound to** | `hub-members` group |
| **Disposition** | **Intentional removal** — This wildcard policy is actively deleted on startup by `narrowHubMemberReadAll`, which also writes a hub setting tombstone to prevent re-creation. Re-creating this policy via the API does not cause it to be deleted again; the tombstone ensures the narrow function only runs once. It was the source of cross-project visibility: it granted read+list on ALL resource types including project and agent. The narrowed per-type policies (item 1) replace it with project/agent excluded. |
| **Post-cutover equivalent** | None. Already removed. |

### 4. Project service-account assign policy

| Field | Value |
|-------|-------|
| **Created by** | `ensureProjectAssignPolicy` |
| **File:Line** | `pkg/hub/seed.go:225-272` |
| **Policy name pattern** | `project:<slug>:member-assign-service-accounts` |
| **Scope** | Project |
| **ResourceType** | `gcp_service_account` |
| **Actions** | `assign` |
| **Effect** | `allow` |
| **Bound to** | Project members group (`project:<slug>:members`) |
| **Disposition** | **RoleBinding** — Becomes `gcp_service_account.assign` in the `project-member` RoleDefinition, granted via a project-scoped RoleBinding to the members group. |
| **Post-cutover equivalent** | `gcp_service_account.assign` permission added to `project-member` role. Project-scoped RoleBinding of `project-member` to the project members group. |

### 5. Project member read policies (project + agent)

| Field | Value |
|-------|-------|
| **Created by** | `ensureProjectMemberReadPolicies` |
| **File:Line** | `pkg/hub/seed.go:413-459` |
| **Policy name pattern** | `project:<slug>:member-read-project`, `project:<slug>:member-read-agent` |
| **Scope** | Project |
| **ResourceType** | `project` and `agent` (one policy per type) |
| **Actions** | `read`, `list` |
| **Effect** | `allow` |
| **Bound to** | Project members group |
| **Disposition** | **RoleBinding** — These become part of the `project-member` RoleDefinition's permission set. The RoleDefinition already contains `project.read`, `project.list`, `agent.read`, `agent.list`. |
| **Post-cutover equivalent** | Subsumed by the project-scoped RoleBinding of `project-member` role which already includes these permissions. |

## Project Membership Policies (pkg/hub/handlers_projects_core.go)

### 6. Project member create-agents policy

| Field | Value |
|-------|-------|
| **Created by** | `createProjectMembersGroupAndPolicy` |
| **File:Line** | `pkg/hub/handlers_projects_core.go:819-890` |
| **Policy name pattern** | `project:<slug>:member-create-agents` |
| **Scope** | Project |
| **ResourceType** | `agent` |
| **Actions** | `create`, `stop_all`, `message` |
| **Effect** | `allow` |
| **Bound to** | Project members group |
| **Disposition** | **RoleBinding** — These actions are already in the `project-member` RoleDefinition (`agent.create`, `agent.stop_all`, `agent.message`). |
| **Post-cutover equivalent** | Subsumed by project-scoped RoleBinding of `project-member` role. |

## Progeny/Delegation Policies (pkg/hub/handlers_env_secrets.go)

### 7. Progeny secret access policy

| Field | Value |
|-------|-------|
| **Created by** | `ensureProgenyPolicy` |
| **File:Line** | `pkg/hub/handlers_env_secrets.go:608-658` |
| **Policy name pattern** | `progeny-secret-access:<secretID>` |
| **Scope** | Resource (scoped to specific secret) |
| **ResourceType** | `secret` |
| **ResourceID** | `<secretID>` |
| **Actions** | `read` |
| **Effect** | `allow` |
| **Conditions** | `DelegatedFrom: {PrincipalType: "user", PrincipalID: <creatorUserID>}` |
| **Bound to** | No binding — uses DelegatedFrom condition for agent ancestry matching |
| **Labels** | `scion.dev/managed-by: progeny-secret-access`, `scion.dev/secret-key`, `scion.dev/secret-id`, `scion.dev/secret-scope` |
| **Disposition** | **Relationship grant** — Becomes a named built-in lineage/progeny grant. The agent's delegation chain (ancestry) determines access, not a stored RoleBinding or policy binding. |
| **Post-cutover equivalent** | A purpose-built progeny/lineage resolver: if agent A's ancestry includes the secret's creator, and the secret was created for progeny access, A may read it. The resolver emits the same decision provenance as a RoleBinding. |

### 8. Progeny environment variable access policy

| Field | Value |
|-------|-------|
| **Created by** | `ensureEnvVarProgenyPolicy` |
| **File:Line** | `pkg/hub/handlers_env_secrets.go:686-733` |
| **Policy name pattern** | `progeny-envvar-access:<envVarID>` |
| **Scope** | Resource (scoped to specific env var) |
| **ResourceType** | `envvar` |
| **ResourceID** | `<envVarID>` |
| **Actions** | `read` |
| **Effect** | `allow` |
| **Conditions** | `DelegatedFrom: {PrincipalType: "user", PrincipalID: <creatorUserID>}` |
| **Bound to** | No binding — uses DelegatedFrom condition |
| **Labels** | `scion.dev/managed-by: progeny-envvar-access`, `scion.dev/envvar-key`, `scion.dev/envvar-id`, `scion.dev/envvar-scope` |
| **Disposition** | **Relationship grant** — Same as secrets (item 7). |
| **Post-cutover equivalent** | Same progeny/lineage resolver as secrets. |

## Skill Injection Policies (pkg/hub/handlers_skills_injection.go)

### 9. Progeny skill injection access policy

| Field | Value |
|-------|-------|
| **Created by** | `ensureSkillProgenyPolicy` |
| **File:Line** | `pkg/hub/handlers_skills_injection.go:831-877` |
| **Policy name pattern** | `progeny-skill-access:<skillInjectionID>` |
| **Scope** | Resource (scoped to specific skill injection) |
| **ResourceType** | `skill_injection` |
| **ResourceID** | `<skillInjectionID>` |
| **Actions** | `read` |
| **Effect** | `allow` |
| **Conditions** | `DelegatedFrom: {PrincipalType: "user", PrincipalID: <creatorUserID>}` |
| **Bound to** | No binding — uses DelegatedFrom condition |
| **Labels** | `scion.dev/managed-by: progeny-skill-access`, `scion.dev/skill-injection-id`, `scion.dev/skill-injection-uri` |
| **Disposition** | **Relationship grant** — Same as secrets and env vars. |
| **Post-cutover equivalent** | Same progeny/lineage resolver. |

## User-Created Policies (pkg/hub/handlers_policies.go)

### 10. REST API CreatePolicy handler

| Field | Value |
|-------|-------|
| **Created by** | `createPolicy` HTTP handler |
| **File:Line** | `pkg/hub/handlers_policies.go:243-265` |
| **Policy name pattern** | User-provided (arbitrary) |
| **Scope** | User-provided (`hub`, `project`, or `resource`) |
| **ResourceType** | User-provided |
| **Actions** | User-provided |
| **Effect** | User-provided (`allow` or `deny`) |
| **Conditions** | User-provided (except `SourceIPs` rejected) |
| **PolicyKind** | Always `explicit` |
| **Disposition** | **Intentional removal** — The Policy API is removed at cutover. User-created allow policies should be converted to RoleBindings (if they are simple grants) or AccessConstraints (if they are deny/boundary policies). Any user-created policies with DelegatedFrom conditions become relationship grants. The design doc explicitly states: "The old Policy API/evaluator/schema are removed at cutover." |
| **Post-cutover equivalent** | Operators create RoleBindings for grants and AccessConstraints for restrictions. The Policy creation API is deleted. |

### 11. REST API AddPolicyBinding handler

| Field | Value |
|-------|-------|
| **Created by** | `addPolicyBinding` HTTP handler |
| **File:Line** | `pkg/hub/handlers_policies.go:556-618` |
| **Policy name pattern** | N/A (binds to existing policy) |
| **Bound to** | User-provided (user, group, or agent) |
| **Disposition** | **Intentional removal** — Removed with the Policy API. |
| **Post-cutover equivalent** | RoleBinding creation API. |

## Code Baselines (pkg/hub/authz.go) — Not Policy-Based

These are not Policy creation sites, but they are authorization grants that must
be inventoried because they bypass the policy engine.

### 12. Admin bypass

| Field | Value |
|-------|-------|
| **File:Line** | `pkg/hub/authz.go:490-495` |
| **Who** | Users with `Role == "admin"` who are not scoped (UAT) or federated |
| **Grants** | Everything — total bypass |
| **Disposition** | **RoleBinding** — Becomes the `super-admin` RoleDefinition with all permissions. The bypass is replaced by a standard role binding evaluation; `super-admin` still passes through constraints per the design doc ("Owner and administrator roles pass through steps 3 and 4"). |
| **Post-cutover equivalent** | System-scoped RoleBinding of `super-admin` role. No early-return bypass in the evaluator. |
| **Intentional change** | The design doc says super-admin should NOT bypass constraints (§5: "Owner/admin grants pass through steps 3 and 4 like any other grant"). The current unconditional bypass is a historical accident that the new model corrects. |

### 13. Resource owner bypass

| Field | Value |
|-------|-------|
| **File:Line** | `pkg/hub/authz.go:510-519` |
| **Who** | The user whose ID matches `resource.OwnerID` |
| **Grants** | All actions on owned resources (except ActionAssign on hub-scoped gcp_service_account) |
| **Disposition** | **RoleBinding** — Where practical, ownership becomes an auditable owner role binding created transactionally with the resource. For resources without a clear owner role (like individual secrets), this may remain a named built-in relationship grant. |
| **Post-cutover equivalent** | Transactional owner RoleBinding or named built-in ownership grant with decision provenance. |

### 14. Ancestry/lineage bypass

| Field | Value |
|-------|-------|
| **File:Line** | `pkg/hub/authz.go:521-527` (`canAccessAsAncestor`) |
| **Who** | Any user/agent whose ID appears in `resource.Ancestry` |
| **Grants** | All actions on the resource |
| **Disposition** | **Relationship grant** — Becomes a named built-in ancestry relationship grant with full decision provenance. |
| **Post-cutover equivalent** | Named relationship grant emitting the same provenance as a RoleBinding. |

### 15. Project owner/admin bypass

| Field | Value |
|-------|-------|
| **File:Line** | `pkg/hub/authz.go:533-539` (`isProjectOwnerOrAdmin`) |
| **Who** | Users with `owner` or `admin` role in the project's members group, or users in groups with owner/admin role binding on the project |
| **Grants** | All actions on resources scoped to the project |
| **Disposition** | **RoleBinding** — Becomes project-scoped `project-owner` and `project-admin` RoleBindings with full project permission sets. No early-return bypass. |
| **Post-cutover equivalent** | Project-scoped RoleBinding. The `project-owner` and `project-admin` RoleDefinitions contain all relevant project-scoped permissions. |
| **Intentional change** | No longer an unconditional bypass — the new evaluator unions grants and applies constraints. Owner/admin grants pass through the restriction pipeline. |

### 16. Hub-scoped SA assign baseline

| Field | Value |
|-------|-------|
| **File:Line** | `pkg/hub/authz.go:565-574` |
| **Who** | Current hub members assigning hub-scoped GCP service accounts |
| **Grants** | `assign` on parentless `gcp_service_account` |
| **Disposition** | **Intrinsic restriction** / **RoleBinding** — The hub membership check becomes a standard role-based evaluation. If the user has `gcp_service_account.assign` through their role binding at system scope, they may assign. |
| **Post-cutover equivalent** | `gcp_service_account.assign` in a suitable system-scoped role, evaluated through the standard pipeline. |

### 17. Role binding permission check (hub-member/hub-admin/hub-viewer)

| Field | Value |
|-------|-------|
| **File:Line** | `pkg/hub/authz.go:576-598` |
| **Who** | Any user with a system-scoped role binding (hub-member, hub-admin, hub-viewer, super-admin) |
| **Grants** | All permissions in the role's permission set |
| **Disposition** | **RoleBinding** — This IS the target model. However, the current implementation has a critical flaw: it runs BEFORE policy evaluation, making role-granted permissions irrevocable by deny policies. The new evaluator unions all grants first, then applies constraints. |
| **Post-cutover equivalent** | Standard role binding evaluation in the unified evaluator. |
| **Intentional change** | Role binding grants are no longer early-returns that bypass policy. They are unioned with all other grants, then constraints apply. |
| **Cross-project visibility risk** | The current `hub-member` role includes ALL read+list permissions (`permissionIDsByActions("read", "list")`), including `project.list`, `project.read`, `agent.list`, `agent.read`. When list handlers check `hasAdminView` using these permissions, hub-members get admin-view access to all projects/agents. This is the vulnerability identified in the design doc (seed.go lines 628-650, authz.go lines 576-598). |

### 18. Agent project read baseline

| Field | Value |
|-------|-------|
| **File:Line** | `pkg/hub/authz.go:682-689` |
| **Who** | Agents reading resources in their own project |
| **Grants** | `read`, `list` on resources in the agent's project |
| **Disposition** | **RoleBinding** — Becomes the `project-agent` role with read+list permissions, granted via a project-scoped RoleBinding to the project's agents group. |
| **Post-cutover equivalent** | Project-scoped RoleBinding of an agent role to `project:<slug>:agents` group. |

### 19. Agent project SA assign baseline

| Field | Value |
|-------|-------|
| **File:Line** | `pkg/hub/authz.go:759-767` |
| **Who** | Agents assigning GCP service accounts in their own project |
| **Grants** | `assign` on project-scoped `gcp_service_account` |
| **Disposition** | **RoleBinding** — Becomes `gcp_service_account.assign` in the agent role, scoped to the agent's project. |
| **Post-cutover equivalent** | Included in the project-scoped agent role. |

### 20. Delegation fallback (checkDelegation)

| Field | Value |
|-------|-------|
| **File:Line** | `pkg/hub/authz.go:781-847` |
| **Who** | Agents matching DelegatedFrom/DelegatedFromGroup conditions in policies |
| **Grants** | Whatever the matched policy allows (typically `read` on specific resources) |
| **Disposition** | **Relationship grant** — The delegation/ancestry resolution becomes a named built-in relationship grant resolver. It checks the agent's creation chain against the resource's creator, without storing arbitrary JSON conditions. |
| **Post-cutover equivalent** | Purpose-built lineage grant resolver (same as items 7-9). |

## UAT Constraints (pkg/hub/authz.go)

### 21. UAT project and scope constraints

| Field | Value |
|-------|-------|
| **File:Line** | `pkg/hub/authz.go:481-485`, `enforceUATConstraints` at line 1004 |
| **Who** | Users authenticating with User Access Tokens (scoped credentials) |
| **Grants** | None — this only restricts |
| **Disposition** | **Intrinsic restriction** — UAT scopes are credential caveats that intersect with granted authority. They are not operator-created constraints and do not become AccessConstraint rows. |
| **Post-cutover equivalent** | Credential caveats applied after the union of grants, per the design doc §6. |

## Summary Table

| # | Policy Name Pattern | Created By | File:Line | Disposition | Post-Cutover Equivalent |
|---|---|---|---|---|---|
| 1 | `hub-member-read-{type}` (×13) | `seedDefaultPoliciesAndGroups` | seed.go:63-74 | RoleBinding | `hub-member` role permissions |
| 2 | `hub-member-create-projects` | `seedDefaultPoliciesAndGroups` | seed.go:77-86 | RoleBinding | `project.create` in `hub-member` role |
| 3 | `hub-member-read-all` (legacy) | Historical seed | seed.go:471-494 (deleted) | Intentional removal | Already removed |
| 4 | `project:<slug>:member-assign-service-accounts` | `ensureProjectAssignPolicy` | seed.go:225-272 | RoleBinding | `project-member` role + project RoleBinding |
| 5 | `project:<slug>:member-read-{project,agent}` | `ensureProjectMemberReadPolicies` | seed.go:413-459 | RoleBinding | `project-member` role permissions |
| 6 | `project:<slug>:member-create-agents` | `createProjectMembersGroupAndPolicy` | handlers_projects_core.go:819-890 | RoleBinding | `project-member` role permissions |
| 7 | `progeny-secret-access:<id>` | `ensureProgenyPolicy` | handlers_env_secrets.go:608-658 | Relationship grant | Lineage resolver |
| 8 | `progeny-envvar-access:<id>` | `ensureEnvVarProgenyPolicy` | handlers_env_secrets.go:686-733 | Relationship grant | Lineage resolver |
| 9 | `progeny-skill-access:<id>` | `ensureSkillProgenyPolicy` | handlers_skills_injection.go:831-877 | Relationship grant | Lineage resolver |
| 10 | (user-provided) | `createPolicy` handler | handlers_policies.go:243-265 | Intentional removal | RoleBinding or AccessConstraint |
| 11 | N/A (binding) | `addPolicyBinding` handler | handlers_policies.go:556-618 | Intentional removal | RoleBinding creation API |
| 12 | (admin bypass) | Code baseline | authz.go:490-495 | RoleBinding | `super-admin` RoleBinding |
| 13 | (owner bypass) | Code baseline | authz.go:510-519 | RoleBinding | Owner RoleBinding or relationship grant |
| 14 | (ancestry bypass) | Code baseline | authz.go:521-527 | Relationship grant | Named ancestry grant |
| 15 | (project owner/admin bypass) | Code baseline | authz.go:533-539 | RoleBinding | Project-scoped owner/admin RoleBinding |
| 16 | (hub SA assign baseline) | Code baseline | authz.go:565-574 | RoleBinding | System-scoped role with assign permission |
| 17 | (role binding check) | Code baseline | authz.go:576-598 | RoleBinding | Standard evaluator path |
| 18 | (agent project read baseline) | Code baseline | authz.go:682-689 | RoleBinding | Project-scoped agent RoleBinding |
| 19 | (agent SA assign baseline) | Code baseline | authz.go:759-767 | RoleBinding | Project-scoped agent RoleBinding |
| 20 | (delegation fallback) | Code baseline | authz.go:781-847 | Relationship grant | Lineage resolver |
| 21 | (UAT constraints) | Code baseline | authz.go:481-485 | Intrinsic restriction | Credential caveats |

## Cross-Project Visibility Risk

The design doc identifies a critical issue at seed.go lines 628-650 and authz.go
lines 576-598. The `hub-member` RoleDefinition is constructed from
`permissionIDsByActions("read", "list")`, which includes `project.list`,
`project.read`, `agent.list`, and `agent.read`. Every user with `User.Role=member`
gets a system-scoped `hub-member` role binding via backfill (seed.go:907-947).

The role binding check at authz.go:576-598 evaluates system-scoped role bindings
**before** policy evaluation. List handlers (`handlers_projects_core.go:212-233`,
`handlers_agents_core.go:310-337`) check `hasAdminView` using these permissions.
Since `hub-member` holds `project.list` and `agent.list`, an ordinary hub member
gains admin-view access — they see ALL projects and ALL agents, not just those
they are members of.

This is the vulnerability that must be corrected before the Policy cutover.
PG1 must curate the `hub-member` permission set to exclude `project.list`,
`project.read`, `agent.list`, and `agent.read` from the system-scoped role.
LS1 must replace hasAdminView with scope-aware list authorization.
