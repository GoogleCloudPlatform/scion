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
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStopAllAgents_Global(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:   tid("project-1"),
		Name: "Test Project",
		Slug: "test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create running agents
	for i, name := range []string{tid("agent-1"), tid("agent-2"), tid("agent-3")} {
		agent := &store.Agent{
			ID:        name,
			Slug:      name,
			Name:      name,
			ProjectID: project.ID,
			Phase:     string(state.PhaseRunning),
		}
		if i == 2 {
			// agent-3 is already stopped
			agent.Phase = string(state.PhaseStopped)
		}
		require.NoError(t, s.CreateAgent(ctx, agent))
	}

	t.Run("stops all running agents", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/stop-all", nil)
		assert.Equal(t, http.StatusOK, rec.Code)

		var resp StopAllAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		assert.Equal(t, 2, resp.Stopped)
		assert.Equal(t, 0, resp.Failed)
		assert.Equal(t, 2, resp.Total)

		// Verify agents are stopped in store
		for _, name := range []string{tid("agent-1"), tid("agent-2")} {
			agent, err := s.GetAgent(ctx, name)
			require.NoError(t, err)
			assert.Equal(t, string(state.PhaseStopped), agent.Phase)
		}
	})

	t.Run("returns empty when no running agents", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/stop-all", nil)
		assert.Equal(t, http.StatusOK, rec.Code)

		var resp StopAllAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		assert.Equal(t, 0, resp.Total)
	})

	t.Run("requires POST method", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/stop-all", nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})

	t.Run("requires authentication", func(t *testing.T) {
		rec := doRequestNoAuth(t, srv, http.MethodPost, "/api/v1/agents/stop-all", nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}

func TestStopAllAgents_ProjectScoped(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create two projects
	project1 := &store.Project{ID: tid("project-1"), Name: "Project 1", Slug: tid("project-1")}
	project2 := &store.Project{ID: tid("project-2"), Name: "Project 2", Slug: tid("project-2")}
	require.NoError(t, s.CreateProject(ctx, project1))
	require.NoError(t, s.CreateProject(ctx, project2))

	// Create running agents in both projects
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("g1-agent-1"), Slug: tid("g1-agent-1"), Name: "G1 Agent 1",
		ProjectID: project1.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("g1-agent-2"), Slug: tid("g1-agent-2"), Name: "G1 Agent 2",
		ProjectID: project1.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("g2-agent-1"), Slug: tid("g2-agent-1"), Name: "G2 Agent 1",
		ProjectID: project2.ID, Phase: string(state.PhaseRunning),
	}))

	t.Run("stops only agents in scoped project", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project1.ID+"/agents/stop-all", nil)
		assert.Equal(t, http.StatusOK, rec.Code)

		var resp StopAllAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		assert.Equal(t, 2, resp.Stopped)
		assert.Equal(t, 0, resp.Failed)
		assert.Equal(t, 2, resp.Total)

		// Verify project-1 agents are stopped
		a1, _ := s.GetAgent(ctx, tid("g1-agent-1"))
		assert.Equal(t, string(state.PhaseStopped), a1.Phase)

		// Verify project-2 agent is still running
		a2, _ := s.GetAgent(ctx, tid("g2-agent-1"))
		assert.Equal(t, string(state.PhaseRunning), a2.Phase)
	})
}

