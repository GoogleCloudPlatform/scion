## Design Note: Project Access via Membership (replacing dead visibility field)

**From:** proj-visibility-dev
**Context:** The project visibility field (private/team/public) has been eradicated -- it was dead code re-introduced by a stale-branch merge. This note proposes the net-new mechanism to capture the same intent using the current role/group/policy model.

### How it works today

1. hub-member-read-all policy grants read+list on ResourceType:"*" to the hub-members group -- every authenticated user can see every project.
2. Per-project project:<slug>:member-create-agents policy grants create, stop_all, message on agents, bound to the project:<slug>:members group.
3. isProjectOwnerOrAdmin bypass (via RoleBindings) gates admin/owner actions.

### Proposed changes -- using existing Policy system (group-aware, fully wired)

Step 1: Narrow the global read-all grant. Replace the ResourceType:"*" wildcard in hub-member-read-all with explicit per-type allows for directory/catalog types only:
- KEEP globally readable: user, group, template, harness_config, broker, runtime_broker, gcp_service_account, policy, skill, quota, role, role_binding, hub
- GATE (remove from global read): project, agent

After this, a project is only visible to users who have a matching policy grant.

Step 2: Add project-scoped read policy. Modify createProjectMembersGroupAndPolicy to also seed a policy granting read, list on project and agent resource types, bound to the project:<slug>:members group. Members can see the project and its agents.

Step 3: Three access levels emerge from membership:
- Private (default): Only owner + explicitly added members. Members group has only the creator.
- Team: Collaborators added to the members group (users and/or nested groups).
- Everyone: Add hub-members group to the project's members group. The project-scoped read policy then applies to all hub users transitively.

No creation-time selector needed. The Members panel IS the access control surface.

Step 4: Enforcement gaps to close:
- getProject (single GET) -- add CheckAccess(ActionRead) gate
- listProjects/listAgents -- fail closed on nil identity (empty/401)

Step 5: Members-card hint. Add hint text to the group member editor when it is a project members group: "To make this project visible to all hub users, add the hub-members group."

### Role tiers (subtractive approach, no policy-engine changes)
- member = read-only (from the project-scoped read policy bound to members group)
- admin/owner = create/manage agents (from isProjectOwnerOrAdmin RoleBinding bypass -- already works)
- Existing members bumped to admin in one-time backfill to preserve current create-agent ability.

### Known gaps (not blockers, flagging for awareness)
1. RoleBinding sync gap: Users added via groups API dont get a corresponding RoleBinding until hub restart.
2. Governance role not carried through group expansion: GetEffectiveGroups drops role. A user who is admin only via a nested group gets read but not create.
3. No policy management UI: Per-project policy changes happen programmatically.

### Implementation approach
This is a direct extension of existing patterns -- the per-project members group exists, hub-members auto-enrollment works, and the Policy systems group-aware enforcement is production-proven. Low-risk, all existing infrastructure being wired together. Will proceed unless you flag concerns.
