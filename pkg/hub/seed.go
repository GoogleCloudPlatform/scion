// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// seedDefaultPoliciesAndGroups creates the default hub-members group and
// associated policies if they don't already exist. This is called once
// during Hub initialization and is idempotent.
func seedDefaultPoliciesAndGroups(ctx context.Context, s store.Store) {
	// 1. Create hub-members group (skip if already exists)
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.Warn("failed to check for hub-members group", "error", err)
			return
		}
		group = &store.Group{
			ID:        api.NewUUID(),
			Name:      "Hub Members",
			Slug:      "hub-members",
			GroupType: store.GroupTypeExplicit,
		}
		if err := s.CreateGroup(ctx, group); err != nil {
			slog.Warn("failed to create hub-members group", "error", err)
			return
		}
		slog.Info("seeded hub-members group", "id", group.ID)
	}

	// 2. Seed hub-member-read-all policy
	seedPolicy(ctx, s, group.ID, &store.Policy{
		ID:           api.NewUUID(),
		Name:         "hub-member-read-all",
		Description:  "Allow hub members to read all resources",
		ScopeType:    "hub",
		ScopeID:      "",
		ResourceType: "*",
		Actions:      []string{"read", "list"},
		Effect:       "allow",
	})

	// 3. Seed hub-member-create-projects policy
	seedPolicy(ctx, s, group.ID, &store.Policy{
		ID:           api.NewUUID(),
		Name:         "hub-member-create-projects",
		Description:  "Allow hub members to create projects",
		ScopeType:    "hub",
		ScopeID:      "",
		ResourceType: "project",
		Actions:      []string{"create"},
		Effect:       "allow",
	})

	// Backfill Origin="seeded" on any existing seeded policies that predate
	// the Origin field.
	backfillSeededPolicyOrigin(ctx, s, []string{
		"hub-member-read-all",
		"hub-member-create-projects",
	})

	// The human half of the svc-accnt service-account assign baseline is
	// deliberately NOT seeded here. It is a PROJECT-scoped policy created per
	// project by createProjectMembersGroupAndPolicy, and backfilled onto
	// existing projects by backfillProjectAssignPolicies below. See
	// projectAssignPolicyName for why hub scope was rejected.
}

