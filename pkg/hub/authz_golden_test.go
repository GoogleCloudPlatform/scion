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

//go:build !no_sqlite

package hub

// Golden decision tests for BI1: Behavior Inventory and Golden Decisions.
//
// These tests capture the INTENDED post-cutover authorization decisions for
// key access paths. They test against the CURRENT evaluator to establish a
// baseline, with clear documentation of which decisions change intentionally.
//
// Each test documents:
// - Current behavior (what happens today)
// - Intended post-cutover behavior (same or changed)
// - If changed: why the change is correct (reference design doc section)
//
// See also: pkg/hub/testdata/policy_inventory.md for the full policy inventory.

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goldenFixture sets up a shared world for the golden decision tests:
//
//   - Two projects (alpha, beta) with members groups and agents
//   - A super-admin user, a hub-admin user
//   - A hub-member user who is a member of project alpha only
//   - A hub-member user with NO project memberships
//   - A project-owner user for alpha
//   - A project-admin user for alpha
//   - Agents in both projects, with ancestry chains for progeny tests
//   - Progeny policies for secrets, env vars, and skill injections
type goldenFixture struct {
	authz *AuthzService
	store store.Store

	projectAlpha *store.Project
	projectBeta  *store.Project

	// Groups
	hubMembersGroup    *store.Group
	alphaMembersGroup  *store.Group
	betaMembersGroup   *store.Group
	alphaAgentsGroup   *store.Group
	betaAgentsGroup    *store.Group

	// Users
	superAdminID       string
	hubAdminID         string
	memberAlphaID      string // hub member AND alpha project member
	memberNoneID       string // hub member with NO project memberships
	projectOwnerID     string // owner of project alpha
	projectAdminID     string // admin of project alpha

	// Agents
	agentAlpha         *store.Agent // agent in project alpha
	agentBeta          *store.Agent // agent in project beta

	// Progeny test resources
	secretID           string
	envVarID           string
	skillInjectionID   string
}

