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
	"bytes"
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

// doRequestAsUser creates a user token and performs an HTTP request as that user.
func doRequestAsUser(t *testing.T, srv *Server, user *store.User, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	var bodyBytes []byte
	if body != nil {
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}

	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// setupDemoPolicyTest creates a test server with two users and a project.
// User "alice" is a project member (project creator); user "bob" is not.
// Both are hub-members. Returns the server, store, users, and project.
func setupDemoPolicyTest(t *testing.T) (*Server, store.Store, *store.User, *store.User, *store.Project) {
	t.Helper()

	srv, s := testServer(t)
	ctx := context.Background()

	// Create users
	alice := &store.User{
		ID:          "user-alice",
		Email:       "alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))

	bob := &store.User{
		ID:          "user-bob",
		Email:       "bob@test.com",
		DisplayName: "Bob",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, bob))

	// Add both to hub-members group (simulates login)
	ensureHubMembership(ctx, s, alice.ID)
	ensureHubMembership(ctx, s, bob.ID)

	// Create a project owned by alice
	project := &store.Project{
		ID:        "project-demo",
		Name:      "Demo Project",
		Slug:      "demo-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create project members group and policy (simulates what project creation handler does)
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	return srv, s, alice, bob, project
}

// ============================================================================
// Agent Creation Authorization Tests (Step 4)
// ============================================================================

func TestDemoPolicy_AgentCreate_ProjectMemberAllowed(t *testing.T) {
	srv, _, alice, _, project := setupDemoPolicyTest(t)

	// Alice is a project member — should pass authorization.
	// Request will fail downstream (no broker/template), but NOT with 403.
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "test-agent",
		ProjectID: project.ID,
	})
	// Should not be 403 — alice has permission
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"project member should not get 403; got: %s", rec.Body.String())
}

func TestDemoPolicy_AgentCreate_NonMemberDenied(t *testing.T) {
	srv, _, _, bob, project := setupDemoPolicyTest(t)

	// Bob is NOT a project member — should be denied with 403
	rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "test-agent",
		ProjectID: project.ID,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-member should get 403; got: %s", rec.Body.String())
}

func TestDemoPolicy_AgentCreate_AdminBypass(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create an admin user (not a project member)
	admin := &store.User{
		ID:          "user-admin",
		Email:       "admin@test.com",
		DisplayName: "Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))

	// Admin should bypass authorization even without project membership
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "admin-agent",
		ProjectID: project.ID,
	})
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"admin should not get 403; got: %s", rec.Body.String())
}

// ============================================================================
// Agent Delete Authorization Tests (Step 5)
// ============================================================================

func TestDemoPolicy_AgentDelete_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create an agent owned by alice
	agent := &store.Agent{
		ID:           "agent-del-owner",
		Slug:         "agent-del-owner",
		Name:         "Agent to Delete",
		ProjectID:      project.ID,
		OwnerID:      alice.ID,
		CreatedBy:    alice.ID,
		Phase:        string(state.PhaseStopped),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Alice (owner) should be able to delete
	rec := doRequestAsUser(t, srv, alice, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"owner should be able to delete agent; got: %s", rec.Body.String())
}

