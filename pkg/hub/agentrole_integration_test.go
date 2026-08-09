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

func TestCreateAgent_RoleFull_RejectedByMemberCeiling(t *testing.T) {
	srv, _, user, project := setupAgentRoleTest(t)

	rec := doAgentRoleRequest(t, srv, user, CreateAgentRequest{
		Name:      "test-full-capped",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	// Member user ceiling is baseline; explicitly requesting full is fail-loud 403.
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"member user requesting full should be forbidden; got: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "user ceiling")
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

func TestCreateSubAgent_NoEscalationEnforced(t *testing.T) {
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

	// Baseline parent requests role=full for child — should be rejected with 403 (F2P2).
	rec := doAgentCallerRequest(t, srv, baselineParent.ID, project.ID, CreateAgentRequest{
		Name:      "child-escalated",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	// Should get a 403 — no-escalation enforcement is in place.
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"baseline parent requesting full child should be forbidden")
	assert.Contains(t, rec.Body.String(), "parent agent role")

	// Agent should NOT have been created.
	_, err := s.GetAgentBySlug(ctx, project.ID, "child-escalated")
	assert.Error(t, err, "escalated sub-agent should not be persisted")
}

func TestCreateAgent_ProjectMaxCapsRole(t *testing.T) {
	srv, st, _, _ := setupAgentRoleTest(t)
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
	require.NoError(t, st.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	admin := &store.User{
		ID:          tid("user-admin-cap"),
		Email:       "admin-cap@test.com",
		DisplayName: "Admin Cap Tester",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, st.CreateUser(ctx, admin))
	ensureHubMembership(ctx, st, admin.ID)

	rec := doAgentRoleRequest(t, srv, admin, CreateAgentRequest{
		Name:      "test-admin-capped",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	// Admin requesting full in a readonly-max project should be fail-loud 403.
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"admin requesting full in readonly-max project should be forbidden; got: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "project maximum")
}

// ---------- F2P2 No-Escalation Integration Tests ----------

// setupFullMaxProject creates a project with max-agent-role=full so that
// projectMax does not interfere with parent-ceiling tests.
func setupFullMaxProject(t *testing.T) (*Server, store.Store, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("user-f2p2-setup"),
		Email:       "f2p2setup@test.com",
		DisplayName: "F2P2 Setup",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("project-f2p2"),
		Name:      "f2p2-project",
		Slug:      "f2p2-project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Annotations: map[string]string{
			"scion.dev/max-agent-role": "full",
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	return srv, s, project
}

func TestCreateSubAgent_BaselineParent_RequestsFull_403(t *testing.T) {
	srv, s, project := setupFullMaxProject(t)
	ctx := context.Background()

	parent := &store.Agent{
		ID:        tid("f2p2-parent-baseline"),
		Slug:      "f2p2-parent-baseline",
		Name:      "f2p2-parent-baseline",
		ProjectID: project.ID,
		Phase:     "running",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "baseline",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, parent))

	rec := doAgentCallerRequest(t, srv, parent.ID, project.ID, CreateAgentRequest{
		Name:      "child-over-request",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"baseline parent requesting full sub-agent should get 403; got: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "parent agent role")
}

func TestCreateSubAgent_FullParent_RequestsBaseline_OK(t *testing.T) {
	srv, s, project := setupFullMaxProject(t)
	ctx := context.Background()

	parent := &store.Agent{
		ID:        tid("f2p2-parent-full-bl"),
		Slug:      "f2p2-parent-full-bl",
		Name:      "f2p2-parent-full-bl",
		ProjectID: project.ID,
		Phase:     "running",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "full",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, parent))

	rec := doAgentCallerRequest(t, srv, parent.ID, project.ID, CreateAgentRequest{
		Name:      "child-baseline-ok",
		ProjectID: project.ID,
		AgentRole: "baseline",
	})

	// Should succeed (not 403)
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"full parent requesting baseline sub-agent should not be forbidden")

	if role, ok := getStoredAgentRole(t, s, project.ID, "child-baseline-ok"); ok {
		assert.Equal(t, "baseline", role,
			"child should get the explicitly requested baseline role")
	}
}

func TestCreateSubAgent_FullParent_RequestsFull_OK(t *testing.T) {
	srv, s, project := setupFullMaxProject(t)
	ctx := context.Background()

	parent := &store.Agent{
		ID:        tid("f2p2-parent-full-f"),
		Slug:      "f2p2-parent-full-f",
		Name:      "f2p2-parent-full-f",
		ProjectID: project.ID,
		Phase:     "running",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "full",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, parent))

	rec := doAgentCallerRequest(t, srv, parent.ID, project.ID, CreateAgentRequest{
		Name:      "child-full-ok",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	// Should succeed (not 403)
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"full parent requesting full sub-agent should not be forbidden")

	if role, ok := getStoredAgentRole(t, s, project.ID, "child-full-ok"); ok {
		assert.Equal(t, "full", role,
			"child of full parent in full-max project should get full")
	}
}

func TestCreateSubAgent_BaselineParent_DefaultsToBaseline(t *testing.T) {
	srv, s, project := setupFullMaxProject(t)
	ctx := context.Background()

	parent := &store.Agent{
		ID:        tid("f2p2-parent-bl-def"),
		Slug:      "f2p2-parent-bl-def",
		Name:      "f2p2-parent-bl-def",
		ProjectID: project.ID,
		Phase:     "running",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "baseline",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, parent))

	// No explicit agentRole — should inherit parent's baseline.
	rec := doAgentCallerRequest(t, srv, parent.ID, project.ID, CreateAgentRequest{
		Name:      "child-default-bl",
		ProjectID: project.ID,
	})

	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"baseline parent with no explicit role should not be forbidden")

	if role, ok := getStoredAgentRole(t, s, project.ID, "child-default-bl"); ok {
		assert.Equal(t, "baseline", role,
			"child with no explicit role should inherit parent's baseline")
	}
}

func TestCreateSubAgent_MultiHop(t *testing.T) {
	srv, s, project := setupFullMaxProject(t)
	ctx := context.Background()

	// Full grandparent creates a baseline child.
	// We verify the grandparent→child request is allowed (not 403), then
	// create the child directly in the store to test the child→grandchild hop
	// (the HTTP path cannot persist agents without a broker).
	grandparent := &store.Agent{
		ID:        tid("f2p2-grandparent"),
		Slug:      "f2p2-grandparent",
		Name:      "f2p2-grandparent",
		ProjectID: project.ID,
		Phase:     "running",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "full",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, grandparent))

	rec := doAgentCallerRequest(t, srv, grandparent.ID, project.ID, CreateAgentRequest{
		Name:      "mh-child-baseline",
		ProjectID: project.ID,
		AgentRole: "baseline",
	})
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"full grandparent creating baseline child should not be forbidden")

	// Create the baseline child directly in store for the second hop test.
	child := &store.Agent{
		ID:        tid("f2p2-mh-child"),
		Slug:      "mh-child-baseline",
		Name:      "mh-child-baseline",
		ProjectID: project.ID,
		Phase:     "running",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "baseline",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, child))

	// Baseline child tries to create a full grandchild — should be 403.
	rec2 := doAgentCallerRequest(t, srv, child.ID, project.ID, CreateAgentRequest{
		Name:      "mh-grandchild-full",
		ProjectID: project.ID,
		AgentRole: "full",
	})
	assert.Equal(t, http.StatusForbidden, rec2.Code,
		"baseline child should not be able to create full grandchild; got: %s", rec2.Body.String())
	assert.Contains(t, rec2.Body.String(), "parent agent role")
}

func TestGetAgent_IncludesAgentRole(t *testing.T) {
	srv, s, user, project := setupAgentRoleTest(t)
	ctx := context.Background()

	// Create an agent with role=baseline stored in AppliedConfig.
	agent := &store.Agent{
		ID:        tid("agent-get-role"),
		Slug:      "agent-get-role",
		Name:      "agent-get-role",
		ProjectID: project.ID,
		Phase:     "running",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "baseline",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// GET the agent via the API.
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code,
		"GET agent should succeed; got: %s", rec.Body.String())

	// Parse the response and verify agentRole is present in appliedConfig.
	var resp AgentWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.AppliedConfig,
		"response should include appliedConfig")
	assert.Equal(t, "baseline", resp.AppliedConfig.AgentRole,
		"GET response should include agentRole in appliedConfig")
}

