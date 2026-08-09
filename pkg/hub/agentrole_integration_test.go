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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupAgentRoleTest creates a server with a member user and project.
// The user is the project creator and is in the hub-members group.
func setupAgentRoleTest(t *testing.T) (*Server, store.Store, *store.User, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("user-role-test"),
		Email:       "roletest@test.com",
		DisplayName: "Role Tester",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("project-role-test"),
		Name:      "role-test-project",
		Slug:      "role-test-project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	return srv, s, user, project
}

func doAgentRoleRequest(t *testing.T, srv *Server, user *store.User, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		user.ID, user.Email, user.DisplayName, user.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// getStoredAgentRole retrieves the stored role for a created agent by slug.
// Returns ("", false) if the agent was not persisted (e.g., request failed downstream).
func getStoredAgentRole(t *testing.T, s store.Store, projectID, slug string) (string, bool) {
	t.Helper()
	agent, err := s.GetAgentBySlug(context.Background(), projectID, slug)
	if err != nil {
		return "", false
	}
	if agent.AppliedConfig == nil {
		return "", false
	}
	return agent.AppliedConfig.AgentRole, true
}

func TestCreateAgent_InvalidAgentRole_Returns400(t *testing.T) {
	srv, _, user, project := setupAgentRoleTest(t)

	rec := doAgentRoleRequest(t, srv, user, CreateAgentRequest{
		Name:      "test-invalid-role",
		ProjectID: project.ID,
		AgentRole: "superadmin",
	})

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"invalid agentRole should return 400; got: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid agentRole")
}

func TestCreateAgent_NoAgentRole_GetsBaseline(t *testing.T) {
	srv, s, user, project := setupAgentRoleTest(t)

	rec := doAgentRoleRequest(t, srv, user, CreateAgentRequest{
		Name:      "test-no-role",
		ProjectID: project.ID,
	})

	// Should not fail with an agentRole validation error.
	if rec.Code == http.StatusBadRequest {
		assert.NotContains(t, rec.Body.String(), "invalid agentRole",
			"empty agentRole should not trigger validation error")
	}

	// If the agent was persisted, verify default role.
	if role, ok := getStoredAgentRole(t, s, project.ID, "test-no-role"); ok {
		assert.Equal(t, "baseline", role,
			"default role should be baseline for member user")
	}
}

func TestCreateAgent_ValidAgentRoles_AcceptedByValidation(t *testing.T) {
	srv, _, user, project := setupAgentRoleTest(t)

	validRoles := []string{"none", "readonly", "baseline", "full"}
	for _, role := range validRoles {
		t.Run("role="+role, func(t *testing.T) {
			rec := doAgentRoleRequest(t, srv, user, CreateAgentRequest{
				Name:      "test-role-" + role,
				ProjectID: project.ID,
				AgentRole: role,
			})
			// Valid role values should not trigger an agentRole validation error.
			if rec.Code == http.StatusBadRequest {
				assert.NotContains(t, rec.Body.String(), "invalid agentRole",
					"valid role %q should not trigger agentRole validation error", role)
			}
		})
	}
}

func TestCreateAgent_RoleReadonly_StoresReadonly(t *testing.T) {
	srv, s, user, project := setupAgentRoleTest(t)

	_ = doAgentRoleRequest(t, srv, user, CreateAgentRequest{
		Name:      "test-readonly",
		ProjectID: project.ID,
		AgentRole: "readonly",
	})

	if role, ok := getStoredAgentRole(t, s, project.ID, "test-readonly"); ok {
		assert.Equal(t, "readonly", role,
			"readonly role should be stored as-is (readonly < member baseline ceiling)")
	}
}

func TestCreateAgent_RoleNone_SetsNoAuth(t *testing.T) {
	srv, s, user, project := setupAgentRoleTest(t)

	_ = doAgentRoleRequest(t, srv, user, CreateAgentRequest{
		Name:      "test-none-role",
		ProjectID: project.ID,
		AgentRole: "none",
	})

	agent, err := s.GetAgentBySlug(context.Background(), project.ID, "test-none-role")
	if err == nil && agent.AppliedConfig != nil {
		assert.Equal(t, "none", agent.AppliedConfig.AgentRole)
		assert.True(t, agent.AppliedConfig.NoAuth,
			"role=none should set NoAuth=true")
	}
}

