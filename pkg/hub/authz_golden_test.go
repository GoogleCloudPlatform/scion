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

	"github.com/go-jose/go-jose/v4/jwt"

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
	hubMembersGroup   *store.Group
	alphaMembersGroup *store.Group
	betaMembersGroup  *store.Group
	alphaAgentsGroup  *store.Group
	betaAgentsGroup   *store.Group

	// Users
	superAdminID   string
	hubAdminID     string
	memberAlphaID  string // hub member AND alpha project member
	memberNoneID   string // hub member with NO project memberships
	projectOwnerID string // owner of project alpha
	projectAdminID string // admin of project alpha

	// Agents
	agentAlpha *store.Agent // agent in project alpha
	agentBeta  *store.Agent // agent in project beta

	// Progeny test resources
	secretID         string
	envVarID         string
	skillInjectionID string
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

	// CO1: Legacy per-project member policies removed. Authorization is
	// handled exclusively by RoleBindings and the AK1 kernel. The
	// createTestUserWithProjectRole helper above creates the necessary
	// project-scoped RoleBindings.

	// --- Progeny resources (CO1: relationship grants replace DelegatedFrom policies) ---
	// The RelationshipGrantResolver uses store.ListProgenySecrets/EnvVars/SkillInjections
	// to check progeny access. These store methods filter for AllowProgeny=true and
	// CreatedBy IN ancestry.

	// Secret with progeny access
	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID:           f.secretID,
		Key:          "golden-secret",
		Scope:        "user",
		ScopeID:      f.projectOwnerID,
		AllowProgeny: true,
		CreatedBy:    f.projectOwnerID,
	}))

	// EnvVar with progeny access
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID:           f.envVarID,
		Key:          "golden-envvar",
		Value:        "test-value",
		Scope:        "user",
		ScopeID:      f.projectOwnerID,
		AllowProgeny: true,
		CreatedBy:    f.projectOwnerID,
	}))

	// Skill injection with progeny access
	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		ID:           f.skillInjectionID,
		Scope:        "user",
		ScopeID:      f.projectOwnerID,
		SkillURI:     "test://golden-skill",
		AllowProgeny: true,
		CreatedBy:    f.projectOwnerID,
	}))

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
	assert.Equal(t, "active bindings do not include permission \"project.read\"", decision.Reason)
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
		OwnerID:    f.projectOwnerID,
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}
	decision := f.authz.CheckAccess(ctx, user, alphaAgentRes, ActionRead)
	assert.True(t, decision.Allowed,
		"member should read agents in their own project")

	// Beta agent: member should NOT read (not a project member, no policy)
	betaAgentRes := Resource{
		Type: "agent", ID: f.agentBeta.ID,
		OwnerID:    tid("golden-beta-owner"),
		ParentType: "project", ParentID: f.projectBeta.ID,
	}
	decision = f.authz.CheckAccess(ctx, user, betaAgentRes, ActionRead)
	assert.False(t, decision.Allowed,
		"member should NOT read agents in a project they are not a member of")
	assert.Equal(t, "active bindings do not include permission \"agent.read\"", decision.Reason)
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
		OwnerID:    f.projectOwnerID,
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
		OwnerID:    tid("golden-beta-owner"),
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
		OwnerID:    f.projectOwnerID,
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}

	// Admin should read, update, attach, message agents
	// CO1: start/stop are enforced through ActionAttach, not as independent permissions.
	// ActionMessage is a scope-level permission but project-admin role includes agent.message.
	for _, action := range []Action{ActionRead, ActionUpdate, ActionAttach, ActionMessage} {
		decision := f.authz.CheckAccess(ctx, admin, alphaAgentRes, action)
		assert.True(t, decision.Allowed,
			"project admin should have %s access on project agents", action)
	}

	// CO1 CUTOVER: Admin cannot delete agents — project-admin role excludes
	// delete action. This is an INTENTIONAL restriction.
	decision := f.authz.CheckAccess(ctx, admin, alphaAgentRes, ActionDelete)
	assert.False(t, decision.Allowed,
		"project admin should NOT have delete access (project-admin role excludes delete)")
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
	// CO1: start/stop are enforced through ActionAttach, not as independent permissions.
	// Super-admin access comes through role binding grant, not admin bypass.
	alphaAgentRes := Resource{
		Type: "agent", ID: f.agentAlpha.ID,
		OwnerID:    f.projectOwnerID,
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}
	for _, action := range []Action{ActionRead, ActionUpdate, ActionDelete, ActionAttach, ActionMessage} {
		decision := f.authz.CheckAccess(ctx, admin, alphaAgentRes, action)
		assert.True(t, decision.Allowed,
			"super-admin should have %s access everywhere", action)
		assert.Equal(t, "role binding grant", decision.Reason,
			"CO1: super-admin uses role binding grant (not admin bypass)")
	}

	// Can access everything in beta too
	betaAgentRes := Resource{
		Type: "agent", ID: f.agentBeta.ID,
		OwnerID:    tid("golden-beta-owner"),
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

	agent := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: f.agentAlpha.ID}, ProjectID: f.projectAlpha.ID}}

	// CO1: Agent project read baseline has been removed. Agents without
	// explicit role bindings or JWT scopes that map to agent.read cannot
	// read agent resources. The agent.read permission has no AgentScopes
	// mapping, so synthetic bindings from JWT don't cover it.
	//
	// Note: project.read IS available through ScopeProjectRead, so agents
	// can still read the project resource itself. Agent-to-agent reads
	// require explicit role bindings (future work).
	alphaAgentRes := Resource{
		Type: "agent", ID: tid("golden-other-agent"),
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}
	decision := f.authz.CheckAccess(ctx, agent, alphaAgentRes, ActionRead)
	assert.False(t, decision.Allowed,
		"agent without role binding or scope cannot read other agents")
	assert.Equal(t, "no candidate bindings", decision.Reason)

	// Agent can read the project resource itself via ScopeProjectRead scope
	agentWithScopes := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: f.agentAlpha.ID}, ProjectID: f.projectAlpha.ID,
		Scopes: ScopesForRole(AgentRoleReadOnly),
	}}
	projectRes := Resource{
		Type: "project", ID: f.projectAlpha.ID,
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}
	decision = f.authz.CheckAccess(ctx, agentWithScopes, projectRes, ActionRead)
	assert.True(t, decision.Allowed, "agent with ScopeProjectRead can read own project")
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

	agent := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: f.agentAlpha.ID}, ProjectID: f.projectAlpha.ID}}

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
// agent access that would otherwise be granted.
//
// Current behavior: Delegation ceiling is checked AFTER the primary decision
// in Decide() at authz.go:285-302. When the delegator (user) loses the
// permission that the agent is attempting to exercise, the ceiling denies
// the agent even though the baseline would allow it.
//
// Intended post-cutover: Credential/delegation ceilings remain intrinsic
// restrictions applied after the union of grants. Behavior preserved.
//
// Reference: design.md §6 (delegation ceilings are relationship constraints).
//
// Full delegation ceiling test coverage is in TestDelegationCeiling_* in
// delegation_ceiling_test.go. This golden test demonstrates the ceiling
// restricting access that the baseline would otherwise grant.
func TestGolden_AgentDelegationCeiling(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	// Set up a dedicated user and agent for the ceiling test.
	// The user gets a project-owner role binding granting broad permissions.
	ceilingUserID := tid("golden-ceiling-user")
	ceilingAgentID := tid("golden-ceiling-agent")
	createDCUser(t, f.store, ceilingUserID, "ceiling-user@golden.test",
		f.projectAlpha.ID, store.ProjectRoleOwner)

	// Create the agent record with the user as owner.
	createDCAgent(t, f.store, ceilingAgentID, f.projectAlpha.ID,
		ceilingUserID, AgentRoleFull)

	// Verify the user is NOT a system admin (ceiling test is vacuous otherwise).
	assertNotSystemAdmin(t, f.authz, ctx, ceilingUserID)

	// Create a delegation edge: user → agent (full role).
	createDCEdge(t, f.store,
		store.DelegationPrincipalUser, ceilingUserID,
		store.DelegationPrincipalAgent, ceilingAgentID,
		store.RoleScopeProject, f.projectAlpha.ID, string(AgentRoleFull))

	// Build an agent identity with proper JWT scopes (dcAgentIdentity uses
	// agentIdentityWrapper, which carries real scopes unlike evaluateAgentIdentity).
	agent := dcAgentIdentity(ceilingAgentID, f.projectAlpha.ID, AgentRoleFull)
	// CO1: Use project resource type since agent.read has no AgentScopes mapping.
	// project.read IS covered by ScopeProjectRead which AgentRoleFull includes.
	resource := Resource{
		Type: "project", ID: f.projectAlpha.ID,
		ParentType: "project", ParentID: f.projectAlpha.ID,
	}

	// ALLOWED: agent JWT scopes grant project.read, ceiling passes because
	// user holds the permission via project-owner role binding.
	decision := f.authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed,
		"agent should read own project resources when delegation ceiling passes")

	// Now remove the user's role binding — the user loses the permission
	// that the agent depends on through the delegation chain.
	bindings, err := f.store.ListRoleBindingsForPrincipal(ctx,
		store.RoleBindingPrincipalUser, ceilingUserID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.projectAlpha.ID {
			require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
		}
	}

	// DENIED: baseline would still grant read, but the delegation ceiling
	// now denies because the delegator no longer holds the permission.
	// This is the ceiling restricting access that would otherwise be granted.
	decision = f.authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.False(t, decision.Allowed,
		"agent MUST be denied when delegator loses permission (ceiling enforcement)")
	assert.Contains(t, decision.Reason, "delegator",
		"denial reason should reference the delegator")
}

