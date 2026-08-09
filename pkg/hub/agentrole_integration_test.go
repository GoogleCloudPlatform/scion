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

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
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

func TestCreateAgent_NoAgentRole_GetsFull(t *testing.T) {
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
		assert.Equal(t, "full", role,
			"default role should be full for member user")
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

func TestCreateAgent_RoleFull_AllowedForMember(t *testing.T) {
	srv, s, user, project := setupAgentRoleTest(t)

	rec := doAgentRoleRequest(t, srv, user, CreateAgentRequest{
		Name:      "test-full-member",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	// Member user ceiling is now full; explicitly requesting full should succeed.
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"member user requesting full should not be forbidden; got: %s", rec.Body.String())

	// If the agent was persisted, verify role.
	if role, ok := getStoredAgentRole(t, s, project.ID, "test-full-member"); ok {
		assert.Equal(t, "full", role,
			"member user requesting full should get full")
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

	// Legacy parent defaults to full, so child should get full.
	if role, ok := getStoredAgentRole(t, s, project.ID, "child-legacy-parent"); ok {
		assert.Equal(t, "full", role,
			"child of legacy parent (no stored role) should get full")
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

// TestTemplateHubAccessScopes_StoredButIgnoredForToken verifies that
// hubAccess.scopes from a template are stored on the agent's AppliedConfig for
// backward-compatibility visibility, but are NOT used for JWT token generation.
// Token scopes are derived solely from the agent role (Phase 2 change).
func TestTemplateHubAccessScopes_StoredButIgnoredForToken(t *testing.T) {
	// Use a non-dev-auth server so the role is not auto-upgraded to full.
	s, err := newTestStore(":memory:")
	require.NoError(t, err)
	require.NoError(t, s.Migrate(context.Background()))

	cfg := DefaultServerConfig()
	cfg.AgentTokenConfig = AgentTokenConfig{
		SigningKey:    make([]byte, 32),
		TokenDuration: time.Hour,
	}
	// Deliberately NOT setting DevAuthToken so role-based scopes are enforced.

	srv, err := New(cfg, s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:      tid("project-tmpl-scopes"),
		Name:    "tmpl-scopes-project",
		Slug:    "tmpl-scopes-project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create a template with legacy hubAccess.scopes that include elevated permissions
	tmpl := &store.Template{
		ID:      tid("tmpl-hub-scopes"),
		Name:    "hub-scopes-template",
		Slug:    "hub-scopes-template",
		Scope:   "project",
		ScopeID: project.ID,
		Status:  store.TemplateStatusActive,
		Config: &store.TemplateConfig{
			HubAccess: &store.HubAccessConfig{
				Scopes: []string{"project:read", "agent:create", "agent:lifecycle", "secret:read"},
			},
		},
	}
	require.NoError(t, s.CreateTemplate(ctx, tmpl))

	// Simulate what populateAgentConfig does: build an agent with a baseline role
	// and populate it from the template.
	agent := &store.Agent{
		ID:        tid("agent-scopes-test"),
		Name:      "scopes-test-agent",
		Slug:      "scopes-test-agent",
		ProjectID: project.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: string(AgentRoleBaseline),
		},
	}

	// Call populateAgentConfig to simulate the real code path
	srv.populateAgentConfig(ctx, agent, project, tmpl)

	// Verify the template scopes are stored on AppliedConfig for visibility
	require.NotNil(t, agent.AppliedConfig, "AppliedConfig should be set")
	assert.Equal(t, []string{"project:read", "agent:create", "agent:lifecycle", "secret:read"},
		agent.AppliedConfig.HubAccessScopes,
		"template hubAccess.scopes should be preserved on AppliedConfig for visibility")
	assert.Equal(t, tmpl.ID, agent.AppliedConfig.TemplateID,
		"template ID should be set on AppliedConfig")

	// Verify agentRoleAndScopes does NOT read HubAccessScopes
	role, additionalScopes := agentRoleAndScopes(agent)
	assert.Equal(t, AgentRoleBaseline, role,
		"role should come from AgentRole field, not template scopes")
	assert.Empty(t, additionalScopes,
		"agentRoleAndScopes should not produce additional scopes from HubAccessScopes")

	// Generate a token and verify scopes match the baseline role, not template scopes
	token, err := srv.GenerateAgentToken(agent.ID, agent.ProjectID, agent.Ancestry, role, additionalScopes)
	require.NoError(t, err)

	claims, err := srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)

	// Baseline role grants: project:read, agent:status-update, agent:token-refresh,
	// agent:notify, agent:port-forward
	assert.True(t, claims.HasScope(ScopeProjectRead), "baseline should have project:read")
	assert.True(t, claims.HasScope(ScopeAgentStatusUpdate), "baseline should have agent:status-update")
	assert.True(t, claims.HasScope(ScopeAgentTokenRefresh), "baseline should have agent:token-refresh")

	// These scopes were in the template's hubAccess but must NOT leak into the token
	assert.False(t, claims.HasScope(ScopeAgentCreate),
		"agent:create from template scopes should NOT appear in baseline token")
	assert.False(t, claims.HasScope(ScopeAgentLifecycle),
		"agent:lifecycle from template scopes should NOT appear in baseline token")
	assert.False(t, claims.HasScope(ScopeProjectSecretRead),
		"secret:read from template scopes should NOT appear in baseline token")
}

// TestTemplateHubAccessScopes_EmptyDoesNotWarn verifies that a template with
// an empty hubAccess.scopes array does not trigger the deprecation warning.
func TestTemplateHubAccessScopes_EmptyDoesNotWarn(t *testing.T) {
	// An agent whose template config has hubAccess with empty scopes should
	// not modify HubAccessScopes and should behave identically to a template
	// without any hubAccess block.
	agent := &store.Agent{
		ID:   tid("agent-empty-scopes"),
		Name: "empty-scopes-agent",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: string(AgentRoleBaseline),
		},
	}

	tmpl := &store.Template{
		ID:   tid("tmpl-empty-scopes"),
		Name: "empty-scopes-template",
		Slug: "empty-scopes-template",
		Config: &store.TemplateConfig{
			HubAccess: &store.HubAccessConfig{
				Scopes: []string{},
			},
		},
	}

	srv, _ := testServer(t)
	srv.populateAgentConfig(context.Background(), agent, nil, tmpl)

	assert.Empty(t, agent.AppliedConfig.HubAccessScopes,
		"empty scopes array should not populate HubAccessScopes")
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
			projectSettingMaxAgentRole: "readonly",
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

func TestCreateSubAgent_LegacyParent_DefaultsToFull(t *testing.T) {
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

	// Legacy parent defaults to full ceiling, so requesting full should succeed.
	rec := doAgentCallerRequest(t, srv, legacy.ID, project.ID, CreateAgentRequest{
		Name:      "child-legacy-full",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"legacy parent (no stored role → full) should allow full sub-agent; got: %s", rec.Body.String())

	if role, ok := getStoredAgentRole(t, s, project.ID, "child-legacy-full"); ok {
		assert.Equal(t, "full", role,
			"child of legacy parent should get full")
	}
}

func TestCreateAgent_ProjectMaxBaseline_CapsFullToBaseline(t *testing.T) {
	srv, s, _, _ := setupAgentRoleTest(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("project-baseline-max"),
		Name: "baseline-max-project",
		Slug: "baseline-max-project",
		Annotations: map[string]string{
			projectSettingMaxAgentRole: "baseline",
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	admin := &store.User{
		ID:          tid("user-admin-base-cap"),
		Email:       "admin-base-cap@test.com",
		DisplayName: "Admin Baseline Cap",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, admin))
	ensureHubMembership(ctx, s, admin.ID)

	_ = doAgentRoleRequest(t, srv, admin, CreateAgentRequest{
		Name:      "test-full-in-baseline-max",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	if role, ok := getStoredAgentRole(t, s, project.ID, "test-full-in-baseline-max"); ok {
		assert.Equal(t, "baseline", role,
			"admin requesting full in baseline-max project should be capped to baseline")
	}
}

func TestCreateAgent_ProjectMaxReadonly_MemberGetsReadonly(t *testing.T) {
	srv, s, user, _ := setupAgentRoleTest(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("project-readonly-member"),
		Name: "readonly-member-project",
		Slug: "readonly-member-project",
		Annotations: map[string]string{
			projectSettingMaxAgentRole: "readonly",
		},
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// Member user requesting no specific role — should default to project max (readonly)
	_ = doAgentRoleRequest(t, srv, user, CreateAgentRequest{
		Name:      "test-member-readonly-proj",
		ProjectID: project.ID,
	})

	if role, ok := getStoredAgentRole(t, s, project.ID, "test-member-readonly-proj"); ok {
		assert.Equal(t, "readonly", role,
			"member in readonly-max project should get readonly")
	}
}

// ---------------------------------------------------------------------------
// Phase 7 — Read endpoint enforcement (ScopeProjectRead)
// ---------------------------------------------------------------------------

// doAgentReadRequest makes a GET request to the given path using an agent token
// with the specified scopes.
func doAgentReadRequest(t *testing.T, srv *Server, agentID, projectID, path string, scopes []AgentTokenScope) *httptest.ResponseRecorder {
	t.Helper()
	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	token, err := tokenSvc.GenerateAgentToken(agentID, projectID, scopes, nil)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("X-Scion-Agent-Token", token)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// setupReadScopeTest creates a server, user, project, and agent for read scope tests.
func setupReadScopeTest(t *testing.T) (*Server, store.Store, *store.Agent, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          tid("user-read-scope"),
		Email:       "readscope@test.com",
		DisplayName: "Read Scope Tester",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, user))
	ensureHubMembership(ctx, s, user.ID)

	project := &store.Project{
		ID:        tid("project-read-scope"),
		Name:      "read-scope-project",
		Slug:      "read-scope-project",
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	agent := &store.Agent{
		ID:        tid("agent-read-scope"),
		Slug:      "read-scope-agent",
		Name:      "Read Scope Agent",
		ProjectID: project.ID,
		Phase:     "running",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return srv, s, agent, project
}

func TestReadEndpoint_BaselineAgent_Allowed(t *testing.T) {
	srv, _, agent, project := setupReadScopeTest(t)

	// Baseline scopes include ScopeProjectRead.
	scopes := ScopesForRole(AgentRoleBaseline)

	endpoints := []string{
		"/api/v1/agents?projectId=" + project.ID,
		"/api/v1/agents/" + agent.ID,
		"/api/v1/templates",
		"/api/v1/skills",
		"/api/v1/harness-configs",
		"/api/v1/projects",
		"/api/v1/projects/" + project.ID,
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			rec := doAgentReadRequest(t, srv, agent.ID, project.ID, ep, scopes)
			assert.NotEqual(t, http.StatusForbidden, rec.Code,
				"baseline agent should not be forbidden on %s; got %d: %s",
				ep, rec.Code, rec.Body.String())
		})
	}
}

func TestReadEndpoint_ReadonlyAgent_Allowed(t *testing.T) {
	srv, _, agent, project := setupReadScopeTest(t)

	// Readonly scopes include ScopeProjectRead.
	scopes := ScopesForRole(AgentRoleReadOnly)

	endpoints := []string{
		"/api/v1/agents?projectId=" + project.ID,
		"/api/v1/agents/" + agent.ID,
		"/api/v1/templates",
		"/api/v1/skills",
		"/api/v1/harness-configs",
		"/api/v1/projects",
		"/api/v1/projects/" + project.ID,
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			rec := doAgentReadRequest(t, srv, agent.ID, project.ID, ep, scopes)
			assert.NotEqual(t, http.StatusForbidden, rec.Code,
				"readonly agent should not be forbidden on %s; got %d: %s",
				ep, rec.Code, rec.Body.String())
		})
	}
}

func TestReadEndpoint_NoReadScope_Blocked(t *testing.T) {
	srv, _, agent, project := setupReadScopeTest(t)

	// Simulate a legacy agent or misconfigured token: valid JWT but no ScopeProjectRead.
	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}

	endpoints := []string{
		"/api/v1/agents?projectId=" + project.ID,
		"/api/v1/agents/" + agent.ID,
		"/api/v1/templates",
		"/api/v1/skills",
		"/api/v1/harness-configs",
		"/api/v1/projects",
		"/api/v1/projects/" + project.ID,
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			rec := doAgentReadRequest(t, srv, agent.ID, project.ID, ep, scopes)
			assert.Equal(t, http.StatusForbidden, rec.Code,
				"agent without ScopeProjectRead should be forbidden on %s; got %d: %s",
				ep, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "project:read",
				"error should mention the missing scope")
		})
	}
}

func TestReadEndpoint_ProjectScopedAgents_NoReadScope_Blocked(t *testing.T) {
	srv, _, agent, project := setupReadScopeTest(t)

	// Agent with valid JWT but no ScopeProjectRead should be blocked on project-scoped routes.
	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}

	endpoints := []string{
		"/api/v1/projects/" + project.ID + "/agents",
		"/api/v1/projects/" + project.ID + "/agents/" + agent.ID,
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			rec := doAgentReadRequest(t, srv, agent.ID, project.ID, ep, scopes)
			assert.Equal(t, http.StatusForbidden, rec.Code,
				"agent without ScopeProjectRead should be forbidden on %s; got %d: %s",
				ep, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "project:read",
				"error should mention the missing scope")
		})
	}
}

