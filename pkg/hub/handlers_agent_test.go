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
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentStatusUpdate_Authorization(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:   "project-1",
		Name: "Test Project",
		Slug: "test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create two agents
	agent1 := &store.Agent{
		ID:      "agent-1",
		Slug:    "agent-1-slug",
		Name:    "Agent 1",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))

	agent2 := &store.Agent{
		ID:      "agent-2",
		Slug:    "agent-2-slug",
		Name:    "Agent 2",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	// Get agent token service
	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	// Generate token for agent 1
	token1, err := tokenSvc.GenerateAgentToken(agent1.ID, project.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

	t.Run("Agent 1 can update its own status", func(t *testing.T) {
		status := store.AgentStatusUpdate{
			Activity: "idle",
			Message:  "Waiting for user input",
		}
		body, _ := json.Marshal(status)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-1/status", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", token1)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		// Verify update in store
		updated, err := s.GetAgent(ctx, agent1.ID)
		require.NoError(t, err)
		assert.Equal(t, "idle", updated.Activity)
		assert.Equal(t, "Waiting for user input", updated.Message)
	})

	t.Run("Agent 1 cannot update Agent 2's status", func(t *testing.T) {
		status := store.AgentStatusUpdate{
			Phase: "error",
		}
		body, _ := json.Marshal(status)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-2/status", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", token1)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("Agent 1 cannot perform lifecycle actions", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-1/stop", nil)
		req.Header.Set("X-Scion-Agent-Token", token1)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("User can update agent status", func(t *testing.T) {
		status := store.AgentStatusUpdate{
			Phase: "running",
		}
		body, _ := json.Marshal(status)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-1/status", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+testDevToken)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)

		updated, err := s.GetAgent(ctx, agent1.ID)
		require.NoError(t, err)
		assert.Equal(t, "running", updated.Phase)
	})
}

func TestAgentStatusUpdate_Heartbeat(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:   "project-h",
		Name: "Heartbeat Project",
		Slug: "heartbeat-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create an agent
	agent := &store.Agent{
		ID:      "agent-h",
		Slug:    "agent-h-slug",
		Name:    "Agent Heartbeat",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Record initial update time
	initial, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	initialTime := initial.LastSeen

	// Small delay to ensure timestamp changes
	time.Sleep(10 * time.Millisecond)

	// Send heartbeat
	status := store.AgentStatusUpdate{
		Phase:     string(state.PhaseRunning),
		Heartbeat: true,
	}
	body, _ := json.Marshal(status)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-h/status", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testDevToken)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify update in store
	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, updated.LastSeen.After(initialTime), "LastSeen should be updated")
}

// setupOfflineBrokerAgent creates a project, an offline broker, and an agent assigned to that broker.
func setupOfflineBrokerAgent(t *testing.T, s store.Store, suffix string) (*store.Project, *store.RuntimeBroker, *store.Agent) {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{
		ID:   fmt.Sprintf("project-offline-%s", suffix),
		Name: fmt.Sprintf("Offline Project %s", suffix),
		Slug: fmt.Sprintf("offline-project-%s", suffix),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     fmt.Sprintf("broker-offline-%s", suffix),
		Name:   fmt.Sprintf("Offline Broker %s", suffix),
		Slug:   fmt.Sprintf("offline-broker-%s", suffix),
		Status: store.BrokerStatusOffline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID:              fmt.Sprintf("agent-offline-%s", suffix),
		Slug:            fmt.Sprintf("agent-offline-%s-slug", suffix),
		Name:            fmt.Sprintf("Agent Offline %s", suffix),
		ProjectID:         project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return project, broker, agent
}

func TestDeleteAgent_BrokerOffline(t *testing.T) {
	srv, s := testServer(t)

	_, _, agent := setupOfflineBrokerAgent(t, s, "del")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// Verify agent was NOT deleted
	ctx := context.Background()
	_, err := s.GetAgent(ctx, agent.ID)
	assert.NoError(t, err, "agent should still exist when broker is offline")
}

func TestDeleteAgent_NoBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-nobroker",
		Name: "No Broker Project",
		Slug: "no-broker-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:      "agent-nobroker",
		Slug:    "agent-nobroker-slug",
		Name:    "Agent No Broker",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
		// No RuntimeBrokerID set
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify agent was deleted
	_, err := s.GetAgent(ctx, agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// deleteDispatcher tracks whether DispatchAgentDelete was called and can simulate errors.
type deleteDispatcher struct {
	createAgentDispatcher
	deleteErr        error
	deleteCalls      int
	lastDeleteFiles  bool
	lastRemoveBranch bool
}

func (d *deleteDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, deleteFiles, removeBranch, _ bool, _ time.Time) error {
	d.deleteCalls++
	d.lastDeleteFiles = deleteFiles
	d.lastRemoveBranch = removeBranch
	return d.deleteErr
}

// setupOnlineBrokerAgent creates a project, an online broker, and an agent assigned to that broker.
func setupOnlineBrokerAgent(t *testing.T, s store.Store, suffix string) (*store.Project, *store.RuntimeBroker, *store.Agent) {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{
		ID:   fmt.Sprintf("project-online-%s", suffix),
		Name: fmt.Sprintf("Online Project %s", suffix),
		Slug: fmt.Sprintf("online-project-%s", suffix),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:       fmt.Sprintf("broker-online-%s", suffix),
		Name:     fmt.Sprintf("Online Broker %s", suffix),
		Slug:     fmt.Sprintf("online-broker-%s", suffix),
		Status:   store.BrokerStatusOnline,
		Endpoint: "http://localhost:9800",
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID:              fmt.Sprintf("agent-online-%s", suffix),
		Slug:            fmt.Sprintf("agent-online-%s-slug", suffix),
		Name:            fmt.Sprintf("Agent Online %s", suffix),
		ProjectID:         project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return project, broker, agent
}

func TestDeleteAgent_DispatchesToBroker(t *testing.T) {
	srv, s := testServer(t)

	disp := &deleteDispatcher{}
	srv.SetDispatcher(disp)

	_, _, agent := setupOnlineBrokerAgent(t, s, "dispatch")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify dispatch was called with correct defaults
	assert.Equal(t, 1, disp.deleteCalls, "DispatchAgentDelete should be called once")
	assert.True(t, disp.lastDeleteFiles, "deleteFiles should default to true")
	assert.True(t, disp.lastRemoveBranch, "removeBranch should default to true")

	// Verify agent was deleted from hub
	ctx := context.Background()
	_, err := s.GetAgent(ctx, agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteAgent_DispatchFailure_ReturnsError(t *testing.T) {
	srv, s := testServer(t)

	disp := &deleteDispatcher{
		deleteErr: fmt.Errorf("broker connection refused"),
	}
	srv.SetDispatcher(disp)

	_, _, agent := setupOnlineBrokerAgent(t, s, "fail")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusBadGateway, rec.Code)

	// Verify error response
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeRuntimeError, errResp.Error.Code)

	// Verify agent was NOT deleted from hub (dispatch failed)
	ctx := context.Background()
	_, err := s.GetAgent(ctx, agent.ID)
	assert.NoError(t, err, "agent should still exist when broker dispatch fails")
}

func TestDeleteAgent_DispatchFailure_ForceDeleteSucceeds(t *testing.T) {
	srv, s := testServer(t)

	disp := &deleteDispatcher{
		deleteErr: fmt.Errorf("broker connection refused"),
	}
	srv.SetDispatcher(disp)

	_, _, agent := setupOnlineBrokerAgent(t, s, "force")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID+"?force=true", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify agent was deleted from hub despite dispatch failure
	ctx := context.Background()
	_, err := s.GetAgent(ctx, agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteAgent_PreserveFiles(t *testing.T) {
	srv, s := testServer(t)

	disp := &deleteDispatcher{}
	srv.SetDispatcher(disp)

	_, _, agent := setupOnlineBrokerAgent(t, s, "preserve")

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID+"?deleteFiles=false&removeBranch=false", nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify dispatch was called with explicit false values
	assert.Equal(t, 1, disp.deleteCalls)
	assert.False(t, disp.lastDeleteFiles, "deleteFiles should be false when explicitly set")
	assert.False(t, disp.lastRemoveBranch, "removeBranch should be false when explicitly set")
}

func TestAgentLifecycle_BrokerOffline(t *testing.T) {
	srv, s := testServer(t)

	_, _, agent := setupOfflineBrokerAgent(t, s, "lc")

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// Verify the error code
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeRuntimeBrokerUnavail, errResp.Error.Code)
}

// ============================================================================
// Agent-as-Caller Tests (Sub-Agent Creation & Lifecycle)
// ============================================================================

func TestAgentCreateAgent_WithScope(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:   "project-parent",
		Name: "Parent Project",
		Slug: "parent-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create a runtime broker and provider for the project
	broker := &store.RuntimeBroker{
		ID:     "broker-parent",
		Name:   "Parent Broker",
		Slug:   "parent-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	contrib := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, contrib))

	// Update project default broker
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	// Create the calling agent
	callingAgent := &store.Agent{
		ID:      "agent-caller",
		Slug:    "agent-caller",
		Name:    "Calling Agent",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, callingAgent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	t.Run("Agent with project:agent:create scope can create agent in same project", func(t *testing.T) {
		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
			ScopeAgentCreate,
		}, nil)
		require.NoError(t, err)

		body, _ := json.Marshal(CreateAgentRequest{
			Name:    "Sub Agent",
			ProjectID: project.ID,
			Task:    "do something",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", token)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusCreated, rec.Code)

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent)
		assert.Equal(t, "sub-agent", resp.Agent.Slug)
		assert.Equal(t, callingAgent.ID, resp.Agent.CreatedBy)
		// Verify CreatorName is the calling agent's name
		require.NotNil(t, resp.Agent.AppliedConfig)
		assert.Equal(t, callingAgent.Name, resp.Agent.AppliedConfig.CreatorName)
	})

	t.Run("Agent with project:agent:create scope rejected for different project", func(t *testing.T) {
		// Create another project
		otherProject := &store.Project{
			ID:   "project-other",
			Name: "Other Project",
			Slug: "other-project",
		}
		require.NoError(t, s.CreateProject(ctx, otherProject))

		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
			ScopeAgentCreate,
		}, nil)
		require.NoError(t, err)

		body, _ := json.Marshal(CreateAgentRequest{
			Name:    "Cross Project Agent",
			ProjectID: otherProject.ID,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", token)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("Agent without project:agent:create scope is rejected", func(t *testing.T) {
		// Token with only status update scope (no create scope)
		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
		}, nil)
		require.NoError(t, err)

		body, _ := json.Marshal(CreateAgentRequest{
			Name:    "Unauthorized Sub",
			ProjectID: project.ID,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", token)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestAgentLifecycle_WithScope(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:   "project-lc",
		Name: "Lifecycle Project",
		Slug: "lifecycle-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create the calling agent
	callingAgent := &store.Agent{
		ID:      "agent-lc-caller",
		Slug:    "agent-lc-caller",
		Name:    "Lifecycle Caller",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, callingAgent))

	// Create a target agent in the same project
	targetAgent := &store.Agent{
		ID:      "agent-lc-target",
		Slug:    "agent-lc-target",
		Name:    "Lifecycle Target",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, targetAgent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	t.Run("Agent with project:agent:lifecycle scope can perform lifecycle actions in same project", func(t *testing.T) {
		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
			ScopeAgentLifecycle,
		}, nil)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+targetAgent.ID+"/stop", nil)
		req.Header.Set("X-Scion-Agent-Token", token)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		// May return 200 or 500 (no dispatcher), but not 403 - the auth check passes
		assert.NotEqual(t, http.StatusForbidden, rec.Code)
	})

	t.Run("Agent with project:agent:lifecycle scope rejected for cross-project lifecycle", func(t *testing.T) {
		// Create another project and agent
		otherProject := &store.Project{
			ID:   "project-lc-other",
			Name: "Other LC Project",
			Slug: "other-lc-project",
		}
		require.NoError(t, s.CreateProject(ctx, otherProject))

		otherAgent := &store.Agent{
			ID:      "agent-lc-other",
			Slug:    "agent-lc-other",
			Name:    "Other LC Agent",
			ProjectID: otherProject.ID,
			Phase:   string(state.PhaseRunning),
		}
		require.NoError(t, s.CreateAgent(ctx, otherAgent))

		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
			ScopeAgentLifecycle,
		}, nil)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+otherAgent.ID+"/stop", nil)
		req.Header.Set("X-Scion-Agent-Token", token)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("Agent without lifecycle scope cannot perform lifecycle actions", func(t *testing.T) {
		// Token with only status update scope (existing behavior)
		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
		}, nil)
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+targetAgent.ID+"/stop", nil)
		req.Header.Set("X-Scion-Agent-Token", token)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestAgentGetAgent_ProjectIsolation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create two projects
	project1 := &store.Project{
		ID:   "project-get1",
		Name: "Get Project 1",
		Slug: "get-project-1",
	}
	require.NoError(t, s.CreateProject(ctx, project1))

	project2 := &store.Project{
		ID:   "project-get2",
		Name: "Get Project 2",
		Slug: "get-project-2",
	}
	require.NoError(t, s.CreateProject(ctx, project2))

	// Create agents in each project
	agent1 := &store.Agent{
		ID:      "agent-get-caller",
		Slug:    "agent-get-caller",
		Name:    "Get Caller",
		ProjectID: project1.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))

	agent2SameProject := &store.Agent{
		ID:      "agent-get-same",
		Slug:    "agent-get-same",
		Name:    "Same Project Agent",
		ProjectID: project1.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2SameProject))

	agentOtherProject := &store.Agent{
		ID:      "agent-get-other",
		Slug:    "agent-get-other",
		Name:    "Other Project Agent",
		ProjectID: project2.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agentOtherProject))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	token, err := tokenSvc.GenerateAgentToken(agent1.ID, project1.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

	t.Run("Agent can GET details of agents in same project", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent2SameProject.ID, nil)
		req.Header.Set("X-Scion-Agent-Token", token)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("Agent cannot GET details of agents in different project", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentOtherProject.ID, nil)
		req.Header.Set("X-Scion-Agent-Token", token)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("Agent cannot access workspace operations", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent2SameProject.ID+"/workspace", nil)
		req.Header.Set("X-Scion-Agent-Token", token)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestDeleteProjectAgent_BrokerOffline(t *testing.T) {
	srv, s := testServer(t)

	project, _, agent := setupOfflineBrokerAgent(t, s, "gdel")

	rec := doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/projects/%s/agents/%s", project.ID, agent.ID), nil)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// Verify agent was NOT deleted
	ctx := context.Background()
	_, err := s.GetAgent(ctx, agent.ID)
	assert.NoError(t, err, "agent should still exist when broker is offline")
}

// createAgentDispatcher is a mock dispatcher for createAgent handler tests.
// It allows controlling the status that DispatchAgentCreate reports back.
type createAgentDispatcher struct {
	createPhase   string // status to set on agent during DispatchAgentCreate
	createRuntime string
	createStatus  string
	envReqs       *RemoteEnvRequirementsResponse
	deleteCalled  bool
	deleteErr     error
	execOutput    string
	execExitCode  int
}

func (d *createAgentDispatcher) DispatchAgentCreate(_ context.Context, agent *store.Agent) error {
	if d.createPhase != "" {
		agent.Phase = d.createPhase
	}
	if d.createRuntime != "" {
		agent.Runtime = d.createRuntime
	}
	if d.createStatus != "" {
		agent.ContainerStatus = d.createStatus
	}
	return nil
}
func (d *createAgentDispatcher) DispatchAgentProvision(_ context.Context, agent *store.Agent) error {
	agent.Phase = string(state.PhaseCreated)
	return nil
}
func (d *createAgentDispatcher) DispatchAgentStart(_ context.Context, _ *store.Agent, _ string) error {
	return nil
}
func (d *createAgentDispatcher) DispatchAgentStop(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *createAgentDispatcher) DispatchAgentRestart(_ context.Context, _ *store.Agent) error {
	return nil
}
func (d *createAgentDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, _, _, _ bool, _ time.Time) error {
	d.deleteCalled = true
	return d.deleteErr
}
func (d *createAgentDispatcher) DispatchAgentMessage(_ context.Context, _ *store.Agent, _ string, _ bool, _ *messages.StructuredMessage) error {
	return nil
}
func (d *createAgentDispatcher) DispatchCheckAgentPrompt(_ context.Context, _ *store.Agent) (bool, error) {
	return false, nil
}
func (d *createAgentDispatcher) DispatchAgentCreateWithGather(_ context.Context, agent *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	if err := d.DispatchAgentCreate(context.Background(), agent); err != nil {
		return nil, err
	}
	return d.envReqs, nil
}

// failingCreateDispatcher is a mock dispatcher whose DispatchAgentCreateWithGather
// always returns an error, simulating a broker-side failure (e.g. auth resolution error).
// It tracks whether DispatchAgentDelete is called so tests can verify cleanup behaviour.
type failingCreateDispatcher struct {
	createAgentDispatcher
	createErr         error
	deleteCalledFiles bool
	deleteBranch      bool
}

func (d *failingCreateDispatcher) DispatchAgentCreateWithGather(_ context.Context, _ *store.Agent) (*RemoteEnvRequirementsResponse, error) {
	return nil, d.createErr
}
func (d *failingCreateDispatcher) DispatchAgentDelete(_ context.Context, _ *store.Agent, deleteFiles, removeBranch, _ bool, _ time.Time) error {
	d.deleteCalled = true
	d.deleteCalledFiles = deleteFiles
	d.deleteBranch = removeBranch
	return nil
}
func (d *createAgentDispatcher) DispatchAgentLogs(_ context.Context, _ *store.Agent, _ int) (string, error) {
	return "", nil
}
func (d *createAgentDispatcher) DispatchAgentExec(_ context.Context, _ *store.Agent, _ []string, _ int) (string, int, error) {
	return d.execOutput, d.execExitCode, nil
}
func (d *createAgentDispatcher) DispatchFinalizeEnv(_ context.Context, _ *store.Agent, _ map[string]string) error {
	return nil
}

// setupCreateAgentServer creates a test server with a dispatcher and a project+broker ready for agent creation.
func setupCreateAgentServer(t *testing.T, disp AgentDispatcher) (*Server, store.Store, *store.Project) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-create",
		Name: "Create Test Project",
		Slug: "create-test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-create",
		Name:   "Create Test Broker",
		Slug:   "create-test-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	provider := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	srv.SetDispatcher(disp)
	return srv, s, project
}

func TestCreateAgent_BrokerStatusPreserved(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create an agent with a task — should dispatch and preserve broker-reported "running" status
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "status-test",
		ProjectID: project.ID,
		Task:    "do something",
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	// The response should reflect the broker-reported status, not hardcoded "provisioning"
	assert.Equal(t, string(state.PhaseRunning), resp.Agent.Phase,
		"agent status should reflect broker response, not hardcoded provisioning")

	// Verify persisted status in store
	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), persisted.Phase,
		"persisted agent status should match broker response")
}

