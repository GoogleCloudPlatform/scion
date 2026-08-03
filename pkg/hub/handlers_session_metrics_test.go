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

// setupSessionMetricsFixtures creates a project, two agents, and several
// session metrics records for testing the session metrics API endpoints.
func setupSessionMetricsFixtures(t *testing.T, s store.Store) (*store.Project, *store.Agent, *store.Agent) {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("sm-project"),
		Name: "Session Metrics Project",
		Slug: "sm-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent1 := &store.Agent{
		ID:        tid("sm-agent-1"),
		Slug:      "sm-agent-1",
		Name:      "SM Agent 1",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent1))

	agent2 := &store.Agent{
		ID:        tid("sm-agent-2"),
		Slug:      "sm-agent-2",
		Name:      "SM Agent 2",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent2))

	// Create session metrics for agent1.
	ended1 := time.Date(2026, 8, 1, 10, 5, 0, 0, time.UTC)
	m1 := &store.AgentSessionMetrics{
		AgentID:         agent1.ID,
		ProjectID:       project.ID,
		SessionID:       "session-a1-1",
		StartedAt:       time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		EndedAt:         &ended1,
		Status:          "completed",
		TurnCount:       5,
		Model:           "claude-4",
		TokensInput:     1000,
		TokensOutput:    200,
		TokensCached:    100,
		TokensReasoning: 50,
		ToolCalls: map[string]any{
			"read_file":  map[string]any{"calls": 3, "success": 3, "error": 0},
			"write_file": map[string]any{"calls": 2, "success": 1, "error": 1},
		},
	}
	require.NoError(t, s.CreateAgentSessionMetrics(ctx, m1))

	ended2 := time.Date(2026, 8, 1, 11, 10, 0, 0, time.UTC)
	m2 := &store.AgentSessionMetrics{
		AgentID:         agent1.ID,
		ProjectID:       project.ID,
		SessionID:       "session-a1-2",
		StartedAt:       time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC),
		EndedAt:         &ended2,
		Status:          "completed",
		TurnCount:       3,
		Model:           "gpt-4o",
		TokensInput:     2000,
		TokensOutput:    400,
		TokensCached:    200,
		TokensReasoning: 100,
		ToolCalls: map[string]any{
			"read_file": map[string]any{"calls": 5, "success": 5, "error": 0},
		},
	}
	require.NoError(t, s.CreateAgentSessionMetrics(ctx, m2))

	// Create session metrics for agent2.
	ended3 := time.Date(2026, 8, 1, 12, 15, 0, 0, time.UTC)
	m3 := &store.AgentSessionMetrics{
		AgentID:         agent2.ID,
		ProjectID:       project.ID,
		SessionID:       "session-a2-1",
		StartedAt:       time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		EndedAt:         &ended3,
		Status:          "completed",
		TurnCount:       7,
		Model:           "claude-4",
		TokensInput:     3000,
		TokensOutput:    600,
		TokensCached:    300,
		TokensReasoning: 150,
		ToolCalls: map[string]any{
			"bash": map[string]any{"calls": 10, "success": 9, "error": 1},
		},
	}
	require.NoError(t, s.CreateAgentSessionMetrics(ctx, m3))

	return project, agent1, agent2
}

// =============================================================================
// GET /api/v1/agents/{id}/metrics/summary
// =============================================================================