func TestGetAgent_IncludesAgentRoleFull(t *testing.T) {
	srv, s, user, project := setupAgentRoleTest(t)
	ctx := context.Background()

	// Create an agent with role=full stored in AppliedConfig.
	agent := &store.Agent{
		ID:        tid("agent-get-role-full"),
		Slug:      "agent-get-role-full",
		Name:      "agent-get-role-full",
		ProjectID: project.ID,
		Phase:     "running",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "full",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// GET the agent via the API.
	rec := doRequestAsUser(t, srv, user, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code,
		"GET agent should succeed; got: %s", rec.Body.String())

	// Parse the response and verify agentRole=full is present.
	var resp AgentWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.AppliedConfig,
		"response should include appliedConfig")
	assert.Equal(t, "full", resp.AppliedConfig.AgentRole,
		"GET response should include agentRole=full in appliedConfig")
}

func TestCreateSubAgent_LegacyParent_CapsAtBaseline(t *testing.T) {
	srv, s, project := setupFullMaxProject(t)
	ctx := context.Background()

	// Legacy agent: AppliedConfig exists but has no AgentRole set.
	legacy := &store.Agent{
		ID:            tid("f2p2-legacy-parent"),
		Slug:          "f2p2-legacy-parent",
		Name:          "f2p2-legacy-parent",
		ProjectID:     project.ID,
		Phase:         "running",
		AppliedConfig: &store.AgentAppliedConfig{},
	}
	require.NoError(t, s.CreateAgent(ctx, legacy))

	// Legacy parent requesting full should be rejected — defaults to baseline ceiling.
	rec := doAgentCallerRequest(t, srv, legacy.ID, project.ID, CreateAgentRequest{
		Name:      "child-legacy-full",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"legacy parent (no stored role → baseline) should not create full sub-agent; got: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "parent agent role")
}