func TestCreateAgent_FallbackToProvisioningWhenNoBrokerStatus(t *testing.T) {
	// Dispatcher that doesn't set a status (leaves it as "pending")
	disp := &createAgentDispatcher{createPhase: ""}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "fallback-test",
		ProjectID: project.ID,
		Task:    "do something",
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	// When broker doesn't report a status, should fall back to "provisioning"
	assert.Equal(t, string(state.PhaseProvisioning), resp.Agent.Phase,
		"agent status should fall back to provisioning when broker doesn't report status")
}

type conflictOnceStore struct {
	store.Store
	failNextUpdate bool
}

func (s *conflictOnceStore) UpdateAgent(ctx context.Context, agent *store.Agent) error {
	if s.failNextUpdate {
		s.failNextUpdate = false
		return store.ErrVersionConflict
	}
	return s.Store.UpdateAgent(ctx, agent)
}

func TestCreateAgent_RetriesVersionConflictAfterDispatch(t *testing.T) {
	disp := &createAgentDispatcher{
		createPhase:   string(state.PhaseRunning),
		createRuntime: "kubernetes",
		createStatus:  "Running",
	}
	srv, baseStore, project := setupCreateAgentServer(t, disp)
	wrapped := &conflictOnceStore{Store: baseStore, failNextUpdate: true}
	srv.store = wrapped
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "version-conflict-agent",
		ProjectID: project.ID,
		Task:    "do something",
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.Empty(t, resp.Warnings)

	persisted, err := wrapped.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.False(t, wrapped.failNextUpdate, "test should exercise the conflict retry path")
	assert.Equal(t, "kubernetes", persisted.Runtime)
	assert.Equal(t, "Running", persisted.ContainerStatus)
	assert.Equal(t, string(state.PhaseRunning), persisted.Phase)
}

func TestCreateAgent_EnvGatherRetriesVersionConflict(t *testing.T) {
	disp := &createAgentDispatcher{
		envReqs: &RemoteEnvRequirementsResponse{
			Required: []string{"API_TOKEN"},
			Needs:    []string{"API_TOKEN"},
		},
	}
	srv, baseStore, project := setupCreateAgentServer(t, disp)
	wrapped := &conflictOnceStore{Store: baseStore, failNextUpdate: true}
	srv.store = wrapped
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "env-gather-conflict-agent",
		ProjectID:   project.ID,
		Task:      "do something",
		GatherEnv: true,
	})

	require.Equal(t, http.StatusAccepted, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.NotNil(t, resp.EnvGather)

	persisted, err := wrapped.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseProvisioning), persisted.Phase)
	assert.False(t, wrapped.failNextUpdate, "test should exercise the conflict retry path")
}

func TestCreateAgent_ProvisionOnlyRetriesVersionConflict(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, baseStore, project := setupCreateAgentServer(t, disp)
	wrapped := &conflictOnceStore{Store: baseStore, failNextUpdate: true}
	srv.store = wrapped

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "provision-only-conflict-agent",
		ProjectID:       project.ID,
		Task:          "some task",
		ProvisionOnly: true,
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.Empty(t, resp.Warnings)
	assert.False(t, wrapped.failNextUpdate, "test should exercise the conflict retry path")
}

func TestCreateAgent_StartsWithoutTask(t *testing.T) {
	// When ProvisionOnly is false (scion start), the agent should be started
	// even if no task is provided — the template may have a built-in prompt.
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "no-task-agent",
		ProjectID: project.ID,
		// No Task, no Attach — should still start (not provision-only)
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	// Should be running, not "created" (which would mean provision-only was used)
	assert.Equal(t, string(state.PhaseRunning), resp.Agent.Phase,
		"agent should be started (running) even without a task when ProvisionOnly is false")
}

func TestCreateAgent_ProvisionOnlyStaysCreated(t *testing.T) {
	// When ProvisionOnly is true (scion create), the agent should not start.
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "provision-only-agent",
		ProjectID:       project.ID,
		Task:          "some task",
		ProvisionOnly: true,
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	assert.Equal(t, string(state.PhaseCreated), resp.Agent.Phase,
		"agent should stay in created status when ProvisionOnly is true")
}

func TestCreateAgent_RestartFromProvisioningStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent stuck in "provisioning" status (simulating Bug 1)
	stuckAgent := &store.Agent{
		ID:              "agent-stuck-prov",
		Slug:            "stuck-agent",
		Name:            "stuck-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseProvisioning),
	}
	require.NoError(t, s.CreateAgent(ctx, stuckAgent))

	// Try to start the same agent name — should succeed by re-starting, not 409
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "stuck-agent",
		ProjectID: project.ID,
		Task:    "retry task",
	})

	assert.Equal(t, http.StatusOK, rec.Code,
		"re-starting an agent stuck in provisioning should succeed (200), not conflict (409)")

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.Equal(t, string(state.PhaseRunning), resp.Agent.Phase)
}

func TestCreateAgent_RestartFromPendingStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent in "pending" status
	pendingAgent := &store.Agent{
		ID:              "agent-pending",
		Slug:            "pending-agent",
		Name:            "pending-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, pendingAgent))

	// Try to start the same agent name — should succeed
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "pending-agent",
		ProjectID: project.ID,
		Task:    "retry task",
	})

	assert.Equal(t, http.StatusOK, rec.Code,
		"re-starting an agent in pending status should succeed")
}

func TestCreateAgent_RecreateFromRunningStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent in "running" status (stale — container may have died)
	runningAgent := &store.Agent{
		ID:              "agent-running-stale",
		Slug:            "running-agent",
		Name:            "running-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, runningAgent))

	// Start with the same name — should delete old agent and create new one
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "running-agent",
		ProjectID: project.ID,
		Task:    "new task",
	})

	require.Equal(t, http.StatusCreated, rec.Code,
		"re-creating agent from running status should succeed with 201")

	// Old agent should be deleted
	_, err := s.GetAgent(ctx, "agent-running-stale")
	assert.ErrorIs(t, err, store.ErrNotFound, "old agent should be deleted")

	// Dispatcher should have been asked to delete
	assert.True(t, disp.deleteCalled, "dispatcher should have been asked to delete old agent")

	// New agent should exist
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.NotEqual(t, "agent-running-stale", resp.Agent.ID, "new agent should have a different ID")
	assert.Equal(t, string(state.PhaseRunning), resp.Agent.Phase)
}