// =============================================================================
// Golden Decision 10b: Agent allowed via relationship grant + delegation ceiling
// =============================================================================

// TestGolden_AgentRelationshipGrantDelegationCeiling verifies that the delegation
// ceiling is enforced on relationship-grant allows, not just kernel allows.
//
// C-1 fix: previously, Step 9 (checkRelationshipGrants) returned early before
// Step 10 (checkDelegationCeiling) could run, allowing an agent whose delegator
// lost permissions to retain access through ancestor/progeny grants.
//
// Setup: agent in project Alpha accesses a resource in project Beta. The kernel
// denies (synthetic binding is scoped to Alpha), but the agent's ID is in the
// resource's Ancestry, so the ancestor relationship grant fires. A delegation
// edge scoped to Beta + user holding project-owner in Beta lets the ceiling
// pass. Removing the user's Beta binding causes the ceiling to deny.
//
// Reference: design.md §6 frozen decision 4 — delegation ceilings run after
// the union of grants and can only reduce.
func TestGolden_AgentRelationshipGrantDelegationCeiling(t *testing.T) {
	f := newGoldenFixture(t)
	ctx := context.Background()

	ceilingUserID := tid("golden-relceil-user")
	ceilingAgentID := tid("golden-relceil-agent")

	// User gets project-owner in both Alpha and Beta.
	createDCUser(t, f.store, ceilingUserID, "relceil-user@golden.test",
		f.projectAlpha.ID, store.ProjectRoleOwner)
	// Add a second binding for Beta (user already exists from Alpha call).
	betaRD, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleOwner, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: betaRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      ceilingUserID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          f.projectBeta.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	// Create agent in Alpha with the user as owner.
	createDCAgent(t, f.store, ceilingAgentID, f.projectAlpha.ID,
		ceilingUserID, AgentRoleFull)

	assertNotSystemAdmin(t, f.authz, ctx, ceilingUserID)

	// Delegation edge scoped to Beta: user → agent.
	createDCEdge(t, f.store,
		store.DelegationPrincipalUser, ceilingUserID,
		store.DelegationPrincipalAgent, ceilingAgentID,
		store.RoleScopeProject, f.projectBeta.ID, string(AgentRoleFull))

	agent := dcAgentIdentity(ceilingAgentID, f.projectAlpha.ID, AgentRoleFull)

	// Resource in Beta with the AGENT's ID in Ancestry. This triggers:
	//  - Kernel deny: synthetic binding scoped to Alpha, resource in Beta.
	//  - Ancestor relationship grant: agent is in Ancestry.
	//  - Scope restriction passes: project.read is in agent scopes.
	//  - Ceiling: edge scoped to Beta exists, user holds permission in Beta.
	resource := Resource{
		Type: "project", ID: f.projectBeta.ID,
		Ancestry: []string{ceilingAgentID},
	}

	// ALLOWED: relationship grant fires (kernel denied), ceiling passes.
	decision := f.authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.True(t, decision.Allowed,
		"agent should be allowed via ancestry relationship grant when ceiling passes; got reason: %s", decision.Reason)
	assert.Contains(t, decision.Reason, "relationship grant",
		"allow should come from relationship grant, not kernel")

	// Remove user's Beta binding — delegator loses the permission.
	bindings, err := f.store.ListRoleBindingsForPrincipal(ctx,
		store.RoleBindingPrincipalUser, ceilingUserID)
	require.NoError(t, err)
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.projectBeta.ID {
			require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
		}
	}

	// DENIED: relationship grant fires, but the delegation ceiling now
	// denies because the delegator no longer holds the permission. Before
	// the C-1 fix, this would incorrectly return allowed because Step 9
	// returned early before Step 10 (ceiling check).
	decision = f.authz.CheckAccess(ctx, agent, resource, ActionRead)
	assert.False(t, decision.Allowed,
		"agent MUST be denied via ceiling even when allowed by relationship grant (C-1 fix)")
	assert.Contains(t, decision.Reason, "delegator",
		"denial reason should reference the delegator")
}

