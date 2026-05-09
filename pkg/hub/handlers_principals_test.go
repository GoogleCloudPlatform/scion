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
// Principal Query Endpoint Tests (Phase 4)
// ============================================================================

func TestMyGroups_Authenticated(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/users/me/groups", nil)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp GroupsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	// Dev user has no group memberships in SQLite store
	assert.NotNil(t, resp.Groups)
	assert.Len(t, resp.Groups, 0)
}

func TestMyGroups_Unauthenticated(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/users/me/groups", nil)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestMyGroups_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/users/me/groups", nil)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestAgentGroups(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-1",
		Name: "Test Project",
		Slug: "test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:      "agent-1",
		Slug:    "agent-1-slug",
		Name:    "Agent 1",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/agent-1/groups", nil)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp GroupsResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.NotNil(t, resp.Groups)
}

func TestAgentGroups_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/nonexistent/groups", nil)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestPrincipalResolve_User(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	user := &store.User{
		ID:          "user-1",
		Email:       "alice@example.com",
		DisplayName: "Alice",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/principals/user/user-1", nil)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp PrincipalResolutionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "user", resp.Principal.Type)
	assert.Equal(t, "user-1", resp.Principal.ID)
	assert.Equal(t, "Alice", resp.Principal.DisplayName)
	assert.NotNil(t, resp.DirectGroups)
	assert.NotNil(t, resp.EffectiveGroups)
}

func TestPrincipalResolve_Agent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-1",
		Name: "Test Project",
		Slug: "test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:      "agent-1",
		Slug:    "agent-1-slug",
		Name:    "Agent 1",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/principals/agent/agent-1", nil)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp PrincipalResolutionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "agent", resp.Principal.Type)
	assert.Equal(t, "agent-1", resp.Principal.ID)
	assert.Equal(t, "Agent 1", resp.Principal.DisplayName)
	assert.Equal(t, "project-1", resp.Principal.ProjectID)
	assert.NotNil(t, resp.DirectGroups)
	assert.NotNil(t, resp.EffectiveGroups)
}

func TestPrincipalResolve_Group(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	group := &store.Group{
		ID:          "group-1",
		Name:        "Platform Team",
		Slug:        "platform-team",
		Description: "The platform team",
	}
	require.NoError(t, s.CreateGroup(ctx, group))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/principals/group/group-1", nil)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp PrincipalResolutionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "group", resp.Principal.Type)
	assert.Equal(t, "group-1", resp.Principal.ID)
	assert.Equal(t, "Platform Team", resp.Principal.DisplayName)
	assert.Empty(t, resp.DirectGroups)
	assert.Empty(t, resp.EffectiveGroups)
	assert.Nil(t, resp.DelegatesFrom)
}

func TestPrincipalResolve_NotFound(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/principals/user/nonexistent", nil)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestPrincipalResolve_InvalidType(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/principals/invalid/some-id", nil)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPrincipalResolve_MissingID(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/principals/user/", nil)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPrincipalResolve_MethodNotAllowed(t *testing.T) {
	srv, _ := testServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/principals/user/some-id", nil)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestPrincipalResolve_AgentWithCreator(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a user as the agent's creator
	user := &store.User{
		ID:          "creator-1",
		Email:       "creator@example.com",
		DisplayName: "Creator User",
		Role:        "member",
		Status:      "active",
	}
	require.NoError(t, s.CreateUser(ctx, user))

	project := &store.Project{
		ID:   "project-1",
		Name: "Test Project",
		Slug: "test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:        "agent-deleg",
		Slug:      "agent-deleg-slug",
		Name:      "Delegated Agent",
		ProjectID:   project.ID,
		Phase:     string(state.PhaseRunning),
		CreatedBy: "creator-1",
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/principals/agent/agent-deleg", nil)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp PrincipalResolutionResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "agent", resp.Principal.Type)
	assert.Equal(t, "agent-deleg", resp.Principal.ID)
	// Should include delegation info
	require.NotNil(t, resp.DelegatesFrom)
	assert.Equal(t, "user", resp.DelegatesFrom.Type)
	assert.Equal(t, "creator-1", resp.DelegatesFrom.ID)
	assert.Equal(t, "Creator User", resp.DelegatesFrom.DisplayName)
}