func TestCreateAgent_RecreateFromErrorStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent in "error" status
	errorAgent := &store.Agent{
		ID:              "agent-errored",
		Slug:            "error-agent",
		Name:            "error-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseError),
	}
	require.NoError(t, s.CreateAgent(ctx, errorAgent))

	// Start with the same name — should delete and recreate
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "error-agent",
		ProjectID: project.ID,
		Task:    "retry after error",
	})

	require.Equal(t, http.StatusCreated, rec.Code,
		"re-creating agent from error status should succeed with 201")

	// Old agent should be deleted
	_, err := s.GetAgent(ctx, "agent-errored")
	assert.ErrorIs(t, err, store.ErrNotFound, "old errored agent should be deleted")
}

func TestCreateAgent_RecreateFromStoppedStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent in "stopped" status
	stoppedAgent := &store.Agent{
		ID:              "agent-stopped",
		Slug:            "stopped-agent",
		Name:            "stopped-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, stoppedAgent))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "stopped-agent",
		ProjectID: project.ID,
		Task:    "restart after stop",
	})

	require.Equal(t, http.StatusCreated, rec.Code,
		"re-creating agent from stopped status should succeed with 201")

	_, err := s.GetAgent(ctx, "agent-stopped")
	assert.ErrorIs(t, err, store.ErrNotFound, "old stopped agent should be deleted")
}

// TestAgentCreate_LocalTemplateWithLocalBroker tests that agent creation succeeds
// when a template is not found on the Hub but the target broker has local filesystem
// access (LocalPath is set), allowing the template to be resolved locally by the broker.
func TestAgentCreate_LocalTemplateWithLocalBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:     "broker_local_tpl",
		Slug:   "local-tpl-broker",
		Name:   "Local Template Broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Create a project with default runtime broker
	project := &store.Project{
		ID:                     "project_local_tpl",
		Slug:                   "local-tpl-project",
		Name:                   "Local Template Project",
		GitRemote:              "github.com/test/local-tpl",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Register the broker as a provider WITH a local path
	provider := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  "/home/user/project/.scion",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	// Create agent with a template name that does NOT exist on the Hub.
	// Because the broker has a LocalPath, this should succeed.
	body := map[string]interface{}{
		"name":     "Local Template Agent",
		"projectId":  project.ID,
		"template": "my-local-template",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	assert.Equal(t, http.StatusCreated, rec.Code, "expected 201 when broker has local access, got %d: %s", rec.Code, rec.Body.String())

	var resp CreateAgentResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotNil(t, resp.Agent)
	assert.Equal(t, "local-template-agent", resp.Agent.Slug)
	assert.Equal(t, "my-local-template", resp.Agent.Template)
	// The harness config should be empty when resolvedTemplate is nil (broker resolves via DefaultHarnessConfig)
	require.NotNil(t, resp.Agent.AppliedConfig)
	assert.Empty(t, resp.Agent.AppliedConfig.HarnessConfig)
	// TemplateID and TemplateHash should be empty since template was not resolved on Hub
	assert.Empty(t, resp.Agent.AppliedConfig.TemplateID)
	assert.Empty(t, resp.Agent.AppliedConfig.TemplateHash)
}

// TestAgentCreate_LocalTemplateWithRemoteBroker tests that agent creation returns
// NotFound when a template is not on the Hub and the broker does NOT have local access.
func TestAgentCreate_LocalTemplateWithRemoteBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a runtime broker
	broker := &store.RuntimeBroker{
		ID:     "broker_remote_tpl",
		Slug:   "remote-tpl-broker",
		Name:   "Remote Template Broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Create a project
	project := &store.Project{
		ID:                     "project_remote_tpl",
		Slug:                   "remote-tpl-project",
		Name:                   "Remote Template Project",
		GitRemote:              "github.com/test/remote-tpl",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Register the broker as a provider WITHOUT a local path
	provider := &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
		// Note: LocalPath is NOT set — broker has no local access
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	// Create agent with a template name that does NOT exist on the Hub.
	// Without local access, this should fail with NotFound.
	body := map[string]interface{}{
		"name":     "Remote Template Agent",
		"projectId":  project.ID,
		"template": "nonexistent-template",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	assert.Equal(t, http.StatusNotFound, rec.Code, "expected 404 when template not on Hub and broker has no local access")
}

// TestAgentCreate_LocalTemplateNoBroker tests that agent creation fails when a
// template is not on the Hub and there is no runtime broker assigned. The error
// occurs because no broker is available (before template resolution is reached).
func TestAgentCreate_LocalTemplateNoBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project WITHOUT a default runtime broker
	project := &store.Project{
		ID:        "project_no_broker_tpl",
		Slug:      "no-broker-tpl-project",
		Name:      "No Broker Template Project",
		GitRemote: "github.com/test/no-broker-tpl",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))

	// Create agent with a template name that does NOT exist on the Hub.
	// Without any broker, this should fail (422 validation error for missing broker).
	body := map[string]interface{}{
		"name":     "No Broker Agent",
		"projectId":  project.ID,
		"template": "nonexistent-template",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	// Expect a client error — the broker resolution fails before template resolution
	assert.True(t, rec.Code >= 400 && rec.Code < 500, "expected client error when no broker assigned, got %d", rec.Code)
}

func TestCreateAgent_CreatorName_UserEmail(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Use dev auth token (which creates a DevUser with email "dev@localhost")
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "user-created-agent",
		ProjectID: project.ID,
		Task:    "do something",
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	// Verify the agent's AppliedConfig.CreatorName is the user's email
	require.NotNil(t, resp.Agent.AppliedConfig)
	assert.Equal(t, "dev@localhost", resp.Agent.AppliedConfig.CreatorName,
		"CreatorName should be the user's email when a user creates an agent")

	// Also verify it's persisted in the store
	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig)
	assert.Equal(t, "dev@localhost", persisted.AppliedConfig.CreatorName)
}

func TestListAgents_ServerTimeIncluded(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project and agent
	project := &store.Project{
		ID:   "project-servertime",
		Name: "ServerTime Project",
		Slug: "servertime-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:      "agent-servertime",
		Slug:    "agent-servertime-slug",
		Name:    "ServerTime Agent",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	before := time.Now().UTC()

	// List agents
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents?projectId="+project.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	after := time.Now().UTC()

	var resp ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// ServerTime should be non-zero and between before/after
	assert.False(t, resp.ServerTime.IsZero(), "ServerTime should be non-zero")
	assert.True(t, !resp.ServerTime.Before(before.Add(-time.Second)),
		"ServerTime %v should not be before request start %v", resp.ServerTime, before)
	assert.True(t, !resp.ServerTime.After(after.Add(time.Second)),
		"ServerTime %v should not be after request end %v", resp.ServerTime, after)
}

func TestListProjectAgents_ServerTimeIncluded(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a project
	project := &store.Project{
		ID:   "project-servertime-g",
		Name: "ServerTime Project G",
		Slug: "servertime-project-g",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	before := time.Now().UTC()

	// List project agents
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/agents", project.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	after := time.Now().UTC()

	var resp ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.False(t, resp.ServerTime.IsZero(), "ServerTime should be non-zero in project-scoped list")
	assert.True(t, !resp.ServerTime.Before(before.Add(-time.Second)),
		"ServerTime should not be before request start")
	assert.True(t, !resp.ServerTime.After(after.Add(time.Second)),
		"ServerTime should not be after request end")
}

// TestCreateProjectAgent_BrokerStatusPreserved tests that the project-scoped agent creation
// endpoint (/api/v1/projects/{projectId}/agents) preserves the status set by the broker's
// response rather than unconditionally overwriting it with "provisioning".
func TestCreateProjectAgent_BrokerStatusPreserved(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create agent via the project-scoped endpoint (this is the path the CLI uses)
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID),
		CreateAgentRequest{
			Name: "project-status-test",
			Task: "do something",
		})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	// The response should reflect the broker-reported status, not hardcoded "provisioning"
	assert.Equal(t, string(state.PhaseRunning), resp.Agent.Phase,
		"project-scoped agent status should reflect broker response, not hardcoded provisioning")

	// Verify persisted status in store
	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), persisted.Phase,
		"persisted agent status should match broker response")
}

// TestCreateProjectAgent_FallbackToProvisioningWhenNoBrokerStatus tests that the project-scoped
// endpoint falls back to "provisioning" when the broker doesn't report a status.
func TestCreateProjectAgent_FallbackToProvisioningWhenNoBrokerStatus(t *testing.T) {
	// Dispatcher that doesn't set a status (leaves it as "pending")
	disp := &createAgentDispatcher{createPhase: ""}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID),
		CreateAgentRequest{
			Name: "project-fallback-test",
			Task: "do something",
		})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	// When broker doesn't report a status, should fall back to "provisioning"
	assert.Equal(t, string(state.PhaseProvisioning), resp.Agent.Phase,
		"agent status should fall back to provisioning when broker doesn't report status")
}

func TestCreateAgent_GitAnchoredProjectPopulatesGitClone(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create a project with GitRemote and labels
	gitProject := &store.Project{
		ID:        "project-git",
		Name:      "Git Project",
		Slug:      "git-project",
		GitRemote: "github.com/example/myrepo",
		Labels: map[string]string{
			"scion.dev/clone-url":      "https://github.com/example/myrepo.git",
			"scion.dev/default-branch": "develop",
		},
		DefaultRuntimeBrokerID: "broker-create",
	}
	require.NoError(t, s.CreateProject(ctx, gitProject))

	// Add project provider
	provider := &store.ProjectProvider{
		ProjectID:    gitProject.ID,
		BrokerID:   "broker-create",
		BrokerName: "Create Test Broker",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "git-agent",
		ProjectID: gitProject.ID,
		Task:    "implement feature",
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	// Verify the agent was created — check that AppliedConfig.GitClone was populated
	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig, "AppliedConfig should be set")
	require.NotNil(t, persisted.AppliedConfig.GitClone, "GitClone should be populated for git-anchored project")
	assert.Equal(t, "https://github.com/example/myrepo.git", persisted.AppliedConfig.GitClone.URL)
	assert.Equal(t, "develop", persisted.AppliedConfig.GitClone.Branch)
	assert.Equal(t, 1, persisted.AppliedConfig.GitClone.Depth)
}

func TestCreateAgent_NonGitProjectNoGitClone(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "non-git-agent",
		ProjectID: project.ID,
		Task:    "do something",
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig, "AppliedConfig should be set")
	assert.Nil(t, persisted.AppliedConfig.GitClone,
		"GitClone should be nil for non-git-anchored project")
}

func TestCreateProjectAgent_GitAnchoredProjectPopulatesGitClone(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create a project with GitRemote and labels
	gitProject := &store.Project{
		ID:        "project-git-scoped",
		Name:      "Git Project Scoped",
		Slug:      "git-project-scoped",
		GitRemote: "github.com/example/myrepo",
		Labels: map[string]string{
			"scion.dev/clone-url":      "https://github.com/example/myrepo.git",
			"scion.dev/default-branch": "develop",
		},
		DefaultRuntimeBrokerID: "broker-create",
	}
	require.NoError(t, s.CreateProject(ctx, gitProject))

	// Add project provider
	provider := &store.ProjectProvider{
		ProjectID:    gitProject.ID,
		BrokerID:   "broker-create",
		BrokerName: "Create Test Broker",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", gitProject.ID),
		CreateAgentRequest{
			Name: "git-agent-scoped",
			Task: "implement feature",
		})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	// Verify the agent was created — check that AppliedConfig.GitClone was populated
	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig, "AppliedConfig should be set")
	require.NotNil(t, persisted.AppliedConfig.GitClone, "GitClone should be populated for git-anchored project")
	assert.Equal(t, "https://github.com/example/myrepo.git", persisted.AppliedConfig.GitClone.URL)
	assert.Equal(t, "develop", persisted.AppliedConfig.GitClone.Branch)
	assert.Equal(t, 1, persisted.AppliedConfig.GitClone.Depth)
}

func TestCreateProjectAgent_NonGitProjectNoGitClone(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID),
		CreateAgentRequest{
			Name: "non-git-agent-scoped",
			Task: "do something",
		})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig, "AppliedConfig should be set")
	assert.Nil(t, persisted.AppliedConfig.GitClone,
		"GitClone should be nil for non-git-anchored project")
}