// projectAssignPolicyName returns the name of a project's service-account
// assign policy.
//
// ⚠️ Policy names are NOT unique hub-wide — there is no such constraint; see
// ensureProjectAssignPolicy. Idempotency here comes from that function looking
// the name up before creating, and from this being the only code that writes
// this name. It is a convention this package keeps, not an invariant the store
// enforces.
//
// ── Why this policy exists ────────────────────────────────────────────────
//
// Assigning a service account to an agent is gated today on ActionRead, which
// the hub-wide hub-member-read-all policy already allows for every hub member.
// svc-accnt P3 converts that gate to ActionAssign so the GCP actAs check has a
// resource-shaped place to hang. Without an assign grant somewhere, that
// conversion would deny every caller who is neither the account's creator nor
// a project owner or admin, since no other seeded policy grants assign. The
// security in that change comes from the actAs check, not from narrowing the
// Hub policy layer — a conversion that denies someone who can assign today is
// a regression, not a hardening.
//
// The population it must reproduce is exactly "members of this project":
//   - seed.go's hub-member-read-all grants read+list on "*" hub-wide, which is
//     what makes assign reachable for plain members today via the ActionRead
//     gate; and
//   - createProjectMembersGroupAndPolicy binds agent create as a PROJECT-scoped
//     policy to the members group, so only members of project P can create an
//     agent in P in the first place.
//
// Effective population today == project members. This policy reproduces it.
//
// ── Why project scope and not hub scope ───────────────────────────────────
//
// A hub-scoped assign policy on gcp_service_account cannot exclude parentless
// resources, so it would grant assign on every HUB-scoped service account to
// every hub member — which is the Goal 2 coupling arriving early, through the
// policy layer, before it has been ruled on.
//
// Project scope dissolves that structurally rather than managing it:
// matchesResource rejects a project-scoped policy against a resource that
// resolves to no project (pid == "" || pid != policy.ScopeID, fail closed
// rather than fall through — #595). gcpServiceAccountResource gives a project
// parent only to project-scoped accounts, so a hub-scoped account yields
// pid == "" and this policy CANNOT match it. No code-side guard is needed, and
// none should be added: a policy that presents as applied but is revoked
// elsewhere is the shape this change exists to avoid.
//
// ⚠️ WHAT CONFINES THIS POLICY is matchesResource alone: it refuses to match a
// project-scoped policy against a resource that resolves to no project (#595).
// That is a property of the authorization engine. It holds no matter what any
// handler does, and it is the only thing that needs to be true.
//
// Do NOT justify this policy by the scope check in createAgentInProject. An
// earlier version of this comment named `sa.ScopeID != projectID` there as an
// enforcing mechanism; a44b2950 replaced it with sa.ReachableFromProject,
// which admits hub-scoped accounts from every project. The justification went
// stale while the policy stayed correct for a reason the comment had not
// recorded. A confinement argument that names a call site can be invalidated
// by a commit to that call site, silently. Name engine properties only.
//
// The agent-side arm (step 3b of checkAccessForAgent) is confined by the SAME
// engine property read the other way: that arm by `pid != ""`, this policy by
// `pid == ""` in matchesResource. One discipline in two places, not two
// unrelated accidents.
//
// Goal 2 makes hub-scoped accounts assignable across projects. That does not
// breach this policy — a hub-scoped account stays parentless, so this policy
// still cannot match it, which is the fail-closed outcome §8.2 rules correct:
// hub-scoped accounts are assignable by hub admins and the account's creator
// and nobody else. If you are here to make hub-scoped accounts broadly
// assignable, that is task #19, and doing it by adding a hub-scoped assign
// policy would grant every hub member every service account on the hub.
//
// ⚠️ #19 CONSTRAINT: whatever implements that toggle must NOT do it by
// deleting or editing a grant by name. CreatePolicy enforces no name
// uniqueness, so a name identifies a SET of rows, and ListPolicies(Name:X,
// Limit:1) returns an arbitrary element of it. Additive operations are safe;
// revocation by name is not.
//
// ── Scope of the "preserves existing reach" claim ─────────────────────────
//
// It restores the reach of hub-member-read-all for project members and nothing
// else. The action-insensitive bypasses — hub admin, resource owner, project
// owner/admin — never consulted the action and are unaffected. A hand-authored
// policy granting read on service accounts to some other group is deliberately
// NOT mirrored, because a grant to read a service account is not a grant to
// assign one. Such an operator loses assign for that group and must grant it
// explicitly. Do not describe this policy as "reachability-preserving" without
// that qualification.
func projectAssignPolicyName(slug string) string {
	return "project:" + slug + ":member-assign-service-accounts"
}

// ensureProjectAssignPolicy creates the project's service-account assign
// policy and binds it to the project's members group. It is idempotent, and an
// existing policy has its ScopeID repaired in case the project was recreated.
// Best-effort; failures are logged.
//
// ResourceType is the single literal "gcp_service_account" and not "*".
// hub-member-read-all uses the wildcard because read is broadly safe; assign
// is not, so this mirrors that policy's reach without inheriting its breadth.
// Actions is assign alone for the same reason.
//
// ⚠️ Idempotency is by name LOOKUP, the way seedPolicy does it — deliberately
// not by creating and catching store.ErrAlreadyExists. CreatePolicy does not
// enforce name uniqueness, so that error never arrives and the catch would be
// dead code: three project touches would produce three identical policies.
// This is a live defect on the neighbouring project:<slug>:member-create-agents
// policy above, which does use the catch pattern. Do not "simplify" this back
// to create-then-catch, and do not assume a uniqueness constraint exists.
func ensureProjectAssignPolicy(ctx context.Context, s store.Store, project *store.Project, groupID string) {
	name := projectAssignPolicyName(project.Slug)

	existing, err := s.ListPolicies(ctx, store.PolicyFilter{Name: name}, store.ListOptions{Limit: 1})
	if err != nil {
		slog.Warn("failed to check for existing project assign policy",
			"project_id", project.ID, "policy", name, "error", err.Error())
		return
	}

	var policy *store.Policy
	if len(existing.Items) > 0 {
		policy = &existing.Items[0]
		if policy.ScopeID != project.ID {
			policy.ScopeID = project.ID
			if updateErr := s.UpdatePolicy(ctx, policy); updateErr != nil {
				slog.Warn("failed to update existing project assign policy",
					"project_id", project.ID, "policy", name, "error", updateErr.Error())
			}
		}
	} else {
		policy = &store.Policy{
			ID:           api.NewUUID(),
			Name:         name,
			Description:  "Allow project members to assign GCP service accounts in this project",
			ScopeType:    "project",
			ScopeID:      project.ID,
			ResourceType: "gcp_service_account",
			Actions:      []string{"assign"},
			Effect:       "allow",
		}
		if createErr := s.CreatePolicy(ctx, policy); createErr != nil {
			slog.Warn("failed to create project assign policy",
				"project_id", project.ID, "policy", name, "error", createErr.Error())
			return
		}
		slog.Info("created project assign policy", "project_id", project.ID, "policy", name, "id", policy.ID)
	}

	if err := s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID:      policy.ID,
		PrincipalType: "group",
		PrincipalID:   groupID,
	}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		slog.Warn("failed to bind project assign policy",
			"project_id", project.ID, "policy", name, "error", err.Error())
	}
}

