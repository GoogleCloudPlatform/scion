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
		ID:   "project-1",
		Name: "Test Project",
		Slug: "test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create running agents
	for i, name := range []string{"agent-1", "agent-2", "agent-3"} {
		agent := &store.Agent{
			ID:      name,
			Slug:    name,
			Name:    name,
			ProjectID: project.ID,
			Phase:   string(state.PhaseRunning),
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
		for _, name := range []string{"agent-1", "agent-2"} {
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
	project1 := &store.Project{ID: "project-1", Name: "Project 1", Slug: "project-1"}
	project2 := &store.Project{ID: "project-2", Name: "Project 2", Slug: "project-2"}
	require.NoError(t, s.CreateProject(ctx, project1))
	require.NoError(t, s.CreateProject(ctx, project2))

	// Create running agents in both projects
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "g1-agent-1", Slug: "g1-agent-1", Name: "G1 Agent 1",
		ProjectID: project1.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "g1-agent-2", Slug: "g1-agent-2", Name: "G1 Agent 2",
		ProjectID: project1.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "g2-agent-1", Slug: "g2-agent-1", Name: "G2 Agent 1",
		ProjectID: project2.ID, Phase: string(state.PhaseRunning),
	}))

	t.Run("stops only agents in scoped project", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/project-1/agents/stop-all", nil)
		assert.Equal(t, http.StatusOK, rec.Code)

		var resp StopAllAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

		assert.Equal(t, 2, resp.Stopped)
		assert.Equal(t, 0, resp.Failed)
		assert.Equal(t, 2, resp.Total)

		// Verify project-1 agents are stopped
		a1, _ := s.GetAgent(ctx, "g1-agent-1")
		assert.Equal(t, string(state.PhaseStopped), a1.Phase)

		// Verify project-2 agent is still running
		a2, _ := s.GetAgent(ctx, "g2-agent-1")
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
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "alice-agent", Slug: "alice-agent", Name: "Alice Agent",
		ProjectID: project.ID, OwnerID: alice.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "other-agent", Slug: "other-agent", Name: "Other Agent",
		ProjectID: project.ID, OwnerID: "user-other", Phase: string(state.PhaseRunning),
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
	a1, _ := s.GetAgent(ctx, "alice-agent")
	assert.Equal(t, string(state.PhaseStopped), a1.Phase)
	a2, _ := s.GetAgent(ctx, "other-agent")
	assert.Equal(t, string(state.PhaseStopped), a2.Phase)
}

func TestStopAllAgents_ProjectMember_StopsOnlyOwnAgents(t *testing.T) {
	srv, s, _, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create a third user "carol" as a regular project member
	carol := &store.User{
		ID:          "user-carol",
		Email:       "carol@test.com",
		DisplayName: "Carol",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, carol))
	ensureHubMembership(ctx, s, carol.ID)

	// Add carol as a regular member of the project's members group
	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    membersGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   carol.ID,
		Role:       store.GroupMemberRoleMember,
	}))

	// Create agents owned by carol and by alice
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "carol-agent-1", Slug: "carol-agent-1", Name: "Carol Agent 1",
		ProjectID: project.ID, OwnerID: carol.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "carol-agent-2", Slug: "carol-agent-2", Name: "Carol Agent 2",
		ProjectID: project.ID, OwnerID: carol.ID, Phase: string(state.PhaseRunning),
	}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "alice-agent", Slug: "alice-agent", Name: "Alice Agent",
		ProjectID: project.ID, OwnerID: "user-alice", Phase: string(state.PhaseRunning),
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
	c1, _ := s.GetAgent(ctx, "carol-agent-1")
	assert.Equal(t, string(state.PhaseStopped), c1.Phase)
	c2, _ := s.GetAgent(ctx, "carol-agent-2")
	assert.Equal(t, string(state.PhaseStopped), c2.Phase)

	// Verify alice's agent is still running
	a1, _ := s.GetAgent(ctx, "alice-agent")
	assert.Equal(t, string(state.PhaseRunning), a1.Phase)
}

func TestStopAllAgents_NonMember_Forbidden(t *testing.T) {
	srv, s, _, bob, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	// Create a running agent in the project
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{
		ID: "agent-1", Slug: "agent-1", Name: "Agent 1",
		ProjectID: project.ID, Phase: string(state.PhaseRunning),
	}))

	// Bob is NOT a project member — should get 403
	rec := doRequestAsUser(t, srv, bob, http.MethodPost,
		"/api/v1/projects/"+project.ID+"/agents/stop-all", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// Agent should still be running
	a, _ := s.GetAgent(ctx, "agent-1")
	assert.Equal(t, string(state.PhaseRunning), a.Phase)
}

func TestStopAllAgents_Global_NonAdmin_Forbidden(t *testing.T) {
	srv, _, alice, _, _ := setupDemoPolicyTest(t)

	// Alice is a regular user (not platform admin) — global stop-all should be denied
	rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/agents/stop-all", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestStopAllAgents_ScopeCapabilities_ProjectMember(t *testing.T) {
	srv, _, alice, bob, project := setupDemoPolicyTest(t)

	// Alice (project member) should see stop_all in project-scoped capabilities
	rec := doRequestAsUser(t, srv, alice, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/agents", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp struct {
		Capabilities *Capabilities `json:"_capabilities"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Capabilities)
	assert.Contains(t, resp.Capabilities.Actions, "stop_all",
		"project member should have stop_all in scope capabilities")

	// Bob (non-member) should NOT see stop_all in project-scoped capabilities
	rec = doRequestAsUser(t, srv, bob, http.MethodGet,
		"/api/v1/projects/"+project.ID+"/agents", nil)
	assert.Equal(t, http.StatusOK, rec.Code)

	var resp2 struct {
		Capabilities *Capabilities `json:"_capabilities"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp2))
	require.NotNil(t, resp2.Capabilities)
	assert.NotContains(t, resp2.Capabilities.Actions, "stop_all",
		"non-member should not have stop_all in scope capabilities")
}
