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

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// =============================================================================
// ptone/scion#2147 — gcp_service_account.assign in project-owner,
// project-admin and project-member RoleDefinitions.
//
// The project-owner/-admin cases (short-circuit capability agreement) live in
// authz_project_owner_test.go, next to the fixture and the pinned hub-scoped
// control they extend. This file covers the remaining required coverage: the
// project-member case (which does not go through that short-circuit), the
// agent-caller path (AgentScopes + delegation ceiling), exact project
// binding, hub-admin confinement, the project-default HTTP path with a
// non-owned service account, and startup reconciliation of an existing hub.
//
// The immediate-creator IAM actAs check (store.EvaluateActAs, via
// evaluateSAAssignment) is mode-dependent. When gcpIamCheckMode is enforce, it
// runs after the Hub policy decision and can refuse an assignment the role
// permission allows. In the default off mode the role permission alone
// authorizes assignment of project-scoped service accounts.
// TestSAAssign2147_MemberExplicitCreate_ModeCharacterization pins both modes;
// the per-path tests below pin, under enforce, which principal the actAs check
// evaluates. The hub-default rung is hub-scoped and unchanged by this grant.
// =============================================================================

// ---------------------------------------------------------------------------
// Human project-member: CheckAccess(assign) on a project-scoped SA they did
// not register.
// ---------------------------------------------------------------------------

