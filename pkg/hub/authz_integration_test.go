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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ============================================================================
// Integration Tests for Policy Evaluation
// ============================================================================

func TestEvaluateEndpoint_UserDirectPolicy(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create user
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: "eval-user-1", Email: "eval1@test.com", DisplayName: "Eval User", Role: "member", Status: "active",
	}))

	// Create policy via API
	policyReq := CreatePolicyRequest{
		Name:         "Allow Read Agents",
		ScopeType:    "hub",
		ResourceType: "agent",
		Actions:      []string{"read"},
		Effect:       "allow",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/policies", policyReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var createdPolicy store.Policy
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&createdPolicy))

	// Add binding via API
	bindReq := AddPolicyBindingRequest{
		PrincipalType: "user",
		PrincipalID:   "eval-user-1",
	}
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/policies/"+createdPolicy.ID+"/bindings", bindReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Evaluate via API
	evalReq := EvaluateRequest{
		PrincipalType: "user",
		PrincipalID:   "eval-user-1",
		ResourceType:  "agent",
		ResourceID:    "agent-1",
		Action:        "read",
	}
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/policies/evaluate", evalReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var evalResp EvaluateResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&evalResp))
	assert.True(t, evalResp.Allowed)
	assert.Equal(t, createdPolicy.ID, evalResp.MatchedPolicy)
}

func TestEvaluateEndpoint_DefaultDeny(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: "eval-user-none", Email: "none@test.com", DisplayName: "No Policy", Role: "member", Status: "active",
	}))

	evalReq := EvaluateRequest{
		PrincipalType: "user",
		PrincipalID:   "eval-user-none",
		ResourceType:  "agent",
		Action:        "delete",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/policies/evaluate", evalReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var evalResp EvaluateResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&evalResp))
	assert.False(t, evalResp.Allowed)
	assert.Equal(t, "default deny", evalResp.Reason)
}

func TestEvaluateEndpoint_ScopeOverride(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: "eval-user-scope", Email: "scope@test.com", DisplayName: "Scope User", Role: "member", Status: "active",
	}))

	// Create hub-level deny
	hubPolicy := &store.Policy{
		ID: "hub-deny-1", Name: "Hub Deny", ScopeType: "hub",
		ResourceType: "agent", Actions: []string{"read"}, Effect: "deny",
	}
	require.NoError(t, s.CreatePolicy(ctx, hubPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: "hub-deny-1", PrincipalType: "user", PrincipalID: "eval-user-scope",
	}))

	// Create project-level allow (should override hub deny)
	projectPolicy := &store.Policy{
		ID: "project-allow-1", Name: "Project Allow", ScopeType: "project",
		ScopeID: "project-scope-1", ResourceType: "agent",
		Actions: []string{"read"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, projectPolicy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: "project-allow-1", PrincipalType: "user", PrincipalID: "eval-user-scope",
	}))

	evalReq := EvaluateRequest{
		PrincipalType: "user",
		PrincipalID:   "eval-user-scope",
		ResourceType:  "agent",
		Action:        "read",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/policies/evaluate", evalReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var evalResp EvaluateResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&evalResp))
	assert.True(t, evalResp.Allowed)
	assert.Equal(t, "project", evalResp.Scope)
}

func TestEvaluateEndpoint_AgentPolicy(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project and agent
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: "project-eval", Name: "Eval Project", Slug: "project-eval",
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "agent-eval", Slug: "agent-eval", Name: "Eval Agent",
		ProjectID: "project-eval", Phase: string(state.PhaseRunning),
	}))

	// Create and bind policy to agent
	policy := &store.Policy{
		ID: "agent-policy-eval", Name: "Agent Read", ScopeType: "hub",
		ResourceType: "project", Actions: []string{"read"}, Effect: "allow",
	}
	require.NoError(t, s.CreatePolicy(ctx, policy))
	require.NoError(t, s.AddPolicyBinding(ctx, &store.PolicyBinding{
		PolicyID: "agent-policy-eval", PrincipalType: "agent", PrincipalID: "agent-eval",
	}))

	evalReq := EvaluateRequest{
		PrincipalType: "agent",
		PrincipalID:   "agent-eval",
		ResourceType:  "project",
		Action:        "read",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/policies/evaluate", evalReq)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var evalResp EvaluateResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&evalResp))
	assert.True(t, evalResp.Allowed)
}

func TestEvaluateEndpoint_AgentBinding(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project and agent
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: "project-bind", Name: "Bind Project", Slug: "project-bind",
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "agent-bind", Slug: "agent-bind", Name: "Bind Agent",
		ProjectID: "project-bind", Phase: string(state.PhaseRunning),
	}))

	// Create policy via API
	policyReq := CreatePolicyRequest{
		Name:         "Agent Manage",
		ScopeType:    "hub",
		ResourceType: "agent",
		Actions:      []string{"manage"},
		Effect:       "allow",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/policies", policyReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var createdPolicy store.Policy
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&createdPolicy))

	// Bind to agent (tests that "agent" is now a valid principal type)
	bindReq := AddPolicyBindingRequest{
		PrincipalType: "agent",
		PrincipalID:   "agent-bind",
	}
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/policies/"+createdPolicy.ID+"/bindings", bindReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// Verify binding exists
	rec = doRequest(t, srv, http.MethodGet, "/api/v1/policies/"+createdPolicy.ID+"/bindings", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var bindingsResp ListPolicyBindingsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&bindingsResp))
	assert.Len(t, bindingsResp.Bindings, 1)
	assert.Equal(t, "agent", bindingsResp.Bindings[0].PrincipalType)
}

func TestEvaluateEndpoint_Validation(t *testing.T) {
	srv, _ := testServer(t)

	tests := []struct {
		name string
		body EvaluateRequest
	}{
		{"missing principal", EvaluateRequest{ResourceType: "agent", Action: "read"}},
		{"missing resource type", EvaluateRequest{PrincipalType: "user", PrincipalID: "u1", Action: "read"}},
		{"missing action", EvaluateRequest{PrincipalType: "user", PrincipalID: "u1", ResourceType: "agent"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/policies/evaluate", tt.body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
}

func TestEvaluateEndpoint_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/policies/evaluate", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestEvaluateEndpoint_CreatedByPopulated(t *testing.T) {
	srv, _ := testServer(t)

	policyReq := CreatePolicyRequest{
		Name:         "Created By Test",
		ScopeType:    "hub",
		ResourceType: "*",
		Actions:      []string{"*"},
		Effect:       "allow",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/policies", policyReq)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var createdPolicy store.Policy
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&createdPolicy))
	// Dev auth should set CreatedBy
	assert.NotEmpty(t, createdPolicy.CreatedBy)
}