// backfillProjectAssignPolicies gives every existing project the assign policy
// described on projectAssignPolicyName. It runs once at startup and is
// idempotent by policy name, like seedPolicy.
//
// It is not redundant with createProjectMembersGroupAndPolicy, which already
// runs on several project touch paths. Most projects would pick the policy up
// on their next touch — but a project nobody touches never would, and its
// members would silently lose the ability to assign service accounts the
// moment the ActionAssign conversion lands. Without this, the conversion is
// not reachability-preserving on existing hubs. Do not remove it on the
// grounds that the create path covers it; the create path covers active
// projects only.
//
// A project with no members group is skipped: there is nothing to bind to, and
// createProjectMembersGroupAndPolicy will create both together on next touch.
func backfillProjectAssignPolicies(ctx context.Context, s store.Store) {
	var cursor string
	for {
		res, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
			Limit:  500,
			Cursor: cursor,
		})
		if err != nil {
			slog.Warn("failed to list projects for assign-policy backfill", "error", err)
			return
		}
		for i := range res.Items {
			project := &res.Items[i]
			group, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
			if err != nil {
				slog.Debug("skipping assign-policy backfill, no members group",
					"project_id", project.ID, "slug", project.Slug)
				continue
			}
			ensureProjectAssignPolicy(ctx, s, project, group.ID)
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
}

// seedPolicyTombstoneKey returns the hub-setting key used to record that a
// seeded policy was intentionally deleted by an operator.
func seedPolicyTombstoneKey(policyName string) string {
	return fmt.Sprintf("seed.policy.deleted.%s", policyName)
}

// hasSeedPolicyTombstone returns true if a tombstone hub setting exists for the
// given seeded policy name, indicating it was intentionally deleted.
// It returns an error for transient store failures so the caller can fail-closed.
func hasSeedPolicyTombstone(ctx context.Context, s store.Store, policyName string) (bool, error) {
	_, err := s.GetHubSetting(ctx, seedPolicyTombstoneKey(policyName))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return false, err
}

// seedPolicy creates a policy and binds it to the given group, skipping
// if a policy with the same name already exists or if a deletion tombstone
// is present.
func seedPolicy(ctx context.Context, s store.Store, groupID string, policy *store.Policy) {
	// Check if policy already exists by name+scope.
	existing, err := s.ListPolicies(ctx, store.PolicyFilter{Name: policy.Name, ScopeType: policy.ScopeType}, store.ListOptions{Limit: 1})
	if err != nil {
		slog.Warn("failed to check for existing policy", "name", policy.Name, "error", err)
		return
	}
	if existing.TotalCount > 0 {
		return
	}

	// Check for deletion tombstone — an operator intentionally deleted this
	// seeded policy and it should not be recreated.
	hasTombstone, err := hasSeedPolicyTombstone(ctx, s, policy.Name)
	if err != nil {
		slog.Warn("failed to check tombstone; skipping recreation as precaution",
			"name", policy.Name, "error", err)
		return // fail-closed
	}
	if hasTombstone {
		slog.Info("seeded policy was intentionally deleted; skipping recreation",
			"name", policy.Name)
		return
	}

	// Mark as seeded so the delete handler can record a tombstone.
	policy.Origin = store.PolicyOriginSeeded
	// Mark as default kind so explicit policies can override at same priority.
	policy.PolicyKind = store.PolicyKindDefault

	if err := s.CreatePolicy(ctx, policy); err != nil {
		slog.Warn("failed to create seed policy", "name", policy.Name, "error", err)
		return
	}
	slog.Info("seeded policy", "name", policy.Name, "id", policy.ID)

	// Bind policy to the group
	binding := &store.PolicyBinding{
		PolicyID:      policy.ID,
		PrincipalType: "group",
		PrincipalID:   groupID,
	}
	if err := s.AddPolicyBinding(ctx, binding); err != nil {
		slog.Warn("failed to bind seed policy to hub-members group",
			"policy", policy.Name, "error", err)
	}
}