// =============================================================================
// Golden Decision 11: Agent accessing progeny secrets
// =============================================================================

// TestGolden_AgentProgenySecretAccess verifies progeny access to secrets.
//
// Post-cutover: The relationship resolver correctly identifies progeny access,
// but the agent scope restriction (Step 7) blocks it because "secret.read" is
// not a registered permission with AgentScopes. In production, agent secret
// reads bypass CheckAccess entirely (handlers_env_secrets.go uses direct
// project membership checks). This test documents the CheckAccess-path
// behavior: progeny grant is found but restricted by credential scope.
//
// The relationship grant itself is verified in TestRelationshipGrant_Progeny*.
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

	// Use an ancestry-bearing agent identity (nil scopes → fail-closed restriction)
	agentIdentity := &testProgenyAgentIdentity{
		id:        progenyAgent.ID,
		projectID: f.projectAlpha.ID,
		ancestry:  []string{f.projectOwnerID},
	}

	secretRes := Resource{
		Type: "secret",
		ID:   f.secretID,
	}
	// Agent scope restriction blocks progeny grants through CheckAccess because
	// "secret.read" has no AgentScopes mapping in the permissions registry.
	// The nil-scope agent gets fail-closed restriction which blocks the
	// relationship grant, and the final decision falls through to the kernel
	// denial ("no candidate bindings").
	decision := f.authz.CheckAccess(ctx, agentIdentity, secretRes, ActionRead)
	assert.False(t, decision.Allowed,
		"progeny grant is restricted by agent credential scope in CheckAccess path")
	assert.Equal(t, "no candidate bindings", decision.Reason)

	// An agent NOT in the ancestry should also be denied
	outsiderAgent := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: tid("golden-outsider-agent")}, ProjectID: f.projectBeta.ID}}
	decision = f.authz.CheckAccess(ctx, outsiderAgent, secretRes, ActionRead)
	assert.False(t, decision.Allowed,
		"non-progeny agent should NOT access ancestor's secrets")
}