func TestCreateAgent_GitProjectCloneURLFallback(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create a project with GitRemote but WITHOUT the scion.dev/clone-url label.
	// The URL should be constructed from gitRemote as "https://<gitRemote>.git".
	gitProject := &store.Project{
		ID:        "project-git-fallback-url",
		Name:      "Git Project Fallback URL",
		Slug:      "git-project-fallback-url",
		GitRemote: "github.com/example/fallback-repo",
		Labels: map[string]string{
			"scion.dev/default-branch": "develop",
		},
		DefaultRuntimeBrokerID: "broker-create",
	}
	require.NoError(t, s.CreateProject(ctx, gitProject))

	provider := &store.ProjectProvider{
		ProjectID:    gitProject.ID,
		BrokerID:   "broker-create",
		BrokerName: "Create Test Broker",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "fallback-url-agent",
		ProjectID: gitProject.ID,
		Task:    "test fallback",
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig, "AppliedConfig should be set")
	require.NotNil(t, persisted.AppliedConfig.GitClone, "GitClone should be populated")

	// clone-url label is missing, so URL should be constructed from GitRemote
	assert.Equal(t, "https://github.com/example/fallback-repo.git", persisted.AppliedConfig.GitClone.URL,
		"clone URL should be constructed from gitRemote when scion.dev/clone-url label is absent")
	assert.Equal(t, "develop", persisted.AppliedConfig.GitClone.Branch)
	assert.Equal(t, 1, persisted.AppliedConfig.GitClone.Depth)
}

func TestCreateAgent_GitProjectSchemelessCloneURL(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create a project where clone-url label is set but missing https:// scheme
	// (as can happen when the web UI stores raw user input).
	gitProject := &store.Project{
		ID:        "project-git-schemeless",
		Name:      "Git Project Schemeless",
		Slug:      "git-project-schemeless",
		GitRemote: "github.com/example/schemeless-repo",
		Labels: map[string]string{
			"scion.dev/clone-url":      "github.com/example/schemeless-repo",
			"scion.dev/default-branch": "main",
		},
		DefaultRuntimeBrokerID: "broker-create",
	}
	require.NoError(t, s.CreateProject(ctx, gitProject))

	provider := &store.ProjectProvider{
		ProjectID:    gitProject.ID,
		BrokerID:   "broker-create",
		BrokerName: "Create Test Broker",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "schemeless-url-agent",
		ProjectID: gitProject.ID,
		Task:    "test schemeless",
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig, "AppliedConfig should be set")
	require.NotNil(t, persisted.AppliedConfig.GitClone, "GitClone should be populated")

	assert.Equal(t, "https://github.com/example/schemeless-repo.git", persisted.AppliedConfig.GitClone.URL,
		"schemeless clone-url label should be normalized to https:// with .git suffix")
	assert.Equal(t, "main", persisted.AppliedConfig.GitClone.Branch)
	assert.Equal(t, 1, persisted.AppliedConfig.GitClone.Depth)
}

func TestCreateAgent_GitProjectDefaultBranchFallback(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create a project with GitRemote and clone-url label but WITHOUT default-branch.
	// The branch should default to "main".
	gitProject := &store.Project{
		ID:        "project-git-fallback-branch",
		Name:      "Git Project Fallback Branch",
		Slug:      "git-project-fallback-branch",
		GitRemote: "github.com/example/branch-repo",
		Labels: map[string]string{
			"scion.dev/clone-url": "https://github.com/example/branch-repo.git",
		},
		DefaultRuntimeBrokerID: "broker-create",
	}
	require.NoError(t, s.CreateProject(ctx, gitProject))

	provider := &store.ProjectProvider{
		ProjectID:    gitProject.ID,
		BrokerID:   "broker-create",
		BrokerName: "Create Test Broker",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddProjectProvider(ctx, provider))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "fallback-branch-agent",
		ProjectID: gitProject.ID,
		Task:    "test branch fallback",
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig, "AppliedConfig should be set")
	require.NotNil(t, persisted.AppliedConfig.GitClone, "GitClone should be populated")

	assert.Equal(t, "https://github.com/example/branch-repo.git", persisted.AppliedConfig.GitClone.URL)
	// default-branch label is missing, so branch should default to "main"
	assert.Equal(t, "main", persisted.AppliedConfig.GitClone.Branch,
		"branch should default to 'main' when scion.dev/default-branch label is absent")
	assert.Equal(t, 1, persisted.AppliedConfig.GitClone.Depth)
}

func TestCreateAgent_ProfileStoredInAppliedConfig(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "profiled-agent",
		ProjectID: project.ID,
		Profile: "custom-profile",
		Task:    "do something",
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.NotNil(t, resp.Agent.AppliedConfig)
	assert.Equal(t, "custom-profile", resp.Agent.AppliedConfig.Profile,
		"Profile should be stored in AppliedConfig")

	// Verify it's persisted in the store
	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig)
	assert.Equal(t, "custom-profile", persisted.AppliedConfig.Profile)
}

func TestCreateAgent_ProfileStoredWithConfigOverride(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "profiled-agent-with-config",
		ProjectID: project.ID,
		Profile: "other-profile",
		Task:    "do something",
		Config:  &api.ScionConfig{Image: "custom-image:latest"},
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.NotNil(t, resp.Agent.AppliedConfig)
	assert.Equal(t, "other-profile", resp.Agent.AppliedConfig.Profile,
		"Profile should be stored even when config override is present")
	assert.Equal(t, "custom-image:latest", resp.Agent.AppliedConfig.Image)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig)
	assert.Equal(t, "other-profile", persisted.AppliedConfig.Profile)
}

func TestCreateAgent_ScionConfigInlineConfigPreserved(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create an agent with a full ScionConfig including fields beyond the old AgentConfigOverride
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "inline-config-agent",
		ProjectID: project.ID,
		Task:    "review code",
		Config: &api.ScionConfig{
			Image:            "custom:latest",
			Model:            "claude-opus-4-6",
			Env:              map[string]string{"FOO": "bar"},
			HarnessConfig:    "claude-default",
			AuthSelectedType: "api-key",
			SystemPrompt:     "You are a code reviewer.",
			MaxTurns:         50,
		},
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.NotNil(t, resp.Agent.AppliedConfig)

	// Verify extracted fields
	assert.Equal(t, "custom:latest", resp.Agent.AppliedConfig.Image)
	assert.Equal(t, "claude-opus-4-6", resp.Agent.AppliedConfig.Model)
	assert.Equal(t, map[string]string{"FOO": "bar"}, resp.Agent.AppliedConfig.Env)
	assert.Equal(t, "claude-default", resp.Agent.AppliedConfig.HarnessConfig)
	assert.Equal(t, "api-key", resp.Agent.AppliedConfig.HarnessAuth)
	assert.Equal(t, "review code", resp.Agent.AppliedConfig.Task)

	// Verify the full inline config is preserved
	require.NotNil(t, resp.Agent.AppliedConfig.InlineConfig, "InlineConfig should be preserved")
	assert.Equal(t, "You are a code reviewer.", resp.Agent.AppliedConfig.InlineConfig.SystemPrompt)
	assert.Equal(t, 50, resp.Agent.AppliedConfig.InlineConfig.MaxTurns)
	assert.Equal(t, "claude-opus-4-6", resp.Agent.AppliedConfig.InlineConfig.Model)

	// Verify persisted in store
	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig)
	require.NotNil(t, persisted.AppliedConfig.InlineConfig)
	assert.Equal(t, "You are a code reviewer.", persisted.AppliedConfig.InlineConfig.SystemPrompt)
}

func TestCreateAgent_ScionConfigTaskFieldMerge(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	// When both req.Task and Config.Task are set, req.Task takes precedence
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "task-merge-agent",
		ProjectID: project.ID,
		Task:    "request-level task",
		Config: &api.ScionConfig{
			Task: "config-level task",
		},
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.NotNil(t, resp.Agent.AppliedConfig)
	assert.Equal(t, "request-level task", resp.Agent.AppliedConfig.Task,
		"Request-level task should take precedence over config-level task")
}

func TestCreateAgent_ScionConfigTaskFromConfigOnly(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	// When only Config.Task is set (no req.Task), it should be used
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "task-config-only-agent",
		ProjectID: project.ID,
		Config: &api.ScionConfig{
			Task: "config-only task",
		},
	})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.NotNil(t, resp.Agent.AppliedConfig)
	assert.Equal(t, "config-only task", resp.Agent.AppliedConfig.Task,
		"Config-level task should be used when no request-level task is set")
}

// TestListAgents_HarnessConfigEnriched verifies that the harness type from
// AppliedConfig.HarnessConfig is surfaced as a top-level harnessConfig field in
// list responses so that clients can display it without parsing appliedConfig.
func TestListAgents_HarnessConfigEnriched(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-harness-enrich",
		Name: "Harness Enrichment Project",
		Slug: "harness-enrich-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:      "agent-harness-enrich",
		Slug:    "agent-harness-enrich",
		Name:    "Harness Agent",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "gemini",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// List via global endpoint
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents?projectId="+project.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Agents, 1)
	assert.Equal(t, "gemini", resp.Agents[0].HarnessConfig,
		"harnessConfig should be enriched from appliedConfig.harness")

	// Also verify the raw JSON has harnessConfig at the top level
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	var agents []map[string]interface{}
	require.NoError(t, json.Unmarshal(raw["agents"], &agents))
	require.Len(t, agents, 1)
	assert.Equal(t, "gemini", agents[0]["harnessConfig"],
		"JSON response should include harnessConfig at top level")

	// List via project-scoped endpoint
	rec2 := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/agents", project.ID), nil)
	require.Equal(t, http.StatusOK, rec2.Code)

	var resp2 ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	require.Len(t, resp2.Agents, 1)
	assert.Equal(t, "gemini", resp2.Agents[0].HarnessConfig,
		"project-scoped harnessConfig should also be enriched")
}

// TestGetAgent_HarnessConfigEnriched verifies that a single agent GET also
// includes the enriched harnessConfig field.
func TestGetAgent_HarnessConfigEnriched(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-harness-get",
		Name: "Harness Get Project",
		Slug: "harness-get-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:      "agent-harness-get",
		Slug:    "agent-harness-get",
		Name:    "Harness Get Agent",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var got store.Agent
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "claude", got.HarnessConfig,
		"single agent GET should include enriched harnessConfig")
}

// TestCreateAgent_HarnessFromRequestField verifies that the explicit Harness
// field in CreateAgentRequest is used as a fallback when the template doesn't
// resolve a harness (e.g., during sync when the template is local-only).
func TestCreateAgent_HarnessFromRequestField(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create agent with no template but explicit harness (sync scenario)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "sync-agent",
		ProjectID:       project.ID,
		HarnessConfig: "gemini",
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Verify harness is stored in AppliedConfig
	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "gemini", agent.AppliedConfig.HarnessConfig,
		"AppliedConfig.HarnessConfig should be set from request HarnessConfig field")

	// Verify enrichment works for list
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/agents?projectId="+project.ID, nil)
	require.Equal(t, http.StatusOK, rec2.Code)

	var listResp ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &listResp))

	found := false
	for _, a := range listResp.Agents {
		if a.Name == "sync-agent" {
			assert.Equal(t, "gemini", a.HarnessConfig,
				"enriched HarnessConfig should show gemini for synced agent")
			found = true
		}
	}
	assert.True(t, found, "sync-agent should be in the list")
}

// TestGetAgent_ProfileInResponse verifies that profile is returned in the
// single-agent GET response via appliedConfig.
func TestGetAgent_ProfileInResponse(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create agent with explicit profile
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "profile-get-agent",
		ProjectID: project.ID,
		Profile: "docker-dev",
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)

	// Verify single-agent GET returns profile in appliedConfig
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec2.Code)

	var got map[string]interface{}
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &got))
	ac, ok := got["appliedConfig"].(map[string]interface{})
	require.True(t, ok, "response should include appliedConfig")
	assert.Equal(t, "docker-dev", ac["profile"],
		"GET agent response appliedConfig should contain profile")
}

// TestHeartbeat_BackfillsProfile verifies that the heartbeat handler
// backfills the profile in AppliedConfig when the agent record is missing it.
func TestHeartbeat_BackfillsProfile(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-profile-hb",
		Name: "Profile HB Project",
		Slug: "profile-hb-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-profile-hb",
		Name:   "Profile HB Broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID:              "agent-profile-hb",
		Slug:            "profile-hb-agent",
		Name:            "Profile HB Agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
		AppliedConfig:   &store.AgentAppliedConfig{},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Verify profile is initially empty
	fetched, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Empty(t, fetched.AppliedConfig.Profile)

	// Send heartbeat with profile
	heartbeat := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:    project.ID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:    agent.Slug,
				Phase:   string(state.PhaseRunning),
				Profile: "k8s-prod",
			}},
		}},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", heartbeat)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify profile was backfilled
	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "k8s-prod", updated.AppliedConfig.Profile,
		"Profile should be backfilled from heartbeat")
}