func newGoldenFixture(t *testing.T) *goldenFixture {
	t.Helper()
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	f := &goldenFixture{
		authz:            authz,
		store:            s,
		superAdminID:     tid("golden-superadmin"),
		hubAdminID:       tid("golden-hubadmin"),
		memberAlphaID:    tid("golden-member-alpha"),
		memberNoneID:     tid("golden-member-none"),
		projectOwnerID:   tid("golden-proj-owner"),
		projectAdminID:   tid("golden-proj-admin"),
		secretID:         tid("golden-secret"),
		envVarID:         tid("golden-envvar"),
		skillInjectionID: tid("golden-skill-inj"),
	}

	// --- Projects ---
	f.projectAlpha = &store.Project{
		ID: tid("golden-project-alpha"), Name: "Alpha", Slug: "golden-alpha",
		OwnerID: f.projectOwnerID,
	}
	f.projectBeta = &store.Project{
		ID: tid("golden-project-beta"), Name: "Beta", Slug: "golden-beta",
		OwnerID: tid("golden-beta-owner"),
	}
	require.NoError(t, s.CreateProject(ctx, f.projectAlpha))
	require.NoError(t, s.CreateProject(ctx, f.projectBeta))

	// --- Groups ---
	// hub-members group (the seeded one may already exist from testServer)
	hmGroup, err := s.GetGroupBySlug(ctx, "hub-members")
	if err != nil {
		hmGroup = &store.Group{
			ID: api.NewUUID(), Name: "Hub Members", Slug: "hub-members",
			GroupType: store.GroupTypeExplicit,
		}
		require.NoError(t, s.CreateGroup(ctx, hmGroup))
	}
	f.hubMembersGroup = hmGroup

	f.alphaMembersGroup = &store.Group{
		ID: api.NewUUID(), Name: "Alpha Members",
		Slug: "project:golden-alpha:members", GroupType: store.GroupTypeExplicit,
		ProjectID: f.projectAlpha.ID,
	}
	f.betaMembersGroup = &store.Group{
		ID: api.NewUUID(), Name: "Beta Members",
		Slug: "project:golden-beta:members", GroupType: store.GroupTypeExplicit,
		ProjectID: f.projectBeta.ID,
	}
	f.alphaAgentsGroup = &store.Group{
		ID: api.NewUUID(), Name: "Alpha Agents",
		Slug: "project:golden-alpha:agents", GroupType: store.GroupTypeProjectAgents,
		ProjectID: f.projectAlpha.ID,
	}
	f.betaAgentsGroup = &store.Group{
		ID: api.NewUUID(), Name: "Beta Agents",
		Slug: "project:golden-beta:agents", GroupType: store.GroupTypeProjectAgents,
		ProjectID: f.projectBeta.ID,
	}
	require.NoError(t, s.CreateGroup(ctx, f.alphaMembersGroup))
	require.NoError(t, s.CreateGroup(ctx, f.betaMembersGroup))
	require.NoError(t, s.CreateGroup(ctx, f.alphaAgentsGroup))
	require.NoError(t, s.CreateGroup(ctx, f.betaAgentsGroup))

	// --- Users ---
	// Super-admin
	createTestUserWithRole(t, s, f.superAdminID, "superadmin@golden.test", "admin", store.SystemRoleSuperAdmin)
	// Hub admin
	createTestUserWithRole(t, s, f.hubAdminID, "hubadmin@golden.test", "member", store.SystemRoleHubAdmin)
	// Hub member who is also alpha project member
	createTestUserWithRole(t, s, f.memberAlphaID, "member-alpha@golden.test", "member", store.SystemRoleHubMember)
	// Hub member with NO project memberships
	createTestUserWithRole(t, s, f.memberNoneID, "member-none@golden.test", "member", store.SystemRoleHubMember)
	// Project owner of alpha (also a hub member)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: f.projectOwnerID, Email: "proj-owner@golden.test",
		DisplayName: "Project Owner", Role: "member", Status: "active",
	}))
	// Project admin of alpha (also a hub member)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: f.projectAdminID, Email: "proj-admin@golden.test",
		DisplayName: "Project Admin", Role: "member", Status: "active",
	}))

	// --- Hub Members group memberships ---
	for _, uid := range []string{f.memberAlphaID, f.memberNoneID, f.projectOwnerID, f.projectAdminID} {
		_ = s.AddGroupMember(ctx, &store.GroupMember{
			GroupID: f.hubMembersGroup.ID, MemberID: uid,
			MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleMember,
		})
	}

	// --- Alpha project memberships ---
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: f.alphaMembersGroup.ID, MemberID: f.projectOwnerID,
		MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleOwner,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: f.alphaMembersGroup.ID, MemberID: f.projectAdminID,
		MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleAdmin,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: f.alphaMembersGroup.ID, MemberID: f.memberAlphaID,
		MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleMember,
	}))

	// --- Project role bindings for alpha members ---
	createTestUserWithProjectRole(t, s, f.projectOwnerID, "proj-owner@golden.test",
		f.projectAlpha.ID, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, s, f.projectAdminID, "proj-admin@golden.test",
		f.projectAlpha.ID, store.ProjectRoleAdmin)
	createTestUserWithProjectRole(t, s, f.memberAlphaID, "member-alpha@golden.test",
		f.projectAlpha.ID, store.ProjectRoleMember)

	// --- Agents ---
	f.agentAlpha = &store.Agent{
		ID: tid("golden-agent-alpha"), Slug: "golden-agent-alpha",
		Name: "Alpha Agent", ProjectID: f.projectAlpha.ID,
		Phase:    string(state.PhaseRunning),
		OwnerID:  f.projectOwnerID,
		Ancestry: []string{f.projectOwnerID},
	}
	f.agentBeta = &store.Agent{
		ID: tid("golden-agent-beta"), Slug: "golden-agent-beta",
		Name: "Beta Agent", ProjectID: f.projectBeta.ID,
		Phase:   string(state.PhaseRunning),
		OwnerID: tid("golden-beta-owner"),
	}
	require.NoError(t, s.CreateAgent(ctx, f.agentAlpha))
	require.NoError(t, s.CreateAgent(ctx, f.agentBeta))

	// --- Per-project member policies (matching seed.go behavior) ---
	// member-read-project/agent and member-create-agents policies for alpha
	for _, rt := range []string{"project", "agent"} {
		policy := &store.Policy{
			ID: api.NewUUID(),
			Name:         "project:golden-alpha:member-read-" + rt,
			ScopeType:    "project",
			ScopeID:      f.projectAlpha.ID,
			ResourceType: rt,
			Actions:      []string{"read", "list"},
			Effect:       "allow",
		}
		require.NoError(t, s.CreatePolicy(ctx, policy))
		require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
			PolicyID: policy.ID, PrincipalType: "group", PrincipalID: f.alphaMembersGroup.ID,
		}))
	}

	// member-create-agents for alpha
	createAgentsPolicy := &store.Policy{
		ID: api.NewUUID(),
		Name:         "project:golden-alpha:member-create-agents",
		ScopeType:    "project",
		ScopeID:      f.projectAlpha.ID,
		ResourceType: "agent",
		Actions:      []string{"create", "stop_all", "message"},
		Effect:       "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, createAgentsPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: createAgentsPolicy.ID, PrincipalType: "group",
		PrincipalID: f.alphaMembersGroup.ID,
	}))

	// --- Progeny policies (matching handlers_env_secrets.go / handlers_skills_injection.go) ---
	// Secret progeny policy
	secretPolicy := &store.Policy{
		ID: api.NewUUID(),
		Name:         "progeny-secret-access:" + f.secretID,
		ScopeType:    store.PolicyScopeResource,
		ScopeID:      f.secretID,
		ResourceType: "secret",
		ResourceID:   f.secretID,
		Actions:      []string{"read"},
		Effect:       store.PolicyEffectAllow,
		Conditions: &store.PolicyConditions{
			DelegatedFrom: &store.DelegatedFromCondition{
				PrincipalType: "user",
				PrincipalID:   f.projectOwnerID,
			},
		},
		CreatedBy: f.projectOwnerID,
	}
	require.NoError(t, s.CreatePolicy(ctx, secretPolicy))

	// EnvVar progeny policy
	envVarPolicy := &store.Policy{
		ID: api.NewUUID(),
		Name:         "progeny-envvar-access:" + f.envVarID,
		ScopeType:    store.PolicyScopeResource,
		ScopeID:      f.envVarID,
		ResourceType: "envvar",
		ResourceID:   f.envVarID,
		Actions:      []string{"read"},
		Effect:       store.PolicyEffectAllow,
		Conditions: &store.PolicyConditions{
			DelegatedFrom: &store.DelegatedFromCondition{
				PrincipalType: "user",
				PrincipalID:   f.projectOwnerID,
			},
		},
		CreatedBy: f.projectOwnerID,
	}
	require.NoError(t, s.CreatePolicy(ctx, envVarPolicy))

	// Skill injection progeny policy
	skillPolicy := &store.Policy{
		ID: api.NewUUID(),
		Name:         "progeny-skill-access:" + f.skillInjectionID,
		ScopeType:    store.PolicyScopeResource,
		ScopeID:      f.skillInjectionID,
		ResourceType: "skill_injection",
		ResourceID:   f.skillInjectionID,
		Actions:      []string{"read"},
		Effect:       store.PolicyEffectAllow,
		Conditions: &store.PolicyConditions{
			DelegatedFrom: &store.DelegatedFromCondition{
				PrincipalType: "user",
				PrincipalID:   f.projectOwnerID,
			},
		},
		CreatedBy: f.projectOwnerID,
	}
	require.NoError(t, s.CreatePolicy(ctx, skillPolicy))

	return f
}