// =============================================================================
// Golden Decision 12: Agent accessing progeny environment variables
// =============================================================================

// TestGolden_AgentProgenyEnvVarAccess verifies progeny access to env vars.
//
// Post-cutover: Same as secrets (item 11). The relationship resolver identifies
// progeny access, but the agent scope restriction blocks it through CheckAccess
// because "envvar.read" has no AgentScopes mapping. Production agent env var
// reads use direct handler checks, not CheckAccess.
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
	// Agent scope restriction blocks progeny grants through CheckAccess.
	// The nil-scope agent gets fail-closed restriction, and the final decision
	// falls through to the kernel denial.
	decision := f.authz.CheckAccess(ctx, agentIdentity, envVarRes, ActionRead)
	assert.False(t, decision.Allowed,
		"progeny grant is restricted by agent credential scope in CheckAccess path")
	assert.Equal(t, "no candidate bindings", decision.Reason)

	// Negative case: an agent NOT in the ancestry should also be denied
	outsiderAgent := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: tid("golden-outsider-envvar")}, ProjectID: f.projectBeta.ID}}
	decision = f.authz.CheckAccess(ctx, outsiderAgent, envVarRes, ActionRead)
	assert.False(t, decision.Allowed,
		"non-progeny agent should NOT access ancestor's env vars")
}