// TestCreateAgent_HarnessNotTemplateUUID verifies that when the template is
// specified as a UUID that doesn't resolve on the hub (e.g., broker has it
// locally), the harness is taken from the explicit Harness field, not from
// the UUID string in Template.
func TestCreateAgent_HarnessNotTemplateUUID(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Update the existing provider to have a LocalPath so the hub allows
	// the template to be resolved locally by the broker.
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   "broker-create",
		BrokerName: "Create Test Broker",
		LocalPath:  "/some/local/path",
		Status:     "online",
	}))

	// Create agent with template UUID that doesn't exist on hub + explicit harness
	templateUUID := "003879ad-f000-426d-b52f-08f537c4c6ce"
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "uuid-tmpl-agent",
		ProjectID:       project.ID,
		Template:      templateUUID,
		HarnessConfig: "gemini",
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "gemini", agent.AppliedConfig.HarnessConfig,
		"AppliedConfig.HarnessConfig should be the harness config name, not the template UUID")
	assert.NotEqual(t, templateUUID, agent.AppliedConfig.HarnessConfig,
		"AppliedConfig.HarnessConfig must not contain the template UUID")
}

// ---------------------------------------------------------------------------
// Project-scoped existing-agent tests (mirror createAgent tests)
// ---------------------------------------------------------------------------

func TestCreateProjectAgent_RecreateFromRunningStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	runningAgent := &store.Agent{
		ID:              "project-agent-running",
		Slug:            "running-project-agent",
		Name:            "running-project-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, runningAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID),
		CreateAgentRequest{
			Name: "running-project-agent",
			Task: "new task",
		})

	require.Equal(t, http.StatusCreated, rec.Code,
		"re-creating a running project agent should succeed with 201")

	_, err := s.GetAgent(ctx, "project-agent-running")
	assert.ErrorIs(t, err, store.ErrNotFound, "old running agent should be deleted")

	assert.True(t, disp.deleteCalled, "dispatcher should have been asked to delete old agent")

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.NotEqual(t, "project-agent-running", resp.Agent.ID)
	assert.Equal(t, string(state.PhaseRunning), resp.Agent.Phase)
}

func TestCreateProjectAgent_RecreateFromStoppedStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	stoppedAgent := &store.Agent{
		ID:              "project-agent-stopped",
		Slug:            "stopped-project-agent",
		Name:            "stopped-project-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, stoppedAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID),
		CreateAgentRequest{
			Name: "stopped-project-agent",
			Task: "restart after stop",
		})

	require.Equal(t, http.StatusCreated, rec.Code,
		"re-creating a stopped project agent should succeed with 201")

	_, err := s.GetAgent(ctx, "project-agent-stopped")
	assert.ErrorIs(t, err, store.ErrNotFound, "old stopped agent should be deleted")
}

func TestCreateProjectAgent_RecreateFromErrorStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	errorAgent := &store.Agent{
		ID:              "project-agent-errored",
		Slug:            "errored-project-agent",
		Name:            "errored-project-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseError),
	}
	require.NoError(t, s.CreateAgent(ctx, errorAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID),
		CreateAgentRequest{
			Name: "errored-project-agent",
			Task: "retry after error",
		})

	require.Equal(t, http.StatusCreated, rec.Code,
		"re-creating an errored project agent should succeed with 201")

	_, err := s.GetAgent(ctx, "project-agent-errored")
	assert.ErrorIs(t, err, store.ErrNotFound, "old errored agent should be deleted")
}

func TestCreateProjectAgent_RestartFromProvisioningStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	provAgent := &store.Agent{
		ID:              "project-agent-prov",
		Slug:            "prov-project-agent",
		Name:            "prov-project-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseProvisioning),
	}
	require.NoError(t, s.CreateAgent(ctx, provAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID),
		CreateAgentRequest{
			Name: "prov-project-agent",
			Task: "retry task",
		})

	assert.Equal(t, http.StatusOK, rec.Code,
		"re-starting a provisioning project agent should succeed (200)")

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.Equal(t, string(state.PhaseRunning), resp.Agent.Phase)
}

func TestCreateProjectAgent_RestartFromPendingStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	pendingAgent := &store.Agent{
		ID:              "project-agent-pending",
		Slug:            "pending-project-agent",
		Name:            "pending-project-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, pendingAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID),
		CreateAgentRequest{
			Name: "pending-project-agent",
			Task: "retry task",
		})

	assert.Equal(t, http.StatusOK, rec.Code,
		"re-starting a pending project agent should succeed (200)")
}

// ---------------------------------------------------------------------------
// Config update and broker-ID recovery tests
// ---------------------------------------------------------------------------

func TestCreateProjectAgent_ConfigUpdateOnRestart(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	existingAgent := &store.Agent{
		ID:              "project-agent-config",
		Slug:            "config-project-agent",
		Name:            "config-project-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseCreated),
		AppliedConfig: &store.AgentAppliedConfig{
			Task:   "old task",
			Attach: false,
		},
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID),
		CreateAgentRequest{
			Name:   "config-project-agent",
			Task:   "new task",
			Attach: true,
		})

	require.Equal(t, http.StatusOK, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig)
	assert.Equal(t, "new task", persisted.AppliedConfig.Task,
		"task should be updated on restart")
	assert.True(t, persisted.AppliedConfig.Attach,
		"attach should be updated on restart")
}

func TestCreateProjectAgent_BrokerIDRecovery(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create agent with empty RuntimeBrokerID (simulates agent created
	// before a broker was registered).
	existingAgent := &store.Agent{
		ID:              "project-agent-no-broker",
		Slug:            "no-broker-project-agent",
		Name:            "no-broker-project-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "", // empty — should be recovered
		Phase:           string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/projects/%s/agents", project.ID),
		CreateAgentRequest{
			Name: "no-broker-project-agent",
			Task: "start with recovered broker",
		})

	require.Equal(t, http.StatusOK, rec.Code,
		"agent with empty broker ID should be started once broker is resolved")

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "broker-create", persisted.RuntimeBrokerID,
		"RuntimeBrokerID should be recovered from resolved broker")
}

func TestCreateAgent_BrokerIDRecovery(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	existingAgent := &store.Agent{
		ID:              "agent-no-broker",
		Slug:            "no-broker-agent",
		Name:            "no-broker-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "", // empty — should be recovered
		Phase:           string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "no-broker-agent",
		ProjectID: project.ID,
		Task:    "start with recovered broker",
	})

	require.Equal(t, http.StatusOK, rec.Code,
		"agent with empty broker ID should be started once broker is resolved")

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "broker-create", persisted.RuntimeBrokerID,
		"RuntimeBrokerID should be recovered from resolved broker")
}

func TestCreateAgent_CleanupModeStrictFailsOnBrokerDeleteError(t *testing.T) {
	disp := &createAgentDispatcher{
		createPhase: string(state.PhaseRunning),
		deleteErr:   fmt.Errorf("broker delete failed"),
	}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	existingAgent := &store.Agent{
		ID:              "agent-stale-strict",
		Slug:            "stale-strict-agent",
		Name:            "stale-strict-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:        "stale-strict-agent",
		ProjectID:     project.ID,
		CleanupMode: "strict",
	})
	require.Equal(t, http.StatusBadGateway, rec.Code)
	assert.True(t, disp.deleteCalled, "expected broker delete attempt in strict mode")

	persisted, err := s.GetAgent(ctx, existingAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, existingAgent.ID, persisted.ID, "strict mode should keep existing DB record")
}

func TestCreateAgent_CleanupModeForceContinuesOnBrokerDeleteError(t *testing.T) {
	disp := &createAgentDispatcher{
		createPhase: string(state.PhaseRunning),
		deleteErr:   fmt.Errorf("broker delete failed"),
	}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	existingAgent := &store.Agent{
		ID:              "agent-stale-force",
		Slug:            "stale-force-agent",
		Name:            "stale-force-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:        "stale-force-agent",
		ProjectID:     project.ID,
		CleanupMode: "force",
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.True(t, disp.deleteCalled, "expected broker delete attempt in force mode")

	_, err := s.GetAgent(ctx, existingAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "force mode should replace stale DB record")
}

func TestCreateAgent_InvalidCleanupMode(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:        "invalid-cleanup-agent",
		ProjectID:     project.ID,
		CleanupMode: "sometimes",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Phase 3: Notification Subscription on Agent Create ---

func TestCreateAgent_NotifyCreatesSubscription(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project and broker infrastructure
	project := &store.Project{
		ID:   "project-notify",
		Name: "Notify Project",
		Slug: "notify-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-notify",
		Name:   "Notify Broker",
		Slug:   "notify-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	// Create the calling agent (the one that will subscribe to notifications)
	callingAgent := &store.Agent{
		ID:      "agent-lead",
		Slug:    "lead-agent",
		Name:    "Lead Agent",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, callingAgent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	t.Run("Notify=true creates subscription for agent caller", func(t *testing.T) {
		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
			ScopeAgentCreate,
			ScopeAgentNotify,
		}, nil)
		require.NoError(t, err)

		body, _ := json.Marshal(CreateAgentRequest{
			Name:    "Sub Worker",
			ProjectID: project.ID,
			Task:    "implement auth module",
			Notify:  true,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", token)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code)

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent)

		// Verify subscription was created for the new agent
		subs, err := s.GetNotificationSubscriptions(ctx, resp.Agent.ID)
		require.NoError(t, err)
		require.Len(t, subs, 1, "should have exactly one subscription")

		sub := subs[0]
		assert.Equal(t, resp.Agent.ID, sub.AgentID)
		assert.Equal(t, store.SubscriberTypeAgent, sub.SubscriberType)
		assert.Equal(t, callingAgent.Slug, sub.SubscriberID)
		assert.Equal(t, project.ID, sub.ProjectID)
		assert.Equal(t, callingAgent.ID, sub.CreatedBy)
		assert.Contains(t, sub.TriggerActivities, "COMPLETED")
		assert.Contains(t, sub.TriggerActivities, "WAITING_FOR_INPUT")
		assert.Contains(t, sub.TriggerActivities, "LIMITS_EXCEEDED")
		assert.Contains(t, sub.TriggerActivities, "STALLED")
		assert.Contains(t, sub.TriggerActivities, "ERROR")
	})

	t.Run("Notify=false does not create subscription", func(t *testing.T) {
		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, project.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
			ScopeAgentCreate,
		}, nil)
		require.NoError(t, err)

		body, _ := json.Marshal(CreateAgentRequest{
			Name:    "Sub Worker No Notify",
			ProjectID: project.ID,
			Task:    "implement tests",
			Notify:  false,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", token)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code)

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent)

		subs, err := s.GetNotificationSubscriptions(ctx, resp.Agent.ID)
		require.NoError(t, err)
		assert.Len(t, subs, 0, "should have no subscriptions when notify=false")
	})

	t.Run("Notify=true for user caller creates user subscription", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name:    "User Notified Agent",
			ProjectID: project.ID,
			Task:    "run analysis",
			Notify:  true,
		})
		require.Equal(t, http.StatusCreated, rec.Code)

		var resp CreateAgentResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		require.NotNil(t, resp.Agent)

		subs, err := s.GetNotificationSubscriptions(ctx, resp.Agent.ID)
		require.NoError(t, err)
		require.Len(t, subs, 1, "should have exactly one subscription")

		sub := subs[0]
		assert.Equal(t, resp.Agent.ID, sub.AgentID)
		assert.Equal(t, store.SubscriberTypeUser, sub.SubscriberType)
		assert.Equal(t, project.ID, sub.ProjectID)
	})
}

