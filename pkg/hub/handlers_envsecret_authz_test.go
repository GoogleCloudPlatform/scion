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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ============================================================================
// Env Var Authorization Tests
// ============================================================================

func TestEnvVar_UserScope_AdminAccess(t *testing.T) {
	srv, _ := testServer(t)

	// Admin (dev-user) should be able to list env vars (user scope)
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/env?scope=user", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for admin user scope, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListEnvVarsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// ScopeID should be the dev-user's ID, not "default"
	if resp.ScopeID == "default" {
		t.Error("scopeId should not be 'default' — should be the authenticated user's ID")
	}
	if resp.ScopeID != DevUserID {
		t.Errorf("expected scopeId 'dev-user', got %q", resp.ScopeID)
	}
}

func TestEnvVar_UserScope_MemberAccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	member := &store.User{
		ID:          tid("member-env-1"),
		Email:       "member-env@example.com",
		DisplayName: "Test Member",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, member); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Member should be able to list their own env vars
	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/env?scope=user", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for member user scope, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListEnvVarsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ScopeID != tid("member-env-1") {
		t.Errorf("expected scopeId 'member-env-1', got %q", resp.ScopeID)
	}
}

func TestEnvVar_UserScope_CreateAndGet(t *testing.T) {
	srv, _ := testServer(t)

	// Create an env var (admin, user scope)
	body := SetEnvVarRequest{
		Value:       "test-value",
		Description: "A test variable",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/MY_VAR?scope=user", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var setResp SetEnvVarResponse
	if err := json.NewDecoder(rec.Body).Decode(&setResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !setResp.Created {
		t.Error("expected created=true for new env var")
	}

	// CreatedBy should be populated
	if setResp.EnvVar.CreatedBy != DevUserID {
		t.Errorf("expected createdBy 'dev-user', got %q", setResp.EnvVar.CreatedBy)
	}

	// Get the env var back
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/env/MY_VAR?scope=user", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestEnvVar_UserScope_Unauthenticated(t *testing.T) {
	srv, _ := testServer(t)

	// Unauthenticated should get 401
	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/env?scope=user", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvVar_UserScope_Delete(t *testing.T) {
	srv, _ := testServer(t)

	// Create an env var first
	body := SetEnvVarRequest{Value: "to-delete"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/DELETE_ME?scope=user", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 creating env var, got %d: %s", rec.Code, rec.Body.String())
	}

	// Delete it
	rec2 := doRequest(t, srv, http.MethodDelete, "/api/v1/env/DELETE_ME?scope=user", nil)
	if rec2.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestEnvVar_UserScope_MemberIsolation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	userA := &store.User{
		ID: tid("user-iso-a"), Email: "a@example.com", DisplayName: "User A",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	userB := &store.User{
		ID: tid("user-iso-b"), Email: "b@example.com", DisplayName: "User B",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, userA); err != nil {
		t.Fatalf("failed to create user A: %v", err)
	}
	if err := s.CreateUser(ctx, userB); err != nil {
		t.Fatalf("failed to create user B: %v", err)
	}

	// User A creates an env var
	body := SetEnvVarRequest{Value: "user-a-value"}
	rec := doRequestAsUser(t, srv, userA, http.MethodPut, "/api/v1/env/PRIVATE_VAR?scope=user", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// User A can see it
	rec2 := doRequestAsUser(t, srv, userA, http.MethodGet, "/api/v1/env?scope=user", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var respA ListEnvVarsResponse
	_ = json.NewDecoder(rec2.Body).Decode(&respA)
	if len(respA.EnvVars) != 1 {
		t.Errorf("expected 1 env var for user A, got %d", len(respA.EnvVars))
	}

	// User B should NOT see user A's env var (different scopeID)
	rec3 := doRequestAsUser(t, srv, userB, http.MethodGet, "/api/v1/env?scope=user", nil)
	if rec3.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec3.Code, rec3.Body.String())
	}
	var respB ListEnvVarsResponse
	_ = json.NewDecoder(rec3.Body).Decode(&respB)
	if len(respB.EnvVars) != 0 {
		t.Errorf("expected 0 env vars for user B, got %d", len(respB.EnvVars))
	}
}

// ============================================================================
// Project-Scoped Env Var Authorization Tests
// ============================================================================

func TestEnvVar_ProjectScope_OwnerAccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	owner := &store.User{
		ID: tid("project-owner-1"), Email: "owner@example.com", DisplayName: "Owner",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, owner); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	project := &store.Project{
		ID:      tid("project_env_owner"),
		Name:    "Owner Test Project",
		Slug:    "owner-test-project",
		OwnerID: tid("project-owner-1"),
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	// Project authority comes from the project-owner binding, not OwnerID
	// (ptone/scion#2586).
	if err := srv.createProjectOwnerRoleBinding(ctx, project.ID, owner.ID); err != nil {
		t.Fatalf("failed to create owner binding: %v", err)
	}

	// Owner should be able to list project env vars
	rec := doRequestAsUser(t, srv, owner, http.MethodGet, "/api/v1/projects/"+project.ID+"/env", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for project owner, got %d: %s", rec.Code, rec.Body.String())
	}

	// Owner should be able to set project env vars
	body := SetEnvVarRequest{Value: "project-val"}
	rec2 := doRequestAsUser(t, srv, owner, http.MethodPut, "/api/v1/projects/"+project.ID+"/env/PROJECT_VAR", body)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 for project owner write, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestEnvVar_ProjectScope_NonOwnerDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	nonOwner := &store.User{
		ID: tid("non-owner-1"), Email: "nonowner@example.com", DisplayName: "Non-Owner",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, nonOwner); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	project := &store.Project{
		ID:      tid("project_env_notown"),
		Name:    "Not Owned Project",
		Slug:    "not-owned-project",
		OwnerID: "someone-else",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Non-owner without policy should be denied
	rec := doRequestAsUser(t, srv, nonOwner, http.MethodGet, "/api/v1/projects/"+project.ID+"/env", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-owner, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvVar_ProjectScope_AdminAccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:      tid("project_env_admin"),
		Name:    "Admin Test Project",
		Slug:    "admin-test-project",
		OwnerID: "someone-else",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Admin (dev-user) should be able to access any project
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/env", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for admin, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvVar_ProjectScope_AgentReadOwnProject(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:      tid("project_agent_env"),
		Name:    "Agent Project",
		Slug:    "agent-project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID:           tid("agent_env_test"),
		Slug:         "env-test-agent",
		Name:         "Env Test Agent",
		ProjectID:    project.ID,
		Phase:        string(state.PhaseRunning),
		StateVersion: 1,
		Created:      time.Now(),
		Updated:      time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	agentToken := srv.generateRecordedAgentToken(t, agent.ID, project.ID,
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectRead}, nil)

	// Agent should be able to read own project env vars
	rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/env", nil, agentToken)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for agent reading own project, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvVar_ProjectScope_AgentOtherProjectDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project1 := &store.Project{
		ID: tid("project_agent_own"), Name: "Agent's Project", Slug: "agents-project",
		Created: time.Now(), Updated: time.Now(),
	}
	project2 := &store.Project{
		ID: tid("project_agent_other"), Name: "Other Project", Slug: "other-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project1); err != nil {
		t.Fatalf("failed to create project1: %v", err)
	}
	if err := s.CreateProject(ctx, project2); err != nil {
		t.Fatalf("failed to create project2: %v", err)
	}

	agent := &store.Agent{
		ID: tid("agent_other_project"), Slug: "other-project-agent", Name: "Other Project Agent",
		ProjectID: project1.ID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	agentToken := srv.generateRecordedAgentToken(t, agent.ID, project1.ID,
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectRead}, nil)

	// Agent should NOT be able to read other project env vars (project isolation)
	rec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/projects/"+project2.ID+"/env", nil, agentToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for agent reading other project, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvVar_ProjectScope_AgentWriteDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_agent_nowrite"), Name: "Agent No Write Project", Slug: "agent-nowrite-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID: tid("agent_nowrite"), Slug: "nowrite-agent", Name: "No Write Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	agentToken := srv.generateRecordedAgentToken(t, agent.ID, project.ID,
		[]AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectRead}, nil)

	// Agent should NOT be able to write project env vars (write is always denied for agents)
	body := SetEnvVarRequest{Value: "agent-val"}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/env/AGENT_VAR", body, agentToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for agent write, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// Broker-Scoped Env Var Authorization Tests
// ============================================================================

func TestEnvVar_BrokerScope_AdminAccess(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	broker := &store.RuntimeBroker{
		ID: tid("broker_env_admin"), Name: "Env Admin Broker", Slug: "env-admin-broker",
		Status: store.BrokerStatusOnline, Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateRuntimeBroker(ctx, broker); err != nil {
		t.Fatalf("failed to create broker: %v", err)
	}

	// Admin should be able to access broker env vars
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/runtime-brokers/"+broker.ID+"/env", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for admin, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// Secrets Authorization Tests
// ============================================================================

func TestSecret_UserScope_AdminAccess(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/secrets?scope=user", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for admin user scope secrets, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListSecretsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Admin users should only see their own secrets in the profile view.
	if resp.ScopeID != DevUserID {
		t.Errorf("expected scopeId %q for admin, got %q", DevUserID, resp.ScopeID)
	}
}

func TestSecret_UserScope_AdminDoesNotSeeOtherUserSecrets(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	// Create a member user and store a secret scoped to them.
	member := &store.User{
		ID: tid("member-other-1"), Email: "other@example.com", DisplayName: "Other User",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, member); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Store a secret as the member.
	if err := s.CreateSecret(ctx, &store.Secret{
		ID: tid("sec-other-1"), Key: "MEMBER_KEY", Scope: store.ScopeUser,
		ScopeID: member.ID, SecretType: store.SecretTypeEnvironment,
		EncryptedValue: "val", Version: 1, Created: time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("failed to set secret: %v", err)
	}

	// Admin (dev-user) should NOT see the member's secret in their profile view.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/secrets?scope=user", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListSecretsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	for _, sec := range resp.Secrets {
		if sec.Key == "MEMBER_KEY" {
			t.Errorf("admin should NOT see other user's secret MEMBER_KEY in profile view, but it was in the list")
		}
	}
}

func TestSecret_UserScope_MemberAccess(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	member := &store.User{
		ID: tid("member-sec-1"), Email: "member-sec@example.com", DisplayName: "Test Member",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, member); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/secrets?scope=user", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for member user scope secrets, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListSecretsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.ScopeID != tid("member-sec-1") {
		t.Errorf("expected scopeId 'member-sec-1', got %q", resp.ScopeID)
	}
}

func TestSecret_UserScope_Unauthenticated(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/secrets?scope=user", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSecret_UserScope_WriteAuthWorks(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	// Verify that an authenticated user passes auth checks for secret writes.
	// The LocalBackend supports Set (stores plaintext in SQLite), so expect 200.
	body := SetSecretRequest{
		Value:       "c3VwZXItc2VjcmV0",
		Description: "A test secret",
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/MY_SECRET?scope=user", body)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for local secret write, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify unauthenticated is rejected before reaching the backend
	rec2 := doRequestNoAuth(t, srv, http.MethodPut, "/api/v1/secrets/MY_SECRET?scope=user", body)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated write, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// ============================================================================
// Project-Scoped Secrets Authorization Tests
// ============================================================================

func TestSecret_ProjectScope_OwnerAccess(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	owner := &store.User{
		ID: tid("project-sec-owner"), Email: "secowner@example.com", DisplayName: "Owner",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, owner); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	project := &store.Project{
		ID: tid("project_sec_owner"), Name: "Secret Owner Project", Slug: "secret-owner-project",
		OwnerID: tid("project-sec-owner"), Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	// Project authority comes from the project-owner binding, not OwnerID
	// (ptone/scion#2586).
	if err := srv.createProjectOwnerRoleBinding(ctx, project.ID, owner.ID); err != nil {
		t.Fatalf("failed to create owner binding: %v", err)
	}

	rec := doRequestAsUser(t, srv, owner, http.MethodGet, "/api/v1/projects/"+project.ID+"/secrets", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for project owner, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSecret_ProjectScope_NonOwnerDenied(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	nonOwner := &store.User{
		ID: tid("non-sec-owner"), Email: "nonsecowner@example.com", DisplayName: "Non-Owner",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, nonOwner); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	project := &store.Project{
		ID: tid("project_sec_notown"), Name: "Not Owned Secret Project", Slug: "not-owned-secret-project",
		OwnerID: "someone-else", Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	rec := doRequestAsUser(t, srv, nonOwner, http.MethodGet, "/api/v1/projects/"+project.ID+"/secrets", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-owner, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestSecret_ProjectScope_AgentReadOwnProject(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_agent_sec"), Name: "Agent Secret Project", Slug: "agent-secret-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID: tid("agent_sec_test"), Slug: "sec-test-agent", Name: "Secret Test Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	putRec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/PROJ_SEC_KEY", SetSecretRequest{Value: "c3VwZXItc2VjcmV0"})
	if putRec.Code != http.StatusOK {
		t.Fatalf("failed to seed project secret: %d: %s", putRec.Code, putRec.Body.String())
	}

	agentToken := srv.generateRecordedAgentToken(t, agent.ID, project.ID, ScopesForRole(AgentRoleFull), nil)

	for _, path := range []string{
		"/api/v1/projects/" + project.ID + "/secrets",
		"/api/v1/projects/" + project.ID + "/secrets/PROJ_SEC_KEY",
		"/api/v1/secrets?scope=project&scopeId=" + project.ID,
		"/api/v1/secrets/PROJ_SEC_KEY?scope=project&scopeId=" + project.ID,
	} {
		rec := doRequestWithAgentToken(t, srv, http.MethodGet, path, nil, agentToken)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: expected 200 for full-role agent reading own project secrets, got %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestSecret_ProjectScope_AgentRequiresSecretReadScope(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_agent_sec_scope"), Name: "Agent Secret Scope Project", Slug: "agent-secret-scope-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID: tid("agent_sec_scope_test"), Slug: "sec-scope-test-agent", Name: "Secret Scope Test Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	putRec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/PROJ_SEC_KEY", SetSecretRequest{Value: "c3VwZXItc2VjcmV0"})
	if putRec.Code != http.StatusOK {
		t.Fatalf("failed to seed project secret: %d: %s", putRec.Code, putRec.Body.String())
	}

	paths := []string{
		"/api/v1/projects/" + project.ID + "/secrets",
		"/api/v1/projects/" + project.ID + "/secrets/PROJ_SEC_KEY",
		"/api/v1/secrets?scope=project&scopeId=" + project.ID,
		"/api/v1/secrets/PROJ_SEC_KEY?scope=project&scopeId=" + project.ID,
	}

	deniedBundles := []struct {
		name   string
		scopes []AgentTokenScope
	}{
		{"status_only_default", nil},
		{"readonly_role", ScopesForRole(AgentRoleReadOnly)},
		{"baseline_role", ScopesForRole(AgentRoleBaseline)},
	}

	for _, bundle := range deniedBundles {
		t.Run(bundle.name, func(t *testing.T) {
			tok := srv.generateRecordedAgentToken(t, agent.ID, project.ID, bundle.scopes, nil)
			for _, path := range paths {
				rec := doRequestWithAgentToken(t, srv, http.MethodGet, path, nil, tok)
				if rec.Code != http.StatusForbidden {
					t.Errorf("%s (%s): expected 403 without %s, got %d: %s",
						path, bundle.name, ScopeProjectSecretRead, rec.Code, rec.Body.String())
				} else if !strings.Contains(rec.Body.String(), string(ScopeProjectSecretRead)) {
					t.Errorf("%s (%s): expected error mentioning %s, got: %s",
						path, bundle.name, ScopeProjectSecretRead, rec.Body.String())
				}
			}
		})
	}
}

func TestEnvVar_ProjectScope_AgentRequiresSecretReadScopeForSecretMetadata(t *testing.T) {
	srv, s := testServer(t)
	backend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(backend)
	ctx := context.Background()

	owner := &store.User{
		ID: tid("env-sec-meta-owner"), Email: "envsecmetaowner@example.com", DisplayName: "Env Sec Meta Owner",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, owner); err != nil {
		t.Fatalf("failed to create owner user: %v", err)
	}

	project := &store.Project{
		ID: tid("project_env_sec_meta"), Name: "Env Secret Meta Project", Slug: "env-secret-meta-project",
		OwnerID: owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	if err := srv.createProjectOwnerRoleBinding(ctx, project.ID, owner.ID); err != nil {
		t.Fatalf("failed to create owner binding: %v", err)
	}

	agent := &store.Agent{
		ID: tid("agent_env_sec_meta"), Slug: "env-sec-meta-agent", Name: "Env Secret Meta Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	// 1. Plain project env var.
	if rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/env/PLAIN_KEY", SetEnvVarRequest{
		Value:       "plain-value",
		Description: "plain env description",
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed PLAIN_KEY: got %d: %s", rec.Code, rec.Body.String())
	}

	// 2. Sensitive (non-secret) plain project env var.
	if rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/env/SENSITIVE_PLAIN_KEY", SetEnvVarRequest{
		Value:       "sensitive-plain-value",
		Description: "sensitive plain description",
		Sensitive:   true,
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed SENSITIVE_PLAIN_KEY: got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. Environment-type secret with no backing store.EnvVar row.
	if rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/SECRET_ONLY_KEY", SetSecretRequest{
		Value:       "c2VjcmV0LW9ubHk=",
		Description: "secret-only metadata description",
		Type:        store.SecretTypeEnvironment,
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed SECRET_ONLY_KEY: got %d: %s", rec.Code, rec.Body.String())
	}

	// 4. Shadowed key: plain store.EnvVar row created first, then shadowed by an
	// environment-type secret written via /secrets (which leaves the plain row in store).
	if rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/env/SHADOWED_KEY", SetEnvVarRequest{
		Value:       "shadowed-plain-value",
		Description: "stale plain description",
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed plain SHADOWED_KEY: got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/SHADOWED_KEY", SetSecretRequest{
		Value:       "c2hhZG93ZWQtc2VjcmV0",
		Description: "shadowing secret metadata description",
		Type:        store.SecretTypeEnvironment,
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed secret SHADOWED_KEY: got %d: %s", rec.Code, rec.Body.String())
	}
	if storedPlain, err := s.GetEnvVar(ctx, "SHADOWED_KEY", store.ScopeProject, project.ID); err != nil || storedPlain.Value != "shadowed-plain-value" {
		t.Fatalf("expected underlying plain store.EnvVar for SHADOWED_KEY to remain shadowed in store, got %+v, err=%v", storedPlain, err)
	}

	// 5. Non-environment secret ("variable") sharing a name with a plain EnvVar:
	// non-environment secrets do not project into /env and must NOT shadow plain env vars.
	if rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/env/VAR_SHARED_KEY", SetEnvVarRequest{
		Value: "var-shared-plain-value",
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed plain VAR_SHARED_KEY: got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/VAR_SHARED_KEY", SetSecretRequest{
		Value: "dmFyLXNlY3JldA==",
		Type:  store.SecretTypeVariable,
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed variable secret VAR_SHARED_KEY: got %d: %s", rec.Code, rec.Body.String())
	}

	// 6. Legacy store.EnvVar row flagged Secret=true (defense-in-depth).
	if _, err := s.UpsertEnvVar(ctx, &store.EnvVar{
		ID:          tid("legacy_secret_env_row"),
		Key:         "LEGACY_SECRET_ROW",
		Value:       "legacy-secret-value",
		Scope:       store.ScopeProject,
		ScopeID:     project.ID,
		Description: "legacy secret row description",
		Sensitive:   true,
		Secret:      true,
	}); err != nil {
		t.Fatalf("seed LEGACY_SECRET_ROW: %v", err)
	}

	assertForbiddenLeaks := func(t *testing.T, label, body string) {
		t.Helper()
		for _, forbidden := range []string{
			"SECRET_ONLY_KEY",
			"secret-only metadata description",
			"SHADOWED_KEY",
			"shadowing secret metadata description",
			"shadowed-plain-value",
			"stale plain description",
			"LEGACY_SECRET_ROW",
			"legacy-secret-value",
			"legacy secret row description",
		} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s: response body leaked %q: %s", label, forbidden, body)
			}
		}
	}

	assertRestrictedList := func(t *testing.T, tok, path string) {
		t.Helper()
		rec := doRequestWithAgentToken(t, srv, http.MethodGet, path, nil, tok)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200 OK, got %d: %s", path, rec.Code, rec.Body.String())
		}
		assertForbiddenLeaks(t, path, rec.Body.String())

		var resp ListEnvVarsResponse
		if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
			t.Fatalf("%s: decode ListEnvVarsResponse: %v", path, err)
		}
		gotByKey := make(map[string]store.EnvVar, len(resp.EnvVars))
		for _, ev := range resp.EnvVars {
			if ev.Secret {
				t.Errorf("%s: unexpected secret-flagged env var %q: %+v", path, ev.Key, ev)
			}
			gotByKey[ev.Key] = ev
		}
		if len(gotByKey) != 3 {
			t.Errorf("%s: expected 3 plain env vars (PLAIN_KEY, SENSITIVE_PLAIN_KEY, VAR_SHARED_KEY), got %d: %+v", path, len(gotByKey), resp.EnvVars)
		}
		if ev, ok := gotByKey["PLAIN_KEY"]; !ok || ev.Value != "plain-value" {
			t.Errorf("%s: expected PLAIN_KEY=plain-value, got %+v (present=%v)", path, ev, ok)
		}
		if ev, ok := gotByKey["SENSITIVE_PLAIN_KEY"]; !ok || ev.Value != "********" {
			t.Errorf("%s: expected masked SENSITIVE_PLAIN_KEY, got %+v (present=%v)", path, ev, ok)
		}
		if ev, ok := gotByKey["VAR_SHARED_KEY"]; !ok || ev.Value != "var-shared-plain-value" {
			t.Errorf("%s: expected VAR_SHARED_KEY=var-shared-plain-value, got %+v (present=%v)", path, ev, ok)
		}
	}

	assertRestrictedGetByKey := func(t *testing.T, tok string, buildPath func(key string) string) {
		t.Helper()
		// Baseline response for a genuinely non-existent key: hidden secret keys and
		// shadowed keys must match status code and body so there is no existence oracle.
		missingPath := buildPath("NO_SUCH_KEY")
		missingRec := doRequestWithAgentToken(t, srv, http.MethodGet, missingPath, nil, tok)
		if missingRec.Code != http.StatusNotFound {
			t.Fatalf("%s: expected 404 for missing key baseline, got %d: %s", missingPath, missingRec.Code, missingRec.Body.String())
		}
		wantMissingBody := missingRec.Body.String()

		for _, plainCase := range []struct {
			key       string
			wantValue string
		}{
			{"PLAIN_KEY", "plain-value"},
			{"SENSITIVE_PLAIN_KEY", "********"},
			{"VAR_SHARED_KEY", "var-shared-plain-value"},
		} {
			p := buildPath(plainCase.key)
			rec := doRequestWithAgentToken(t, srv, http.MethodGet, p, nil, tok)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: expected 200 OK for plain env var, got %d: %s", p, rec.Code, rec.Body.String())
			}
			var ev store.EnvVar
			if err := json.NewDecoder(rec.Body).Decode(&ev); err != nil {
				t.Fatalf("%s: decode EnvVar: %v", p, err)
			}
			if ev.Secret || ev.Value != plainCase.wantValue {
				t.Errorf("%s: got Secret=%v Value=%q, want Secret=false Value=%q", p, ev.Secret, ev.Value, plainCase.wantValue)
			}
		}

		for _, hiddenKey := range []string{"SECRET_ONLY_KEY", "SHADOWED_KEY", "LEGACY_SECRET_ROW"} {
			p := buildPath(hiddenKey)
			rec := doRequestWithAgentToken(t, srv, http.MethodGet, p, nil, tok)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s: expected 404 Not Found without %s, got %d: %s", p, ScopeProjectSecretRead, rec.Code, rec.Body.String())
			}
			assertForbiddenLeaks(t, p, rec.Body.String())
			if rec.Body.String() != wantMissingBody {
				t.Errorf("%s: response body %q differs from missing-key baseline %q (potential oracle)", p, rec.Body.String(), wantMissingBody)
			}
		}
	}

	assertRestrictedFilteredList := func(t *testing.T, tok string) {
		t.Helper()
		plainFiltered := "/api/v1/env?scope=project&scopeId=" + project.ID + "&key=PLAIN_KEY"
		rec := doRequestWithAgentToken(t, srv, http.MethodGet, plainFiltered, nil, tok)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200 OK, got %d: %s", plainFiltered, rec.Code, rec.Body.String())
		}
		var plainResp ListEnvVarsResponse
		if err := json.NewDecoder(rec.Body).Decode(&plainResp); err != nil {
			t.Fatalf("%s: decode: %v", plainFiltered, err)
		}
		if len(plainResp.EnvVars) != 1 || plainResp.EnvVars[0].Key != "PLAIN_KEY" || plainResp.EnvVars[0].Value != "plain-value" {
			t.Errorf("%s: unexpected filtered plain response: %+v", plainFiltered, plainResp.EnvVars)
		}

		for _, hiddenKey := range []string{"SECRET_ONLY_KEY", "SHADOWED_KEY", "LEGACY_SECRET_ROW", "NO_SUCH_KEY"} {
			p := "/api/v1/env?scope=project&scopeId=" + project.ID + "&key=" + hiddenKey
			hRec := doRequestWithAgentToken(t, srv, http.MethodGet, p, nil, tok)
			if hRec.Code != http.StatusOK {
				t.Fatalf("%s: expected 200 OK with empty list, got %d: %s", p, hRec.Code, hRec.Body.String())
			}
			assertForbiddenLeaks(t, p, hRec.Body.String())
			var hResp ListEnvVarsResponse
			if err := json.NewDecoder(hRec.Body).Decode(&hResp); err != nil {
				t.Fatalf("%s: decode: %v", p, err)
			}
			if len(hResp.EnvVars) != 0 {
				t.Errorf("%s: expected empty EnvVars without %s, got %+v", p, ScopeProjectSecretRead, hResp.EnvVars)
			}
		}
	}

	// Agents with project:read but without project:secret:read (readonly, baseline)
	// reach all 4 project /env read routes and must receive plain env vars only.
	for _, bundle := range []struct {
		name   string
		scopes []AgentTokenScope
	}{
		{"readonly_role", ScopesForRole(AgentRoleReadOnly)},
		{"baseline_role", ScopesForRole(AgentRoleBaseline)},
	} {
		t.Run(bundle.name, func(t *testing.T) {
			tok := srv.generateRecordedAgentToken(t, agent.ID, project.ID, bundle.scopes, nil)
			assertRestrictedList(t, tok, "/api/v1/projects/"+project.ID+"/env")
			assertRestrictedList(t, tok, "/api/v1/env?scope=project&scopeId="+project.ID)
			assertRestrictedFilteredList(t, tok)
			assertRestrictedGetByKey(t, tok, func(key string) string {
				return "/api/v1/projects/" + project.ID + "/env/" + key
			})
			assertRestrictedGetByKey(t, tok, func(key string) string {
				return "/api/v1/env/" + key + "?scope=project&scopeId=" + project.ID
			})
		})
	}

	// status_only_default lacks project:read (so /api/v1/projects/{id}/env* stays 403),
	// and also lacks project:secret:read on /api/v1/env?scope=project&scopeId={id}.
	t.Run("status_only_default", func(t *testing.T) {
		tok := srv.generateRecordedAgentToken(t, agent.ID, project.ID, nil, nil)
		for _, projRoute := range []string{
			"/api/v1/projects/" + project.ID + "/env",
			"/api/v1/projects/" + project.ID + "/env/PLAIN_KEY",
			"/api/v1/projects/" + project.ID + "/env/SECRET_ONLY_KEY",
		} {
			rec := doRequestWithAgentToken(t, srv, http.MethodGet, projRoute, nil, tok)
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s: expected 403 without %s, got %d: %s", projRoute, ScopeProjectRead, rec.Code, rec.Body.String())
			}
		}
		assertRestrictedList(t, tok, "/api/v1/env?scope=project&scopeId="+project.ID)
		assertRestrictedFilteredList(t, tok)
		assertRestrictedGetByKey(t, tok, func(key string) string {
			return "/api/v1/env/" + key + "?scope=project&scopeId=" + project.ID
		})
	})

	// Control: full-role agent (holding project:secret:read) and project owner user
	// remain completely unchanged and continue seeing merged environment-secret metadata.
	assertFullSecretMetadataAccess := func(t *testing.T, label string, doGet func(path string) *http.Response) {
		t.Helper()
		for _, listPath := range []string{
			"/api/v1/projects/" + project.ID + "/env",
			"/api/v1/env?scope=project&scopeId=" + project.ID,
		} {
			resp := doGet(listPath)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s: expected 200 OK, got %d", label, listPath, resp.StatusCode)
			}
			var list ListEnvVarsResponse
			if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
				t.Fatalf("%s %s: decode: %v", label, listPath, err)
			}
			byKey := make(map[string]store.EnvVar, len(list.EnvVars))
			for _, ev := range list.EnvVars {
				byKey[ev.Key] = ev
			}
			if ev, ok := byKey["PLAIN_KEY"]; !ok || ev.Secret || ev.Value != "plain-value" {
				t.Errorf("%s %s: expected plain PLAIN_KEY, got %+v (ok=%v)", label, listPath, ev, ok)
			}
			if ev, ok := byKey["SECRET_ONLY_KEY"]; !ok || !ev.Secret || ev.Value != "********" || ev.Description != "secret-only metadata description" {
				t.Errorf("%s %s: expected secret metadata for SECRET_ONLY_KEY, got %+v (ok=%v)", label, listPath, ev, ok)
			}
			if ev, ok := byKey["SHADOWED_KEY"]; !ok || !ev.Secret || ev.Value != "********" || ev.Description != "shadowing secret metadata description" {
				t.Errorf("%s %s: expected merged secret metadata for SHADOWED_KEY (suppressing plain value), got %+v (ok=%v)", label, listPath, ev, ok)
			}
		}
		for _, getPath := range []string{
			"/api/v1/projects/" + project.ID + "/env/SECRET_ONLY_KEY",
			"/api/v1/env/SECRET_ONLY_KEY?scope=project&scopeId=" + project.ID,
		} {
			resp := doGet(getPath)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s %s: expected 200 OK for SECRET_ONLY_KEY, got %d", label, getPath, resp.StatusCode)
			}
			var ev store.EnvVar
			if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
				t.Fatalf("%s %s: decode: %v", label, getPath, err)
			}
			if !ev.Secret || ev.Value != "********" || ev.Description != "secret-only metadata description" {
				t.Errorf("%s %s: unexpected SECRET_ONLY_KEY EnvVar: %+v", label, getPath, ev)
			}
		}
	}

	t.Run("full_role_agent_unchanged", func(t *testing.T) {
		fullTok := srv.generateRecordedAgentToken(t, agent.ID, project.ID, ScopesForRole(AgentRoleFull), nil)
		assertFullSecretMetadataAccess(t, "full_role", func(path string) *http.Response {
			return doRequestWithAgentToken(t, srv, http.MethodGet, path, nil, fullTok).Result()
		})
	})

	t.Run("project_owner_user_unchanged", func(t *testing.T) {
		assertFullSecretMetadataAccess(t, "project_owner", func(path string) *http.Response {
			return doRequestAsUser(t, srv, owner, http.MethodGet, path, nil).Result()
		})
	})
}