// backfillSeededPolicyOrigin marks existing seeded policies with
// Origin="seeded" if they were created before the Origin field existed.
// This ensures existing deployments get the marker on upgrade.
func backfillSeededPolicyOrigin(ctx context.Context, s store.Store, seededNames []string) {
	for _, name := range seededNames {
		existing, err := s.ListPolicies(ctx, store.PolicyFilter{Name: name, ScopeType: "hub"}, store.ListOptions{Limit: 1})
		if err != nil || len(existing.Items) == 0 {
			continue
		}
		p := &existing.Items[0]
		needsUpdate := false
		if p.Origin == "" {
			p.Origin = store.PolicyOriginSeeded
			needsUpdate = true
		}
		// Also backfill PolicyKind for seeded policies
		if p.Origin == store.PolicyOriginSeeded && p.PolicyKind == "" {
			p.PolicyKind = store.PolicyKindDefault
			needsUpdate = true
		}
		if needsUpdate {
			if err := s.UpdatePolicy(ctx, p); err != nil {
				slog.Warn("failed to backfill seeded policy origin/kind",
					"name", name, "error", err)
			} else {
				slog.Info("backfilled seeded policy origin/kind", "name", name, "id", p.ID)
			}
		}
	}
}

// seedDevUser ensures the development pseudo-user exists in the store.
// This is needed because Ent enforces foreign key constraints on owner_id,
// and the dev user must exist as a User record for project group creation to
// succeed in workstation/dev-auth mode.
func seedDevUser(ctx context.Context, s store.Store, cfg DevUserConfig) {
	u := NewDevUser(cfg)
	_, err := s.GetUser(ctx, DevUserID)
	if err == nil {
		return // already exists
	}
	if !errors.Is(err, store.ErrNotFound) {
		slog.Warn("failed to check for dev user", "error", err)
		return
	}
	if err := s.CreateUser(ctx, &store.User{
		ID:          DevUserID,
		Email:       u.Email(),
		DisplayName: u.DisplayName(),
		Role:        "admin",
		Status:      "active",
	}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		slog.Warn("failed to seed dev user", "error", err)
	}
}