// =============================================================================
// Golden Decision 1: Hub member listing projects
// =============================================================================

// TestGolden_HubMemberListProjects_OnlyOwnProjects verifies that a hub member
// should see ONLY projects they are a member of.
//
// Current behavior: A hub member with the hub-member role binding holds
// project.list and project.read through permissionIDsByActions("read","list").
// When list handlers call hasAdminView with permission "project.list", the
// role binding check (authz.go:576-598) returns allowed. The handler then
// skips per-project filtering, showing ALL projects.
//
// Intended post-cutover behavior: Hub members see ONLY projects they have a
// project-scoped RoleBinding for. The hub-member RoleDefinition is curated
// to exclude project.list and project.read at system scope. List handlers
// use ResolveAuthorizedScopes instead of hasAdminView.
//
// Reference: design.md §5 (ResolveAuthorizedScopes), §Immediate sequencing
// constraint.
func TestGolden_HubMemberListProjects_OnlyOwnProjects(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	// Hub member who IS a member of alpha but NOT beta
	user := NewAuthenticatedUser(f.memberAlphaID, "member-alpha@golden.test", "Member Alpha", "member", "api")

	// Alpha project: member should be able to read (via project membership)
	alphaRes := Resource{
		Type: "project", ID: f.projectAlpha.ID,
		OwnerID: f.projectOwnerID,
	}
	decision := f.authz.CheckAccess(ctx, user, alphaRes, ActionRead)
	assert.True(t, decision.Allowed,
		"member should be able to read their own project")

	// Beta project: member should NOT be able to read (not a member)
	betaRes := Resource{
		Type: "project", ID: f.projectBeta.ID,
		OwnerID: tid("golden-beta-owner"),
	}
	decision = f.authz.CheckAccess(ctx, user, betaRes, ActionRead)
	// CURRENT: This is allowed via role binding grant (project.read from hub-member role)
	// POST-CUTOVER: This MUST be denied — member is not in project beta.
	//
	// Document the current behavior: the hub-member system role grants
	// project.read hub-wide, so CheckAccess with Permission="project.read"
	// would return allowed. The direct CheckAccess without Permission relies
	// on policy, which correctly denies.
	//
	// The actual vulnerability is in the hasAdminView pattern at handler level,
	// not in CheckAccess(identity, resource, action). CheckAccess with just
	// action correctly denies because no policy grants read on beta for this user.
	assert.False(t, decision.Allowed,
		"member should NOT be able to read a project they are not a member of via direct CheckAccess")
	assert.Equal(t, "default deny", decision.Reason)
}

// =============================================================================
// Golden Decision 2: Hub member listing agents
// =============================================================================

// TestGolden_HubMemberListAgents_OnlyOwnProjectAgents verifies that a hub
// member should see ONLY agents in projects they are a member of.
//
// Current behavior: Same hasAdminView vulnerability as projects. The hub-member
// role holds agent.list and agent.read through permissionIDsByActions.
//
// Intended post-cutover: Same fix as projects — curated role, scope-aware lists.
//
// Reference: design.md §Immediate sequencing constraint.
func TestGolden_HubMemberListAgents_OnlyOwnProjectAgents(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	user := NewAuthenticatedUser(f.memberAlphaID, "member-alpha@golden.test", "Member Alpha", "member", "api")

	// Alpha agent: member should read (project member + policy grants read+list on agent)
	alphaAgentRes := Resource{
		Type: "agent", ID: f.agentAlpha.ID,
		OwnerID: f.projectOwnerID,
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}
	decision := f.authz.CheckAccess(ctx, user, alphaAgentRes, ActionRead)
	assert.True(t, decision.Allowed,
		"member should read agents in their own project")

	// Beta agent: member should NOT read (not a project member, no policy)
	betaAgentRes := Resource{
		Type: "agent", ID: f.agentBeta.ID,
		OwnerID: tid("golden-beta-owner"),
		ParentType: "project", ParentID: f.projectBeta.ID,
	}
	decision = f.authz.CheckAccess(ctx, user, betaAgentRes, ActionRead)
	assert.False(t, decision.Allowed,
		"member should NOT read agents in a project they are not a member of")
	assert.Equal(t, "default deny", decision.Reason)
}