func TestDemoPolicy_AgentDelete_NonOwnerDenied(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create an agent owned by alice
	agent := &store.Agent{
		ID:           "agent-del-nonowner",
		Slug:         "agent-del-nonowner",
		Name:         "Agent to Delete",
		ProjectID:      project.ID,
		OwnerID:      alice.ID,
		CreatedBy:    alice.ID,
		Phase:        string(state.PhaseStopped),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Bob (not the owner) should be denied
	rec := doRequestAsUser(t, srv, bob, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-owner should get 403; got: %s", rec.Body.String())
}

func TestDemoPolicy_AgentDelete_AdminBypass(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	admin := &store.User{
		ID:          "user-admin-del",
		Email:       "admin-del@test.com",
		DisplayName: "Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))

	agent := &store.Agent{
		ID:           "agent-del-admin",
		Slug:         "agent-del-admin",
		Name:         "Agent for Admin Delete",
		ProjectID:      project.ID,
		OwnerID:      alice.ID,
		CreatedBy:    alice.ID,
		Phase:        string(state.PhaseStopped),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Admin (not the owner) should bypass and be able to delete
	rec := doRequestAsUser(t, srv, admin, http.MethodDelete,
		"/api/v1/projects/"+project.ID+"/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"admin should be able to delete agent; got: %s", rec.Body.String())
}

func TestDemoPolicy_AgentDelete_DirectPath_NonOwnerDenied(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:           "agent-del-direct",
		Slug:         "agent-del-direct",
		Name:         "Agent Direct Delete",
		ProjectID:      project.ID,
		OwnerID:      alice.ID,
		CreatedBy:    alice.ID,
		Phase:        string(state.PhaseStopped),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Bob via the non-project-scoped /api/v1/agents/{id} path
	rec := doRequestAsUser(t, srv, bob, http.MethodDelete,
		"/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-owner should get 403 on direct delete path; got: %s", rec.Body.String())
}

// ============================================================================
// Agent Interaction Authorization Tests (Step 6)
// ============================================================================

func TestDemoPolicy_AgentAction_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:           "agent-action-owner",
		Slug:         "agent-action-owner",
		Name:         "Agent Action Test",
		ProjectID:      project.ID,
		OwnerID:      alice.ID,
		CreatedBy:    alice.ID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Alice (owner) should pass authorization for lifecycle actions.
	// The action may fail downstream (no broker), but should NOT get 403.
	for _, action := range []string{"start", "stop", "restart"} {
		t.Run(action, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, alice, http.MethodPost,
				"/api/v1/projects/"+project.ID+"/agents/"+agent.ID+"/"+action, nil)
			assert.NotEqual(t, http.StatusForbidden, rec.Code,
				"owner should not get 403 for %s; got: %s", action, rec.Body.String())
		})
	}
}

func TestDemoPolicy_AgentAction_NonOwnerDenied(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:           "agent-action-nonowner",
		Slug:         "agent-action-nonowner",
		Name:         "Agent Action Test",
		ProjectID:      project.ID,
		OwnerID:      alice.ID,
		CreatedBy:    alice.ID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Bob (not the owner) should be denied for interactive actions
	for _, action := range []string{"start", "stop", "restart", "message"} {
		t.Run(action, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, bob, http.MethodPost,
				"/api/v1/projects/"+project.ID+"/agents/"+agent.ID+"/"+action, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code,
				"non-owner should get 403 for %s; got: %s", action, rec.Body.String())
		})
	}
}

func TestDemoPolicy_AgentAction_AdminBypass(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	admin := &store.User{
		ID:          "user-admin-action",
		Email:       "admin-action@test.com",
		DisplayName: "Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))

	agent := &store.Agent{
		ID:           "agent-action-admin",
		Slug:         "agent-action-admin",
		Name:         "Agent Admin Action",
		ProjectID:      project.ID,
		OwnerID:      alice.ID,
		CreatedBy:    alice.ID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Admin should bypass authorization for all actions
	rec := doRequestAsUser(t, srv, admin, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/"+agent.ID+"/stop", nil)
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"admin should not get 403; got: %s", rec.Body.String())
}

func TestDemoPolicy_AgentAction_DirectPath_NonOwnerDenied(t *testing.T) {
	srv, s, alice, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:           "agent-action-direct",
		Slug:         "agent-action-direct",
		Name:         "Agent Direct Action",
		ProjectID:      project.ID,
		OwnerID:      alice.ID,
		CreatedBy:    alice.ID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Bob via the non-project-scoped /api/v1/agents/{id}/{action} path
	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/agents/"+agent.ID+"/start", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-owner should get 403 on direct action path; got: %s", rec.Body.String())
}

// ============================================================================
// Seed Groups and Policies Tests
// ============================================================================

func TestDemoPolicy_SeedGroupsAndPolicies(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Verify hub-members group was created
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)
	assert.Equal(t, "Hub Members", group.Name)
	assert.Equal(t, store.GroupTypeExplicit, group.GroupType)

	// Verify seed policies exist
	policies, err := s.ListPolicies(ctx, store.PolicyFilter{Name: "hub-member-read-all"}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, policies.TotalCount, "hub-member-read-all policy should exist")

	policies, err = s.ListPolicies(ctx, store.PolicyFilter{Name: "hub-member-create-projects"}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, policies.TotalCount, "hub-member-create-projects policy should exist")
}