// seedRoleDefinitions creates the system role definitions if they don't
// already exist. It is called once during Hub initialization and is idempotent.
func seedRoleDefinitions(ctx context.Context, s store.Store) {
	allPermIDs := allPermissionIDs()
	readListPermIDs := permissionIDsByActions("read", "list")
	readOnlyPermIDs := permissionIDsByActions("read")

	// Project-scoped permission IDs (all permissions that can apply to project resources)
	projectAllPermIDs := projectScopedPermissionIDs()
	projectAdminPermIDs := projectPermissionIDsExcluding("delete")
	projectMemberPermIDs := projectMemberPermissionIDs()

	// Agent role permission IDs (mapped from agent token scopes)
	agentReadonlyPermIDs := agentRolePermissionIDs(AgentRoleReadOnly)
	agentBaselinePermIDs := agentRolePermissionIDs(AgentRoleBaseline)
	agentFullPermIDs := agentRolePermissionIDs(AgentRoleFull)

	systemRoles := []struct {
		name        string
		description string
		scopeType   string
		permissions []string
	}{
		{store.SystemRoleSuperAdmin, "Full platform administrator with all permissions", store.RoleScopeSystem, allPermIDs},
		{store.SystemRoleHubMember, "Hub member with read access and project creation", store.RoleScopeSystem, readListPermIDs},
		{store.SystemRoleHubViewer, "Hub viewer with read-only access", store.RoleScopeSystem, readOnlyPermIDs},
		{store.ProjectRoleOwner, "Project owner with full project permissions", store.RoleScopeProject, projectAllPermIDs},
		{store.ProjectRoleAdmin, "Project admin with most project permissions (no delete)", store.RoleScopeProject, projectAdminPermIDs},
		{store.ProjectRoleMember, "Project member with basic project permissions", store.RoleScopeProject, projectMemberPermIDs},
		{store.AgentRoleDefNone, "No agent permissions", store.RoleScopeSystem, nil},
		{store.AgentRoleDefReadonly, "Read-only agent permissions", store.RoleScopeSystem, agentReadonlyPermIDs},
		{store.AgentRoleDefBaseline, "Baseline agent permissions", store.RoleScopeSystem, agentBaselinePermIDs},
		{store.AgentRoleDefFull, "Full agent permissions", store.RoleScopeSystem, agentFullPermIDs},
	}

	for _, role := range systemRoles {
		seedRoleDefinition(ctx, s, role.name, role.description, role.scopeType, role.permissions)
	}
}

// seedRoleDefinition creates a single role definition if it doesn't already exist.
func seedRoleDefinition(ctx context.Context, s store.Store, name, description, scopeType string, perms []string) {
	_, err := s.GetRoleDefinitionByName(ctx, name, scopeType)
	if err == nil {
		return // already exists
	}
	if !errors.Is(err, store.ErrNotFound) {
		slog.Warn("failed to check for existing role definition", "name", name, "error", err)
		return
	}
	rd := &store.RoleDefinition{
		Name:        name,
		Description: description,
		ScopeType:   scopeType,
		Permissions: perms,
		System:      true,
	}
	if _, err := s.CreateRoleDefinition(ctx, rd); err != nil {
		slog.Warn("failed to seed role definition", "name", name, "error", err)
		return
	}
	slog.Info("seeded role definition", "name", name, "scope_type", scopeType)
}

// allPermissionIDs returns IDs for all permissions in the registry.
func allPermissionIDs() []string {
	ids := make([]string, len(permissions.Registry))
	for i, p := range permissions.Registry {
		ids[i] = p.ID
	}
	return ids
}