func TestCreateAgent_NotifySubscriptionCascadeOnDelete(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-cascade",
		Name: "Cascade Project",
		Slug: "cascade-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-cascade",
		Name:   "Cascade Broker",
		Slug:   "cascade-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	callingAgent := &store.Agent{
		ID:      "agent-cascade-lead",
		Slug:    "cascade-lead",
		Name:    "Cascade Lead",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, callingAgent))

	tokenSvc := srv.GetAgentTokenService()
	token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, project.ID, []AgentTokenScope{
		ScopeAgentStatusUpdate,
		ScopeAgentCreate,
		ScopeAgentNotify,
	}, nil)
	require.NoError(t, err)

	// Create agent with notify
	body, _ := json.Marshal(CreateAgentRequest{
		Name:    "Cascade Sub",
		ProjectID: project.ID,
		Task:    "do work",
		Notify:  true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
	req.Header.Set("X-Scion-Agent-Token", token)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Verify subscription exists
	subs, err := s.GetNotificationSubscriptions(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.Len(t, subs, 1)

	// Delete the agent — subscriptions should cascade delete
	require.NoError(t, s.DeleteAgent(ctx, resp.Agent.ID))

	subs, err = s.GetNotificationSubscriptions(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.Len(t, subs, 0, "subscriptions should be cascade-deleted with agent")
}

func TestBrokerHeartbeat_PublishesActivitySSE(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Wire up a real event publisher so we can subscribe to SSE events
	pub := NewChannelEventPublisher()
	defer pub.Close()
	srv.SetEventPublisher(pub)

	// Create project, broker, and agent
	project := &store.Project{ID: "project-hb-sse", Name: "HB SSE Project", Slug: "hb-sse-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID: "broker-hb-sse", Name: "HB SSE Broker", Slug: "hb-sse-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-hb-sse", Slug: "agent-hb-slug", Name: "HB SSE Agent",
		ProjectID: project.ID, RuntimeBrokerID: broker.ID,
		Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Subscribe to agent-specific status events
	ch, unsub := pub.Subscribe("agent." + agent.ID + ".status")
	defer unsub()

	// Send broker heartbeat with an activity change
	heartbeat := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:    project.ID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:     agent.Slug,
				Phase:    string(state.PhaseRunning),
				Activity: "thinking",
			}},
		}},
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", heartbeat)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify store was updated
	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "thinking", updated.Activity)

	// Verify SSE event was published
	select {
	case evt := <-ch:
		assert.Equal(t, "agent."+agent.ID+".status", evt.Subject)
		var statusEvt AgentStatusEvent
		require.NoError(t, json.Unmarshal(evt.Data, &statusEvt))
		assert.Equal(t, "thinking", statusEvt.Activity)
		assert.Equal(t, string(state.PhaseRunning), statusEvt.Phase)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SSE event from broker heartbeat")
	}
}

func TestBrokerHeartbeat_RepeatedActivityDoesNotRefreshLastActivityEvent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project, broker, and agent
	project := &store.Project{ID: "project-stall-hb", Name: "Stall HB Project", Slug: "stall-hb-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID: "broker-stall-hb", Name: "Stall HB Broker", Slug: "stall-hb-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-stall-hb", Slug: "stall-hb-slug", Name: "Stall HB Agent",
		ProjectID: project.ID, RuntimeBrokerID: broker.ID,
		Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// First heartbeat: set activity to "thinking" — this should set last_activity_event
	hb1 := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:    project.ID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:     agent.Slug,
				Phase:    string(state.PhaseRunning),
				Activity: "thinking",
			}},
		}},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb1)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify activity was set and record last_activity_event
	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "thinking", updated.Activity)
	assert.False(t, updated.LastActivityEvent.IsZero(), "last_activity_event should be set after first heartbeat with activity")

	// Backdate last_activity_event to simulate time passing
	pastTime := time.Now().Add(-10 * time.Minute)
	db := s.(*sqlite.SQLiteStore).DB()
	_, err = db.ExecContext(ctx, "UPDATE agents SET last_activity_event = ? WHERE id = ?", pastTime, agent.ID)
	require.NoError(t, err)

	// Second heartbeat: same activity "thinking" — should NOT refresh last_activity_event
	hb2 := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:    project.ID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:     agent.Slug,
				Phase:    string(state.PhaseRunning),
				Activity: "thinking",
			}},
		}},
	}
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb2)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify last_activity_event was NOT refreshed (still in the past)
	updated, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "thinking", updated.Activity)
	assert.WithinDuration(t, pastTime, updated.LastActivityEvent, time.Second,
		"last_activity_event should NOT be refreshed by a heartbeat repeating the same activity")

	// Third heartbeat: different activity "executing" — SHOULD refresh last_activity_event
	hb3 := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:    project.ID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:     agent.Slug,
				Phase:    string(state.PhaseRunning),
				Activity: "executing",
			}},
		}},
	}
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb3)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify last_activity_event WAS refreshed (now recent)
	updated, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "executing", updated.Activity)
	assert.WithinDuration(t, time.Now(), updated.LastActivityEvent, 5*time.Second,
		"last_activity_event should be refreshed when activity changes")
}

func TestBrokerHeartbeat_StalledAgentNotOverwrittenBySameActivity(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project, broker, and agent
	project := &store.Project{ID: "project-stall-keep", Name: "Stall Keep Project", Slug: "stall-keep-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID: "broker-stall-keep", Name: "Stall Keep Broker", Slug: "stall-keep-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-stall-keep", Slug: "stall-keep-slug", Name: "Stall Keep Agent",
		ProjectID: project.ID, RuntimeBrokerID: broker.ID,
		Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Set agent to running+thinking
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityThinking),
	}))

	// Simulate stalled detection: mark agent stalled with stalled_from_activity = thinking
	db := s.(*sqlite.SQLiteStore).DB()
	staleActivity := time.Now().Add(-10 * time.Minute)
	_, err := db.ExecContext(ctx,
		"UPDATE agents SET activity = 'stalled', stalled_from_activity = 'thinking', last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivity, time.Now().Add(-10*time.Second), agent.ID)
	require.NoError(t, err)

	// Send heartbeat reporting the same pre-stall activity ("thinking")
	hb := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:    project.ID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:     agent.Slug,
				Phase:    string(state.PhaseRunning),
				Activity: "thinking",
			}},
		}},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Agent should still be stalled — heartbeat with same activity should NOT overwrite
	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "stalled", updated.Activity, "agent should remain stalled when heartbeat reports same pre-stall activity")
	assert.Equal(t, "thinking", updated.StalledFromActivity, "stalled_from_activity should be preserved")
}

func TestBrokerHeartbeat_StalledAgentRecoveredByNewActivity(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create project, broker, and agent
	project := &store.Project{ID: "project-stall-recover", Name: "Stall Recover Project", Slug: "stall-recover-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID: "broker-stall-recover", Name: "Stall Recover Broker", Slug: "stall-recover-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-stall-recover", Slug: "stall-recover-slug", Name: "Stall Recover Agent",
		ProjectID: project.ID, RuntimeBrokerID: broker.ID,
		Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Set agent to running+thinking
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityThinking),
	}))

	// Simulate stalled detection: mark agent stalled with stalled_from_activity = thinking
	db := s.(*sqlite.SQLiteStore).DB()
	staleActivity := time.Now().Add(-10 * time.Minute)
	_, err := db.ExecContext(ctx,
		"UPDATE agents SET activity = 'stalled', stalled_from_activity = 'thinking', last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivity, time.Now().Add(-10*time.Second), agent.ID)
	require.NoError(t, err)

	// Send heartbeat reporting a genuinely new activity ("executing")
	hb := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:    project.ID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:     agent.Slug,
				Phase:    string(state.PhaseRunning),
				Activity: "executing",
			}},
		}},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Agent should recover — new activity is genuinely different from stalled_from_activity
	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "executing", updated.Activity, "agent should recover when heartbeat reports genuinely new activity")
	assert.Empty(t, updated.StalledFromActivity, "stalled_from_activity should be cleared on recovery")
}

func TestBrokerHeartbeat_StalledIdleAgentNotOverwrittenBySameActivity(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-stall-idle", Name: "Stall Idle Project", Slug: "stall-idle-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID: "broker-stall-idle", Name: "Stall Idle Broker", Slug: "stall-idle-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-stall-idle", Slug: "stall-idle-slug", Name: "Stall Idle Agent",
		ProjectID: project.ID, RuntimeBrokerID: broker.ID,
		Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Set agent to running+idle
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityIdle),
	}))

	// Simulate stalled detection: mark agent stalled with stalled_from_activity = idle
	db := s.(*sqlite.SQLiteStore).DB()
	staleActivity := time.Now().Add(-10 * time.Minute)
	_, err := db.ExecContext(ctx,
		"UPDATE agents SET activity = 'stalled', stalled_from_activity = 'idle', last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivity, time.Now().Add(-10*time.Second), agent.ID)
	require.NoError(t, err)

	// Send heartbeat still reporting "idle" — should NOT clear the stall
	hb := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:    project.ID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:     agent.Slug,
				Phase:    string(state.PhaseRunning),
				Activity: "idle",
			}},
		}},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
	assert.Equal(t, http.StatusOK, rec.Code)

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "stalled", updated.Activity, "agent should remain stalled when heartbeat reports same pre-stall idle activity")
	assert.Equal(t, "idle", updated.StalledFromActivity, "stalled_from_activity should be preserved")
}

func TestBrokerHeartbeat_DoesNotRevertStoppedAgent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-stop-revert", Name: "Stop Revert Project", Slug: "stop-revert-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID: "broker-stop-revert", Name: "Stop Revert Broker", Slug: "stop-revert-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-stop-revert", Slug: "stop-revert-slug", Name: "Stop Revert Agent",
		ProjectID: project.ID, RuntimeBrokerID: broker.ID,
		Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Simulate hub setting agent to stopped (as handleAgentLifecycle does)
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:           string(state.PhaseStopped),
		Activity:        "",
		ContainerStatus: "stopped",
	}))

	// Verify agent is stopped
	stopped, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), stopped.Phase)
	assert.Empty(t, stopped.Activity)

	// Send a heartbeat with stale data (as if the forced heartbeat raced
	// and still reports the old running+completed state)
	hb := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:    project.ID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:            agent.Slug,
				Phase:           string(state.PhaseRunning),
				Activity:        "completed",
				ContainerStatus: "running",
			}},
		}},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Agent should remain stopped — heartbeat must not revert terminal phase
	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), updated.Phase, "heartbeat should not revert stopped phase")
	assert.Empty(t, updated.Activity, "heartbeat should not restore activity on stopped agent")
}

func TestBrokerHeartbeat_DoesNotRevertStoppedAgent_LegacyPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-stop-legacy", Name: "Stop Legacy Project", Slug: "stop-legacy-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID: "broker-stop-legacy", Name: "Stop Legacy Broker", Slug: "stop-legacy-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-stop-legacy", Slug: "stop-legacy-slug", Name: "Stop Legacy Agent",
		ProjectID: project.ID, RuntimeBrokerID: broker.ID,
		Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Set agent to stopped
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:           string(state.PhaseStopped),
		Activity:        "",
		ContainerStatus: "stopped",
	}))

	// Send heartbeat with NO Phase (legacy path) but container reports "Up"
	hb := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:    project.ID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:            agent.Slug,
				ContainerStatus: "Up 5 minutes",
			}},
		}},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Agent should remain stopped
	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), updated.Phase, "legacy heartbeat should not revert stopped phase")
}

func TestBrokerHeartbeat_PropagatesTerminalActivityOnStoppedAgent(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	grove := &store.Project{ID: "grove-crash-hb", Name: "Crash HB Grove", Slug: "crash-hb-grove"}
	require.NoError(t, s.CreateProject(ctx, grove))

	broker := &store.RuntimeBroker{
		ID: "broker-crash-hb", Name: "Crash HB Broker", Slug: "crash-hb-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-crash-hb", Slug: "crash-hb-slug", Name: "Crash HB Agent",
		ProjectID: grove.ID, RuntimeBrokerID: broker.ID,
		Phase: string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Send heartbeat confirming stopped phase with crashed activity.
	// This simulates the broker relaying crash state that the direct Hub
	// report missed (race condition fix).
	hb := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:    grove.ID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:     agent.Slug,
				Phase:    string(state.PhaseStopped),
				Activity: string(state.ActivityCrashed),
				Message:  "crashed with exit code 1",
			}},
		}},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Agent should now have crashed activity
	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), updated.Phase)
	assert.Equal(t, string(state.ActivityCrashed), updated.Activity,
		"heartbeat should propagate terminal activity on stopped agent")
	assert.Equal(t, "crashed with exit code 1", updated.Message,
		"heartbeat should propagate crash message")
}

func TestBrokerHeartbeat_DoesNotOverwriteTerminalActivityWithNonTerminal(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	grove := &store.Project{ID: "grove-term-guard", Name: "Term Guard Grove", Slug: "term-guard-grove"}
	require.NoError(t, s.CreateProject(ctx, grove))

	broker := &store.RuntimeBroker{
		ID: "broker-term-guard", Name: "Term Guard Broker", Slug: "term-guard-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-term-guard", Slug: "term-guard-slug", Name: "Term Guard Agent",
		ProjectID: grove.ID, RuntimeBrokerID: broker.ID,
		Phase:    string(state.PhaseStopped),
		Activity: string(state.ActivityCrashed),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Send heartbeat with non-terminal activity — should be blocked
	hb := brokerHeartbeatRequest{
		Status: "online",
		Projects: []brokerProjectHeartbeat{{
			ProjectID:    grove.ID,
			AgentCount: 1,
			Agents: []brokerAgentHeartbeat{{
				Slug:     agent.Slug,
				Phase:    string(state.PhaseStopped),
				Activity: string(state.ActivityIdle),
			}},
		}},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/runtime-brokers/"+broker.ID+"/heartbeat", hb)
	assert.Equal(t, http.StatusOK, rec.Code)

	// Agent should retain crashed activity
	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.ActivityCrashed), updated.Activity,
		"heartbeat should not overwrite terminal activity with non-terminal")
}