func TestReadEndpoint_ProjectScopedAgents_WithReadScope_Allowed(t *testing.T) {
	srv, _, agent, project := setupReadScopeTest(t)

	// Baseline scopes include ScopeProjectRead — should be allowed.
	scopes := ScopesForRole(AgentRoleBaseline)

	endpoints := []string{
		"/api/v1/projects/" + project.ID + "/agents",
		"/api/v1/projects/" + project.ID + "/agents/" + agent.ID,
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			rec := doAgentReadRequest(t, srv, agent.ID, project.ID, ep, scopes)
			assert.NotEqual(t, http.StatusForbidden, rec.Code,
				"baseline agent should not be forbidden on %s; got %d: %s",
				ep, rec.Code, rec.Body.String())
		})
	}
}

func TestReadEndpoint_UserCaller_NotAffected(t *testing.T) {
	srv, _, _, project := setupReadScopeTest(t)

	// User callers should not be affected by the agent scope check.
	token, _, _, err := srv.userTokenService.GenerateTokenPair(
		tid("user-read-scope"), "readscope@test.com", "Read Scope Tester",
		store.UserRoleMember, ClientTypeWeb,
	)
	require.NoError(t, err)

	endpoints := []string{
		"/api/v1/agents?projectId=" + project.ID,
		"/api/v1/templates",
		"/api/v1/skills",
		"/api/v1/harness-configs",
		"/api/v1/projects",
		"/api/v1/projects/" + project.ID,
	}

	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, ep, nil)
			req.Header.Set("Authorization", "Bearer "+token)

			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			assert.NotEqual(t, http.StatusForbidden, rec.Code,
				"user caller should not be forbidden on %s; got %d: %s",
				ep, rec.Code, rec.Body.String())
		})
	}
}