// permissionIDsByActions returns permission IDs for permissions whose action matches any given action.
func permissionIDsByActions(actions ...string) []string {
	actionSet := make(map[string]bool, len(actions))
	for _, a := range actions {
		actionSet[a] = true
	}
	var ids []string
	for _, p := range permissions.Registry {
		if actionSet[p.Action] {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// projectScopedPermissionIDs returns permission IDs for resources that are
// typically project-scoped (agents, templates, harness configs, skills, etc.)
// plus project.* itself.
func projectScopedPermissionIDs() []string {
	projectResources := map[string]bool{
		permissions.ResourceAgent:         true,
		permissions.ResourceProject:       true,
		permissions.ResourceTemplate:      true,
		permissions.ResourceHarnessConfig: true,
		permissions.ResourceSkill:         true,
	}
	var ids []string
	for _, p := range permissions.Registry {
		if projectResources[p.Resource] {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// projectPermissionIDsExcluding returns project-scoped permission IDs
// excluding those with the given action.
func projectPermissionIDsExcluding(excludeAction string) []string {
	projectResources := map[string]bool{
		permissions.ResourceAgent:         true,
		permissions.ResourceProject:       true,
		permissions.ResourceTemplate:      true,
		permissions.ResourceHarnessConfig: true,
		permissions.ResourceSkill:         true,
	}
	var ids []string
	for _, p := range permissions.Registry {
		if projectResources[p.Resource] && p.Action != excludeAction {
			ids = append(ids, p.ID)
		}
	}
	return ids
}

// projectMemberPermissionIDs returns the permission IDs that a regular
// project member gets: create agents, read/list most things.
func projectMemberPermissionIDs() []string {
	memberActions := map[string]bool{
		"create": true,
		"read":   true,
		"list":   true,
	}
	projectResources := map[string]bool{
		permissions.ResourceAgent:         true,
		permissions.ResourceProject:       true,
		permissions.ResourceTemplate:      true,
		permissions.ResourceHarnessConfig: true,
		permissions.ResourceSkill:         true,
	}
	var ids []string
	for _, p := range permissions.Registry {
		if projectResources[p.Resource] && memberActions[p.Action] {
			ids = append(ids, p.ID)
		}
	}
	// Also include stop_all for project members (matching existing policy)
	ids = append(ids, "agent.stop_all")
	return ids
}

// agentRolePermissionIDs maps an AgentRole to permission IDs by examining
// what scopes that role grants.
func agentRolePermissionIDs(role AgentRole) []string {
	scopes := ScopesForRole(role)
	if scopes == nil {
		return nil
	}
	// Build a set of scope strings
	scopeSet := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		scopeSet[string(s)] = true
	}
	// Map scopes back to permission IDs
	var ids []string
	for _, p := range permissions.Registry {
		for _, s := range p.AgentScopes {
			if scopeSet[s] {
				ids = append(ids, p.ID)
				break
			}
		}
	}
	return ids
}

// BackfillRoleBindings creates role bindings from existing User.Role values and
// project group memberships. It is idempotent (skips if binding already exists)
// and called from the startup/migration path.
func BackfillRoleBindings(ctx context.Context, s store.Store) error {
	// Backfill system role bindings from User.Role
	if err := backfillUserRoleBindings(ctx, s); err != nil {
		return fmt.Errorf("backfill user role bindings: %w", err)
	}

	// Backfill project role bindings from group memberships
	if err := backfillProjectRoleBindings(ctx, s); err != nil {
		return fmt.Errorf("backfill project role bindings: %w", err)
	}

	return nil
}

// backfillUserRoleBindings creates system-scoped role bindings from User.Role.
// It paginates through all users to avoid silent truncation by store defaults.
func backfillUserRoleBindings(ctx context.Context, s store.Store) error {
	userRoleMap := map[string]string{
		"admin":  store.SystemRoleSuperAdmin,
		"member": store.SystemRoleHubMember,
		"viewer": store.SystemRoleHubViewer,
	}

	var cursor string
	var created int
	for {
		users, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{
			Limit:  200,
			Cursor: cursor,
		})
		if err != nil {
			return err
		}

		for i := range users.Items {
			u := &users.Items[i]
			roleName, ok := userRoleMap[u.Role]
			if !ok {
				continue
			}

			rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
			if err != nil {
				slog.Warn("role definition not found during backfill", "role", roleName, "error", err)
				continue
			}

			_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
				RoleDefinitionID: rd.ID,
				PrincipalType:    store.RoleBindingPrincipalUser,
				PrincipalID:      u.ID,
				ScopeType:        store.RoleScopeSystem,
				ScopeID:          "",
				CreatedBy:        "system-backfill",
			})
			if err != nil {
				if errors.Is(err, store.ErrAlreadyExists) {
					continue // already backfilled
				}
				slog.Warn("failed to create role binding during backfill",
					"user_id", u.ID, "role", roleName, "error", err)
				continue
			}
			created++
		}

		if users.NextCursor == "" {
			break
		}
		cursor = users.NextCursor
	}

	if created > 0 {
		slog.Info("backfilled user role bindings", "created", created)
	}
	return nil
}

// backfillProjectRoleBindings creates project-scoped role bindings from group memberships.
// It paginates through all projects to avoid silent truncation by store defaults.
func backfillProjectRoleBindings(ctx context.Context, s store.Store) error {
	groupRoleMap := map[string]string{
		store.GroupMemberRoleOwner:  store.ProjectRoleOwner,
		store.GroupMemberRoleAdmin:  store.ProjectRoleAdmin,
		store.GroupMemberRoleMember: store.ProjectRoleMember,
	}

	var cursor string
	var created int
	for {
		projects, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{
			Limit:  500,
			Cursor: cursor,
		})
		if err != nil {
			return err
		}

		for i := range projects.Items {
			project := &projects.Items[i]
			groupSlug := "project:" + project.Slug + ":members"

			group, err := s.GetGroupBySlug(ctx, groupSlug)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					continue
				}
				slog.Warn("failed to look up project members group",
					"project_id", project.ID, "slug", groupSlug, "error", err)
				continue
			}

			members, err := s.GetGroupMembers(ctx, group.ID)
			if err != nil {
				slog.Warn("failed to get group members",
					"project_id", project.ID, "group_id", group.ID, "error", err)
				continue
			}

			for _, m := range members {
				if m.MemberType != store.GroupMemberTypeUser {
					continue
				}

				roleName, ok := groupRoleMap[m.Role]
				if !ok {
					continue
				}

				rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
				if err != nil {
					slog.Warn("role definition not found during project backfill",
						"role", roleName, "error", err)
					continue
				}

				_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: rd.ID,
					PrincipalType:    store.RoleBindingPrincipalUser,
					PrincipalID:      m.MemberID,
					ScopeType:        store.RoleScopeProject,
					ScopeID:          project.ID,
					CreatedBy:        "system-backfill",
				})
				if err != nil {
					if errors.Is(err, store.ErrAlreadyExists) {
						continue
					}
					slog.Warn("failed to create project role binding during backfill",
						"user_id", m.MemberID, "project_id", project.ID,
						"role", roleName, "error", err)
					continue
				}
				created++
			}
		}

		if projects.NextCursor == "" {
			break
		}
		cursor = projects.NextCursor
	}

	if created > 0 {
		slog.Info("backfilled project role bindings", "created", created)
	}
	return nil
}