// TestSAAssign2147_ProjectMember_CanAssignUnregisteredProjectSA verifies that
// a plain project member — not the short-circuit path owner/admin use — gets
// ActionAssign on a project-scoped SA registered by someone else in the same
// project, because project-member now carries gcp_service_account.assign.
func TestSAAssign2147_ProjectMember_CanAssignUnregisteredProjectSA(t *testing.T) {
	srv, s, _, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	member := makeProjectMemberUser(t, s, project, tid("2147-plain-member"), "Plain Member", store.GroupMemberRoleMember)

	sa := &store.GCPServiceAccount{
		ID:        tid("2147-member-sa"),
		Scope:     store.ScopeProject,
		ScopeID:   project.ID,
		Email:     "2147-member-sa@example.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: bob.ID, // registered by someone other than member
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	identity := NewAuthenticatedUser(member.ID, member.Email, member.DisplayName, "member", "api")
	resource := gcpServiceAccountResource(sa)

	decision := srv.authzService.CheckAccess(ctx, identity, resource, ActionAssign)
	assert.True(t, decision.Allowed,
		"project member should be allowed to assign a project-scoped SA registered by another member; reason=%q",
		decision.Reason)

	caps := srv.authzService.ComputeCapabilities(ctx, identity, resource)
	assert.Contains(t, caps.Actions, string(ActionAssign),
		"project member's capabilities should agree and list assign")
}

// TestSAAssign2147_OtherProjectMember_Denied pins exact project-binding
// confinement: a member of project A has no standing over a service account
// scoped to project B, regardless of the new permission.
func TestSAAssign2147_OtherProjectMember_Denied(t *testing.T) {
	srv, s, _, _, projectA := setupDemoPolicyTest(t)
	ctx := context.Background()

	projectB := &store.Project{
		ID:        tid("2147-project-b"),
		Name:      "Project B",
		Slug:      "project-b-2147",
		OwnerID:   tid("2147-project-b-owner"),
		CreatedBy: tid("2147-project-b-owner"),
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, projectB))
	srv.seedProjectCreatorMembership(ctx, projectB)

	memberOfA := makeProjectMemberUser(t, s, projectA, tid("2147-member-of-a"), "Member A", store.GroupMemberRoleMember)

	saInB := &store.GCPServiceAccount{
		ID:        tid("2147-sa-in-b"),
		Scope:     store.ScopeProject,
		ScopeID:   projectB.ID,
		Email:     "2147-sa-in-b@example.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: tid("2147-stranger"),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, saInB))

	identity := NewAuthenticatedUser(memberOfA.ID, memberOfA.Email, memberOfA.DisplayName, "member", "api")
	decision := srv.authzService.CheckAccess(ctx, identity, gcpServiceAccountResource(saInB), ActionAssign)
	assert.False(t, decision.Allowed,
		"a member of project A must not be able to assign a service account scoped to project B; reason=%q",
		decision.Reason)
}

// TestSAAssign2147_HubAdmin_NoProjectBinding_Denied verifies that a hub-admin
// (system-scoped RoleDefinition, no project binding at all) still cannot
// assign a project-scoped SA in a project they have no binding to. The
// ruling explicitly excludes hub-admin from this permission; this pins that
// hub-admin's system-scoped role grants nothing here and that IsHubAdmin
// alone is not a route to project-scoped access.
func TestSAAssign2147_HubAdmin_NoProjectBinding_Denied(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("2147-hubadmin-project")
	createDCProject(t, s, projectID, "2147-hubadmin-project")

	hubAdminID := tid("2147-hub-admin")
	createTestUserWithRole(t, s, hubAdminID, "2147-hubadmin@test.com", "member", store.SystemRoleHubAdmin)

	sa := &store.GCPServiceAccount{
		ID:        tid("2147-hubadmin-sa"),
		Scope:     store.ScopeProject,
		ScopeID:   projectID,
		Email:     "2147-hubadmin-sa@example.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: tid("2147-stranger-2"),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	identity := NewAuthenticatedUser(hubAdminID, "2147-hubadmin@test.com", "Hub Admin", "member", "api")
	decision := authz.CheckAccess(ctx, identity, gcpServiceAccountResource(sa), ActionAssign)
	assert.False(t, decision.Allowed,
		"hub-admin with no project binding must not be able to assign a project-scoped SA; reason=%q",
		decision.Reason)
}

// ---------------------------------------------------------------------------
// Agent (AgentRoleFull) callers: AgentScopes maps project:agent:create to
// gcp_service_account.assign, and the delegation ceiling requires the human
// delegator to hold gcp_service_account.assign through their own role
// bindings, which the project-owner, project-admin and project-member roles
// grant.
// ---------------------------------------------------------------------------

// TestSAAssign2147_AgentCreatedByOwner_RealEdge_CanAssignProjectSA covers an
// AgentRoleFull agent created by a live project owner, with the
// delegation-edge backfill marker restored via requireMarkerPresent
// (testServer deletes it by default) and a real user->agent delegation edge
// recorded.
func TestSAAssign2147_AgentCreatedByOwner_RealEdge_CanAssignProjectSA(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	requireMarkerPresent(t, s)

	projectID := tid("2147-agent-owner-project")
	ownerID := tid("2147-agent-owner")
	agentID := tid("2147-agent-owner-agent")

	createDCProject(t, s, projectID, "2147-agent-owner-project")
	createTestUserWithProjectRole(t, s, ownerID, "2147-agent-owner@test.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, ownerID, AgentRoleFull)
	seedRecordedDelegationEdge(t, s, store.DelegationPrincipalUser, ownerID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	sa := &store.GCPServiceAccount{
		ID:        tid("2147-agent-owner-sa"),
		Scope:     store.ScopeProject,
		ScopeID:   projectID,
		Email:     "2147-agent-owner-sa@example.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: tid("2147-agent-owner-stranger"),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	decision := authz.CheckAccess(ctx, agent, gcpServiceAccountResource(sa), ActionAssign)
	assert.True(t, decision.Allowed,
		"an AgentRoleFull agent created by a project owner, with a real delegation edge, "+
			"should be allowed to assign a project-scoped SA; reason=%q", decision.Reason)
}

// TestSAAssign2147_AgentCreatedByMember_RealEdge_CanAssignProjectSA is the
// same, with a plain project-member as the delegator instead of the owner.
func TestSAAssign2147_AgentCreatedByMember_RealEdge_CanAssignProjectSA(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	requireMarkerPresent(t, s)

	projectID := tid("2147-agent-member-project")
	memberID := tid("2147-agent-member")
	agentID := tid("2147-agent-member-agent")

	createDCProject(t, s, projectID, "2147-agent-member-project")
	createTestUserWithProjectRole(t, s, memberID, "2147-agent-member@test.com", projectID, store.ProjectRoleMember)
	createDCAgent(t, s, agentID, projectID, memberID, AgentRoleFull)
	seedRecordedDelegationEdge(t, s, store.DelegationPrincipalUser, memberID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	sa := &store.GCPServiceAccount{
		ID:        tid("2147-agent-member-sa"),
		Scope:     store.ScopeProject,
		ScopeID:   projectID,
		Email:     "2147-agent-member-sa@example.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: tid("2147-agent-member-stranger"),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	decision := authz.CheckAccess(ctx, agent, gcpServiceAccountResource(sa), ActionAssign)
	assert.True(t, decision.Allowed,
		"an AgentRoleFull agent created by a project member, with a real delegation edge, "+
			"should be allowed to assign a project-scoped SA; reason=%q", decision.Reason)
}

// TestSAAssign2147_Agent_HubScopedSA_DenialUnchanged pins that this ticket
// does not touch hub-scoped SA assignment for agent callers: an
// AgentRoleFull agent, real edge, live project-owner delegator, is still
// denied on a hub-scoped (parentless) account. Agent principals never carry
// the hub-members baseline (that grant comes from group membership humans
// have and agents do not), and the project-scoped synthetic binding this
// ticket's permission feeds cannot match a parentless resource either way.
func TestSAAssign2147_Agent_HubScopedSA_DenialUnchanged(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	requireMarkerPresent(t, s)

	projectID := tid("2147-agent-hubscoped-project")
	ownerID := tid("2147-agent-hubscoped-owner")
	agentID := tid("2147-agent-hubscoped-agent")

	createDCProject(t, s, projectID, "2147-agent-hubscoped-project")
	createTestUserWithProjectRole(t, s, ownerID, "2147-agent-hubscoped-owner@test.com", projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, agentID, projectID, ownerID, AgentRoleFull)
	createDCEdge(t, s, store.DelegationPrincipalUser, ownerID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	hubSA := &store.GCPServiceAccount{
		ID:        tid("2147-agent-hubscoped-sa"),
		Scope:     store.ScopeHub,
		ScopeID:   "some-hub-instance",
		Email:     "2147-agent-hubscoped-sa@example.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: tid("2147-agent-hubscoped-stranger"),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, hubSA))

	agent := dcAgentIdentity(agentID, projectID, AgentRoleFull)
	decision := authz.CheckAccess(ctx, agent, gcpServiceAccountResource(hubSA), ActionAssign)
	assert.False(t, decision.Allowed,
		"an agent caller must still be denied assign on a hub-scoped SA; reason=%q", decision.Reason)
}

// requireMarkerPresent restores the delegation-edge backfill completion
// marker that testServer (and therefore authzTestSetup) deletes by default
// for unrelated tests that create agents directly without edges. Without
// this, a bug that dropped the real delegation edge created alongside it
// would fall through to the pre-backfill temporary allow (walkDelegationChain)
// and the test would pass for the wrong reason. Restoring the marker makes
// the edge load-bearing: the ceiling check runs post-backfill, and only the
// edge this ticket's tests create can satisfy it.
func requireMarkerPresent(t *testing.T, s store.Store) {
	t.Helper()
	_, err := s.UpsertHubSetting(context.Background(), "migration_delegation_edge_backfill_v1",
		json.RawMessage(`{"schema_version":1,"completed":true}`), "migration", 0, "seeded")
	require.NoError(t, err)
}

// ---------------------------------------------------------------------------
// Startup reconciliation: an existing hub at the old role revision (3,
// without gcp_service_account.assign) converges to the new set after
// reconcileBuiltInRoles runs.
// ---------------------------------------------------------------------------

func TestSAAssign2147_SeedReconciliation_ExistingHubGetsAssignPermission(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	oldPerms := map[string][]string{
		store.ProjectRoleOwner: {
			"agent.create", "agent.delete", "agent.lifecycle", "agent.list",
			"agent.message", "agent.read", "agent.set_message_mode", "agent.stop_all", "agent.update",
			"harness_config.create", "harness_config.delete", "harness_config.list",
			"harness_config.read", "harness_config.update",
			"project.delete", "project.list", "project.manage", "project.read",
			"project.secret_read", "project.update",
			"scheduled_event.create", "scheduled_event.delete", "scheduled_event.list",
			"scheduled_event.read", "scheduled_event.update",
			"skill.create", "skill.delete", "skill.list", "skill.read", "skill.register", "skill.update",
			"template.create", "template.delete", "template.list", "template.read", "template.update",
		},
		store.ProjectRoleAdmin: {
			"agent.create", "agent.lifecycle", "agent.list", "agent.message", "agent.read",
			"agent.stop_all", "agent.update",
			"harness_config.create", "harness_config.list", "harness_config.read", "harness_config.update",
			"project.list", "project.manage", "project.read", "project.secret_read", "project.update",
			"scheduled_event.create", "scheduled_event.list", "scheduled_event.read", "scheduled_event.update",
			"skill.create", "skill.list", "skill.read", "skill.register", "skill.update",
			"template.create", "template.list", "template.read", "template.update",
		},
		store.ProjectRoleMember: {
			"agent.create", "agent.list", "agent.read",
			"harness_config.create", "harness_config.list", "harness_config.read",
			"project.list", "project.read",
			"scheduled_event.create", "scheduled_event.list", "scheduled_event.read",
			"skill.list", "skill.read",
			"template.create", "template.list", "template.read",
		},
	}

	for roleName, perms := range oldPerms {
		rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, err)
		require.NoError(t, s.UpdateSystemRoleDefinitionPermissions(ctx, rd.ID, perms))
		recordBuiltInRoleMarker(ctx, s, roleName, builtInRoleMarker{
			Revision: 3,
			PermHash: permListHash(perms),
		})

		// Precondition: assign is absent, revision is stale.
		rd, err = s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, err)
		assert.NotContains(t, rd.Permissions, "gcp_service_account.assign",
			"precondition: %s must start without assign", roleName)
		marker := getAppliedBuiltInRoleMarker(ctx, s, roleName)
		assert.Equal(t, 3, marker.Revision, "precondition: %s marker must start at revision 3", roleName)
	}

	// Startup reconciliation, as New() runs it.
	reconcileBuiltInRoles(ctx, s)

	for roleName := range oldPerms {
		rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, err)
		assert.Contains(t, rd.Permissions, "gcp_service_account.assign",
			"%s should have gcp_service_account.assign after reconciliation", roleName)

		marker := getAppliedBuiltInRoleMarker(ctx, s, roleName)
		assert.Equal(t, builtInRoleRevision(t, roleName), marker.Revision,
			"%s marker should advance to its current revision", roleName)
	}
}

// builtInRoleRevision returns the declared revision of a built-in project role.
func builtInRoleRevision(t *testing.T, name string) int {
	t.Helper()
	for _, role := range BuiltInRoles() {
		if role.Name == name && role.ScopeType == store.RoleScopeProject {
			return role.Revision
		}
	}
	t.Fatalf("no built-in project role %q", name)
	return 0
}

// ---------------------------------------------------------------------------
// Project-default HTTP path, end to end, for a service account the caller
// did not register. Companion to TestProjectDefaultGate_CreatorWith(out)ActAsSucceeds
// in project_default_gate_test.go, which use an SA the owner registered
// themselves (satisfied by the resource-owner relationship grant regardless
// of this ticket). Here the SA belongs to a stranger, so the Hub-policy half
// of the gate is satisfied ONLY by the new project-owner permission — and the
// actAs half still decides the outcome, proving the two layers are
// independent per the ruling.
// ---------------------------------------------------------------------------

func TestSAAssign2147_ProjectDefault_OwnerNotSACreator_ActAsStillGates(t *testing.T) {
	t.Run("actAs denied -> request denied", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		sa := wiringSA(t, f.store, store.ScopeProject, f.proj.ID, "2147-default-deny@p.iam.gserviceaccount.com")

		ctx := context.Background()
		proj, err := f.store.GetProject(ctx, f.proj.ID)
		require.NoError(t, err)
		if proj.Annotations == nil {
			proj.Annotations = map[string]string{}
		}
		proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
		proj.Annotations[projectSettingDefaultGCPIdentitySAID] = sa.ID
		require.NoError(t, f.store.UpdateProject(ctx, proj))

		checker := store.NewFakeCallerPermissionChecker().
			DenyTarget(sa.Email, "no actAs grant for this caller")
		enforceSAAssign(f.srv, checker)

		rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "2147-default-deny-agent"})
		require.Equal(t, http.StatusForbidden, rec.Code,
			"Hub policy now allows the owner via gcp_service_account.assign, but actAs must still gate; got: %s",
			rec.Body.String())
		require.Equal(t, 1, checker.CallCount(), "the refusal must come from the actAs checker")
	})

	t.Run("actAs allowed -> request succeeds", func(t *testing.T) {
		f := bypassAgentsSetup(t)
		sa := wiringSA(t, f.store, store.ScopeProject, f.proj.ID, "2147-default-allow@p.iam.gserviceaccount.com")

		ctx := context.Background()
		proj, err := f.store.GetProject(ctx, f.proj.ID)
		require.NoError(t, err)
		if proj.Annotations == nil {
			proj.Annotations = map[string]string{}
		}
		proj.Annotations[projectSettingDefaultGCPIdentityMode] = store.GCPMetadataModeAssign
		proj.Annotations[projectSettingDefaultGCPIdentitySAID] = sa.ID
		require.NoError(t, f.store.UpdateProject(ctx, proj))

		checker := store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email)
		enforceSAAssign(f.srv, checker)

		rec := createAgentAsOwner(t, f, CreateAgentRequest{Name: "2147-default-allow-agent"})
		require.Equal(t, http.StatusCreated, rec.Code,
			"owner should be able to use a project-default SA registered by someone else once actAs allows; got: %s",
			rec.Body.String())
		require.Equal(t, 1, checker.CallCount(), "the actAs checker must be consulted exactly once")
	})
}

// ---------------------------------------------------------------------------
// gcpIamCheckMode characterization and per-path principal selection for a
// plain project member.
//
// Every case below uses a plain project member (not owner or admin) and a
// verified, project-scoped service account registered by someone else
// (wiringSA), so Scion policy is satisfied only by the project-member role's
// gcp_service_account.assign permission. The fake checker records which
// principal each actAs evaluation used.
//
// The hub-default rung is not covered here: a hub default is a hub-scoped
// account, and hub-scoped assignment is unchanged by the project-role grant.
// ---------------------------------------------------------------------------

// memberAssignFixture extends the shared agent-authorization fixture with a
// plain project member of proj, added through the project members group and a
// project-member role binding.
type memberAssignFixture struct {
	*bypassAgentsFixture
	member *store.User
}

func setupMemberAssign(t *testing.T) *memberAssignFixture {
	t.Helper()
	f := bypassAgentsSetup(t)
	f.srv.seedProjectCreatorMembership(context.Background(), f.proj)
	member := makeProjectMemberUser(t, f.store, f.proj, tid("2147-path-member"), "Path Member", store.GroupMemberRoleMember)
	return &memberAssignFixture{bypassAgentsFixture: f, member: member}
}

func (m *memberAssignFixture) createWithSA(t *testing.T, name, saID string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAsUser(t, m.srv, m.member, http.MethodPost, "/api/v1/projects/"+m.proj.ID+"/agents",
		CreateAgentRequest{
			Name: name,
			GCPIdentity: &GCPIdentityAssignment{
				MetadataMode:     store.GCPMetadataModeAssign,
				ServiceAccountID: saID,
			},
		})
}

func (m *memberAssignFixture) agentExists(t *testing.T, name string) bool {
	t.Helper()
	_, err := m.store.GetAgentBySlug(context.Background(), m.proj.ID, name)
	if err == nil {
		return true
	}
	require.ErrorIs(t, err, store.ErrNotFound)
	return false
}

// requireMemberCaller asserts the checker was consulted exactly once, for sa,
// with the member user as the evaluated principal.
func (m *memberAssignFixture) requireMemberCaller(t *testing.T, checker *store.FakeCallerPermissionChecker, sa *store.GCPServiceAccount) {
	t.Helper()
	require.Equal(t, 1, checker.CallCount(), "the actAs checker must be consulted exactly once")
	call := checker.Calls()[0]
	assert.Equal(t, sa.ID, call.TargetSAID)
	assert.Equal(t, store.PrincipalUser, call.Caller.Kind, "the evaluated principal must be the human caller")
	assert.Equal(t, m.member.ID, call.Caller.ID, "the evaluated principal must be the member, not the owner or the hub")
}

// TestSAAssign2147_MemberExplicitCreate_ModeCharacterization pins how
// gcpIamCheckMode shapes a project member's assignment of a project-scoped
// service account they did not register. In off mode (the default) the
// project-member role permission alone authorizes the assignment. In enforce
// mode the member's own iam.serviceAccounts.actAs grant decides it.
func TestSAAssign2147_MemberExplicitCreate_ModeCharacterization(t *testing.T) {
	t.Run("off (default): Scion role permission alone authorizes", func(t *testing.T) {
		m := setupMemberAssign(t)
		require.Equal(t, SAAssignCheckOff, m.srv.saAssignCheckMode,
			"precondition: a server built from DefaultServerConfig runs with gcpIamCheckMode off")
		sa := wiringSA(t, m.store, store.ScopeProject, m.proj.ID, "2147-mode-off@p.iam.gserviceaccount.com")

		rec := m.createWithSA(t, "2147-mode-off", sa.ID)
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		assert.True(t, m.agentExists(t, "2147-mode-off"))
	})

	t.Run("enforce: actAs denial refuses the request", func(t *testing.T) {
		m := setupMemberAssign(t)
		sa := wiringSA(t, m.store, store.ScopeProject, m.proj.ID, "2147-mode-enforce-deny@p.iam.gserviceaccount.com")
		checker := store.NewFakeCallerPermissionChecker().DenyTarget(sa.Email, "caller lacks iam.serviceAccounts.actAs")
		enforceSAAssign(m.srv, checker)

		rec := m.createWithSA(t, "2147-mode-enforce-deny", sa.ID)
		require.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
		require.Equal(t, 1, checker.CallCount(), "the refusal must come from the actAs checker")
		assert.False(t, m.agentExists(t, "2147-mode-enforce-deny"), "no agent may be created when actAs denies")
	})

	t.Run("enforce: actAs allow permits the request", func(t *testing.T) {
		m := setupMemberAssign(t)
		sa := wiringSA(t, m.store, store.ScopeProject, m.proj.ID, "2147-mode-enforce-allow@p.iam.gserviceaccount.com")
		checker := store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email)
		enforceSAAssign(m.srv, checker)

		rec := m.createWithSA(t, "2147-mode-enforce-allow", sa.ID)
		require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
		require.Equal(t, 1, checker.CallCount())
	})
}

// TestSAAssign2147_MemberExplicitCreate_Enforce_EvaluatesMember covers the
// explicit gcp_identity on POST /api/v1/projects/{id}/agents.
func TestSAAssign2147_MemberExplicitCreate_Enforce_EvaluatesMember(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow bool
		want  int
	}{
		{"deny", false, http.StatusForbidden},
		{"allow", true, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := setupMemberAssign(t)
			sa := wiringSA(t, m.store, store.ScopeProject, m.proj.ID, "2147-path-create-"+tc.name+"@p.iam.gserviceaccount.com")
			checker := scriptedChecker(sa, tc.allow)
			enforceSAAssign(m.srv, checker)

			agentName := "2147-path-create-" + tc.name
			rec := m.createWithSA(t, agentName, sa.ID)
			require.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())
			m.requireMemberCaller(t, checker, sa)
			assert.Equal(t, tc.allow, m.agentExists(t, agentName))
		})
	}
}