type envSecretFaultBackend struct {
	secret.SecretBackend
	listErr    error
	getMetaErr error
}

func (b *envSecretFaultBackend) List(ctx context.Context, filter secret.Filter) ([]secret.SecretMeta, error) {
	if b.listErr != nil {
		return nil, b.listErr
	}
	return b.SecretBackend.List(ctx, filter)
}

func (b *envSecretFaultBackend) GetMeta(ctx context.Context, name, scope, scopeID string) (*secret.SecretMeta, error) {
	if b.getMetaErr != nil {
		return nil, b.getMetaErr
	}
	return b.SecretBackend.GetMeta(ctx, name, scope, scopeID)
}

func TestEnvVar_ProjectScope_RestrictedAgentFailsClosedOnSecretBackendError(t *testing.T) {
	srv, s := testServer(t)
	localBackend := secret.NewLocalBackend(s, "test-hub-id", "test-secret")
	srv.SetSecretBackend(localBackend)
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_env_failclosed"), Name: "Env Fail Closed Project", Slug: "env-failclosed-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID: tid("agent_env_failclosed"), Slug: "env-failclosed-agent", Name: "Env Fail Closed Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	tok := srv.generateRecordedAgentToken(t, agent.ID, project.ID, ScopesForRole(AgentRoleBaseline), nil)

	// When no plain env vars exist at all, listing as a restricted agent needs no
	// shadow check and succeeds with an empty list even if secretBackend fails.
	fault := &envSecretFaultBackend{
		SecretBackend: localBackend,
		listErr:       errors.New("simulated secret backend list failure"),
		getMetaErr:    errors.New("simulated secret backend getMeta failure"),
	}
	srv.SetSecretBackend(fault)
	emptyRec := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/env", nil, tok)
	if emptyRec.Code != http.StatusOK {
		t.Fatalf("empty project list with failing secret backend: expected 200 OK, got %d: %s", emptyRec.Code, emptyRec.Body.String())
	}

	// Seed a shadowed plain env var, then inject backend errors: neither list nor
	// get-by-key may fail open and leak the shadowed plain value.
	srv.SetSecretBackend(localBackend)
	if rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/env/SHADOWED_KEY", SetEnvVarRequest{
		Value: "shadowed-plain-secret-leak",
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed plain SHADOWED_KEY: got %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/SHADOWED_KEY", SetSecretRequest{
		Value: "c2hhZG93ZWQ=",
		Type:  store.SecretTypeEnvironment,
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed secret SHADOWED_KEY: got %d: %s", rec.Code, rec.Body.String())
	}

	srv.SetSecretBackend(fault)
	for _, path := range []string{
		"/api/v1/projects/" + project.ID + "/env",
		"/api/v1/env?scope=project&scopeId=" + project.ID,
		"/api/v1/projects/" + project.ID + "/env/SHADOWED_KEY",
		"/api/v1/env/SHADOWED_KEY?scope=project&scopeId=" + project.ID,
	} {
		rec := doRequestWithAgentToken(t, srv, http.MethodGet, path, nil, tok)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: expected 500 fail-closed on secret backend error, got %d: %s", path, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "shadowed-plain-secret-leak") {
			t.Errorf("%s: leaked shadowed plain value on backend error: %s", path, rec.Body.String())
		}
	}
}