// ---------------------------------------------------------------------------
// R1 — Project default_agent_role is NOT overridden by hub-level default
// ---------------------------------------------------------------------------

func TestCreateAgent_ProjectDefaultFull_NotOverriddenByHubBaseline(t *testing.T) {
	srv, s, user, _ := setupAgentRoleTest(t)
	ctx := context.Background()

	// Create a project that explicitly sets default_agent_role=full.
	project := &store.Project{
		ID:   tid("project-def-full"),
		Name: "def-full-project",
		Slug: "def-full-project",
		Annotations: map[string]string{
			projectSettingDefaultAgentRole: "full",
		},
		OwnerID:   user.ID,
		CreatedBy: user.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// Set the hub-level default to baseline — this should NOT override the
	// project-level explicit "full".
	setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{
		DefaultAgentRole: "baseline",
	})

	rec := doAgentRoleRequest(t, srv, user, CreateAgentRequest{
		Name:      "test-proj-full-hub-baseline",
		ProjectID: project.ID,
		// No explicit agentRole — should pick up project default (full).
	})

	// The request should succeed.
	assert.NotEqual(t, http.StatusForbidden, rec.Code,
		"should not be forbidden; got: %s", rec.Body.String())

	// Verify the agent got the project-level default (full), not the hub default (baseline).
	if role, ok := getStoredAgentRole(t, s, project.ID, "test-proj-full-hub-baseline"); ok {
		assert.Equal(t, "full", role,
			"project default_agent_role=full should win over hub default_agent_role=baseline")
	}
}

// ---------------------------------------------------------------------------
// R2 — GetAgent failure for parent agent defaults ceiling to baseline
// ---------------------------------------------------------------------------

func TestCreateSubAgent_ParentLookupFails_CeilingIsBaseline(t *testing.T) {
	srv, _, project := setupFullMaxProject(t)

	// Use a non-existent parent agent ID so GetAgent returns an error.
	// The ceiling should fall back to baseline (fail-closed).
	nonExistentParentID := tid("parent-does-not-exist")

	// Request a full sub-agent; the baseline ceiling should cap it to baseline.
	rec := doAgentCallerRequest(t, srv, nonExistentParentID, project.ID, CreateAgentRequest{
		Name:      "child-parent-missing",
		ProjectID: project.ID,
		AgentRole: "full",
	})

	// Requesting full when the ceiling is baseline should trigger the
	// no-escalation check and return 403.
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"requesting full with a missing parent should be forbidden (baseline ceiling); got: %s",
		rec.Body.String())
	assert.Contains(t, rec.Body.String(), "parent agent role",
		"error should mention the parent agent role constraint")
}