func TestHandleAgentMetricsSummary(t *testing.T) {
	srv, s := testServer(t)
	_, agent1, _ := setupSessionMetricsFixtures(t, s)

	t.Run("returns aggregate metrics for agent", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet,
			"/api/v1/agents/"+agent1.ID+"/metrics/summary", nil)

		assert.Equal(t, http.StatusOK, rec.Code)

		var resp agentMetricsSummaryResponse
		err := json.Unmarshal(rec.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, agent1.ID, resp.AgentID)
		assert.Equal(t, 2, resp.TotalSessions)
		assert.Equal(t, int64(3000), resp.TotalTokensInput)    // 1000 + 2000
		assert.Equal(t, int64(600), resp.TotalTokensOutput)    // 200 + 400
		assert.Equal(t, int64(300), resp.TotalTokensCached)    // 100 + 200
		assert.Equal(t, int64(150), resp.TotalTokensReasoning) // 50 + 100
		assert.Equal(t, 10, resp.TotalToolCalls)               // 3+2+5
		assert.Greater(t, resp.AvgSessionDurationMs, int64(0))
		assert.Equal(t, int64(1800), resp.AvgTokensPerSession) // (3000+600)/2
		assert.NotEmpty(t, resp.MostUsedTools)
		assert.NotEmpty(t, resp.MostUsedModels)
	})

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		rec := doRequestNoAuth(t, srv, http.MethodGet,
			"/api/v1/agents/"+agent1.ID+"/metrics/summary", nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("non-existent agent returns 404", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet,
			"/api/v1/agents/"+tid("no-such-agent")+"/metrics/summary", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("POST method not allowed", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPost,
			"/api/v1/agents/"+agent1.ID+"/metrics/summary", nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})
}

// =============================================================================
// GET /api/v1/metrics/session/{id}
// =============================================================================

func TestHandleSessionMetrics(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{
		ID:   tid("sm-session-project"),
		Name: "Session Project",
		Slug: "sm-session-project",
	}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:        tid("sm-session-agent"),
		Slug:      "sm-session-agent",
		Name:      "SM Session Agent",
		ProjectID: project.ID,
		Phase:     string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	ended := time.Date(2026, 8, 1, 10, 5, 0, 0, time.UTC)
	m := &store.AgentSessionMetrics{
		AgentID:      agent.ID,
		ProjectID:    project.ID,
		SessionID:    "session-detail-1",
		StartedAt:    time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
		EndedAt:      &ended,
		Status:       "completed",
		TurnCount:    4,
		Model:        "claude-4",
		TokensInput:  5000,
		TokensOutput: 1200,
	}
	require.NoError(t, s.CreateAgentSessionMetrics(ctx, m))

	t.Run("returns session metrics by ID", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet,
			"/api/v1/metrics/session/"+m.ID, nil)

		assert.Equal(t, http.StatusOK, rec.Code)

		var resp store.AgentSessionMetrics
		err := json.Unmarshal(rec.Body.Bytes(), &resp)
		require.NoError(t, err)
		assert.Equal(t, m.ID, resp.ID)
		assert.Equal(t, agent.ID, resp.AgentID)
		assert.Equal(t, "session-detail-1", resp.SessionID)
		assert.Equal(t, int64(5000), resp.TokensInput)
	})

	t.Run("non-existent session returns 404", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet,
			"/api/v1/metrics/session/"+tid("no-such-session"), nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		rec := doRequestNoAuth(t, srv, http.MethodGet,
			"/api/v1/metrics/session/"+m.ID, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("session with deleted agent returns error", func(t *testing.T) {
		// Create a session record pointing to an agent that does not exist.
		// The handler must look up the agent for authorization; a missing
		// agent should produce a 404 rather than leaking the record.
		orphanMetrics := &store.AgentSessionMetrics{
			AgentID:     tid("deleted-agent"),
			ProjectID:   project.ID,
			SessionID:   "session-orphan",
			StartedAt:   time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC),
			TokensInput: 999,
		}
		require.NoError(t, s.CreateAgentSessionMetrics(ctx, orphanMetrics))

		rec := doRequest(t, srv, http.MethodGet,
			"/api/v1/metrics/session/"+orphanMetrics.ID, nil)

		// The agent lookup should fail with 404.
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})
}

// =============================================================================
// GET /api/v1/projects/{id}/metrics/summary
// =============================================================================