func TestDemoPolicy_ProjectCreationSetsUpMembersGroupAndPolicy(t *testing.T) {
	srv, s, alice, _, _ := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create a new project as alice to trigger the full handler flow
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/projects", map[string]string{
		"name":      "New Test Project",
		"gitRemote": "https://github.com/test/new-project",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "project creation should succeed; got: %s", rec.Body.String())

	var createdProject store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&createdProject))

	// Verify project members group was created
	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+createdProject.Slug+":members")
	require.NoError(t, err, "project members group should exist")
	assert.Equal(t, createdProject.Name+" Members", membersGroup.Name)

	// Verify alice is a member of the project members group
	_, err = s.GetGroupMembership(ctx, membersGroup.ID, store.GroupMemberTypeUser, alice.ID)
	assert.NoError(t, err, "project creator should be a member of the project members group")

	// Verify project-level agent creation policy was created
	policies, err := s.ListPolicies(ctx,
		store.PolicyFilter{Name: "project:" + createdProject.Slug + ":member-create-agents"},
		store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, policies.TotalCount, "project member-create-agents policy should exist")
}

// TestDemoPolicy_EndToEnd_ProjectCreatorCanCreateAgent tests the complete flow:
// a non-admin user creates a project via the HTTP API and then creates an agent
// in that project. This exercises the full handler chain including authorization.
func TestDemoPolicy_EndToEnd_ProjectCreatorCanCreateAgent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a non-admin user
	alice := &store.User{
		ID:          "user-e2e-alice",
		Email:       "e2e-alice@test.com",
		DisplayName: "E2E Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	ensureHubMembership(ctx, s, alice.ID)

	// Step 1: Create a project via the HTTP handler (as alice)
	projectRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name: "E2E Test Project",
	})
	require.Equal(t, http.StatusCreated, projectRec.Code,
		"project creation should succeed; got: %s", projectRec.Body.String())

	var project store.Project
	require.NoError(t, json.NewDecoder(projectRec.Body).Decode(&project))

	// Step 2: Create an agent in the project via the HTTP handler (as alice)
	agentRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "e2e-test-agent",
		ProjectID: project.ID,
	})

	// The agent creation may fail downstream (no broker/template), but should
	// NOT fail with 403 — the project creator must have permission.
	assert.NotEqual(t, http.StatusForbidden, agentRec.Code,
		"project creator should not get 403 when creating agent in own project; got: %s", agentRec.Body.String())
}