// =============================================================================
// Golden Decision 3: Project owner accessing project resources
// =============================================================================

// TestGolden_ProjectOwnerFullAccess verifies that a project owner has full
// access within their project scope.
//
// Current behavior: Project owner/admin bypass at authz.go:533-539 grants all
// actions on all resources scoped to the project.
//
// Intended post-cutover: The project-owner RoleDefinition contains all
// project-scoped permissions. Evaluated through the standard pipeline, not
// an early-return bypass. Behavior is preserved.
//
// Reference: design.md §4 (Project membership), §5 (no privileged early-allow).
func TestGolden_ProjectOwnerFullAccess(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	owner := NewAuthenticatedUser(f.projectOwnerID, "proj-owner@golden.test", "Project Owner", "member", "api")
	alphaAgentRes := Resource{
		Type: "agent", ID: f.agentAlpha.ID,
		OwnerID: f.projectOwnerID,
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}

	// Owner should be able to do everything on project resources
	for _, action := range []Action{ActionRead, ActionUpdate, ActionDelete, ActionStart, ActionStop, ActionMessage} {
		decision := f.authz.CheckAccess(ctx, owner, alphaAgentRes, action)
		assert.True(t, decision.Allowed,
			"project owner should have %s access on project agents", action)
	}

	// Owner should also access the project itself
	projectRes := Resource{Type: "project", ID: f.projectAlpha.ID, OwnerID: f.projectOwnerID}
	decision := f.authz.CheckAccess(ctx, owner, projectRes, ActionUpdate)
	assert.True(t, decision.Allowed, "project owner should update their own project")

	// Owner should NOT access beta project resources (not owner/member there)
	betaAgentRes := Resource{
		Type: "agent", ID: f.agentBeta.ID,
		OwnerID: tid("golden-beta-owner"),
		ParentType: "project", ParentID: f.projectBeta.ID,
	}
	decision = f.authz.CheckAccess(ctx, owner, betaAgentRes, ActionRead)
	assert.False(t, decision.Allowed,
		"project alpha owner should NOT access project beta resources")
}

// =============================================================================
// Golden Decision 4: Project admin accessing project resources
// =============================================================================

// TestGolden_ProjectAdminAccess verifies admin access within project scope.
//
// Current behavior: Project owner/admin bypass at authz.go:533-539 gives the
// same total bypass as the owner.
//
// Intended post-cutover: The project-admin RoleDefinition has all project
// permissions except delete and agent.set_message_mode. Behavior narrows
// intentionally.
//
// Reference: design.md §4, seed.go projectPermissionIDsExcluding("delete").
func TestGolden_ProjectAdminAccess(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser(f.projectAdminID, "proj-admin@golden.test", "Project Admin", "member", "api")
	alphaAgentRes := Resource{
		Type: "agent", ID: f.agentAlpha.ID,
		OwnerID: f.projectOwnerID,
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}

	// Admin should read, update, start, stop, message agents
	for _, action := range []Action{ActionRead, ActionUpdate, ActionStart, ActionStop, ActionMessage} {
		decision := f.authz.CheckAccess(ctx, admin, alphaAgentRes, action)
		assert.True(t, decision.Allowed,
			"project admin should have %s access on project agents", action)
	}

	// CURRENT: Admin can delete agents (via project owner/admin bypass).
	// POST-CUTOVER: Admin should NOT be able to delete (project-admin excludes
	// delete action). This is an INTENTIONAL change.
	decision := f.authz.CheckAccess(ctx, admin, alphaAgentRes, ActionDelete)
	assert.True(t, decision.Allowed,
		"CURRENT: project admin CAN delete via bypass (will change post-cutover)")
	// POST-CUTOVER expected: assert.False(t, decision.Allowed)
}

// =============================================================================
// Golden Decision 5: Project member creating agents
// =============================================================================

// TestGolden_ProjectMemberCreateAgents verifies that project members can
// create agents within their project.
//
// Current behavior: The project:<slug>:member-create-agents policy grants
// create, stop_all, message on agent resources scoped to the project.
//
// Intended post-cutover: Same behavior via project-member RoleDefinition
// which includes agent.create, agent.stop_all, agent.message.
//
// Reference: design.md §4, seed.go projectMemberPermissionIDs().
func TestGolden_ProjectMemberCreateAgents(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	member := NewAuthenticatedUser(f.memberAlphaID, "member-alpha@golden.test", "Member Alpha", "member", "api")

	// Creating an agent in alpha project (uses project-scoped create policy)
	alphaAgentRes := Resource{
		Type: "agent", ID: "new-agent",
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}
	decision := f.authz.CheckAccess(ctx, member, alphaAgentRes, ActionCreate)
	assert.True(t, decision.Allowed,
		"project member should be able to create agents in their project")

	// Member should NOT create agents in beta project
	betaAgentRes := Resource{
		Type: "agent", ID: "new-agent-beta",
		ParentType: "project", ParentID: f.projectBeta.ID,
	}
	decision = f.authz.CheckAccess(ctx, member, betaAgentRes, ActionCreate)
	assert.False(t, decision.Allowed,
		"project member should NOT create agents in a different project")
}