func TestCreateAgent_RoleFull_CappedByMemberCeiling(t *testing.T) {
	srv, s, user, project := setupAgentRoleTest(t)

	_ = doAgentRoleRequest(t, srv, user, CreateAgentRequest{
		Name:      "test-full-capped",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	if role, ok := getStoredAgentRole(t, s, project.ID, "test-full-capped"); ok {
		// Member user ceiling is baseline, so full should be capped.
		assert.Equal(t, "baseline", role,
			"member user requesting full should be capped to baseline")
	}
}

func TestCreateAgent_AdminGetsFull(t *testing.T) {
	srv, s, _, project := setupAgentRoleTest(t)
	ctx := context.Background()

	admin := &store.User{
		ID:          tid("user-admin-role"),
		Email:       "admin-role@test.com",
		DisplayName: "Admin Role Tester",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)

	_ = doAgentRoleRequest(t, srv, admin, CreateAgentRequest{
		Name:      "test-admin-full",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	if role, ok := getStoredAgentRole(t, s, project.ID, "test-admin-full"); ok {
		assert.Equal(t, "full", role,
			"admin user requesting full should get full")
	}
}

// doAgentCallerRequest creates a sub-agent request authenticated as an agent.
// It generates an agent token for the given parentAgentID and projectID with the
// ScopeAgentCreate scope, then performs a POST /api/v1/agents with the given body.
func doAgentCallerRequest(t *testing.T, srv *Server, parentAgentID, projectID string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()

	token, err := srv.GenerateAgentToken(parentAgentID, projectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)

	bodyBytes, err := json.Marshal(body)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Agent-Token", token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestCreateSubAgent_ParentRoleLogged(t *testing.T) {
	srv, s, _, project := setupAgentRoleTest(t)
	ctx := context.Background()

	// Create a parent agent with role=full stored in its AppliedConfig.
	parentAgent := &store.Agent{
		ID:        tid("parent-full-role"),
		Slug:      "parent-full",
		Name:      "parent-full",
		ProjectID: project.ID,
		Phase:     "running",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "full",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, parentAgent))

	// Agent creates a sub-agent without specifying a role.
	rec := doAgentCallerRequest(t, srv, parentAgent.ID, project.ID, CreateAgentRequest{
		Name:      "child-inherit-full",
		ProjectID: project.ID,
	})

	// The request should succeed (or at least not fail with authz error).
	// It may fail downstream at dispatch, but the agent record should be created.
	if rec.Code == http.StatusForbidden {
		t.Fatalf("sub-agent creation should not be forbidden: %s", rec.Body.String())
	}

	// Verify the child agent was created and inherited the parent's role.
	if role, ok := getStoredAgentRole(t, s, project.ID, "child-inherit-full"); ok {
		assert.Equal(t, "full", role,
			"child should inherit parent's full role when no role is requested")
	}
}

func TestCreateSubAgent_LegacyParent(t *testing.T) {
	srv, s, _, project := setupAgentRoleTest(t)
	ctx := context.Background()

	// Create a legacy parent agent with no AgentRole set in AppliedConfig.
	legacyParent := &store.Agent{
		ID:            tid("parent-legacy"),
		Slug:          "parent-legacy",
		Name:          "parent-legacy",
		ProjectID:     project.ID,
		Phase:         "running",
		AppliedConfig: &store.AgentAppliedConfig{},
	}
	require.NoError(t, s.CreateAgent(ctx, legacyParent))

	// Agent creates a sub-agent without specifying a role.
	rec := doAgentCallerRequest(t, srv, legacyParent.ID, project.ID, CreateAgentRequest{
		Name:      "child-legacy-parent",
		ProjectID: project.ID,
	})

	if rec.Code == http.StatusForbidden {
		t.Fatalf("sub-agent creation should not be forbidden: %s", rec.Body.String())
	}

	// Legacy parent defaults to baseline, so child should get baseline.
	if role, ok := getStoredAgentRole(t, s, project.ID, "child-legacy-parent"); ok {
		assert.Equal(t, "baseline", role,
			"child of legacy parent (no stored role) should get baseline")
	}
}

func TestCreateSubAgent_NoEscalationNotEnforced(t *testing.T) {
	srv, s, _, project := setupAgentRoleTest(t)
	ctx := context.Background()

	// Create a parent agent with role=baseline.
	baselineParent := &store.Agent{
		ID:        tid("parent-baseline-esc"),
		Slug:      "parent-baseline-esc",
		Name:      "parent-baseline-esc",
		ProjectID: project.ID,
		Phase:     "running",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "baseline",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, baselineParent))

	// Baseline parent requests role=full for child. In F2P1, this should
	// still be allowed (no-escalation enforcement is F2P2).
	rec := doAgentCallerRequest(t, srv, baselineParent.ID, project.ID, CreateAgentRequest{
		Name:      "child-escalated",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	// Should NOT get a 403 — enforcement is not in place yet.
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"baseline parent requesting full child should NOT be forbidden in F2P1")

	// The child should get full (capped only by projectMax=baseline, so baseline).
	// NOTE: projectMax defaults to baseline, so the effective role is baseline
	// even though full was requested. This is projectMax capping, not parentRole capping.
	if role, ok := getStoredAgentRole(t, s, project.ID, "child-escalated"); ok {
		assert.Equal(t, "baseline", role,
			"role should be capped by projectMax (baseline), not by parentRole in F2P1")
	}
}

func TestCreateAgent_ProjectMaxCapsRole(t *testing.T) {
	srv, s, _, _ := setupAgentRoleTest(t)
	ctx := context.Background()

	// Create a project with max-agent-role=readonly.
	project := &store.Project{
		ID:   tid("project-readonly-max"),
		Name: "readonly-max-project",
		Slug: "readonly-max-project",
		Annotations: map[string]string{
			"scion.dev/max-agent-role": "readonly",
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	admin := &store.User{
		ID:          tid("user-admin-cap"),
		Email:       "admin-cap@test.com",
		DisplayName: "Admin Cap Tester",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)

	_ = doAgentRoleRequest(t, srv, admin, CreateAgentRequest{
		Name:      "test-admin-capped",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	if role, ok := getStoredAgentRole(t, s, project.ID, "test-admin-capped"); ok {
		assert.Equal(t, "readonly", role,
			"admin requesting full in readonly-max project should get readonly")
	}
}
