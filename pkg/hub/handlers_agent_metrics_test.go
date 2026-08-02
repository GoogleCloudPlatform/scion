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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleAgentMetrics_SelfAuth(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("metrics-project"),
		Name: "Metrics Project",
		Slug: "metrics-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent1 := &store.Agent{
		ID:        tid("metrics-agent-1"),
		Slug:      "metrics-agent-1",
		Name:      "Metrics Agent 1",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))

	agent2 := &store.Agent{
		ID:        tid("metrics-agent-2"),
		Slug:      "metrics-agent-2",
		Name:      "Metrics Agent 2",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	// Agent token with only ScopeAgentStatusUpdate (default scope).
	token1, err := tokenSvc.GenerateAgentToken(agent1.ID, project.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

	payload := metricsPayloadRequest{
		Type:    "session_metrics",
		AgentID: agent1.ID,
		Session: metricsSession{
			ID:        "session-1",
			StartedAt: "2026-08-01T10:00:00Z",
			EndedAt:   "2026-08-01T10:05:00Z",
			Status:    "completed",
			TurnCount: 5,
			Model:     "claude-4",
		},
		Tokens: metricsTokens{
			Input:  1000,
			Output: 200,
		},
	}

	t.Run("agent cannot report metrics for another agent", func(t *testing.T) {
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent2.ID+"/metrics", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", token1)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		assert.Equal(t, http.StatusForbidden, rec.Code)
	})

	t.Run("unauthenticated request is rejected", func(t *testing.T) {
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent1.ID+"/metrics", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		// Without any auth token, the request should be rejected.
		assert.True(t, rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden,
			"expected 401 or 403, got %d", rec.Code)
	})
}

func TestHandleAgentMetrics_Validation(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("metrics-val-project"),
		Name: "Metrics Validation Project",
		Slug: "metrics-val-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:        tid("metrics-val-agent"),
		Slug:      "metrics-val-agent",
		Name:      "Metrics Val Agent",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	token, err := tokenSvc.GenerateAgentToken(agent.ID, project.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

	sendMetrics := func(t *testing.T, payload metricsPayloadRequest) *httptest.ResponseRecorder {
		t.Helper()
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/metrics", bytes.NewReader(body))
		req.Header.Set("X-Scion-Agent-Token", token)
		req.Header.Set("Content-Type", "application/json")

		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	t.Run("missing session.id returns 400", func(t *testing.T) {
		rec := sendMetrics(t, metricsPayloadRequest{
			Session: metricsSession{
				StartedAt: "2026-08-01T10:00:00Z",
			},
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("missing session.started_at returns 400", func(t *testing.T) {
		rec := sendMetrics(t, metricsPayloadRequest{
			Session: metricsSession{
				ID: "session-1",
			},
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("malformed session.started_at returns 400", func(t *testing.T) {
		rec := sendMetrics(t, metricsPayloadRequest{
			Session: metricsSession{
				ID:        "session-1",
				StartedAt: "not-a-timestamp",
			},
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("malformed session.ended_at returns 400", func(t *testing.T) {
		rec := sendMetrics(t, metricsPayloadRequest{
			Session: metricsSession{
				ID:        "session-1",
				StartedAt: "2026-08-01T10:00:00Z",
				EndedAt:   "not-a-timestamp",
			},
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

func TestHandleAgentMetrics_HappyPath(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("metrics-hp-project"),
		Name: "Metrics HP Project",
		Slug: "metrics-hp-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:        tid("metrics-hp-agent"),
		Slug:      "metrics-hp-agent",
		Name:      "Metrics HP Agent",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	tokenSvc := srv.GetAgentTokenService()
	require.NotNil(t, tokenSvc)

	token, err := tokenSvc.GenerateAgentToken(agent.ID, project.ID, []AgentTokenScope{ScopeAgentStatusUpdate}, nil)
	require.NoError(t, err)

	payload := metricsPayloadRequest{
		Type:    "session_metrics",
		AgentID: agent.ID,
		Session: metricsSession{
			ID:        "session-hp-1",
			StartedAt: "2026-08-01T10:00:00Z",
			EndedAt:   "2026-08-01T10:05:00Z",
			Status:    "completed",
			TurnCount: 3,
			Model:     "claude-4",
		},
		Tokens: metricsTokens{
			Input:     5000,
			Output:    1200,
			Cached:    800,
			Reasoning: 300,
		},
		Tools: map[string]toolStats{
			"read_file":  {Calls: 5, Success: 5, Error: 0},
			"write_file": {Calls: 2, Success: 1, Error: 1},
		},
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agent.ID+"/metrics", bytes.NewReader(body))
	req.Header.Set("X-Scion-Agent-Token", token)
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	// Verify response body contains the created ID.
	var respBody map[string]string
	err = json.Unmarshal(rec.Body.Bytes(), &respBody)
	require.NoError(t, err, "response body should be valid JSON")
	assert.NotEmpty(t, respBody["id"], "response should contain created record id")

	// Verify the record was persisted.
	metrics, err := s.GetAgentSessionMetrics(ctx, respBody["id"])
	require.NoError(t, err)
	assert.Equal(t, agent.ID, metrics.AgentID)
	assert.Equal(t, "session-hp-1", metrics.SessionID)
	assert.Equal(t, "completed", metrics.Status)
	assert.Equal(t, 3, metrics.TurnCount)
	assert.Equal(t, "claude-4", metrics.Model)
	assert.Equal(t, int64(5000), metrics.TokensInput)
	assert.Equal(t, int64(1200), metrics.TokensOutput)
	assert.Equal(t, int64(800), metrics.TokensCached)
	assert.Equal(t, int64(300), metrics.TokensReasoning)
	assert.Equal(t, project.ID, metrics.ProjectID)
}