func TestHandleProjectSessionMetricsSummary(t *testing.T) {
	srv, s := testServer(t)
	project, _, _ := setupSessionMetricsFixtures(t, s)

	t.Run("returns aggregate project metrics", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet,
			"/api/v1/projects/"+project.ID+"/metrics/summary", nil)

		assert.Equal(t, http.StatusOK, rec.Code)

		var resp projectMetricsSummaryResponse
		err := json.Unmarshal(rec.Body.Bytes(), &resp)
		require.NoError(t, err)

		assert.Equal(t, project.ID, resp.ProjectID)
		assert.Equal(t, 3, resp.TotalSessions)                 // 2 from agent1 + 1 from agent2
		assert.Equal(t, int64(6000), resp.TotalTokensInput)    // 1000+2000+3000
		assert.Equal(t, int64(1200), resp.TotalTokensOutput)   // 200+400+600
		assert.Equal(t, int64(600), resp.TotalTokensCached)    // 100+200+300
		assert.Equal(t, int64(300), resp.TotalTokensReasoning) // 50+100+150
		assert.Equal(t, 2, resp.ActiveAgents)                  // agent1 and agent2
		assert.NotEmpty(t, resp.MostUsedTools)
		assert.NotEmpty(t, resp.MostUsedModels)
	})

	t.Run("unauthenticated returns 401", func(t *testing.T) {
		rec := doRequestNoAuth(t, srv, http.MethodGet,
			"/api/v1/projects/"+project.ID+"/metrics/summary", nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("non-existent project returns 404", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodGet,
			"/api/v1/projects/"+tid("no-such-project")+"/metrics/summary", nil)
		assert.Equal(t, http.StatusNotFound, rec.Code)
	})

	t.Run("POST method not allowed", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPost,
			"/api/v1/projects/"+project.ID+"/metrics/summary", nil)
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	})
}

// =============================================================================
// Aggregation helper unit tests
// =============================================================================

func TestAggregateToolCalls(t *testing.T) {
	sessions := []*store.AgentSessionMetrics{
		{
			ToolCalls: map[string]any{
				"read_file":  map[string]any{"calls": float64(3), "success": float64(3), "error": float64(0)},
				"write_file": map[string]any{"calls": float64(2), "success": float64(1), "error": float64(1)},
			},
		},
		{
			ToolCalls: map[string]any{
				"read_file": map[string]any{"calls": float64(5), "success": float64(5), "error": float64(0)},
				"bash":      map[string]any{"calls": float64(1), "success": float64(1), "error": float64(0)},
			},
		},
	}

	tools, total := aggregateToolCalls(sessions)

	assert.Equal(t, 11, total) // 3+2+5+1
	assert.Len(t, tools, 3)

	// Sorted by calls descending: read_file(8), write_file(2), bash(1)
	assert.Equal(t, "read_file", tools[0].Name)
	assert.Equal(t, 8, tools[0].Calls)
	assert.Equal(t, 8, tools[0].Success)
}

func TestAggregateModels(t *testing.T) {
	sessions := []*store.AgentSessionMetrics{
		{Model: "claude-4"},
		{Model: "claude-4"},
		{Model: "gpt-4o"},
		{Model: ""},
	}

	models := aggregateModels(sessions)

	assert.Len(t, models, 2)
	// claude-4 has 2 sessions, gpt-4o has 1
	assert.Equal(t, "claude-4", models[0].Model)
	assert.Equal(t, 2, models[0].Sessions)
}

func TestAvgSessionDuration(t *testing.T) {
	ended1 := time.Date(2026, 8, 1, 10, 5, 0, 0, time.UTC)  // 5 min
	ended2 := time.Date(2026, 8, 1, 11, 15, 0, 0, time.UTC) // 15 min

	sessions := []*store.AgentSessionMetrics{
		{
			StartedAt: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
			EndedAt:   &ended1,
		},
		{
			StartedAt: time.Date(2026, 8, 1, 11, 0, 0, 0, time.UTC),
			EndedAt:   &ended2,
		},
		{
			// Session without EndedAt should be skipped.
			StartedAt: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		},
	}

	avg := avgSessionDuration(sessions)
	// Average of 5min and 15min = 10min = 600,000 ms
	assert.Equal(t, int64(600000), avg)
}

func TestIntFromAny(t *testing.T) {
	assert.Equal(t, 5, intFromAny(float64(5)))
	assert.Equal(t, 3, intFromAny(int(3)))
	assert.Equal(t, 7, intFromAny(int64(7)))
	assert.Equal(t, 0, intFromAny("not a number"))
	assert.Equal(t, 0, intFromAny(nil))
}