// =============================================================================
// Golden Decision 6: Super-admin accessing everything
// =============================================================================

// TestGolden_SuperAdminFullAccess verifies system-wide access for super-admin.
//
// Current behavior: Admin bypass at authz.go:490-495 grants everything.
//
// Intended post-cutover: super-admin RoleDefinition contains all permissions.
// Evaluated through the standard pipeline. Owner/admin roles pass through
// constraints per design doc §5. Behavior is preserved but path changes.
//
// Reference: design.md §5 (evaluator order, no privileged early-allow).
func TestGolden_SuperAdminFullAccess(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	admin := NewAuthenticatedUser(f.superAdminID, "superadmin@golden.test", "Super Admin", "admin", "api")

	// Can access everything in alpha
	alphaAgentRes := Resource{
		Type: "agent", ID: f.agentAlpha.ID,
		OwnerID: f.projectOwnerID,
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}
	for _, action := range []Action{ActionRead, ActionUpdate, ActionDelete, ActionStart, ActionStop, ActionMessage} {
		decision := f.authz.CheckAccess(ctx, admin, alphaAgentRes, action)
		assert.True(t, decision.Allowed,
			"super-admin should have %s access everywhere", action)
		assert.Equal(t, "admin bypass", decision.Reason,
			"CURRENT: super-admin uses admin bypass (will become role binding grant post-cutover)")
	}

	// Can access everything in beta too
	betaAgentRes := Resource{
		Type: "agent", ID: f.agentBeta.ID,
		OwnerID: tid("golden-beta-owner"),
		ParentType: "project", ParentID: f.projectBeta.ID,
	}
	decision := f.authz.CheckAccess(ctx, admin, betaAgentRes, ActionDelete)
	assert.True(t, decision.Allowed, "super-admin should access any project")

	// Hub-level resources too
	hubRes := Resource{Type: "hub", ID: "settings"}
	decision = f.authz.CheckAccess(ctx, admin, hubRes, ActionUpdate)
	assert.True(t, decision.Allowed, "super-admin should access hub settings")
}

// =============================================================================
// Golden Decision 7: Hub admin accessing hub settings
// =============================================================================

// TestGolden_HubAdminHubSettings verifies hub admin access to hub settings.
//
// Current behavior: Hub admin holds hub.settings.read, hub.settings.update
// through the hub-admin role binding. CheckAccess with Permission field
// returns allowed via role binding check at authz.go:576-598.
//
// Intended post-cutover: Same behavior via the standard evaluator pipeline.
// hub-admin RoleDefinition keeps its curated permission set.
//
// Reference: design.md §5, seed.go hubAdminPermissionIDs().
func TestGolden_HubAdminHubSettings(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	hubAdmin := NewAuthenticatedUser(f.hubAdminID, "hubadmin@golden.test", "Hub Admin", "member", "api")

	// Hub admin can read hub settings via role binding
	hubSettingsRes := Resource{Type: "hub", ID: "settings"}
	req := AuthzRequest{
		Principal:  principalContextForIdentity(hubAdmin),
		Credential: credentialContextForIdentity(hubAdmin),
		Resource:   hubSettingsRes,
		Action:     ActionRead,
		Permission: "hub.settings.read",
	}
	decision := f.authz.Decide(ctx, req)
	assert.True(t, decision.Allowed, "hub-admin should read hub settings")
	assert.Equal(t, "role binding grant", decision.Reason)

	// Hub admin can update hub settings
	req.Action = ActionUpdate
	req.Permission = "hub.settings.update"
	decision = f.authz.Decide(ctx, req)
	assert.True(t, decision.Allowed, "hub-admin should update hub settings")

	// Hub admin should NOT be able to suspend users (not in hub-admin permission set)
	req.Permission = "user.suspend"
	req.Action = ActionSuspend
	decision = f.authz.Decide(ctx, req)
	assert.False(t, decision.Allowed,
		"hub-admin should NOT suspend users (super-admin only)")
}

// =============================================================================
// Golden Decision 8: Agent reading its own project resources
// =============================================================================

// TestGolden_AgentReadOwnProject verifies agent read access within its project.
//
// Current behavior: Agent project read baseline at authz.go:682-689 allows
// read+list on resources in the agent's own project.
//
// Intended post-cutover: project-agent RoleBinding with read+list permissions
// at project scope. Behavior preserved.
//
// Reference: design.md §4, authz.go checkAccessForAgent step 3.
func TestGolden_AgentReadOwnProject(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	agent := &evaluateAgentIdentity{id: f.agentAlpha.ID, projectID: f.projectAlpha.ID}

	// Agent can read resources in its own project
	alphaAgentRes := Resource{
		Type: "agent", ID: tid("golden-other-agent"),
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}
	decision := f.authz.CheckAccess(ctx, agent, alphaAgentRes, ActionRead)
	assert.True(t, decision.Allowed)
	assert.Equal(t, "agent project read baseline", decision.Reason,
		"CURRENT: uses baseline code path (will become RoleBinding grant post-cutover)")

	// Agent can list in its own project
	decision = f.authz.CheckAccess(ctx, agent, alphaAgentRes, ActionList)
	assert.True(t, decision.Allowed)
}