// =============================================================================
// Golden Decision 13: Agent accessing progeny skill injections
// =============================================================================

// TestGolden_AgentProgenySkillInjectionAccess verifies progeny access to skill
// injections.
//
// Post-cutover: Same as secrets (item 11). The relationship resolver identifies
// progeny access, but the agent scope restriction blocks it through CheckAccess
// because "skill_injection.read" has no AgentScopes mapping.
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
	// Agent scope restriction blocks progeny grants through CheckAccess.
	// The nil-scope agent gets fail-closed restriction, and the final decision
	// falls through to the kernel denial.
	decision := f.authz.CheckAccess(ctx, agentIdentity, skillRes, ActionRead)
	assert.False(t, decision.Allowed,
		"progeny grant is restricted by agent credential scope in CheckAccess path")
	assert.Equal(t, "no candidate bindings", decision.Reason)

	// Negative case: an agent NOT in the ancestry should also be denied
	outsiderAgent := &agentIdentityWrapper{&AgentTokenClaims{Claims: jwt.Claims{Subject: tid("golden-outsider-skill")}, ProjectID: f.projectBeta.ID}}
	decision = f.authz.CheckAccess(ctx, outsiderAgent, skillRes, ActionRead)
	assert.False(t, decision.Allowed,
		"non-progeny agent should NOT access ancestor's skill injections")
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
// PG1 FIX: The hub-member role is now curated with explicit permission lists
// that EXCLUDE project.list, project.read, agent.list, and agent.read at
// system scope. The role binding grant path at authz.go:576-598 no longer
// produces an admin-view grant for ordinary hub members.
//
// This test VERIFIES the vulnerability is closed at the role definition level.
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

	// PG1 FIX: The hub-member RoleDefinition is now curated to exclude
	// project.list and project.read at system scope. A hub member with no
	// project memberships can no longer obtain admin-view access.
	assert.False(t, decision.Allowed,
		"PG1 FIX: hub-member role no longer grants project.list hub-wide")

	// Same fix for agent.list — excluded from hub-member role
	agentAdminViewReq := AuthzRequest{
		Principal:  principalContextForIdentity(user),
		Credential: credentialContextForIdentity(user),
		Resource:   Resource{Type: "agent", ID: "hub"},
		Action:     ActionList,
		Permission: "agent.list",
	}
	decision = f.authz.Decide(ctx, agentAdminViewReq)
	assert.False(t, decision.Allowed,
		"PG1 FIX: hub-member role no longer grants agent.list hub-wide")
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

	t.Run("super_admin_through_standard_evaluation", func(t *testing.T) {
		// CO1 CUTOVER: super-admin goes through standard kernel evaluation.
		// The super-admin RoleDefinition grants all registered permissions.
		// No unconditional bypass — constraints can still restrict super-admin.
		// Note: Resource type must be registered (not arbitrary) because
		// allPermissionIDs() only includes registry permissions.
		admin := NewAuthenticatedUser(f.superAdminID, "superadmin@golden.test", "Super Admin", "admin", "api")

		// Admin can do anything via super-admin role binding (using valid resource type)
		decision := f.authz.CheckAccess(ctx, admin, Resource{Type: "agent", ID: "whatever"}, ActionDelete)
		assert.True(t, decision.Allowed,
			"super-admin should be allowed via super-admin role binding")
	})

	t.Run("owner_access_through_relationship_grants", func(t *testing.T) {
		// CO1 CUTOVER: Resource ownership is a named relationship grant.
		// The owner of a resource gets access through the "resource owner"
		// relationship grant in checkRelationshipGrants. This replaces
		// the old owner bypass with a documented grant path.
		owner := NewAuthenticatedUser(f.projectOwnerID, "proj-owner@golden.test", "Owner", "member", "api")
		ownedRes := Resource{Type: "agent", ID: "owned-agent", OwnerID: f.projectOwnerID}

		// Owner gets access through relationship grant: resource owner
		decision := f.authz.CheckAccess(ctx, owner, ownedRes, ActionDelete)
		assert.True(t, decision.Allowed,
			"resource owner should be allowed via relationship grant")
		assert.Equal(t, "relationship grant: resource owner", decision.Reason)
	})

	t.Run("project_admin_cannot_delete", func(t *testing.T) {
		// CO1 CUTOVER: project-admin RoleDefinition has specific permission
		// sets. Admin intentionally loses delete action. Owner retains full
		// project permissions.
		admin := NewAuthenticatedUser(f.projectAdminID, "proj-admin@golden.test", "Admin", "member", "api")
		projRes := Resource{
			Type: "agent", ID: f.agentAlpha.ID,
			ParentType: "project", ParentID: f.projectAlpha.ID,
		}

		// Admin cannot delete (project-admin role excludes delete)
		decision := f.authz.CheckAccess(ctx, admin, projRes, ActionDelete)
		assert.False(t, decision.Allowed,
			"project admin should NOT have delete access (role excludes delete)")
	})

	t.Run("ancestor_access_through_relationship_grants", func(t *testing.T) {
		// CO1 CUTOVER: Ancestor access is handled by the RelationshipGrantResolver
		// (RG1). The canAccessAsAncestor check still runs as a relationship grant.
		user := NewAuthenticatedUser(f.projectOwnerID, "proj-owner@golden.test", "Owner", "member", "api")
		descendantRes := Resource{
			Type: "agent", ID: "descendant",
			Ancestry: []string{f.projectOwnerID, tid("agent-child")},
		}

		decision := f.authz.CheckAccess(ctx, user, descendantRes, ActionDelete)
		assert.True(t, decision.Allowed,
			"ancestor should retain access through relationship grants")
		assert.Equal(t, "relationship grant: ancestor access", decision.Reason)
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

// =============================================================================
// C1 Regression: Members-group owner cannot escalate to project-owner
// =============================================================================

// TestGolden_C1_GroupOwnerCannotEscalateToProjectOwner is a regression test for
// the C1 security fix. It verifies that a user who is the "owner" of a project's
// members group but has NO project-owner RoleBinding cannot perform owner-level
// actions (e.g., deleting the project or agents within it).
//
// Before C1, backfillProjectRoleBindings() would infer a project-owner RoleBinding
// from the group membership role, creating a privilege-escalation vector.
//
// After C1, project role bindings must be granted explicitly — group membership
// role has no effect on authorization.
//
// Reference: wave2-xl-findings-for-co1.md §C1
func TestGolden_C1_GroupOwnerCannotEscalateToProjectOwner(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("c1-project")
	realOwnerID := tid("c1-real-owner")
	groupOwnerID := tid("c1-group-owner")

	// Create the project
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "C1 Test Project", Slug: "c1-test-project",
		OwnerID: realOwnerID,
	}))

	// Create real owner user with project-owner role binding
	createTestUserWithRole(t, s, realOwnerID, "real-owner@c1.test", "member", store.SystemRoleHubMember)
	createTestUserWithProjectRole(t, s, realOwnerID, "real-owner@c1.test", projectID, store.ProjectRoleOwner)

	// Create the group-owner user — member of hub but with NO project-level role binding
	createTestUserWithRole(t, s, groupOwnerID, "group-owner@c1.test", "member", store.SystemRoleHubMember)

	// Create the project members group and make groupOwnerID the "owner" of it
	membersGroup := &store.Group{
		ID: api.NewUUID(), Name: "C1 Project Members",
		Slug: "project:c1-test-project:members", GroupType: store.GroupTypeExplicit,
		ProjectID: projectID,
	}
	require.NoError(t, s.CreateGroup(ctx, membersGroup))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: membersGroup.ID, MemberID: groupOwnerID,
		MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleOwner,
	}))

	// Also give the group-owner a project-member role binding (the most they
	// should get via normal membership assignment).
	createTestUserWithProjectRole(t, s, groupOwnerID, "group-owner@c1.test", projectID, store.ProjectRoleMember)

	// Create a test agent in the project
	agentID := tid("c1-agent")
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "c1-agent", Name: "C1 Agent",
		ProjectID: projectID, Phase: "running", OwnerID: realOwnerID,
	}))

	groupOwnerUser := NewAuthenticatedUser(groupOwnerID, "group-owner@c1.test", "Group Owner", "member", "api")

	// Project resource
	projectRes := Resource{Type: "project", ID: projectID, OwnerID: realOwnerID}

	// Agent resource
	agentRes := Resource{
		Type: "agent", ID: agentID,
		OwnerID:    realOwnerID,
		ParentType: "project", ParentID: projectID,
	}

	// The group-owner should NOT be able to delete the project (owner-only action)
	decision := authz.CheckAccess(ctx, groupOwnerUser, projectRes, ActionDelete)
	assert.False(t, decision.Allowed,
		"C1 regression: members-group owner must NOT escalate to project delete")

	// The group-owner should NOT be able to delete agents (owner-level action)
	decision = authz.CheckAccess(ctx, groupOwnerUser, agentRes, ActionDelete)
	assert.False(t, decision.Allowed,
		"C1 regression: members-group owner must NOT escalate to agent delete")

	// The group-owner SHOULD be able to read (they have project-member role binding)
	decision = authz.CheckAccess(ctx, groupOwnerUser, agentRes, ActionRead)
	assert.True(t, decision.Allowed,
		"C1: members-group owner with member role binding should still read")

	// Verify the real owner CAN do everything (positive control)
	realOwnerUser := NewAuthenticatedUser(realOwnerID, "real-owner@c1.test", "Real Owner", "member", "api")
	decision = authz.CheckAccess(ctx, realOwnerUser, projectRes, ActionDelete)
	assert.True(t, decision.Allowed,
		"C1 positive control: actual project owner should delete project")
}