func TestStopAllAgents_ScopeCapabilities(t *testing.T) {
	srv, _ := testServer(t)

	// The stop_all action should appear in scope capabilities for admin users
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Capabilities *Capabilities `json:"_capabilities"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Capabilities)
	assert.Contains(t, resp.Capabilities.Actions, "stop_all")
}

// ============================================================================
// Role-Based Stop-All Tests
// ============================================================================

func TestStopAllAgents_ProjectOwner_StopsAllAgents(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create running agents owned by different users
	permSeedUser(t, ctx, s, tid("user-other"))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("alice-agent"), Slug: tid("alice-agent"), Name: "Alice Agent",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("other-agent"), Slug: tid("other-agent"), Name: "Other Agent",
		ProjectID: project.ID, OwnerID: tid("user-other"), Phase: string(state.PhaseRunning),
	}))

	// Alice is project owner — should stop ALL agents, scope = "all"
	rec := doRequestAsUser(t, srv, alice, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, 2, resp.Stopped)
	assert.Equal(t, 0, resp.Failed)
	assert.Equal(t, "all", resp.Scope)

	// Verify both agents are stopped
	a1, _ := s.GetAgent(ctx, tid("alice-agent"))
	assert.Equal(t, string(state.PhaseStopped), a1.Phase)
	a2, _ := s.GetAgent(ctx, tid("other-agent"))
	assert.Equal(t, string(state.PhaseStopped), a2.Phase)
}

func TestStopAllAgents_ProjectMember_StopsOnlyOwnAgents(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create a third user "carol" as a regular project member
	carol := createStopAllTestUser(t, s, "user-carol")

	// Add carol as a project-member role binding through the members API
	alice, err := s.GetUser(ctx, tid("user-alice"))
	require.NoError(t, err)
	addProjectMemberViaAPI(t, srv, s, alice, project.ID, store.RoleBindingPrincipalUser, carol.ID, store.ProjectRoleMember)

	// Create agents owned by carol and by alice
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("carol-agent-1"), Slug: tid("carol-agent-1"), Name: "Carol Agent 1",
		ProjectID: project.ID, OwnerID: carol.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("carol-agent-2"), Slug: tid("carol-agent-2"), Name: "Carol Agent 2",
		ProjectID: project.ID, OwnerID: carol.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("alice-agent"), Slug: tid("alice-agent"), Name: "Alice Agent",
		ProjectID: project.ID, OwnerID: tid("user-alice"), Phase: string(state.PhaseRunning),
	}))

	// Carol (regular member) should only stop her own agents, scope = "own"
	rec := doRequestAsUser(t, srv, carol, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, 2, resp.Stopped)
	assert.Equal(t, 0, resp.Failed)
	assert.Equal(t, "own", resp.Scope)

	// Verify carol's agents are stopped
	c1, _ := s.GetAgent(ctx, tid("carol-agent-1"))
	assert.Equal(t, string(state.PhaseStopped), c1.Phase)
	c2, _ := s.GetAgent(ctx, tid("carol-agent-2"))
	assert.Equal(t, string(state.PhaseStopped), c2.Phase)

	// Verify alice's agent is still running
	a1, _ := s.GetAgent(ctx, tid("alice-agent"))
	assert.Equal(t, string(state.PhaseRunning), a1.Phase)
}

func TestStopAllAgents_NonMember_Forbidden(t *testing.T) {
	srv, s, _, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create a running agent in the project
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("agent-1"), Slug: tid("agent-1"), Name: "Agent 1",
		ProjectID: project.ID, Phase: string(state.PhaseRunning),
	}))

	// Bob is NOT a project member — should get 403
	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Agent should still be running
	a, _ := s.GetAgent(ctx, tid("agent-1"))
	assert.Equal(t, string(state.PhaseRunning), a.Phase)
}

func TestStopAllAgents_Global_NonAdmin_Forbidden(t *testing.T) {
	srv, _, alice, _, _ := setupDemoPolicyTest(t)

	// Alice is a regular user (not platform admin) — global stop-all should be denied
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents/stop-all", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestStopAllAgents_ScopeCapabilities_ProjectOwner(t *testing.T) {
	srv, _, alice, bob, project := setupDemoPolicyTest(t)

	// Alice (project owner) should see stop_all in project-scoped capabilities
	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/agents", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Capabilities *Capabilities `json:"_capabilities"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Capabilities)
	assert.Contains(t, resp.Capabilities.Actions, "stop_all",
		"project owner should have stop_all in scope capabilities")

	// Bob (non-member) should not reach this endpoint at all: listProjectAgents
	// now requires agent.list on the project (fix-1908 F1 -- this route used
	// to have no authorization check for a user identity, so a non-member
	// used to get 200 back with an empty scope-capabilities list; the
	// stronger, correct outcome is that they are denied the endpoint
	// entirely, which trivially also means they never see "stop_all").
	rec = doRequestAsUser(t, srv, bob, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/agents", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-member should be denied the project agent list outright; got: %s", rec.Body.String())
}

// memberRequestOption adjusts the POST /members request body built by
// addProjectMemberViaAPI.
type memberRequestOption func(*addProjectMemberRequest)