// =============================================================================
// Golden Decision 9: Agent accessing cross-project resources
// =============================================================================

// TestGolden_AgentCrossProjectDenied verifies agents cannot access cross-project
// resources.
//
// Current behavior: No policy, no baseline, no ancestry — default deny.
//
// Intended post-cutover: Same — project-scoped agent RoleBinding does not
// reach other projects. Behavior preserved.
func TestGolden_AgentCrossProjectDenied(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	agent := &evaluateAgentIdentity{id: f.agentAlpha.ID, projectID: f.projectAlpha.ID}

	// Agent CANNOT read resources in beta project
	betaRes := Resource{
		Type: "agent", ID: f.agentBeta.ID,
		ParentType: "project", ParentID: f.projectBeta.ID,
	}

	for _, action := range []Action{ActionRead, ActionUpdate, ActionDelete, ActionCreate} {
		decision := f.authz.CheckAccess(ctx, agent, betaRes, action)
		assert.False(t, decision.Allowed,
			"agent should NOT have %s access to cross-project resources", action)
	}
}

// =============================================================================
// Golden Decision 10: Agent with delegation ceiling
// =============================================================================

// TestGolden_AgentDelegationCeiling verifies that delegation ceilings restrict
// agent access.
//
// Current behavior: Delegation ceiling is checked AFTER the primary decision
// in Decide() at authz.go:285-302. For non-read-only operations, a failed
// ceiling check denies.
//
// Intended post-cutover: Credential/delegation ceilings remain intrinsic
// restrictions applied after the union of grants. Behavior preserved.
//
// Reference: design.md §6 (delegation ceilings are relationship constraints).
func TestGolden_AgentDelegationCeiling(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	agent := &evaluateAgentIdentity{id: f.agentAlpha.ID, projectID: f.projectAlpha.ID}

	// Agent can read in its own project via baseline
	ownRes := Resource{
		Type: "agent", ID: tid("golden-proj-resource"),
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}
	decision := f.authz.CheckAccess(ctx, agent, ownRes, ActionRead)
	assert.True(t, decision.Allowed,
		"agent should read own project resources (before ceiling)")

	// Note: Full delegation ceiling testing requires the delegation edge store
	// and agent token claims. The ceiling is applied in Decide() after
	// checkAccessForAgent returns. This test verifies the baseline still works;
	// ceiling enforcement is tested in dedicated delegation test files.
}

// =============================================================================
// Golden Decision 11: Agent accessing progeny secrets
// =============================================================================

// TestGolden_AgentProgenySecretAccess verifies progeny access to secrets.
//
// Current behavior: The progeny-secret-access policy with DelegatedFrom condition
// is matched by checkDelegation (authz.go:781-847). The ancestry chain is walked
// to find the DelegatedFrom principal.
//
// Intended post-cutover: Named lineage/progeny relationship grant. The resolver
// checks the agent's creation chain against the secret's creator without
// using Policy table. Behavior preserved, mechanism changes.
//
// Reference: design.md §6 (progeny access → named relationship grant).
func TestGolden_AgentProgenySecretAccess(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	// Create an agent with projectOwnerID in its ancestry (so DelegatedFrom matches)
	progenyAgent := &store.Agent{
		ID: tid("golden-progeny-agent"), Slug: "golden-progeny-agent",
		Name: "Progeny Agent", ProjectID: f.projectAlpha.ID,
		Phase:   string(state.PhaseRunning),
		OwnerID: f.projectOwnerID,
		// Mark creator for delegation check
		CreatedBy: f.projectOwnerID,
	}
	require.NoError(t, f.store.CreateAgent(ctx, progenyAgent))

	// Use an ancestry-bearing agent identity
	agentIdentity := &testProgenyAgentIdentity{
		id:        progenyAgent.ID,
		projectID: f.projectAlpha.ID,
		ancestry:  []string{f.projectOwnerID},
	}

	secretRes := Resource{
		Type: "secret",
		ID:   f.secretID,
	}
	decision := f.authz.CheckAccess(ctx, agentIdentity, secretRes, ActionRead)
	assert.True(t, decision.Allowed,
		"progeny agent should access secrets created by its ancestor")
	assert.Equal(t, "delegated access", decision.Reason)

	// An agent NOT in the ancestry should be denied
	outsiderAgent := &evaluateAgentIdentity{
		id:        tid("golden-outsider-agent"),
		projectID: f.projectBeta.ID,
	}
	decision = f.authz.CheckAccess(ctx, outsiderAgent, secretRes, ActionRead)
	assert.False(t, decision.Allowed,
		"non-progeny agent should NOT access ancestor's secrets")
}

// =============================================================================
// Golden Decision 12: Agent accessing progeny environment variables
// =============================================================================

