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

// setupDemoPolicyTest creates a test server with two users and a grove.
// User "alice" is a grove member (grove creator); user "bob" is not.
// Both are hub-members. Returns the server, store, users, and grove.
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

	// Create a grove owned by alice
	grove := &store.Project{
		ID:        "grove-demo",
		Name:      "Demo Project",
		Slug:      "demo-grove",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, grove))

	// Create grove members group and policy (simulates what grove creation handler does)
	srv.createGroveMembersGroupAndPolicy(ctx, grove)

	return srv, s, alice, bob, grove
}

// ============================================================================
// Agent Creation Authorization Tests (Step 4)
// ============================================================================

func TestDemoPolicy_AgentCreate_GroveMemberAllowed(t *testing.T) {
	srv, _, alice, _, grove := setupDemoPolicyTest(t)

	// Alice is a grove member — should pass authorization.
	// Request will fail downstream (no broker/template), but NOT with 403.
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "test-agent",
		ProjectID: grove.ID,
	})
	// Should not be 403 — alice has permission
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"grove member should not get 403; got: %s", rec.Body.String())
}

func TestDemoPolicy_AgentCreate_NonMemberDenied(t *testing.T) {
	srv, _, _, bob, grove := setupDemoPolicyTest(t)

	// Bob is NOT a grove member — should be denied with 403
	rec := doRequestAsUser(t, srv, bob, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "test-agent",
		ProjectID: grove.ID,
	})
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-member should get 403; got: %s", rec.Body.String())
}

func TestDemoPolicy_AgentCreate_AdminBypass(t *testing.T) {
	srv, s, _, _, grove := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create an admin user (not a grove member)
	admin := &store.User{
		ID:          "user-admin",
		Email:       "admin@test.com",
		DisplayName: "Admin",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))

	// Admin should bypass authorization even without grove membership
	rec := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "admin-agent",
		ProjectID: grove.ID,
	})
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"admin should not get 403; got: %s", rec.Body.String())
}

// ============================================================================
// Agent Delete Authorization Tests (Step 5)
// ============================================================================

func TestDemoPolicy_AgentDelete_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, grove := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create an agent owned by alice
	agent := &store.Agent{
		ID:           "agent-del-owner",
		Slug:         "agent-del-owner",
		Name:         "Agent to Delete",
		ProjectID:      grove.ID,
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
		"/api/v1/groves/"+grove.ID+"/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"owner should be able to delete agent; got: %s", rec.Body.String())
}

func TestDemoPolicy_AgentDelete_NonOwnerDenied(t *testing.T) {
	srv, s, alice, bob, grove := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create an agent owned by alice
	agent := &store.Agent{
		ID:           "agent-del-nonowner",
		Slug:         "agent-del-nonowner",
		Name:         "Agent to Delete",
		ProjectID:      grove.ID,
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
		"/api/v1/groves/"+grove.ID+"/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-owner should get 403; got: %s", rec.Body.String())
}

func TestDemoPolicy_AgentDelete_AdminBypass(t *testing.T) {
	srv, s, alice, _, grove := setupDemoPolicyTest(t)
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
		ProjectID:      grove.ID,
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
		"/api/v1/groves/"+grove.ID+"/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code,
		"admin should be able to delete agent; got: %s", rec.Body.String())
}

func TestDemoPolicy_AgentDelete_DirectPath_NonOwnerDenied(t *testing.T) {
	srv, s, alice, bob, grove := setupDemoPolicyTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:           "agent-del-direct",
		Slug:         "agent-del-direct",
		Name:         "Agent Direct Delete",
		ProjectID:      grove.ID,
		OwnerID:      alice.ID,
		CreatedBy:    alice.ID,
		Phase:        string(state.PhaseStopped),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Bob via the non-grove-scoped /api/v1/agents/{id} path
	rec := doRequestAsUser(t, srv, bob, http.MethodDelete,
		"/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-owner should get 403 on direct delete path; got: %s", rec.Body.String())
}

// ============================================================================
// Agent Interaction Authorization Tests (Step 6)
// ============================================================================

func TestDemoPolicy_AgentAction_OwnerAllowed(t *testing.T) {
	srv, s, alice, _, grove := setupDemoPolicyTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:           "agent-action-owner",
		Slug:         "agent-action-owner",
		Name:         "Agent Action Test",
		ProjectID:      grove.ID,
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
				"/api/v1/groves/"+grove.ID+"/agents/"+agent.ID+"/"+action, nil)
			assert.NotEqual(t, http.StatusForbidden, rec.Code,
				"owner should not get 403 for %s; got: %s", action, rec.Body.String())
		})
	}
}