// ReconcileSuperAdminBindings ensures bidirectional consistency between
// User.Role == "admin", the AdminEmails config list, and system-scoped
// super-admin role bindings (Phase 1F, D11 revocability fix). On startup:
//
// Single-pass reconciliation (D11-fix2):
//
//	For each user, in one pass:
//	  - If the user is in adminEmails: promote Role to "admin" (if needed) and
//	    ensure a super-admin binding exists.
//	  - If the user is NOT in adminEmails AND adminEmails is non-empty: demote
//	    Role from "admin" to "member" (if needed) and delete any super-admin
//	    binding. Ordinary grants (non-super-admin) are NOT touched.
//
// Empty-list safety guard:
//   - When adminEmails is nil or empty, no demotions occur and a warning is
//     logged. An empty list is almost always a config load failure, not an
//     instruction to remove every administrator. Both nil and len==0 take the
//     same branch.
//
// Revocation latency: super-admin revocation takes effect on next hub restart.
//
// This is called after BackfillRoleBindings and is idempotent.
func ReconcileSuperAdminBindings(ctx context.Context, s store.Store, adminEmails []string) (demotionSafe bool, err error) {
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			slog.Warn("super-admin role definition not found; skipping reconciliation")
			return false, nil
		}
		return false, fmt.Errorf("lookup super-admin role definition: %w", err)
	}

	// Build a lowercase set of admin emails for O(1) lookup.
	adminSet := make(map[string]bool, len(adminEmails))
	for _, e := range adminEmails {
		adminSet[strings.ToLower(strings.TrimSpace(e))] = true
	}

	// ---- Effect guard (D11-fix3): two-pass classify-then-apply ----
	//
	// Pass 1: scan ALL existing users to build the intended final admin set
	// (existing users whose normalized email appears in AdminEmails). If the
	// intended admin set is empty, refuse all demotions — regardless of the
	// reason (empty config, whitespace corruption, encoding issues).
	//
	// This MUST be computed before any mutation; evaluating inside the
	// mutation loop would be row-order dependent (same bug class as R1).

	canDemote := len(adminEmails) > 0

	if canDemote {
		// Pre-scan: count existing users who match AdminEmails. This is the
		// intended admin set — the set of users who WILL be admin after
		// reconciliation completes.
		var intendedAdminCount int
		var preCursor string
		for {
			users, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{
				Limit:  200,
				Cursor: preCursor,
			})
			if err != nil {
				return false, err
			}
			for i := range users.Items {
				if adminSet[strings.ToLower(users.Items[i].Email)] {
					intendedAdminCount++
				}
			}
			if users.NextCursor == "" {
				break
			}
			preCursor = users.NextCursor
		}

		if intendedAdminCount == 0 {
			// Every email in AdminEmails belongs to a user who has never
			// logged in, or AdminEmails contains only whitespace/typos that
			// match nobody. Proceeding would demote every current admin,
			// leaving zero administrators. Refuse.
			slog.Error("effect guard: reconciliation would leave ZERO administrators — "+
				"AdminEmails matches no existing users; refusing all demotions. "+
				"Verify AdminEmails entries match real user emails",
				"admin_emails_count", len(adminEmails))
			canDemote = false
		}
	}

	if !canDemote && len(adminEmails) == 0 {
		// When AdminEmails is empty, BOTH directions are disabled: inAdminList is
		// false for every user (disabling forward promotion/binding creation) and
		// canDemote is false (disabling reverse demotion/binding deletion). This is
		// fail-closed: a config load failure cannot cause new promotions OR
		// demotions. Operators seeing this warning should verify their AdminEmails
		// config is loading correctly — until it is non-empty, reconciliation will
		// not repair any admin state.
		slog.Warn("AdminEmails is nil or empty; all reconciliation disabled to prevent hub-wide lockout — verify config load")
	}

	// ---- Pass 2: apply promotions and (if safe) demotions ----

	var created, demoted, deleted int
	var cursor string
	for {
		users, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{
			Limit:  200,
			Cursor: cursor,
		})
		if err != nil {
			return false, err
		}
		for i := range users.Items {
			u := &users.Items[i]
			inAdminList := adminSet[strings.ToLower(u.Email)]

			if inAdminList {
				// Forward: ensure admin role and super-admin binding.
				if u.Role != "admin" {
					u.Role = "admin"
					if err := s.UpdateUser(ctx, u); err != nil {
						slog.Warn("failed to promote user during reconciliation",
							"user_id", u.ID, "error", err)
					}
				}
				_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
					RoleDefinitionID: rd.ID,
					PrincipalType:    store.RoleBindingPrincipalUser,
					PrincipalID:      u.ID,
					ScopeType:        store.RoleScopeSystem,
					ScopeID:          "",
					CreatedBy:        store.SystemReconcileCreatedBy,
				})
				if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
					slog.Warn("failed to create super-admin binding during reconciliation",
						"user_id", u.ID, "error", err)
				} else if err == nil {
					created++
				}
			} else if canDemote {
				// Reverse: demote role and delete super-admin binding.
				if u.Role == "admin" {
					slog.Warn("demoting user: removed from AdminEmails",
						"user_id", u.ID, "email", u.Email, "old_role", "admin", "new_role", "member")
					u.Role = "member"
					if err := s.UpdateUser(ctx, u); err != nil {
						slog.Warn("failed to demote user during reconciliation",
							"user_id", u.ID, "error", err)
					} else {
						demoted++
					}
				}
				// Delete orphaned super-admin bindings for users NOT in adminEmails.
				// Only the super-admin binding is touched — ordinary grants are preserved.
				bindings, err := s.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, u.ID)
				if err != nil {
					continue
				}
				for _, b := range bindings {
					if b.ScopeType == store.RoleScopeSystem && b.RoleDefinitionID == rd.ID {
						slog.Warn("deleting orphaned super-admin binding",
							"user_id", u.ID, "email", u.Email, "binding_id", b.ID)
						if err := s.DeleteRoleBinding(ctx, b.ID); err != nil {
							slog.Warn("failed to delete orphaned super-admin binding",
								"binding_id", b.ID, "error", err)
						} else {
							deleted++
						}
					}
				}
			}
		}
		if users.NextCursor == "" {
			break
		}
		cursor = users.NextCursor
	}

	if created > 0 {
		slog.Info("reconciled super-admin bindings (forward)", "created", created)
	}
	if demoted > 0 || deleted > 0 {
		slog.Info("reconciled super-admin bindings (reverse/D11)",
			"users_demoted", demoted, "bindings_deleted", deleted)
	}

	return canDemote, nil
}

// ensureHubMembership adds the given user to the hub-members group.
// This is best-effort; errors are logged at debug level and ignored.
func ensureHubMembership(ctx context.Context, s store.Store, userID string) {
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	if err != nil {
		slog.Debug("hub-members group not found, skipping membership", "error", err)
		return
	}

	err = s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    group.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       store.GroupMemberRoleMember,
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		slog.Debug("failed to add user to hub-members group", "userID", userID, "error", err)
	}
}