// withNotBefore makes the new role binding inactive until notBefore.
func withNotBefore(notBefore time.Time) memberRequestOption {
	return func(req *addProjectMemberRequest) { req.NotBefore = &notBefore }
}

// addProjectMemberViaAPI grants the principal (a user or a group) the named
// built-in project role by calling POST /api/v1/projects/{id}/members as
// actor, which creates a role binding (the membership source of truth).
func addProjectMemberViaAPI(t *testing.T, srv *Server, s store.Store, actor *store.User, projectID, principalType, principalID, roleName string, opts ...memberRequestOption) {
	t.Helper()
	rd, err := s.GetRoleDefinitionByName(context.Background(), roleName, store.RoleScopeProject)
	require.NoError(t, err)
	req := addProjectMemberRequest{
		RoleDefinitionID: rd.ID,
		PrincipalType:    principalType,
		PrincipalID:      principalID,
	}
	for _, opt := range opts {
		opt(&req)
	}
	rec := doRequestAsUser(t, srv, actor, http.MethodPost,
		"/api/v1/projects/"+projectID+"/members", req)
	require.Equal(t, http.StatusCreated, rec.Code, "add member: %s", rec.Body.String())
}

func TestStopAllAgents_ProjectAdminViaMembersAPI_StopsAllAgents(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	addProjectMemberViaAPI(t, srv, s, alice, project.ID, store.RoleBindingPrincipalUser, bob.ID, store.ProjectRoleAdmin)

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("alice-agent"), Slug: tid("alice-agent"), Name: "Alice Agent",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("bob-agent"), Slug: tid("bob-agent"), Name: "Bob Agent",
		ProjectID: project.ID, OwnerID: bob.ID, Phase: string(state.PhaseRunning),
	}))

	// Bob holds project-admin through a role binding only (he is not in the
	// project's legacy members group) and so has agent.stop_all.
	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "all", resp.Scope)
	assert.Equal(t, 2, resp.Stopped)

	for _, id := range []string{tid("alice-agent"), tid("bob-agent")} {
		a, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseStopped), a.Phase, id)
	}

	// The project agent list advertises stop_all to the admin.
	assert.Contains(t, projectAgentListActions(t, srv, bob, project.ID), "stop_all",
		"project admin should have stop_all in scope capabilities")

	// A plain project member is not advertised stop_all.
	carol := createStopAllTestUser(t, s, "user-carol")
	addProjectMemberViaAPI(t, srv, s, alice, project.ID, store.RoleBindingPrincipalUser, carol.ID, store.ProjectRoleMember)
	assert.NotContains(t, projectAgentListActions(t, srv, carol, project.ID), "stop_all",
		"project member should not have stop_all in scope capabilities")
}

func TestStopAllAgents_MembersGroupOnly_Forbidden(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Bob is in an explicit group carrying the project's ID but holds no
	// project role binding, so he is not a project member.
	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    membersGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   bob.ID,
		Role:       store.GroupMemberRoleOwner,
	}))

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("alice-agent"), Slug: tid("alice-agent"), Name: "Alice Agent",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("bob-agent"), Slug: tid("bob-agent"), Name: "Bob Agent",
		ProjectID: project.ID, OwnerID: bob.ID, Phase: string(state.PhaseRunning),
	}))

	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	for _, id := range []string{tid("alice-agent"), tid("bob-agent")} {
		a, err := s.GetAgent(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseRunning), a.Phase, id)
	}
}