// TestSAAssign2147_MemberPatch_Enforce_EvaluatesMember covers PATCH of an
// existing agent's gcp_identity. The member owns the agent (agent update
// rights come from ownership); the service account is still stranger-owned.
func TestSAAssign2147_MemberPatch_Enforce_EvaluatesMember(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow bool
		want  int
	}{
		{"deny", false, http.StatusForbidden},
		{"allow", true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := setupMemberAssign(t)
			sa := wiringSA(t, m.store, store.ScopeProject, m.proj.ID, "2147-path-patch-"+tc.name+"@p.iam.gserviceaccount.com")
			agent := &store.Agent{
				ID:        tid("2147-path-patch-" + tc.name),
				Slug:      "2147-path-patch-" + tc.name,
				Name:      "2147-path-patch-" + tc.name,
				ProjectID: m.proj.ID,
				Phase:     string(state.PhaseCreated),
				CreatedBy: m.member.ID,
				OwnerID:   m.member.ID,
			}
			require.NoError(t, m.store.CreateAgent(context.Background(), agent))

			checker := scriptedChecker(sa, tc.allow)
			enforceSAAssign(m.srv, checker)

			rec := doRequestAsUser(t, m.srv, m.member, http.MethodPatch, "/api/v1/agents/"+agent.ID,
				map[string]interface{}{
					"gcp_identity": map[string]interface{}{
						"metadata_mode":      store.GCPMetadataModeAssign,
						"service_account_id": sa.ID,
					},
				})
			require.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())
			m.requireMemberCaller(t, checker, sa)

			got, err := m.store.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			attached := got.AppliedConfig != nil && got.AppliedConfig.GCPIdentity != nil &&
				got.AppliedConfig.GCPIdentity.MetadataMode == store.GCPMetadataModeAssign &&
				got.AppliedConfig.GCPIdentity.ServiceAccountID == sa.ID
			assert.Equal(t, tc.allow, attached, "the identity is attached only when actAs allows")
		})
	}
}