func TestDemoPolicy_AgentAction_NonOwnerDenied(t *testing.T) {
	srv, s, alice, bob, grove := setupDemoPolicyTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:           "agent-action-nonowner",
		Slug:         "agent-action-nonowner",
		Name:         "Agent Action Test",
		ProjectID:      grove.ID,
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
				"/api/v1/groves/"+grove.ID+"/agents/"+agent.ID+"/"+action, nil)
			assert.Equal(t, http.StatusForbidden, rec.Code,
				"non-owner should get 403 for %s; got: %s", action, rec.Body.String())
		})
	}
}

func TestDemoPolicy_AgentAction_AdminBypass(t *testing.T) {
	srv, s, alice, _, grove := setupDemoPolicyTest(t)
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
		ProjectID:      grove.ID,
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
		"/api/v1/groves/"+grove.ID+"/agents/"+agent.ID+"/stop", nil)
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"admin should not get 403; got: %s", rec.Body.String())
}

func TestDemoPolicy_AgentAction_DirectPath_NonOwnerDenied(t *testing.T) {
	srv, s, alice, bob, grove := setupDemoPolicyTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:           "agent-action-direct",
		Slug:         "agent-action-direct",
		Name:         "Agent Direct Action",
		ProjectID:      grove.ID,
		OwnerID:      alice.ID,
		CreatedBy:    alice.ID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Bob via the non-grove-scoped /api/v1/agents/{id}/{action} path
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

	policies, err = s.ListPolicies(ctx, store.PolicyFilter{Name: "hub-member-create-groves"}, store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, policies.TotalCount, "hub-member-create-groves policy should exist")
}

func TestDemoPolicy_GroveCreationSetsUpMembersGroupAndPolicy(t *testing.T) {
	srv, s, alice, _, _ := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create a new grove as alice to trigger the full handler flow
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/groves", map[string]string{
		"name":      "New Test Project",
		"gitRemote": "https://github.com/test/new-grove",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "grove creation should succeed; got: %s", rec.Body.String())

	var createdProject store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&createdProject))

	// Verify grove members group was created
	membersGroup, err := s.GetGroupBySlug(ctx, "grove:"+createdProject.Slug+":members")
	require.NoError(t, err, "grove members group should exist")
	assert.Equal(t, createdProject.Name+" Members", membersGroup.Name)

	// Verify alice is a member of the grove members group
	_, err = s.GetGroupMembership(ctx, membersGroup.ID, store.GroupMemberTypeUser, alice.ID)
	assert.NoError(t, err, "grove creator should be a member of the grove members group")

	// Verify grove-level agent creation policy was created
	policies, err := s.ListPolicies(ctx,
		store.PolicyFilter{Name: "grove:" + createdProject.Slug + ":member-create-agents"},
		store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, policies.TotalCount, "grove member-create-agents policy should exist")
}

// TestDemoPolicy_EndToEnd_GroveCreatorCanCreateAgent tests the complete flow:
// a non-admin user creates a grove via the HTTP API and then creates an agent
// in that grove. This exercises the full handler chain including authorization.
func TestDemoPolicy_EndToEnd_GroveCreatorCanCreateAgent(t *testing.T) {
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

	// Step 1: Create a grove via the HTTP handler (as alice)
	groveRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/groves", CreateProjectRequest{
		Name: "E2E Test Project",
	})
	require.Equal(t, http.StatusCreated, groveRec.Code,
		"grove creation should succeed; got: %s", groveRec.Body.String())

	var grove store.Project
	require.NoError(t, json.NewDecoder(groveRec.Body).Decode(&grove))

	// Step 2: Create an agent in the grove via the HTTP handler (as alice)
	agentRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "e2e-test-agent",
		ProjectID: grove.ID,
	})

	// The agent creation may fail downstream (no broker/template), but should
	// NOT fail with 403 — the grove creator must have permission.
	assert.NotEqual(t, http.StatusForbidden, agentRec.Code,
		"grove creator should not get 403 when creating agent in own grove; got: %s", agentRec.Body.String())
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