// =============================================================================
// C-2 regression test: AccessConstraints applied to ResolveListScopes
// =============================================================================

// TestResolveListScopes_AccessConstraintExcludesListPermission verifies that an
// active AccessConstraint excluding project.list from a principal causes
// ResolveListScopes to return ScopeSetNone.
//
// C-2 fix: ResolveListScopes previously never loaded or applied
// AccessConstraints, so a principal whose project.list was removed by an
// operator boundary still received full list visibility.
func TestResolveListScopes_AccessConstraintExcludesListPermission(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	userID := tid("c2-list-user")
	projectID := tid("c2-list-project")

	// Create user + project + project-member binding (which includes project.list).
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: "c2list@test.com", DisplayName: "C2 List User",
		Role: "member", Status: "active",
	}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Slug: "c2-list-project", Name: "C2 List Project",
	}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	user := NewAuthenticatedUser(userID, "c2list@test.com", "C2 List User", "member", "api")

	// Without constraint: user should see the project.
	scopes := authz.ResolveListScopes(ctx, user, "project.list")
	assert.False(t, scopes.IsNone(),
		"without constraint, user with project-member binding should have list visibility")
	assert.True(t, scopes.Contains(projectID),
		"user should see their project")

	// Create an AccessConstraint that excludes project.list for all principals.
	// MaximumPermissions is an allowlist — NOT including project.list means it's excluded.
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name:               "c2-deny-project-list",
		SubjectKind:        store.ConstraintSubjectAllPrincipals,
		ScopeType:          store.RoleScopeSystem,
		MaximumPermissions: []string{"agent.read"}, // project.list NOT included
		Purpose:            "C-2 regression test: exclude project.list",
		CreatedBy:          "test",
	})
	require.NoError(t, err)

	// With constraint: user should get ScopeSetNone because the constraint
	// excludes project.list at system scope.
	scopes = authz.ResolveListScopes(ctx, user, "project.list")
	assert.True(t, scopes.IsNone(),
		"C-2 fix: active boundary excluding project.list must return empty list scope")
}