func TestDemoPolicy_HubMembershipOnLogin(t *testing.T) {
	_, s := testServer(t)
	ctx := context.Background()

	// Create a user and add to hub-members (simulating login)
	user := &store.User{
		ID:          "user-login-test",
		Email:       "login@test.com",
		DisplayName: "Login User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	// Verify user is in hub-members group
	group, err := s.GetGroupBySlug(ctx, "hub-members")
	require.NoError(t, err)

	_, err = s.GetGroupMembership(ctx, group.ID, store.GroupMemberTypeUser, user.ID)
	assert.NoError(t, err, "user should be in hub-members group after ensureHubMembership")

	// Calling again should be idempotent (no error)
	ensureHubMembership(ctx, s, user.ID)
}

// TestDemoPolicy_ProjectRecreation_CreatorCanCreateAgent tests that when a project
// is deleted and recreated with the same slug, the new creator still gets
// permission to create agents. This was a bug where the members group from the
// old project persisted, causing an "already exists" error that prevented the new
// creator from being added to the group.
func TestDemoPolicy_ProjectRecreation_CreatorCanCreateAgent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	alice := &store.User{
		ID:          "user-recreate-alice",
		Email:       "recreate-alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	ensureHubMembership(ctx, s, alice.ID)

	// Step 1: Create a project
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name: "Recreatable Project",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "first project creation should succeed")

	var project1 store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project1))

	// Verify alice can create agents
	agentRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "agent-before-delete",
		ProjectID: project1.ID,
	})
	assert.NotEqual(t, http.StatusForbidden, agentRec.Code,
		"creator should not get 403 in first project; got: %s", agentRec.Body.String())

	// Step 2: Delete the project
	delRec := doRequestAsUser(t, srv, alice, http.MethodDelete, "/api/v1/projects/"+project1.ID, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code, "project deletion should succeed")

	// Step 3: Recreate the project with the same name (same slug)
	rec2 := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name: "Recreatable Project",
	})
	require.Equal(t, http.StatusCreated, rec2.Code,
		"recreated project should succeed; got: %s", rec2.Body.String())

	var project2 store.Project
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&project2))

	// Step 4: Verify alice can still create agents in the recreated project
	agentRec2 := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "agent-after-recreate",
		ProjectID: project2.ID,
	})
	assert.NotEqual(t, http.StatusForbidden, agentRec2.Code,
		"creator should not get 403 in recreated project; got: %s", agentRec2.Body.String())
}

// TestDemoPolicy_ProjectMembersGroupIdempotent tests that calling
// createProjectMembersGroupAndPolicy twice for the same project is safe — the
// second call should still ensure the creator is a member.
func TestDemoPolicy_ProjectMembersGroupIdempotent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	alice := &store.User{
		ID:          "user-idempotent-alice",
		Email:       "idempotent-alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	ensureHubMembership(ctx, s, alice.ID)

	project := &store.Project{
		ID:        "project-idempotent",
		Name:      "Idempotent Project",
		Slug:      "idempotent-project",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Call twice — second call should not fail or skip adding the user
	srv.createProjectMembersGroupAndPolicy(ctx, project)
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// Verify alice is still a member of the project members group
	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err, "project members group should exist")

	_, err = s.GetGroupMembership(ctx, membersGroup.ID, store.GroupMemberTypeUser, alice.ID)
	assert.NoError(t, err, "alice should be in the members group after idempotent calls")

	// Verify alice can create agents
	agentRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "agent-idempotent",
		ProjectID: project.ID,
	})
	assert.NotEqual(t, http.StatusForbidden, agentRec.Code,
		"project member should not get 403 after idempotent group creation; got: %s", agentRec.Body.String())
}

// TestDemoPolicy_ProjectDeleteCleansUpGroupsAndPolicies verifies that deleting
// a project removes associated groups and policies so they don't leak.
func TestDemoPolicy_ProjectDeleteCleansUpGroupsAndPolicies(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	alice := &store.User{
		ID:          "user-cleanup-alice",
		Email:       "cleanup-alice@test.com",
		DisplayName: "Alice",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, alice))
	ensureHubMembership(ctx, s, alice.ID)

	// Create project
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/projects", CreateProjectRequest{
		Name: "Cleanup Project",
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var project store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&project))

	// Verify groups and policy exist
	_, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err, "members group should exist before deletion")

	policies, err := s.ListPolicies(ctx,
		store.PolicyFilter{Name: "project:" + project.Slug + ":member-create-agents"},
		store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, policies.TotalCount, "policy should exist before deletion")

	// Delete project
	delRec := doRequestAsUser(t, srv, alice, http.MethodDelete, "/api/v1/projects/"+project.ID, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code)

	// Verify groups are cleaned up
	_, err = s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	assert.Error(t, err, "members group should be deleted after project deletion")

	// Verify policy is cleaned up
	policies, err = s.ListPolicies(ctx,
		store.PolicyFilter{Name: "project:" + project.Slug + ":member-create-agents"},
		store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 0, policies.TotalCount, "policy should be deleted after project deletion")
}