// TestDemoPolicy_GroveRecreation_CreatorCanCreateAgent tests that when a grove
// is deleted and recreated with the same slug, the new creator still gets
// permission to create agents. This was a bug where the members group from the
// old grove persisted, causing an "already exists" error that prevented the new
// creator from being added to the group.
func TestDemoPolicy_GroveRecreation_CreatorCanCreateAgent(t *testing.T) {
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

	// Step 1: Create a grove
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/groves", CreateProjectRequest{
		Name: "Recreatable Project",
	})
	require.Equal(t, http.StatusCreated, rec.Code, "first grove creation should succeed")

	var grove1 store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove1))

	// Verify alice can create agents
	agentRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "agent-before-delete",
		ProjectID: grove1.ID,
	})
	assert.NotEqual(t, http.StatusForbidden, agentRec.Code,
		"creator should not get 403 in first grove; got: %s", agentRec.Body.String())

	// Step 2: Delete the grove
	delRec := doRequestAsUser(t, srv, alice, http.MethodDelete, "/api/v1/groves/"+grove1.ID, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code, "grove deletion should succeed")

	// Step 3: Recreate the grove with the same name (same slug)
	rec2 := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/groves", CreateProjectRequest{
		Name: "Recreatable Project",
	})
	require.Equal(t, http.StatusCreated, rec2.Code,
		"recreated grove should succeed; got: %s", rec2.Body.String())

	var grove2 store.Project
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&grove2))

	// Step 4: Verify alice can still create agents in the recreated grove
	agentRec2 := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "agent-after-recreate",
		ProjectID: grove2.ID,
	})
	assert.NotEqual(t, http.StatusForbidden, agentRec2.Code,
		"creator should not get 403 in recreated grove; got: %s", agentRec2.Body.String())
}

// TestDemoPolicy_GroveMembersGroupIdempotent tests that calling
// createGroveMembersGroupAndPolicy twice for the same grove is safe — the
// second call should still ensure the creator is a member.
func TestDemoPolicy_GroveMembersGroupIdempotent(t *testing.T) {
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

	grove := &store.Project{
		ID:        "grove-idempotent",
		Name:      "Idempotent Project",
		Slug:      "idempotent-grove",
		OwnerID:   alice.ID,
		CreatedBy: alice.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, grove))

	// Call twice — second call should not fail or skip adding the user
	srv.createGroveMembersGroupAndPolicy(ctx, grove)
	srv.createGroveMembersGroupAndPolicy(ctx, grove)

	// Verify alice is still a member of the grove members group
	membersGroup, err := s.GetGroupBySlug(ctx, "grove:"+grove.Slug+":members")
	require.NoError(t, err, "grove members group should exist")

	_, err = s.GetGroupMembership(ctx, membersGroup.ID, store.GroupMemberTypeUser, alice.ID)
	assert.NoError(t, err, "alice should be in the members group after idempotent calls")

	// Verify alice can create agents
	agentRec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "agent-idempotent",
		ProjectID: grove.ID,
	})
	assert.NotEqual(t, http.StatusForbidden, agentRec.Code,
		"grove member should not get 403 after idempotent group creation; got: %s", agentRec.Body.String())
}

// TestDemoPolicy_GroveDeleteCleansUpGroupsAndPolicies verifies that deleting
// a grove removes associated groups and policies so they don't leak.
func TestDemoPolicy_GroveDeleteCleansUpGroupsAndPolicies(t *testing.T) {
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

	// Create grove
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/groves", CreateProjectRequest{
		Name: "Cleanup Project",
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var grove store.Project
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&grove))

	// Verify groups and policy exist
	_, err := s.GetGroupBySlug(ctx, "grove:"+grove.Slug+":members")
	require.NoError(t, err, "members group should exist before deletion")

	policies, err := s.ListPolicies(ctx,
		store.PolicyFilter{Name: "grove:" + grove.Slug + ":member-create-agents"},
		store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, policies.TotalCount, "policy should exist before deletion")

	// Delete grove
	delRec := doRequestAsUser(t, srv, alice, http.MethodDelete, "/api/v1/groves/"+grove.ID, nil)
	require.Equal(t, http.StatusNoContent, delRec.Code)

	// Verify groups are cleaned up
	_, err = s.GetGroupBySlug(ctx, "grove:"+grove.Slug+":members")
	assert.Error(t, err, "members group should be deleted after grove deletion")

	// Verify policy is cleaned up
	policies, err = s.ListPolicies(ctx,
		store.PolicyFilter{Name: "grove:" + grove.Slug + ":member-create-agents"},
		store.ListOptions{Limit: 1})
	require.NoError(t, err)
	assert.Equal(t, 0, policies.TotalCount, "policy should be deleted after grove deletion")
}
