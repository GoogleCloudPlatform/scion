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

	// Create a grove
	grove := &store.Grove{
		ID:   "grove-1",
		Name: "Test Grove",
		Slug: "test-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create two agents
	agent1 := &store.Agent{
		ID:      "agent-1",
		Slug:    "agent-1-slug",
		Name:    "Agent 1",
		GroveID: grove.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))

	agent2 := &store.Agent{
		ID:      "agent-2",
		Slug:    "agent-2-slug",
		Name:    "Agent 2",
		GroveID: grove.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	// Get agent token service
	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	// Generate token for agent 1
	token1, err := tokenSvc.GenerateAgentToken(agent1.ID, grove.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
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

	// Create a grove
	grove := &store.Grove{
		ID:   "grove-h",
		Name: "Heartbeat Grove",
		Slug: "heartbeat-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create an agent
	agent := &store.Agent{
		ID:      "agent-h",
		Slug:    "agent-h-slug",
		Name:    "Agent Heartbeat",
		GroveID: grove.ID,
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

// setupOfflineBrokerAgent creates a grove, an offline broker, and an agent assigned to that broker.
func setupOfflineBrokerAgent(t *testing.T, s store.Store, suffix string) (*store.Grove, *store.RuntimeBroker, *store.Agent) {
	t.Helper()
	ctx := context.Background()

	grove := &store.Grove{
		ID:   fmt.Sprintf("grove-offline-%s", suffix),
		Name: fmt.Sprintf("Offline Grove %s", suffix),
		Slug: fmt.Sprintf("offline-grove-%s", suffix),
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

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
		GroveID:         grove.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return grove, broker, agent
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

	grove := &store.Grove{
		ID:   "grove-nobroker",
		Name: "No Broker Grove",
		Slug: "no-broker-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	agent := &store.Agent{
		ID:      "agent-nobroker",
		Slug:    "agent-nobroker-slug",
		Name:    "Agent No Broker",
		GroveID: grove.ID,
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

// setupOnlineBrokerAgent creates a grove, an online broker, and an agent assigned to that broker.
func setupOnlineBrokerAgent(t *testing.T, s store.Store, suffix string) (*store.Grove, *store.RuntimeBroker, *store.Agent) {
	t.Helper()
	ctx := context.Background()

	grove := &store.Grove{
		ID:   fmt.Sprintf("grove-online-%s", suffix),
		Name: fmt.Sprintf("Online Grove %s", suffix),
		Slug: fmt.Sprintf("online-grove-%s", suffix),
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

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
		GroveID:         grove.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	return grove, broker, agent
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

	// Create a grove
	grove := &store.Grove{
		ID:   "grove-parent",
		Name: "Parent Grove",
		Slug: "parent-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create a runtime broker and provider for the grove
	broker := &store.RuntimeBroker{
		ID:     "broker-parent",
		Name:   "Parent Broker",
		Slug:   "parent-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	contrib := &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddGroveProvider(ctx, contrib))

	// Update grove default broker
	grove.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateGrove(ctx, grove))

	// Create the calling agent
	callingAgent := &store.Agent{
		ID:      "agent-caller",
		Slug:    "agent-caller",
		Name:    "Calling Agent",
		GroveID: grove.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, callingAgent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	t.Run("Agent with grove:agent:create scope can create agent in same grove", func(t *testing.T) {
		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, grove.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
			ScopeAgentCreate,
		}, nil)
		require.NoError(t, err)

		body, _ := json.Marshal(CreateAgentRequest{
			Name:    "Sub Agent",
			GroveID: grove.ID,
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

	t.Run("Agent with grove:agent:create scope rejected for different grove", func(t *testing.T) {
		// Create another grove
		otherGrove := &store.Grove{
			ID:   "grove-other",
			Name: "Other Grove",
			Slug: "other-grove",
		}
		require.NoError(t, s.CreateGrove(ctx, otherGrove))

		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, grove.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
			ScopeAgentCreate,
		}, nil)
		require.NoError(t, err)

		body, _ := json.Marshal(CreateAgentRequest{
			Name:    "Cross Grove Agent",
			GroveID: otherGrove.ID,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", token)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("Agent without grove:agent:create scope is rejected", func(t *testing.T) {
		// Token with only status update scope (no create scope)
		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, grove.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
		}, nil)
		require.NoError(t, err)

		body, _ := json.Marshal(CreateAgentRequest{
			Name:    "Unauthorized Sub",
			GroveID: grove.ID,
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

	// Create a grove
	grove := &store.Grove{
		ID:   "grove-lc",
		Name: "Lifecycle Grove",
		Slug: "lifecycle-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create the calling agent
	callingAgent := &store.Agent{
		ID:      "agent-lc-caller",
		Slug:    "agent-lc-caller",
		Name:    "Lifecycle Caller",
		GroveID: grove.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, callingAgent))

	// Create a target agent in the same grove
	targetAgent := &store.Agent{
		ID:      "agent-lc-target",
		Slug:    "agent-lc-target",
		Name:    "Lifecycle Target",
		GroveID: grove.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, targetAgent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	t.Run("Agent with grove:agent:lifecycle scope can perform lifecycle actions in same grove", func(t *testing.T) {
		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, grove.ID, []AgentTokenScope{
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

	t.Run("Agent with grove:agent:lifecycle scope rejected for cross-grove lifecycle", func(t *testing.T) {
		// Create another grove and agent
		otherGrove := &store.Grove{
			ID:   "grove-lc-other",
			Name: "Other LC Grove",
			Slug: "other-lc-grove",
		}
		require.NoError(t, s.CreateGrove(ctx, otherGrove))

		otherAgent := &store.Agent{
			ID:      "agent-lc-other",
			Slug:    "agent-lc-other",
			Name:    "Other LC Agent",
			GroveID: otherGrove.ID,
			Phase:   string(state.PhaseRunning),
		}
		require.NoError(t, s.CreateAgent(ctx, otherAgent))

		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, grove.ID, []AgentTokenScope{
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
		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, grove.ID, []AgentTokenScope{
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

func TestAgentGetAgent_GroveIsolation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create two groves
	grove1 := &store.Grove{
		ID:   "grove-get1",
		Name: "Get Grove 1",
		Slug: "get-grove-1",
	}
	require.NoError(t, s.CreateGrove(ctx, grove1))

	grove2 := &store.Grove{
		ID:   "grove-get2",
		Name: "Get Grove 2",
		Slug: "get-grove-2",
	}
	require.NoError(t, s.CreateGrove(ctx, grove2))

	// Create agents in each grove
	agent1 := &store.Agent{
		ID:      "agent-get-caller",
		Slug:    "agent-get-caller",
		Name:    "Get Caller",
		GroveID: grove1.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))

	agent2SameGrove := &store.Agent{
		ID:      "agent-get-same",
		Slug:    "agent-get-same",
		Name:    "Same Grove Agent",
		GroveID: grove1.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2SameGrove))

	agentOtherGrove := &store.Agent{
		ID:      "agent-get-other",
		Slug:    "agent-get-other",
		Name:    "Other Grove Agent",
		GroveID: grove2.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agentOtherGrove))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	token, err := tokenSvc.GenerateAgentToken(agent1.ID, grove1.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

	t.Run("Agent can GET details of agents in same grove", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent2SameGrove.ID, nil)
		req.Header.Set("X-Scion-Agent-Token", token)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("Agent cannot GET details of agents in different grove", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agentOtherGrove.ID, nil)
		req.Header.Set("X-Scion-Agent-Token", token)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("Agent cannot access workspace operations", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/"+agent2SameGrove.ID+"/workspace", nil)
		req.Header.Set("X-Scion-Agent-Token", token)

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})
}

func TestDeleteGroveAgent_BrokerOffline(t *testing.T) {
	srv, s := testServer(t)

	grove, _, agent := setupOfflineBrokerAgent(t, s, "gdel")

	rec := doRequest(t, srv, http.MethodDelete,
		fmt.Sprintf("/api/v1/groves/%s/agents/%s", grove.ID, agent.ID), nil)
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

// setupCreateAgentServer creates a test server with a dispatcher and a grove+broker ready for agent creation.
func setupCreateAgentServer(t *testing.T, disp AgentDispatcher) (*Server, store.Store, *store.Grove) {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:   "grove-create",
		Name: "Create Test Grove",
		Slug: "create-test-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID:     "broker-create",
		Name:   "Create Test Broker",
		Slug:   "create-test-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	provider := &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddGroveProvider(ctx, provider))

	grove.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateGrove(ctx, grove))

	srv.SetDispatcher(disp)
	return srv, s, grove
}

func TestCreateAgent_BrokerStatusPreserved(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create an agent with a task — should dispatch and preserve broker-reported "running" status
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "status-test",
		GroveID: grove.ID,
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
	srv, _, grove := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "fallback-test",
		GroveID: grove.ID,
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
	srv, baseStore, grove := setupCreateAgentServer(t, disp)
	wrapped := &conflictOnceStore{Store: baseStore, failNextUpdate: true}
	srv.store = wrapped
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "version-conflict-agent",
		GroveID: grove.ID,
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
	srv, baseStore, grove := setupCreateAgentServer(t, disp)
	wrapped := &conflictOnceStore{Store: baseStore, failNextUpdate: true}
	srv.store = wrapped
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "env-gather-conflict-agent",
		GroveID:   grove.ID,
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
	srv, baseStore, grove := setupCreateAgentServer(t, disp)
	wrapped := &conflictOnceStore{Store: baseStore, failNextUpdate: true}
	srv.store = wrapped

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "provision-only-conflict-agent",
		GroveID:       grove.ID,
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
	srv, _, grove := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "no-task-agent",
		GroveID: grove.ID,
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
	srv, _, grove := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "provision-only-agent",
		GroveID:       grove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent stuck in "provisioning" status (simulating Bug 1)
	stuckAgent := &store.Agent{
		ID:              "agent-stuck-prov",
		Slug:            "stuck-agent",
		Name:            "stuck-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseProvisioning),
	}
	require.NoError(t, s.CreateAgent(ctx, stuckAgent))

	// Try to start the same agent name — should succeed by re-starting, not 409
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "stuck-agent",
		GroveID: grove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent in "pending" status
	pendingAgent := &store.Agent{
		ID:              "agent-pending",
		Slug:            "pending-agent",
		Name:            "pending-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, pendingAgent))

	// Try to start the same agent name — should succeed
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "pending-agent",
		GroveID: grove.ID,
		Task:    "retry task",
	})

	assert.Equal(t, http.StatusOK, rec.Code,
		"re-starting an agent in pending status should succeed")
}

func TestCreateAgent_RecreateFromRunningStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent in "running" status (stale — container may have died)
	runningAgent := &store.Agent{
		ID:              "agent-running-stale",
		Slug:            "running-agent",
		Name:            "running-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, runningAgent))

	// Start with the same name — should delete old agent and create new one
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "running-agent",
		GroveID: grove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent in "error" status
	errorAgent := &store.Agent{
		ID:              "agent-errored",
		Slug:            "error-agent",
		Name:            "error-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseError),
	}
	require.NoError(t, s.CreateAgent(ctx, errorAgent))

	// Start with the same name — should delete and recreate
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "error-agent",
		GroveID: grove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent in "stopped" status
	stoppedAgent := &store.Agent{
		ID:              "agent-stopped",
		Slug:            "stopped-agent",
		Name:            "stopped-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, stoppedAgent))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "stopped-agent",
		GroveID: grove.ID,
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

	// Create a grove with default runtime broker
	grove := &store.Grove{
		ID:                     "grove_local_tpl",
		Slug:                   "local-tpl-grove",
		Name:                   "Local Template Grove",
		GitRemote:              "github.com/test/local-tpl",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Register the broker as a provider WITH a local path
	provider := &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  "/home/user/project/.scion",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddGroveProvider(ctx, provider))

	// Create agent with a template name that does NOT exist on the Hub.
	// Because the broker has a LocalPath, this should succeed.
	body := map[string]interface{}{
		"name":     "Local Template Agent",
		"groveId":  grove.ID,
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

	// Create a grove
	grove := &store.Grove{
		ID:                     "grove_remote_tpl",
		Slug:                   "remote-tpl-grove",
		Name:                   "Remote Template Grove",
		GitRemote:              "github.com/test/remote-tpl",
		DefaultRuntimeBrokerID: broker.ID,
		Created:                time.Now(),
		Updated:                time.Now(),
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Register the broker as a provider WITHOUT a local path
	provider := &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
		// Note: LocalPath is NOT set — broker has no local access
	}
	require.NoError(t, s.AddGroveProvider(ctx, provider))

	// Create agent with a template name that does NOT exist on the Hub.
	// Without local access, this should fail with NotFound.
	body := map[string]interface{}{
		"name":     "Remote Template Agent",
		"groveId":  grove.ID,
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

	// Create a grove WITHOUT a default runtime broker
	grove := &store.Grove{
		ID:        "grove_no_broker_tpl",
		Slug:      "no-broker-tpl-grove",
		Name:      "No Broker Template Grove",
		GitRemote: "github.com/test/no-broker-tpl",
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create agent with a template name that does NOT exist on the Hub.
	// Without any broker, this should fail (422 validation error for missing broker).
	body := map[string]interface{}{
		"name":     "No Broker Agent",
		"groveId":  grove.ID,
		"template": "nonexistent-template",
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)

	// Expect a client error — the broker resolution fails before template resolution
	assert.True(t, rec.Code >= 400 && rec.Code < 500, "expected client error when no broker assigned, got %d", rec.Code)
}

func TestCreateAgent_CreatorName_UserEmail(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Use dev auth token (which creates a DevUser with email "dev@localhost")
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "user-created-agent",
		GroveID: grove.ID,
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

	// Create a grove and agent
	grove := &store.Grove{
		ID:   "grove-servertime",
		Name: "ServerTime Grove",
		Slug: "servertime-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	agent := &store.Agent{
		ID:      "agent-servertime",
		Slug:    "agent-servertime-slug",
		Name:    "ServerTime Agent",
		GroveID: grove.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	before := time.Now().UTC()

	// List agents
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents?groveId="+grove.ID, nil)
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

func TestListGroveAgents_ServerTimeIncluded(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create a grove
	grove := &store.Grove{
		ID:   "grove-servertime-g",
		Name: "ServerTime Grove G",
		Slug: "servertime-grove-g",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	before := time.Now().UTC()

	// List grove agents
	rec := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/groves/%s/agents", grove.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	after := time.Now().UTC()

	var resp ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.False(t, resp.ServerTime.IsZero(), "ServerTime should be non-zero in grove-scoped list")
	assert.True(t, !resp.ServerTime.Before(before.Add(-time.Second)),
		"ServerTime should not be before request start")
	assert.True(t, !resp.ServerTime.After(after.Add(time.Second)),
		"ServerTime should not be after request end")
}

// TestCreateGroveAgent_BrokerStatusPreserved tests that the grove-scoped agent creation
// endpoint (/api/v1/groves/{groveId}/agents) preserves the status set by the broker's
// response rather than unconditionally overwriting it with "provisioning".
func TestCreateGroveAgent_BrokerStatusPreserved(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create agent via the grove-scoped endpoint (this is the path the CLI uses)
	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/groves/%s/agents", grove.ID),
		CreateAgentRequest{
			Name: "grove-status-test",
			Task: "do something",
		})

	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)

	// The response should reflect the broker-reported status, not hardcoded "provisioning"
	assert.Equal(t, string(state.PhaseRunning), resp.Agent.Phase,
		"grove-scoped agent status should reflect broker response, not hardcoded provisioning")

	// Verify persisted status in store
	persisted, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), persisted.Phase,
		"persisted agent status should match broker response")
}

// TestCreateGroveAgent_FallbackToProvisioningWhenNoBrokerStatus tests that the grove-scoped
// endpoint falls back to "provisioning" when the broker doesn't report a status.
func TestCreateGroveAgent_FallbackToProvisioningWhenNoBrokerStatus(t *testing.T) {
	// Dispatcher that doesn't set a status (leaves it as "pending")
	disp := &createAgentDispatcher{createPhase: ""}
	srv, _, grove := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/groves/%s/agents", grove.ID),
		CreateAgentRequest{
			Name: "grove-fallback-test",
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

func TestCreateAgent_GitAnchoredGrovePopulatesGitClone(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create a grove with GitRemote and labels
	gitGrove := &store.Grove{
		ID:        "grove-git",
		Name:      "Git Grove",
		Slug:      "git-grove",
		GitRemote: "github.com/example/myrepo",
		Labels: map[string]string{
			"scion.dev/clone-url":      "https://github.com/example/myrepo.git",
			"scion.dev/default-branch": "develop",
		},
		DefaultRuntimeBrokerID: "broker-create",
	}
	require.NoError(t, s.CreateGrove(ctx, gitGrove))

	// Add grove provider
	provider := &store.GroveProvider{
		GroveID:    gitGrove.ID,
		BrokerID:   "broker-create",
		BrokerName: "Create Test Broker",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddGroveProvider(ctx, provider))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "git-agent",
		GroveID: gitGrove.ID,
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
	require.NotNil(t, persisted.AppliedConfig.GitClone, "GitClone should be populated for git-anchored grove")
	assert.Equal(t, "https://github.com/example/myrepo.git", persisted.AppliedConfig.GitClone.URL)
	assert.Equal(t, "develop", persisted.AppliedConfig.GitClone.Branch)
	assert.Equal(t, 1, persisted.AppliedConfig.GitClone.Depth)
}

func TestCreateAgent_NonGitGroveNoGitClone(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "non-git-agent",
		GroveID: grove.ID,
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
		"GitClone should be nil for non-git-anchored grove")
}

func TestCreateGroveAgent_GitAnchoredGrovePopulatesGitClone(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create a grove with GitRemote and labels
	gitGrove := &store.Grove{
		ID:        "grove-git-scoped",
		Name:      "Git Grove Scoped",
		Slug:      "git-grove-scoped",
		GitRemote: "github.com/example/myrepo",
		Labels: map[string]string{
			"scion.dev/clone-url":      "https://github.com/example/myrepo.git",
			"scion.dev/default-branch": "develop",
		},
		DefaultRuntimeBrokerID: "broker-create",
	}
	require.NoError(t, s.CreateGrove(ctx, gitGrove))

	// Add grove provider
	provider := &store.GroveProvider{
		GroveID:    gitGrove.ID,
		BrokerID:   "broker-create",
		BrokerName: "Create Test Broker",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddGroveProvider(ctx, provider))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/groves/%s/agents", gitGrove.ID),
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
	require.NotNil(t, persisted.AppliedConfig.GitClone, "GitClone should be populated for git-anchored grove")
	assert.Equal(t, "https://github.com/example/myrepo.git", persisted.AppliedConfig.GitClone.URL)
	assert.Equal(t, "develop", persisted.AppliedConfig.GitClone.Branch)
	assert.Equal(t, 1, persisted.AppliedConfig.GitClone.Depth)
}

func TestCreateGroveAgent_NonGitGroveNoGitClone(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/groves/%s/agents", grove.ID),
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
		"GitClone should be nil for non-git-anchored grove")
}

func TestCreateAgent_GitGroveCloneURLFallback(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create a grove with GitRemote but WITHOUT the scion.dev/clone-url label.
	// The URL should be constructed from gitRemote as "https://<gitRemote>.git".
	gitGrove := &store.Grove{
		ID:        "grove-git-fallback-url",
		Name:      "Git Grove Fallback URL",
		Slug:      "git-grove-fallback-url",
		GitRemote: "github.com/example/fallback-repo",
		Labels: map[string]string{
			"scion.dev/default-branch": "develop",
		},
		DefaultRuntimeBrokerID: "broker-create",
	}
	require.NoError(t, s.CreateGrove(ctx, gitGrove))

	provider := &store.GroveProvider{
		GroveID:    gitGrove.ID,
		BrokerID:   "broker-create",
		BrokerName: "Create Test Broker",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddGroveProvider(ctx, provider))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "fallback-url-agent",
		GroveID: gitGrove.ID,
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

func TestCreateAgent_GitGroveSchemelessCloneURL(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create a grove where clone-url label is set but missing https:// scheme
	// (as can happen when the web UI stores raw user input).
	gitGrove := &store.Grove{
		ID:        "grove-git-schemeless",
		Name:      "Git Grove Schemeless",
		Slug:      "git-grove-schemeless",
		GitRemote: "github.com/example/schemeless-repo",
		Labels: map[string]string{
			"scion.dev/clone-url":      "github.com/example/schemeless-repo",
			"scion.dev/default-branch": "main",
		},
		DefaultRuntimeBrokerID: "broker-create",
	}
	require.NoError(t, s.CreateGrove(ctx, gitGrove))

	provider := &store.GroveProvider{
		GroveID:    gitGrove.ID,
		BrokerID:   "broker-create",
		BrokerName: "Create Test Broker",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddGroveProvider(ctx, provider))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "schemeless-url-agent",
		GroveID: gitGrove.ID,
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

func TestCreateAgent_GitGroveDefaultBranchFallback(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, _ := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create a grove with GitRemote and clone-url label but WITHOUT default-branch.
	// The branch should default to "main".
	gitGrove := &store.Grove{
		ID:        "grove-git-fallback-branch",
		Name:      "Git Grove Fallback Branch",
		Slug:      "git-grove-fallback-branch",
		GitRemote: "github.com/example/branch-repo",
		Labels: map[string]string{
			"scion.dev/clone-url": "https://github.com/example/branch-repo.git",
		},
		DefaultRuntimeBrokerID: "broker-create",
	}
	require.NoError(t, s.CreateGrove(ctx, gitGrove))

	provider := &store.GroveProvider{
		GroveID:    gitGrove.ID,
		BrokerID:   "broker-create",
		BrokerName: "Create Test Broker",
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddGroveProvider(ctx, provider))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "fallback-branch-agent",
		GroveID: gitGrove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "profiled-agent",
		GroveID: grove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "profiled-agent-with-config",
		GroveID: grove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create an agent with a full ScionConfig including fields beyond the old AgentConfigOverride
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "inline-config-agent",
		GroveID: grove.ID,
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
	srv, _, grove := setupCreateAgentServer(t, disp)

	// When both req.Task and Config.Task are set, req.Task takes precedence
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "task-merge-agent",
		GroveID: grove.ID,
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
	srv, _, grove := setupCreateAgentServer(t, disp)

	// When only Config.Task is set (no req.Task), it should be used
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "task-config-only-agent",
		GroveID: grove.ID,
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

	grove := &store.Grove{
		ID:   "grove-harness-enrich",
		Name: "Harness Enrichment Grove",
		Slug: "harness-enrich-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	agent := &store.Agent{
		ID:      "agent-harness-enrich",
		Slug:    "agent-harness-enrich",
		Name:    "Harness Agent",
		GroveID: grove.ID,
		Phase:   string(state.PhaseRunning),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "gemini",
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// List via global endpoint
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents?groveId="+grove.ID, nil)
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

	// List via grove-scoped endpoint
	rec2 := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/groves/%s/agents", grove.ID), nil)
	require.Equal(t, http.StatusOK, rec2.Code)

	var resp2 ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	require.Len(t, resp2.Agents, 1)
	assert.Equal(t, "gemini", resp2.Agents[0].HarnessConfig,
		"grove-scoped harnessConfig should also be enriched")
}

// TestGetAgent_HarnessConfigEnriched verifies that a single agent GET also
// includes the enriched harnessConfig field.
func TestGetAgent_HarnessConfigEnriched(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:   "grove-harness-get",
		Name: "Harness Get Grove",
		Slug: "harness-get-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	agent := &store.Agent{
		ID:      "agent-harness-get",
		Slug:    "agent-harness-get",
		Name:    "Harness Get Agent",
		GroveID: grove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create agent with no template but explicit harness (sync scenario)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "sync-agent",
		GroveID:       grove.ID,
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
	rec2 := doRequest(t, srv, http.MethodGet, "/api/v1/agents?groveId="+grove.ID, nil)
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create agent with explicit profile
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "profile-get-agent",
		GroveID: grove.ID,
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

	grove := &store.Grove{
		ID:   "grove-profile-hb",
		Name: "Profile HB Grove",
		Slug: "profile-hb-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

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
		GroveID:         grove.ID,
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
		Groves: []brokerGroveHeartbeat{{
			GroveID:    grove.ID,
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

// TestCreateAgent_HarnessFieldIgnoredWhenTemplateResolved verifies that
// when a template resolves successfully, its harness takes precedence
// over the explicit Harness field.
func TestCreateAgent_HarnessFieldIgnoredWhenTemplateResolved(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Create a template with harness
	tmpl := &store.Template{
		ID:      "tmpl-with-harness",
		Name:    "claude-template",
		Slug:    "claude-template",
		Harness: "claude",
		GroveID: grove.ID,
		Scope:   "grove",
		ScopeID: grove.ID,
		Status:  "active",
	}
	require.NoError(t, s.CreateTemplate(ctx, tmpl))

	// Create agent with template that resolves AND explicit harness
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "tmpl-agent",
		GroveID:       grove.ID,
		Template:      "claude-template",
		HarnessConfig: "gemini", // Should be ignored since template resolves
	})
	require.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	agent, err := s.GetAgent(ctx, resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, agent.AppliedConfig)
	assert.Equal(t, "claude", agent.AppliedConfig.HarnessConfig,
		"template-resolved harness should take precedence over request HarnessConfig field")
}

// TestCreateAgent_HarnessNotTemplateUUID verifies that when the template is
// specified as a UUID that doesn't resolve on the hub (e.g., broker has it
// locally), the harness is taken from the explicit Harness field, not from
// the UUID string in Template.
func TestCreateAgent_HarnessNotTemplateUUID(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Update the existing provider to have a LocalPath so the hub allows
	// the template to be resolved locally by the broker.
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   "broker-create",
		BrokerName: "Create Test Broker",
		LocalPath:  "/some/local/path",
		Status:     "online",
	}))

	// Create agent with template UUID that doesn't exist on hub + explicit harness
	templateUUID := "003879ad-f000-426d-b52f-08f537c4c6ce"
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:          "uuid-tmpl-agent",
		GroveID:       grove.ID,
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
// Grove-scoped existing-agent tests (mirror createAgent tests)
// ---------------------------------------------------------------------------

func TestCreateGroveAgent_RecreateFromRunningStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	runningAgent := &store.Agent{
		ID:              "grove-agent-running",
		Slug:            "running-grove-agent",
		Name:            "running-grove-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, runningAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/groves/%s/agents", grove.ID),
		CreateAgentRequest{
			Name: "running-grove-agent",
			Task: "new task",
		})

	require.Equal(t, http.StatusCreated, rec.Code,
		"re-creating a running grove agent should succeed with 201")

	_, err := s.GetAgent(ctx, "grove-agent-running")
	assert.ErrorIs(t, err, store.ErrNotFound, "old running agent should be deleted")

	assert.True(t, disp.deleteCalled, "dispatcher should have been asked to delete old agent")

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.NotEqual(t, "grove-agent-running", resp.Agent.ID)
	assert.Equal(t, string(state.PhaseRunning), resp.Agent.Phase)
}

func TestCreateGroveAgent_RecreateFromStoppedStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	stoppedAgent := &store.Agent{
		ID:              "grove-agent-stopped",
		Slug:            "stopped-grove-agent",
		Name:            "stopped-grove-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, stoppedAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/groves/%s/agents", grove.ID),
		CreateAgentRequest{
			Name: "stopped-grove-agent",
			Task: "restart after stop",
		})

	require.Equal(t, http.StatusCreated, rec.Code,
		"re-creating a stopped grove agent should succeed with 201")

	_, err := s.GetAgent(ctx, "grove-agent-stopped")
	assert.ErrorIs(t, err, store.ErrNotFound, "old stopped agent should be deleted")
}

func TestCreateGroveAgent_RecreateFromErrorStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	errorAgent := &store.Agent{
		ID:              "grove-agent-errored",
		Slug:            "errored-grove-agent",
		Name:            "errored-grove-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseError),
	}
	require.NoError(t, s.CreateAgent(ctx, errorAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/groves/%s/agents", grove.ID),
		CreateAgentRequest{
			Name: "errored-grove-agent",
			Task: "retry after error",
		})

	require.Equal(t, http.StatusCreated, rec.Code,
		"re-creating an errored grove agent should succeed with 201")

	_, err := s.GetAgent(ctx, "grove-agent-errored")
	assert.ErrorIs(t, err, store.ErrNotFound, "old errored agent should be deleted")
}

func TestCreateGroveAgent_RestartFromProvisioningStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	provAgent := &store.Agent{
		ID:              "grove-agent-prov",
		Slug:            "prov-grove-agent",
		Name:            "prov-grove-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseProvisioning),
	}
	require.NoError(t, s.CreateAgent(ctx, provAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/groves/%s/agents", grove.ID),
		CreateAgentRequest{
			Name: "prov-grove-agent",
			Task: "retry task",
		})

	assert.Equal(t, http.StatusOK, rec.Code,
		"re-starting a provisioning grove agent should succeed (200)")

	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.Equal(t, string(state.PhaseRunning), resp.Agent.Phase)
}

func TestCreateGroveAgent_RestartFromPendingStatus(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	pendingAgent := &store.Agent{
		ID:              "grove-agent-pending",
		Slug:            "pending-grove-agent",
		Name:            "pending-grove-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, pendingAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/groves/%s/agents", grove.ID),
		CreateAgentRequest{
			Name: "pending-grove-agent",
			Task: "retry task",
		})

	assert.Equal(t, http.StatusOK, rec.Code,
		"re-starting a pending grove agent should succeed (200)")
}

// ---------------------------------------------------------------------------
// Config update and broker-ID recovery tests
// ---------------------------------------------------------------------------

func TestCreateGroveAgent_ConfigUpdateOnRestart(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	existingAgent := &store.Agent{
		ID:              "grove-agent-config",
		Slug:            "config-grove-agent",
		Name:            "config-grove-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseCreated),
		AppliedConfig: &store.AgentAppliedConfig{
			Task:   "old task",
			Attach: false,
		},
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/groves/%s/agents", grove.ID),
		CreateAgentRequest{
			Name:   "config-grove-agent",
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

func TestCreateGroveAgent_BrokerIDRecovery(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create agent with empty RuntimeBrokerID (simulates agent created
	// before a broker was registered).
	existingAgent := &store.Agent{
		ID:              "grove-agent-no-broker",
		Slug:            "no-broker-grove-agent",
		Name:            "no-broker-grove-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "", // empty — should be recovered
		Phase:           string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	rec := doRequest(t, srv, http.MethodPost,
		fmt.Sprintf("/api/v1/groves/%s/agents", grove.ID),
		CreateAgentRequest{
			Name: "no-broker-grove-agent",
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	existingAgent := &store.Agent{
		ID:              "agent-no-broker",
		Slug:            "no-broker-agent",
		Name:            "no-broker-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "", // empty — should be recovered
		Phase:           string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "no-broker-agent",
		GroveID: grove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	existingAgent := &store.Agent{
		ID:              "agent-stale-strict",
		Slug:            "stale-strict-agent",
		Name:            "stale-strict-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:        "stale-strict-agent",
		GroveID:     grove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	existingAgent := &store.Agent{
		ID:              "agent-stale-force",
		Slug:            "stale-force-agent",
		Name:            "stale-force-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:        "stale-force-agent",
		GroveID:     grove.ID,
		CleanupMode: "force",
	})
	require.Equal(t, http.StatusCreated, rec.Code)
	assert.True(t, disp.deleteCalled, "expected broker delete attempt in force mode")

	_, err := s.GetAgent(ctx, existingAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "force mode should replace stale DB record")
}

func TestCreateAgent_InvalidCleanupMode(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, grove := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:        "invalid-cleanup-agent",
		GroveID:     grove.ID,
		CleanupMode: "sometimes",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Phase 3: Notification Subscription on Agent Create ---

func TestCreateAgent_NotifyCreatesSubscription(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create grove and broker infrastructure
	grove := &store.Grove{
		ID:   "grove-notify",
		Name: "Notify Grove",
		Slug: "notify-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID:     "broker-notify",
		Name:   "Notify Broker",
		Slug:   "notify-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	grove.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateGrove(ctx, grove))

	// Create the calling agent (the one that will subscribe to notifications)
	callingAgent := &store.Agent{
		ID:      "agent-lead",
		Slug:    "lead-agent",
		Name:    "Lead Agent",
		GroveID: grove.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, callingAgent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	t.Run("Notify=true creates subscription for agent caller", func(t *testing.T) {
		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, grove.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
			ScopeAgentCreate,
			ScopeAgentNotify,
		}, nil)
		require.NoError(t, err)

		body, _ := json.Marshal(CreateAgentRequest{
			Name:    "Sub Worker",
			GroveID: grove.ID,
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
		assert.Equal(t, grove.ID, sub.GroveID)
		assert.Equal(t, callingAgent.ID, sub.CreatedBy)
		assert.Contains(t, sub.TriggerActivities, "COMPLETED")
		assert.Contains(t, sub.TriggerActivities, "WAITING_FOR_INPUT")
		assert.Contains(t, sub.TriggerActivities, "LIMITS_EXCEEDED")
		assert.Contains(t, sub.TriggerActivities, "STALLED")
		assert.Contains(t, sub.TriggerActivities, "ERROR")
	})

	t.Run("Notify=false does not create subscription", func(t *testing.T) {
		token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, grove.ID, []AgentTokenScope{
			ScopeAgentStatusUpdate,
			ScopeAgentCreate,
		}, nil)
		require.NoError(t, err)

		body, _ := json.Marshal(CreateAgentRequest{
			Name:    "Sub Worker No Notify",
			GroveID: grove.ID,
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
			GroveID: grove.ID,
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
		assert.Equal(t, grove.ID, sub.GroveID)
	})
}

func TestCreateAgent_NotifySubscriptionCascadeOnDelete(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:   "grove-cascade",
		Name: "Cascade Grove",
		Slug: "cascade-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID:     "broker-cascade",
		Name:   "Cascade Broker",
		Slug:   "cascade-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	grove.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateGrove(ctx, grove))

	callingAgent := &store.Agent{
		ID:      "agent-cascade-lead",
		Slug:    "cascade-lead",
		Name:    "Cascade Lead",
		GroveID: grove.ID,
		Phase:   string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, callingAgent))

	tokenSvc := srv.GetAgentTokenService()
	token, err := tokenSvc.GenerateAgentToken(callingAgent.ID, grove.ID, []AgentTokenScope{
		ScopeAgentStatusUpdate,
		ScopeAgentCreate,
		ScopeAgentNotify,
	}, nil)
	require.NoError(t, err)

	// Create agent with notify
	body, _ := json.Marshal(CreateAgentRequest{
		Name:    "Cascade Sub",
		GroveID: grove.ID,
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

	// Create grove, broker, and agent
	grove := &store.Grove{ID: "grove-hb-sse", Name: "HB SSE Grove", Slug: "hb-sse-grove"}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID: "broker-hb-sse", Name: "HB SSE Broker", Slug: "hb-sse-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-hb-sse", Slug: "agent-hb-slug", Name: "HB SSE Agent",
		GroveID: grove.ID, RuntimeBrokerID: broker.ID,
		Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Subscribe to agent-specific status events
	ch, unsub := pub.Subscribe("agent." + agent.ID + ".status")
	defer unsub()

	// Send broker heartbeat with an activity change
	heartbeat := brokerHeartbeatRequest{
		Status: "online",
		Groves: []brokerGroveHeartbeat{{
			GroveID:    grove.ID,
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

	// Create grove, broker, and agent
	grove := &store.Grove{ID: "grove-stall-hb", Name: "Stall HB Grove", Slug: "stall-hb-grove"}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID: "broker-stall-hb", Name: "Stall HB Broker", Slug: "stall-hb-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-stall-hb", Slug: "stall-hb-slug", Name: "Stall HB Agent",
		GroveID: grove.ID, RuntimeBrokerID: broker.ID,
		Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// First heartbeat: set activity to "thinking" — this should set last_activity_event
	hb1 := brokerHeartbeatRequest{
		Status: "online",
		Groves: []brokerGroveHeartbeat{{
			GroveID:    grove.ID,
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
		Groves: []brokerGroveHeartbeat{{
			GroveID:    grove.ID,
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
		Groves: []brokerGroveHeartbeat{{
			GroveID:    grove.ID,
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

	// Create grove, broker, and agent
	grove := &store.Grove{ID: "grove-stall-keep", Name: "Stall Keep Grove", Slug: "stall-keep-grove"}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID: "broker-stall-keep", Name: "Stall Keep Broker", Slug: "stall-keep-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-stall-keep", Slug: "stall-keep-slug", Name: "Stall Keep Agent",
		GroveID: grove.ID, RuntimeBrokerID: broker.ID,
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
		Groves: []brokerGroveHeartbeat{{
			GroveID:    grove.ID,
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

	// Create grove, broker, and agent
	grove := &store.Grove{ID: "grove-stall-recover", Name: "Stall Recover Grove", Slug: "stall-recover-grove"}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID: "broker-stall-recover", Name: "Stall Recover Broker", Slug: "stall-recover-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-stall-recover", Slug: "stall-recover-slug", Name: "Stall Recover Agent",
		GroveID: grove.ID, RuntimeBrokerID: broker.ID,
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
		Groves: []brokerGroveHeartbeat{{
			GroveID:    grove.ID,
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

	grove := &store.Grove{ID: "grove-stall-idle", Name: "Stall Idle Grove", Slug: "stall-idle-grove"}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID: "broker-stall-idle", Name: "Stall Idle Broker", Slug: "stall-idle-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-stall-idle", Slug: "stall-idle-slug", Name: "Stall Idle Agent",
		GroveID: grove.ID, RuntimeBrokerID: broker.ID,
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
		Groves: []brokerGroveHeartbeat{{
			GroveID:    grove.ID,
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

	grove := &store.Grove{ID: "grove-stop-revert", Name: "Stop Revert Grove", Slug: "stop-revert-grove"}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID: "broker-stop-revert", Name: "Stop Revert Broker", Slug: "stop-revert-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-stop-revert", Slug: "stop-revert-slug", Name: "Stop Revert Agent",
		GroveID: grove.ID, RuntimeBrokerID: broker.ID,
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
		Groves: []brokerGroveHeartbeat{{
			GroveID:    grove.ID,
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

	grove := &store.Grove{ID: "grove-stop-legacy", Name: "Stop Legacy Grove", Slug: "stop-legacy-grove"}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID: "broker-stop-legacy", Name: "Stop Legacy Broker", Slug: "stop-legacy-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	agent := &store.Agent{
		ID: "agent-stop-legacy", Slug: "stop-legacy-slug", Name: "Stop Legacy Agent",
		GroveID: grove.ID, RuntimeBrokerID: broker.ID,
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
		Groves: []brokerGroveHeartbeat{{
			GroveID:    grove.ID,
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

func TestCreateAgent_RestartCreatesNotificationSubscription(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent in "created" phase (provisioned but not started)
	existingAgent := &store.Agent{
		ID:              "agent-notify-restart",
		Slug:            "notify-agent",
		Name:            "notify-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	// Restart the agent with Notify: true — should create a notification subscription
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "notify-agent",
		GroveID: grove.ID,
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
	assert.Equal(t, grove.ID, subs[0].GroveID)
}

func TestCreateAgent_RestartNoSubscriptionWithoutNotify(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Pre-create an agent in "created" phase
	existingAgent := &store.Agent{
		ID:              "agent-no-notify",
		Slug:            "no-notify-agent",
		Name:            "no-notify-agent",
		GroveID:         grove.ID,
		RuntimeBrokerID: "broker-create",
		Phase:           string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, existingAgent))

	// Restart the agent without Notify — should NOT create a subscription
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "no-notify-agent",
		GroveID: grove.ID,
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

	grove := &store.Grove{
		ID:   "grove-msg",
		Name: "Msg Test Grove",
		Slug: "msg-test-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID:     "broker-msg",
		Name:   "Msg Test Broker",
		Slug:   "msg-test-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-msg-1",
		Slug:            "agent-msg-1",
		Name:            "Msg Agent",
		GroveID:         grove.ID,
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

	grove := &store.Grove{
		ID:   "grove-msg-sender",
		Name: "Msg Sender Grove",
		Slug: "msg-sender-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID:     "broker-msg-sender",
		Name:   "Msg Sender Broker",
		Slug:   "msg-sender-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-msg-sender-1",
		Slug:            "agent-msg-sender-1",
		Name:            "Msg Sender Agent",
		GroveID:         grove.ID,
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

	grove := &store.Grove{
		ID:   "grove-msg-notify",
		Name: "Msg Notify Grove",
		Slug: "msg-notify-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID:     "broker-msg-notify",
		Name:   "Msg Notify Broker",
		Slug:   "msg-notify-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-msg-notify-1",
		Slug:            "agent-msg-notify-1",
		Name:            "Msg Notify Agent",
		GroveID:         grove.ID,
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
	assert.Equal(t, agent.GroveID, subs[0].GroveID)
}

// TestHandleAgentMessage_NoNotifyNoSubscription verifies that sending a message
// without notify=true does NOT create a notification subscription.
func TestHandleAgentMessage_NoNotifyNoSubscription(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:   "grove-msg-no-notify",
		Name: "Msg No Notify Grove",
		Slug: "msg-no-notify-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID:     "broker-msg-no-notify",
		Name:   "Msg No Notify Broker",
		Slug:   "msg-no-notify-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-msg-no-notify-1",
		Slug:            "agent-msg-no-notify-1",
		Name:            "Msg No Notify Agent",
		GroveID:         grove.ID,
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

	grove := &store.Grove{
		ID:   "grove-msg-503",
		Name: "Msg 503 Grove",
		Slug: "msg-503-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	agent := &store.Agent{
		ID:      "agent-msg-503",
		Slug:    "agent-msg-503",
		Name:    "Msg 503 Agent",
		GroveID: grove.ID,
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

	grove := &store.Grove{
		ID:   "grove-msg-503-nobroker",
		Name: "Msg 503 NoBroker Grove",
		Slug: "msg-503-nobroker-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	agent := &store.Agent{
		ID:      "agent-msg-503-nobroker",
		Slug:    "agent-msg-503-nobroker",
		Name:    "Msg 503 NoBroker Agent",
		GroveID: grove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "auth-fail-agent",
		GroveID: grove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	// Register and verify a GCP service account
	sa := &store.GCPServiceAccount{
		ID:         "sa-assign-1",
		Scope:      store.ScopeGrove,
		ScopeID:    grove.ID,
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
		GroveID: grove.ID,
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
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-block-agent",
		GroveID: grove.ID,
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
	srv, _, grove := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-passthrough-agent",
		GroveID: grove.ID,
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
	srv, _, grove := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-none-agent",
		GroveID: grove.ID,
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
	srv, _, grove := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-invalid-mode",
		GroveID: grove.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "invalid",
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPIdentityAssignMissingSA(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, grove := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-missing-sa",
		GroveID: grove.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "assign",
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPIdentityAssignNonexistentSA(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, grove := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-nonexistent-sa",
		GroveID: grove.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     "assign",
			ServiceAccountID: "nonexistent-sa-id",
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPIdentityAssignUnverifiedSA(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	sa := &store.GCPServiceAccount{
		ID:        "sa-unverified-1",
		Scope:     store.ScopeGrove,
		ScopeID:   grove.ID,
		Email:     "unverified@project.iam.gserviceaccount.com",
		ProjectID: "my-project",
		Verified:  false,
		CreatedBy: "user-1",
		CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-unverified-sa",
		GroveID: grove.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     "assign",
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPIdentityAssignWrongGrove(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, grove := setupCreateAgentServer(t, disp)
	ctx := context.Background()

	sa := &store.GCPServiceAccount{
		ID:         "sa-other-grove-1",
		Scope:      store.ScopeGrove,
		ScopeID:    "other-grove-id",
		Email:      "other@project.iam.gserviceaccount.com",
		ProjectID:  "my-project",
		Verified:   true,
		VerifiedAt: time.Now(),
		CreatedBy:  "user-1",
		CreatedAt:  time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(ctx, sa))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-wrong-grove",
		GroveID: grove.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     "assign",
			ServiceAccountID: sa.ID,
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPIdentityBlockWithSAID(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, grove := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-block-with-sa",
		GroveID: grove.ID,
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     "block",
			ServiceAccountID: "should-not-be-here",
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestCreateAgent_GCPIdentityPassthroughWithSAID(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, grove := setupCreateAgentServer(t, disp)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "gcp-passthrough-with-sa",
		GroveID: grove.ID,
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

	// Create a grove owned by the broker owner with proper policies
	grove := &store.Grove{
		ID:        "grove-pt-owner",
		Name:      "Passthrough Owner Grove",
		Slug:      "passthrough-owner-grove",
		OwnerID:   owner.ID,
		CreatedBy: owner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateGrove(ctx, grove))
	srv.createGroveMembersGroupAndPolicy(ctx, grove)

	// Create a broker owned by the same user
	broker := &store.RuntimeBroker{
		ID:        "broker-pt-owner",
		Name:      "Owner Broker",
		Slug:      "owner-broker",
		Status:    store.BrokerStatusOnline,
		CreatedBy: owner.ID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	grove.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateGrove(ctx, grove))

	// Broker owner should be allowed to use passthrough
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "pt-owner-agent",
		GroveID: grove.ID,
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

	// Create a grove where the non-owner is a member
	grove := &store.Grove{
		ID:        "grove-pt-nonowner",
		Name:      "Passthrough NonOwner Grove",
		Slug:      "passthrough-nonowner-grove",
		OwnerID:   nonOwner.ID,
		CreatedBy: nonOwner.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateGrove(ctx, grove))
	srv.createGroveMembersGroupAndPolicy(ctx, grove)

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
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	grove.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateGrove(ctx, grove))

	// Non-owner should be DENIED passthrough
	rec := doRequestAsUser(t, srv, nonOwner, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "pt-denied-agent",
		GroveID: grove.ID,
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

	grove := &store.Grove{
		ID:        "grove-pt-admin",
		Name:      "Passthrough Admin Grove",
		Slug:      "passthrough-admin-grove",
		OwnerID:   adminUser.ID,
		CreatedBy: adminUser.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	require.NoError(t, s.CreateGrove(ctx, grove))
	srv.createGroveMembersGroupAndPolicy(ctx, grove)

	// Broker owned by someone else
	broker := &store.RuntimeBroker{
		ID:        "broker-pt-admin",
		Name:      "Admin Test Broker",
		Slug:      "admin-test-broker",
		Status:    store.BrokerStatusOnline,
		CreatedBy: brokerOwner.ID,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	grove.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateGrove(ctx, grove))

	// Admin (non-owner) should be allowed passthrough
	rec := doRequestAsUser(t, srv, adminUser, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:    "pt-admin-agent",
		GroveID: grove.ID,
		Task:    "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode: "passthrough",
		},
	})
	require.Equal(t, http.StatusCreated, rec.Code)
}

func TestPreserveTerminalPhase(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	grove := &store.Grove{ID: "grove-tp", Name: "TP Grove", Slug: "tp-grove"}
	require.NoError(t, s.CreateGrove(ctx, grove))

	t.Run("preserves error phase", func(t *testing.T) {
		agent := &store.Agent{
			ID:      "agent-tp-error",
			Slug:    "agent-tp-error",
			Name:    "TP Error Agent",
			GroveID: grove.ID,
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
			GroveID: grove.ID,
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
			GroveID: grove.ID,
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
// /api/v1/agents endpoint returns agents from all groves, consistent with
// the grove-scoped endpoint behavior.
func TestListAgents_GlobalEndpointReturnsAllAgents(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Create two groves with agents in each
	grove1 := &store.Grove{ID: "grove-global-1", Name: "Grove One", Slug: "grove-one"}
	grove2 := &store.Grove{ID: "grove-global-2", Name: "Grove Two", Slug: "grove-two"}
	require.NoError(t, s.CreateGrove(ctx, grove1))
	require.NoError(t, s.CreateGrove(ctx, grove2))

	agent1 := &store.Agent{
		ID: "agent-g1", Slug: "agent-g1", Name: "Agent G1",
		GroveID: grove1.ID, Phase: string(state.PhaseRunning),
	}
	agent2 := &store.Agent{
		ID: "agent-g2", Slug: "agent-g2", Name: "Agent G2",
		GroveID: grove2.ID, Phase: string(state.PhaseCreated),
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))
	require.NoError(t, s.CreateAgent(ctx, agent2))

	// Global list should return agents from both groves
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Len(t, resp.Agents, 2, "global list should return agents from all groves")
	assert.Equal(t, 2, resp.TotalCount, "TotalCount should match number of agents")

	// Verify both agents are present
	names := map[string]bool{}
	for _, a := range resp.Agents {
		names[a.Name] = true
	}
	assert.True(t, names["Agent G1"], "should include agent from grove 1")
	assert.True(t, names["Agent G2"], "should include agent from grove 2")

	// Verify grove-scoped lists are consistent
	rec1 := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/groves/%s/agents", grove1.ID), nil)
	require.Equal(t, http.StatusOK, rec1.Code)
	var resp1 ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	assert.Len(t, resp1.Agents, 1, "grove-scoped list should return only grove 1 agents")

	rec2 := doRequest(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/groves/%s/agents", grove2.ID), nil)
	require.Equal(t, http.StatusOK, rec2.Code)
	var resp2 ListAgentsResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	assert.Len(t, resp2.Agents, 1, "grove-scoped list should return only grove 2 agents")
}

func TestHandleAgentExec_DispatchesToRuntimeBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:   "grove-exec",
		Name: "Exec Grove",
		Slug: "exec-grove",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID:     "broker-exec",
		Name:   "Exec Broker",
		Slug:   "exec-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-exec-1",
		Slug:            "agent-exec-1",
		Name:            "Exec Agent",
		GroveID:         grove.ID,
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

func TestHandleGroveAgentExec_DispatchesToRuntimeBroker(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:   "grove-exec-grove-route",
		Name: "Exec Grove Route",
		Slug: "exec-grove-route",
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	broker := &store.RuntimeBroker{
		ID:     "broker-exec-grove-route",
		Name:   "Exec Broker Grove Route",
		Slug:   "exec-broker-grove-route",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              "agent-exec-grove-route",
		Slug:            "agent-exec-grove-route",
		Name:            "Exec Agent Grove Route",
		GroveID:         grove.ID,
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	srv.SetDispatcher(&createAgentDispatcher{execOutput: "terminal output"})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/groves/"+grove.ID+"/agents/"+agent.Slug+"/exec", map[string]interface{}{
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