func TestCreateAgent_RestartCreatesNotificationSubscription(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent in "created" phase (provisioned but not started)
	existingAgent := &store.Agent{
		ID:              "agent-notify-restart",
		Slug:            "notify-agent",
		Name:            "notify-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	// Restart the agent with Notify: true — should create a notification subscription
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "notify-agent",
		ProjectID: project.ID,
		Task:    "restart task",
		Notify:  true,
	})

	assert.Equal(t, http.StatusOK, rec.Code, "restarting agent should succeed")

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	// Verify a notification subscription was created for the agent
	subs, err := s.GetNotificationSubscriptions(ctx, existingAgent.ID)
	require.NoError(t, err)
	assert.Len(t, subs, 1, "expected one notification subscription after restart with Notify")
	assert.Equal(t, existingAgent.ID, subs[0].AgentID)
	assert.Equal(t, project.ID, subs[0].ProjectID)
}

func TestCreateAgent_RestartNoSubscriptionWithoutNotify(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent in "created" phase
	existingAgent := &store.Agent{
		ID:              "agent-no-notify",
		Slug:            "no-notify-agent",
		Name:            "no-notify-agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	// Restart the agent without Notify — should NOT create a subscription
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "no-notify-agent",
		ProjectID: project.ID,
		Task:    "restart task",
	})

	assert.Equal(t, http.StatusOK, rec.Code)

	// Verify no notification subscription was created
	subs, err := s.GetNotificationSubscriptions(ctx, existingAgent.ID)
	require.NoError(t, err)
	assert.Len(t, subs, 0, "expected no notification subscription without Notify flag")
}

func TestHandleAgentMessage_PlainTextBuildsStructuredMessage(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-msg",
		Name: "Msg Test Project",
		Slug: "msg-test-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-msg",
		Name:   "Msg Test Broker",
		Slug:   "msg-test-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-msg-1",
		Slug:            "agent-msg-1",
		Name:            "Msg Agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	disp := &recordingDispatcher{}
	srv.SetDispatcher(disp)

	// Send a plain-text message (no structured_message field)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message":   "hello from the UI",
		"interrupt": false,
	})
	require.Equal(t, http.StatusOK, rec.Code, "response body: %s", rec.Body.String())

	calls := disp.getCalls()
	require.Len(t, calls, 1, "expected exactly one dispatch call")

	call := calls[0]
	assert.Equal(t, "hello from the UI", call.Message)
	require.NotNil(t, call.StructuredMessage, "expected a StructuredMessage to be constructed from the plain text")

	sm := call.StructuredMessage
	assert.Equal(t, messages.Version, sm.Version)
	assert.Equal(t, messages.TypeInstruction, sm.Type)
	assert.Equal(t, "hello from the UI", sm.Msg)
	assert.Equal(t, "agent:"+agent.Slug, sm.Recipient)
	// Dev auth sets DisplayName to "Development User"
	assert.Equal(t, "user:Development User", sm.Sender)
	assert.NotEmpty(t, sm.Timestamp)
}

// TestHandleAgentMessage_StructuredMessagePopulatesSender verifies that when
// a structured_message is sent without a sender (e.g. from the web UI), the
// handler populates the sender from the authenticated user identity.
func TestHandleAgentMessage_StructuredMessagePopulatesSender(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-msg-sender",
		Name: "Msg Sender Project",
		Slug: "msg-sender-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-msg-sender",
		Name:   "Msg Sender Broker",
		Slug:   "msg-sender-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-msg-sender-1",
		Slug:            "agent-msg-sender-1",
		Name:            "Msg Sender Agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	disp := &recordingDispatcher{}
	srv.SetDispatcher(disp)

	// Send a structured_message without sender (simulates web UI)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"structured_message": map[string]interface{}{
			"msg":   "hello from web UI",
			"plain": true,
		},
		"interrupt": false,
	})
	require.Equal(t, http.StatusOK, rec.Code, "response body: %s", rec.Body.String())

	calls := disp.getCalls()
	require.Len(t, calls, 1, "expected exactly one dispatch call")

	sm := calls[0].StructuredMessage
	require.NotNil(t, sm, "expected a StructuredMessage")
	assert.Equal(t, "hello from web UI", sm.Msg)
	assert.Equal(t, "agent:"+agent.Slug, sm.Recipient)
	// Dev auth sets DisplayName to "Development User"
	assert.Equal(t, "user:Development User", sm.Sender, "sender should be populated from authenticated user")
	assert.NotEmpty(t, sm.SenderID, "sender ID should be populated")
}

// TestHandleAgentMessage_NotifyCreatesSubscription verifies that sending a message
// with notify=true creates a notification subscription for the sender.
func TestHandleAgentMessage_NotifyCreatesSubscription(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-msg-notify",
		Name: "Msg Notify Project",
		Slug: "msg-notify-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-msg-notify",
		Name:   "Msg Notify Broker",
		Slug:   "msg-notify-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-msg-notify-1",
		Slug:            "agent-msg-notify-1",
		Name:            "Msg Notify Agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	disp := &recordingDispatcher{}
	srv.SetDispatcher(disp)

	// Send a message with notify=true
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message":   "check on this",
		"interrupt": false,
		"notify":    true,
	})
	require.Equal(t, http.StatusOK, rec.Code, "response body: %s", rec.Body.String())

	// Verify the message was dispatched
	calls := disp.getCalls()
	require.Len(t, calls, 1)

	// Verify a notification subscription was created
	subs, err := s.GetNotificationSubscriptions(ctx, agent.ID)
	require.NoError(t, err)
	require.Len(t, subs, 1, "expected one notification subscription for the agent")
	assert.Equal(t, store.SubscriberTypeUser, subs[0].SubscriberType)
	assert.Equal(t, agent.ProjectID, subs[0].ProjectID)
}

// TestHandleAgentMessage_NoNotifyNoSubscription verifies that sending a message
// without notify=true does NOT create a notification subscription.
func TestHandleAgentMessage_NoNotifyNoSubscription(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-msg-no-notify",
		Name: "Msg No Notify Project",
		Slug: "msg-no-notify-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-msg-no-notify",
		Name:   "Msg No Notify Broker",
		Slug:   "msg-no-notify-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-msg-no-notify-1",
		Slug:            "agent-msg-no-notify-1",
		Name:            "Msg No Notify Agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	disp := &recordingDispatcher{}
	srv.SetDispatcher(disp)

	// Send a message without notify
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message":   "just a message",
		"interrupt": false,
	})
	require.Equal(t, http.StatusOK, rec.Code)

	// Verify no subscription was created
	subs, err := s.GetNotificationSubscriptions(ctx, agent.ID)
	require.NoError(t, err)
	assert.Len(t, subs, 0, "no subscription should be created without notify flag")
}

// TestHandleAgentMessage_NoDispatcher_Returns503 verifies that sending a message
// when no dispatcher is configured returns 503 with a Retry-After header.
func TestHandleAgentMessage_NoDispatcher_Returns503(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-msg-503",
		Name: "Msg 503 Project",
		Slug: "msg-503-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:      "agent-msg-503",
		Slug:    "agent-msg-503",
		Name:    "Msg 503 Agent",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Do NOT set a dispatcher — simulates server still starting up
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message": "hello",
	})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "5", rec.Header().Get("Retry-After"))
	assert.Contains(t, rec.Body.String(), "starting up")
}

// TestHandleAgentMessage_NoBrokerID_Returns503 verifies that sending a message
// to an agent with no RuntimeBrokerID returns 503 with a Retry-After header.
func TestHandleAgentMessage_NoBrokerID_Returns503(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-msg-503-nobroker",
		Name: "Msg 503 NoBroker Project",
		Slug: "msg-503-nobroker-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:      "agent-msg-503-nobroker",
		Slug:    "agent-msg-503-nobroker",
		Name:    "Msg 503 NoBroker Agent",
		ProjectID: project.ID,
		Phase:   string(state.PhaseRunning),
		// No RuntimeBrokerID set
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	disp := &recordingDispatcher{}
	srv.SetDispatcher(disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/message", map[string]interface{}{
		"message": "hello",
	})
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "5", rec.Header().Get("Retry-After"))
	assert.Contains(t, rec.Body.String(), "no runtime broker assigned")

	// Verify no dispatch was attempted
	assert.Len(t, disp.getCalls(), 0)
}

// TestCreateAgent_DispatchFailure_CleansUpBroker verifies that when the dispatch
// to the runtime broker fails (e.g. auth resolution error), the hub dispatches a
// delete with deleteFiles=true to clean up provisioned files on the broker, and
// then deletes the agent record from the hub store.
func TestCreateAgent_DispatchFailure_CleansUpBroker(t *testing.T) {
	disp := &failingCreateDispatcher{
		createErr: fmt.Errorf("auth resolution failed: gemini: auth type \"api-key\" selected but no API key found"),
	}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "auth-fail-agent",
		ProjectID: project.ID,
		Task:    "do something",
	})

	// Should return a runtime error
	require.Equal(t, http.StatusBadGateway, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeRuntimeError, errResp.Error.Code)
	assert.Contains(t, errResp.Error.Message, "auth resolution failed")

	// Verify delete was dispatched to the broker with deleteFiles=true
	assert.True(t, disp.deleteCalled, "hub should dispatch delete to broker to clean up provisioned files")
	assert.True(t, disp.deleteCalledFiles, "delete should request file cleanup (deleteFiles=true)")
	assert.True(t, disp.deleteBranch, "delete should request branch cleanup (removeBranch=true)")

	// Verify agent record was deleted from hub store
	_, err := s.GetAgent(ctx, "auth-fail-agent")
	assert.ErrorIs(t, err, store.ErrNotFound, "agent should be deleted from hub store after dispatch failure")
}

// --- GCP Identity Assignment Tests ---

func TestCreateAgent_GCPIdentityAssign(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Register and verify a GCP service account
	sa := &store.GCPServiceAccount{
		ID:         "sa-assign-1",
		Scope:      store.ScopeProject,
		ScopeID:    project.ID,
		Email:      "worker@project.iam.gserviceaccount.com",
		ProjectID:  "my-project",
		Verified:   true,
		VerifiedAt: time.Now(),
		CreatedBy:  "user-1",
		CreatedAt:  time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-assign-agent",
		ProjectID: project.ID,
		Task:    "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     "assign",
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	require.NotNil(t, resp.Agent.AppliedConfig)
	require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeAssign, resp.Agent.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Equal(t, sa.ID, resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountID)
	assert.Equal(t, sa.Email, resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountEmail)
	assert.Equal(t, sa.ProjectID, resp.Agent.AppliedConfig.GCPIdentity.ProjectID)

	// Verify persistence
	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, persisted.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeAssign, persisted.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Equal(t, sa.ID, persisted.AppliedConfig.GCPIdentity.ServiceAccountID)
}

func TestCreateAgent_GCPIdentityBlock(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-block-agent",
		ProjectID: project.ID,
		Task:    "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "block",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeBlock, resp.Agent.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Empty(t, resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountID)

	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.GCPMetadataModeBlock, persisted.AppliedConfig.GCPIdentity.MetadataMode)
}

func TestCreateAgent_GCPIdentityPassthrough(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-passthrough-agent",
		ProjectID: project.ID,
		Task:    "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, resp.Agent.AppliedConfig.GCPIdentity.MetadataMode)
}

func TestCreateAgent_GCPIdentityNoField(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-none-agent",
		ProjectID: project.ID,
		Task:    "do something",
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	// When no GCP identity is specified, default to "block" to prevent
	// leaking the underlying compute identity.
	require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity, "GCPIdentity should default to block when not specified")
	assert.Equal(t, store.GCPMetadataModeBlock, resp.Agent.AppliedConfig.GCPIdentity.MetadataMode)
	assert.Empty(t, resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountID)
}