// TestSAAssign2147_MemberProjectDefault_Enforce_EvaluatesMember covers the
// project-default rung of agent create.
func TestSAAssign2147_MemberProjectDefault_Enforce_EvaluatesMember(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow bool
		want  int
	}{
		{"deny", false, http.StatusForbidden},
		{"allow", true, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := setupMemberAssign(t)
			sa := wiringSA(t, m.store, store.ScopeProject, m.proj.ID, "2147-path-default-"+tc.name+"@p.iam.gserviceaccount.com")
			setProjectDefaultSAAnnotations(t, m.bypassAgentsFixture, sa.ID)
			checker := scriptedChecker(sa, tc.allow)
			enforceSAAssign(m.srv, checker)

			agentName := "2147-path-default-" + tc.name
			rec := doRequestAsUser(t, m.srv, m.member, http.MethodPost, "/api/v1/projects/"+m.proj.ID+"/agents",
				CreateAgentRequest{Name: agentName})
			require.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())
			m.requireMemberCaller(t, checker, sa)
			assert.Equal(t, tc.allow, m.agentExists(t, agentName))
		})
	}
}

// TestSAAssign2147_MemberAgentCreatesAgent_Enforce_EvaluatesCreatingSA covers
// agent-creates-agent where the creating agent was delegated by a plain
// project member. The delegation-edge backfill marker is restored and a real
// user->agent edge is recorded, so the delegation ceiling evaluates the
// member's role permissions. The actAs principal is the creating agent's own
// assigned service account.
func TestSAAssign2147_MemberAgentCreatesAgent_Enforce_EvaluatesCreatingSA(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow bool
		want  int
	}{
		{"deny", false, http.StatusForbidden},
		{"allow", true, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := setupMemberAssign(t)
			ctx := context.Background()
			requireMarkerPresent(t, m.store)

			creatingSA := wiringSA(t, m.store, store.ScopeProject, m.proj.ID, "2147-path-creating-"+tc.name+"@p.iam.gserviceaccount.com")
			creator := &store.Agent{
				ID:        tid("2147-path-creator-" + tc.name),
				Slug:      "2147-path-creator-" + tc.name,
				Name:      "2147-path-creator-" + tc.name,
				ProjectID: m.proj.ID,
				Phase:     string(state.PhaseRunning),
				CreatedBy: m.member.ID,
				OwnerID:   m.member.ID,
				Ancestry:  []string{m.member.ID},
				AppliedConfig: &store.AgentAppliedConfig{
					AgentRole: string(AgentRoleFull),
					GCPIdentity: &store.GCPIdentityConfig{
						MetadataMode:        store.GCPMetadataModeAssign,
						ServiceAccountID:    creatingSA.ID,
						ServiceAccountEmail: creatingSA.Email,
					},
				},
			}
			require.NoError(t, m.store.CreateAgent(ctx, creator))
			seedRecordedDelegationEdge(t, m.store, store.DelegationPrincipalUser, m.member.ID, store.DelegationPrincipalAgent, creator.ID,
				store.RoleScopeProject, m.proj.ID, string(AgentRoleFull))

			targetSA := wiringSA(t, m.store, store.ScopeProject, m.proj.ID, "2147-path-target-"+tc.name+"@p.iam.gserviceaccount.com")
			setProjectDefaultSAAnnotations(t, m.bypassAgentsFixture, targetSA.ID)
			checker := scriptedChecker(targetSA, tc.allow)
			enforceSAAssign(m.srv, checker)

			tok, err := m.srv.GetAgentTokenService().GenerateAgentToken(creator.ID, m.proj.ID,
				append([]AgentTokenScope{ScopeProjectRead}, ScopesForRole(AgentRoleFull)...), nil)
			require.NoError(t, err)

			childName := "2147-path-child-" + tc.name
			rec := doRequestWithAgentToken(t, m.srv, http.MethodPost, "/api/v1/projects/"+m.proj.ID+"/agents",
				CreateAgentRequest{Name: childName}, tok)
			require.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())

			require.Equal(t, 1, checker.CallCount(), "the actAs checker must be consulted exactly once")
			call := checker.Calls()[0]
			assert.Equal(t, targetSA.ID, call.TargetSAID)
			assert.Equal(t, store.PrincipalAgent, call.Caller.Kind)
			assert.Equal(t, creator.ID, call.Caller.ID)
			assert.Equal(t, creatingSA.Email, call.Caller.ServiceAccountEmail,
				"the evaluated principal must be the creating agent's assigned service account")
			assert.Equal(t, tc.allow, m.agentExists(t, childName))
		})
	}
}

