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

	"github.com/ptone/scion-agent/pkg/store"
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
		Slug: "agent-1-slug",
		Name:    "Agent 1",
		GroveID: grove.ID,
		Status:  store.AgentStatusRunning,
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))

	agent2 := &store.Agent{
		ID:      "agent-2",
		Slug: "agent-2-slug",
		Name:    "Agent 2",
		GroveID: grove.ID,
		Status:  store.AgentStatusRunning,
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	// Get agent token service
	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	// Generate token for agent 1
	token1, err := tokenSvc.GenerateAgentToken(agent1.ID, grove.ID, []AgentTokenScope{ScopeAgentStatusUpdate})
	require.NoError(t, err)

	t.Run("Agent 1 can update its own status", func(t *testing.T) {
		status := store.AgentStatusUpdate{
			Status:  "idle",
			Message: "Waiting for user input",
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
		assert.Equal(t, "idle", updated.Status)
		assert.Equal(t, "Waiting for user input", updated.Message)
	})

	t.Run("Agent 1 cannot update Agent 2's status", func(t *testing.T) {
		status := store.AgentStatusUpdate{
			Status: "error",
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
			Status: "running",
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
		assert.Equal(t, "running", updated.Status)
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
		Slug: "agent-h-slug",
		Name:    "Agent Heartbeat",
		GroveID: grove.ID,
		Status:  store.AgentStatusRunning,
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
		Status:    store.AgentStatusRunning,
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
		Slug:         fmt.Sprintf("agent-offline-%s-slug", suffix),
		Name:            fmt.Sprintf("Agent Offline %s", suffix),
		GroveID:         grove.ID,
		RuntimeBrokerID: broker.ID,
		Status:          store.AgentStatusRunning,
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
		Slug: "agent-nobroker-slug",
		Name:    "Agent No Broker",
		GroveID: grove.ID,
		Status:  store.AgentStatusRunning,
		// No RuntimeBrokerID set
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	assert.Equal(t, http.StatusNoContent, rec.Code)

	// Verify agent was deleted
	_, err := s.GetAgent(ctx, agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
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