func TestCreateAgent_GCPIdentityInvalidMode(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-invalid-mode",
		ProjectID: project.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "invalid",
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPIdentityAssignMissingSA(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-missing-sa",
		ProjectID: project.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "assign",
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPIdentityAssignNonexistentSA(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-nonexistent-sa",
		ProjectID: project.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     "assign",
			ServiceAccountID: "nonexistent-sa-id",
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPIdentityAssignUnverifiedSA(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	sa := &store.GCPServiceAccount{
		ID:        "sa-unverified-1",
		Scope:     store.ScopeProject,
		ScopeID:   project.ID,
		Email:     "unverified@project.iam.gserviceaccount.com",
		ProjectID: "my-project",
		Verified:  false,
		CreatedBy: "user-1",
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-unverified-sa",
		ProjectID: project.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     "assign",
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPIdentityAssignWrongProject(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	sa := &store.GCPServiceAccount{
		ID:         "sa-other-project-1",
		Scope:      store.ScopeProject,
		ScopeID:    "other-project-id",
		Email:      "other@project.iam.gserviceaccount.com",
		ProjectID:  "my-project",
		Verified:   true,
		VerifiedAt: time.Now(),
		CreatedBy:  "user-1",
		CreatedAt:  time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-wrong-project",
		ProjectID: project.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     "assign",
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPIdentityBlockWithSAID(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-block-with-sa",
		ProjectID: project.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     "block",
			ServiceAccountID: "should-not-be-here",
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPIdentityPassthroughWithSAID(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-passthrough-with-sa",
		ProjectID: project.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     "passthrough",
			ServiceAccountID: "should-not-be-here",
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPPassthrough_BrokerOwnerAllowed(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create a user who owns the broker
	owner := &store.User{
		ID:          "user-broker-owner",
		Email:       "owner@test.com",
		DisplayName: "Broker Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))
	ensureHubMembership(ctx, s, owner.ID)

	// Create a project owned by the broker owner with proper policies
	project := &store.Project{
		ID:        "project-pt-owner",
		Name:      "Passthrough Owner Project",
		Slug:      "passthrough-owner-project",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// Create a broker owned by the same user
	broker := &store.RuntimeBroker{
		ID:        "broker-pt-owner",
		Name:      "Owner Broker",
		Slug:      "owner-broker",
		Status:    store.BrokerStatusOnline,
		CreatedBy: owner.ID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	// Broker owner should be allowed to use passthrough
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "pt-owner-agent",
		ProjectID: project.ID,
		Task:    "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModePassthrough, resp.Agent.AppliedConfig.GCPIdentity.MetadataMode)
}

func TestCreateAgent_GCPPassthrough_NonOwnerDenied(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create the broker owner
	owner := &store.User{
		ID:          "user-broker-owner-2",
		Email:       "owner2@test.com",
		DisplayName: "Broker Owner 2",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, owner))

	// Create a non-owner user
	nonOwner := &store.User{
		ID:          "user-non-owner",
		Email:       "nonowner@test.com",
		DisplayName: "Non Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, nonOwner))
	ensureHubMembership(ctx, s, nonOwner.ID)

	// Create a project where the non-owner is a member
	project := &store.Project{
		ID:        "project-pt-nonowner",
		Name:      "Passthrough NonOwner Project",
		Slug:      "passthrough-nonowner-project",
		OwnerID:   nonOwner.ID,
		CreatedBy: nonOwner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// Create a broker owned by a DIFFERENT user
	broker := &store.RuntimeBroker{
		ID:          "broker-pt-nonowner",
		Name:        "Other Broker",
		Slug:        "other-broker",
		Status:      store.BrokerStatusOnline,
		CreatedBy:   owner.ID,
		AutoProvide: true, // AutoProvide so dispatch is allowed for any user
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	// Non-owner should be DENIED passthrough
	rec := doRequestAsUser(t, srv, nonOwner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "pt-denied-agent",
		ProjectID: project.ID,
		Task:    "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})
	require.Equal(t, http.StatusForbidden, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Contains(t, errResp.Error.Message, "broker ownership")
}

func TestCreateAgent_GCPPassthrough_AdminAllowed(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	brokerOwner := &store.User{
		ID:          "user-broker-owner-3",
		Email:       "owner3@test.com",
		DisplayName: "Broker Owner 3",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, brokerOwner))

	adminUser := &store.User{
		ID:          "user-admin-pt",
		Email:       "admin@test.com",
		DisplayName: "Admin User",
		Role:        store.UserRoleAdmin,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, adminUser))
	ensureHubMembership(ctx, s, adminUser.ID)

	project := &store.Project{
		ID:        "project-pt-admin",
		Name:      "Passthrough Admin Project",
		Slug:      "passthrough-admin-project",
		OwnerID:   adminUser.ID,
		CreatedBy: adminUser.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	srv.createProjectMembersGroupAndPolicy(ctx, project)

	// Broker owned by someone else
	broker := &store.RuntimeBroker{
		ID:        "broker-pt-admin",
		Name:      "Admin Test Broker",
		Slug:      "admin-test-broker",
		Status:    store.BrokerStatusOnline,
		CreatedBy: brokerOwner.ID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))

	// Admin (non-owner) should be allowed passthrough
	rec := doRequestAsUser(t, srv, adminUser, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "pt-admin-agent",
		ProjectID: project.ID,
		Task:    "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code)
}

func TestCreateAgent_GCPIdentityBlockOverridesProjectDefault(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Register and verify a GCP service account
	sa := &store.GCPServiceAccount{
		ID:         "sa-project-default",
		Scope:      store.ScopeProject,
		ScopeID:    project.ID,
		Email:      "project-default@project.iam.gserviceaccount.com",
		ProjectID:  "my-project",
		Verified:   true,
		VerifiedAt: time.Now(),
		CreatedBy:  "user-1",
		CreatedAt:  time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	// Set project defaults to assign the service account
	project.Annotations = map[string]string{
		"scion.io/default-gcp-identity-mode":               "assign",
		"scion.io/default-gcp-identity-service-account-id": sa.ID,
	}
	require.NoError(t, s.UpdateProject(ctx, project))

	// Create agent with explicit "block" — should override project default
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-block-override-agent",
		ProjectID: project.ID,
		Task:    "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "block",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeBlock, resp.Agent.AppliedConfig.GCPIdentity.MetadataMode,
		"explicit block should override project default assign")
	assert.Empty(t, resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountID,
		"no service account should be assigned when block is explicit")
}

func TestCreateAgent_GCPIdentityProjectDefaultApplied(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Register and verify a GCP service account
	sa := &store.GCPServiceAccount{
		ID:         "sa-project-applied",
		Scope:      store.ScopeProject,
		ScopeID:    project.ID,
		Email:      "project-applied@project.iam.gserviceaccount.com",
		ProjectID:  "my-project",
		Verified:   true,
		VerifiedAt: time.Now(),
		CreatedBy:  "user-1",
		CreatedAt:  time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	// Set project defaults to assign the service account
	project.Annotations = map[string]string{
		"scion.io/default-gcp-identity-mode":               "assign",
		"scion.io/default-gcp-identity-service-account-id": sa.ID,
	}
	require.NoError(t, s.UpdateProject(ctx, project))

	// Create agent WITHOUT GCP identity — project default should apply
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-project-default-agent",
		ProjectID: project.ID,
		Task:    "do something",
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent.AppliedConfig.GCPIdentity)
	assert.Equal(t, store.GCPMetadataModeAssign, resp.Agent.AppliedConfig.GCPIdentity.MetadataMode,
		"project default assign should be applied when no GCP identity specified")
	assert.Equal(t, sa.ID, resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountID)
	assert.Equal(t, sa.Email, resp.Agent.AppliedConfig.GCPIdentity.ServiceAccountEmail)
}

func TestPreserveTerminalPhase(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: "project-tp", Name: "TP Project", Slug: "tp-project"}
	require.NoError(t, s.CreateProject(ctx, project))

	t.Run("preserves error phase", func(t *testing.T) {
		agent := &store.Agent{
			ID:      "agent-tp-error",
			Slug:    "agent-tp-error",
			Name:    "TP Error Agent",
			ProjectID: project.ID,
			Phase:   string(state.PhaseCreated),
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		// Simulate sciontool reporting error via UpdateAgentStatus (concurrent update)
		require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
			Phase:   string(state.PhaseError),
			Message: "git clone failed: no GITHUB_TOKEN",
		}))

		// Simulate broker response setting phase to running on the in-memory agent
		agent.Phase = string(state.PhaseRunning)
		agent.Activity = string(state.ActivityIdle)

		// preserveTerminalPhase should detect the DB has error and preserve it
		srv.preserveTerminalPhase(ctx, agent)

		assert.Equal(t, string(state.PhaseError), agent.Phase)
		assert.Equal(t, "git clone failed: no GITHUB_TOKEN", agent.Message)
	})

	t.Run("preserves stopped phase", func(t *testing.T) {
		agent := &store.Agent{
			ID:      "agent-tp-stopped",
			Slug:    "agent-tp-stopped",
			Name:    "TP Stopped Agent",
			ProjectID: project.ID,
			Phase:   string(state.PhaseCreated),
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseStopped),
		}))

		agent.Phase = string(state.PhaseRunning)
		srv.preserveTerminalPhase(ctx, agent)

		assert.Equal(t, string(state.PhaseStopped), agent.Phase)
	})

	t.Run("does not overwrite non-terminal phase", func(t *testing.T) {
		agent := &store.Agent{
			ID:      "agent-tp-running",
			Slug:    "agent-tp-running",
			Name:    "TP Running Agent",
			ProjectID: project.ID,
			Phase:   string(state.PhaseCreated),
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		// DB still has "created" phase — broker says "running", should keep "running"
		agent.Phase = string(state.PhaseRunning)
		srv.preserveTerminalPhase(ctx, agent)

		assert.Equal(t, string(state.PhaseRunning), agent.Phase)
	})
}

// TestListAgents_GlobalEndpointReturnsAllAgents verifies that the global
// /api/v1/agents endpoint returns agents from all projects, consistent with
// the project-scoped endpoint behavior.
func TestListAgents_GlobalEndpointReturnsAllAgents(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create two projects with agents in each
	project1 := &store.Project{ID: "project-global-1", Name: "Project One", Slug: "project-one"}
	project2 := &store.Project{ID: "project-global-2", Name: "Project Two", Slug: "project-two"}
	require.NoError(t, s.CreateProject(ctx, project1))
	require.NoError(t, s.CreateProject(ctx, project2))

	agent1 := &store.Agent{
		ID: "agent-g1", Slug: "agent-g1", Name: "Agent G1",
		ProjectID: project1.ID, Phase: string(state.PhaseRunning),
	}
	agent2 := &store.Agent{
		ID: "agent-g2", Slug: "agent-g2", Name: "Agent G2",
		ProjectID: project2.ID, Phase: string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))
	require.NoError(t, s.CreateAgent(ctx, agent2))

	// Global list should return agents from both projects
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Len(t, resp.Agents, 2, "global list should return agents from all projects")
	assert.Equal(t, 2, resp.TotalCount, "TotalCount should match number of agents")

	// Verify both agents are present
	names := map[string]bool{}
	for _, a := range resp.Agents {
		names[a.Name] = true
	}
	assert.True(t, names["Agent G1"], "should include agent from project 1")
	assert.True(t, names["Agent G2"], "should include agent from project 2")

	// Verify project-scoped lists are consistent
	rec1 := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/agents", project1.ID), nil)
	require.Equal(t, http.StatusOK, rec1.Code)
	var resp1 ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	assert.Len(t, resp1.Agents, 1, "project-scoped list should return only project 1 agents")

	rec2 := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/agents", project2.ID), nil)
	require.Equal(t, http.StatusOK, rec2.Code)
	var resp2 ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Len(t, resp2.Agents, 1, "project-scoped list should return only project 2 agents")
}

func TestHandleAgentExec_DispatchesToRuntimeBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-exec",
		Name: "Exec Project",
		Slug: "exec-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-exec",
		Name:   "Exec Broker",
		Slug:   "exec-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-exec-1",
		Slug:            "agent-exec-1",
		Name:            "Exec Agent",
		ProjectID:         project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	srv.SetDispatcher(&createAgentDispatcher{execOutput: "terminal output"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/exec", map[string]interface{}{
		"command": []string{"echo", "hello"},
		"timeout": 10,
	})
	require.Equal(t, http.StatusOK, rec.Code, "response body: %s", rec.Body.String())

	var resp2 struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exitCode"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp2))
	assert.Equal(t, "terminal output", resp2.Output)
	assert.Equal(t, 0, resp2.ExitCode)
}

func TestHandleProjectAgentExec_DispatchesToRuntimeBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   "project-exec-project-route",
		Name: "Exec Project Route",
		Slug: "exec-project-route",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	broker := &store.RuntimeBroker{
		ID:     "broker-exec-project-route",
		Name:   "Exec Broker Project Route",
		Slug:   "exec-broker-project-route",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:    project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-exec-project-route",
		Slug:            "agent-exec-project-route",
		Name:            "Exec Agent Project Route",
		ProjectID:         project.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	srv.SetDispatcher(&createAgentDispatcher{execOutput: "terminal output"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents/"+agent.Slug+"/exec", map[string]interface{}{
		"command": []string{"echo", "hello"},
		"timeout": 10,
	})
	require.Equal(t, http.StatusOK, rec.Code, "response body: %s", rec.Body.String())

	var resp struct {
		Output   string `json:"output"`
		ExitCode int    `json:"exitCode"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "terminal output", resp.Output)
	assert.Equal(t, 0, resp.ExitCode)
}