func TestSecret_ProjectScope_AgentWriteDenied(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_agent_sec_nowrite"), Name: "Agent Secret No Write", Slug: "agent-sec-nowrite-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID: tid("agent_sec_nowrite"), Slug: "sec-nowrite-agent", Name: "Secret No Write Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	agentToken := srv.generateRecordedAgentToken(t, agent.ID, project.ID, nil, nil)

	body := SetSecretRequest{Value: "agent-secret"}
	rec := doRequestWithAgentToken(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/secrets/AGENT_SECRET", body, agentToken)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for agent secret write, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// Hub-Level Env Var with Project/Broker Scope via Query Params
// ============================================================================

func TestEnvVar_HubEndpoint_ProjectScope_Authorized(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_hub_env"), Name: "Hub Env Project", Slug: "hub-env-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Admin should be able to list via hub endpoint with project scope
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/env?scope=project&scopeId="+project.ID, nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for admin project scope, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvVar_HubEndpoint_ProjectScope_NonOwnerDenied(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	member := &store.User{
		ID: tid("hub-env-member"), Email: "hubenvmember@example.com", DisplayName: "Member",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, member); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	project := &store.Project{
		ID: tid("project_hub_env_deny"), Name: "Hub Env Deny Project", Slug: "hub-env-deny-project",
		OwnerID: "someone-else", Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	rec := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/env?scope=project&scopeId="+project.ID, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for non-owner via hub endpoint, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// Secret Promotion & Unified View Tests
// ============================================================================

func TestEnvVar_SecretPromotion_NoBackend_Returns501(t *testing.T) {
	srv, _ := testServer(t)
	// Do NOT set a secret backend — secretBackend is nil

	body := SetEnvVarRequest{
		Value:  "super-secret",
		Secret: true,
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/MY_SECRET_VAR?scope=user", body)
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("expected 501 when secret backend is nil, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvVar_SecretPromotion_LocalBackend_Succeeds(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	// LocalBackend.Set() now works — promotion should succeed with 200
	body := SetEnvVarRequest{
		Value:  "super-secret",
		Secret: true,
	}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/MY_SECRET_VAR?scope=user", body)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for local secret promotion, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvVar_UnifiedList_MergesSecrets(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	// Create a plain env var
	plainBody := SetEnvVarRequest{Value: "plain-value", Description: "A plain var"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/PLAIN_VAR?scope=user", plainBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("failed to create plain env var: %d: %s", rec.Code, rec.Body.String())
	}

	// Create a secret directly in the store with type "environment"
	if err := s.CreateSecret(ctx, &store.Secret{
		ID:             tid("sec-env-1"),
		Key:            "SECRET_ENV_VAR",
		EncryptedValue: "encrypted-val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "SECRET_ENV_VAR",
		Scope:          store.ScopeUser,
		ScopeID:        DevUserID,
		Description:    "A secret env var",
		CreatedBy:      DevUserID,
	}); err != nil {
		t.Fatalf("failed to create secret: %v", err)
	}

	// List env vars — should include both plain and secret
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/env?scope=user", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp ListEnvVarsResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.EnvVars) != 2 {
		t.Fatalf("expected 2 env vars in unified list, got %d", len(resp.EnvVars))
	}

	// Find the secret-backed entry
	var foundSecret bool
	for _, ev := range resp.EnvVars {
		if ev.Key == "SECRET_ENV_VAR" {
			foundSecret = true
			if !ev.Secret {
				t.Error("expected secret=true for secret-backed env var")
			}
			if !ev.Sensitive {
				t.Error("expected sensitive=true for secret-backed env var")
			}
			if ev.Value != "********" {
				t.Errorf("expected masked value, got %q", ev.Value)
			}
		}
	}
	if !foundSecret {
		t.Error("SECRET_ENV_VAR not found in unified list")
	}
}

func TestEnvVar_UnifiedList_Deduplication(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	// Create a plain env var with key "DUPED_KEY"
	plainBody := SetEnvVarRequest{Value: "plain-value"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/DUPED_KEY?scope=user", plainBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("failed to create plain env var: %d: %s", rec.Code, rec.Body.String())
	}

	// Also create a secret with the same key
	if err := s.CreateSecret(ctx, &store.Secret{
		ID:             tid("sec-dup-1"),
		Key:            "DUPED_KEY",
		EncryptedValue: "secret-value",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "DUPED_KEY",
		Scope:          store.ScopeUser,
		ScopeID:        DevUserID,
	}); err != nil {
		t.Fatalf("failed to create secret: %v", err)
	}

	// List — should show only 1 entry (the secret version wins)
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/env?scope=user", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp ListEnvVarsResponse
	_ = json.NewDecoder(rec2.Body).Decode(&resp)

	count := 0
	for _, ev := range resp.EnvVars {
		if ev.Key == "DUPED_KEY" {
			count++
			if !ev.Secret {
				t.Error("expected the secret version to win deduplication")
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 entry for DUPED_KEY, got %d", count)
	}
}

func TestEnvVar_UnifiedList_KeyFiltersSecrets(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	for _, key := range []string{"MATCHING_SECRET", "OTHER_SECRET"} {
		if err := s.CreateSecret(ctx, &store.Secret{
			ID:             tid("sec-key-filter-" + key),
			Key:            key,
			EncryptedValue: "secret-value",
			SecretType:     store.SecretTypeEnvironment,
			Target:         key,
			Scope:          store.ScopeUser,
			ScopeID:        DevUserID,
		}); err != nil {
			t.Fatalf("failed to create %s: %v", key, err)
		}
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/env?scope=user&key=MATCHING_SECRET", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListEnvVarsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.EnvVars) != 1 || resp.EnvVars[0].Key != "MATCHING_SECRET" {
		t.Fatalf("expected only MATCHING_SECRET, got %+v", resp.EnvVars)
	}
}

func TestEnvVar_FallbackGet_FromSecretBackend(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	// Create a secret (no plain env var)
	if err := s.CreateSecret(ctx, &store.Secret{
		ID:             tid("sec-get-1"),
		Key:            "ONLY_SECRET",
		EncryptedValue: "secret-val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "ONLY_SECRET",
		Scope:          store.ScopeUser,
		ScopeID:        DevUserID,
		Description:    "Only in secret backend",
	}); err != nil {
		t.Fatalf("failed to create secret: %v", err)
	}

	// Get via env var endpoint — should fallback to secret backend
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/env/ONLY_SECRET?scope=user", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var envVar store.EnvVar
	if err := json.NewDecoder(rec.Body).Decode(&envVar); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !envVar.Secret {
		t.Error("expected secret=true for fallback env var")
	}
	if envVar.Value != "********" {
		t.Errorf("expected masked value, got %q", envVar.Value)
	}
	if envVar.Key != "ONLY_SECRET" {
		t.Errorf("expected key ONLY_SECRET, got %q", envVar.Key)
	}
}

func TestEnvVar_FallbackDelete_FromSecretBackend(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	// Create a secret (no plain env var)
	if err := s.CreateSecret(ctx, &store.Secret{
		ID:             tid("sec-del-1"),
		Key:            "DEL_SECRET",
		EncryptedValue: "secret-val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "DEL_SECRET",
		Scope:          store.ScopeUser,
		ScopeID:        DevUserID,
	}); err != nil {
		t.Fatalf("failed to create secret: %v", err)
	}

	// Delete via env var endpoint — should fallback to secret backend
	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/env/DEL_SECRET?scope=user", nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify it's gone — get should 404
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/env/DEL_SECRET?scope=user", nil)
	if rec2.Code != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestEnvVar_StaleCleanup_PlainEnvVarRemovedOnPromotion(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	// Create a plain env var
	plainBody := SetEnvVarRequest{Value: "plain-val"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/UPGRADE_ME?scope=user", plainBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("failed to create plain env var: %d: %s", rec.Code, rec.Body.String())
	}

	// Verify it's in the store
	_, err := s.GetEnvVar(ctx, "UPGRADE_ME", store.ScopeUser, DevUserID)
	if err != nil {
		t.Fatalf("expected env var in store, got error: %v", err)
	}

	// Promote to secret — LocalBackend.Set now succeeds, and the handler
	// cleans up the stale plain env var (line 4184 in handlers.go).
	secretBody := SetEnvVarRequest{Value: "secret-val", Secret: true}
	rec2 := doRequest(t, srv, http.MethodPut, "/api/v1/env/UPGRADE_ME?scope=user", secretBody)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 for local secret promotion, got %d: %s", rec2.Code, rec2.Body.String())
	}

	// Plain env var should be removed after successful promotion
	_, err = s.GetEnvVar(ctx, "UPGRADE_ME", store.ScopeUser, DevUserID)
	if err != store.ErrNotFound {
		t.Errorf("plain env var should be removed after promotion, got err: %v", err)
	}
}

func TestEnvVar_NonEnvironmentSecrets_NotMerged(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	// Create a secret with type "variable" (not "environment")
	if err := s.CreateSecret(ctx, &store.Secret{
		ID:             tid("sec-var-1"),
		Key:            "VARIABLE_SECRET",
		EncryptedValue: "var-val",
		SecretType:     store.SecretTypeVariable,
		Target:         "VARIABLE_SECRET",
		Scope:          store.ScopeUser,
		ScopeID:        DevUserID,
	}); err != nil {
		t.Fatalf("failed to create secret: %v", err)
	}

	// List env vars — should NOT include the variable-type secret
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/env?scope=user", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp ListEnvVarsResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)

	for _, ev := range resp.EnvVars {
		if ev.Key == "VARIABLE_SECRET" {
			t.Error("variable-type secret should not appear in env var list")
		}
	}
}

func TestEnvVar_ProjectScope_SecretPromotion_Succeeds(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_promo_test"), Name: "Promo Project", Slug: "promo-project",
		OwnerID: DevUserID, Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	body := SetEnvVarRequest{Value: "secret-val", Secret: true}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/env/PROJECT_SECRET", body)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for project secret promotion with LocalBackend, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvVar_ProjectScope_UnifiedList(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_unified_list"), Name: "Unified Project", Slug: "unified-project",
		OwnerID: DevUserID, Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a plain project env var
	plainBody := SetEnvVarRequest{Value: "project-plain"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/env/PROJECT_PLAIN", plainBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("failed to create project env var: %d: %s", rec.Code, rec.Body.String())
	}

	// Create an environment secret in the project scope directly
	if err := s.CreateSecret(ctx, &store.Secret{
		ID:             tid("sec-project-env-1"),
		Key:            "PROJECT_SECRET_VAR",
		EncryptedValue: "project-secret-val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "PROJECT_SECRET_VAR",
		Scope:          store.ScopeProject,
		ScopeID:        project.ID,
	}); err != nil {
		t.Fatalf("failed to create project secret: %v", err)
	}

	// List project env vars — should include both
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/env", nil)
	if rec2.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp ListEnvVarsResponse
	_ = json.NewDecoder(rec2.Body).Decode(&resp)

	if len(resp.EnvVars) != 2 {
		t.Errorf("expected 2 env vars in project unified list, got %d", len(resp.EnvVars))
	}

	var foundSecret bool
	for _, ev := range resp.EnvVars {
		if ev.Key == "PROJECT_SECRET_VAR" {
			foundSecret = true
			if !ev.Secret {
				t.Error("expected secret=true")
			}
		}
	}
	if !foundSecret {
		t.Error("PROJECT_SECRET_VAR not found in project unified list")
	}
}

func TestEnvVar_ProjectScope_FallbackGet(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_fallback_get"), Name: "Fallback Get Project", Slug: "fallback-get-project",
		OwnerID: DevUserID, Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	if err := s.CreateSecret(ctx, &store.Secret{
		ID:             tid("sec-project-fb-1"),
		Key:            "PROJECT_ONLY_SEC",
		EncryptedValue: "secret-val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "PROJECT_ONLY_SEC",
		Scope:          store.ScopeProject,
		ScopeID:        project.ID,
	}); err != nil {
		t.Fatalf("failed to create secret: %v", err)
	}

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/env/PROJECT_ONLY_SEC", nil)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for project fallback get, got %d: %s", rec.Code, rec.Body.String())
	}

	var envVar store.EnvVar
	_ = json.NewDecoder(rec.Body).Decode(&envVar)
	if !envVar.Secret {
		t.Error("expected secret=true from fallback")
	}
}

func TestEnvVar_ProjectScope_FallbackDelete(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_fallback_del"), Name: "Fallback Del Project", Slug: "fallback-del-project",
		OwnerID: DevUserID, Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	if err := s.CreateSecret(ctx, &store.Secret{
		ID:             tid("sec-project-del-1"),
		Key:            "PROJECT_DEL_SEC",
		EncryptedValue: "secret-val",
		SecretType:     store.SecretTypeEnvironment,
		Target:         "PROJECT_DEL_SEC",
		Scope:          store.ScopeProject,
		ScopeID:        project.ID,
	}); err != nil {
		t.Fatalf("failed to create secret: %v", err)
	}

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/projects/"+project.ID+"/env/PROJECT_DEL_SEC", nil)
	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204 for project fallback delete, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// Hub-Scoped Env Var Authorization Tests
// ============================================================================

func TestEnvVar_HubScope_AdminCanSetAndGet(t *testing.T) {
	srv, _ := testServer(t)

	// Admin (dev-user) should be able to set hub-scoped env vars
	body := SetEnvVarRequest{Value: "hub-value", Description: "Hub-wide default", Scope: "hub"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/HUB_VAR", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for admin hub scope write, got %d: %s", rec.Code, rec.Body.String())
	}

	var setResp SetEnvVarResponse
	if err := json.NewDecoder(rec.Body).Decode(&setResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if !setResp.Created {
		t.Error("expected created=true for new hub env var")
	}

	// Admin should be able to get hub-scoped env vars
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/env/HUB_VAR?scope=hub", nil)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 for admin hub scope read, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestEnvVar_HubScope_AdminCanList(t *testing.T) {
	srv, _ := testServer(t)

	// Set a hub-scoped env var first
	body := SetEnvVarRequest{Value: "hub-list-val", Scope: "hub"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/HUB_LIST_VAR", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// List hub-scoped env vars
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/env?scope=hub", nil)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 for admin hub scope list, got %d: %s", rec2.Code, rec2.Body.String())
	}

	var resp ListEnvVarsResponse
	if err := json.NewDecoder(rec2.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if resp.ScopeID != "test-hub-id" {
		t.Errorf("expected scopeId %q, got %q", "test-hub-id", resp.ScopeID)
	}
}

func TestEnvVar_HubScope_AdminCanDelete(t *testing.T) {
	srv, _ := testServer(t)

	// Create then delete a hub-scoped env var
	body := SetEnvVarRequest{Value: "to-delete", Scope: "hub"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/HUB_DEL_VAR", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec2 := doRequest(t, srv, http.MethodDelete, "/api/v1/env/HUB_DEL_VAR?scope=hub", nil)
	if rec2.Code != http.StatusNoContent {
		t.Errorf("expected 204 for hub scope delete, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestEnvVar_HubScope_MemberReadForbidden(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	member := &store.User{
		ID: tid("hub-env-member-1"), Email: "hub-member@example.com", DisplayName: "Hub Member",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, member); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Admin creates a hub env var
	body := SetEnvVarRequest{Value: "hub-shared", Scope: "hub"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/HUB_SHARED", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Non-admin member should be forbidden from reading hub-scoped env vars
	rec2 := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/env?scope=hub", nil)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("expected 403 for member hub scope read, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestEnvVar_HubScope_MemberWriteForbidden(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	member := &store.User{
		ID: tid("hub-env-member-2"), Email: "hub-member2@example.com", DisplayName: "Hub Member 2",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, member); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Non-admin member should be forbidden from writing hub-scoped env vars
	body := SetEnvVarRequest{Value: "should-fail", Scope: "hub"}
	rec := doRequestAsUser(t, srv, member, http.MethodPut, "/api/v1/env/HUB_FAIL", body)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for member hub scope write, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEnvVar_HubScope_MemberDeleteForbidden(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	member := &store.User{
		ID: tid("hub-env-member-3"), Email: "hub-member3@example.com", DisplayName: "Hub Member 3",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, member); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Admin creates a hub env var
	body := SetEnvVarRequest{Value: "hub-owned", Scope: "hub"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/HUB_OWNED", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Non-admin member should be forbidden from deleting hub-scoped env vars
	rec2 := doRequestAsUser(t, srv, member, http.MethodDelete, "/api/v1/env/HUB_OWNED?scope=hub", nil)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("expected 403 for member hub scope delete, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestEnvVar_HubScope_AgentCanRead(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID: tid("project_hub_agent"), Name: "Hub Agent Project", Slug: "hub-agent-project",
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateProject(ctx, project); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	agent := &store.Agent{
		ID: tid("agent_hub_read"), Slug: "hub-read-agent", Name: "Hub Read Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	if err := s.CreateAgent(ctx, agent); err != nil {
		t.Fatalf("failed to create agent: %v", err)
	}

	agentToken := srv.generateRecordedAgentToken(t, agent.ID, project.ID, nil, nil)

	// Admin creates a hub env var
	body := SetEnvVarRequest{Value: "hub-agent-val", Scope: "hub"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/env/HUB_AGENT_VAR", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Agent should be able to read hub-scoped env vars
	rec2 := doRequestWithAgentToken(t, srv, http.MethodGet, "/api/v1/env?scope=hub", nil, agentToken)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 for agent hub scope read, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestEnvVar_HubScope_Unauthenticated(t *testing.T) {
	srv, _ := testServer(t)

	// Unauthenticated should get 401
	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/env?scope=hub", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated hub scope, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ============================================================================
// Hub-Scoped Secrets Authorization Tests
// ============================================================================

func TestSecret_HubScope_AdminCanSetAndGet(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))

	// Admin should be able to set hub-scoped secrets
	body := SetSecretRequest{Value: "aHViLXNlY3JldC12YWw=", Description: "Hub-wide secret", Scope: "hub"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/HUB_SECRET", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for admin hub scope secret write, got %d: %s", rec.Code, rec.Body.String())
	}

	// Admin should be able to get hub-scoped secret metadata
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/secrets/HUB_SECRET?scope=hub", nil)
	if rec2.Code != http.StatusOK {
		t.Errorf("expected 200 for admin hub scope secret read, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestSecret_HubScope_MemberReadForbidden(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	member := &store.User{
		ID: tid("hub-sec-member-1"), Email: "hubsecmember@example.com", DisplayName: "Hub Sec Member",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, member); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Admin creates a hub secret
	body := SetSecretRequest{Value: "aHViLXNoYXJlZC1zZWNyZXQ=", Scope: "hub"}
	rec := doRequest(t, srv, http.MethodPut, "/api/v1/secrets/HUB_SHARED_SEC", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Non-admin member should be forbidden from reading hub-scoped secrets
	rec2 := doRequestAsUser(t, srv, member, http.MethodGet, "/api/v1/secrets?scope=hub", nil)
	if rec2.Code != http.StatusForbidden {
		t.Errorf("expected 403 for member hub scope secret read, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

func TestSecret_HubScope_MemberWriteForbidden(t *testing.T) {
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	ctx := context.Background()

	member := &store.User{
		ID: tid("hub-sec-member-2"), Email: "hubsecmember2@example.com", DisplayName: "Hub Sec Member 2",
		Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	if err := s.CreateUser(ctx, member); err != nil {
		t.Fatalf("failed to create user: %v", err)
	}

	// Non-admin member should be forbidden from writing hub-scoped secrets
	body := SetSecretRequest{Value: "c2hvdWxkLWZhaWw=", Scope: "hub"}
	rec := doRequestAsUser(t, srv, member, http.MethodPut, "/api/v1/secrets/HUB_FAIL_SEC", body)
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for member hub scope secret write, got %d: %s", rec.Code, rec.Body.String())
	}
}