// TestGolden_AgentProgenyEnvVarAccess verifies progeny access to env vars.
//
// Current/post-cutover: Same as secrets (item 11). Mechanism changes from
// DelegatedFrom policy to named relationship grant.
//
// Reference: design.md §6.
func TestGolden_AgentProgenyEnvVarAccess(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	agentIdentity := &testProgenyAgentIdentity{
		id:        f.agentAlpha.ID,
		projectID: f.projectAlpha.ID,
		ancestry:  []string{f.projectOwnerID},
	}

	envVarRes := Resource{
		Type: "envvar",
		ID:   f.envVarID,
	}
	decision := f.authz.CheckAccess(ctx, agentIdentity, envVarRes, ActionRead)
	assert.True(t, decision.Allowed,
		"progeny agent should access env vars created by its ancestor")
	assert.Equal(t, "delegated access", decision.Reason)
}

// =============================================================================
// Golden Decision 13: Agent accessing progeny skill injections
// =============================================================================

// TestGolden_AgentProgenySkillInjectionAccess verifies progeny access to skill
// injections.
//
// Current/post-cutover: Same as secrets (item 11). Mechanism changes.
//
// Reference: design.md §6.
func TestGolden_AgentProgenySkillInjectionAccess(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	agentIdentity := &testProgenyAgentIdentity{
		id:        f.agentAlpha.ID,
		projectID: f.projectAlpha.ID,
		ancestry:  []string{f.projectOwnerID},
	}

	skillRes := Resource{
		Type: "skill_injection",
		ID:   f.skillInjectionID,
	}
	decision := f.authz.CheckAccess(ctx, agentIdentity, skillRes, ActionRead)
	assert.True(t, decision.Allowed,
		"progeny agent should access skill injections created by its ancestor")
	assert.Equal(t, "delegated access", decision.Reason)
}

// =============================================================================
// Golden Decision 14: Hub member cross-project visibility regression
// =============================================================================

// TestGolden_CrossProjectVisibilityRegression is the HIGHEST PRIORITY test.
//
// The design doc explicitly identifies that hub-member and hub-viewer roles
// have overly broad permission sets that can reopen cross-project visibility
// (design.md §Immediate sequencing constraint).
//
// The vulnerability: seedRoleDefinitions constructs hub-member from ALL
// read+list permissions (seed.go:628-650), including project.list, project.read,
// agent.list, agent.read. List handlers interpret these as admin-view authority
// and skip per-item filtering (handlers_projects_core.go:212-233,
// handlers_agents_core.go:310-337).
//
// This test verifies the vulnerability exists at the role binding level and
// documents it for correction.
func TestGolden_CrossProjectVisibilityRegression(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	// A hub member with NO project memberships
	user := NewAuthenticatedUser(f.memberNoneID, "member-none@golden.test", "Member None", "member", "api")

	// --- Direct CheckAccess tests (resource-level) ---
	// Direct CheckAccess WITHOUT Permission field correctly denies because
	// there is no policy granting access to beta resources for this user.
	alphaRes := Resource{
		Type: "project", ID: f.projectAlpha.ID,
		OwnerID: f.projectOwnerID,
	}
	decision := f.authz.CheckAccess(ctx, user, alphaRes, ActionRead)
	assert.False(t, decision.Allowed,
		"member-none should NOT directly read project alpha via CheckAccess(action)")

	betaRes := Resource{
		Type: "project", ID: f.projectBeta.ID,
		OwnerID: tid("golden-beta-owner"),
	}
	decision = f.authz.CheckAccess(ctx, user, betaRes, ActionRead)
	assert.False(t, decision.Allowed,
		"member-none should NOT directly read project beta via CheckAccess(action)")

	// --- Role binding permission check (the vulnerability path) ---
	// When the list handler calls Decide with Permission="project.list", the
	// role binding check at authz.go:576-598 evaluates system-scoped role
	// bindings. hub-member holds project.list, so this returns allowed.
	//
	// This is the hasAdminView path that bypasses per-resource filtering.
	adminViewReq := AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "project", ID: "hub"},
		Action:     ActionList,
		Permission: "project.list",
	}
	decision = f.authz.Decide(ctx, adminViewReq)

	// DOCUMENT THE VULNERABILITY:
	// This assertion proves the cross-project visibility gap exists.
	// A hub member with no project memberships gets "allowed" on project.list
	// at hub scope, which list handlers interpret as admin-view authority.
	//
	// POST-CUTOVER: This MUST return false. The hub-member RoleDefinition
	// must be curated to exclude project.list and project.read at system scope.
	// List handlers must use ResolveAuthorizedScopes.
	assert.True(t, decision.Allowed,
		"KNOWN VULNERABILITY: hub-member role binding grants project.list hub-wide")
	assert.Equal(t, "role binding grant", decision.Reason,
		"vulnerability comes from role binding check, not policy")

	// Same vulnerability for agent.list
	agentAdminViewReq := AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "agent", ID: "hub"},
		Action:     ActionList,
		Permission: "agent.list",
	}
	decision = f.authz.Decide(ctx, agentAdminViewReq)
	assert.True(t, decision.Allowed,
		"KNOWN VULNERABILITY: hub-member role binding grants agent.list hub-wide")
	assert.Equal(t, "role binding grant", decision.Reason)

	// --- Verify fix target ---
	// After PG1 curates the hub-member role, these assertions flip:
	// assert.False(t, decision.Allowed)
	// The test documents both the current gap and the fix criteria.
}

// =============================================================================
// Golden Decision 15: Owner/admin bypass behavior
// =============================================================================