// TestSAAssign2147_MemberScheduledDispatch_Enforce_EvaluatesScheduleCreator
// covers a dispatch_agent scheduled event created by a plain project member.
// The actAs principal is the principal of the event's latest revision, here
// the member who created it.
func TestSAAssign2147_MemberScheduledDispatch_Enforce_EvaluatesScheduleCreator(t *testing.T) {
	for _, tc := range []struct {
		name  string
		allow bool
	}{
		{"deny", false},
		{"allow", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := setupMemberAssign(t)
			sa := wiringSA(t, m.store, store.ScopeProject, m.proj.ID, "2147-path-sched-"+tc.name+"@p.iam.gserviceaccount.com")
			setProjectDefaultSAAnnotations(t, m.bypassAgentsFixture, sa.ID)
			checker := scriptedChecker(sa, tc.allow)
			enforceSAAssign(m.srv, checker)

			agentName := "2147-path-sched-" + tc.name
			err := m.srv.dispatchAgentEventHandler()(context.Background(), withSessionRevision(store.ScheduledEvent{
				ID:        "evt-" + agentName,
				ProjectID: m.proj.ID,
				EventType: "dispatch_agent",
				Payload:   `{"agentName":"` + agentName + `","task":"scheduled work"}`,
				CreatedBy: m.member.ID,
			}, m.member.ID))
			if tc.allow {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), store.PermissionActAs, "the refusal must come from the actAs gate")
			}
			m.requireMemberCaller(t, checker, sa)
			assert.Equal(t, tc.allow, m.agentExists(t, agentName))
		})
	}
}