// projectAgentListActions returns the scope capability actions that
// GET /api/v1/projects/{id}/agents reports for user.
func projectAgentListActions(t *testing.T, srv *Server, user *store.User, projectID string) []string {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodGet,
		"/api/v1/projects/"+projectID+"/agents", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp struct {
		Capabilities *Capabilities `json:"_capabilities"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Capabilities)
	return resp.Capabilities.Actions
}

// createStopAllTestUser creates an active hub member with the given ID suffix.
func createStopAllTestUser(t *testing.T, s store.Store, name string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID:          tid(name),
		Email:       name + "@test.com",
		DisplayName: name,
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, u))
	ensureHubMembership(ctx, s, u.ID)
	return u
}

func TestStopAllAgents_GroupProjectMember_StopsOnlyOwnAgents(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Bob belongs to a group that holds project-member on the project; he
	// has no direct binding.
	groupID := tid("stopall-team")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "stopall-team", Name: "Stop-all Team",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   bob.ID,
		Role:       store.GroupMemberRoleMember,
	}))
	addProjectMemberViaAPI(t, srv, s, alice, project.ID, store.RoleBindingPrincipalGroup, groupID, store.ProjectRoleMember)

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("alice-agent"), Slug: tid("alice-agent"), Name: "Alice Agent",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("bob-agent"), Slug: tid("bob-agent"), Name: "Bob Agent",
		ProjectID: project.ID, OwnerID: bob.ID, Phase: string(state.PhaseRunning),
	}))

	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "own", resp.Scope)
	assert.Equal(t, 1, resp.Stopped)

	b, err := s.GetAgent(ctx, tid("bob-agent"))
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), b.Phase)
	a, err := s.GetAgent(ctx, tid("alice-agent"))
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), a.Phase)

	// The project agent list does not advertise stop_all to a member whose
	// role comes from a group binding.
	assert.NotContains(t, projectAgentListActions(t, srv, bob, project.ID), "stop_all",
		"group-derived project member should not have stop_all in scope capabilities")
}

func TestStopAllAgents_InactiveMemberBinding_Forbidden(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Bob's direct project-member binding is not active until tomorrow.
	addProjectMemberViaAPI(t, srv, s, alice, project.ID, store.RoleBindingPrincipalUser, bob.ID,
		store.ProjectRoleMember, withNotBefore(time.Now().Add(24*time.Hour)))

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: tid("bob-agent"), Slug: tid("bob-agent"), Name: "Bob Agent",
		ProjectID: project.ID, OwnerID: bob.ID, Phase: string(state.PhaseRunning),
	}))

	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	a, err := s.GetAgent(ctx, tid("bob-agent"))
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), a.Phase)
}

// seedStopAllAgents creates one running agent owned by each of the given
// users in the project, with ID tid("<user ID>-agent").
func seedStopAllAgents(t *testing.T, s store.Store, projectID string, owners ...*store.User) {
	t.Helper()
	for _, u := range owners {
		id := tid(u.ID + "-agent")
		require.NoError(t, s.CreateAgent(context.Background(), &store.Agent{
			ID: id, Slug: id, Name: u.DisplayName + " agent",
			ProjectID: projectID, OwnerID: u.ID, Phase: string(state.PhaseRunning),
		}))
	}
}

// assertStopAllAgentPhase asserts the phase of each user's seeded agent.
func assertStopAllAgentPhase(t *testing.T, s store.Store, phase state.Phase, owners ...*store.User) {
	t.Helper()
	for _, u := range owners {
		a, err := s.GetAgent(context.Background(), tid(u.ID+"-agent"))
		require.NoError(t, err)
		assert.Equal(t, string(phase), a.Phase, u.ID)
	}
}

func TestStopAllAgents_ExpiredMemberBinding_Forbidden(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// The members API rejects an ExpiresAt in the past, so seed the expired
	// direct project-member binding through the store.
	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	expired := time.Now().Add(-time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      bob.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		ExpiresAt:        &expired,
		CreatedBy:        alice.ID,
	})
	require.NoError(t, err)
	seedStopAllAgents(t, s, project.ID, bob)

	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assertStopAllAgentPhase(t, s, state.PhaseRunning, bob)
}

func TestStopAllAgents_GroupProjectAdmin_StopsAllAgents(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Bob holds project-admin only through a group binding.
	groupID := tid("stopall-admins")
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Slug: "stopall-admins", Name: "Stop-all Admins",
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groupID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   bob.ID,
		Role:       store.GroupMemberRoleMember,
	}))
	addProjectMemberViaAPI(t, srv, s, alice, project.ID, store.RoleBindingPrincipalGroup, groupID, store.ProjectRoleAdmin)
	seedStopAllAgents(t, s, project.ID, alice, bob)

	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp StopAllAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "all", resp.Scope)
	assert.Equal(t, 2, resp.Stopped)
	assertStopAllAgentPhase(t, s, state.PhaseStopped, alice, bob)

	// The project agent list advertises stop_all to an admin whose role
	// comes from a group binding.
	assert.Contains(t, projectAgentListActions(t, srv, bob, project.ID), "stop_all",
		"group-derived project admin should have stop_all in scope capabilities")
}

func TestStopAllAgents_CustomProjectRoleOnly_Forbidden(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// A custom (non-built-in) project role ranks 0 in higherProjectRole, the
	// same as no role, so it does not make the caller a member for the
	// owner-scoped stop-all.
	custom, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "stopall-custom-viewer",
		Description: "custom project role for stop-all tests",
		ScopeType:   store.RoleScopeProject,
		Permissions: []string{"project.read"},
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: custom.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      bob.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          project.ID,
		CreatedBy:        alice.ID,
	})
	require.NoError(t, err)
	seedStopAllAgents(t, s, project.ID, bob)

	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assertStopAllAgentPhase(t, s, state.PhaseRunning, bob)
}

func TestStopAllAgents_MembershipStoreError_InternalError(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)

	addProjectMemberViaAPI(t, srv, s, alice, project.ID, store.RoleBindingPrincipalUser, bob.ID, store.ProjectRoleMember)
	seedStopAllAgents(t, s, project.ID, bob)

	// Fail the membership lookup's group read. The authz service keeps its
	// own store reference, so only the handler's membership resolution sees
	// the fault.
	srv.store = &errorInjectingStore{Store: s, getEffectiveGroupsErr: errors.New("injected store fault")}

	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "failed to resolve project membership")
	assert.NotContains(t, rec.Body.String(), "injected store fault")
	assertStopAllAgentPhase(t, s, state.PhaseRunning, bob)
}

func TestStopAllAgents_NilMembershipService_InternalError(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)

	addProjectMemberViaAPI(t, srv, s, alice, project.ID, store.RoleBindingPrincipalUser, bob.ID, store.ProjectRoleMember)
	seedStopAllAgents(t, s, project.ID, bob)

	// New always wires the membership service; a nil one is a wiring fault
	// and must be a 500, not a misleading "not a member" 403.
	srv.membershipService = nil

	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "membership service unavailable")
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeInternalError, errResp.Error.Code)
	assertStopAllAgentPhase(t, s, state.PhaseRunning, bob)
}

func TestStopAllAgents_MemberWithScopedToken(t *testing.T) {
	ctx := context.Background()

	// setup makes bob a project member of the demo project and a project
	// admin of a second project, with one running agent owned by bob in
	// the demo project.
	setup := func(t *testing.T) (*Server, store.Store, *store.User, *store.Project, *store.Project) {
		srv, s, alice, bob, project := setupDemoPolicyTest(t)
		addProjectMemberViaAPI(t, srv, s, alice, project.ID, store.RoleBindingPrincipalUser, bob.ID, store.ProjectRoleMember)
		other := &store.Project{
			ID: tid("project-other"), Name: "Other Project", Slug: "other-project",
			OwnerID: alice.ID, CreatedBy: alice.ID,
		}
		require.NoError(t, s.CreateProject(ctx, other))
		rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
		require.NoError(t, err)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      bob.ID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          other.ID,
			CreatedBy:        alice.ID,
		})
		require.NoError(t, err)
		seedStopAllAgents(t, s, project.ID, bob)
		return srv, s, bob, project, other
	}

	stopAgentPath := func(project *store.Project, bob *store.User) string {
		return "/api/v1/projects/" + project.ID + "/agents/" + tid(bob.ID+"-agent") + "/stop"
	}

	t.Run("read-only token", func(t *testing.T) {
		srv, s, bob, project, _ := setup(t)
		key := mintScopedUAT(t, srv, bob.ID, project.ID, []string{"project:read"})

		// The per-agent route refuses this token.
		rec := doRequestWithUAT(t, srv, key, http.MethodPost, stopAgentPath(project, bob), nil)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		rec = doRequestWithUAT(t, srv, key, http.MethodPost,
			"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertStopAllAgentPhase(t, s, state.PhaseRunning, bob)
	})

	t.Run("token for another project", func(t *testing.T) {
		srv, s, bob, project, other := setup(t)
		key := mintScopedUAT(t, srv, bob.ID, other.ID, []string{"agent:lifecycle"})

		rec := doRequestWithUAT(t, srv, key, http.MethodPost, stopAgentPath(project, bob), nil)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		rec = doRequestWithUAT(t, srv, key, http.MethodPost,
			"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertStopAllAgentPhase(t, s, state.PhaseRunning, bob)
	})
}