// TestGolden_OwnerAdminBypassBehavior documents which current bypasses are
// intentional grants vs. historical accidents.
//
// Reference: design.md §5 (no privileged early-allow path).
func TestGolden_OwnerAdminBypassBehavior(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	t.Run("admin_bypass_is_total_and_unconditional", func(t *testing.T) {
		// CURRENT: admin bypass at authz.go:490-495 is a total bypass for
		// users with Role="admin", regardless of resource, action, or scope.
		// POST-CUTOVER: super-admin RoleDefinition grants all permissions but
		// passes through constraints. This is an INTENTIONAL change — the
		// unconditional bypass is a historical accident per design doc §5.
		admin := NewAuthenticatedUser(f.superAdminID, "superadmin@golden.test", "Super Admin", "admin", "api")

		// Admin can do anything, including actions that should be constraint-controlled
		decision := f.authz.CheckAccess(ctx, admin, Resource{Type: "anything", ID: "whatever"}, ActionDelete)
		assert.True(t, decision.Allowed)
		assert.Equal(t, "admin bypass", decision.Reason,
			"CURRENT: unconditional admin bypass; POST-CUTOVER: standard evaluation")
	})

	t.Run("owner_bypass_grants_all_actions", func(t *testing.T) {
		// CURRENT: owner bypass at authz.go:510-519 grants ALL actions to the
		// resource creator, except ActionAssign on hub-scoped gcp_service_account.
		// POST-CUTOVER: Ownership becomes an explicit RoleBinding or named
		// relationship grant with specific permissions, not a blanket bypass.
		owner := NewAuthenticatedUser(f.projectOwnerID, "proj-owner@golden.test", "Owner", "member", "api")
		ownedRes := Resource{Type: "agent", ID: "owned-agent", OwnerID: f.projectOwnerID}

		decision := f.authz.CheckAccess(ctx, owner, ownedRes, ActionDelete)
		assert.True(t, decision.Allowed)
		assert.Equal(t, "resource owner", decision.Reason,
			"CURRENT: owner bypass is blanket; POST-CUTOVER: specific owner permissions")
	})

	t.Run("project_owner_admin_bypass_is_blanket", func(t *testing.T) {
		// CURRENT: isProjectOwnerOrAdmin at authz.go:533-539 grants ALL actions
		// on ALL project-scoped resources, regardless of the specific action or
		// resource type.
		// POST-CUTOVER: project-owner/admin RoleDefinitions have specific
		// permission sets. This is an INTENTIONAL narrowing for admin
		// (loses delete), but owner retains full project permissions.
		admin := NewAuthenticatedUser(f.projectAdminID, "proj-admin@golden.test", "Admin", "member", "api")
		projRes := Resource{
			Type: "agent", ID: f.agentAlpha.ID,
			ParentType: "project", ParentID: f.projectAlpha.ID,
		}

		// Admin can currently delete (blanket bypass)
		decision := f.authz.CheckAccess(ctx, admin, projRes, ActionDelete)
		assert.True(t, decision.Allowed)
		assert.Equal(t, "project owner/admin", decision.Reason,
			"CURRENT: blanket project bypass; POST-CUTOVER: admin cannot delete")
	})

	t.Run("ancestry_bypass_grants_all_actions", func(t *testing.T) {
		// CURRENT: ancestry bypass at authz.go:521-527 grants ALL actions if
		// the principal appears in resource.Ancestry.
		// POST-CUTOVER: Named relationship grant with the same behavior for
		// creator access; specific progeny grants for delegation.
		user := NewAuthenticatedUser(f.projectOwnerID, "proj-owner@golden.test", "Owner", "member", "api")
		descendantRes := Resource{
			Type: "agent", ID: "descendant",
			Ancestry: []string{f.projectOwnerID, tid("agent-child")},
		}

		decision := f.authz.CheckAccess(ctx, user, descendantRes, ActionDelete)
		assert.True(t, decision.Allowed)
		assert.Equal(t, "ancestor access", decision.Reason)
	})
}

// =============================================================================
// testProgenyAgentIdentity — agent identity with ancestry for progeny tests
// =============================================================================

// testProgenyAgentIdentity is a test-only AgentIdentity that carries an
// ancestry chain. This is needed because evaluateAgentIdentity (the standard
// test helper) returns nil ancestry, which causes delegation/progeny checks
// to fall through.
type testProgenyAgentIdentity struct {
	id        string
	projectID string
	ancestry  []string
}

func (a *testProgenyAgentIdentity) ID() string                    { return a.id }
func (a *testProgenyAgentIdentity) Type() string                  { return "agent" }
func (a *testProgenyAgentIdentity) ProjectID() string             { return a.projectID }
func (a *testProgenyAgentIdentity) Scopes() []AgentTokenScope     { return nil }
func (a *testProgenyAgentIdentity) HasScope(AgentTokenScope) bool { return true }
func (a *testProgenyAgentIdentity) Ancestry() []string            { return a.ancestry }
func (a *testProgenyAgentIdentity) OriginUserID() string {
	if len(a.ancestry) > 0 {
		return a.ancestry[0]
	}
	return ""
}
func (a *testProgenyAgentIdentity) TokenID() string { return "" }