// scriptedChecker returns a fake checker that allows or denies sa.
func scriptedChecker(sa *store.GCPServiceAccount, allow bool) *store.FakeCallerPermissionChecker {
	if allow {
		return store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email)
	}
	return store.NewFakeCallerPermissionChecker().DenyTarget(sa.Email, "caller lacks iam.serviceAccounts.actAs")
}

// ---------------------------------------------------------------------------
// UAT scope and project binding through Decide for a project member's
// role-derived gcp_service_account.assign.
// ---------------------------------------------------------------------------

func TestSAAssign2147_MemberUAT_ScopeAndProjectBinding(t *testing.T) {
	authz, s, userID, projectID := uatTestSetup(t)
	ctx := context.Background()

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

	sa := &store.GCPServiceAccount{
		ID:        tid("2147-uat-sa"),
		Scope:     store.ScopeProject,
		ScopeID:   projectID,
		Email:     "2147-uat-sa@example.iam.gserviceaccount.com",
		ProjectID: "gcp-proj",
		CreatedBy: tid("2147-uat-stranger"),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))
	resource := gcpServiceAccountResource(sa)

	t.Run("scope missing assign denies", func(t *testing.T) {
		d := decideAsUAT(ctx, authz, userID, projectID,
			[]string{"gcp_service_account:read", "agent:create"}, resource, ActionAssign, "gcp_service_account.assign")
		assert.False(t, d.Allowed)
		assert.Contains(t, d.Reason, "token does not have scope")
	})

	t.Run("token scoped to another project denies", func(t *testing.T) {
		d := decideAsUAT(ctx, authz, userID, tid("2147-uat-other-project"),
			[]string{"gcp_service_account:assign"}, resource, ActionAssign, "gcp_service_account.assign")
		assert.False(t, d.Allowed)
		assert.Contains(t, d.Reason, "token not scoped for this project")
	})

	t.Run("scope present allows", func(t *testing.T) {
		d := decideAsUAT(ctx, authz, userID, projectID,
			[]string{"gcp_service_account:assign"}, resource, ActionAssign, "gcp_service_account.assign")
		assert.True(t, d.Allowed, "reason=%q", d.Reason)
	})
}
